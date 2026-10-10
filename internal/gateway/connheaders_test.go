// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package gateway_test

// Regression tests for the gateway-injected headers surviving client
// header manipulation: a client's Connection token list must never remove
// the broker-injected Authorization, the rebuilt forwarding chain or
// Sec-WebSocket-Origin, and the client-supplied forwarding family must
// never reach the runtime verbatim. On v0.5.0 the injected headers were
// stamped in the ReverseProxy Director, which runs BEFORE the stdlib
// hop-by-hop strip — a Connection token list naming them deleted them
// outright — and only three forwarding headers were rebuilt.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// headerRecorder is a fake runtime that captures the full inbound header
// map the gateway actually puts on the wire.
type headerRecorder struct {
	mu   sync.Mutex
	hdrs []http.Header
	srv  *httptest.Server
}

func newHeaderRecorder(t *testing.T) *headerRecorder {
	u := &headerRecorder{}
	u.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.hdrs = append(u.hdrs, r.Header.Clone())
		u.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *headerRecorder) last() http.Header {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.hdrs) == 0 {
		return nil
	}
	return u.hdrs[len(u.hdrs)-1]
}

// TestProxy_ConnectionTokensCannotStripInjectedHeaders: a GET whose
// Connection token list names the gateway-injected headers must still
// carry them upstream — the tokens are dropped and the injection happens
// after the hop-by-hop strip. On v0.5.0 the upstream saw an empty
// Authorization/Forwarded/X-Real-Ip here.
func TestProxy_ConnectionTokensCannotStripInjectedHeaders(t *testing.T) {
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-conn", testWSUID)
	up := newHeaderRecorder(t)
	srv := newGateway(t, fb, nil)
	cookie := launchOK(t, srv, testHost, "tk-conn")
	fb.pointLeaseAt(t, "tk-conn", up.srv)

	resp := proxied(t, srv, testHost, "/", cookie, map[string]string{
		"Connection": "Authorization, X-Real-Ip, Forwarded, X-Forwarded-For",
	})
	drain(resp)
	h := up.last()
	if h == nil {
		t.Fatal("upstream not reached")
	}
	if got := h.Get("Authorization"); !strings.HasPrefix(got, "Basic ") {
		t.Fatalf("injected Authorization = %q, want the broker-resolved Basic credential", got)
	}
	for _, name := range []string{"X-Forwarded-For", "Forwarded", "X-Real-Ip"} {
		if h.Get(name) == "" {
			t.Fatalf("injected %s header missing upstream", name)
		}
	}
}

// TestProxy_UpgradeConnectionTokensCannotStripInjected: on a websocket
// upgrade a Connection list of "upgrade" plus names of the injected
// headers must leave Authorization and Sec-WebSocket-Origin intact —
// KasmVNC requires both on the upgrade request. On v0.5.0 both arrived
// empty.
func TestProxy_UpgradeConnectionTokensCannotStripInjected(t *testing.T) {
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-ws", testWSUID)
	up := newHeaderRecorder(t)
	srv := newGateway(t, fb, nil)
	cookie := launchOK(t, srv, testHost, "tk-ws")
	fb.pointLeaseAt(t, "tk-ws", up.srv)

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/websockify", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = testHost
	req.Header.Set("Cookie", "__Host-tcdi_session="+cookie)
	req.Header.Set("Connection", "upgrade, authorization, sec-websocket-origin")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Origin", testOrigin)
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	req.Header.Set("Sec-WebSocket-Version", "13")
	resp, err := srv.Client().Transport.(*http.Transport).RoundTrip(req)
	if err != nil {
		t.Fatalf("upgrade roundtrip: %v", err)
	}
	drain(resp)
	h := up.last()
	if h == nil {
		t.Fatal("upgrade never reached upstream")
	}
	if got := h.Get("Authorization"); !strings.HasPrefix(got, "Basic ") {
		t.Fatalf("upgrade upstream Authorization = %q, want the injected credential", got)
	}
	if got := h.Get("Sec-Websocket-Origin"); got != testOrigin {
		t.Fatalf("Sec-WebSocket-Origin = %q, want the verified origin %q", got, testOrigin)
	}
}

// TestProxy_ClientForwardedFamilyStripped: every client-supplied
// forwarding-family header — X-Forwarded-* variants, X-Original-*,
// X-Rewrite-Url — must be dropped before the verified rebuild; on v0.5.0
// all but the three rebuilt names reached the runtime verbatim.
func TestProxy_ClientForwardedFamilyStripped(t *testing.T) {
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-xf", testWSUID)
	up := newHeaderRecorder(t)
	srv := newGateway(t, fb, nil)
	cookie := launchOK(t, srv, testHost, "tk-xf")
	fb.pointLeaseAt(t, "tk-xf", up.srv)

	forged := map[string]string{
		"X-Forwarded-Host":         "evil.example.com",
		"X-Forwarded-Proto":        "http",
		"X-Forwarded-Port":         "1337",
		"X-Forwarded-Ssl":          "off",
		"X-Forwarded-Server":       "evil.example.com",
		"X-Forwarded-Custom":       "evil.example.com",
		"X-Original-Url":           "/api/admin",
		"X-Original-Forwarded-For": "6.6.6.6",
		"X-Rewrite-Url":            "/api/admin",
	}
	resp := proxied(t, srv, testHost, "/", cookie, forged)
	drain(resp)
	h := up.last()
	if h == nil {
		t.Fatal("upstream not reached")
	}
	for name := range forged {
		if got := h.Get(name); got != "" {
			t.Fatalf("client-supplied %s reached the runtime: %q", name, got)
		}
	}
	// The rebuilt chain still identifies the real client: the socket peer.
	for _, name := range []string{"X-Forwarded-For", "Forwarded", "X-Real-Ip"} {
		if h.Get(name) == "" {
			t.Fatalf("rebuilt %s missing upstream", name)
		}
	}
}
