package gateway_test

// Proxy + authorization contract tests (design §6.4–6.7; every fixture
// gateway defect — SEC-1/2/3 — has a product equivalent here).

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/broker"
	"github.com/tinyorbitvn/tinycdi/internal/gateway"
	"github.com/tinyorbitvn/tinycdi/internal/observability"
)

// TestProxy_RequiresSession: every desktop route needs a valid session
// cookie — no anonymous access to runtime traffic.
func TestProxy_RequiresSession(t *testing.T) {
	fb := newFakeBroker(t)
	srv := newGateway(t, fb, nil)

	resp := proxied(t, srv, testHost, "/", "", map[string]string{"Origin": testOrigin})
	defer drain(resp)
	if resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden {
		t.Fatalf("unauthenticated proxy = %d, want 401/403", resp.StatusCode)
	}
	resp = proxied(t, srv, testHost, "/", "bogus-cookie", map[string]string{"Origin": testOrigin})
	defer drain(resp)
	if resp.StatusCode == http.StatusOK {
		t.Fatal("bogus session cookie proxied to the runtime")
	}
}

// TestProxy_BadHostRejected: Host allowlist applies to proxied paths too —
// a spoofed Host must not ride a valid session.
func TestProxy_BadHostRejected(t *testing.T) {
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-host2", testWSUID)
	srv := newGateway(t, fb, nil)
	cookie := launchOK(t, srv, testHost, "tk-host2")

	resp := proxied(t, srv, "evil.test", "/", cookie, map[string]string{"Origin": testOrigin})
	defer drain(resp)
	if resp.StatusCode != http.StatusMisdirectedRequest {
		t.Fatalf("proxied request with foreign Host = %d, want 421", resp.StatusCode)
	}
}

// TestProxy_ManagementPathsDenied: upstream management routes (e.g. the
// runtime's /api/*) are denied at the gateway before auth — the allowlist
// only exposes streaming assets and the socket endpoint.
func TestProxy_ManagementPathsDenied(t *testing.T) {
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-mgmt", testWSUID)
	srv := newGateway(t, fb, nil)
	cookie := launchOK(t, srv, testHost, "tk-mgmt")

	// /Downloads is the kasm images' writable-inside-webroot path (KASM-6);
	// it must stay off the allowlist even though the runtime rootfs is now
	// read-only — the gateway is the outer gate.
	for _, p := range []string{"/api/get_users", "/api/", "/admin", "/Downloads/", "/Downloads"} {
		resp := proxied(t, srv, testHost, p, cookie, map[string]string{"Origin": testOrigin})
		drain(resp)
		if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusSwitchingProtocols {
			t.Fatalf("management path %s was proxied (status %d)", p, resp.StatusCode)
		}
	}
}

// TestProxy_ArbitraryTargetRejected: the gateway never honours a
// client-supplied upstream — the target comes only from ResolveTarget on a
// live lease (SSRF guard). Probe: absolute-form request-URI aimed at a
// different host, sent over a raw socket to the gateway listener.
func TestProxy_ArbitraryTargetRejected(t *testing.T) {
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-ssrf", testWSUID)
	srv := newGateway(t, fb, nil)
	cookie := launchOK(t, srv, testHost, "tk-ssrf")

	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatalf("dial gateway: %v", err)
	}
	defer conn.Close()
	_, err = fmt.Fprintf(conn,
		"GET http://169.254.169.254/latest/meta-data HTTP/1.1\r\nHost: %s\r\nCookie: %s=%s\r\nOrigin: %s\r\nConnection: close\r\n\r\n",
		testHost, gateway.SessionCookieName, cookie, testOrigin)
	if err != nil {
		t.Fatalf("write SSRF probe: %v", err)
	}
	br := bufio.NewReader(conn)
	statusLine, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read probe response: %v", err)
	}
	if strings.Contains(statusLine, " 200") {
		t.Fatalf("absolute-form probe to metadata endpoint got %q — must not proxy client-supplied targets", strings.TrimSpace(statusLine))
	}
}

// TestUpgrade_OriginEnforced: WS upgrades require Origin equal to the public
// origin and request authority (triple match); foreign or absent Origin is
// rejected before the socket opens.
func TestUpgrade_OriginEnforced(t *testing.T) {
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-ws", testWSUID)
	srv := newGateway(t, fb, nil)
	cookie := launchOK(t, srv, testHost, "tk-ws")

	resp := upgrade(t, srv, testHost, "/websockify", cookie, map[string]string{"Origin": "https://evil.test"})
	drain(resp)
	if resp.StatusCode == http.StatusSwitchingProtocols {
		t.Fatal("upgrade with foreign Origin got 101")
	}

	resp = upgrade(t, srv, testHost, "/websockify", cookie, nil)
	drain(resp)
	if resp.StatusCode == http.StatusSwitchingProtocols {
		t.Fatal("upgrade with absent Origin got 101")
	}

	resp = upgrade(t, srv, testHost, "/websockify", cookie, map[string]string{"Origin": testOrigin})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("valid upgrade = %d, want 101", resp.StatusCode)
	}
}

// TestUpgrade_NonWebSocketRejected (SEC-3): an upgrade offer that is not
// websocket (e.g. h2c) must get 400 — never be proxied as an upgrade.
func TestUpgrade_NonWebSocketRejected(t *testing.T) {
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-h2c", testWSUID)
	srv := newGateway(t, fb, nil)
	cookie := launchOK(t, srv, testHost, "tk-h2c")

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/websockify", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = testHost
	req.Header.Set("Connection", "upgrade")
	req.Header.Set("Upgrade", "h2c")
	req.Header.Set("Cookie", "__Host-tcdi_session="+cookie)
	req.Header.Set("Origin", testOrigin)
	resp, err := srv.Client().Transport.(*http.Transport).RoundTrip(req)
	if err != nil {
		t.Fatalf("h2c offer: %v", err)
	}
	defer drain(resp)
	if resp.StatusCode == http.StatusSwitchingProtocols {
		t.Fatal("non-websocket upgrade offer was switched — SEC-3")
	}
	if resp.StatusCode != http.StatusBadRequest && resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-websocket upgrade = %d, want 400/403", resp.StatusCode)
	}
}

// TestUpgrade_HeaderWithoutConnectionNotUpgrade (SEC-3 second direction): a
// stray `Upgrade: websocket` without a `Connection: upgrade` token is NOT an
// upgrade — it must be treated as a plain request and must not fence the
// live stream.
func TestUpgrade_HeaderWithoutConnectionNotUpgrade(t *testing.T) {
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-stray", testWSUID)
	srv := newGateway(t, fb, nil)
	cookie := launchOK(t, srv, testHost, "tk-stray")

	// open a real stream first
	live := upgrade(t, srv, testHost, "/websockify", cookie, map[string]string{"Origin": testOrigin})
	if live.StatusCode != http.StatusSwitchingProtocols {
		drain(live)
		t.Fatalf("initial upgrade = %d, want 101", live.StatusCode)
	}
	defer live.Body.Close()

	// stray Upgrade header, no Connection token → plain request, no fence
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/websockify", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = testHost
	req.Header.Set("Upgrade", "websocket") // deliberately no Connection header
	req.Header.Set("Cookie", "__Host-tcdi_session="+cookie)
	req.Header.Set("Origin", testOrigin)
	resp, err := srv.Client().Transport.(*http.Transport).RoundTrip(req)
	if err != nil {
		t.Fatalf("stray upgrade request: %v", err)
	}
	drain(resp)
	if resp.StatusCode == http.StatusSwitchingProtocols {
		t.Fatal("stray Upgrade header produced a 101 — upgrade detection must require Connection token")
	}
	// and the live stream must still be alive — a fenced conn errors fast
	readDone := make(chan error, 1)
	go func() {
		_, err := live.Body.Read(make([]byte, 1))
		readDone <- err
	}()
	select {
	case err := <-readDone:
		if err != nil {
			t.Fatalf("live stream was fenced by a stray Upgrade header: %v", err)
		}
		// data arrived — inconclusive but not fenced; accept
	case <-time.After(100 * time.Millisecond):
		// still open — good
	}
}

// TestRevoke_UnknownTargetDoesNotKillSession (SEC-1): a control revoke naming
// an unknown/superseded ticket or leaseId must not kill the live session —
// revoke acts only on what it resolves to.
func TestRevoke_UnknownTargetDoesNotKillSession(t *testing.T) {
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-live", testWSUID)
	srv := newGateway(t, fb, nil)
	cookie := launchOK(t, srv, testHost, "tk-live")

	// revoke a lease that does not resolve
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/control/revoke", strings.NewReader(`{"leaseId":"no-such-lease"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = testControlHost
	req.Header.Set("Authorization", "Bearer control-test-token")
	resp, err := srv.Client().Transport.(*http.Transport).RoundTrip(req)
	if err != nil {
		t.Fatalf("control revoke: %v", err)
	}
	drain(resp)

	// live session must still work
	ok := proxied(t, srv, testHost, "/", cookie, map[string]string{"Origin": testOrigin})
	defer drain(ok)
	if ok.StatusCode == http.StatusUnauthorized || ok.StatusCode == http.StatusForbidden {
		t.Fatalf("revoke of unknown lease killed the live session (status %d) — SEC-1", ok.StatusCode)
	}
}

// TestControlEndpoints_RequireBearer (SEC-2): when a control token is
// configured, an absent bearer is denied — never silently unauthenticated.
func TestControlEndpoints_RequireBearer(t *testing.T) {
	fb := newFakeBroker(t)
	srv := newGateway(t, fb, nil)

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/v1/control/session", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = testControlHost
	resp, err := srv.Client().Transport.(*http.Transport).RoundTrip(req)
	if err != nil {
		t.Fatalf("control request: %v", err)
	}
	defer drain(resp)
	if resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden {
		t.Fatalf("control endpoint without bearer = %d, want 401/403 — SEC-2", resp.StatusCode)
	}
}

// controlRoundTrip issues one request against the gateway's control
// surface and returns status + body.
func controlRoundTrip(t *testing.T, srv *httptest.Server, method, path, bearer, body string) (int, string) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, srv.URL+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = testControlHost
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := srv.Client().Transport.(*http.Transport).RoundTrip(req)
	if err != nil {
		t.Fatalf("control %s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

// TestControlEndpoints_NoTokenFailsClosed (GW-1, SEC-2): with no
// ControlToken configured the control surface is closed entirely — both
// /v1/control/session and /v1/control/revoke reject every request
// (unauthenticated AND bearer-present), and the denial must not leak a
// leaseId or confirm session existence.
func TestControlEndpoints_NoTokenFailsClosed(t *testing.T) {
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-notoken", testWSUID)
	srv := newGateway(t, fb, func(c *gateway.Config) { c.ControlToken = "" })
	cookie := launchOK(t, srv, testHost, "tk-notoken")
	lease := fb.leaseOf(t, "tk-notoken")

	cases := []struct {
		name, method, path, bearer, body string
	}{
		{"session no bearer", http.MethodGet, "/v1/control/session", "", ""},
		{"session any bearer", http.MethodGet, "/v1/control/session", "attacker-guess", ""},
		{"revoke no bearer", http.MethodPost, "/v1/control/revoke", "", `{"leaseId":"` + lease.ID + `"}`},
		{"revoke any bearer", http.MethodPost, "/v1/control/revoke", "attacker-guess", `{"leaseId":"` + lease.ID + `"}`},
	}
	for _, tc := range cases {
		status, body := controlRoundTrip(t, srv, tc.method, tc.path, tc.bearer, tc.body)
		if status != http.StatusUnauthorized && status != http.StatusForbidden {
			t.Fatalf("%s = %d, want 401/403 — no-token control surface must fail closed (GW-1)", tc.name, status)
		}
		if strings.Contains(body, lease.ID) {
			t.Fatalf("%s response leaks the lease id: %s", tc.name, body)
		}
	}

	// the live session is untouched: a denied revoke must not have killed it
	resp := proxied(t, srv, testHost, "/", cookie, map[string]string{"Origin": testOrigin})
	defer drain(resp)
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		t.Fatalf("denied control probe killed the live session (status %d)", resp.StatusCode)
	}
}

// TestControlEndpoints_WrongBearerRejected (GW-1, SEC-2): with a token
// configured, a missing or wrong bearer is 401 on both control routes —
// only the exact token passes.
func TestControlEndpoints_WrongBearerRejected(t *testing.T) {
	fb := newFakeBroker(t)
	srv := newGateway(t, fb, nil) // ControlToken = "control-test-token"

	for _, bearer := range []string{"", "wrong-token", "control-test-token-extra"} {
		status, _ := controlRoundTrip(t, srv, http.MethodGet, "/v1/control/session", bearer, "")
		if status != http.StatusUnauthorized {
			t.Fatalf("/v1/control/session bearer %q = %d, want 401", bearer, status)
		}
		status, _ = controlRoundTrip(t, srv, http.MethodPost, "/v1/control/revoke", bearer, `{"leaseId":"l-1"}`)
		if status != http.StatusUnauthorized {
			t.Fatalf("/v1/control/revoke bearer %q = %d, want 401", bearer, status)
		}
	}

	// exact token reaches the handler (200 on session, 400 on a
	// missing-lease revoke body — auth already passed)
	status, _ := controlRoundTrip(t, srv, http.MethodGet, "/v1/control/session", "control-test-token", "")
	if status != http.StatusOK {
		t.Fatalf("/v1/control/session with token = %d, want 200", status)
	}
	status, _ = controlRoundTrip(t, srv, http.MethodPost, "/v1/control/revoke", "control-test-token", `{"leaseId":"no-such"}`)
	if status != http.StatusOK {
		t.Fatalf("/v1/control/revoke with token = %d, want 200", status)
	}
}

// TestProxy_StripsClientAuth: client-supplied Authorization and Cookie are
// never forwarded upstream; the gateway injects broker-resolved credentials.
func TestProxy_StripsClientAuth(t *testing.T) {
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-auth", testWSUID)
	up := newUpstream(t)
	srv := newGateway(t, fb, nil)
	cookie := launchOK(t, srv, testHost, "tk-auth")
	// Point the lease's resolved target at the recording upstream.
	lease := fb.leaseOf(t, "tk-auth")
	fb.mu.Lock()
	fb.resolveT[lease.ID] = targetFor(up.srv)
	fb.mu.Unlock()

	resp := proxied(t, srv, testHost, "/", cookie, map[string]string{
		"Origin":        testOrigin,
		"Authorization": "Bearer client-token-must-not-leak",
		"Cookie":        "__Host-tcdi_session=" + cookie + "; injected=1",
	})
	drain(resp)
	up.mu.Lock()
	gotAuth, gotCookie, hits := up.gotAuth, up.gotCookie, up.hits
	up.mu.Unlock()
	if hits == 0 {
		t.Fatalf("upstream not reached (status %d)", resp.StatusCode)
	}
	if strings.Contains(gotAuth, "client-token-must-not-leak") || strings.Contains(gotCookie, "injected=1") {
		t.Fatalf("client credentials reached upstream: auth=%q cookie=%q", gotAuth, gotCookie)
	}
	if gotAuth == "" {
		t.Fatal("gateway did not inject upstream credentials")
	}
}

// TestUpstreamTLS_RequiresPinnedCA: the upstream handshake validates the
// pinned CA — a foreign CA fails 502 and the request never reaches upstream
// (proven semantics: wrong-CA → 502, zero requests relayed).
func TestUpstreamTLS_RequiresPinnedCA(t *testing.T) {
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-tls", testWSUID)
	// foreign-CA TLS upstream
	foreign := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("request reached wrong-CA upstream — must be rejected at the handshake")
	}))
	defer foreign.Close()
	srv := newGateway(t, fb, nil)
	cookie := launchOK(t, srv, testHost, "tk-tls")
	lease := fb.leaseOf(t, "tk-tls")
	// ResolveTarget now points at the foreign-CA upstream; the gateway's
	// pinned CA pool does not trust it.
	fb.mu.Lock()
	fb.resolveT[lease.ID] = broker.Target{
		Protocol:   "kasmvnc-websocket",
		ServiceDNS: strings.TrimPrefix(foreign.URL, "https://"),
		TLSCA:      []byte("pinned-ca-not-the-foreign-one"),
	}
	fb.mu.Unlock()

	resp := proxied(t, srv, testHost, "/", cookie, map[string]string{"Origin": testOrigin})
	defer drain(resp)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("proxy to wrong-CA upstream = %d, want 502", resp.StatusCode)
	}
}

// TestProxy_UpstreamSetCookieNotForwarded (OBS-4): a compromised or buggy
// upstream must not be able to overwrite the session cookie — Set-Cookie from
// the runtime is stripped before the response reaches the browser.
func TestProxy_UpstreamSetCookieNotForwarded(t *testing.T) {
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-cookie", testWSUID)
	srv := newGateway(t, fb, nil)
	cookie := launchOK(t, srv, testHost, "tk-cookie")

	resp := proxied(t, srv, testHost, "/", cookie, map[string]string{"Origin": testOrigin})
	defer drain(resp)
	for _, c := range resp.Cookies() {
		if c.Name == gateway.SessionCookieName {
			t.Fatalf("upstream Set-Cookie for %s reached the browser", gateway.SessionCookieName)
		}
	}
}

// TestRevoke_ClosesOpenStream: once the broker stops renewing the lease the
// gateway must close open sockets within the revoke deadline (fail closed).
func TestRevoke_ClosesOpenStream(t *testing.T) {
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-live2", testWSUID)
	srv := newGateway(t, fb, nil)
	cookie := launchOK(t, srv, testHost, "tk-live2")
	lease := fb.leaseOf(t, "tk-live2")

	resp := upgrade(t, srv, testHost, "/websockify", cookie, map[string]string{"Origin": testOrigin})
	if resp.StatusCode != http.StatusSwitchingProtocols {
		drain(resp)
		t.Fatalf("upgrade = %d, want 101", resp.StatusCode)
	}
	defer resp.Body.Close()

	fb.failRenew(lease.ID, broker.ErrRevoked)

	// the open socket must die within the configured revoke deadline
	conn := resp.Body.(io.ReadCloser)
	done := make(chan error, 1)
	go func() {
		_, err := conn.Read(make([]byte, 1))
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("stream stayed readable after revoke")
		}
	case <-time.After(500 * time.Millisecond): // >> 150ms test deadline
		t.Fatal("open stream not closed within revoke deadline after lease failure")
	}
}

// TestUpgrade_SequentialFencesPrevious: a second upgrade on the same session
// fences the old socket (sequential replacement) — never two live streams.
func TestUpgrade_SequentialFencesPrevious(t *testing.T) {
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-seq", testWSUID)
	srv := newGateway(t, fb, nil)
	cookie := launchOK(t, srv, testHost, "tk-seq")

	first := upgrade(t, srv, testHost, "/websockify", cookie, map[string]string{"Origin": testOrigin})
	if first.StatusCode != http.StatusSwitchingProtocols {
		drain(first)
		t.Fatalf("first upgrade = %d, want 101", first.StatusCode)
	}
	defer first.Body.Close()

	second := upgrade(t, srv, testHost, "/websockify", cookie, map[string]string{"Origin": testOrigin})
	defer second.Body.Close()
	if second.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("second upgrade = %d, want 101 (replacement)", second.StatusCode)
	}
	// first must be fenced
	if _, err := first.Body.Read(make([]byte, 1)); err == nil {
		t.Fatal("first stream survived replacement — must be fenced")
	}
}

// ---------------------------------------------------------------------------
// SEC-07 / SEC-20 regression tests: proxied-response policy and
// upstream path hygiene on the shared session origin.
// ---------------------------------------------------------------------------

// TestProxy_HostileUpstreamHeadersDropped (SEC-07 regression): a tenant
// controls its own runtime, so the upstream may be fully hostile. On the
// shared session origin the gateway must drop headers that mutate browser
// state (Set-Cookie for ANY name, Service-Worker-Allowed, Clear-Site-Data,
// Refresh, Link) and must pin its own CSP/nosniff/HSTS — an upstream
// CSP must never weaken the gateway's.
func TestProxy_HostileUpstreamHeadersDropped(t *testing.T) {
	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/javascript")
		w.Header().Add("Set-Cookie", gateway.SessionCookieName+"=stolen; Path=/; Secure; HttpOnly")
		w.Header().Add("Set-Cookie", "tcdi_evil=1; Path=/; Secure")
		w.Header().Set("Service-Worker-Allowed", "/")
		w.Header().Set("Clear-Site-Data", `"*"`)
		w.Header().Set("Refresh", "0;url=https://evil.example/")
		w.Header().Set("Link", "</sw.js>; rel=serviceworker")
		w.Header().Set("Content-Security-Policy", "default-src *")
		w.Header().Set("X-Content-Type-Options", "")
		w.Header().Set("Strict-Transport-Security", "max-age=0")
		_, _ = w.Write([]byte("self.addEventListener('fetch',e=>{})"))
	}))
	defer up.Close()

	fb := newFakeBroker(t)
	fb.scriptTicket("tk-sec7", testWSUID)
	fb.pointLeaseAt(t, "tk-sec7", up)
	srv := newGateway(t, fb, nil)
	cookie := launchOK(t, srv, testHost, "tk-sec7")

	resp := proxied(t, srv, testHost, "/app/hostile.js", cookie, map[string]string{"Origin": testOrigin})
	defer drain(resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("proxied status = %d, want 200", resp.StatusCode)
	}
	for _, h := range []string{
		"Set-Cookie", "Service-Worker-Allowed", "Clear-Site-Data", "Refresh", "Link",
	} {
		if got := resp.Header.Values(h); len(got) > 0 {
			t.Fatalf("hostile %s relayed to the session origin: %v", h, got)
		}
	}
	csp := resp.Header.Get("Content-Security-Policy")
	for _, want := range []string{
		"default-src 'self'", "connect-src 'self' wss://" + testHost,
		"frame-ancestors 'none'", "base-uri 'none'", "object-src 'none'",
		"form-action 'none'",
	} {
		if !strings.Contains(csp, want) {
			t.Fatalf("CSP %q missing %q", csp, want)
		}
	}
	if strings.Contains(csp, "default-src *") {
		t.Fatalf("upstream CSP survived: %q", csp)
	}
	if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := resp.Header.Get("Strict-Transport-Security"); got == "" || got == "max-age=0" {
		t.Fatalf("HSTS = %q, want a real max-age", got)
	}
}

// TestProxy_ServiceWorkerScriptFetchRejected (SEC-07): a fetch carrying
// `Service-Worker: script` is a browser service-worker registration —
// refuse it so no service worker can ever be installed on the shared
// session origin (it would MITM every later request, including other
// tenants' sessions on the same origin).
func TestProxy_ServiceWorkerScriptFetchRejected(t *testing.T) {
	u := newUpstream(t)
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-sw", testWSUID)
	fb.pointLeaseAt(t, "tk-sw", u.srv)
	srv := newGateway(t, fb, nil)
	cookie := launchOK(t, srv, testHost, "tk-sw")

	for _, sw := range []string{"script", "SCRIPT", " script "} {
		resp := proxied(t, srv, testHost, "/app/sw.js", cookie, map[string]string{
			"Origin":         testOrigin,
			"Service-Worker": sw,
		})
		drain(resp)
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("Service-Worker: %q fetch = %d, want 403", sw, resp.StatusCode)
		}
	}
	u.mu.Lock()
	hits := u.hits
	u.mu.Unlock()
	if hits != 0 {
		t.Fatalf("service-worker fetch reached upstream %d times", hits)
	}
}

// TestProxy_SecurityHeadersEverywhere (SEC-07): CSP/nosniff/HSTS apply to
// non-proxied gateway responses too (launch redirect, control, errors) —
// the policy must not depend on hitting the runtime.
func TestProxy_SecurityHeadersEverywhere(t *testing.T) {
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-hdr", testWSUID)
	srv := newGateway(t, fb, nil)

	resp := doLaunch(t, srv, testHost, "tk-hdr", map[string]string{
		"Origin":         testOrigin,
		"Sec-Fetch-Site": "same-origin",
	})
	defer drain(resp)
	for _, h := range []string{"Content-Security-Policy", "X-Content-Type-Options", "Strict-Transport-Security"} {
		if resp.Header.Get(h) == "" {
			t.Fatalf("launch response missing %s", h)
		}
	}

	bad := proxied(t, srv, testHost, "/app/x", "", map[string]string{"Origin": testOrigin})
	defer drain(bad)
	if bad.Header.Get("Content-Security-Policy") == "" {
		t.Fatal("401 response missing CSP")
	}
}

// TestProxy_DotSegmentsRejected (SEC-20 regression): the allowlist must
// be evaluated on the cleaned path. Dot segments, encoded dots/slashes,
// backslashes, double slashes and semicolons must be rejected before auth
// and must never reach the runtime carrying injected credentials.
func TestProxy_DotSegmentsRejected(t *testing.T) {
	u := newUpstream(t)
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-dots", testWSUID)
	fb.pointLeaseAt(t, "tk-dots", u.srv)
	srv := newGateway(t, fb, nil)
	cookie := launchOK(t, srv, testHost, "tk-dots")

	bad := []string{
		"/app/../api/get_users",
		"/app/%2e%2e/api/get_users",
		"/websockify/../api/get_users",
		"/app//assets/x.js",
		"/app/x;y",
		"/app/x%3by",
		"/app/%2fapi/get_users",
		"/app/x%5c..%5capi",
		"/app/./x",
		"/api/%2e%2e/app/x.js",
		"/app/%2E%2E/api/get_users",
	}
	for _, p := range bad {
		resp := proxied(t, srv, testHost, p, cookie, map[string]string{"Origin": testOrigin})
		drain(resp)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("unclean path %q = %d, want 404", p, resp.StatusCode)
		}
	}
	if seen := u.seenURIs(); len(seen) != 0 {
		t.Fatalf("rejected paths reached upstream: %v", seen)
	}
}

// TestProxy_ForwardsCleanedPath (SEC-20): an allowlisted path that arrives
// with unnecessary percent-encoding is forwarded to the runtime in its
// canonical cleaned form — the runtime always sees the path the gateway
// actually authorized.
func TestProxy_ForwardsCleanedPath(t *testing.T) {
	u := newUpstream(t)
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-clean", testWSUID)
	fb.pointLeaseAt(t, "tk-clean", u.srv)
	srv := newGateway(t, fb, nil)
	cookie := launchOK(t, srv, testHost, "tk-clean")

	resp := proxied(t, srv, testHost, "/app/%61ssets/main.js", cookie, map[string]string{"Origin": testOrigin})
	drain(resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("clean encoded path = %d, want 200", resp.StatusCode)
	}
	seen := u.seenURIs()
	if len(seen) != 1 || seen[0] != "/app/assets/main.js" {
		t.Fatalf("upstream saw %v, want [/app/assets/main.js]", seen)
	}
}

// TestControlRevoke_OtherReplica: the operator's revoke reaches whichever
// replica the Service picked; the session may live on the other one. The
// revoke must still reach the broker (the shared lease directory), and the
// replica that holds the session sees the lease die on its next renew and
// closes the stream.
func TestControlRevoke_OtherReplica(t *testing.T) {
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-xrev", testWSUID)
	_, srvA := newReplica(t, fb, "gw-A")
	_, srvB := newReplica(t, fb, "gw-B")
	cookie := launchOK(t, srvA, testHost, "tk-xrev")
	lease := fb.leaseOf(t, "tk-xrev")

	resp := upgrade(t, srvA, testHost, "/websockify", cookie, map[string]string{"Origin": testOrigin})
	if resp.StatusCode != http.StatusSwitchingProtocols {
		drain(resp)
		t.Fatalf("upgrade on A = %d, want 101", resp.StatusCode)
	}
	defer resp.Body.Close()
	wsWrite(t, resp, []byte("x"))
	wsRead(t, resp, 1, 2*time.Second) // the stream is live

	closed := make(chan error, 1)
	go func() {
		_, err := resp.Body.Read(make([]byte, 1))
		closed <- err
	}()

	// B holds no such session — the revoke must not be a local-only lookup.
	status, body := controlRoundTrip(t, srvB, http.MethodPost, "/v1/control/revoke",
		"control-test-token", `{"leaseId":"`+lease.ID+`"}`)
	if status != http.StatusOK || !strings.Contains(body, `"revoked":true`) {
		t.Fatalf("revoke on the other replica = %d %s, want 200 {\"revoked\":true}", status, body)
	}
	if n := fb.leaseRevokeCount(lease.ID); n != 1 {
		t.Fatalf("RevokeLease calls = %d, want 1", n)
	}
	select {
	case err := <-closed:
		if err == nil {
			t.Fatal("A's stream survived the revoke")
		}
	case <-time.After(2 * testRenewInterval):
		t.Fatal("A's stream not closed within 2xRenewInterval of the revoke")
	}
}

// TestControlRevoke_UnknownOrDeadLeaseNotRevoked (R9c): the answer is
// {"revoked":true} and the audit says success only when a live lease was
// actually revoked. An unknown lease ID, or a lease that is already dead,
// answers {"revoked":false} and records no success event.
func TestControlRevoke_UnknownOrDeadLeaseNotRevoked(t *testing.T) {
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-revdead", testWSUID)
	audit := &auditRecorder{}
	srv := newGateway(t, fb, func(c *gateway.Config) { c.Audit = audit })
	launchOK(t, srv, testHost, "tk-revdead")
	lease := fb.leaseOf(t, "tk-revdead")

	revoke := func(id string) (int, string) {
		return controlRoundTrip(t, srv, http.MethodPost, "/v1/control/revoke",
			"control-test-token", `{"leaseId":"`+id+`"}`)
	}
	successes := func() int {
		audit.mu.Lock()
		defer audit.mu.Unlock()
		n := 0
		for _, e := range audit.events {
			if e.Action == "session.revoke" && e.Outcome == observability.OutcomeSuccess {
				n++
			}
		}
		return n
	}

	if status, body := revoke("lease-that-never-existed"); status != http.StatusOK || !strings.Contains(body, `"revoked":false`) {
		t.Fatalf("unknown lease = %d %s, want 200 {\"revoked\":false}", status, body)
	}
	if n := successes(); n != 0 {
		t.Fatalf("unknown lease recorded %d success audit events, want 0", n)
	}

	if status, body := revoke(lease.ID); status != http.StatusOK || !strings.Contains(body, `"revoked":true`) {
		t.Fatalf("live lease = %d %s, want 200 {\"revoked\":true}", status, body)
	}
	if n := successes(); n != 1 {
		t.Fatalf("live lease recorded %d success audit events, want 1", n)
	}

	if status, body := revoke(lease.ID); status != http.StatusOK || !strings.Contains(body, `"revoked":false`) {
		t.Fatalf("already-dead lease = %d %s, want 200 {\"revoked\":false}", status, body)
	}
	if n := successes(); n != 1 {
		t.Fatalf("already-dead lease added a success audit event (%d total, want 1)", n)
	}
}

// TestControlRevoke_BrokerErrorIs503: a revoke the broker did not accept is
// not reported as done — the operator retries.
func TestControlRevoke_BrokerErrorIs503(t *testing.T) {
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-rev503", testWSUID)
	_, srv := newReplica(t, fb, "gw-A")
	launchOK(t, srv, testHost, "tk-rev503")
	lease := fb.leaseOf(t, "tk-rev503")

	fb.mu.Lock()
	fb.revokeErr = errors.New("broker unreachable")
	fb.mu.Unlock()
	status, body := controlRoundTrip(t, srv, http.MethodPost, "/v1/control/revoke",
		"control-test-token", `{"leaseId":"`+lease.ID+`"}`)
	if status != http.StatusServiceUnavailable || strings.Contains(body, `"revoked":true`) {
		t.Fatalf("revoke with a failing broker = %d %s, want 503 and no revoked:true", status, body)
	}
}
