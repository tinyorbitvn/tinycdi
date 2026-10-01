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
	fb.scriptTicket("tk-query", "ws-1")
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
	ok := doLaunch(t, srv, "tk-query", map[string]string{"Origin": testOrigin})
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
	fb.scriptTicket("tk-1", "ws-1")
	srv := newGateway(t, fb, nil)

	resp := doLaunch(t, srv, "tk-1", map[string]string{"Origin": testOrigin})
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

// TestLaunch_BadHostRejected_NoConsume: a Host outside the allowlist is
// rejected and — critically — the ticket is NOT consumed (bad-host
// launch must not burn the ticket).
func TestLaunch_BadHostRejected_NoConsume(t *testing.T) {
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-host", "ws-1")
	srv := newGateway(t, fb, nil)

	resp := doLaunch(t, srv, "tk-host", map[string]string{"Host": "evil.test", "Origin": testOrigin})
	defer drain(resp)
	if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("launch with foreign Host = %d, want 403/400", resp.StatusCode)
	}
	if fb.wasRedeemed("tk-host") {
		t.Fatal("bad-host launch consumed the ticket")
	}
}

// TestLaunch_CrossOriginRejected: a foreign Origin must not redeem.
func TestLaunch_CrossOriginRejected(t *testing.T) {
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-origin", "ws-1")
	srv := newGateway(t, fb, nil)

	resp := doLaunch(t, srv, "tk-origin", map[string]string{"Origin": "https://evil.test"})
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
	fb.scriptTicket("tk-fetch", "ws-1")
	srv := newGateway(t, fb, nil)

	resp := doLaunch(t, srv, "tk-fetch", map[string]string{"Sec-Fetch-Site": "cross-site"})
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

	resp := doLaunch(t, srv, "tk-nope", map[string]string{"Origin": testOrigin})
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
