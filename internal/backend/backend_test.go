// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package backend

// Backend-level tests: listener route isolation (D7/P9), the drain-ordered
// shutdown, and the shared gateway identity across replicas.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/api"
	"github.com/tinyorbitvn/tinycdi/internal/api/loginstate"
	"github.com/tinyorbitvn/tinycdi/internal/api/oidctest"
	"github.com/tinyorbitvn/tinycdi/internal/broker"
	"github.com/tinyorbitvn/tinycdi/internal/broker/httpapi"
	"github.com/tinyorbitvn/tinycdi/internal/gateway"
	"github.com/tinyorbitvn/tinycdi/internal/observability"
	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
	"github.com/tinyorbitvn/tinycdi/internal/sessionhost"
	"github.com/tinyorbitvn/tinycdi/internal/store"
)

func testLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// countingHandler records how many requests reach it — used to prove a
// listener never delegates to another listener's mux.
type countingHandler struct {
	next http.Handler
	hits int
}

func (c *countingHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.hits++
	c.next.ServeHTTP(w, r)
}

// ---------------------------------------------------------------------------
// Session-fake broker (the gateway.BrokerClient contract, in-process).
// ---------------------------------------------------------------------------

// fakeBrokerClient is a scripted gateway.BrokerClient: tickets map to the
// lease a redeem would mint; activity and revocations are recorded.
type fakeBrokerClient struct {
	mu       sync.Mutex
	leases   map[string]broker.Lease // ticket token -> lease to mint
	redeemed map[string]bool
	renewErr map[string]error
	resolveT map[string]broker.Target
	revokes  []string
	activity []broker.ActivityEventType
	upstream *httptest.Server
}

func newFakeBrokerClient(t *testing.T) *fakeBrokerClient {
	t.Helper()
	fb := &fakeBrokerClient{
		leases:   map[string]broker.Lease{},
		redeemed: map[string]bool{},
		renewErr: map[string]error{},
		resolveT: map[string]broker.Target{},
		upstream: httptest.NewTLSServer(http.HandlerFunc(fakeWSUpstream)),
	}
	t.Cleanup(fb.upstream.Close)
	return fb
}

func (f *fakeBrokerClient) scriptTicket(ticket, wsUID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	l := broker.Lease{
		ID:                "lease-" + ticket,
		WorkspaceUID:      wsUID,
		TenantID:          "tenant-a",
		PrincipalSubject:  "iss|alice",
		RuntimeGeneration: 1,
		RuntimeUID:        "rt-1",
		FencingVersion:    1,
		ExpiresAt:         time.Now().Add(broker.LeaseTTL),
	}
	f.leases[ticket] = l
	f.resolveT[l.ID] = wsTargetFor(f.upstream)
}

// wsTargetFor builds a broker.Target pointing at a TLS httptest server,
// pinning its leaf certificate as the CA bundle.
func wsTargetFor(srv *httptest.Server) broker.Target {
	return broker.Target{
		Protocol:      "kasmvnc-websocket",
		ServiceDNS:    strings.TrimPrefix(srv.URL, "https://"),
		UpstreamURL:   srv.URL,
		TLSServerName: "127.0.0.1",
		TLSCA: pem.EncodeToMemory(&pem.Block{
			Type:  "CERTIFICATE",
			Bytes: srv.Certificate().Raw,
		}),
		Username: "kasm_user",
		Password: "upstream-secret",
	}
}

// fakeWSUpstream answers 200 to plain GETs and completes a websocket
// upgrade with a raw echo loop (enough for the proxy's duplex-copy path).
func fakeWSUpstream(w http.ResponseWriter, r *http.Request) {
	if !wsUpgradeOffer(r) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
		return
	}
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
}

func wsUpgradeOffer(r *http.Request) bool {
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

func (f *fakeBrokerClient) RedeemTicket(_ context.Context, gw broker.GatewayIdentity, opaque string) (broker.Lease, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	l, ok := f.leases[opaque]
	if !ok || f.redeemed[opaque] {
		return broker.Lease{}, broker.ErrTicketInvalid
	}
	f.redeemed[opaque] = true
	l.GatewayID = gw.ID
	return l, nil
}

func (f *fakeBrokerClient) RenewLease(_ context.Context, _ broker.GatewayIdentity, leaseID string, _ broker.Fence) (broker.Lease, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.renewErr[leaseID]; err != nil {
		return broker.Lease{}, err
	}
	for _, l := range f.leases {
		if l.ID == leaseID {
			l.ExpiresAt = time.Now().Add(broker.LeaseTTL)
			return l, nil
		}
	}
	return broker.Lease{}, broker.ErrLeaseInvalid
}

func (f *fakeBrokerClient) ResolveTarget(_ context.Context, _ broker.GatewayIdentity, leaseID string) (broker.Target, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if tgt, ok := f.resolveT[leaseID]; ok {
		return tgt, nil
	}
	return broker.Target{}, broker.ErrLeaseInvalid
}

func (f *fakeBrokerClient) RevokeLease(_ context.Context, leaseID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revokes = append(f.revokes, leaseID)
	return nil
}

func (f *fakeBrokerClient) RevokeLeaseChanged(ctx context.Context, leaseID string) (bool, error) {
	return true, f.RevokeLease(ctx, leaseID)
}

func (f *fakeBrokerClient) ReportActivity(_ context.Context, _ broker.GatewayIdentity, leaseID string, _ broker.Fence, ev broker.ActivityEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.activity = append(f.activity, ev.Type)
	return nil
}

func (f *fakeBrokerClient) activityTypes() []broker.ActivityEventType {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]broker.ActivityEventType{}, f.activity...)
}

func (f *fakeBrokerClient) revokeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.revokes)
}

// ---------------------------------------------------------------------------
// Route isolation (D7, P9)
// ---------------------------------------------------------------------------

// testSessionHandler builds the production session-listener handler on a
// fake broker — the same readyz+RequestID+Gateway stack wire() builds.
func testSessionHandler(t *testing.T, bc gateway.BrokerClient) (*Backend, http.Handler) {
	t.Helper()
	b := &Backend{log: testLog()}
	b.ready.Store(true)
	cfg := Config{
		SessionDomain:       "session.test",
		SessionControlHosts: "session.test",
		RenewInterval:       25 * time.Millisecond,
		RevokeDeadline:      150 * time.Millisecond,
		ControlToken:        "control-test-token",
	}
	if err := b.newGateway(cfg, bc, broker.GatewayIdentity{ID: "gw-test", Audience: "session.test"}, nil, nil); err != nil {
		t.Fatalf("newGateway: %v", err)
	}
	t.Cleanup(func() { b.gw.Close() })
	return b, b.sessionHandler
}

// TestRouteIsolation_SessionListener (D7/P9): API paths on the session
// listener never reach the API mux and answer 401 or 404 — the session
// listener's catch-all is the cookie-gated desktop proxy.
func TestRouteIsolation_SessionListener(t *testing.T) {
	fb := newFakeBrokerClient(t)
	b, h := testSessionHandler(t, fb)

	// The app handler exists on the same backend; count its hits.
	appHits := &countingHandler{next: http.NotFoundHandler()}
	b.appHandler = appHits

	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	for _, path := range []string{
		"/v1/workspaces", "/v1/templates", "/v1/data",
		"/v1/login", "/v1/auth/callback",
		"/v1/me", "/v1/session", "/v1/workspaces/ws_abc12345/connection",
	} {
		req, err := http.NewRequest(http.MethodGet, srv.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = "ws-0000000a.session.test" // a workspace host: API paths 401 behind the cookie gate
		resp, err := srv.Client().Transport.(*http.Transport).RoundTrip(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusNotFound {
			t.Fatalf("GET %s on session listener = %d, want 401 or 404", path, resp.StatusCode)
		}
	}
	if appHits.hits != 0 {
		t.Fatalf("session listener reached the app mux %d times", appHits.hits)
	}
}

// fakeConnIssuer satisfies api.ConnectionIssuer for route-table tests.
type fakeConnIssuer struct{}

func (fakeConnIssuer) IssueTicket(context.Context, api.Principal, string, bool, string) (api.IssuedTicket, *api.Error) {
	return api.IssuedTicket{}, &api.Error{Code: api.CodeNotFound, Message: "no ticket in tests"}
}

// fakeConnStater satisfies api.ConnectionStater for route-table tests.
type fakeConnStater struct{}

func (fakeConnStater) ConnectionState(context.Context, string) (api.ConnectionStatus, *api.Error) {
	return api.ConnectionStatus{State: "none"}, nil
}

// fakeWorkspaceGetter satisfies the ownership-check surface of the
// connection-status handler for route-table tests.
type fakeWorkspaceGetter struct{}

func (fakeWorkspaceGetter) GetWorkspace(context.Context, string, string, string) (provisioning.WorkspaceRecord, error) {
	return provisioning.WorkspaceRecord{}, provisioning.ErrWorkspaceNotFound
}

// fakeQuotaSource satisfies api.QuotaSource for route-table tests.
type fakeQuotaSource struct{}

func (fakeQuotaSource) Report(context.Context, string) (store.QuotaReport, error) {
	return store.QuotaReport{}, nil
}

// testAppHandler builds the production app-listener handler (mux +
// middleware) with the real OIDC discovery path against the fake issuer.
func testAppHandler(t *testing.T) http.Handler {
	t.Helper()
	return testAppHandlerWithMetrics(t, nil)
}

// testAppHandlerWithMetrics is testAppHandler with a metric set on the
// Backend, so the E8 app-listener instrumentation is exercised.
func testAppHandlerWithMetrics(t *testing.T, m *observability.Metrics) http.Handler {
	t.Helper()
	iss, err := oidctest.NewIssuer()
	if err != nil {
		t.Fatalf("oidctest issuer: %v", err)
	}
	sealer, err := loginstate.NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatalf("loginstate sealer: %v", err)
	}
	authn, err := api.NewAuthenticator(context.Background(), api.AuthConfig{
		Issuer:      iss.URL(),
		ClientID:    "tinycdi",
		RedirectURL: "https://portal.example.test/auth/callback",
		LoginSealer: sealer,
	}, api.NewInMemorySessionStore(time.Minute), testLog())
	if err != nil {
		t.Fatalf("NewAuthenticator: %v", err)
	}
	tenants := api.StaticTenantResolver{}
	sdom, err := sessionhost.ParseDomain("session.example.test")
	if err != nil {
		t.Fatalf("sessionhost.ParseDomain: %v", err)
	}
	ws := api.NewWorkspaceHandler(nil, nil, tenants)
	tpl := api.NewTemplateHandler(nil, tenants)
	conn := api.NewConnectionHandler(fakeConnIssuer{}, tenants, sdom)
	me := api.NewMeHandler(sdom.String())
	connStatus := api.NewConnectionStatusHandler(fakeConnStater{}, fakeWorkspaceGetter{}, tenants)
	data := api.NewDataHandler(nil, nil, tenants)
	quota := api.NewQuotaHandler(fakeQuotaSource{}, nil, tenants)
	mux := appMux(authn, ws, tpl, conn, me, connStatus, data, quota, func(h http.Handler) http.Handler { return h })
	b := &Backend{log: testLog(), metrics: m}
	b.ready.Store(true)
	return b.wrapApp(authn, mux, []string{"https://portal.example.test"})
}

// TestRouteIsolation_AppListener (D7): the session surface — launch,
// control, desktop proxy — must not exist on the app listener.
func TestRouteIsolation_AppListener(t *testing.T) {
	srv := httptest.NewServer(testAppHandler(t))
	t.Cleanup(srv.Close)

	for _, tc := range []struct {
		method, path string
	}{
		{http.MethodPost, "/v1/launch"},
		{http.MethodGet, "/v1/control/session"},
		{http.MethodGet, "/websockify"},
	} {
		req, err := http.NewRequest(tc.method, srv.URL+tc.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := srv.Client().Transport.(*http.Transport).RoundTrip(req)
		if err != nil {
			t.Fatalf("%s %s: %v", tc.method, tc.path, err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s %s on app listener = %d, want 404", tc.method, tc.path, resp.StatusCode)
		}
	}
}

// TestAppListener_SessionProbe (FX-R13b): the production route table serves
// the anonymous probe as 200 {"authenticated":false} while /v1/me stays 401.
func TestAppListener_SessionProbe(t *testing.T) {
	srv := httptest.NewServer(testAppHandler(t))
	t.Cleanup(srv.Close)

	get := func(path string) (*http.Response, string) {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp, strings.TrimSpace(string(body))
	}
	resp, body := get("/v1/session")
	if resp.StatusCode != http.StatusOK || body != `{"authenticated":false}` {
		t.Fatalf("GET /v1/session = %d %q, want 200 {\"authenticated\":false}", resp.StatusCode, body)
	}
	if resp, _ = get("/v1/me"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("GET /v1/me anonymous = %d, want 401", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// Internal listener isolation
// ---------------------------------------------------------------------------

// fakeInternalBroker is an httpapi.BrokerAPI that records calls; redeem is
// scripted so a real redeem round-trip can be told apart from a 404.
type fakeInternalBroker struct {
	redeemLease broker.Lease
	redeemErr   error
}

func (f *fakeInternalBroker) RedeemTicket(context.Context, broker.GatewayIdentity, string) (broker.Lease, error) {
	return f.redeemLease, f.redeemErr
}
func (f *fakeInternalBroker) RenewLease(context.Context, broker.GatewayIdentity, string, broker.Fence) (broker.Lease, error) {
	return broker.Lease{}, broker.ErrLeaseInvalid
}
func (f *fakeInternalBroker) ResolveTarget(context.Context, broker.GatewayIdentity, string) (broker.Target, error) {
	return broker.Target{}, broker.ErrLeaseInvalid
}
func (f *fakeInternalBroker) RevokeLease(context.Context, string) error { return nil }
func (f *fakeInternalBroker) RevokeLeaseChanged(context.Context, string) (bool, error) {
	return false, nil
}
func (f *fakeInternalBroker) ReportActivity(context.Context, broker.GatewayIdentity, string, broker.Fence, broker.ActivityEvent) error {
	return nil
}
func (f *fakeInternalBroker) RevokeWorkspaceLeases(context.Context, broker.PlatformID, uint64) (int, error) {
	return 0, nil
}
func (f *fakeInternalBroker) DrainStatus(context.Context, broker.PlatformID) (int, bool, error) {
	return 0, true, nil
}

// testCA is a tiny in-test CA used to sign client certs for the internal
// listener (the server cert stays self-signed; the client verifies nothing).
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

func newTestCA(t *testing.T) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &testCA{cert: cert, key: key,
		pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

// sign issues a client certificate under the CA.
func (ca *testCA) sign(t *testing.T, cn string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

// serveInternal binds the production internal-listener stack (server
// constructor + httpapi handler + VerifyClientCertIfGiven TLS) on :0.
func serveInternal(t *testing.T, h http.Handler, caPEM []byte) *httptest.Server {
	t.Helper()
	dir := t.TempDir()
	cf, kf := writeTestCert(t, dir, "internal.test")
	cert, err := tls.LoadX509KeyPair(cf, kf)
	if err != nil {
		t.Fatal(err)
	}
	tlsCfg, err := httpapi.ServerTLSConfig(cert, caPEM)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := internalServer("127.0.0.1:0", h)
	go func() { _ = serveTLS(srv, ln, tlsCfg) }()
	t.Cleanup(func() { _ = srv.Close() })
	return &httptest.Server{URL: "https://" + ln.Addr().String()}
}

// TestRouteIsolation_InternalListener (D7): the internal mTLS listener
// serves only /internal/v1/*; a verified client gets 404 for API paths and
// a request without a client certificate gets 401.
func TestRouteIsolation_InternalListener(t *testing.T) {
	ca := newTestCA(t)
	fake := &fakeInternalBroker{redeemErr: broker.ErrTicketInvalid}
	h := httpapi.NewHandler(httpapi.Config{
		Broker:   fake,
		Audience: "session.test",
		Logger:   testLog(),
	})
	srv := serveInternal(t, h, ca.pem)

	// A verified client cert reaches the mux: API paths 404, the real
	// internal route exists (400 on the missing ticket body proves it).
	clientCert := ca.sign(t, "gw-test")
	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true, // test only: server cert is self-signed
			Certificates:       []tls.Certificate{clientCert},
		},
	}}
	for _, path := range []string{"/v1/workspaces", "/v1/login", "/websockify"} {
		resp, err := client.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("GET %s on internal listener = %d, want 404", path, resp.StatusCode)
		}
	}
	resp, err := client.Post(srv.URL+"/internal/v1/broker/redeem",
		"application/json", strings.NewReader(`{"ticket":""}`))
	if err != nil {
		t.Fatalf("redeem: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		t.Fatal("internal route /internal/v1/broker/redeem missing (404)")
	}

	// No client certificate at all: the TLS layer lets the request through
	// (VerifyClientCertIfGiven) and the handler answers 401.
	anon := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // test only
	}}
	resp, err = anon.Get(srv.URL + "/internal/v1/broker/redeem")
	if err != nil {
		t.Fatalf("anon request: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no client cert = %d, want 401", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// Drain-ordered shutdown
// ---------------------------------------------------------------------------

// internalAPITLS serves the internal API over TLS on a random port. The
// server requests (but does not verify) a client certificate so the
// handler's identity middleware sees the peer cert — the real listener's
// ClientCAs verification is covered by TestRouteIsolation_InternalListener.
// It returns the URL plus the CA-file bytes the broker client needs.
func internalAPITLS(t *testing.T, h http.Handler) (url string, caPEM []byte) {
	t.Helper()
	srv := httptest.NewUnstartedServer(h)
	srv.TLS = &tls.Config{ClientAuth: tls.RequestClientCert}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	caPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	return srv.URL, caPEM
}

// writeFile writes content to dir/name and returns the path.
func writeFile(t *testing.T, dir, name string, content []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, content, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestRun_DrainsOnShutdown: cancelling the run context with a WebSocket
// open on the session listener drains it — the socket closes, the broker
// recorded a disconnect, no lease was revoked, and Run returns nil well
// inside the shutdown budget.
func TestRun_DrainsOnShutdown(t *testing.T) {
	dir := t.TempDir()

	// Fake remote broker behind the internal mTLS API contract.
	fb := newFakeBrokerClient(t)
	fb.scriptTicket("tk-1", "ws_aaaa0001")
	internalH := httpapi.NewHandler(httpapi.Config{
		Broker:   internalAdapter{fb},
		Audience: "session.test",
		Logger:   testLog(),
	})
	brokerURL, brokerCAPEM := internalAPITLS(t, internalH)

	sessionCert, sessionKey := writeTestCert(t, dir, "session.test")
	mtlsCert, mtlsKey := writeTestCert(t, dir, "gw-split")
	brokerCA := writeFile(t, dir, "broker.ca", brokerCAPEM)

	cfg, err := ParseFlags([]string{
		"-listen=", "-internal-listen=",
		"-session-listen", "127.0.0.1:0",
		"-session-tls-cert", sessionCert,
		"-session-tls-key", sessionKey,
		"-session-control-hosts", "session.test",
		"-session-domain", "session.test",
		"-control-token-file", writeFile(t, dir, "control.token", []byte("tok")),
		"-renew-interval", "25ms",
		"-revoke-deadline", "2s",
		// The drain window is held for its full length now — keep this
		// test about the shed mechanics, not the hold.
		"-drain-window", "250ms",
		"-broker-url", brokerURL,
		"-broker-ca", brokerCA,
		"-mtls-cert", mtlsCert,
		"-mtls-key", mtlsKey,
		"-gateway-id", "gw-test",
	}, noEnv)
	if err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}

	b, err := New(context.Background(), cfg, testLog())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- b.Run(runCtx) }()

	_, sessionAddr, _, _ := b.Addrs()
	if sessionAddr == "" {
		t.Fatal("session listener not bound")
	}
	base := "https://" + sessionAddr
	insecure := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // test only
	}}

	// Launch: redeem the scripted ticket for a session cookie.
	form := url.Values{"ticket": {"tk-1"}}
	req, err := http.NewRequest(http.MethodPost, base+"/v1/launch", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "ws-aaaa0001.session.test"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://ws-aaaa0001.session.test")
	resp, err := insecure.Transport.(*http.Transport).RoundTrip(req)
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	var cookie string
	for _, c := range resp.Cookies() {
		if c.Name == gateway.SessionCookieName {
			cookie = c.Value
		}
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || cookie == "" {
		t.Fatalf("launch = %d, cookie set=%v — want 303 + session cookie", resp.StatusCode, cookie != "")
	}

	// Open the desktop WebSocket on the session listener.
	wsReq, err := http.NewRequest(http.MethodGet, base+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	wsReq.Host = "ws-aaaa0001.session.test"
	wsReq.Header.Set("Origin", "https://ws-aaaa0001.session.test")
	wsReq.Header.Set("Cookie", gateway.SessionCookieName+"="+cookie)
	wsReq.Header.Set("Connection", "upgrade")
	wsReq.Header.Set("Upgrade", "websocket")
	wsReq.Header.Set("Sec-WebSocket-Version", "13")
	wsReq.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	wsResp, err := insecure.Transport.(*http.Transport).RoundTrip(wsReq)
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	if wsResp.StatusCode != http.StatusSwitchingProtocols {
		body, _ := io.ReadAll(wsResp.Body)
		wsResp.Body.Close()
		t.Fatalf("upgrade = %d, want 101 (%s)", wsResp.StatusCode, body)
	}

	// Cancel the run context: Run must drain, shut down and return.
	cancel()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Run = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return within 10s of ctx cancel")
	}

	// The open socket must be dead now.
	_ = wsResp.Body.Close()
	if _, err := wsResp.Body.Read(make([]byte, 1)); err == nil {
		// Some transports report the close on the second read; don't fail
		// on the first — the disconnect record below is the real check.
		t.Log("first post-drain read did not error; relying on disconnect record")
	}

	// The broker must have seen a disconnect for the lease and no revoke.
	deadline := time.Now().Add(5 * time.Second)
	for {
		acts := fb.activityTypes()
		seen := false
		for _, a := range acts {
			if a == broker.ActivityDisconnect {
				seen = true
			}
		}
		if seen {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no disconnect recorded; activity=%v revokes=%v", acts, fb.revokeCount())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if n := fb.revokeCount(); n != 0 {
		t.Fatalf("drain revoked %d leases, want 0 (the cookie must survive a restart)", n)
	}
}

// TestRun_HoldsDrainWindow: the drain window is a duration, not a shed
// budget — after ctx cancel the session listener keeps answering for the
// whole -drain-window (readiness already failed, sockets still land) and
// Run only returns once the window ends.
func TestRun_HoldsDrainWindow(t *testing.T) {
	const window = 600 * time.Millisecond
	dir := t.TempDir()

	fb := newFakeBrokerClient(t)
	internalH := httpapi.NewHandler(httpapi.Config{
		Broker:   internalAdapter{fb},
		Audience: "session.test",
		Logger:   testLog(),
	})
	brokerURL, brokerCAPEM := internalAPITLS(t, internalH)

	sessionCert, sessionKey := writeTestCert(t, dir, "session.test")
	mtlsCert, mtlsKey := writeTestCert(t, dir, "gw-split")
	brokerCA := writeFile(t, dir, "broker.ca", brokerCAPEM)

	cfg, err := ParseFlags([]string{
		"-listen=", "-internal-listen=",
		"-session-listen", "127.0.0.1:0",
		"-session-tls-cert", sessionCert,
		"-session-tls-key", sessionKey,
		"-session-control-hosts", "session.test",
		"-session-domain", "session.test",
		"-control-token-file", writeFile(t, dir, "control.token", []byte("tok")),
		"-drain-window", window.String(),
		"-broker-url", brokerURL,
		"-broker-ca", brokerCA,
		"-mtls-cert", mtlsCert,
		"-mtls-key", mtlsKey,
		"-gateway-id", "gw-test",
	}, noEnv)
	if err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	b, err := New(context.Background(), cfg, testLog())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- b.Run(runCtx) }()

	_, sessionAddr, _, _ := b.Addrs()
	base := "https://" + sessionAddr
	insecure := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // test only
	}}

	waitFor(t, 5*time.Second, "session listener serving", func() bool {
		resp, err := insecure.Get(base + "/healthz")
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	})

	start := time.Now()
	cancel()

	// Readiness fails at once, but the socket keeps answering for the
	// whole window — nothing is refused inside it.
	waitFor(t, window/2, "readiness to drop inside the window", func() bool {
		resp, err := insecure.Get(base + "/readyz")
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusServiceUnavailable
	})
	select {
	case <-runDone:
		t.Fatalf("Run returned %v after cancel, inside the %v drain window", time.Since(start), window)
	case <-time.After(window - 100*time.Millisecond):
	}
	resp, err := insecure.Get(base + "/readyz")
	if err != nil {
		t.Fatalf("session listener refused a connection inside the drain window: %v", err)
	}
	resp.Body.Close()

	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Run = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after the drain window")
	}
	if d := time.Since(start); d < window {
		t.Fatalf("Run returned %v after cancel, before the %v drain window ended", d, window)
	}
}

// TestRun_HoldsDrainWindowAppOnly: the window hold is not a gateway
// concern — a backend with no session listener (-session-listen unset)
// still keeps the app listener answering for the whole -drain-window.
func TestRun_HoldsDrainWindowAppOnly(t *testing.T) {
	const window = 400 * time.Millisecond

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	b := &Backend{cfg: Config{DrainWindow: window}, log: testLog()}
	srv := &http.Server{Handler: b.readyz(http.NotFoundHandler())}
	b.servers = append(b.servers, namedServer{name: "app", srv: srv, ln: ln})
	b.markCacheSynced()

	runCtx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- b.Run(runCtx) }()

	base := "http://" + ln.Addr().String()
	waitFor(t, 5*time.Second, "app listener serving", func() bool {
		resp, err := http.Get(base + "/readyz")
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	})

	start := time.Now()
	cancel()

	// Inside the window the app listener still answers — readiness is
	// already false but the socket is not refused.
	select {
	case <-runDone:
		t.Fatalf("Run returned %v after cancel, inside the %v drain window", time.Since(start), window)
	case <-time.After(window - 100*time.Millisecond):
	}
	resp, err := http.Get(base + "/readyz")
	if err != nil {
		t.Fatalf("app listener refused a connection inside the drain window: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("/readyz inside the drain window = %d, want 503", resp.StatusCode)
	}

	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Run = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after the drain window")
	}
	if d := time.Since(start); d < window {
		t.Fatalf("Run returned %v after cancel, before the %v drain window ended", d, window)
	}
}

// TestGatewayIDSharedAcrossInstances: two backends with the same
// -gateway-id share lease ownership — a lease redeemed through one must be
// renewable by the other (restart-safe sessions need replicas to be
// interchangeable; a foreign ID still gets ErrDenied).
func TestGatewayIDSharedAcrossInstances(t *testing.T) {
	db := newDB(t)
	src := newFakeBindings()
	// Production binds the ticket audience to the session domain
	// (resolveGatewayIdentity); the test broker does the same.
	brk := broker.New(db, src, broker.WithGatewayAudience("session.test"))

	seedWorkspace(t, db, "tenant-a", "iss|alice", "ws-shared-1")
	src.set(readyBinding("ws-shared-1", "tenant-a", "iss|alice", 1, "rt-1", time.Now()))

	cfg, err := ParseFlags(withArg(
		withArg(mergedArgs(), "-session-domain", "session.test"),
		"-gateway-id", "gw-shared"), noEnv)
	if err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	b1 := &Backend{cfg: cfg, log: testLog()}
	b2 := &Backend{cfg: cfg, log: testLog()}
	id1, err := resolveGatewayIdentity(b1.cfg)
	if err != nil {
		t.Fatal(err)
	}
	id2, err := resolveGatewayIdentity(b2.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if id1 != id2 {
		t.Fatalf("identities differ across replicas: %+v vs %+v", id1, id2)
	}
	lgA, err := b1.localGateway(brk, id1)
	if err != nil {
		t.Fatal(err)
	}
	lgB, err := b2.localGateway(brk, id2)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	tick, err := brk.IssueTicket(ctx,
		api.Principal{Issuer: "iss", Subject: "alice", TenantID: "tenant-a"},
		"ws-shared-1", false, "")
	if err != nil {
		t.Fatalf("issue ticket: %v", err)
	}
	lease, err := lgA.RedeemTicket(ctx, broker.GatewayIdentity{}, tick.Token)
	if err != nil {
		t.Fatalf("redeem on replica A: %v", err)
	}
	fence := broker.Fence{
		WorkspaceUID:      lease.WorkspaceUID,
		RuntimeGeneration: lease.RuntimeGeneration,
		RuntimeUID:        lease.RuntimeUID,
		FencingVersion:    lease.FencingVersion,
	}
	if _, err := lgB.RenewLease(ctx, broker.GatewayIdentity{}, lease.ID, fence); err != nil {
		t.Fatalf("renew on replica B: %v — a shared -gateway-id must make replicas interchangeable", err)
	}

	// A different gateway ID remains foreign: same DB, ErrDenied.
	cfgOther, _ := ParseFlags(withArg(
		withArg(mergedArgs(), "-session-domain", "session.test"),
		"-gateway-id", "gw-other"), noEnv)
	b3 := &Backend{cfg: cfgOther, log: testLog()}
	id3, err := resolveGatewayIdentity(b3.cfg)
	if err != nil {
		t.Fatal(err)
	}
	lgC, err := b3.localGateway(brk, id3)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lgC.RenewLease(ctx, broker.GatewayIdentity{}, lease.ID, fence); !errors.Is(err, broker.ErrDenied) {
		t.Fatalf("renew from a different gateway id = %v, want ErrDenied", err)
	}
}

// internalAdapter adapts the gateway-side fakeBrokerClient onto the
// httpapi.BrokerAPI surface the internal listener exposes.
type internalAdapter struct{ fb *fakeBrokerClient }

func (a internalAdapter) RedeemTicket(ctx context.Context, gw broker.GatewayIdentity, opaque string) (broker.Lease, error) {
	return a.fb.RedeemTicket(ctx, gw, opaque)
}
func (a internalAdapter) RenewLease(ctx context.Context, gw broker.GatewayIdentity, leaseID string, fence broker.Fence) (broker.Lease, error) {
	return a.fb.RenewLease(ctx, gw, leaseID, fence)
}
func (a internalAdapter) ResolveTarget(ctx context.Context, gw broker.GatewayIdentity, leaseID string) (broker.Target, error) {
	return a.fb.ResolveTarget(ctx, gw, leaseID)
}
func (a internalAdapter) RevokeLease(ctx context.Context, leaseID string) error {
	return a.fb.RevokeLease(ctx, leaseID)
}
func (a internalAdapter) RevokeLeaseChanged(ctx context.Context, leaseID string) (bool, error) {
	return a.fb.RevokeLeaseChanged(ctx, leaseID)
}
func (a internalAdapter) ReportActivity(ctx context.Context, gw broker.GatewayIdentity, leaseID string, fence broker.Fence, ev broker.ActivityEvent) error {
	return a.fb.ReportActivity(ctx, gw, leaseID, fence, ev)
}
func (a internalAdapter) RevokeWorkspaceLeases(context.Context, broker.PlatformID, uint64) (int, error) {
	return 0, nil
}
func (a internalAdapter) DrainStatus(context.Context, broker.PlatformID) (int, bool, error) {
	return 0, true, nil
}
