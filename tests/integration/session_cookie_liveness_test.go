//go:build integration

// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package integration

import (
	"net/http"
	"testing"
	"time"
)

// TestTwoReplicas_SessionCookieStaysValidWhileStreaming (FX-R26): with the
// chart's lease cadence (renew 10 s, revoke deadline 30 s) and a stream held
// open, the session cookie must keep authenticating fresh HTTP requests on
// both replicas for as long as the lease is alive — a page reload re-fetches
// the desktop document and reconnects the websocket with the same cookie and
// no new launch ticket.
func TestTwoReplicas_SessionCookieStaysValidWhileStreaming(t *testing.T) {
	f := newRestartFixture(t)
	a := f.startReplicaWith(t, "a", "-renew-interval", "10s", "-revoke-deadline", "30s")
	defer a.cleanup(t)
	b := f.startReplicaWith(t, "b", "-renew-interval", "10s", "-revoke-deadline", "30s")
	defer b.cleanup(t)

	sess, csrf := f.portalLogin(t, a, a)
	cookie := f.launch(t, b, f.issueTicket(t, a, sess, csrf))
	tickets := f.ticketCount(t)
	leaseID := f.activeLeaseID(t)

	stream := f.wsOpen(t, a, cookie)
	defer stream.Body.Close()
	start := time.Now()

	for _, at := range []time.Duration{5 * time.Second, 15 * time.Second, 25 * time.Second, 35 * time.Second, 60 * time.Second} {
		time.Sleep(time.Until(start.Add(at)))
		for _, r := range []*replica{a, b} {
			resp := f.sessionGet(t, r, "/", cookie)
			code := resp.StatusCode
			drainBody(resp)
			if code != http.StatusOK {
				t.Errorf("GET / on %s at +%s = %d, want 200", r.name, at, code)
			}
		}
	}

	// Reload-style reconnect: the old stream goes away, a new websocket opens
	// with the same cookie (no ticket) and must land on the same lease.
	stream.Body.Close()
	var again *http.Response
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if r := f.wsTry(a, cookie); r != nil {
			if r.StatusCode == http.StatusSwitchingProtocols {
				again = r
				break
			}
			drainBody(r)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if again == nil {
		t.Fatal("reload-style reconnect with the same cookie failed at +60 s")
	}
	defer again.Body.Close()
	wsEcho(t, again)
	if got := f.activeLeaseID(t); got != leaseID {
		t.Fatalf("lease changed across the reload: %s -> %s", leaseID, got)
	}
	if got := f.ticketCount(t); got != tickets {
		t.Fatalf("launch_ticket rows grew: %d -> %d", tickets, got)
	}
}
