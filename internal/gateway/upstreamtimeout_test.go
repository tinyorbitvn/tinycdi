// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package gateway_test

// Tests for the per-session upstream transport deadlines (a hostile
// runtime must not pin request goroutines or the upgrade admission) and
// for the dropped-Connection-token counter.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/tinyorbitvn/tinycdi/internal/gateway"
	"github.com/tinyorbitvn/tinycdi/internal/observability"
)

// TestProxy_UpstreamResponseHeaderTimeout: a runtime that accepts TLS and
// then stalls must not hold a proxied request open forever — the gateway
// answers 502 inside the configured bound. The established websocket
// stream is exempt by construction: the timeout covers only the wait for
// response headers, the 101 IS the response headers, and once it is read
// the transport yields the conn to the hijacked stream — so a live stream
// survives well past the timeout.
func TestProxy_UpstreamResponseHeaderTimeout(t *testing.T) {
	// Never sends response headers; returns when the transport gives up
	// and cancels the request context so server cleanup cannot hang.
	stalled := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(stalled.Close)

	const timeout = 200 * time.Millisecond
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-stall", testWSUID)
	fb.pointLeaseAt(t, "tk-stall", stalled)
	// The live-stream check needs a second workspace host: two sessions on
	// one host would fence each other.
	fb.scriptTicket("tk-livews", testWSUID2) // default target: the echo upstream
	srv := newGateway(t, fb, func(c *gateway.Config) {
		c.UpstreamResponseHeaderTimeout = timeout
	})
	cookie := launchOK(t, srv, testHost, "tk-stall")
	cookieWS := launchOK(t, srv, testHost2, "tk-livews")

	// Cap the client side so a regression cannot hang the test forever.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = testHost
	req.Header.Set("Cookie", gateway.SessionCookieName+"="+cookie)

	start := time.Now()
	resp, err := srv.Client().Transport.(*http.Transport).RoundTrip(req)
	if err != nil {
		t.Fatalf("stalled-upstream roundtrip: %v", err)
	}
	drain(resp)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("stalled upstream = %d, want 502", resp.StatusCode)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("stalled request took %v — ResponseHeaderTimeout did not bound it", d)
	}

	// An established upgrade is unaffected: it stays usable long past the
	// timeout that only bounded its handshake.
	up := upgrade(t, srv, testHost2, "/websockify", cookieWS, map[string]string{"Origin": testOrigin2})
	if up.StatusCode != http.StatusSwitchingProtocols {
		drain(up)
		t.Fatalf("upgrade = %d, want 101", up.StatusCode)
	}
	defer up.Body.Close()
	time.Sleep(3 * timeout)
	wsWrite(t, up, []byte("ping"))
	if got := wsRead(t, up, 4, 2*time.Second); string(got) != "ping" {
		t.Fatalf("established stream died past the header timeout: echo %q", got)
	}
}

// TestProxy_ConnectionTokensDroppedMetric: Connection tokens outside the
// upgrade/keep-alive/close allowlist are counted, not forwarded —
// requests that send only legitimate tokens never move the counter.
func TestProxy_ConnectionTokensDroppedMetric(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := observability.NewMetrics(reg, nil)
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-ctm", testWSUID)
	srv := newGateway(t, fb, func(c *gateway.Config) { c.Metrics = m })
	cookie := launchOK(t, srv, testHost, "tk-ctm")

	counter := func() float64 {
		t.Helper()
		mfs, err := reg.Gather()
		if err != nil {
			t.Fatal(err)
		}
		for _, mf := range mfs {
			if mf.GetName() != "tinycdi_gateway_connection_tokens_dropped_total" {
				continue
			}
			var v float64
			for _, met := range mf.GetMetric() {
				v += met.GetCounter().GetValue()
			}
			return v
		}
		return 0
	}

	resp := proxied(t, srv, testHost, "/", cookie, map[string]string{
		"Connection": "keep-alive, authorization, x-real-ip",
	})
	drain(resp)
	if got := counter(); got != 2 {
		t.Fatalf("dropped-token counter = %v, want 2", got)
	}

	// A clean token list drops nothing and moves nothing.
	resp = proxied(t, srv, testHost, "/", cookie, map[string]string{
		"Connection": "keep-alive",
	})
	drain(resp)
	if got := counter(); got != 2 {
		t.Fatalf("dropped-token counter = %v after a clean request, want 2", got)
	}
}
