package gateway

// authorization.go holds the session authorization seam:
//
//   - every proxied route requires a valid session cookie resolving to a live
//     lease held by this gateway;
//   - leases are renewed/checked every ~10 s; on revoke or a lost broker the
//     session fails closed and open sockets die within the revoke deadline;
//   - WebSocket upgrades require Connection: upgrade AND Upgrade: websocket
//     (non-websocket upgrade offers are rejected outright — SEC-3), plus an
//     Origin triple-match (Origin == public origin == request authority);
//   - at most one interactive stream per lease — sequential replacement with
//     fencing (a new upgrade fences the old socket, an in-flight handshake
//     holds an exclusive admission);
//   - the upstream path allowlist exposes only client assets and the
//     streaming endpoint, evaluated on the cleaned path — runtime
//     management routes (e.g. /api/*) and any dot-segment/encoded
//     traversal are denied before auth (SEC-20);
//   - upstream requests carry broker-injected credentials; client-supplied
//     Authorization and Cookie headers are stripped, never forwarded;
//   - upstream TLS validates against the target's pinned CA — never
//     InsecureSkipVerify.
//
// The admission/fencing machinery below is ported from the proven fixture
// gateway, including the fixes for the regression-suite defects: generation-scoped upgrade admission, conn
// registration re-checking liveness, and closure-captured untrack.

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/broker"
	"github.com/tinyorbitvn/tinycdi/internal/ratelimit"
)

// session is one redeemed lease: the cookie value maps here, the renew loop
// keeps it alive, and at most one hijacked stream may be bound at a time.
type session struct {
	id    string // opaque cookie value — never the lease ID or ticket
	lease broker.Lease
	fence broker.Fence // pinned at redemption, refreshed from renews

	// createdAt is the mint time — the anchor of the unattached TTL. A
	// session is "attached" once a request carrying its cookie is
	// admitted on its own host (the 303 landed in a browser); until then
	// it is a ghost the renew loop reaps at UnattachedSessionTTL.
	createdAt time.Time
	attached  atomic.Bool

	// target/transport resolve lazily on first proxied request (broker
	// ResolveTarget is internal-only and may change after redemption).
	target    *broker.Target
	upURL     *url.URL // parsed t.UpstreamURL
	transport http.RoundTripper

	done   chan struct{} // closed when the session dies
	dieOne sync.Once

	// events is the ordered activity-report queue drained by the gateway's
	// activitySender goroutine (connected/disconnect/input, FIFO).
	events chan activityEvent

	mu          sync.Mutex
	lastRenewOK time.Time // gateway clock time of the last successful renew
	// lastInputReport rate-limits "input" activity reports to the broker.
	lastInputReport time.Time
	nextID          int
	conns           map[net.Conn]struct{} // hijacked (upgraded) conns
	cancels         map[int]context.CancelFunc
	// streamEpoch is the lease stream epoch this process claimed or last
	// observed on renew. A renew reporting a newer epoch means another
	// replica admitted the stream: our conns are fenced (P3).
	streamEpoch uint64
	// pendingReports counts activity reports that are queued or whose broker
	// call has not returned. An event is counted from the moment it is
	// enqueued (under s.mu) until its report finished, so there is no
	// instant — in particular between the sender's dequeue and its broker
	// call — at which Drain could see "nothing pending" with a report still
	// on its way.
	pendingReports int

	// upgradeInFlight makes stream admission atomic: a second concurrent
	// upgrade is rejected instead of both surviving the fence. upgradeGen
	// identifies the reservation owner so an old stream's deferred release
	// cannot clear a successor's admission.
	upgradeInFlight bool
	upgradeGen      int
	// streamTrackID is the cancel registration of the current upgrade
	// request, so self-fencing kills only the old stream, not sibling
	// asset fetches.
	streamTrackID int
	// streamSeen marks that a stream conn was actually hijacked on this
	// replica — the local half of "the lease already had a stream" for
	// the frame-reload counter (streamEpoch covers the directory mode).
	streamSeen bool
}

// sessionMints counts newSession calls. A test cannot intercept a
// package-internal call, so the counter lives here and is read through
// export_test.go — it is how the allocate-nothing tests prove an unseen
// cookie mints no session object at all.
var sessionMints atomic.Int64

func newSession(cookieID string, l broker.Lease, now time.Time) *session {
	sessionMints.Add(1)
	return &session{
		id:            cookieID,
		lease:         l,
		fence:         fenceOf(l),
		createdAt:     now,
		lastRenewOK:   now,
		streamEpoch:   l.StreamEpoch,
		done:          make(chan struct{}),
		events:        make(chan activityEvent, activityQueueLen),
		conns:         map[net.Conn]struct{}{},
		cancels:       map[int]context.CancelFunc{},
		streamTrackID: -1,
	}
}

func fenceOf(l broker.Lease) broker.Fence {
	return broker.Fence{
		WorkspaceUID:      l.WorkspaceUID,
		RuntimeGeneration: l.RuntimeGeneration,
		RuntimeUID:        l.RuntimeUID,
		FencingVersion:    l.FencingVersion,
	}
}

func (s *session) fenceSnapshot() broker.Fence {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fence
}

// noteRenewed records a successful renew and adopts the returned lease state.
func (s *session) noteRenewed(l broker.Lease, now time.Time) {
	s.mu.Lock()
	s.lastRenewOK = now
	s.lease = l
	s.fence = fenceOf(l)
	s.mu.Unlock()
}

func (s *session) lastRenew() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastRenewOK
}

// leaseID returns the bound lease ID (lease is replaced by renews).
func (s *session) leaseID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lease.ID
}

// workspaceUID returns the workspace this session serves.
func (s *session) workspaceUID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lease.WorkspaceUID
}

// noteAttached records that a request carrying this session's cookie was
// admitted on its own host — the launch's 303 reached a browser. Until the
// first such request the session is "unattached": a client that abandoned
// the launch between ticket redemption and delivery left it a ghost no
// browser holds. Idempotent and cheap on the per-request path.
func (s *session) noteAttached() {
	s.attached.Store(true)
}

// unattachedExpired reports whether the session minted, never admitted a
// request, and outlived the unattached TTL — the ghost-session case: the
// redeem succeeded but the 303 never landed.
func (s *session) unattachedExpired(g *Gateway) bool {
	return !s.attached.Load() && !g.now().Before(s.createdAt.Add(g.cfg.UnattachedSessionTTL))
}

// live reports whether the session is still usable: not explicitly dead and
// inside the fail-closed window since the last successful renew.
func (s *session) live(g *Gateway) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.deadLocked() && g.now().Sub(s.lastRenewOK) <= g.cfg.RevokeDeadline
}

// deadLocked reports terminal state. Channel-based, so it is safe with or
// without s.mu held.
func (s *session) deadLocked() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

// kill marks the session dead exactly once and closes everything: in-flight
// request contexts are cancelled and hijacked conns closed.
func (s *session) kill() {
	s.dieOne.Do(func() {
		close(s.done)
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, c := range s.cancels {
			c()
		}
		for c := range s.conns {
			c.Close()
		}
		s.conns = map[net.Conn]struct{}{}
		s.cancels = map[int]context.CancelFunc{}
	})
}

// track registers an in-flight request's cancel. Under the session lock it
// refuses if already dead so a handshake cannot survive a concurrent revoke.
func (s *session) track(ctx context.Context) (context.Context, int, bool) {
	cctx, cancel := context.WithCancel(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.deadLocked() {
		cancel()
		return cctx, -1, false
	}
	id := s.nextID
	s.nextID++
	s.cancels[id] = cancel
	return cctx, id, true
}

func (s *session) untrack(id int, conn net.Conn) {
	s.mu.Lock()
	if c, ok := s.cancels[id]; ok {
		delete(s.cancels, id)
		c()
	}
	if conn != nil {
		delete(s.conns, conn)
	}
	s.mu.Unlock()
}

// addConn registers a hijacked conn; refuses (and closes it) if the session
// died between the auth check and the hijack.
func (s *session) addConn(c net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.deadLocked() {
		c.Close()
		return false
	}
	s.conns[c] = struct{}{}
	s.streamSeen = true
	return true
}

// admitUpgrade atomically checks liveness, fences this session's previous
// stream (self-takeover on reload), and reserves the single stream slot. A
// concurrent second upgrade returns false instead of surviving the fence.
// The returned generation token identifies this reservation: only its owner
// may release the slot via endUpgrade.
func (s *session) admitUpgrade() (int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.deadLocked() {
		return -1, false
	}
	if s.upgradeInFlight {
		return -1, false
	}
	s.upgradeGen++
	s.upgradeInFlight = true
	s.dropStreamsLocked()
	return s.upgradeGen, true
}

// dropStreamsLocked closes every open stream conn and cancels the tracked
// stream request — the local half of both self-takeover fencing and the
// cross-replica epoch fence. The close is abrupt on purpose: a fenced
// stream must not let its client retry — the retry would fence the new
// owner right back. Closed conns stay in the map until their proxy request
// unwinds: untrack removes a conn only after its disconnect report was
// queued, so "no conns" means every disconnect is at least enqueued — the
// invariant Drain waits on. Caller holds s.mu.
func (s *session) dropStreamsLocked() {
	for _, c := range s.detachStreamsLocked() {
		c.Close()
	}
}

// detachStreamsLocked collects the open stream conns and cancels the
// tracked stream request without closing anything. Caller holds s.mu.
func (s *session) detachStreamsLocked() []net.Conn {
	conns := make([]net.Conn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	if c, ok := s.cancels[s.streamTrackID]; ok {
		delete(s.cancels, s.streamTrackID)
		c()
	}
	s.streamTrackID = -1
	return conns
}

// drainStreams sheds every open stream conn the way a rollout wants it
// (FX-R32): a proper WebSocket close frame at a frame boundary, so the
// KasmVNC client inside the portal frame sees a clean disconnect and
// retries the websocket itself instead of waiting for the SPA to
// re-navigate it. The session and its lease stay live. Closes run in
// parallel so one silent conn cannot eat the drain window.
//
// Order matters: the graceful close runs BEFORE the tracked request ctx
// is cancelled — cancelling first unwinds the proxy's copy loops and
// closes the conn, and a conn parked mid-frame would never get the chance
// to reach a boundary (the close frame must be on the wire first).
func (s *session) drainStreams() {
	s.mu.Lock()
	conns := make([]net.Conn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()
	var wg sync.WaitGroup
	for _, c := range conns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			gracefulCloseConn(c)
		}()
	}
	wg.Wait()
	s.mu.Lock()
	s.detachStreamsLocked()
	s.mu.Unlock()
}

// streamBusy reports whether a stream admission is in flight or a hijacked
// conn is still open — Drain is not done until every session is quiet.
func (s *session) streamBusy() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.upgradeInFlight || len(s.conns) > 0
}

// pendingActivity reports undelivered activity reports: events still in the
// queue plus reports whose broker call has not returned.
func (s *session) pendingActivity() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pendingReports
}

// streamEpochValue returns the lease's stream epoch as this replica last
// observed it — the only cross-replica attach signal the lease carries:
// a nonzero value proves some replica claimed a stream on this lease.
func (s *session) streamEpochValue() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.streamEpoch
}

// setStreamEpoch records the stream epoch ClaimStream returned — the fence
// other replicas will compare their renewed leases against.
func (s *session) setStreamEpoch(epoch uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if epoch > s.streamEpoch {
		s.streamEpoch = epoch
	}
}

// fenceStreamsFor applies the cross-replica stream fence on each successful
// renew: a stream epoch newer than the one this process claimed means
// another replica admitted the stream, so our conns close — the session
// itself stays alive and keeps renewing. It reports whether the fence
// actually fired, for tinycdi_gateway_streams_fenced_total (E8).
func (s *session) fenceStreamsFor(epoch uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if epoch <= s.streamEpoch {
		return false
	}
	s.streamEpoch = epoch
	s.dropStreamsLocked()
	return true
}

// setStreamTrack records which tracked request owns the stream slot.
func (s *session) setStreamTrack(id int) {
	s.mu.Lock()
	s.streamTrackID = id
	s.mu.Unlock()
}

// endUpgrade releases the stream slot only if gen still owns the
// reservation. A fenced older stream returning from ServeHTTP must not
// clear a newer upgrade's in-flight admission.
func (s *session) endUpgrade(gen int) {
	s.mu.Lock()
	if s.upgradeInFlight && s.upgradeGen == gen {
		s.upgradeInFlight = false
	}
	s.mu.Unlock()
}

func (s *session) streamCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.conns)
}

// streamOwner returns the lease's current stream-owner tab id as this
// replica last observed it (claimStream's own write or the last
// renew/rehydrate); "" when no valid id is recorded.
func (s *session) streamOwner() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lease.StreamOwnerTab
}

// setStreamOwner records the owner this process's claim just wrote — the
// local mirror of the lease's stream_owner_tab until the next renew
// confirms it ("" when the claim carried no valid id, matching the NULL
// the broker stores).
func (s *session) setStreamOwner(ownerTab string) {
	if !broker.ValidStreamOwnerTab(ownerTab) {
		ownerTab = ""
	}
	s.mu.Lock()
	s.lease.StreamOwnerTab = ownerTab
	s.mu.Unlock()
}

// hadStream reports whether this lease already claimed a live stream —
// the "already had a stream on the same lease" half of the frame-reload
// counter (NAVTEL-1): a document load before then is a first load, after
// it a re-navigation. streamEpoch > 0 is the lease-level record — a claim
// on ANY replica bumps it, so a session rehydrated here after a rollout
// still knows — while streamSeen covers the no-directory mode where
// claims never bump an epoch.
func (s *session) hadStream() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.streamEpoch > 0 || s.streamSeen
}

// ---------------------------------------------------------------------------
// Request validators (ported from the proven fixture)
// ---------------------------------------------------------------------------

// controlHostOK reports whether the request Host names a configured
// control host (the in-cluster Service names) — the only hosts where
// /healthz and /v1/control/* exist.
func (g *Gateway) controlHostOK(r *http.Request) bool {
	h, _, err := net.SplitHostPort(r.Host)
	if err != nil {
		h = r.Host
	}
	return g.controlHosts[h]
}

// originOK enforces D12 on WebSocket upgrades: Origin must equal the
// request's own workspace origin — "https://" + the request Host, which
// ServeHTTP already matched against the session domain. An absent Origin
// fails the equality too: browsers always send it on upgrades.
func (g *Gateway) originOK(r *http.Request) bool {
	return r.Header.Get("Origin") == "https://"+r.Host
}

// headerHasToken reports whether header name contains token in its
// comma-separated value list (per RFC 7230 §3.2.6 / net/http upgrade rules).
func headerHasToken(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for _, t := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(t), token) {
				return true
			}
		}
	}
	return false
}

// isUpgrade matches net/http's upgrade detection: the Connection token list
// must contain "upgrade" AND Upgrade must name websocket. A bare
// "Upgrade: websocket" header (no Connection token) is NOT an upgrade —
// treating it as one let a plain request consume the stream slot and fence
// the live conn without ever switching protocols (SEC-3).
func isUpgrade(r *http.Request) bool {
	return headerHasToken(r.Header, "Connection", "upgrade") &&
		strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}

// upstreamAllowlist: only the paths the stock KasmVNC web client needs.
// Everything else — including upstream management/API surface — is denied
// at the gateway, never reaching the runtime.
var upstreamAllowlist = []string{
	"/", "/index.html", "/vnc.html", "/screen.html", "/disconnected.html",
	"/favicon.ico", "/package.json",
}

// cleanedProxyPath returns the canonical upstream path for u, or false when
// the request path carries traversal or parser-differential tricks
// (SEC-20): dot segments or anything path.Clean would rewrite, encoded
// dots/slashes/backslashes in the escaped form, literal backslash or
// semicolon, and leftover %-double-encodings in the decoded path. The
// allowlist runs on the returned path and the runtime receives exactly it.
func cleanedProxyPath(u *url.URL) (string, bool) {
	p := u.Path
	if p == "" {
		p = "/"
	}
	if path.Clean(p) != p || strings.Contains(p, "..") ||
		strings.ContainsAny(p, "\\;") {
		return "", false
	}
	raw := u.EscapedPath()
	rawLow := strings.ToLower(raw)
	if strings.Contains(raw, "..") || strings.ContainsAny(raw, "\\;") ||
		strings.Contains(rawLow, "%2e") || strings.Contains(rawLow, "%2f") ||
		strings.Contains(rawLow, "%5c") || strings.Contains(rawLow, "%00") {
		return "", false
	}
	// Double encoding: the decoded path must not still carry encodings of
	// the banned bytes for an upstream that unescapes a second time.
	low := strings.ToLower(p)
	if strings.Contains(low, "%2e") || strings.Contains(low, "%2f") ||
		strings.Contains(low, "%5c") || strings.Contains(low, "%3b") {
		return "", false
	}
	return p, true
}

func upstreamPathOK(p string) bool {
	for _, a := range upstreamAllowlist {
		if p == a {
			return true
		}
	}
	for _, prefix := range []string{"/app/", "/assets/", "/core/", "/websockify"} {
		if strings.HasPrefix(p, prefix) || p == strings.TrimSuffix(prefix, "/") {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Lease renew / fail-closed loop
// ---------------------------------------------------------------------------

// terminalBrokerErr reports whether err is a definitive lease death
// (revoked/stale/denied) rather than a transient reachability failure.
func terminalBrokerErr(err error) bool {
	return errors.Is(err, broker.ErrRevoked) ||
		errors.Is(err, broker.ErrLeaseInvalid) ||
		errors.Is(err, broker.ErrStaleBinding) ||
		errors.Is(err, broker.ErrDenied) ||
		errors.Is(err, broker.ErrNotFound)
}

func leaseFailureReason(err error) string {
	switch {
	case errors.Is(err, broker.ErrRevoked), errors.Is(err, broker.ErrLeaseInvalid),
		errors.Is(err, broker.ErrStaleBinding):
		return "expired"
	case errors.Is(err, broker.ErrDenied), errors.Is(err, broker.ErrNotFound):
		return "denied"
	case errors.Is(err, broker.ErrConnectionInUse):
		return "conflict"
	default:
		return "unavailable"
	}
}

// renewLoop renews the lease every RenewInterval. Terminal broker errors kill
// the session immediately; transient failures kill it once no successful
// renew has landed inside RevokeDeadline (fail closed ≤30 s).
func (g *Gateway) renewLoop(s *session) {
	t := time.NewTicker(g.cfg.RenewInterval)
	defer t.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-g.done:
			return
		case <-t.C:
		}
		// The ghost TTL runs before the liveness check so an abandoned
		// launch is always reaped through the lease-revoking path —
		// never silently by the renew deadline. A session that admitted
		// a request is attached forever and skips this entirely.
		if s.unattachedExpired(g) {
			g.reapUnattached(s)
			return
		}
		if !s.live(g) {
			g.killSession(s, "renew_deadline")
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), g.cfg.RenewInterval)
		l, err := g.cfg.Broker.RenewLease(ctx, g.cfg.Identity, s.leaseID(), s.fenceSnapshot())
		cancel()
		if err == nil {
			s.noteRenewed(l, g.now())
			if s.fenceStreamsFor(l.StreamEpoch) && g.cfg.Metrics != nil {
				g.cfg.Metrics.IncStreamsFenced()
			}
			continue
		}
		if g.cfg.Metrics != nil {
			g.cfg.Metrics.IncLeaseFailure(leaseFailureReason(err))
		}
		if terminalBrokerErr(err) {
			g.killSession(s, "lease_"+leaseFailureReason(err))
			return
		}
		// Transient failure: survive until the fail-closed deadline.
		if g.now().Sub(s.lastRenew()) > g.cfg.RevokeDeadline {
			g.killSession(s, "broker_unreachable")
			return
		}
	}
}

// reapUnattached ends a session no client ever came back for: the lease is
// revoked at the broker first — a live lease is what pins the workspace
// (IssueTicket answers CONNECTION_IN_USE), so revoking drops the pin in one
// round trip instead of at the lease's TTL lapse or the bound portal
// session's absolute expiry — then the local session is torn down as usual.
// A failed revoke still kills the session: with renewal stopped the lease
// lapses on its own inside one lease TTL.
//
// The revoke is skipped when the lease's stream epoch is nonzero: a claim
// from ANY replica bumps it, so a nonzero epoch proves this replica's copy
// is a ghost but the lease is in use elsewhere — revoking would cut a live
// session (the epoch reaches this replica through its own renews, so the
// guard is at most one renew interval stale). Only the local copy dies.
func (g *Gateway) reapUnattached(s *session) {
	if s.streamEpochValue() == 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := g.cfg.Broker.RevokeLease(ctx, s.leaseID())
		cancel()
		if err != nil && g.cfg.Logger != nil {
			g.cfg.Logger.Warn("unattached session: lease revoke failed — lease lapses at TTL", "err", err)
		}
	}
	g.killSession(s, "unattached_expired")
}

// killSession tears s down and unmaps it — never touching a successor that
// has already taken over the workspace slot (SEC-1 shape).
func (g *Gateway) killSession(s *session, reason string) {
	s.kill()
	leaseID, wsUID := s.leaseID(), s.workspaceUID()
	g.mu.Lock()
	removed := g.sessions[s.id] == s
	if removed {
		delete(g.sessions, s.id)
	}
	if g.byLease[leaseID] == s {
		delete(g.byLease, leaseID)
	}
	if g.byWorkspace[wsUID] == s {
		delete(g.byWorkspace, wsUID)
	}
	g.mu.Unlock()
	if removed && g.cfg.Metrics != nil {
		g.cfg.Metrics.AddSessionsActive(-1)
	}
	if g.cfg.Logger != nil {
		g.cfg.Logger.Info("session closed", "reason", reason, "request_id", "")
	}
}

// lookupSession resolves the session cookie to a live session. With a
// session directory configured, a cookie this replica never saw falls back
// to a digest lookup and rebuilds the session (D19); without one the
// v0.1 behaviour is unchanged. wsID is the workspace the request Host
// names: a digest that resolves to another workspace's lease is refused
// inside fetchSession before any session state is allocated. A non-nil
// error means the directory could not answer (not "no session"): the caller
// answers 503 and nothing is cached.
func (g *Gateway) lookupSession(r *http.Request, wsID string) (*session, error) {
	c, err := r.Cookie(SessionCookieName)
	if err != nil || c.Value == "" {
		return nil, nil
	}
	g.mu.Lock()
	s := g.sessions[c.Value]
	g.mu.Unlock()
	if s != nil {
		if s.live(g) {
			return s, nil
		}
		return nil, nil
	}
	if g.cfg.Sessions == nil {
		return nil, nil
	}
	// Unknown cookie on a directory-enabled replica: this is the ONLY
	// proxy-path request that spends Postgres reads before auth — a
	// digest lookup plus an EXISTS probe on miss — so it carries its own
	// per-client bound. A spray of random cookie values mints a fresh
	// singleflight entry per request (the dedup cannot help), which would
	// otherwise turn request rate directly into indexed reads. The key is
	// the plain client key: an unseen cookie cannot key on a session.
	if g.cfg.SessionLookupLimiter != nil {
		if ok, retry := g.cfg.SessionLookupLimiter.Allow(ratelimit.ClientKey(r, g.cfg.TrustedProxies)); !ok {
			return nil, &lookupLimitedError{retryAfter: retry}
		}
	}
	return g.rehydrate(r, c.Value, wsID)
}

// streamOwnerTabParam is the query parameter a stream claim carries the
// owning tab's id in: the portal puts it on the KasmVNC client's `path`
// setting (path=websockify?tcdi_tab=<id>), so every WebSocket retry from
// the same frame claims with the same id (FX-R31). It is an opaque
// correlator, never a credential.
const streamOwnerTabParam = "tcdi_tab"

// claimStream bumps the lease's stream epoch on an admitted upgrade and
// remembers it on the session: another replica's renew loop then fences
// whichever process holds the older epoch. ownerTab is the claiming tab's
// id, forwarded to the broker for same-UPDATE storage.
func (g *Gateway) claimStream(ctx context.Context, s *session, ownerTab string) (uint64, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	epoch, err := g.cfg.Sessions.ClaimStream(ctx, g.cfg.Identity, s.leaseID(), s.fenceSnapshot(), ownerTab)
	cancel()
	if err != nil {
		return 0, err
	}
	s.setStreamEpoch(epoch)
	s.setStreamOwner(ownerTab)
	return epoch, nil
}
