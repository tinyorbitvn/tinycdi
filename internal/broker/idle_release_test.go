// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package broker_test

// ReleaseIdleLease: the gateway replica that owns a lease's current stream
// epoch ends the lease once its last stream closed — conditionally, under
// the lease row lock, so a claim from any replica since, a stream still
// counted open, a foreign gateway or a stale fence keeps the lease alive.

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/tinyorbitvn/tinycdi/internal/broker"
	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
)

// idleLease issues, redeems and claims one stream on a fresh lease for
// wsUID, then reports that stream connected and disconnected — the state a
// closed session tab leaves behind. It returns the lease and the claimed
// epoch.
func idleLease(t *testing.T, b *broker.Broker, wsUID broker.PlatformID) (broker.Lease, uint64) {
	t.Helper()
	lease := leaseFor(t, b, gwA, wsUID, false)
	epoch, err := b.ClaimStream(ctx, gwA, lease.ID, fenceOf(lease), testTab)
	if err != nil {
		t.Fatalf("ClaimStream: %v", err)
	}
	for _, typ := range []broker.ActivityEventType{broker.ActivityConnected, broker.ActivityDisconnect} {
		if err := b.ReportActivity(ctx, gwA, lease.ID, fenceOf(lease),
			broker.ActivityEvent{Type: typ, StreamEpoch: epoch}); err != nil {
			t.Fatalf("ReportActivity(%s): %v", typ, err)
		}
	}
	return lease, epoch
}

// TestReleaseIdleLease_ReleasesIdleLease: the owner of the current epoch,
// with no open stream, ends the lease — the workspace is free for a plain
// reopen, and the dead lease can neither renew nor claim.
func TestReleaseIdleLease_ReleasesIdleLease(t *testing.T) {
	db, b, clock, src := setup(t)
	seedWorkspace(t, db, "tenant-a", alice.Owner(), "ws-1")
	src.set(readyBinding("ws-1", "tenant-a", alice.Owner(), 1, "rt-1", clock.Now()))
	lease, epoch := idleLease(t, b, "ws-1")

	released, err := b.ReleaseIdleLease(ctx, gwA, lease.ID, fenceOf(lease), epoch)
	if err != nil || !released {
		t.Fatalf("ReleaseIdleLease = %v, %v; want true, nil", released, err)
	}
	if state, closed := leaseState(t, db, lease.ID); state != "revoked" || !closed {
		t.Fatalf("lease state=%q closed=%v, want revoked+closed_at", state, closed)
	}
	if open, disconn := openStreams(t, db, "ws-1", lease.RuntimeGeneration); open != 0 || !disconn {
		t.Fatalf("open_streams=%d disconnected_since set=%v, want 0+anchored", open, disconn)
	}
	if _, err := b.RenewLease(ctx, gwA, lease.ID, fenceOf(lease)); !errors.Is(err, broker.ErrRevoked) {
		t.Fatalf("renew after release = %v, want ErrRevoked", err)
	}
	if _, err := b.ClaimStream(ctx, gwA, lease.ID, fenceOf(lease), testTab); !errors.Is(err, broker.ErrRevoked) {
		t.Fatalf("claim after release = %v, want ErrRevoked", err)
	}
	// The workspace is free: a plain reopen (no takeover) gets a ticket.
	if _, err := b.IssueTicket(ctx, alice, "ws-1", false, provisioning.SessionPolicy{}, testPortalSession); err != nil {
		t.Fatalf("IssueTicket without takeover after release: %v", err)
	}
	// Releasing the dead lease again is a refusal, not an error.
	if again, err := b.ReleaseIdleLease(ctx, gwA, lease.ID, fenceOf(lease), epoch); err != nil || again {
		t.Fatalf("second release = %v, %v; want false, nil", again, err)
	}
}

// TestReleaseIdleLease_RefusedOnEpochAdvance: a stream claimed since the
// caller's epoch (any replica) means the lease is in use — refused.
func TestReleaseIdleLease_RefusedOnEpochAdvance(t *testing.T) {
	db, b, clock, src := setup(t)
	seedWorkspace(t, db, "tenant-a", alice.Owner(), "ws-1")
	src.set(readyBinding("ws-1", "tenant-a", alice.Owner(), 1, "rt-1", clock.Now()))
	lease, epoch := idleLease(t, b, "ws-1")
	if _, err := b.ClaimStream(ctx, gwA, lease.ID, fenceOf(lease), testTab); err != nil {
		t.Fatalf("second ClaimStream: %v", err)
	}

	released, err := b.ReleaseIdleLease(ctx, gwA, lease.ID, fenceOf(lease), epoch)
	if err != nil || released {
		t.Fatalf("release at a superseded epoch = %v, %v; want false, nil", released, err)
	}
	if state, _ := leaseState(t, db, lease.ID); state != "active" {
		t.Fatalf("lease state=%q after a refused release, want active", state)
	}
}

// TestReleaseIdleLease_RefusedWhileStreamOpen: the generation still counts
// an open stream (its disconnect never arrived) — refused.
func TestReleaseIdleLease_RefusedWhileStreamOpen(t *testing.T) {
	db, b, clock, src := setup(t)
	seedWorkspace(t, db, "tenant-a", alice.Owner(), "ws-1")
	src.set(readyBinding("ws-1", "tenant-a", alice.Owner(), 1, "rt-1", clock.Now()))
	lease := leaseFor(t, b, gwA, "ws-1", false)
	epoch, err := b.ClaimStream(ctx, gwA, lease.ID, fenceOf(lease), testTab)
	if err != nil {
		t.Fatalf("ClaimStream: %v", err)
	}
	if err := b.ReportActivity(ctx, gwA, lease.ID, fenceOf(lease),
		broker.ActivityEvent{Type: broker.ActivityConnected, StreamEpoch: epoch}); err != nil {
		t.Fatalf("ReportActivity(connected): %v", err)
	}

	released, err := b.ReleaseIdleLease(ctx, gwA, lease.ID, fenceOf(lease), epoch)
	if err != nil || released {
		t.Fatalf("release with an open stream = %v, %v; want false, nil", released, err)
	}
	if state, _ := leaseState(t, db, lease.ID); state != "active" {
		t.Fatalf("lease state=%q after a refused release, want active", state)
	}
}

// TestReleaseIdleLease_RefusedOnIdentityFenceOrExpiry: a mismatch is a
// refusal (false, nil), never an error, and never touches the lease.
func TestReleaseIdleLease_RefusedOnIdentityFenceOrExpiry(t *testing.T) {
	db, b, clock, src := setup(t)
	seedWorkspace(t, db, "tenant-a", alice.Owner(), "ws-1")
	src.set(readyBinding("ws-1", "tenant-a", alice.Owner(), 1, "rt-1", clock.Now()))
	lease, epoch := idleLease(t, b, "ws-1")

	staleVersion := fenceOf(lease)
	staleVersion.FencingVersion++
	otherRuntime := fenceOf(lease)
	otherRuntime.RuntimeUID = "rt-other"
	cases := []struct {
		name    string
		gw      broker.GatewayIdentity
		leaseID string
		fence   broker.Fence
		epoch   uint64
	}{
		{"foreign gateway", gwB, lease.ID, fenceOf(lease), epoch},
		{"stale fencing version", gwA, lease.ID, staleVersion, epoch},
		{"other runtime incarnation", gwA, lease.ID, otherRuntime, epoch},
		{"epoch zero (never claimed)", gwA, lease.ID, fenceOf(lease), 0},
		{"future epoch", gwA, lease.ID, fenceOf(lease), epoch + 1},
		{"unknown lease", gwA, "lease-does-not-exist", fenceOf(lease), epoch},
	}
	for _, tc := range cases {
		released, err := b.ReleaseIdleLease(ctx, tc.gw, tc.leaseID, tc.fence, tc.epoch)
		if err != nil || released {
			t.Fatalf("%s: release = %v, %v; want false, nil", tc.name, released, err)
		}
	}
	if state, _ := leaseState(t, db, lease.ID); state != "active" {
		t.Fatalf("lease state=%q after refused releases, want active", state)
	}

	// Past its TTL the row is the lazy expiry's to close, not the release's.
	clock.Advance(broker.LeaseTTL + 1)
	released, err := b.ReleaseIdleLease(ctx, gwA, lease.ID, fenceOf(lease), epoch)
	if err != nil || released {
		t.Fatalf("release of an expired lease = %v, %v; want false, nil", released, err)
	}
}

// TestReleaseIdleLease_RaceWithClaimHasOneWinner: a release and a stream
// claim on the same idle lease serialize on the lease row — either the
// release wins (the claim sees ErrRevoked) or the claim wins (the release
// sees the new epoch and refuses). Never both, never neither.
func TestReleaseIdleLease_RaceWithClaimHasOneWinner(t *testing.T) {
	db, b, clock, src := setup(t)
	for i := 0; i < 10; i++ {
		ws := broker.PlatformID(fmt.Sprintf("ws-race-%d", i))
		seedWorkspace(t, db, "tenant-a", alice.Owner(), string(ws))
		src.set(readyBinding(ws, "tenant-a", alice.Owner(), 1, "rt-1", clock.Now()))
		lease, epoch := idleLease(t, b, ws)

		var (
			wg       sync.WaitGroup
			released bool
			relErr   error
			claimErr error
		)
		start := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			released, relErr = b.ReleaseIdleLease(ctx, gwA, lease.ID, fenceOf(lease), epoch)
		}()
		go func() {
			defer wg.Done()
			<-start
			_, claimErr = b.ClaimStream(ctx, gwA, lease.ID, fenceOf(lease), testTab)
		}()
		close(start)
		wg.Wait()
		if relErr != nil {
			t.Fatalf("iter %d: ReleaseIdleLease error: %v", i, relErr)
		}
		state, _ := leaseState(t, db, lease.ID)
		switch {
		case released && errors.Is(claimErr, broker.ErrRevoked) && state == "revoked":
		case !released && claimErr == nil && state == "active":
		default:
			t.Fatalf("iter %d: released=%v claimErr=%v state=%q — want exactly one winner", i, released, claimErr, state)
		}
	}
}
