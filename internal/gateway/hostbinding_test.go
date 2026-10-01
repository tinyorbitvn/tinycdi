// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package gateway_test

// Host-binding tests (design §3.2, D9–D12): each workspace is served on its
// own host <label>.<sessionDomain>. Launch and the desktop proxy answer
// only on a host whose label maps to a workspace ID, the redeemed or
// presented session must belong to that workspace, and WebSocket upgrades
// require Origin == the request's own workspace origin. The in-cluster
// control surface (/healthz, /v1/control/*) lives on ControlHosts only.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tinyorbitvn/tinycdi/internal/gateway"
)

// errorBody decodes the {"error": "..."} JSON body.
func errorBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	var v struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	return v.Error
}

// TestHostBinding_LaunchOnOwnHost: a ticket for workspace A posted to A's
// own session host redeems — 303 plus the session cookie.
func TestHostBinding_LaunchOnOwnHost(t *testing.T) {
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-own", testWSUID)
	srv := newGateway(t, fb, nil)

	resp := doLaunch(t, srv, testHost, "tk-own", map[string]string{
		"Origin":         testOrigin,
		"Sec-Fetch-Site": "same-origin",
	})
	defer drain(resp)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("own-host launch = %d, want 303", resp.StatusCode)
	}
	found := false
	for _, c := range resp.Cookies() {
		if c.Name == gateway.SessionCookieName {
			found = true
		}
	}
	if !found {
		t.Fatalf("own-host launch set no %s cookie", gateway.SessionCookieName)
	}
}

// TestHostBinding_LaunchOnOtherHost: a ticket for workspace A posted to
// B's host redeems then fails the host↔lease binding — 403 host_mismatch,
// no cookie, and the minted lease is revoked exactly once.
func TestHostBinding_LaunchOnOtherHost(t *testing.T) {
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-other", testWSUID)
	audit := &auditRecorder{}
	srv := newGateway(t, fb, func(c *gateway.Config) { c.Audit = audit })

	resp := doLaunch(t, srv, testHost2, "tk-other", map[string]string{
		"Origin":         testOrigin2,
		"Sec-Fetch-Site": "same-origin",
	})
	code := resp.StatusCode
	if code != http.StatusForbidden {
		drain(resp)
		t.Fatalf("other-host launch = %d, want 403", code)
	}
	if got := errorBody(t, resp); got != "host_mismatch" {
		t.Fatalf("other-host launch error = %q, want host_mismatch", got)
	}
	for _, c := range resp.Cookies() {
		if c.Name == gateway.SessionCookieName {
			t.Fatal("mismatched launch set a session cookie")
		}
	}
	lease := fb.leaseOf(t, "tk-other")
	if n := fb.revokeCount(lease.ID); n != 1 {
		t.Fatalf("RevokeLease called %d times, want exactly 1", n)
	}
	for _, a := range audit.auditActions() {
		if a == "launch.host_mismatch" {
			return
		}
	}
	t.Fatalf("no launch.host_mismatch audit event in %v", audit.auditActions())
}

// TestHostBinding_CookieOnOtherHost: A's session cookie sent to B's host
// is treated as absent — 401 and nothing reaches the runtime.
func TestHostBinding_CookieOnOtherHost(t *testing.T) {
	u := newUpstream(t)
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-cookie-b", testWSUID)
	fb.pointLeaseAt(t, "tk-cookie-b", u.srv)
	audit := &auditRecorder{}
	srv := newGateway(t, fb, func(c *gateway.Config) { c.Audit = audit })
	cookieA := launchOK(t, srv, testHost, "tk-cookie-b")

	resp := proxied(t, srv, testHost2, "/", cookieA, map[string]string{"Origin": testOrigin2})
	drain(resp)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("A's cookie on B's host = %d, want 401", resp.StatusCode)
	}
	u.mu.Lock()
	hits := u.hits
	u.mu.Unlock()
	if hits != 0 {
		t.Fatalf("foreign-host request reached upstream %d times", hits)
	}
	found := false
	for _, a := range audit.auditActions() {
		if a == "session.host_mismatch" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no session.host_mismatch audit event in %v", audit.auditActions())
	}

	// The probe must not kill A's session: the cookie still works on A's
	// own host.
	ok := proxied(t, srv, testHost, "/", cookieA, map[string]string{"Origin": testOrigin})
	drain(ok)
	if ok.StatusCode != http.StatusOK {
		t.Fatalf("A's cookie on A's host after foreign probe = %d, want 200", ok.StatusCode)
	}
}

// TestHostBinding_TwoWorkspacesStayConnected (D10): the session cookie is
// host-only, so a browser holding cookies for workspaces A and B (one jar
// keyed by host) keeps both sessions — launching B must not disturb A.
func TestHostBinding_TwoWorkspacesStayConnected(t *testing.T) {
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-wsa", testWSUID)
	fb.scriptTicket("tk-wsb", testWSUID2)
	srv := newGateway(t, fb, nil)

	cookieA := launchOK(t, srv, testHost, "tk-wsa")
	cookieB := launchOK(t, srv, testHost2, "tk-wsb")
	if cookieA == cookieB {
		t.Fatal("both launches issued the same cookie value")
	}

	respA := proxied(t, srv, testHost, "/", cookieA, map[string]string{"Origin": testOrigin})
	drain(respA)
	respB := proxied(t, srv, testHost2, "/", cookieB, map[string]string{"Origin": testOrigin2})
	drain(respB)
	if respA.StatusCode != http.StatusOK || respB.StatusCode != http.StatusOK {
		t.Fatalf("proxied A=%d B=%d, want 200/200", respA.StatusCode, respB.StatusCode)
	}
	for _, ticket := range []string{"tk-wsa", "tk-wsb"} {
		if n := fb.revokeCount(fb.leaseOf(t, ticket).ID); n != 0 {
			t.Fatalf("lease for %s revoked %d times — B's launch must not touch A", ticket, n)
		}
	}
}

// TestHostBinding_UnknownHost: a Host that does not Match the session
// domain — the bare domain, a foreign domain, or an extra label — is
// misdirected: 421 bad_host, nothing proxied, no ticket consumed.
func TestHostBinding_UnknownHost(t *testing.T) {
	u := newUpstream(t)
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-h", testWSUID)
	fb.pointLeaseAt(t, "tk-h", u.srv)
	srv := newGateway(t, fb, nil)
	cookie := launchOK(t, srv, testHost, "tk-h")
	fb.scriptTicket("tk-h2", testWSUID)

	for _, host := range []string{testDomain, "evil.test", "a." + testHost} {
		resp := proxied(t, srv, host, "/", cookie, nil)
		if resp.StatusCode != http.StatusMisdirectedRequest {
			drain(resp)
			t.Fatalf("proxied Host %q = %d, want 421", host, resp.StatusCode)
		}
		if got := errorBody(t, resp); got != "bad_host" {
			t.Fatalf("proxied Host %q error = %q, want bad_host", host, got)
		}
		resp = doLaunch(t, srv, host, "tk-h2", map[string]string{"Origin": "https://" + host})
		if resp.StatusCode != http.StatusMisdirectedRequest {
			drain(resp)
			t.Fatalf("launch Host %q = %d, want 421", host, resp.StatusCode)
		}
		drain(resp)
	}
	if fb.wasRedeemed("tk-h2") {
		t.Fatal("launch on an unknown host consumed the ticket")
	}
	u.mu.Lock()
	hits := u.hits
	u.mu.Unlock()
	if hits != 0 {
		t.Fatalf("unknown-host requests reached upstream %d times", hits)
	}
}

// TestHostBinding_WebSocketOrigin (D12): an upgrade requires Origin equal
// to the request's own workspace origin — B's origin on A's host is
// rejected; A's own origin gets 101.
func TestHostBinding_WebSocketOrigin(t *testing.T) {
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-ws-o", testWSUID)
	srv := newGateway(t, fb, nil)
	cookie := launchOK(t, srv, testHost, "tk-ws-o")

	resp := upgrade(t, srv, testHost, "/websockify", cookie, map[string]string{"Origin": testOrigin2})
	drain(resp)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("upgrade with B's origin on A's host = %d, want 403", resp.StatusCode)
	}
	resp = upgrade(t, srv, testHost, "/websockify", cookie, map[string]string{"Origin": testOrigin})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade with A's origin on A's host = %d, want 101", resp.StatusCode)
	}
}

// TestHostBinding_ControlOnlyOnControlHosts: the operator surface is
// bound to the in-cluster Service names — on a workspace host
// /v1/control/* does not exist; on a ControlHosts entry the bearer gate
// applies as before.
func TestHostBinding_ControlOnlyOnControlHosts(t *testing.T) {
	fb := newFakeBroker(t)
	srv := newGateway(t, fb, nil)

	resp := proxied(t, srv, testHost, "/v1/control/session", "", map[string]string{
		"Authorization": "Bearer control-test-token",
	})
	drain(resp)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("/v1/control/session on workspace host = %d, want 404", resp.StatusCode)
	}

	resp = proxied(t, srv, testControlHost, "/v1/control/session", "", map[string]string{
		"Authorization": "Bearer control-test-token",
	})
	drain(resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/v1/control/session on control host with bearer = %d, want 200", resp.StatusCode)
	}
}

// TestHeaders_SessionResponses (D14): every session response carries CSP
// with frame-ancestors <portal origins> (or 'none' when none configured),
// CORP same-origin and Origin-Agent-Cluster — and never X-Frame-Options.
func TestHeaders_SessionResponses(t *testing.T) {
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-hdr", testWSUID)
	srv := newGateway(t, fb, withPortalOrigin(testPortalOrigin))

	resp := doLaunch(t, srv, testHost, "tk-hdr", map[string]string{
		"Origin":         testPortalOrigin,
		"Sec-Fetch-Site": "cross-site",
	})
	drain(resp)
	if got := cspDirective(t, resp.Header.Get("Content-Security-Policy"), "frame-ancestors"); got != "frame-ancestors "+testPortalOrigin {
		t.Fatalf("frame-ancestors = %q, want %q", got, "frame-ancestors "+testPortalOrigin)
	}
	if got := resp.Header.Get("Cross-Origin-Resource-Policy"); got != "same-origin" {
		t.Fatalf("Cross-Origin-Resource-Policy = %q, want same-origin", got)
	}
	if got := resp.Header.Get("Origin-Agent-Cluster"); got != "?1" {
		t.Fatalf("Origin-Agent-Cluster = %q, want ?1", got)
	}
	if xfo := resp.Header.Values("X-Frame-Options"); len(xfo) > 0 {
		t.Fatalf("X-Frame-Options present: %v", xfo)
	}

	// No portal origin configured → framing denied outright.
	fb2 := newFakeBroker(t)
	fb2.scriptTicket("tk-hdr2", testWSUID)
	srv2 := newGateway(t, fb2, nil)
	resp = doLaunch(t, srv2, testHost, "tk-hdr2", nil)
	drain(resp)
	if got := cspDirective(t, resp.Header.Get("Content-Security-Policy"), "frame-ancestors"); got != "frame-ancestors 'none'" {
		t.Fatalf("frame-ancestors without portal origin = %q, want 'none'", got)
	}
}

// TestHeaders_UpstreamCannotOverride: a hostile runtime sends its own CSP,
// X-Frame-Options, CORP and Set-Cookie — the client must see only the
// gateway's pinned values and never the upstream's cookie.
func TestHeaders_UpstreamCannotOverride(t *testing.T) {
	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src *; frame-ancestors *")
		w.Header().Set("X-Frame-Options", "ALLOWALL")
		w.Header().Set("Cross-Origin-Resource-Policy", "cross-origin")
		w.Header().Set("Origin-Agent-Cluster", "?0")
		w.Header().Set("Permissions-Policy", "camera=*")
		w.Header().Add("Set-Cookie", gateway.SessionCookieName+"=stolen; Path=/; Secure; HttpOnly")
		w.Header().Add("Set-Cookie", "tcdi_evil=1; Path=/; Secure")
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(up.Close)

	fb := newFakeBroker(t)
	fb.scriptTicket("tk-ovr", testWSUID)
	fb.pointLeaseAt(t, "tk-ovr", up)
	srv := newGateway(t, fb, withPortalOrigin(testPortalOrigin))
	cookie := launchOK(t, srv, testHost, "tk-ovr")

	resp := proxied(t, srv, testHost, "/index.html", cookie, map[string]string{"Origin": testOrigin})
	drain(resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("proxied status = %d, want 200", resp.StatusCode)
	}
	csp := resp.Header.Values("Content-Security-Policy")
	if len(csp) != 1 || strings.Contains(csp[0], "default-src *") {
		t.Fatalf("upstream CSP reached the client: %v", csp)
	}
	if got := cspDirective(t, csp[0], "frame-ancestors"); got != "frame-ancestors "+testPortalOrigin {
		t.Fatalf("frame-ancestors = %q, want the gateway's portal allowlist", got)
	}
	if xfo := resp.Header.Values("X-Frame-Options"); len(xfo) > 0 {
		t.Fatalf("upstream X-Frame-Options reached the client: %v", xfo)
	}
	if corp := resp.Header.Values("Cross-Origin-Resource-Policy"); len(corp) != 1 || corp[0] != "same-origin" {
		t.Fatalf("Cross-Origin-Resource-Policy = %v, want [same-origin]", corp)
	}
	if oac := resp.Header.Values("Origin-Agent-Cluster"); len(oac) != 1 || oac[0] != "?1" {
		t.Fatalf("Origin-Agent-Cluster = %v, want [?1]", oac)
	}
	if len(resp.Cookies()) > 0 {
		t.Fatalf("upstream cookies reached the client: %v", resp.Cookies())
	}
}

// TestCookieMode_Partitioned (D16): partitioned mode issues
// SameSite=None; Secure; Partitioned — still host-only, HttpOnly, Path=/.
func TestCookieMode_Partitioned(t *testing.T) {
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-part", testWSUID)
	srv := newGateway(t, fb, func(c *gateway.Config) {
		c.CookieMode = gateway.CookieModePartitioned
	})

	resp := doLaunch(t, srv, testHost, "tk-part", map[string]string{
		"Origin":         testOrigin,
		"Sec-Fetch-Site": "same-origin",
	})
	drain(resp)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("partitioned launch = %d, want 303", resp.StatusCode)
	}
	raw := sessionSetCookie(t, resp)
	a := cookieAttrs(raw)
	if a["samesite"] != "None" {
		t.Fatalf("SameSite = %q, want None (%s)", a["samesite"], raw)
	}
	for _, need := range []string{"partitioned", "secure", "httponly"} {
		if _, ok := a[need]; !ok {
			t.Fatalf("partitioned cookie missing %s: %s", need, raw)
		}
	}
	if a["path"] != "/" {
		t.Fatalf("Path = %q, want / (%s)", a["path"], raw)
	}
	if _, ok := a["domain"]; ok {
		t.Fatalf("host-only cookie carries Domain: %s", raw)
	}
}

// TestCSP_PerHostConnectSrc: connect-src names the request's own workspace
// host — A's host gets wss://A, never wss://B.
func TestCSP_PerHostConnectSrc(t *testing.T) {
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-csp", testWSUID)
	srv := newGateway(t, fb, nil)
	cookie := launchOK(t, srv, testHost, "tk-csp")

	resp := proxied(t, srv, testHost, "/", cookie, map[string]string{"Origin": testOrigin})
	drain(resp)
	csp := resp.Header.Get("Content-Security-Policy")
	connect := cspDirective(t, csp, "connect-src")
	if !strings.Contains(connect, "wss://"+testHost) {
		t.Fatalf("connect-src %q does not name wss://%s", connect, testHost)
	}
	if strings.Contains(csp, testHost2) {
		t.Fatalf("CSP on A's host names B's host: %q", csp)
	}
}
