// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package gateway_test

// In-portal session view: the session origin is framed by the portal
// origin. These tests pin the framing policy (CSP frame-ancestors limited
// to portal origins, never X-Frame-Options, Permissions-Policy for
// clipboard/fullscreen) and the session cookie modes (lax default,
// partitioned for cross-site portals).

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tinyorbitvn/tinycdi/internal/gateway"
)

const crossSitePortal = "https://workspace.portal-site.test"

func cspDirective(t *testing.T, csp, name string) string {
	t.Helper()
	for _, d := range strings.Split(csp, ";") {
		d = strings.TrimSpace(d)
		if d == name || strings.HasPrefix(d, name+" ") {
			return d
		}
	}
	t.Fatalf("CSP %q has no %s directive", csp, name)
	return ""
}

func assertFramingPolicy(t *testing.T, where string, resp *http.Response, ancestors string, embedders ...string) {
	t.Helper()
	if got := cspDirective(t, resp.Header.Get("Content-Security-Policy"), "frame-ancestors"); got != "frame-ancestors "+ancestors {
		t.Errorf("%s: %q, want frame-ancestors %s", where, got, ancestors)
	}
	if xfo := resp.Header.Values("X-Frame-Options"); len(xfo) > 0 {
		t.Errorf("%s: X-Frame-Options present (%v) — framing must be governed by frame-ancestors only", where, xfo)
	}
	pp := resp.Header.Values("Permissions-Policy")
	if len(pp) != 1 {
		t.Fatalf("%s: Permissions-Policy values=%v, want exactly one pinned value", where, pp)
	}
	allow := "self"
	for _, e := range embedders {
		allow += ` "` + e + `"`
	}
	for _, feat := range []string{"clipboard-read", "clipboard-write", "fullscreen"} {
		if !strings.Contains(pp[0], feat+"=("+allow+")") {
			t.Errorf("%s: Permissions-Policy %q does not grant %s to exactly (%s)", where, pp[0], feat, allow)
		}
	}
	for _, denied := range []string{"camera=()", "microphone=()", "geolocation=()"} {
		if !strings.Contains(pp[0], denied) {
			t.Errorf("%s: Permissions-Policy %q missing %s", where, pp[0], denied)
		}
	}
}

// With no portal origin configured the session origin cannot be framed.
func TestFraming_NoPortalOriginDeniesFraming(t *testing.T) {
	fb := newFakeBroker(t)
	srv := newGateway(t, fb, nil)
	resp := proxied(t, srv, testControlHost, "/healthz", "", nil)
	drain(resp)
	assertFramingPolicy(t, "healthz", resp, "'none'")
}

// Every session-listener response — launch redirect, healthz, errors and
// runtime-proxied content — carries frame-ancestors <portal origins>, the
// pinned Permissions-Policy and no X-Frame-Options, and a hostile upstream
// cannot reintroduce XFO or widen the Permissions-Policy.
func TestFraming_PortalOriginsOnEveryResponse(t *testing.T) {
	up := newHostileFramingUpstream(t)
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-frame", testWSUID)
	fb.pointLeaseAt(t, "tk-frame", up)
	// Mixed case + explicit default port normalize to the browser
	// serialization; a non-default port is kept.
	srv := newGateway(t, fb, withPortalOrigin("https://Portal.Test:443", "https://alt.portal.test:8443"))
	ancestors := "https://portal.test https://alt.portal.test:8443"
	embedders := []string{"https://portal.test", "https://alt.portal.test:8443"}

	resp := doLaunch(t, srv, testHost, "tk-frame", map[string]string{
		"Origin":         testPortalOrigin,
		"Sec-Fetch-Site": "same-site",
		"Sec-Fetch-Dest": "iframe",
	})
	drain(resp)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("framed launch = %d, want 303", resp.StatusCode)
	}
	assertFramingPolicy(t, "launch", resp, ancestors, embedders...)
	var cookie string
	for _, c := range resp.Cookies() {
		if c.Name == gateway.SessionCookieName {
			cookie = c.Value
		}
	}

	for _, tc := range []struct {
		where, host, path, cookie string
	}{
		{"healthz", testControlHost, "/healthz", ""},
		{"not-found", testHost, "/v1/workspaces", cookie},
		{"unauthorized", testHost, "/", ""},
		{"proxied", testHost, "/index.html", cookie},
	} {
		r := proxied(t, srv, tc.host, tc.path, tc.cookie, map[string]string{"Origin": testOrigin})
		drain(r)
		assertFramingPolicy(t, tc.where, r, ancestors, embedders...)
	}
	r := proxied(t, srv, testHost, "/index.html", cookie, map[string]string{"Origin": testOrigin})
	drain(r)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("proxied status=%d", r.StatusCode)
	}
}

func newHostileFramingUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Permissions-Policy", "clipboard-read=*, camera=*")
		w.Header().Set("Feature-Policy", "camera *")
		_, _ = w.Write([]byte("<html></html>"))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestFraming_BadPortalOriginRejected(t *testing.T) {
	fb := newFakeBroker(t)
	for _, bad := range []string{"https://portal.test/path", "javascript:alert(1)", "https://a;b.test"} {
		_, err := gateway.New(gateway.Config{
			SessionDomain: testSessionDomain,
			ControlHosts:  []string{testControlHost},
			Broker:        fb,
			PortalOrigins: []string{bad},
		})
		if err == nil {
			t.Errorf("portal origin %q accepted", bad)
		}
	}
}

func sessionSetCookie(t *testing.T, resp *http.Response) string {
	t.Helper()
	for _, v := range resp.Header.Values("Set-Cookie") {
		if strings.HasPrefix(v, gateway.SessionCookieName+"=") {
			return v
		}
	}
	t.Fatalf("no %s Set-Cookie (status %d)", gateway.SessionCookieName, resp.StatusCode)
	return ""
}

func cookieAttrs(raw string) map[string]string {
	out := map[string]string{}
	for i, part := range strings.Split(raw, ";") {
		if i == 0 {
			continue
		}
		k, v, _ := strings.Cut(strings.TrimSpace(part), "=")
		out[strings.ToLower(k)] = v
	}
	return out
}

// Default (lax) mode: SameSite=Lax, never Partitioned.
func TestCookieMode_LaxDefault(t *testing.T) {
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-lax", testWSUID)
	srv := newGateway(t, fb, withPortalOrigin(testPortalOrigin))
	resp := doLaunch(t, srv, testHost, "tk-lax", map[string]string{
		"Origin":         testPortalOrigin,
		"Sec-Fetch-Site": "same-site",
		"Sec-Fetch-Dest": "iframe",
	})
	drain(resp)
	a := cookieAttrs(sessionSetCookie(t, resp))
	if a["samesite"] != "Lax" {
		t.Fatalf("SameSite=%q want Lax (attrs %v)", a["samesite"], a)
	}
	if _, ok := a["partitioned"]; ok {
		t.Fatalf("lax mode cookie is Partitioned: %v", a)
	}
	for _, need := range []string{"secure", "httponly", "path"} {
		if _, ok := a[need]; !ok {
			t.Fatalf("cookie missing %s: %v", need, a)
		}
	}
	if _, ok := a["domain"]; ok {
		t.Fatalf("host-only cookie carries Domain: %v", a)
	}
}

// Cross-site partitioned case: the portal lives on another site and frames
// the session origin. The launch POST arrives cross-site from the iframe;
// the cookie is SameSite=None; Secure; Partitioned; HttpOnly, host-only,
// and authenticates the follow-up desktop request inside the frame.
func TestCookieMode_PartitionedCrossSite(t *testing.T) {
	u := newUpstream(t)
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-chips", testWSUID)
	fb.pointLeaseAt(t, "tk-chips", u.srv)
	srv := newGateway(t, fb, func(c *gateway.Config) {
		c.PortalOrigins = []string{crossSitePortal}
		c.CookieMode = gateway.CookieModePartitioned
	})
	resp := doLaunch(t, srv, testHost, "tk-chips", map[string]string{
		"Origin":         crossSitePortal,
		"Sec-Fetch-Site": "cross-site",
		"Sec-Fetch-Dest": "iframe",
		"Sec-Fetch-Mode": "navigate",
	})
	drain(resp)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("cross-site framed launch = %d, want 303", resp.StatusCode)
	}
	raw := sessionSetCookie(t, resp)
	a := cookieAttrs(raw)
	if a["samesite"] != "None" {
		t.Fatalf("SameSite=%q want None (%s)", a["samesite"], raw)
	}
	for _, need := range []string{"partitioned", "secure", "httponly"} {
		if _, ok := a[need]; !ok {
			t.Fatalf("partitioned cookie missing %s: %s", need, raw)
		}
	}
	if a["path"] != "/" {
		t.Fatalf("Path=%q want / (%s)", a["path"], raw)
	}
	if _, ok := a["domain"]; ok {
		t.Fatalf("__Host- cookie carries Domain: %s", raw)
	}
	assertFramingPolicy(t, "launch", resp, crossSitePortal, crossSitePortal)

	var cookie string
	for _, c := range resp.Cookies() {
		if c.Name == gateway.SessionCookieName {
			cookie = c.Value
		}
	}
	r := proxied(t, srv, testHost, "/index.html", cookie, map[string]string{
		"Sec-Fetch-Site": "same-origin",
		"Sec-Fetch-Dest": "iframe",
	})
	drain(r)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("desktop page inside the frame = %d, want 200", r.StatusCode)
	}
	// Any other cross-site origin still cannot launch.
	fb.scriptTicket("tk-chips-2", testWSUID2)
	r = doLaunch(t, srv, testHost2, "tk-chips-2", map[string]string{
		"Origin":         "https://evil.example.test",
		"Sec-Fetch-Site": "cross-site",
		"Sec-Fetch-Dest": "iframe",
	})
	drain(r)
	if r.StatusCode != http.StatusForbidden || fb.wasRedeemed("tk-chips-2") {
		t.Fatalf("foreign cross-site launch = %d (redeemed=%v), want 403 unconsumed", r.StatusCode, fb.wasRedeemed("tk-chips-2"))
	}
}

func TestCookieMode_Parse(t *testing.T) {
	for in, want := range map[string]gateway.CookieMode{
		"": gateway.CookieModeLax, "lax": gateway.CookieModeLax, "LAX": gateway.CookieModeLax,
		"partitioned": gateway.CookieModePartitioned, " Partitioned ": gateway.CookieModePartitioned,
	} {
		got, err := gateway.ParseCookieMode(in)
		if err != nil || got != want {
			t.Errorf("ParseCookieMode(%q)=%q,%v want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"strict", "none", "lax;partitioned"} {
		if _, err := gateway.ParseCookieMode(bad); err == nil {
			t.Errorf("ParseCookieMode(%q) accepted", bad)
		}
	}
	_, err := gateway.New(gateway.Config{
		SessionDomain: testSessionDomain, ControlHosts: []string{testControlHost},
		Broker: newFakeBroker(t), CookieMode: "strict",
	})
	if err == nil {
		t.Fatal("gateway.New accepted an unknown cookie mode")
	}
}
