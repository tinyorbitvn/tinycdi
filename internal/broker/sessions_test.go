// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package broker_test

// Contract tests for the restart-safe session directory (design §3.6,
// migration 011): a session cookie resolves — by SHA-256 digest only — to
// the live lease on any replica, and each interactive stream claims a
// monotonically increasing epoch so an older stream is fenced.

import (
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/broker"
)

func digestOf(cookie string) broker.SessionDigest {
	return broker.SessionDigest(sha256.Sum256([]byte(cookie)))
}

// TestBindSession_ThenLookup: a digest bound to a live lease resolves back
// to the same lease — the restart-safe cookie→lease path.
func TestBindSession_ThenLookup(t *testing.T) {
	db, b, clock, src := setup(t)
	seedWorkspace(t, db, "tenant-a", alice.Owner(), "ws-1")
	src.set(readyBinding("ws-1", "tenant-a", alice.Owner(), 1, "rt-1", clock.Now()))

	lease := leaseFor(t, b, gwA, "ws-1", false)
	d := digestOf("cookie-1")
	if err := b.BindSession(ctx, gwA, lease.ID, d); err != nil {
		t.Fatalf("BindSession: %v", err)
	}
	got, err := b.LeaseBySession(ctx, gwA, d)
	if err != nil {
		t.Fatalf("LeaseBySession: %v", err)
	}
	if got.ID != lease.ID ||
		got.WorkspaceUID != lease.WorkspaceUID ||
		got.FencingVersion != lease.FencingVersion {
		t.Fatalf("resolved lease %+v, want %+v", got, lease)
	}
}

// TestBindSession_Idempotent: rebinding the same digest is a no-op, so a
// retried bind never fails.
func TestBindSession_Idempotent(t *testing.T) {
	db, b, clock, src := setup(t)
	seedWorkspace(t, db, "tenant-a", alice.Owner(), "ws-1")
	src.set(readyBinding("ws-1", "tenant-a", alice.Owner(), 1, "rt-1", clock.Now()))

	lease := leaseFor(t, b, gwA, "ws-1", false)
	d := digestOf("cookie-1")
	if err := b.BindSession(ctx, gwA, lease.ID, d); err != nil {
		t.Fatalf("BindSession: %v", err)
	}
	if err := b.BindSession(ctx, gwA, lease.ID, d); err != nil {
		t.Fatalf("second BindSession same digest = %v, want nil", err)
	}
}

// TestBindSession_SecondDigestDenied: one lease carries at most one session;
// a second digest is denied and the first keeps working.
func TestBindSession_SecondDigestDenied(t *testing.T) {
	db, b, clock, src := setup(t)
	seedWorkspace(t, db, "tenant-a", alice.Owner(), "ws-1")
	src.set(readyBinding("ws-1", "tenant-a", alice.Owner(), 1, "rt-1", clock.Now()))

	lease := leaseFor(t, b, gwA, "ws-1", false)
	d1, d2 := digestOf("cookie-1"), digestOf("cookie-2")
	if err := b.BindSession(ctx, gwA, lease.ID, d1); err != nil {
		t.Fatalf("BindSession: %v", err)
	}
	if err := b.BindSession(ctx, gwA, lease.ID, d2); !errors.Is(err, broker.ErrDenied) {
		t.Fatalf("second digest bind = %v, want ErrDenied", err)
	}
	if _, err := b.LeaseBySession(ctx, gwA, d1); err != nil {
		t.Fatalf("LeaseBySession(first) = %v, want the original session intact", err)
	}
}

// TestLeaseBySession_Unknown: a cookie that was never bound resolves to
// nothing — unknown digest, not a lease.
func TestLeaseBySession_Unknown(t *testing.T) {
	db, b, clock, src := setup(t)
	seedWorkspace(t, db, "tenant-a", alice.Owner(), "ws-1")
	src.set(readyBinding("ws-1", "tenant-a", alice.Owner(), 1, "rt-1", clock.Now()))
	leaseFor(t, b, gwA, "ws-1", false)

	if _, err := b.LeaseBySession(ctx, gwA, digestOf("never-bound")); !errors.Is(err, broker.ErrLeaseInvalid) {
		t.Fatalf("unknown digest = %v, want ErrLeaseInvalid", err)
	}
}

// TestLeaseBySession_AfterRevoke: a revoked lease's digest must not resolve —
// the session is gone even though the digest row remains.
func TestLeaseBySession_AfterRevoke(t *testing.T) {
	db, b, clock, src := setup(t)
	seedWorkspace(t, db, "tenant-a", alice.Owner(), "ws-1")
	src.set(readyBinding("ws-1", "tenant-a", alice.Owner(), 1, "rt-1", clock.Now()))

	lease := leaseFor(t, b, gwA, "ws-1", false)
	d := digestOf("cookie-1")
	if err := b.BindSession(ctx, gwA, lease.ID, d); err != nil {
		t.Fatalf("BindSession: %v", err)
	}
	if err := b.RevokeLease(ctx, lease.ID); err != nil {
		t.Fatalf("RevokeLease: %v", err)
	}
	if _, err := b.LeaseBySession(ctx, gwA, d); !errors.Is(err, broker.ErrRevoked) {
		t.Fatalf("lookup after revoke = %v, want ErrRevoked", err)
	}
}

// TestLeaseBySession_AfterExpiry: an un-renewed lease dies on its TTL; the
// digest then resolves to ErrRevoked, not to a zombie session.
func TestLeaseBySession_AfterExpiry(t *testing.T) {
	db, b, clock, src := setup(t)
	seedWorkspace(t, db, "tenant-a", alice.Owner(), "ws-1")
	src.set(readyBinding("ws-1", "tenant-a", alice.Owner(), 1, "rt-1", clock.Now()))

	lease := leaseFor(t, b, gwA, "ws-1", false)
	d := digestOf("cookie-1")
	if err := b.BindSession(ctx, gwA, lease.ID, d); err != nil {
		t.Fatalf("BindSession: %v", err)
	}
	clock.Advance(broker.LeaseTTL + time.Second)
	if _, err := b.LeaseBySession(ctx, gwA, d); !errors.Is(err, broker.ErrRevoked) {
		t.Fatalf("lookup after expiry = %v, want ErrRevoked", err)
	}
}

// TestLeaseBySession_ForeignGateway: a digest bound on gateway gw-1 does not
// resolve for gateway gw-2 — replica sessions stay partitioned by identity.
func TestLeaseBySession_ForeignGateway(t *testing.T) {
	db, b, clock, src := setup(t)
	seedWorkspace(t, db, "tenant-a", alice.Owner(), "ws-1")
	src.set(readyBinding("ws-1", "tenant-a", alice.Owner(), 1, "rt-1", clock.Now()))

	lease := leaseFor(t, b, gwA, "ws-1", false)
	d := digestOf("cookie-1")
	if err := b.BindSession(ctx, gwA, lease.ID, d); err != nil {
		t.Fatalf("BindSession: %v", err)
	}
	if _, err := b.LeaseBySession(ctx, gwB, d); !errors.Is(err, broker.ErrDenied) {
		t.Fatalf("foreign gateway lookup = %v, want ErrDenied", err)
	}
}

// TestLeaseBySession_SupersededByTakeover: a takeover fences the old lease —
// its digest resolves to ErrRevoked — while the new lease may bind a digest
// of its own (the partial unique index freed the old binding).
func TestLeaseBySession_SupersededByTakeover(t *testing.T) {
	db, b, clock, src := setup(t)
	seedWorkspace(t, db, "tenant-a", alice.Owner(), "ws-1")
	src.set(readyBinding("ws-1", "tenant-a", alice.Owner(), 1, "rt-1", clock.Now()))

	old := leaseFor(t, b, gwA, "ws-1", false)
	d := digestOf("cookie-1")
	if err := b.BindSession(ctx, gwA, old.ID, d); err != nil {
		t.Fatalf("BindSession: %v", err)
	}
	newLease := leaseFor(t, b, gwA, "ws-1", true)

	if _, err := b.LeaseBySession(ctx, gwA, d); !errors.Is(err, broker.ErrRevoked) {
		t.Fatalf("lookup of superseded digest = %v, want ErrRevoked", err)
	}
	if err := b.BindSession(ctx, gwA, newLease.ID, digestOf("cookie-2")); err != nil {
		t.Fatalf("new lease BindSession = %v", err)
	}
}

// TestClaimStream_Increments: each claim takes the next stream epoch and the
// lease reports it — the cross-replica stream fence of §3.6.
func TestClaimStream_Increments(t *testing.T) {
	db, b, clock, src := setup(t)
	seedWorkspace(t, db, "tenant-a", alice.Owner(), "ws-1")
	src.set(readyBinding("ws-1", "tenant-a", alice.Owner(), 1, "rt-1", clock.Now()))

	lease := leaseFor(t, b, gwA, "ws-1", false)
	e1, err := b.ClaimStream(ctx, gwA, lease.ID, fenceOf(lease))
	if err != nil {
		t.Fatalf("ClaimStream #1: %v", err)
	}
	e2, err := b.ClaimStream(ctx, gwA, lease.ID, fenceOf(lease))
	if err != nil {
		t.Fatalf("ClaimStream #2: %v", err)
	}
	if e1 != 1 || e2 != 2 {
		t.Fatalf("epochs = %d, %d; want 1, 2", e1, e2)
	}
	renewed, err := b.RenewLease(ctx, gwA, lease.ID, fenceOf(lease))
	if err != nil {
		t.Fatalf("RenewLease: %v", err)
	}
	if renewed.StreamEpoch != 2 {
		t.Fatalf("renewed lease StreamEpoch = %d, want 2", renewed.StreamEpoch)
	}
}

// TestClaimStream_StaleFence: a fence naming a different runtimeUID cannot
// claim a stream epoch — the dead incarnation's stream stays fenced.
func TestClaimStream_StaleFence(t *testing.T) {
	db, b, clock, src := setup(t)
	seedWorkspace(t, db, "tenant-a", alice.Owner(), "ws-1")
	src.set(readyBinding("ws-1", "tenant-a", alice.Owner(), 1, "rt-1", clock.Now()))

	lease := leaseFor(t, b, gwA, "ws-1", false)
	stale := fenceOf(lease)
	stale.RuntimeUID = "rt-other"
	if _, err := b.ClaimStream(ctx, gwA, lease.ID, stale); !errors.Is(err, broker.ErrStaleBinding) {
		t.Fatalf("claim with stale fence = %v, want ErrStaleBinding", err)
	}
}

// TestLeaseExpiry_ZeroesOpenStreams: a replica dying mid-stream must not
// leave the workspace looking connected — when the lease expires
// un-renewed, the lazy expiry zeroes its generation's open_streams and sets
// disconnected_since so the disconnect timeout still fires.
func TestLeaseExpiry_ZeroesOpenStreams(t *testing.T) {
	db, b, clock, src := setup(t)
	seedWorkspace(t, db, "tenant-a", alice.Owner(), "ws-1")
	src.set(readyBinding("ws-1", "tenant-a", alice.Owner(), 1, "rt-1", clock.Now()))

	lease := leaseFor(t, b, gwA, "ws-1", false)
	d := digestOf("cookie-1")
	if err := b.BindSession(ctx, gwA, lease.ID, d); err != nil {
		t.Fatalf("BindSession: %v", err)
	}
	if err := b.ReportActivity(ctx, gwA, lease.ID, fenceOf(lease),
		broker.ActivityEvent{Type: broker.ActivityConnected}); err != nil {
		t.Fatalf("ReportActivity(connected): %v", err)
	}

	clock.Advance(broker.LeaseTTL + time.Second)
	if _, err := b.LeaseBySession(ctx, gwA, d); !errors.Is(err, broker.ErrRevoked) {
		t.Fatalf("lookup after expiry = %v, want ErrRevoked", err)
	}

	var (
		open    int
		disconn *time.Time
	)
	if err := db.Pool().QueryRow(ctx,
		`SELECT open_streams, disconnected_since FROM workspace_activity
		 WHERE workspace_id = $1 AND runtime_generation = $2`,
		"ws-1", int64(lease.RuntimeGeneration)).Scan(&open, &disconn); err != nil {
		t.Fatalf("workspace_activity row: %v", err)
	}
	if open != 0 {
		t.Fatalf("open_streams = %d after lease expiry, want 0", open)
	}
	if disconn == nil {
		t.Fatal("disconnected_since NULL after lease expiry — disconnect timeout could never fire")
	}
}

// TestMigration011_Idempotent: the digest/epoch migration applies cleanly on
// a fresh database and on re-run, and the partial unique index exists.
func TestMigration011_Idempotent(t *testing.T) {
	db := newDB(t)
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	var def string
	if err := db.Pool().QueryRow(ctx,
		`SELECT indexdef FROM pg_indexes
		 WHERE schemaname = 'public' AND indexname = 'connection_lease_session_digest'`).
		Scan(&def); err != nil {
		t.Fatalf("connection_lease_session_digest index missing: %v", err)
	}
	if !strings.Contains(def, "UNIQUE") || !strings.Contains(def, "WHERE") {
		t.Fatalf("index %q is not the pinned partial unique index", def)
	}
}
