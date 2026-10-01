package gateway_test

// Launch origin policy tests (ADR 0004): the portal is a
// DIFFERENT registrable domain than the session host, so the designed
// launch POST arrives with Origin=<portal origin> and
// Sec-Fetch-Site: cross-site — that flow must succeed. CSRF protection
// comes from the one-use 60s ticket plus the portal-origin allowlist, not
// from demanding Origin == session origin.

import (
	"net/http"
	"testing"

	"github.com/tinyorbitvn/tinycdi/internal/gateway"
)

const testPortalOrigin = "https://portal.test"

// withPortalOrigin configures the test gateway's portal-origin allowlist.
func withPortalOrigin(origins ...string) func(*gateway.Config) {
	return func(c *gateway.Config) { c.PortalOrigins = origins }
}

// TestLaunch_PortalOriginCrossSite: THE designed production flow — portal
// form POST is cross-site by design (design §6) and must redeem.
func TestLaunch_PortalOriginCrossSite(t *testing.T) {
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-xs", testWSUID)
	srv := newGateway(t, fb, withPortalOrigin(testPortalOrigin))

	resp := doLaunch(t, srv, testHost, "tk-xs", map[string]string{
		"Origin":         testPortalOrigin,
		"Sec-Fetch-Site": "cross-site",
	})
	defer drain(resp)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("portal-origin cross-site launch = %d, want 303", resp.StatusCode)
	}
}

// TestLaunch_PortalOriginSameSite: same-site portal (e.g. dev stack where
// portal and session share a registrable domain) must also redeem.
func TestLaunch_PortalOriginSameSite(t *testing.T) {
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-ss", testWSUID)
	srv := newGateway(t, fb, withPortalOrigin(testPortalOrigin))

	resp := doLaunch(t, srv, testHost, "tk-ss", map[string]string{
		"Origin":         testPortalOrigin,
		"Sec-Fetch-Site": "same-site",
	})
	defer drain(resp)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("portal-origin same-site launch = %d, want 303", resp.StatusCode)
	}
}

// TestLaunch_SessionOriginStillAllowed: a POST whose Origin is the
// gateway's own public origin (same-origin tooling) keeps working.
func TestLaunch_SessionOriginStillAllowed(t *testing.T) {
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-so", testWSUID)
	srv := newGateway(t, fb, withPortalOrigin(testPortalOrigin))

	resp := doLaunch(t, srv, testHost, "tk-so", map[string]string{
		"Origin":         testOrigin,
		"Sec-Fetch-Site": "same-origin",
	})
	defer drain(resp)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("session-origin launch = %d, want 303", resp.StatusCode)
	}
}

// TestLaunch_UnknownOriginRejected_NoConsume: an Origin outside the
// allowlist AND not the public origin is rejected without consuming the
// ticket — it must still redeem with a valid Origin afterwards.
func TestLaunch_UnknownOriginRejected_NoConsume(t *testing.T) {
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-unk", testWSUID)
	srv := newGateway(t, fb, withPortalOrigin(testPortalOrigin))

	resp := doLaunch(t, srv, testHost, "tk-unk", map[string]string{
		"Origin":         "https://portal.test.evil.example",
		"Sec-Fetch-Site": "cross-site",
	})
	drain(resp)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("unknown-origin launch = %d, want 403", resp.StatusCode)
	}
	if fb.wasRedeemed("tk-unk") {
		t.Fatal("rejected launch consumed the ticket")
	}
	ok := doLaunch(t, srv, testHost, "tk-unk", map[string]string{"Origin": testPortalOrigin})
	defer drain(ok)
	if ok.StatusCode != http.StatusSeeOther {
		t.Fatalf("ticket burned by rejected launch — redeem = %d, want 303", ok.StatusCode)
	}
}

// TestLaunch_FetchSiteWithoutOrigin: fetch metadata present but no
// attributable Origin (absent or "null") is never enough to redeem.
func TestLaunch_FetchSiteWithoutOrigin(t *testing.T) {
	fb := newFakeBroker(t)
	srv := newGateway(t, fb, withPortalOrigin(testPortalOrigin))

	for _, tc := range []struct {
		name    string
		sfs     string
		headers map[string]string
	}{
		{"same-site no origin", "same-site", map[string]string{"Sec-Fetch-Site": "same-site"}},
		{"same-origin no origin", "same-origin", map[string]string{"Sec-Fetch-Site": "same-origin"}},
		{"null origin", "cross-site", map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "null"}},
	} {
		ticket := "tk-" + tc.name
		fb.scriptTicket(ticket, testWSUID)
		resp := doLaunch(t, srv, testHost, ticket, tc.headers)
		drain(resp)
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s: launch = %d, want 403", tc.name, resp.StatusCode)
		}
		if fb.wasRedeemed(ticket) {
			t.Fatalf("%s: rejected launch consumed the ticket", tc.name)
		}
	}
}

// TestLaunch_NullOriginAlone: a bare `Origin: null` (sandboxed/redirected
// context) is not attributable — reject.
func TestLaunch_NullOriginAlone(t *testing.T) {
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-null", testWSUID)
	srv := newGateway(t, fb, withPortalOrigin(testPortalOrigin))

	resp := doLaunch(t, srv, testHost, "tk-null", map[string]string{"Origin": "null"})
	drain(resp)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("null-origin launch = %d, want 403", resp.StatusCode)
	}
	if fb.wasRedeemed("tk-null") {
		t.Fatal("null-origin launch consumed the ticket")
	}
}

// TestLaunch_NoBrowserHeaders: clients sending neither Origin nor
// Sec-Fetch-Site are non-browser tools (curl, control scripts); they stay
// allowed — the one-use ticket is still required.
func TestLaunch_NoBrowserHeaders(t *testing.T) {
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-cli", testWSUID)
	srv := newGateway(t, fb, withPortalOrigin(testPortalOrigin))

	resp := doLaunch(t, srv, testHost, "tk-cli", nil)
	defer drain(resp)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("headerless launch = %d, want 303 (non-browser clients allowed)", resp.StatusCode)
	}
}

// TestUpgrade_PortalOriginRejected: the portal-origin allowlist applies to
// /v1/launch ONLY — WebSocket upgrades still require Origin == session
// public origin.
func TestUpgrade_PortalOriginRejected(t *testing.T) {
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-ws", testWSUID)
	srv := newGateway(t, fb, withPortalOrigin(testPortalOrigin))
	cookie := launchOK(t, srv, testHost, "tk-ws")

	resp := upgrade(t, srv, testHost, "/websockify", cookie, map[string]string{"Origin": testPortalOrigin})
	defer drain(resp)
	if resp.StatusCode == http.StatusSwitchingProtocols {
		t.Fatal("upgrade with portal Origin got 101 — WS must require the session origin")
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("portal-origin upgrade = %d, want 403", resp.StatusCode)
	}
}
