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
	"net/http/httptest"
	"strings"
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
	fb.scriptTicket("tk-rehy", testWSUID)
	_, srvA := newReplica(t, fb, "gw-A")
	_, srvB := newReplica(t, fb, "gw-B")

	cookie := launchOK(t, srvA, testHost, "tk-rehy")

	resp := proxied(t, srvB, testHost, "/", cookie, nil)
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
	resp := proxied(t, srvB, testHost, "/", "random", nil)
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
	fb.scriptTicket("tk-dead", testWSUID)
	_, srvA := newReplica(t, fb, "gw-A")
	_, srvB := newReplica(t, fb, "gw-B")
	cookie := launchOK(t, srvA, testHost, "tk-dead")
	lease := fb.leaseOf(t, "tk-dead")

	if err := fb.RevokeLease(context.Background(), lease.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	resp := proxied(t, srvB, testHost, "/", cookie, nil)
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
	fb.scriptTicket("tk-conc", testWSUID)
	_, srvA := newReplica(t, fb, "gw-A")
	gwB, srvB := newReplica(t, fb, "gw-B")
	cookie := launchOK(t, srvA, testHost, "tk-conc")

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

// rehydrateStatus sends one GET with the session cookie on srv's workspace
// host under ctx and reports the status (-1 on a transport error) plus the
// response body.
func rehydrateStatus(ctx context.Context, srv *httptest.Server, cookie string) (int, string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/", nil)
	if err != nil {
		return -1, ""
	}
	req.Host = testHost
	req.Header.Set("Cookie", gateway.SessionCookieName+"="+cookie)
	resp, err := srv.Client().Transport.(*http.Transport).RoundTrip(req)
	if err != nil {
		return -1, ""
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestRehydrate_FirstRequestCancelledOthersSucceed: the shared lookup is
// detached from the first request — cancelling the request that started it
// must not turn every parked waiter into a 401.
func TestRehydrate_FirstRequestCancelledOthersSucceed(t *testing.T) {
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-cancel", testWSUID)
	_, srvA := newReplica(t, fb, "gw-A")
	gwB, srvB := newReplica(t, fb, "gw-B")
	cookie := launchOK(t, srvA, testHost, "tk-cancel")

	release := fb.gateLookups()

	firstCtx, cancelFirst := context.WithCancel(context.Background())
	defer cancelFirst()
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		rehydrateStatus(firstCtx, srvB, cookie)
	}()
	waitFor(t, "first request inside the lookup", 5*time.Second, func() bool { return fb.lookupCount() == 1 })

	const waiters = 5
	codes := make(chan int, waiters)
	for i := 0; i < waiters; i++ {
		go func() {
			c, _ := rehydrateStatus(context.Background(), srvB, cookie)
			codes <- c
		}()
	}
	waitFor(t, "waiters parked on the shared lookup", 5*time.Second, func() bool { return gwB.InflightWaiters() == waiters })

	cancelFirst() // the client hangs up; the server sees its request context cancelled
	<-firstDone
	time.Sleep(100 * time.Millisecond)
	close(release)

	for i := 0; i < waiters; i++ {
		select {
		case c := <-codes:
			if c != http.StatusOK {
				t.Fatalf("waiter status = %d after the first request was cancelled, want 200", c)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("waiter never answered")
		}
	}
	if got := fb.lookupCount(); got != 1 {
		t.Fatalf("LeaseBySession calls = %d, want 1 (shared lookup)", got)
	}
}

// TestRehydrate_TransientErrorIs503: a directory failure that is not a
// definitive lease death is "unavailable", not "no session".
func TestRehydrate_TransientErrorIs503(t *testing.T) {
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-blip", testWSUID)
	_, srvA := newReplica(t, fb, "gw-A")
	_, srvB := newReplica(t, fb, "gw-B")
	cookie := launchOK(t, srvA, testHost, "tk-blip")

	fb.setLookupErr(errors.New("connection reset by peer"))
	code, body := rehydrateStatus(context.Background(), srvB, cookie)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("status on a directory blip = %d, want 503", code)
	}
	if !strings.Contains(body, `"unavailable"`) {
		t.Fatalf("body = %q, want error unavailable", body)
	}
}

// TestRehydrate_TerminalErrorsAre401: a definitive lease death keeps the
// 401 the cookie always got.
func TestRehydrate_TerminalErrorsAre401(t *testing.T) {
	for _, terr := range []error{broker.ErrLeaseInvalid, broker.ErrRevoked, broker.ErrDenied} {
		fb := newFakeBroker(t)
		fb.scriptTicket("tk-term", testWSUID)
		_, srvA := newReplica(t, fb, "gw-A")
		_, srvB := newReplica(t, fb, "gw-B")
		cookie := launchOK(t, srvA, testHost, "tk-term")

		fb.setLookupErr(terr)
		if code, _ := rehydrateStatus(context.Background(), srvB, cookie); code != http.StatusUnauthorized {
			t.Fatalf("status for %v = %d, want 401", terr, code)
		}
	}
}

// TestRehydrate_TransientErrorNotCached: a failed lookup leaves no state —
// the next request asks the directory again and succeeds.
func TestRehydrate_TransientErrorNotCached(t *testing.T) {
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-nocache", testWSUID)
	_, srvA := newReplica(t, fb, "gw-A")
	gwB, srvB := newReplica(t, fb, "gw-B")
	cookie := launchOK(t, srvA, testHost, "tk-nocache")

	fb.setLookupErr(errors.New("db blip"))
	if code, _ := rehydrateStatus(context.Background(), srvB, cookie); code != http.StatusServiceUnavailable {
		t.Fatalf("first status = %d, want 503", code)
	}
	if s, l, w, i := gwB.SessionMapCounts(); s+l+w+i != 0 {
		t.Fatalf("state after a failed lookup: sessions=%d byLease=%d byWorkspace=%d inflight=%d, want all 0", s, l, w, i)
	}
	fb.setLookupErr(nil)
	if code, _ := rehydrateStatus(context.Background(), srvB, cookie); code != http.StatusOK {
		t.Fatalf("second status = %d, want 200", code)
	}
	if got := fb.lookupCount(); got != 2 {
		t.Fatalf("LeaseBySession calls = %d, want 2 (failure not cached)", got)
	}
}

// TestRehydrate_PanicDoesNotParkWaiters: if the lookup panics in the
// request that owns it, waiters are released (with an error) and the
// in-flight entry is removed.
func TestRehydrate_PanicDoesNotParkWaiters(t *testing.T) {
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-panic", testWSUID)
	_, srvA := newReplica(t, fb, "gw-A")
	gwB, srvB := newReplica(t, fb, "gw-B")
	cookie := launchOK(t, srvA, testHost, "tk-panic")

	release := fb.gateLookups()
	fb.panicNextLookup()

	leader := make(chan int, 1)
	go func() {
		c, _ := rehydrateStatus(context.Background(), srvB, cookie)
		leader <- c
	}()
	waitFor(t, "leader inside the lookup", 5*time.Second, func() bool { return fb.lookupCount() == 1 })

	const waiters = 4
	codes := make(chan int, waiters)
	for i := 0; i < waiters; i++ {
		go func() {
			c, _ := rehydrateStatus(context.Background(), srvB, cookie)
			codes <- c
		}()
	}
	waitFor(t, "waiters parked", 5*time.Second, func() bool { return gwB.InflightWaiters() == waiters })
	close(release) // the leader's lookup now panics

	for i := 0; i < waiters; i++ {
		select {
		case c := <-codes:
			if c != http.StatusServiceUnavailable {
				t.Fatalf("waiter status after a panicked lookup = %d, want 503", c)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("waiter still parked after the lookup panicked")
		}
	}
	<-leader // the connection was dropped by the server's panic recovery
	if _, _, _, i := gwB.SessionMapCounts(); i != 0 {
		t.Fatalf("in-flight entries after panic = %d, want 0", i)
	}
}

// TestRehydrate_NilDirectoryKeepsV01Behaviour: with no session directory a
// replica has only its own sessions — A's cookie is a miss on B.
func TestRehydrate_NilDirectoryKeepsV01Behaviour(t *testing.T) {
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-nil", testWSUID)
	_, srvA := newReplica(t, fb, "gw-A")
	srvB := newGateway(t, fb, nil) // no Sessions: v0.1 single-process mode

	cookie := launchOK(t, srvA, testHost, "tk-nil")
	resp := proxied(t, srvB, testHost, "/", cookie, nil)
	defer drain(resp)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("proxied on directory-less B = %d, want 401", resp.StatusCode)
	}
}

// TestLaunch_BindFailureIssuesNoCookie: when the directory cannot record
// the digest, launch fails closed — the lease is revoked, no cookie is set.
func TestLaunch_BindFailureIssuesNoCookie(t *testing.T) {
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-bindfail", testWSUID)
	fb.setBindErr(errors.New("directory unreachable"))
	_, srv := newReplica(t, fb, "gw-A")

	resp := doLaunch(t, srv, testHost, "tk-bindfail", map[string]string{
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
	fb.scriptTicket("tk-fence", testWSUID)
	_, srvA := newReplica(t, fb, "gw-A")
	_, srvB := newReplica(t, fb, "gw-B")
	cookie := launchOK(t, srvA, testHost, "tk-fence")

	respA := upgrade(t, srvA, testHost, "/websockify", cookie, map[string]string{"Origin": testOrigin})
	if respA.StatusCode != http.StatusSwitchingProtocols {
		drain(respA)
		t.Fatalf("upgrade on A = %d, want 101", respA.StatusCode)
	}
	defer respA.Body.Close()
	wsWrite(t, respA, []byte("x"))
	wsRead(t, respA, 1, 2*time.Second) // prove the stream is live

	respB := upgrade(t, srvB, testHost, "/websockify", cookie, map[string]string{"Origin": testOrigin})
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
	fb.scriptTicket("tk-drain", testWSUID)
	gwA, srvA := newReplica(t, fb, "gw-A")
	_, srvB := newReplica(t, fb, "gw-B")
	cookie := launchOK(t, srvA, testHost, "tk-drain")

	resp := upgrade(t, srvA, testHost, "/websockify", cookie, map[string]string{"Origin": testOrigin})
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

	respB := proxied(t, srvB, testHost, "/", cookie, nil)
	defer drain(respB)
	if respB.StatusCode != http.StatusOK {
		t.Fatalf("proxied on B after A drained = %d, want 200", respB.StatusCode)
	}
}

// TestDrain_RefusesNewUpgrades: once Drain starts the replica sheds — new
// WebSocket upgrades get 503 even with a valid cookie.
func TestDrain_RefusesNewUpgrades(t *testing.T) {
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-drain2", testWSUID)
	gwA, srvA := newReplica(t, fb, "gw-A")
	cookie := launchOK(t, srvA, testHost, "tk-drain2")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	gwA.Drain(ctx)
	cancel()

	resp := upgrade(t, srvA, testHost, "/websockify", cookie, map[string]string{"Origin": testOrigin})
	defer drain(resp)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("upgrade after Drain = %d, want 503", resp.StatusCode)
	}
}

// TestActivity_EventsCarryStreamEpoch: the connected and disconnect reports
// of a stream carry the epoch ClaimStream returned for it, so the broker
// can ignore reports of a stream another epoch fenced.
func TestActivity_EventsCarryStreamEpoch(t *testing.T) {
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-epoch", testWSUID)
	gwA, srvA := newReplica(t, fb, "gw-A")
	cookie := launchOK(t, srvA, testHost, "tk-epoch")

	resp := upgrade(t, srvA, testHost, "/websockify", cookie, map[string]string{"Origin": testOrigin})
	if resp.StatusCode != http.StatusSwitchingProtocols {
		drain(resp)
		t.Fatalf("upgrade = %d, want 101", resp.StatusCode)
	}
	defer resp.Body.Close()
	wsWrite(t, resp, []byte("x"))
	wsRead(t, resp, 1, 2*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	gwA.Drain(ctx)
	cancel()

	var sawConnected, sawDisconnect bool
	for _, ev := range fb.activityEvents() {
		switch ev.Type {
		case broker.ActivityConnected:
			sawConnected = true
		case broker.ActivityDisconnect:
			sawDisconnect = true
		default:
			continue
		}
		if ev.StreamEpoch != 1 {
			t.Fatalf("%s report carries StreamEpoch %d, want 1 (the epoch ClaimStream returned)", ev.Type, ev.StreamEpoch)
		}
	}
	if !sawConnected || !sawDisconnect {
		t.Fatalf("connected=%v disconnect=%v, want both reported", sawConnected, sawDisconnect)
	}
}

// TestDrain_DisconnectReportedBeforeReturn: Drain's "quiet" verdict must
// cover the window between the sender dequeuing a disconnect and its broker
// call starting — after Drain returns, the broker has the disconnect of
// every drained stream. Repeated under -race to catch the window.
func TestDrain_DisconnectReportedBeforeReturn(t *testing.T) {
	for i := 0; i < 200; i++ {
		t.Run("", func(t *testing.T) {
			fb := newFakeBroker(t)
			fb.scriptTicket("tk-dr", testWSUID)
			gwA, srvA := newReplica(t, fb, "gw-A")
			cookie := launchOK(t, srvA, testHost, "tk-dr")
			resp := upgrade(t, srvA, testHost, "/websockify", cookie, map[string]string{"Origin": testOrigin})
			if resp.StatusCode != http.StatusSwitchingProtocols {
				drain(resp)
				t.Fatalf("upgrade = %d, want 101", resp.StatusCode)
			}
			defer resp.Body.Close()
			wsWrite(t, resp, []byte("x"))
			wsRead(t, resp, 1, 2*time.Second)

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			gwA.Drain(ctx)
			cancel()

			var disconnects int
			for _, ty := range fb.activityTypes() {
				if ty == broker.ActivityDisconnect {
					disconnects++
				}
			}
			if disconnects != 1 {
				t.Fatalf("disconnects at the broker when Drain returned = %d, want 1", disconnects)
			}
		})
	}
}
