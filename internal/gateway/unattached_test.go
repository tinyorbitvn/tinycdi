// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package gateway_test

// Ghost-session TTL: a redeemed launch whose 303 never reaches a browser
// leaves a session no client holds renewing the lease — and pinning the
// workspace: a live lease makes IssueTicket answer 409 CONNECTION_IN_USE.
// Sessions that never admitted a request are reaped at the unattached
// TTL by the renew loop (the session's existing expiry check): the lease
// is revoked — dropping the pin promptly — unless the lease's stream
// epoch proves a sibling replica is already serving it.

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/broker"
	"github.com/tinyorbitvn/tinycdi/internal/gateway"
)

// TestLaunch_UnclaimedSessionReaped: redeem-then-abandon — the launch
// 303 is never followed. Past the unattached TTL the lease must be
// revoked at the broker (the workspace pin released) and the same cookie
// must resolve to a dead lease, not resurrect the ghost. Fails on
// v0.5.0: the ghost is dropped by the renew deadline WITHOUT a revoke —
// the lease stays live and the cookie rehydrates a fresh session.
func TestLaunch_UnclaimedSessionReaped(t *testing.T) {
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-ghost", testWSUID)
	clock := &fakeClock{now: time.Now()}
	gw, srv := newGatewayHandle(t, fb, func(c *gateway.Config) {
		c.Now = clock.Now
		// Pin the fail-closed window well above the advance so only the
		// ghost TTL — not a stale last-renew clock — can kill the session.
		c.RevokeDeadline = time.Hour
	})
	cookie := launchOK(t, srv, testHost, "tk-ghost")
	lease := fb.leaseOf(t, "tk-ghost")

	// The client abandons the launch here — no request ever carries the
	// cookie. Jump past the unattached TTL; a renew tick reaps the ghost.
	clock.Advance(2 * time.Minute)
	waitFor(t, "ghost lease revoked", 5*time.Second, func() bool {
		return fb.leaseRevokeCount(lease.ID) == 1
	})
	waitFor(t, "ghost session unmapped", 5*time.Second, func() bool {
		s, l, w, _ := gw.SessionMapCounts()
		return s+l+w == 0
	})

	// The pin is gone for good: the same cookie resolves to a dead lease
	// and must allocate nothing on its way out.
	mints := gateway.SessionMints()
	resp := proxied(t, srv, testHost, "/", cookie, nil)
	defer drain(resp)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("reaped ghost cookie = %d, want 401 — the lease must be dead, not rehydratable", resp.StatusCode)
	}
	if got := gateway.SessionMints() - mints; got != 0 {
		t.Fatalf("dead ghost cookie minted %d sessions, want 0", got)
	}
}

// TestLaunch_AttachedSessionSurvivesTTL: a session that admitted a
// request is a real client — the ghost TTL must never reap it.
func TestLaunch_AttachedSessionSurvivesTTL(t *testing.T) {
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-live", testWSUID)
	clock := &fakeClock{now: time.Now()}
	srv := newGateway(t, fb, func(c *gateway.Config) {
		c.Now = clock.Now
		c.RevokeDeadline = time.Hour
	})
	cookie := launchOK(t, srv, testHost, "tk-live")
	lease := fb.leaseOf(t, "tk-live")

	resp := proxied(t, srv, testHost, "/", cookie, nil)
	drain(resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first post-launch request = %d, want 200", resp.StatusCode)
	}

	// Well past the TTL with real renew ticks: the session keeps its
	// lease and keeps serving.
	clock.Advance(2 * time.Minute)
	time.Sleep(4 * testRenewInterval)
	if n := fb.leaseRevokeCount(lease.ID); n != 0 {
		t.Fatalf("attached session lease revoked %d times — the ghost TTL must not touch it", n)
	}
	resp = proxied(t, srv, testHost, "/", cookie, nil)
	defer drain(resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("attached session past the TTL = %d, want 200", resp.StatusCode)
	}
}

// TestLaunch_GhostReapSkipsUsedLease: the launch landed on replica A but
// the client's first contact landed on replica B — B's stream claim
// bumped the lease's stream epoch, which A learns on its next renew.
// A's ghost reap then kills ONLY its local copy: the lease a sibling
// replica serves is never revoked out from under it.
func TestLaunch_GhostReapSkipsUsedLease(t *testing.T) {
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-xrep", testWSUID)
	clock := &fakeClock{now: time.Now()}
	gwA, srvA := newGatewayHandle(t, fb, func(c *gateway.Config) {
		c.Identity = broker.GatewayIdentity{ID: "gw-A", Audience: testDomain}
		c.Sessions = fb
		c.Now = clock.Now
		c.RevokeDeadline = time.Hour
	})
	_, srvB := newGatewayHandle(t, fb, func(c *gateway.Config) {
		c.Identity = broker.GatewayIdentity{ID: "gw-B", Audience: testDomain}
		c.Sessions = fb
		c.Now = clock.Now
		c.RevokeDeadline = time.Hour
	})
	cookie := launchOK(t, srvA, testHost, "tk-xrep")
	lease := fb.leaseOf(t, "tk-xrep")

	// B admits the stream: the lease's epoch proves cross-replica use.
	up := upgrade(t, srvB, testHost, "/websockify", cookie, map[string]string{"Origin": testOrigin})
	if up.StatusCode != http.StatusSwitchingProtocols {
		drain(up)
		t.Fatalf("upgrade on B = %d, want 101", up.StatusCode)
	}
	defer up.Body.Close()
	if e := fb.epochOf(lease.ID); e == 0 {
		t.Fatal("stream claim did not bump the lease epoch")
	}

	// Let A's renew observe the bumped epoch, then lapse the ghost TTL.
	time.Sleep(2 * testRenewInterval)
	clock.Advance(2 * time.Minute)
	waitFor(t, "A reaps its ghost copy", 5*time.Second, func() bool {
		s, l, w, _ := gwA.SessionMapCounts()
		return s+l+w == 0
	})
	if n := fb.leaseRevokeCount(lease.ID); n != 0 {
		t.Fatalf("lease in use on another replica revoked %d times — the reap must stay local", n)
	}

	// B's stream is untouched by A's reap.
	wsWrite(t, up, []byte("x"))
	if got := wsRead(t, up, 1, 2*time.Second); len(got) != 1 {
		t.Fatalf("B's stream echoed %d bytes after A's reap, want 1", len(got))
	}
}

// TestLaunch_ReapOnRevokeFailureStillFrees: when the broker revoke fails
// the ghost still dies — its lease lapses with renewal stopped instead
// of pinning the workspace on a broker blip.
func TestLaunch_ReapOnRevokeFailureStillFrees(t *testing.T) {
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-rf", testWSUID)
	clock := &fakeClock{now: time.Now()}
	gw, srv := newGatewayHandle(t, fb, func(c *gateway.Config) {
		c.Now = clock.Now
	})
	cookie := launchOK(t, srv, testHost, "tk-rf")

	fb.revokeErr = errors.New("broker down")
	clock.Advance(2 * time.Minute)
	waitFor(t, "ghost session reaped despite revoke failure", 5*time.Second, func() bool {
		s, l, w, _ := gw.SessionMapCounts()
		return s+l+w == 0
	})

	// The cookie is dead locally even though the broker-side lease row
	// may linger until its own TTL.
	resp := proxied(t, srv, testHost, "/", cookie, nil)
	defer drain(resp)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("cookie after local reap = %d, want 401", resp.StatusCode)
	}
}
