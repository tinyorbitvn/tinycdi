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
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/broker"
	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
	"github.com/tinyorbitvn/tinycdi/internal/store"
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

// setTransferRow moves a minted transfer row to status with claimed_at at
// claimedAt — a claim in flight, a dead claim, or a finished transfer.
func setTransferRow(t *testing.T, db *store.DB, id, status string, claimedAt time.Time) {
	t.Helper()
	if _, err := db.Pool().Exec(ctx, `
		UPDATE session_file_transfer SET status = $2, claimed_at = $3::timestamptz,
			completed_at = CASE WHEN $2 IN ('completed', 'failed') THEN $3::timestamptz END
		WHERE id = $1`, id, status, claimedAt); err != nil {
		t.Fatalf("set transfer %s: %v", id, err)
	}
}

// TestReleaseIdleLease_RefusedWhileTransferLive: a file transfer claimed on
// any replica keeps the workspace busy even with no stream open — the
// releasing replica cannot see another replica's transfer, so the broker
// refuses on the same live-claim predicate ClaimTransfer uses.
func TestReleaseIdleLease_RefusedWhileTransferLive(t *testing.T) {
	db, b, clock, src := setup(t)
	seedWorkspace(t, db, "tenant-a", alice.Owner(), "ws-1")
	src.set(readyBinding("ws-1", "tenant-a", alice.Owner(), 1, "rt-1", clock.Now()))
	lease, epoch := idleLease(t, b, "ws-1")
	mintRow(t, db, "xf_live", "ws-1", "download", 1, "rt-1", clock.Now().Add(time.Minute))
	setTransferRow(t, db, "xf_live", "claimed", clock.Now().Add(-store.FileTransferClaimDeadAfter+time.Minute))

	released, err := b.ReleaseIdleLease(ctx, gwA, lease.ID, fenceOf(lease), epoch)
	if err != nil || released {
		t.Fatalf("release with a live transfer = %v, %v; want false, nil", released, err)
	}
	if state, _ := leaseState(t, db, lease.ID); state != "active" {
		t.Fatalf("lease state=%q after a refused release, want active", state)
	}
}

// TestReleaseIdleLease_ReleasedPastDeadOrFinishedTransfer: a claim older
// than the dead-after window, or a completed transfer, holds nothing.
func TestReleaseIdleLease_ReleasedPastDeadOrFinishedTransfer(t *testing.T) {
	for _, tc := range []struct {
		name, status string
		claimedAgo   time.Duration
	}{
		{"stale claim", "claimed", store.FileTransferClaimDeadAfter + time.Second},
		{"completed", "completed", time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, b, clock, src := setup(t)
			seedWorkspace(t, db, "tenant-a", alice.Owner(), "ws-1")
			src.set(readyBinding("ws-1", "tenant-a", alice.Owner(), 1, "rt-1", clock.Now()))
			lease, epoch := idleLease(t, b, "ws-1")
			mintRow(t, db, "xf_done", "ws-1", "download", 1, "rt-1", clock.Now().Add(time.Minute))
			setTransferRow(t, db, "xf_done", tc.status, clock.Now().Add(-tc.claimedAgo))

			released, err := b.ReleaseIdleLease(ctx, gwA, lease.ID, fenceOf(lease), epoch)
			if err != nil || !released {
				t.Fatalf("release past a %s transfer = %v, %v; want true, nil", tc.name, released, err)
			}
		})
	}
}

// TestReleaseIdleLease_RaceWithTransferClaimHasOneWinner: a release and a
// transfer claim serialize on the workspace row — either the release wins
// (the claim is refused: the lease is gone) or the claim wins (the release
// sees the live transfer and refuses). Never both.
func TestReleaseIdleLease_RaceWithTransferClaimHasOneWinner(t *testing.T) {
	db, b, clock, src := setup(t)
	b.WithTransferPolicy(policy("Bidirectional"))
	for i := 0; i < 10; i++ {
		ws := broker.PlatformID(fmt.Sprintf("ws-xrace-%d", i))
		seedWorkspace(t, db, "tenant-a", alice.Owner(), string(ws))
		src.set(readyBinding(ws, "tenant-a", alice.Owner(), 1, "rt-1", clock.Now()))
		lease, epoch := idleLease(t, b, ws)
		xf := fmt.Sprintf("xf_race_%d", i)
		mintRow(t, db, xf, string(ws), "download", 1, "rt-1", clock.Now().Add(time.Minute))

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
			_, claimErr = b.ClaimTransfer(ctx, gwA, lease.ID, xf, "GET")
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

// TestReleaseIdleLease_ClaimAfterReleaseRefused: a transfer claim that read
// the lease live before a release committed must not start once the release
// holds the workspace — the claim re-checks the lease under the lock.
func TestReleaseIdleLease_ClaimAfterReleaseRefused(t *testing.T) {
	db, b, clock, src := setup(t)
	b.WithTransferPolicy(policy("Bidirectional"))
	seedWorkspace(t, db, "tenant-a", alice.Owner(), "ws-1")
	src.set(readyBinding("ws-1", "tenant-a", alice.Owner(), 1, "rt-1", clock.Now()))
	lease, epoch := idleLease(t, b, "ws-1")
	mintRow(t, db, "xf_late", "ws-1", "download", 1, "rt-1", clock.Now().Add(time.Minute))
	b.WithTransferClaimHook(func() {
		if released, err := b.ReleaseIdleLease(ctx, gwA, lease.ID, fenceOf(lease), epoch); err != nil || !released {
			t.Errorf("release inside the claim gap = %v, %v; want true, nil", released, err)
		}
	})
	if _, err := b.ClaimTransfer(ctx, gwA, lease.ID, "xf_late", "GET"); !errors.Is(err, broker.ErrRevoked) {
		t.Fatalf("claim after a release = %v, want ErrRevoked", err)
	}
	if got := rowStatus(t, db, "xf_late"); got != "pending" {
		t.Fatalf("transfer status %q, want pending (never claimed on a released lease)", got)
	}
}
