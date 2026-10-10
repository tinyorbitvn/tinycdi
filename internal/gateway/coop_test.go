// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package gateway_test

// COOP pinning: the session listener pins
// Cross-Origin-Opener-Policy: same-origin on every response like the
// frontend does — a document that opens a session-host URL can no longer
// keep it in its own browsing-context group — and a tenant-controlled
// upstream can never weaken it through the proxy. COOP does not affect
// framing: the portal iframe embedding is governed by CSP
// frame-ancestors, which stays unchanged.

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tinyorbitvn/tinycdi/internal/gateway"
)

// TestResponsePolicy_PinsCOOP: launch, proxied, denied and control
// responses all carry COOP: same-origin. Fails on v0.5.0 — no COOP was
// emitted.
func TestResponsePolicy_PinsCOOP(t *testing.T) {
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-coop", testWSUID)
	srv := newGateway(t, fb, withPortalOrigin(testPortalOrigin))

	coop := func(where string, resp *http.Response) {
		t.Helper()
		if got := resp.Header.Get("Cross-Origin-Opener-Policy"); got != "same-origin" {
			t.Fatalf("%s: Cross-Origin-Opener-Policy = %q, want same-origin", where, got)
		}
	}

	resp := doLaunch(t, srv, testHost, "tk-coop", map[string]string{
		"Origin":         testPortalOrigin,
		"Sec-Fetch-Site": "cross-site",
	})
	coop("launch 303", resp)
	// COOP changes nothing about who may frame the session origin: the
	// portal iframe embedding keeps working because frame-ancestors —
	// unchanged — still names the portal.
	if got := cspDirective(t, resp.Header.Get("Content-Security-Policy"), "frame-ancestors"); got != "frame-ancestors "+testPortalOrigin {
		t.Fatalf("launch 303 frame-ancestors = %q, want %q — portal embedding must be unaffected", got, "frame-ancestors "+testPortalOrigin)
	}
	var cookie string
	for _, c := range resp.Cookies() {
		if c.Name == gateway.SessionCookieName {
			cookie = c.Value
		}
	}
	drain(resp)
	if cookie == "" {
		t.Fatal("launch set no session cookie")
	}

	resp = proxied(t, srv, testHost, "/", cookie, nil)
	coop("proxied 200", resp)
	drain(resp)

	resp = proxied(t, srv, testHost, "/", "bogus", nil)
	coop("unauthenticated 401", resp)
	drain(resp)

	resp = proxied(t, srv, testHost, "/", "", nil)
	coop("cookie-less 401", resp)
	drain(resp)
}

// TestResponsePolicy_StripsUpstreamCOOP: a tenant-controlled runtime may
// answer with a permissive COOP — the gateway drops it before pinning
// its own value, exactly like the rest of the security-header set.
// Fails on v0.5.0 (COOP was neither stripped nor pinned).
func TestResponsePolicy_StripsUpstreamCOOP(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cross-Origin-Opener-Policy", "unsafe-none")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(upstream.Close)

	fb := newFakeBroker(t)
	fb.scriptTicket("tk-coop-strip", testWSUID)
	fb.pointLeaseAt(t, "tk-coop-strip", upstream)
	srv := newGateway(t, fb, nil)
	cookie := launchOK(t, srv, testHost, "tk-coop-strip")

	resp := proxied(t, srv, testHost, "/", cookie, nil)
	defer drain(resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("proxied = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Values("Cross-Origin-Opener-Policy"); len(got) != 1 || got[0] != "same-origin" {
		t.Fatalf("proxied COOP = %v, want [same-origin] — the upstream value must be stripped", got)
	}
}
