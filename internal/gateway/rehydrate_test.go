// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package gateway_test

// Cross-replica session tests: two gateway handlers built on one fakeBroker
// stand for two replicas of the same deployment — the fake's digest map is
// the Postgres lease directory (D19/P3/P6). Sessions are rebuilt on the
// replica that receives the cookie, stream epochs fence older streams, and
// Drain sheds streams without revoking the lease.

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/broker"
	"github.com/tinyorbitvn/tinycdi/internal/gateway"
)

// TestRehydrate_SecondGatewayServesCookie: a session launched on replica A
// is served by replica B, which rebuilds it from the digest and renews the
// lease itself.
func TestRehydrate_SecondGatewayServesCookie(t *testing.T) {
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-rehy", "ws-1")
	_, srvA := newReplica(t, fb, "gw-A")
	_, srvB := newReplica(t, fb, "gw-B")

	cookie := launchOK(t, srvA, "tk-rehy")

	resp := proxied(t, srvB, "/", cookie, nil)
	defer drain(resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("proxied on B = %d, want 200", resp.StatusCode)
	}
	if fb.lookupCount() != 1 {
		t.Fatalf("LeaseBySession calls = %d, want 1", fb.lookupCount())
	}
	// B rebuilt the session and started its own renew loop.
	deadline := time.Now().Add(2 * testRenewInterval)
	for fb.renewCount("gw-B") == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if fb.renewCount("gw-B") == 0 {
		t.Fatal("no RenewLease from replica B within 2xRenewInterval")
	}
}

// TestRehydrate_UnknownCookieAllocatesNothing: a flood-safe cookie miss is
// one store lookup — no session object, no renew or activity goroutines.
// The map and mint counters also catch an allocate-then-kill
// implementation: a session built and then destroyed inside the request
// path leaves no observable side effect except these.
func TestRehydrate_UnknownCookieAllocatesNothing(t *testing.T) {
	fb := newFakeBroker(t)
	gwB, srvB := newReplica(t, fb, "gw-B")

	mintsBefore := gateway.SessionMints()
	resp := proxied(t, srvB, "/", "random", nil)
	drain(resp)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("proxied with random cookie = %d, want 401", resp.StatusCode)
	}
	if s, l, w, i := gwB.SessionMapCounts(); s+l+w+i != 0 {
		t.Fatalf("session state after unknown cookie: sessions=%d byLease=%d byWorkspace=%d inflight=%d, want all 0",
			s, l, w, i)
	}
	if got := gateway.SessionMints() - mintsBefore; got != 0 {
		t.Fatalf("newSession calls for unknown cookie = %d, want 0 — a miss must allocate nothing", got)
	}
	time.Sleep(3 * testRenewInterval)
	if n := fb.renewCount("gw-B"); n != 0 {
		t.Fatalf("renew calls after unknown cookie = %d, want 0", n)
	}
	if n := len(fb.activityTypes()); n != 0 {
		t.Fatalf("activity reports after unknown cookie = %d, want 0", n)
	}
}

// TestRehydrate_DeadLeaseRejected: a cookie bound to a revoked lease gets
// 401 on the other replica, not a resurrected session.
func TestRehydrate_DeadLeaseRejected(t *testing.T) {
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-dead", "ws-1")
	_, srvA := newReplica(t, fb, "gw-A")
	_, srvB := newReplica(t, fb, "gw-B")
	cookie := launchOK(t, srvA, "tk-dead")
	lease := fb.leaseOf(t, "tk-dead")

	if err := fb.RevokeLease(context.Background(), lease.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	resp := proxied(t, srvB, "/", cookie, nil)
	defer drain(resp)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("proxied on B after revoke = %d, want 401", resp.StatusCode)
	}
}

// TestRehydrate_ConcurrentRequestsSingleLookup: 20 parallel requests with
// the same unseen cookie share a single directory lookup. The fake's
// LeaseBySession blocks on a channel until every request is in flight —
// one goroutine inside the lookup, the rest parked on the shared call's
// done channel — so the overlap is a deterministic rendezvous: a missing
// dedup makes every request enter LeaseBySession and is caught on the
// spot, not probabilistically at -count=5.
func TestRehydrate_ConcurrentRequestsSingleLookup(t *testing.T) {
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-conc", "ws-1")
	_, srvA := newReplica(t, fb, "gw-A")
	gwB, srvB := newReplica(t, fb, "gw-B")
	cookie := launchOK(t, srvA, "tk-conc")

	release := fb.gateLookups()

	const n = 20
	var wg sync.WaitGroup
	codes := make(chan int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, err := http.NewRequest(http.MethodGet, srvB.URL+"/", nil)
			if err != nil {
				codes <- -1
				return
			}
			req.Host = testHost
			req.Header.Set("Cookie", gateway.SessionCookieName+"="+cookie)
			resp, err := srvB.Client().Transport.(*http.Transport).RoundTrip(req)
			if err != nil {
				codes <- -1
				return
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			codes <- resp.StatusCode
		}()
	}

	// Rendezvous: all n requests are inside the session lookup — either in
	// LeaseBySession or parked on the shared call's done channel. With the
	// dedup intact that is exactly 1 entered call + n-1 waiters.
	deadline := time.Now().Add(10 * time.Second)
	for fb.lookupCount()+gwB.InflightWaiters() < n && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	close(release)
	if got := fb.lookupCount(); got != 1 {
		t.Fatalf("LeaseBySession calls while %d requests were in flight = %d, want 1 (shared lookup)", n, got)
	}
	wg.Wait()
	close(codes)
	for c := range codes {
		if c != http.StatusOK {
			t.Fatalf("concurrent proxied status = %d, want 200", c)
		}
	}
	if got := fb.lookupCount(); got != 1 {
		t.Fatalf("LeaseBySession calls = %d, want 1 (shared lookup)", got)
	}
}

// TestRehydrate_NilDirectoryKeepsV01Behaviour: with no session directory a
// replica has only its own sessions — A's cookie is a miss on B.
func TestRehydrate_NilDirectoryKeepsV01Behaviour(t *testing.T) {
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-nil", "ws-1")
	_, srvA := newReplica(t, fb, "gw-A")
	srvB := newGateway(t, fb, nil) // no Sessions: v0.1 single-process mode

	cookie := launchOK(t, srvA, "tk-nil")
	resp := proxied(t, srvB, "/", cookie, nil)
	defer drain(resp)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("proxied on directory-less B = %d, want 401", resp.StatusCode)
	}
}

// TestLaunch_BindFailureIssuesNoCookie: when the directory cannot record
// the digest, launch fails closed — the lease is revoked, no cookie is set.
func TestLaunch_BindFailureIssuesNoCookie(t *testing.T) {
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-bindfail", "ws-1")
	fb.setBindErr(errors.New("directory unreachable"))
	_, srv := newReplica(t, fb, "gw-A")

	resp := doLaunch(t, srv, "tk-bindfail", map[string]string{
		"Origin":         testOrigin,
		"Sec-Fetch-Site": "same-origin",
	})
	defer drain(resp)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("launch with failing BindSession = %d, want 503", resp.StatusCode)
	}
	if sc := resp.Header.Get("Set-Cookie"); sc != "" {
		t.Fatalf("bind failure still set cookie: %q", sc)
	}
	if n := fb.revokeCount(); n != 1 {
		t.Fatalf("RevokeLease calls = %d, want 1", n)
	}
}

// TestStreamFence_CrossGateway: admitting a stream on replica B bumps the
// lease's stream epoch; A's renew loop sees the newer epoch and closes its
// stream while the session itself stays alive.
func TestStreamFence_CrossGateway(t *testing.T) {
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-fence", "ws-1")
	_, srvA := newReplica(t, fb, "gw-A")
	_, srvB := newReplica(t, fb, "gw-B")
	cookie := launchOK(t, srvA, "tk-fence")

	respA := upgrade(t, srvA, "/websockify", cookie, map[string]string{"Origin": testOrigin})
	if respA.StatusCode != http.StatusSwitchingProtocols {
		drain(respA)
		t.Fatalf("upgrade on A = %d, want 101", respA.StatusCode)
	}
	defer respA.Body.Close()
	wsWrite(t, respA, []byte("x"))
	wsRead(t, respA, 1, 2*time.Second) // prove the stream is live

	respB := upgrade(t, srvB, "/websockify", cookie, map[string]string{"Origin": testOrigin})
	if respB.StatusCode != http.StatusSwitchingProtocols {
		drain(respB)
		t.Fatalf("upgrade on B = %d, want 101", respB.StatusCode)
	}
	defer respB.Body.Close()

	// A's stream must be fenced once its renew observes the bumped epoch.
	done := make(chan error, 1)
	go func() {
		_, err := respA.Body.Read(make([]byte, 1))
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("A's stream survived B's stream claim")
		}
	case <-time.After(2 * testRenewInterval):
		t.Fatal("A's stream not fenced within 2xRenewInterval")
	}

	// B's stream stays usable.
	wsWrite(t, respB, []byte("y"))
	wsRead(t, respB, 1, 2*time.Second)
}

// TestDrain_KeepsLease: Drain closes open streams, reports disconnect for
// each, and never revokes — the same cookie reconnects on replica B.
func TestDrain_KeepsLease(t *testing.T) {
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-drain", "ws-1")
	gwA, srvA := newReplica(t, fb, "gw-A")
	_, srvB := newReplica(t, fb, "gw-B")
	cookie := launchOK(t, srvA, "tk-drain")

	resp := upgrade(t, srvA, "/websockify", cookie, map[string]string{"Origin": testOrigin})
	if resp.StatusCode != http.StatusSwitchingProtocols {
		drain(resp)
		t.Fatalf("upgrade on A = %d, want 101", resp.StatusCode)
	}
	defer resp.Body.Close()
	wsWrite(t, resp, []byte("x"))
	wsRead(t, resp, 1, 2*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	gwA.Drain(ctx)
	cancel()

	if _, err := resp.Body.Read(make([]byte, 1)); err == nil {
		t.Fatal("stream survived Drain")
	}

	// Drain waited for the queue to flush: the disconnect is recorded and
	// the lease was never revoked.
	var disconnects int
	for _, ty := range fb.activityTypes() {
		if ty == broker.ActivityDisconnect {
			disconnects++
		}
	}
	if disconnects == 0 {
		t.Fatal("Drain did not report a disconnect")
	}
	if n := fb.revokeCount(); n != 0 {
		t.Fatalf("RevokeLease calls = %d, want 0", n)
	}

	respB := proxied(t, srvB, "/", cookie, nil)
	defer drain(respB)
	if respB.StatusCode != http.StatusOK {
		t.Fatalf("proxied on B after A drained = %d, want 200", respB.StatusCode)
	}
}

// TestDrain_RefusesNewUpgrades: once Drain starts the replica sheds — new
// WebSocket upgrades get 503 even with a valid cookie.
func TestDrain_RefusesNewUpgrades(t *testing.T) {
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-drain2", "ws-1")
	gwA, srvA := newReplica(t, fb, "gw-A")
	cookie := launchOK(t, srvA, "tk-drain2")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	gwA.Drain(ctx)
	cancel()

	resp := upgrade(t, srvA, "/websockify", cookie, map[string]string{"Origin": testOrigin})
	defer drain(resp)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("upgrade after Drain = %d, want 503", resp.StatusCode)
	}
}
