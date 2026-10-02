// Package gateway terminates browser desktop traffic on the session origin:
// ticket redemption (launch), session-cookie authn/z, lease enforcement and
// the authenticated HTTP/WebSocket reverse proxy to the runtime (design
// design §6).
package gateway

import (
	"bufio"
	"context"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/broker"
	"github.com/tinyorbitvn/tinycdi/internal/observability"
	"github.com/tinyorbitvn/tinycdi/internal/sessionhost"
)

// BrokerClient is the session-broker surface the gateway needs. Production
// wires internal/gateway/brokerclient.Client (mTLS to the broker's internal
// listener); tests inject a fake.
type BrokerClient interface {
	RedeemTicket(ctx context.Context, gw broker.GatewayIdentity, opaque string) (broker.Lease, error)
	RenewLease(ctx context.Context, gw broker.GatewayIdentity, leaseID string, fence broker.Fence) (broker.Lease, error)
	ResolveTarget(ctx context.Context, gw broker.GatewayIdentity, leaseID string) (broker.Target, error)
	RevokeLease(ctx context.Context, leaseID string) error
	// ReportActivity posts one session signal (input/connected/disconnect)
	// for the lease's bound incarnation; the broker stamps receipt time.
	ReportActivity(ctx context.Context, gw broker.GatewayIdentity, leaseID string, fence broker.Fence, ev broker.ActivityEvent) error
}

// Contract constants from design §6.
const (
	// LeaseRenewInterval — the gateway renews/checks every lease this often.
	LeaseRenewInterval = 10 * time.Second
	// RevokeDeadline — after a revoke or a lost-broker failure the session
	// must be dead within this budget (fail closed).
	RevokeDeadline = 30 * time.Second
	// SessionCookieName — host-only Secure/HttpOnly/SameSite session cookie,
	// set only after a successful ticket redemption.
	SessionCookieName = "__Host-tcdi_session"
)

// CookieMode selects the session cookie's cross-site behavior. The portal
// embeds the session origin in an iframe, so the cookie must reach the
// session origin from inside that frame.
type CookieMode string

const (
	// CookieModeLax (default) issues SameSite=Lax. Browsers send it from the
	// portal's iframe only when portal and session origins share a
	// registrable domain (same site, e.g. workspace.example.com and
	// workspace-session.example.com). Cross-site portals still work via
	// "open in new tab" (a top-level navigation), not in-portal.
	CookieModeLax CookieMode = "lax"
	// CookieModePartitioned issues SameSite=None; Secure; Partitioned
	// (CHIPS) for deployments where portal and session are different
	// sites: the cookie is keyed to the embedding portal's site, so it is
	// usable inside that portal's iframe and nowhere else.
	CookieModePartitioned CookieMode = "partitioned"
)

// ParseCookieMode validates a -session-cookie-mode value; "" means lax.
func ParseCookieMode(v string) (CookieMode, error) {
	switch m := CookieMode(strings.ToLower(strings.TrimSpace(v))); m {
	case "", CookieModeLax:
		return CookieModeLax, nil
	case CookieModePartitioned:
		return m, nil
	}
	return "", fmt.Errorf("gateway: unknown session cookie mode %q (want lax or partitioned)", v)
}

// Config wires a Gateway.
type Config struct {
	// Identity this gateway presents to the broker.
	Identity broker.GatewayIdentity
	// SessionDomain replaces PublicOrigin (design §3.2, D9). Launch and the
	// desktop proxy are served only on <label>.<SessionDomain>, where the
	// label names the workspace the request is for.
	SessionDomain sessionhost.Domain
	// PortalOrigins is the allowlist of portal origins permitted to POST
	// /v1/launch (ADR 0004): portal and session live on different
	// registrable domains, so a real launch arrives cross-site with
	// Origin=<portal origin>. Entries must be serialized origins
	// (scheme://host[:port], no path); matching is exact on
	// scheme+host+port with default ports normalized. The allowlist does
	// not relax any other route — WebSocket upgrades and the desktop
	// proxy still require Origin == the request's own workspace origin.
	PortalOrigins []string
	// ControlHosts is the Host allowlist for /healthz and /v1/control/*
	// (in-cluster Service names). It replaces AllowedHosts; the launch and
	// proxy surface takes its hosts from SessionDomain instead.
	ControlHosts []string
	// CookieMode selects the session cookie attributes (default lax).
	CookieMode CookieMode
	// Broker is required.
	Broker BrokerClient
	// Sessions is the optional session directory (design §3.6, P6): when
	// set, launch binds the cookie digest to the lease, a cookie this
	// replica never saw rehydrates its session from the store, and stream
	// admission claims the lease's stream epoch so a newer claim on any
	// replica fences this process's stream. Nil keeps v0.1 single-process
	// session semantics.
	Sessions SessionDirectory
	// UpstreamCA is the fallback trust root for runtime upstream TLS when a
	// resolved Target carries no TLSCA; verification is never skipped.
	UpstreamCA *x509.CertPool
	// ControlToken authenticates the operator/control endpoints. When it is
	// empty the control surface is disabled entirely (never fail open on
	// the public listener); when set, an absent bearer must be denied
	// (SEC-2).
	ControlToken string
	// RenewInterval / RevokeDeadline are injectable for tests.
	RenewInterval  time.Duration
	RevokeDeadline time.Duration
	// InputReportInterval is the minimum spacing between "input" activity
	// reports per session (default InputReportInterval); injectable.
	InputReportInterval time.Duration
	// Now injects a clock for tests.
	Now func() time.Time
	// Metrics, Audit and Logger are optional observability sinks; all three
	// are nil-safe.
	Metrics *observability.Metrics
	Audit   observability.AuditSink
	Logger  *slog.Logger
}

// Gateway is the session-domain HTTP handler: launch, cookie-gated desktop
// proxy on per-workspace hosts, and bearer-gated control endpoints on the
// in-cluster control hosts — one mux, two disjoint host classes.
type Gateway struct {
	cfg           Config
	portalOrigins []*url.URL
	controlHosts  map[string]bool
	embedders     []string // serialized portal origins for frame-ancestors
	proxy         *httputil.ReverseProxy
	permissions   string // Permissions-Policy pinned on every response

	mu          sync.Mutex
	sessions    map[string]*session                     // cookie value -> session
	byLease     map[string]*session                     // lease ID -> session
	byWorkspace map[string]*session                     // workspace UID -> session (takeover fence)
	inflight    map[broker.SessionDigest]*rehydrateCall // digest -> shared lookup
	// inflightWaiters counts requests parked on a shared lookup's done
	// channel — test instrumentation that makes the dedup rendezvous
	// observable (read through export_test.go).
	inflightWaiters int
	draining        bool          // set by Drain: refuse new upgrades
	done            chan struct{} // closed by Close
	closeOnce       sync.Once
}

func (g *Gateway) now() time.Time { return g.cfg.Now() }

// New builds the session gateway handler.
func New(cfg Config) (*Gateway, error) {
	if cfg.Broker == nil || cfg.SessionDomain.String() == "" {
		return nil, errors.New("gateway: Config requires Broker and SessionDomain")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.RenewInterval <= 0 {
		cfg.RenewInterval = LeaseRenewInterval
	}
	if cfg.RevokeDeadline <= 0 {
		cfg.RevokeDeadline = RevokeDeadline
	}
	if cfg.InputReportInterval <= 0 {
		cfg.InputReportInterval = InputReportInterval
	}
	mode, err := ParseCookieMode(string(cfg.CookieMode))
	if err != nil {
		return nil, err
	}
	cfg.CookieMode = mode
	g := &Gateway{
		cfg:          cfg,
		controlHosts: map[string]bool{},
		sessions:     map[string]*session{},
		byLease:      map[string]*session{},
		byWorkspace:  map[string]*session{},
		inflight:     map[broker.SessionDigest]*rehydrateCall{},
		done:         make(chan struct{}),
	}
	for _, po := range cfg.PortalOrigins {
		u, ok := parseOrigin(strings.TrimSpace(po))
		if !ok {
			return nil, fmt.Errorf("gateway: PortalOrigins entry %q must be a scheme://host[:port] origin", po)
		}
		ser, ok := serializeOrigin(u)
		if !ok {
			return nil, fmt.Errorf("gateway: PortalOrigins entry %q is not a plain origin", po)
		}
		g.portalOrigins = append(g.portalOrigins, u)
		g.embedders = append(g.embedders, ser)
	}
	g.permissions = sessionPermissionsPolicy(g.embedders)
	for _, h := range cfg.ControlHosts {
		g.controlHosts[h] = true
	}
	g.proxy = &httputil.ReverseProxy{
		Director:       g.direct,
		Transport:      sessionRoundTripper{},
		ModifyResponse: g.responsePolicy,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if g.cfg.Logger != nil {
				g.cfg.Logger.Info("upstream error", "path", r.URL.Path, "err", err)
			}
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "upstream_unreachable"})
		},
	}
	return g, nil
}

// Close stops renew loops and kills every live session.
func (g *Gateway) Close() {
	g.closeOnce.Do(func() {
		close(g.done)
		g.mu.Lock()
		all := make([]*session, 0, len(g.sessions))
		for _, s := range g.sessions {
			all = append(all, s)
		}
		g.sessions = map[string]*session{}
		g.byLease = map[string]*session{}
		g.byWorkspace = map[string]*session{}
		g.mu.Unlock()
		for _, s := range all {
			s.kill()
		}
	})
}

// ServeHTTP routes by host class first: workspace hosts (a label that
// Domain.Match resolves under the session domain) get launch and the
// cookie-gated desktop proxy; configured control hosts get /healthz and
// the bearer-gated /v1/control/* endpoints. Anything else is misdirected.
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	wsID, wsOK := g.cfg.SessionDomain.Match(r.Host)
	controlOK := g.controlHostOK(r)

	// SEC-07/D14: pin the session-origin security policy on every response
	// (proxied or not). Proxied upstream copies of these headers are
	// dropped in responsePolicy so a hostile runtime cannot weaken them.
	// Framing is governed by CSP frame-ancestors (portal origins only) —
	// never X-Frame-Options, which cannot express an allowlist and would
	// break the in-portal session view. The CSP names the request's own
	// host in connect-src; an unmatched Host is attacker-controlled, so
	// the configured domain stands in for it rather than echoing bytes a
	// request smuggled into the Host header.
	cspHost := r.Host
	if !wsOK && !controlOK {
		cspHost = g.cfg.SessionDomain.String()
	}
	h := w.Header()
	h.Set("Content-Security-Policy", sessionCSP(cspHost, g.embedders))
	h.Set("Permissions-Policy", g.permissions)
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
	h.Set("Origin-Agent-Cluster", "?1")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")

	rec := &statusRecorder{ResponseWriter: w}
	start := g.now()
	route := "proxy"
	switch {
	case r.URL.Path == LaunchPath:
		route = "launch"
		if !wsOK {
			writeJSON(rec, http.StatusMisdirectedRequest, map[string]string{"error": "bad_host"})
			break
		}
		g.handleLaunch(rec, r, wsID)
	case strings.HasPrefix(r.URL.Path, "/v1/control/") || r.URL.Path == "/healthz":
		// The operator surface exists only on the in-cluster control
		// hosts; on workspace or foreign hosts these paths do not exist.
		if !controlOK {
			writeJSON(rec, http.StatusNotFound, map[string]string{"error": "not_found"})
			break
		}
		if r.URL.Path == "/healthz" {
			route = "healthz"
			writeJSON(rec, http.StatusOK, map[string]string{"status": "ok"})
		} else {
			route = "control"
			g.serveControl(rec, r)
		}
	default:
		if !wsOK {
			writeJSON(rec, http.StatusMisdirectedRequest, map[string]string{"error": "bad_host"})
			break
		}
		g.serveProxy(rec, r, wsID)
	}
	if g.cfg.Metrics != nil {
		g.cfg.Metrics.ObserveHTTP(route, r.Method, codeClass(rec.status), g.now().Sub(start))
	}
}

// ---------------------------------------------------------------------------
// Control plane (operator surface, SEC-2 bearer-gated)
// ---------------------------------------------------------------------------

func (g *Gateway) controlAuth(r *http.Request) bool {
	if g.cfg.ControlToken == "" {
		return false // no token configured: control surface closed (SEC-2)
	}
	const p = "Bearer "
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, p) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(h, p)), []byte(g.cfg.ControlToken)) == 1
}

// serveControl is reached only on a control host (ServeHTTP gates the
// path); the bearer check is the remaining gate.
func (g *Gateway) serveControl(w http.ResponseWriter, r *http.Request) {
	if !g.controlAuth(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	switch r.URL.Path {
	case "/v1/control/session":
		g.handleControlSession(w, r)
	case "/v1/control/revoke":
		g.handleControlRevoke(w, r)
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
	}
}

func (g *Gateway) handleControlSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method"})
		return
	}
	g.mu.Lock()
	all := make([]*session, 0, len(g.sessions))
	for _, s := range g.sessions {
		all = append(all, s)
	}
	g.mu.Unlock()
	type view struct {
		LeaseID     string `json:"leaseId"`
		Connections int    `json:"connections"`
		Transport   string `json:"transport,omitempty"`
	}
	var out []view
	for _, s := range all {
		if !s.live(g) {
			continue
		}
		v := view{LeaseID: s.leaseID(), Connections: s.streamCount()}
		if v.Connections > 0 {
			v.Transport = "websocket"
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{"active": len(out) > 0, "sessions": out})
}

// handleControlRevoke kills the named lease's session and tells the broker.
// SEC-1: an identifier that resolves to no live session must not affect any
// other session — there is no implicit "current session" fallback.
func (g *Gateway) handleControlRevoke(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method"})
		return
	}
	var body struct {
		LeaseID string `json:"leaseId"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_body"})
		return
	}
	if body.LeaseID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_lease"})
		return
	}
	g.mu.Lock()
	s := g.byLease[body.LeaseID]
	g.mu.Unlock()
	if s == nil {
		writeJSON(w, http.StatusOK, map[string]bool{"revoked": false})
		return
	}
	g.killSession(s, "control_revoke")
	// Best-effort broker revoke; the local session is already dead either way.
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	_ = g.cfg.Broker.RevokeLease(ctx, body.LeaseID)
	cancel()
	g.audit(r, "session.revoke", s.workspaceUID(), observability.OutcomeSuccess, "")
	writeJSON(w, http.StatusOK, map[string]bool{"revoked": true})
}

// ---------------------------------------------------------------------------
// Desktop proxy
// ---------------------------------------------------------------------------

type ctxKey int

const ctxKeySession ctxKey = iota

// serveProxy is the desktop catch-all on a workspace host (ServeHTTP
// already resolved wsID from the request Host): session auth -> host
// binding -> upgrade admission -> authenticated reverse proxy to the
// resolved runtime target.
func (g *Gateway) serveProxy(w http.ResponseWriter, r *http.Request, wsID string) {
	if r.URL.Query().Get(TicketField) != "" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "ticket_in_query"})
		return
	}
	clean, ok := cleanedProxyPath(r.URL)
	if !ok || !upstreamPathOK(clean) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	// SEC-07: refuse service-worker registration fetches on the shared
	// session origin — a registered worker would MITM every later request,
	// including other tenants' sessions on this origin.
	if strings.EqualFold(strings.TrimSpace(r.Header.Get("Service-Worker")), "script") {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "service_worker_forbidden"})
		return
	}
	// A malformed or non-websocket upgrade offer (Upgrade header present
	// but not a complete RFC6455 request) must never be proxied: a foreign
	// upgrade token would reach upstream bypassing originOK+admitUpgrade,
	// and a stray Upgrade header must not consume the stream slot (SEC-3).
	if r.Header.Get("Upgrade") != "" && !isUpgrade(r) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_upgrade"})
		return
	}
	s, lookupErr := g.lookupSession(r, wsID)
	if lookupErr != nil {
		// The session directory could not answer: not "no session". 503
		// so the browser retries; nothing is cached.
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "unavailable"})
		return
	}
	if s != nil && s.workspaceUID() != wsID {
		// D11: a session bound to a different workspace is "absent" on
		// this host — the cookie is host-only and can never arrive here
		// through a browser, so a mismatch means replay or confusion.
		// Answer as if no session existed and audit; the session itself
		// stays live on its own host.
		g.audit(r, "session.host_mismatch", s.workspaceUID(), observability.OutcomeDenied, "host_mismatch")
		s = nil
	}
	if s == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	var gen int
	var streamEpoch uint64 // the epoch this stream claimed; 0 without a directory
	if isUpgrade(r) {
		if g.isDraining() {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "unavailable"})
			return
		}
		if !g.originOK(r) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "bad_origin"})
			return
		}
		var ok bool
		if gen, ok = s.admitUpgrade(); !ok {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "stream_in_use"})
			return
		}
		// Release only our own reservation: if this stream was fenced by a
		// newer upgrade, the defer is a no-op and cannot clear the
		// successor's admission.
		defer s.endUpgrade(gen)
		// With a session directory the lease's stream epoch is the
		// cross-replica fence: claiming it here makes the previous
		// replica's renew loop drop its copy of this stream (P3).
		if g.cfg.Sessions != nil {
			epoch, err := g.claimStream(r.Context(), s)
			streamEpoch = epoch
			if err != nil {
				if terminalBrokerErr(err) {
					g.killSession(s, "claim_"+leaseFailureReason(err))
					writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "session_revoked"})
					return
				}
				writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "unavailable"})
				return
			}
		}
	}
	if err := g.ensureTarget(r.Context(), s); err != nil {
		if terminalBrokerErr(err) {
			g.killSession(s, "target_"+leaseFailureReason(err))
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "session_revoked"})
			return
		}
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "target_unresolvable"})
		return
	}

	var captured net.Conn
	hw := &hijackCapture{
		ResponseWriter: w,
		// wrapConn intercepts client->upstream bytes so the sniffer can spot
		// RFB input frames inside the proxied WebSocket stream.
		wrapConn: func(c net.Conn) net.Conn {
			return &sniffingConn{Conn: c, sniffer: &wsFrameSniffer{
				onInput: func() { g.noteInput(s) },
			}}
		},
		onHijack: func(c net.Conn) {
			if s.addConn(c) {
				captured = c
				// The stream is admitted and live: report connected so the
				// broker counts an open stream and cancels any pending
				// disconnect grace window (design §8).
				s.enqueueActivity(broker.ActivityConnected, streamEpoch)
			}
			// The upstream handshake is done and the conn is registered:
			// free the admission slot so a later upgrade on this session
			// fences this conn (reload), while a second upgrade during the
			// handshake itself still gets 409. endUpgrade also runs via the
			// defer above for upgrades that never reach hijack.
			s.endUpgrade(gen)
		},
	}
	ctx, tid, ok := s.track(r.Context())
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "session_revoked"})
		return
	}
	if isUpgrade(r) {
		s.setStreamTrack(tid)
	}
	defer func() {
		if captured != nil {
			// The interactive stream closed — start the disconnect grace
			// window server-side. The FIFO queue keeps this ordered behind
			// any earlier connected report and ahead of a later reconnect.
			// Enqueued BEFORE untrack drops the conn so Drain can trust
			// "no open conns" to mean "disconnect already queued".
			s.enqueueActivity(broker.ActivityDisconnect, streamEpoch)
		}
		s.untrack(tid, captured)
	}()
	g.proxy.ServeHTTP(hw, r.WithContext(context.WithValue(ctx, ctxKeySession, s)))
}

// ensureTarget resolves the upstream once per session and builds the
// pinned-CA transport. ResolveTarget errors classify like renew errors:
// terminal (revoked/stale/denied) kills the session, transient → 502.
func (g *Gateway) ensureTarget(ctx context.Context, s *session) error {
	s.mu.Lock()
	if s.target != nil {
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	t, err := g.cfg.Broker.ResolveTarget(ctx, g.cfg.Identity, s.leaseID())
	cancel()
	if err != nil {
		return err
	}
	up, err := upstreamURL(t)
	if err != nil {
		return err
	}
	pool := x509.NewCertPool()
	switch {
	case len(t.TLSCA) > 0 && pool.AppendCertsFromPEM(t.TLSCA):
		// per-lease pinned bundle (the runtime's self-signed tls.crt)
	case g.cfg.UpstreamCA != nil:
		pool = g.cfg.UpstreamCA
	default:
		// empty pool: verify always runs, so every upstream fails closed
	}
	// TLSServerName pins the SNI/verify name: the runtime's self-signed cert
	// carries SANs for PodName/ServiceName/localhost — NOT the .svc FQDN in
	// the dial address (design-review addendum). Verification is full TLS;
	// never InsecureSkipVerify.
	rt := &http.Transport{
		TLSClientConfig: &tls.Config{
			RootCAs:    pool,
			ServerName: t.TLSServerName,
			MinVersion: tls.VersionTLS12,
		},
		TLSHandshakeTimeout: 10 * time.Second,
	}
	s.mu.Lock()
	s.target = &t
	s.upURL = up
	s.transport = rt
	s.mu.Unlock()
	return nil
}

// upstreamURL resolves the dial URL for a target: UpstreamURL (the contract
// field, e.g. https://ws-<uid>.<ns>.svc:8443) wins; ServiceDNS as bare
// authority is the fallback for older fakes.
func upstreamURL(t broker.Target) (*url.URL, error) {
	raw := t.UpstreamURL
	if raw == "" && t.ServiceDNS != "" {
		raw = "https://" + t.ServiceDNS
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, fmt.Errorf("gateway: target has no usable https upstream")
	}
	return u, nil
}

// resolved returns the lazily-resolved upstream target, its parsed URL and
// the pinned-CA transport; nil when resolution has not happened/failed yet.
func (s *session) resolved() (*broker.Target, *url.URL, http.RoundTripper) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.target, s.upURL, s.transport
}

// direct rewrites the request to the resolved upstream: authority from
// broker.Target.UpstreamURL (never client-supplied — SSRF guard), runtime
// credentials injected server-side, client Authorization/Cookie stripped.
func (g *Gateway) direct(r *http.Request) {
	s, _ := r.Context().Value(ctxKeySession).(*session)
	if s == nil {
		return // Transport will fail; ErrorHandler answers 502
	}
	t, up, _ := s.resolved()
	if t == nil || up == nil {
		return
	}
	r.URL.Scheme = up.Scheme
	r.URL.Host = up.Host
	r.Host = up.Host
	// SEC-20: forward the cleaned path. serveProxy already rejected any path
	// whose decoded or escaped form carries traversal tricks; clearing
	// RawPath forces the canonical escaping of the verified Path onto the
	// wire so the runtime sees exactly what the allowlist authorized.
	r.URL.RawPath = ""
	if t.Username != "" || t.Password != "" {
		r.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString(
			[]byte(t.Username+":"+t.Password)))
	} else {
		r.Header.Del("Authorization")
	}
	r.Header.Del("Cookie")
	// Upstream quirk: KasmVNC requires the legacy Sec-WebSocket-Origin
	// header on upgrades; browsers only send Origin. We have already
	// validated Origin (scheme+exact authority) before proxying, so relay
	// that verified value.
	if isUpgrade(r) {
		r.Header.Set("Sec-WebSocket-Origin", r.Header.Get("Origin"))
	} else {
		r.Header.Del("Sec-WebSocket-Origin")
	}
}

// sessionRoundTripper forwards through the per-session pinned-CA transport.
type sessionRoundTripper struct{}

func (sessionRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	s, _ := r.Context().Value(ctxKeySession).(*session)
	if s == nil {
		return nil, errors.New("gateway: no session on request")
	}
	_, _, rt := s.resolved()
	if rt == nil {
		return nil, errors.New("gateway: upstream not resolved")
	}
	return rt.RoundTrip(r)
}

// sessionCSP is the Content-Security-Policy pinned on every gateway
// response (SEC-07). It is sized to the pinned KasmVNC 1.5.0 web client —
// verified against the sha256-locked deb:
//
//   - script-src 'unsafe-inline': index/vnc/screen.html carry inline
//     bootstrap <script> blocks and an onclick attribute we cannot rewrite
//     (the assets are served by the runtime, not us); 'wasm-unsafe-eval'
//     covers the WebAssembly decoder worker (WebAssembly.instantiate).
//   - style-src 'unsafe-inline': disconnected.html has an inline <style>
//     block and the markup uses style= attributes.
//   - img-src data:: the control bar embeds the Kasm logo as a data: URI.
//   - connect-src wss://<host>: the /websockify desktop socket.
//   - worker-src 'self': decoder and port-relay workers.
//   - frame-src blob:: the print feature renders a PDF blob in a hidden
//     iframe.
//   - frame-ancestors: only the configured portal origins may embed the
//     session (the in-portal session view); with no portal origin
//     configured framing is denied outright ('none').
//
// Every other directive stays default-src 'self' or harder; plugins, forms
// and base-tag rewriting are all denied outright. The policy is per-host:
// connect-src names wss://<this workspace's host>, never a sibling's.
func sessionCSP(host string, embedders []string) string {
	ancestors := "'none'"
	if len(embedders) > 0 {
		ancestors = strings.Join(embedders, " ")
	}
	return "default-src 'self'" +
		"; script-src 'self' 'unsafe-inline' 'wasm-unsafe-eval'" +
		"; style-src 'self' 'unsafe-inline'" +
		"; img-src 'self' data:" +
		"; font-src 'self'" +
		"; connect-src 'self' wss://" + host +
		"; worker-src 'self'" +
		"; media-src 'self'" +
		"; frame-src blob:" +
		"; frame-ancestors " + ancestors +
		"; base-uri 'none'" +
		"; object-src 'none'" +
		"; form-action 'none'"
}

// sessionPermissionsPolicy grants clipboard and fullscreen to the session
// origin itself and to the embedding portal origins only (the portal's
// iframe must also delegate them via its allow attribute); powerful
// features the desktop client never uses are disabled outright.
func sessionPermissionsPolicy(embedders []string) string {
	allow := "self"
	for _, e := range embedders {
		allow += ` "` + e + `"`
	}
	return "clipboard-read=(" + allow + ")" +
		", clipboard-write=(" + allow + ")" +
		", fullscreen=(" + allow + ")" +
		", camera=(), microphone=(), geolocation=(), payment=(), usb=()"
}

// serializeOrigin renders u as a browser-serialized origin for policy
// headers (CSP frame-ancestors, Permissions-Policy): lowercase
// scheme://host with default ports dropped. Anything that could break out
// of a header token is refused.
func serializeOrigin(u *url.URL) (string, bool) {
	host := strings.ToLower(u.Hostname())
	if host == "" || strings.ContainsAny(host, " \t;,'\"()*") {
		return "", false
	}
	if strings.Contains(host, ":") { // IPv6 literal
		host = "[" + host + "]"
	}
	out := u.Scheme + "://" + host
	if p := u.Port(); p != "" && p != effectivePortDefault(u.Scheme) {
		out += ":" + p
	}
	return out, true
}

func effectivePortDefault(scheme string) string {
	return effectivePort(&url.URL{Scheme: scheme})
}

// responsePolicy is the ModifyResponse policy for runtime-proxied responses
// (SEC-07). The runtime is tenant-controlled, so headers that mutate
// session-origin browser state — any Set-Cookie (OBS-4 hardened: now all
// names, not just the session cookie), Service-Worker-Allowed,
// Clear-Site-Data, Refresh, Link (Link: rel=serviceworker registers a
// service worker) — are dropped outright, and upstream-supplied security
// headers are deleted so they can never weaken the policy ServeHTTP pinned.
func (g *Gateway) responsePolicy(resp *http.Response) error {
	for _, h := range []string{
		"Set-Cookie", "Service-Worker-Allowed", "Clear-Site-Data",
		"Refresh", "Link",
		"Content-Security-Policy", "Content-Security-Policy-Report-Only",
		"X-Content-Type-Options", "Strict-Transport-Security",
		"X-Frame-Options", "Permissions-Policy", "Feature-Policy",
		"Cross-Origin-Resource-Policy", "Origin-Agent-Cluster",
	} {
		resp.Header.Del(h)
	}
	return nil
}

// hijackCapture keeps the real net.Conn of upgraded (WebSocket) requests so
// revoke/fencing can close active streams. wrapConn, when set, replaces the
// conn handed to the reverse proxy's copy loop — letting us observe
// client->upstream bytes transparently.
type hijackCapture struct {
	http.ResponseWriter
	onHijack func(net.Conn)
	wrapConn func(net.Conn) net.Conn
}

func (h *hijackCapture) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	// ResponseController walks the Unwrap chain past middleware recorders.
	c, rw, err := http.NewResponseController(h.ResponseWriter).Hijack()
	if err == nil && h.wrapConn != nil {
		c = h.wrapConn(c)
	}
	if err == nil && h.onHijack != nil {
		h.onHijack(c)
	}
	return c, rw, err
}

func (h *hijackCapture) Flush() {
	if f, ok := h.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (h *hijackCapture) Unwrap() http.ResponseWriter { return h.ResponseWriter }

// statusRecorder captures the response status for metrics; it forwards
// Hijack/Flush/Unwrap so wrapping never breaks WebSocket upgrades.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

func codeClass(status int) string {
	switch {
	case status >= 100 && status < 600:
		return string(rune('0'+status/100)) + "xx"
	default:
		return "unknown"
	}
}
