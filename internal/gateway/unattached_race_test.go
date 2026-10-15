// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package gateway_test

// Attach/reap serialization: the ghost reap's decision runs under the
// session lock together with the lease revoke, so an attach can only
// commit before the decision (the reap aborts, the session lives) or
// after it (the request loses to a session already committed to die).
// There is no window where an attach that already won still gets
// revoked.

import (
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/gateway"
)

// raceRequest performs the attach request without the proxied helper's
// t.Fatalf on transport errors — a request that loses the race may see
// the session die mid-flight.
func raceRequest(t *testing.T, srvURL, host, cookie string) (int, error) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srvURL+"/", nil)
	if err != nil {
		t.Fatalf("build attach request: %v", err)
	}
	req.Host = host
	req.Header.Set("Cookie", gateway.SessionCookieName+"="+cookie)
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		return -1, err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode, nil
}

// TestLaunch_AttachDuringReapLoses: an attach request that arrives while
// the reap's lease revoke is in flight (the decision already committed
// under s.mu) MUST lose — it cannot commit attached state underneath the
// reap and ride out with a 200 on a revoked lease. With the revoke call
// gated at the fake broker this ordering is deterministic: the request
// stays parked on s.mu until the gate closes, then answers 401.
func TestLaunch_AttachDuringReapLoses(t *testing.T) {
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-att-lose", testWSUID)
	clock := &fakeClock{now: time.Now()}
	_, srv := newGatewayHandle(t, fb, func(c *gateway.Config) {
		c.Now = clock.Now
		c.RevokeDeadline = time.Hour
	})
	cookie := launchOK(t, srv, testHost, "tk-att-lose")
	lease := fb.leaseOf(t, "tk-att-lose")

	entered, gate := fb.gateRevokes()
	clock.Advance(2 * time.Minute)
	// The renew tick decides the reap and enters the (blocked) revoke.
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("no revoke call within 5s — the ghost was never reaped")
	}

	done := make(chan int, 1)
	go func() {
		code, _ := raceRequest(t, srv.URL, testHost, cookie)
		done <- code
	}()
	// The attach must NOT complete while the revoke holds the session
	// lock — the decision is already committed and the request can only
	// lose.
	select {
	case c := <-done:
		t.Fatalf("attach completed with %d while the reap's revoke was still in flight — the decision is not serialized", c)
	case <-time.After(150 * time.Millisecond):
	}
	close(gate)
	select {
	case c := <-done:
		if c != http.StatusUnauthorized {
			t.Fatalf("attach after a committed reap = %d, want 401", c)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("attach request never answered after the gate closed")
	}
	if n := fb.leaseRevokeCount(lease.ID); n != 1 {
		t.Fatalf("RevokeLease calls = %d, want 1", n)
	}
}

// TestLaunch_AttachReapRaceConsistency: attach requests fired
// concurrently with the first post-TTL renew tick, many iterations. The
// end state must always be consistent: a session that survived never had
// its lease revoked, and an attach that got 200 proves the reap aborted
// (session alive, lease live); a 401 means the reap won and the lease is
// gone. Run under -race.
func TestLaunch_AttachReapRaceConsistency(t *testing.T) {
	const iters = 60
	var won, reaped int
	for i := 0; i < iters; i++ {
		wsUID := fmt.Sprintf("ws_%08x", 0x300+i)
		host := fmt.Sprintf("ws-%08x.%s", 0x300+i, testDomain)
		ticket := fmt.Sprintf("tk-race-%d", i)

		fb := newFakeBroker(t)
		fb.scriptTicket(ticket, wsUID)
		clock := &fakeClock{now: time.Now()}
		gw, srv := newGatewayHandle(t, fb, func(c *gateway.Config) {
			c.Now = clock.Now
			c.RevokeDeadline = 24 * time.Hour // only the ghost TTL may kill here
		})
		cookie := launchOK(t, srv, host, ticket)
		lease := fb.leaseOf(t, ticket)

		// Jump past the TTL, then race the attach against the pending
		// tick: a rotating stagger lands some attaches before the reap
		// decision and some after it.
		clock.Advance(2 * time.Minute)
		time.Sleep(time.Duration(i%4) * 12 * time.Millisecond)
		done := make(chan int, 1)
		go func() {
			code, _ := raceRequest(t, srv.URL, host, cookie)
			done <- code
		}()
		var code int
		select {
		case code = <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("iter %d: attach request never answered", i)
		}
		// Let the deciding tick land whichever way the race went.
		time.Sleep(3 * testRenewInterval)

		s, _, _, _ := gw.SessionMapCounts()
		alive := s > 0
		revoked := fb.leaseRevokeCount(lease.ID) > 0
		switch {
		case code == http.StatusOK:
			if !alive || revoked {
				t.Fatalf("iter %d: attach won (200) but alive=%v revoked=%v — an attach that already won was revoked", i, alive, revoked)
			}
			won++
		case code == http.StatusUnauthorized:
			if alive || !revoked {
				t.Fatalf("iter %d: attach lost (401) but alive=%v revoked=%v — ghost reap did not run to completion", i, alive, revoked)
			}
			reaped++
		default:
			t.Fatalf("iter %d: attach = %d, want 200 or 401", i, code)
		}
		gw.Close()
		srv.Close()
	}
	if won == 0 || reaped == 0 {
		t.Fatalf("race never exercised both orders: attach-won=%d reap-won=%d", won, reaped)
	}
	t.Logf("attach-won=%d reap-won=%d", won, reaped)
}
