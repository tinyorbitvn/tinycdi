package gateway_test

// Launch contract tests (design §6.3; semantics proven in ADR 0001 by the
// fixture gateway suite).

import (
	"net/http"
	"strings"
	"testing"

	"github.com/tinyorbitvn/tinycdi/internal/gateway"
)

// TestLaunch_PostOnly: the launch endpoint accepts POST only — a GET must
// never redeem (and could leak the ticket via query/referer).
func TestLaunch_PostOnly(t *testing.T) {
	fb := newFakeBroker(t)
	srv := newGateway(t, fb, nil)

	req, err := http.NewRequest(http.MethodGet, srv.URL+gateway.LaunchPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = testHost
	resp, err := srv.Client().Transport.(*http.Transport).RoundTrip(req)
	if err != nil {
		t.Fatalf("GET launch: %v", err)
	}
	defer drain(resp)
	if resp.StatusCode == http.StatusSeeOther || resp.StatusCode == http.StatusOK {
		t.Fatalf("GET /v1/launch returned %d — launch is POST-only", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET /v1/launch = %d, want 405", resp.StatusCode)
	}
}

// TestLaunch_TicketInQueryRejected: a ticket in the query string is never
// redeemed — the ticket travels only in the POST body (verified: ticket-in-query
// 405/reject, non-consumption proven). The ticket must remain usable via the
// body afterwards, proving the attempt did not consume it.
func TestLaunch_TicketInQueryRejected(t *testing.T) {
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-query", testWSUID)
	srv := newGateway(t, fb, nil)

	req, err := http.NewRequest(http.MethodPost, srv.URL+gateway.LaunchPath+"?ticket=tk-query", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = testHost
	req.Header.Set("Origin", testOrigin)
	r, err := srv.Client().Transport.(*http.Transport).RoundTrip(req)
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	drain(r)
	if r.StatusCode == http.StatusSeeOther {
		t.Fatal("ticket in query string redeemed — must be rejected")
	}
	if fb.wasRedeemed("tk-query") {
		t.Fatal("ticket in query was consumed by the broker")
	}
	// non-consumption proof: the same ticket still redeems via the body
	ok := doLaunch(t, srv, testHost, "tk-query", map[string]string{"Origin": testOrigin})
	defer drain(ok)
	if ok.StatusCode != http.StatusSeeOther {
		t.Fatalf("ticket burned by rejected query attempt — body redeem = %d, want 303", ok.StatusCode)
	}
}

// TestLaunch_SetsHostOnlyCookie: a valid redeem returns 303 to a clean URL
// plus a __Host- Secure HttpOnly SameSite=Lax cookie whose value is not the
// ticket (cookie-flag contract). Lax, not Strict: the launch POST is
// cross-site by design, and a Strict cookie is not sent on the POST→303
// top-level redirect, so the desktop would load "unauthorized" (a launch
// regression).
func TestLaunch_SetsHostOnlyCookie(t *testing.T) {
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-1", testWSUID)
	srv := newGateway(t, fb, nil)

	resp := doLaunch(t, srv, testHost, "tk-1", map[string]string{"Origin": testOrigin})
	defer drain(resp)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("launch status = %d, want 303", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if loc == "" || strings.Contains(loc, "tk-1") || strings.Contains(loc, "ticket=") {
		t.Fatalf("redirect %q leaks ticket or is not clean", loc)
	}
	var sess *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == gateway.SessionCookieName {
			sess = c
		}
	}
	if sess == nil {
		t.Fatalf("no %s cookie set", gateway.SessionCookieName)
	}
	if !sess.Secure || !sess.HttpOnly || sess.SameSite != http.SameSiteLaxMode || sess.Path != "/" || sess.Domain != "" {
		t.Fatalf("cookie flags wrong: %+v — need Secure HttpOnly SameSite=Lax Path=/ no Domain", sess)
	}
	if strings.Contains(sess.Value, "tk-1") {
		t.Fatal("session cookie value contains the ticket")
	}
}

// TestLaunch_RedirectLoadsDesktopWithRemoteResize (FX-R18): the 303 lands the
// KasmVNC web client with resize=remote plus the static embedded-parity
// settings (V3.24: tab-mode WebP offer, no client-side idle cut before the
// platform lifecycle). The client treats a page inside an iframe as an
// embedded widget and silently forces resize=off, which keeps the remote
// screen at its old size: a larger in-portal frame then shows large dark
// regions around (or instead of) the desktop. The query is a static,
// non-secret client setting; it never carries ticket or session material.
func TestLaunch_RedirectLoadsDesktopWithRemoteResize(t *testing.T) {
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-1", testWSUID)
	srv := newGateway(t, fb, nil)

	resp := doLaunch(t, srv, testHost, "tk-1", map[string]string{"Origin": testOrigin})
	defer drain(resp)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("launch status = %d, want 303", resp.StatusCode)
	}
	if loc, want := resp.Header.Get("Location"),
		"/?resize=remote&enable_webp=true&idle_disconnect=1440&clipboard_up=false&clipboard_down=false"; loc != want {
		t.Fatalf("redirect Location = %q, want %q", loc, want)
	}
}

// TestLaunch_RedirectCarriesRecordedClipboardPolicy (V3.24): the 303 is
// the URL the session frame actually loads — the ticket POST's redirect
// supersedes the portal's iframe-src params — so the redirect re-asserts
// the client's clipboard flags from the policy the ticket recorded at
// issue. clipboard_seamless follows the client's own non-embed default:
// Chrome-family only (Firefox/Safari disable it upstream).
func TestLaunch_RedirectCarriesRecordedClipboardPolicy(t *testing.T) {
	chrome := "Mozilla/5.0 Chrome/120.0 Safari/537.36"
	firefox := "Mozilla/5.0 Firefox/121.0"
	for _, tc := range []struct {
		policy, ua, want string
	}{
		{"Bidirectional", chrome, "clipboard_up=true&clipboard_down=true&clipboard_seamless=true"},
		{"Bidirectional", firefox, "clipboard_up=true&clipboard_down=true&clipboard_seamless=false"},
		{"Send", chrome, "clipboard_up=true&clipboard_down=false&clipboard_seamless=true"},
		{"Receive", chrome, "clipboard_up=false&clipboard_down=true&clipboard_seamless=true"},
		{"Disabled", chrome, "clipboard_up=false&clipboard_down=false"},
		{"", chrome, "clipboard_up=false&clipboard_down=false"},
	} {
		fb := newFakeBroker(t)
		fb.scriptTicketPolicy("tk-1", testWSUID, tc.policy)
		srv := newGateway(t, fb, nil)
		resp := doLaunch(t, srv, testHost, "tk-1", map[string]string{
			"Origin":     testOrigin,
			"User-Agent": tc.ua,
		})
		loc := resp.Header.Get("Location")
		drain(resp)
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("policy %q: launch status = %d, want 303", tc.policy, resp.StatusCode)
		}
		if !strings.HasSuffix(loc, tc.want) {
			t.Fatalf("policy %q UA %q: Location %q missing %q", tc.policy, tc.ua, loc, tc.want)
		}
	}
}

// TestLaunch_BadHostRejected_NoConsume: a Host outside the allowlist is
// rejected and — critically — the ticket is NOT consumed (bad-host
// launch must not burn the ticket).
func TestLaunch_BadHostRejected_NoConsume(t *testing.T) {
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-host", testWSUID)
	srv := newGateway(t, fb, nil)

	resp := doLaunch(t, srv, "evil.test", "tk-host", map[string]string{"Origin": testOrigin})
	defer drain(resp)
	if resp.StatusCode != http.StatusMisdirectedRequest {
		t.Fatalf("launch with foreign Host = %d, want 421", resp.StatusCode)
	}
	if fb.wasRedeemed("tk-host") {
		t.Fatal("bad-host launch consumed the ticket")
	}
}

// TestLaunch_CrossOriginRejected: a foreign Origin must not redeem.
func TestLaunch_CrossOriginRejected(t *testing.T) {
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-origin", testWSUID)
	srv := newGateway(t, fb, nil)

	resp := doLaunch(t, srv, testHost, "tk-origin", map[string]string{"Origin": "https://evil.test"})
	defer drain(resp)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin launch = %d, want 403", resp.StatusCode)
	}
	if fb.wasRedeemed("tk-origin") {
		t.Fatal("cross-origin launch consumed the ticket")
	}
}

// TestLaunch_CrossSiteFetchMetadataRejected: Firefox omits Origin on
// same-site POSTs, so Sec-Fetch-Site is the remaining browser-context signal
// — cross-site must be rejected (fetch-metadata gate).
func TestLaunch_CrossSiteFetchMetadataRejected(t *testing.T) {
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-fetch", testWSUID)
	srv := newGateway(t, fb, nil)

	resp := doLaunch(t, srv, testHost, "tk-fetch", map[string]string{"Sec-Fetch-Site": "cross-site"})
	defer drain(resp)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-site fetch-metadata launch = %d, want 403", resp.StatusCode)
	}
	if fb.wasRedeemed("tk-fetch") {
		t.Fatal("cross-site launch consumed the ticket")
	}
}

// TestLaunch_InvalidTicket: a bad ticket never yields a session.
func TestLaunch_InvalidTicket(t *testing.T) {
	fb := newFakeBroker(t)
	srv := newGateway(t, fb, nil)

	resp := doLaunch(t, srv, testHost, "tk-nope", map[string]string{"Origin": testOrigin})
	defer drain(resp)
	if resp.StatusCode == http.StatusSeeOther {
		t.Fatal("invalid ticket redeemed to a session")
	}
	if resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden {
		t.Fatalf("invalid ticket launch = %d, want 401/403", resp.StatusCode)
	}
	for _, c := range resp.Cookies() {
		if c.Name == gateway.SessionCookieName {
			t.Fatal("session cookie set on failed launch")
		}
	}
}
