// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package gateway_test

// Idle lease release: an attached session with nothing in flight for
// IdleSessionRelease stops being served. The replica that claimed the
// lease's current stream epoch asks the broker to end the lease; every
// other replica only stops renewing, so the lease lapses at its TTL unless
// a replica still serving a stream renews it.

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/tinyorbitvn/tinycdi/internal/broker"
	"github.com/tinyorbitvn/tinycdi/internal/gateway"
	"github.com/tinyorbitvn/tinycdi/internal/observability"
)

const idleG = gateway.DefaultIdleSessionRelease

// newIdleReplica builds one replica on the shared fake directory with the
// idle release on, the given fake clock and its own metric registry.
func newIdleReplica(t *testing.T, fb *fakeBroker, id string, clock *fakeClock, extra ...func(*gateway.Config)) (*gateway.Gateway, *httptest.Server, *prometheus.Registry) {
	t.Helper()
	reg := prometheus.NewRegistry()
	m := observability.NewMetrics(reg, nil)
	gw, srv := newGatewayHandle(t, fb, func(c *gateway.Config) {
		c.Identity = broker.GatewayIdentity{ID: id, Audience: testDomain}
		c.Sessions = fb
		c.Now = clock.Now
		// Only the idle release may end a session here: the fail-closed
		// window sits far above every clock advance.
		c.RevokeDeadline = 24 * time.Hour
		c.IdleSessionRelease = idleG
		c.Metrics = m
		for _, f := range extra {
			f(c)
		}
	})
	return gw, srv, reg
}

// streamOn opens the session websocket on srv for host and requires 101.
func streamOn(t *testing.T, srv *httptest.Server, host, cookie string) *http.Response {
	t.Helper()
	up := upgrade(t, srv, host, "/websockify", cookie, map[string]string{"Origin": "https://" + host})
	if up.StatusCode != http.StatusSwitchingProtocols {
		drain(up)
		t.Fatalf("upgrade = %d, want 101", up.StatusCode)
	}
	return up
}

// waitQuiet waits until gw's session for wsUID has nothing in flight —
// e.g. a closed stream's proxy request has unwound.
func waitQuiet(t *testing.T, gw *gateway.Gateway, wsUID string) {
	t.Helper()
	waitFor(t, "session quiet", 5*time.Second, func() bool {
		busy, ok := gw.SessionBusy(wsUID)
		return ok && !busy
	})
}

// sessionMapped reports whether gw still serves any session.
func sessionMapped(gw *gateway.Gateway) bool {
	s, _, _, _ := gw.SessionMapCounts()
	return s > 0
}

func idleResult(t *testing.T, reg *prometheus.Registry, result string) float64 {
	t.Helper()
	return metricValue(t, reg, "tinycdi_gateway_idle_release_total", map[string]string{"result": result})
}

// TestIdleRelease_ReleasesAfterLastStreamCloses: the stream owner keeps the
// lease just inside the window and releases it just past it; the cookie is
// dead afterwards.
func TestIdleRelease_ReleasesAfterLastStreamCloses(t *testing.T) {
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-idle", testWSUID)
	clock := &fakeClock{now: time.Now()}
	gw, srv, reg := newIdleReplica(t, fb, "gw-A", clock)
	cookie := launchOK(t, srv, testHost, "tk-idle")
	lease := fb.leaseOf(t, "tk-idle")

	up := streamOn(t, srv, testHost, cookie)
	up.Body.Close()
	waitQuiet(t, gw, testWSUID)

	clock.Advance(idleG - time.Second)
	time.Sleep(4 * testRenewInterval)
	if calls := fb.releaseCalls(lease.ID); len(calls) != 0 {
		t.Fatalf("released inside the window: %v", calls)
	}
	if !sessionMapped(gw) {
		t.Fatal("session dropped inside the idle window")
	}

	clock.Advance(2 * time.Second)
	waitFor(t, "idle session released", 5*time.Second, func() bool { return !sessionMapped(gw) })
	if calls := fb.releaseCalls(lease.ID); len(calls) != 1 || calls[0] != 1 {
		t.Fatalf("release calls = %v, want exactly one at epoch 1", calls)
	}
	if got := idleResult(t, reg, "released"); got != 1 {
		t.Fatalf("idle_release{released} = %v, want 1", got)
	}
	resp := proxied(t, srv, testHost, "/", cookie, nil)
	defer drain(resp)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("cookie after release = %d, want 401", resp.StatusCode)
	}
}

// TestIdleRelease_ReconnectInsideWindowKeepsLease: a KasmVNC reconnect or
// frame reload inside the window resumes on the same lease, and an open
// stream is never idle however long it runs.
func TestIdleRelease_ReconnectInsideWindowKeepsLease(t *testing.T) {
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-recon", testWSUID)
	clock := &fakeClock{now: time.Now()}
	gw, srv, _ := newIdleReplica(t, fb, "gw-A", clock)
	cookie := launchOK(t, srv, testHost, "tk-recon")
	lease := fb.leaseOf(t, "tk-recon")

	up := streamOn(t, srv, testHost, cookie)
	up.Body.Close()
	waitQuiet(t, gw, testWSUID)
	clock.Advance(idleG - 5*time.Second)
	up2 := streamOn(t, srv, testHost, cookie)
	defer up2.Body.Close()

	clock.Advance(2 * idleG)
	time.Sleep(4 * testRenewInterval)
	if calls := fb.releaseCalls(lease.ID); len(calls) != 0 {
		t.Fatalf("released with a live stream: %v", calls)
	}
	wsWrite(t, up2, []byte("x"))
	if got := wsRead(t, up2, 1, 2*time.Second); len(got) != 1 {
		t.Fatalf("reconnected stream echoed %d bytes, want 1", len(got))
	}
}

// TestIdleRelease_InFlightRequestBlocksRelease: a plain request still in
// flight pins the session; once it ends the window starts over. A replica
// that never claimed only lapses — no broker release call.
func TestIdleRelease_InFlightRequestBlocksRelease(t *testing.T) {
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-slow", testWSUID)
	hold := make(chan struct{})
	slow := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/app/slow.js" {
			<-hold
		}
		w.WriteHeader(http.StatusOK)
	}))
	var once sync.Once
	unblock := func() { once.Do(func() { close(hold) }) }
	t.Cleanup(slow.Close)
	t.Cleanup(unblock) // runs first (LIFO): a failed test must not hang slow.Close
	fb.pointLeaseAt(t, "tk-slow", slow)
	clock := &fakeClock{now: time.Now()}
	gw, srv, reg := newIdleReplica(t, fb, "gw-A", clock)
	cookie := launchOK(t, srv, testHost, "tk-slow")
	lease := fb.leaseOf(t, "tk-slow")
	drain(proxied(t, srv, testHost, "/", cookie, nil))

	done := make(chan int, 1)
	go func() {
		code, _ := func() (int, error) {
			req, err := http.NewRequest(http.MethodGet, srv.URL+"/app/slow.js", nil)
			if err != nil {
				return -1, err
			}
			req.Host = testHost
			req.Header.Set("Cookie", gateway.SessionCookieName+"="+cookie)
			resp, err := http.DefaultTransport.RoundTrip(req)
			if err != nil {
				return -1, err
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			return resp.StatusCode, nil
		}()
		done <- code
	}()
	waitFor(t, "slow request in flight", 5*time.Second, func() bool {
		busy, ok := gw.SessionBusy(testWSUID)
		return ok && busy
	})
	clock.Advance(2 * idleG)
	time.Sleep(4 * testRenewInterval)
	if !sessionMapped(gw) {
		t.Fatal("session released while a request was in flight")
	}

	unblock()
	if code := <-done; code != http.StatusOK {
		t.Fatalf("slow request = %d, want 200", code)
	}
	waitQuiet(t, gw, testWSUID)
	clock.Advance(idleG + time.Second)
	waitFor(t, "session dropped after the request ended", 5*time.Second, func() bool { return !sessionMapped(gw) })
	if calls := fb.releaseCalls(lease.ID); len(calls) != 0 {
		t.Fatalf("a replica that never claimed called release: %v", calls)
	}
	if got := idleResult(t, reg, "lapsed"); got != 1 {
		t.Fatalf("idle_release{lapsed} = %v, want 1", got)
	}
}

// TestIdleRelease_TransferInFlightBlocksRelease (Review Focus 2): an upload
// still running after its tab closed holds the lease until it ends.
func TestIdleRelease_TransferInFlightBlocksRelease(t *testing.T) {
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-xf-idle", testWSUID)
	stub := newXferStub(t)
	lease := fb.leaseOf(t, "tk-xf-idle")
	tgt := targetFor(stub.srv)
	tgt.XferToken = "xt"
	fb.mu.Lock()
	fb.resolveT[lease.ID] = tgt
	fb.transfers[xfID] = broker.TransferGrant{ID: xfID, WorkspaceUID: testWSUID, Direction: "upload", Path: "big.bin", MaxBytes: 100}
	fb.mu.Unlock()
	clock := &fakeClock{now: time.Now()}
	gw, srv, reg := newIdleReplica(t, fb, "gw-A", clock, func(c *gateway.Config) {
		c.PortalOrigins = []string{portalOrigin}
	})
	cookie := launchOK(t, srv, testHost, "tk-xf-idle")
	drain(proxied(t, srv, testHost, "/", cookie, nil)) // attach

	pr, pw := io.Pipe()
	// A failed test must not leave the upload parked (srv.Close would wait).
	t.Cleanup(func() { _ = pw.CloseWithError(io.ErrClosedPipe) })
	req, err := http.NewRequest(http.MethodPut, srv.URL+xfPath, pr)
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = 5
	req.Host = testHost
	req.Header.Set("Cookie", gateway.SessionCookieName+"="+cookie)
	req.Header.Set("Origin", portalOrigin)
	done := make(chan int, 1)
	go func() {
		res, err := srv.Client().Transport.(*http.Transport).RoundTrip(req)
		if err != nil {
			done <- -1
			return
		}
		drain(res)
		done <- res.StatusCode
	}()
	if _, err := pw.Write([]byte("he")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "transfer in flight", 5*time.Second, func() bool {
		busy, ok := gw.SessionBusy(testWSUID)
		return ok && busy
	})
	clock.Advance(2 * idleG)
	time.Sleep(4 * testRenewInterval)
	if !sessionMapped(gw) {
		t.Fatal("session released while a file transfer was in flight")
	}

	if _, err := pw.Write([]byte("llo")); err != nil {
		t.Fatal(err)
	}
	pw.Close()
	select {
	case code := <-done:
		if code != http.StatusOK {
			t.Fatalf("upload = %d, want 200", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("upload never answered")
	}
	if v, _ := stub.file("big.bin"); v != "hello" {
		t.Fatalf("uploaded %q, want hello", v)
	}
	waitQuiet(t, gw, testWSUID)
	clock.Advance(idleG + time.Second)
	waitFor(t, "session dropped after the transfer", 5*time.Second, func() bool { return !sessionMapped(gw) })
	if got := idleResult(t, reg, "lapsed"); got != 1 {
		t.Fatalf("idle_release{lapsed} = %v, want 1", got)
	}
}

// TestIdleRelease_NeverClaimedCopyLapses (Review Focus 3): replica A holds
// an attached copy that never claimed while B streams. A only lapses; the
// lease is never released or revoked and B's stream keeps working.
func TestIdleRelease_NeverClaimedCopyLapses(t *testing.T) {
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-copy", testWSUID)
	clock := &fakeClock{now: time.Now()}
	gwA, srvA, regA := newIdleReplica(t, fb, "gw-A", clock)
	_, srvB, _ := newIdleReplica(t, fb, "gw-B", clock)
	cookie := launchOK(t, srvA, testHost, "tk-copy")
	lease := fb.leaseOf(t, "tk-copy")
	drain(proxied(t, srvA, testHost, "/", cookie, nil))
	up := streamOn(t, srvB, testHost, cookie)
	defer up.Body.Close()

	clock.Advance(idleG + time.Second)
	waitFor(t, "A drops its idle copy", 5*time.Second, func() bool { return !sessionMapped(gwA) })
	if calls := fb.releaseCalls(lease.ID); len(calls) != 0 {
		t.Fatalf("never-claimed copy called release: %v", calls)
	}
	if n := fb.leaseRevokeCount(lease.ID); n != 0 {
		t.Fatalf("lease revoked %d times, want 0", n)
	}
	if got := idleResult(t, regA, "lapsed"); got != 1 {
		t.Fatalf("A idle_release{lapsed} = %v, want 1", got)
	}
	wsWrite(t, up, []byte("x"))
	if got := wsRead(t, up, 1, 2*time.Second); len(got) != 1 {
		t.Fatalf("B's stream echoed %d bytes after A lapsed, want 1", len(got))
	}
}

// TestIdleRelease_FencedFormerOwnerLapses (Review Focus 1): the stream moved
// from A (epoch 1) to B (epoch 2). A's idle copy must not release the lease
// B now serves.
func TestIdleRelease_FencedFormerOwnerLapses(t *testing.T) {
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-moved", testWSUID)
	clock := &fakeClock{now: time.Now()}
	gwA, srvA, regA := newIdleReplica(t, fb, "gw-A", clock)
	_, srvB, _ := newIdleReplica(t, fb, "gw-B", clock)
	cookie := launchOK(t, srvA, testHost, "tk-moved")
	lease := fb.leaseOf(t, "tk-moved")

	upA := streamOn(t, srvA, testHost, cookie)
	defer upA.Body.Close()
	upB := streamOn(t, srvB, testHost, cookie)
	defer upB.Body.Close()
	waitQuiet(t, gwA, testWSUID) // A's renew saw epoch 2 and fenced its stream

	clock.Advance(idleG + time.Second)
	waitFor(t, "A drops its fenced copy", 5*time.Second, func() bool { return !sessionMapped(gwA) })
	if calls := fb.releaseCalls(lease.ID); len(calls) != 0 {
		t.Fatalf("fenced former owner called release: %v", calls)
	}
	if got := idleResult(t, regA, "lapsed"); got != 1 {
		t.Fatalf("A idle_release{lapsed} = %v, want 1", got)
	}
	wsWrite(t, upB, []byte("x"))
	if got := wsRead(t, upB, 1, 2*time.Second); len(got) != 1 {
		t.Fatalf("B's stream echoed %d bytes, want 1", len(got))
	}
}

// TestIdleRelease_BrokerRefusalOrErrorStillStopsRenewing (Review Focus 4):
// whatever the broker answers, the idle session dies locally and stops
// renewing — the lease lapses at its TTL, no retry loop.
func TestIdleRelease_BrokerRefusalOrErrorStillStopsRenewing(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*fakeBroker)
	}{
		{"refused", func(f *fakeBroker) { f.releaseRefuse = true }},
		{"error", func(f *fakeBroker) { f.releaseErr = errors.New("broker down") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fb := newFakeBroker(t)
			fb.scriptTicket("tk-bad", testWSUID)
			tc.setup(fb)
			clock := &fakeClock{now: time.Now()}
			gw, srv, reg := newIdleReplica(t, fb, "gw-A", clock)
			cookie := launchOK(t, srv, testHost, "tk-bad")
			lease := fb.leaseOf(t, "tk-bad")
			up := streamOn(t, srv, testHost, cookie)
			up.Body.Close()
			waitQuiet(t, gw, testWSUID)

			clock.Advance(idleG + time.Second)
			waitFor(t, "session dropped", 5*time.Second, func() bool { return !sessionMapped(gw) })
			if calls := fb.releaseCalls(lease.ID); len(calls) != 1 {
				t.Fatalf("release calls = %v, want exactly 1", calls)
			}
			if got := idleResult(t, reg, tc.name); got != 1 {
				t.Fatalf("idle_release{%s} = %v, want 1", tc.name, got)
			}
			renews := fb.renewCount("gw-A")
			time.Sleep(4 * testRenewInterval)
			if got := fb.renewCount("gw-A"); got != renews {
				t.Fatalf("renews continued after the idle drop: %d -> %d", renews, got)
			}
		})
	}
}

// TestIdleRelease_RefusedRequestRestartsWindow (Review Focus 5): a request
// refused before tracking (here an upgrade with no tab id, 400) still proves
// a live client and restarts the idle window.
func TestIdleRelease_RefusedRequestRestartsWindow(t *testing.T) {
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-retry", testWSUID)
	clock := &fakeClock{now: time.Now()}
	gw, srv, _ := newIdleReplica(t, fb, "gw-A", clock)
	cookie := launchOK(t, srv, testHost, "tk-retry")
	drain(proxied(t, srv, testHost, "/", cookie, nil))
	waitQuiet(t, gw, testWSUID)

	clock.Advance(idleG - 5*time.Second)
	bad := upgradeExact(t, srv, testHost, "/websockify", cookie, map[string]string{"Origin": testOrigin})
	drain(bad)
	if bad.StatusCode != http.StatusBadRequest {
		t.Fatalf("upgrade without tab id = %d, want 400", bad.StatusCode)
	}
	clock.Advance(10 * time.Second) // 65 s after the first request, 10 s after the refused one
	time.Sleep(4 * testRenewInterval)
	if !sessionMapped(gw) {
		t.Fatal("a refused request did not restart the idle window")
	}
	clock.Advance(idleG)
	waitFor(t, "session dropped a full window after the refused request", 5*time.Second, func() bool { return !sessionMapped(gw) })
}

// TestIdleRelease_DisabledKeepsAttachedSession: IdleSessionRelease 0 is
// today's behaviour — an attached, quiet session renews indefinitely.
func TestIdleRelease_DisabledKeepsAttachedSession(t *testing.T) {
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-off", testWSUID)
	clock := &fakeClock{now: time.Now()}
	gw, srv, _ := newIdleReplica(t, fb, "gw-A", clock, func(c *gateway.Config) {
		c.IdleSessionRelease = 0
	})
	cookie := launchOK(t, srv, testHost, "tk-off")
	lease := fb.leaseOf(t, "tk-off")
	up := streamOn(t, srv, testHost, cookie)
	up.Body.Close()
	waitQuiet(t, gw, testWSUID)

	clock.Advance(10 * idleG)
	time.Sleep(4 * testRenewInterval)
	if !sessionMapped(gw) {
		t.Fatal("disabled idle release still dropped the session")
	}
	if calls := fb.releaseCalls(lease.ID); len(calls) != 0 {
		t.Fatalf("disabled idle release called the broker: %v", calls)
	}
}

// TestIdleRelease_GhostReapUnchanged: with the idle release on, a session
// that never attached is still reaped by the ghost path (lease revoke),
// never by the idle release.
func TestIdleRelease_GhostReapUnchanged(t *testing.T) {
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-ghost-idle", testWSUID)
	clock := &fakeClock{now: time.Now()}
	gw, srv, reg := newIdleReplica(t, fb, "gw-A", clock)
	launchOK(t, srv, testHost, "tk-ghost-idle")
	lease := fb.leaseOf(t, "tk-ghost-idle")

	clock.Advance(2 * time.Minute)
	waitFor(t, "ghost lease revoked", 5*time.Second, func() bool { return fb.leaseRevokeCount(lease.ID) == 1 })
	waitFor(t, "ghost unmapped", 5*time.Second, func() bool { return !sessionMapped(gw) })
	if calls := fb.releaseCalls(lease.ID); len(calls) != 0 {
		t.Fatalf("ghost went through the idle release: %v", calls)
	}
	for _, r := range []string{"released", "refused", "lapsed", "error"} {
		if got := idleResult(t, reg, r); got != 0 {
			t.Fatalf("idle_release{%s} = %v for a ghost, want 0", r, got)
		}
	}
}

// TestIdleRelease_AttachDuringReleaseLoses: a request arriving while the
// release call is in flight (decision committed under s.mu) must lose.
func TestIdleRelease_AttachDuringReleaseLoses(t *testing.T) {
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-rel-lose", testWSUID)
	clock := &fakeClock{now: time.Now()}
	gw, srv, _ := newIdleReplica(t, fb, "gw-A", clock)
	cookie := launchOK(t, srv, testHost, "tk-rel-lose")
	lease := fb.leaseOf(t, "tk-rel-lose")
	up := streamOn(t, srv, testHost, cookie)
	up.Body.Close()
	waitQuiet(t, gw, testWSUID)

	entered, gate := fb.gateReleases()
	clock.Advance(idleG + time.Second)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("no release call within 5s")
	}
	done := make(chan int, 1)
	go func() {
		code, _ := raceRequest(t, srv.URL, testHost, cookie)
		done <- code
	}()
	select {
	case c := <-done:
		t.Fatalf("request completed with %d while the release held the session lock", c)
	case <-time.After(150 * time.Millisecond):
	}
	close(gate)
	select {
	case c := <-done:
		if c != http.StatusUnauthorized {
			t.Fatalf("request after a committed release = %d, want 401", c)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("request never answered after the gate closed")
	}
	if calls := fb.releaseCalls(lease.ID); len(calls) != 1 {
		t.Fatalf("release calls = %v, want 1", calls)
	}
}

// TestIdleRelease_AttachReleaseRaceConsistency: requests racing the first
// post-window tick. A 200 means the release aborted (session alive, no
// release call); a 401 means it won (session gone, one release call).
func TestIdleRelease_AttachReleaseRaceConsistency(t *testing.T) {
	const iters = 40
	var won, released int
	for i := 0; i < iters; i++ {
		wsUID := fmt.Sprintf("ws_%08x", 0x500+i)
		host := fmt.Sprintf("ws-%08x.%s", 0x500+i, testDomain)
		ticket := fmt.Sprintf("tk-idle-race-%d", i)

		fb := newFakeBroker(t)
		fb.scriptTicket(ticket, wsUID)
		clock := &fakeClock{now: time.Now()}
		gw, srv, _ := newIdleReplica(t, fb, "gw-A", clock)
		cookie := launchOK(t, srv, host, ticket)
		lease := fb.leaseOf(t, ticket)
		up := streamOn(t, srv, host, cookie)
		up.Body.Close()
		waitQuiet(t, gw, wsUID)

		clock.Advance(idleG + time.Second)
		time.Sleep(time.Duration(i%4) * 12 * time.Millisecond)
		code, _ := raceRequest(t, srv.URL, host, cookie)
		time.Sleep(3 * testRenewInterval)

		alive := sessionMapped(gw)
		calls := len(fb.releaseCalls(lease.ID))
		switch code {
		case http.StatusOK:
			if !alive || calls != 0 {
				t.Fatalf("iter %d: request won (200) but alive=%v releaseCalls=%d", i, alive, calls)
			}
			won++
		case http.StatusUnauthorized:
			if alive || calls != 1 {
				t.Fatalf("iter %d: request lost (401) but alive=%v releaseCalls=%d", i, alive, calls)
			}
			released++
		default:
			t.Fatalf("iter %d: request = %d, want 200 or 401", i, code)
		}
		gw.Close()
		srv.Close()
	}
	if won == 0 || released == 0 {
		t.Fatalf("race never exercised both orders: request-won=%d release-won=%d", won, released)
	}
	t.Logf("request-won=%d release-won=%d", won, released)
}
