package gateway_test

// Shared fakes for the gateway contract tests. The fake broker plays the
// session-broker role so gateway tests express the full intended flow; the
// gateway stub still answers 501 so every test is red for the right reason.

import (
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/broker"
	"github.com/tinyorbitvn/tinycdi/internal/gateway"
	"github.com/tinyorbitvn/tinycdi/internal/observability"
	"github.com/tinyorbitvn/tinycdi/internal/sessionhost"
)

const (
	testDomain = "session.test" // the session domain tests run under
	// testWSUID/testHost are the default workspace's platform ID and its
	// per-workspace session host; testWSUID2/testHost2 are a second
	// workspace for host-binding tests.
	testWSUID   = "ws_0000000a"
	testHost    = "ws-0000000a." + testDomain
	testOrigin  = "https://" + testHost // the workspace's own session origin
	testWSUID2  = "ws_0000000b"
	testHost2   = "ws-0000000b." + testDomain
	testOrigin2 = "https://" + testHost2
	// testControlHost is the in-cluster Service name /v1/control/* and
	// /healthz answer on.
	testControlHost   = "backend.tinycdi.svc"
	testRenewInterval = 25 * time.Millisecond // RenewInterval newGateway uses
)

var testSessionDomain = mustParseDomain(testDomain)

func mustParseDomain(s string) sessionhost.Domain {
	d, err := sessionhost.ParseDomain(s)
	if err != nil {
		panic(err)
	}
	return d
}

// fakeBroker is a scripted broker.BrokerClient: tickets map to the lease a
// redeem would mint; renewErr/resolveErr inject broker-side failures. It
// also plays the gateway.SessionDirectory: two test gateways sharing one
// fakeBroker stand for two replicas of the same deployment.
type fakeBroker struct {
	mu        sync.Mutex
	leases    map[string]broker.Lease // ticket token -> lease to mint
	redeemed  map[string]bool
	renewErr  map[string]error
	resolveT  map[string]broker.Target
	revokedAt map[string]bool
	revokes   map[string]int         // lease ID -> RevokeLease call count
	activity  []broker.ActivityEvent // recorded ReportActivity types, in order
	actLease  []string               // lease IDs parallel to activity
	actErr    error                  // injected ReportActivity failure
	upstream  *httptest.Server       // default runtime upstream scripted per lease

	// SessionDirectory stand-in: digests bind cookie digests to lease IDs,
	// epochs is the per-lease stream epoch ClaimStream increments.
	digests map[broker.SessionDigest]string
	epochs  map[string]uint64
	bindErr error          // injected BindSession failure
	renewBy map[string]int // renew calls per gateway identity
	lookupN int            // LeaseBySession calls
	revokeN int            // RevokeLease calls
	// lookupGate, when set, makes every LeaseBySession call block on the
	// channel — a scripted rendezvous for the concurrent-miss test, so the
	// overlap is deterministic rather than timing-dependent.
	lookupGate chan struct{}
}

func newFakeBroker(t *testing.T) *fakeBroker {
	t.Helper()
	fb := &fakeBroker{
		leases:    map[string]broker.Lease{},
		redeemed:  map[string]bool{},
		renewErr:  map[string]error{},
		resolveT:  map[string]broker.Target{},
		revokedAt: map[string]bool{},
		revokes:   map[string]int{},
		digests:   map[broker.SessionDigest]string{},
		epochs:    map[string]uint64{},
		renewBy:   map[string]int{},
		upstream:  httptest.NewTLSServer(http.HandlerFunc(fakeUpstream)),
	}
	t.Cleanup(fb.upstream.Close)
	return fb
}

// scriptTicket makes ticket redeemable into a lease for wsUID and resolves
// the lease's target to the fake broker's TLS upstream.
func (f *fakeBroker) scriptTicket(ticket, wsUID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var rnd [4]byte
	_, _ = rand.Read(rnd[:])
	l := broker.Lease{
		ID:                "lease-" + hex.EncodeToString(rnd[:]),
		WorkspaceUID:      wsUID,
		TenantID:          "tenant-a",
		PrincipalSubject:  "iss|alice",
		RuntimeGeneration: 1,
		RuntimeUID:        "rt-1",
		FencingVersion:    1,
		GatewayID:         "gw-test",
		ExpiresAt:         time.Now().Add(broker.LeaseTTL),
	}
	f.leases[ticket] = l
	f.resolveT[l.ID] = targetFor(f.upstream)
}

// targetFor builds a broker.Target pointing at a TLS httptest server, pinning
// its leaf certificate as the CA bundle.
func targetFor(srv *httptest.Server) broker.Target {
	return broker.Target{
		Protocol:      "kasmvnc-websocket",
		ServiceDNS:    strings.TrimPrefix(srv.URL, "https://"),
		UpstreamURL:   srv.URL,
		TLSServerName: "127.0.0.1", // httptest leaf carries IP SANs
		TLSCA: pem.EncodeToMemory(&pem.Block{
			Type:  "CERTIFICATE",
			Bytes: srv.Certificate().Raw,
		}),
		Username: "kasm_user",
		Password: "upstream-secret",
	}
}

// fakeUpstream is the stand-in runtime: it answers 200 to plain GETs and
// completes a websocket upgrade with a raw echo loop (enough for the proxy's
// duplex-copy path, fencing and revoke tests).
func fakeUpstream(w http.ResponseWriter, r *http.Request) {
	if upgradeOffer(r) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "no hijack", http.StatusInternalServerError)
			return
		}
		c, rw, err := hj.Hijack()
		if err != nil {
			return
		}
		defer c.Close()
		sum := sha1.Sum([]byte(r.Header.Get("Sec-WebSocket-Key") +
			"258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\n" +
			"Upgrade: websocket\r\nConnection: upgrade\r\n" +
			"Sec-WebSocket-Accept: " + base64.StdEncoding.EncodeToString(sum[:]) +
			"\r\n\r\n")
		_ = rw.Flush()
		_, _ = io.Copy(struct{ io.Writer }{c}, struct{ io.Reader }{rw})
		return
	}
	// A hostile upstream trying to overwrite the gateway session cookie —
	// the gateway must strip it (OBS-4).
	w.Header().Set("Set-Cookie", gateway.SessionCookieName+"=forged; Path=/")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// upgradeOffer is the test-local net/http upgrade check (the gateway's own
// isUpgrade stays internal).
func upgradeOffer(r *http.Request) bool {
	conn := false
	for _, v := range r.Header.Values("Connection") {
		for _, tok := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(tok), "upgrade") {
				conn = true
			}
		}
	}
	return conn && strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}

func (f *fakeBroker) RedeemTicket(_ context.Context, gw broker.GatewayIdentity, opaque string) (broker.Lease, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	l, ok := f.leases[opaque]
	if !ok || f.redeemed[opaque] || f.revokedAt[opaque] {
		return broker.Lease{}, broker.ErrTicketInvalid
	}
	f.redeemed[opaque] = true
	l.GatewayID = gw.ID
	return l, nil
}

func (f *fakeBroker) RenewLease(_ context.Context, gw broker.GatewayIdentity, leaseID string, _ broker.Fence) (broker.Lease, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.renewBy[gw.ID]++
	if err := f.renewErr[leaseID]; err != nil {
		return broker.Lease{}, err
	}
	for _, l := range f.leases {
		if l.ID == leaseID {
			l.ExpiresAt = time.Now().Add(broker.LeaseTTL)
			l.StreamEpoch = f.epochs[leaseID]
			return l, nil
		}
	}
	return broker.Lease{}, broker.ErrLeaseInvalid
}

func (f *fakeBroker) ResolveTarget(_ context.Context, _ broker.GatewayIdentity, leaseID string) (broker.Target, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.renewErr[leaseID]; err != nil {
		return broker.Target{}, err
	}
	if tgt, ok := f.resolveT[leaseID]; ok {
		return tgt, nil
	}
	return broker.Target{}, broker.ErrLeaseInvalid
}

func (f *fakeBroker) RevokeLease(_ context.Context, leaseID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revokeN++
	f.renewErr[leaseID] = broker.ErrRevoked
	f.revokes[leaseID]++
	return nil
}

// leaseRevokeCount reports how many times RevokeLease ran for the lease —
// the host-mismatch tests pin exactly one call.
func (f *fakeBroker) leaseRevokeCount(leaseID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.revokes[leaseID]
}

// BindSession records the digest binding launch established. A second
// digest for the same lease is refused like the real directory.
func (f *fakeBroker) BindSession(_ context.Context, _ broker.GatewayIdentity, leaseID string, d broker.SessionDigest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.bindErr != nil {
		return f.bindErr
	}
	if cur, ok := f.digests[d]; ok && cur != leaseID {
		return broker.ErrDenied
	}
	for other, id := range f.digests {
		if id == leaseID && other != d {
			return broker.ErrDenied
		}
	}
	f.digests[d] = leaseID
	return nil
}

// LeaseBySession resolves a cookie digest to its scripted lease, applying
// the same liveness view RenewLease has (a revoked/failed lease is dead).
// When a lookup gate is scripted the call blocks until the test closes it,
// so tests can hold the lookup open while more requests pile up behind it.
func (f *fakeBroker) LeaseBySession(_ context.Context, _ broker.GatewayIdentity, d broker.SessionDigest) (broker.Lease, error) {
	f.mu.Lock()
	f.lookupN++
	gate := f.lookupGate
	f.mu.Unlock()
	if gate != nil {
		<-gate
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	leaseID, ok := f.digests[d]
	if !ok {
		return broker.Lease{}, broker.ErrLeaseInvalid
	}
	if err := f.renewErr[leaseID]; err != nil {
		return broker.Lease{}, err
	}
	for _, l := range f.leases {
		if l.ID == leaseID {
			l.StreamEpoch = f.epochs[leaseID]
			return l, nil
		}
	}
	return broker.Lease{}, broker.ErrLeaseInvalid
}

// ClaimStream bumps the lease's stream epoch so the replica holding the
// previous stream sees it on its next renew.
func (f *fakeBroker) ClaimStream(_ context.Context, _ broker.GatewayIdentity, leaseID string, _ broker.Fence) (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.renewErr[leaseID]; err != nil {
		return 0, err
	}
	f.epochs[leaseID]++
	return f.epochs[leaseID], nil
}

// renewCount reports how many RenewLease calls arrived under gateway ID
// gwID — how tests tell which replica is renewing a shared lease.
func (f *fakeBroker) renewCount(gwID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.renewBy[gwID]
}

// gateLookups makes every subsequent LeaseBySession call block on the
// returned channel until the test closes it. The count of calls that have
// entered LeaseBySession is readable via lookupCount while they block.
func (f *fakeBroker) gateLookups() chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lookupGate = make(chan struct{})
	return f.lookupGate
}

// lookupCount reports the LeaseBySession call count.
func (f *fakeBroker) lookupCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lookupN
}

// revokeCount reports the RevokeLease call count.
func (f *fakeBroker) revokeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.revokeN
}

// setBindErr makes BindSession fail (directory unavailable at launch).
func (f *fakeBroker) setBindErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bindErr = err
}

func (f *fakeBroker) ReportActivity(_ context.Context, _ broker.GatewayIdentity, leaseID string, _ broker.Fence, ev broker.ActivityEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.activity = append(f.activity, ev)
	f.actLease = append(f.actLease, leaseID)
	return f.actErr
}

// activityTypes returns the recorded activity event types in order.
func (f *fakeBroker) activityTypes() []broker.ActivityEventType {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]broker.ActivityEventType, len(f.activity))
	for i, ev := range f.activity {
		out[i] = ev.Type
	}
	return out
}

// failRenew makes the broker answer renew/resolve for leaseID with err
// (simulating a revoked lease or unreachable broker → fail closed).
func (f *fakeBroker) failRenew(leaseID string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.renewErr[leaseID] = err
}

func (f *fakeBroker) wasRedeemed(ticket string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.redeemed[ticket]
}

func (f *fakeBroker) leaseOf(t *testing.T, ticket string) broker.Lease {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	l, ok := f.leases[ticket]
	if !ok {
		t.Fatalf("no scripted lease for ticket")
	}
	return l
}

// auditRecorder captures every audit event the gateway emits.
type auditRecorder struct {
	mu     sync.Mutex
	events []observability.AuditEvent
}

func (a *auditRecorder) WriteAudit(_ context.Context, e observability.AuditEvent) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events = append(a.events, e)
	return nil
}

// auditActions returns the recorded action names in order.
func (a *auditRecorder) auditActions() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, len(a.events))
	for i, e := range a.events {
		out[i] = e.Action
	}
	return out
}

// newGateway builds the gateway handler on an httptest server. The session
// domain defaults to testDomain and /v1/control/* + /healthz answer only on
// testControlHost.
func newGateway(t *testing.T, fb *fakeBroker, mutate func(*gateway.Config)) *httptest.Server {
	t.Helper()
	_, srv := newGatewayHandle(t, fb, mutate)
	return srv
}

// newGatewayHandle is newGateway but also returns the *gateway.Gateway, so
// tests can drive lifecycle methods (Drain) directly.
func newGatewayHandle(t *testing.T, fb *fakeBroker, mutate func(*gateway.Config)) (*gateway.Gateway, *httptest.Server) {
	t.Helper()
	cfg := gateway.Config{
		Identity:       broker.GatewayIdentity{ID: "gw-test", Audience: testDomain},
		SessionDomain:  testSessionDomain,
		ControlHosts:   []string{testControlHost},
		Broker:         fb,
		ControlToken:   "control-test-token",
		RenewInterval:  testRenewInterval, // fast cadence so revoke tests don't sleep
		RevokeDeadline: 150 * time.Millisecond,
		Now:            time.Now,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	h, err := gateway.New(cfg)
	if err != nil {
		t.Fatalf("gateway.New: %v", err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return h, srv
}

// newReplica builds a gateway wired to the shared session directory under
// its own (recorded) identity — one replica of the deployment. A distinct
// ID is how the fake attributes renews; real replicas share one identity.
func newReplica(t *testing.T, fb *fakeBroker, id string) (*gateway.Gateway, *httptest.Server) {
	t.Helper()
	return newGatewayHandle(t, fb, func(c *gateway.Config) {
		c.Identity = broker.GatewayIdentity{ID: id, Audience: "session.example.dev"}
		c.Sessions = fb
	})
}

// doLaunch POSTs the ticket to /v1/launch on the given request Host with
// the given header tweaks and returns the response without following the
// redirect.
func doLaunch(t *testing.T, srv *httptest.Server, host, ticket string, hdr map[string]string) *http.Response {
	t.Helper()
	form := url.Values{gateway.TicketField: {ticket}}
	req, err := http.NewRequest(http.MethodPost, srv.URL+gateway.LaunchPath, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("build launch request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Host = host
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := srv.Client().Transport.(*http.Transport).RoundTrip(req)
	if err != nil {
		t.Fatalf("launch roundtrip: %v", err)
	}
	return resp
}

// launchOK performs a valid launch on host and returns the session cookie
// value.
func launchOK(t *testing.T, srv *httptest.Server, host, ticket string) string {
	t.Helper()
	resp := doLaunch(t, srv, host, ticket, map[string]string{
		"Origin":         "https://" + host,
		"Sec-Fetch-Site": "same-origin",
	})
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("launch status = %d, want 303", resp.StatusCode)
	}
	for _, c := range resp.Cookies() {
		if c.Name == gateway.SessionCookieName {
			return c.Value
		}
	}
	t.Fatalf("launch returned no %s cookie", gateway.SessionCookieName)
	return ""
}

// proxied does a GET through the gateway on the given request Host with a
// session cookie and headers.
func proxied(t *testing.T, srv *httptest.Server, host, path, cookie string, hdr map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv.URL+path, nil)
	if err != nil {
		t.Fatalf("build proxy request: %v", err)
	}
	req.Host = host
	if cookie != "" {
		req.Header.Set("Cookie", gateway.SessionCookieName+"="+cookie)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := srv.Client().Transport.(*http.Transport).RoundTrip(req)
	if err != nil {
		t.Fatalf("proxy roundtrip: %v", err)
	}
	return resp
}

// drain reads and discards a response body.
func drain(resp *http.Response) {
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}

// upgrade performs a raw WebSocket upgrade request on the given request
// Host, returning the response; on 101 the body is the live socket.
func upgrade(t *testing.T, srv *httptest.Server, host, path, cookie string, hdr map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv.URL+path, nil)
	if err != nil {
		t.Fatalf("build upgrade request: %v", err)
	}
	req.Host = host
	req.Header.Set("Connection", "upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	if cookie != "" {
		req.Header.Set("Cookie", gateway.SessionCookieName+"="+cookie)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := srv.Client().Transport.(*http.Transport).RoundTrip(req)
	if err != nil {
		t.Fatalf("upgrade roundtrip: %v", err)
	}
	return resp
}

// upstreamRecorder is a fake runtime upstream that records what reaches it.
type upstreamRecorder struct {
	mu        sync.Mutex
	gotAuth   string
	gotCookie string
	uris      []string
	hits      int
	srv       *httptest.Server
}

func newUpstream(t *testing.T) *upstreamRecorder {
	u := &upstreamRecorder{}
	u.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.hits++
		u.gotAuth = r.Header.Get("Authorization")
		u.gotCookie = r.Header.Get("Cookie")
		u.uris = append(u.uris, r.RequestURI)
		u.mu.Unlock()
		fakeUpstream(w, r)
	}))
	t.Cleanup(u.srv.Close)
	return u
}

// seenURIs returns the upstream RequestURIs observed so far.
func (u *upstreamRecorder) seenURIs() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string{}, u.uris...)
}

// pointLeaseAt rewrites the scripted ticket's resolved target to srv —
// for tests that need a hostile or instrumented upstream.
func (f *fakeBroker) pointLeaseAt(t *testing.T, ticket string, srv *httptest.Server) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	l, ok := f.leases[ticket]
	if !ok {
		t.Fatalf("no scripted lease for ticket")
	}
	f.resolveT[l.ID] = targetFor(srv)
}
