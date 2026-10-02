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
	"github.com/tinyorbitvn/tinycdi/internal/store"
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

// TestLeaseBySession_MissUsesIndex: lease rows are never pruned, so the
// dead-row check on the miss path must be an index probe, not a scan of
// every lease ever issued — otherwise random cookies become full scans.
func TestLeaseBySession_MissUsesIndex(t *testing.T) {
	db, _, _, _ := setup(t)
	seedWorkspace(t, db, "tenant-a", alice.Owner(), "ws-1")
	if _, err := db.Pool().Exec(ctx, `
		INSERT INTO connection_lease
		  (id, workspace_id, tenant_id, principal_subject, runtime_generation,
		   runtime_uid, fencing_version, gateway_id, state, expires_at, closed_at,
		   session_digest)
		SELECT 'dead-' || g, 'ws-1', 'tenant-a', 'iss|alice', 1, 'rt-1', 1, 'gw-a',
		       'revoked', now(), now(), sha256(('cookie-' || g)::bytea)
		FROM generate_series(1, 5000) AS g`); err != nil {
		t.Fatalf("seed dead leases: %v", err)
	}
	if _, err := db.Pool().Exec(ctx, `ANALYZE connection_lease`); err != nil {
		t.Fatalf("analyze: %v", err)
	}

	// The exact miss-path probe from LeaseBySession, for a digest that
	// does not exist.
	miss := digestOf("never-issued")
	rows, err := db.Pool().Query(ctx,
		`EXPLAIN SELECT EXISTS (SELECT 1 FROM connection_lease WHERE session_digest = $1)`, miss[:])
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		plan.WriteString(line + "\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("plan rows: %v", err)
	}
	if strings.Contains(plan.String(), "Seq Scan") || !strings.Contains(plan.String(), "Index") {
		t.Fatalf("miss query is not an index scan over 5000 dead leases:\n%s", plan.String())
	}
}

// openStreamsOf reads the (workspace, generation) activity row's stream
// accounting.
func openStreamsOf(t *testing.T, db *store.DB, wsUID string, gen uint64) (open int, disconnectedSince *time.Time) {
	t.Helper()
	if err := db.Pool().QueryRow(ctx,
		`SELECT open_streams, disconnected_since FROM workspace_activity
		 WHERE workspace_id = $1 AND runtime_generation = $2`,
		wsUID, int64(gen)).Scan(&open, &disconnectedSince); err != nil {
		t.Fatalf("workspace_activity row: %v", err)
	}
	return open, disconnectedSince
}

// TestStreams_HardKillThenRehydrate: a replica killed mid-stream never
// reports its disconnect. When the session rehydrates on another replica,
// the new stream's ClaimStream fences the dead one, so the count follows
// the CURRENT epoch (1), not the sum of every stream ever opened (2) — and
// the next disconnect reaches 0 and starts the grace window.
func TestStreams_HardKillThenRehydrate(t *testing.T) {
	db, b, clock, src := setup(t)
	seedWorkspace(t, db, "tenant-a", alice.Owner(), "ws-1")
	src.set(readyBinding("ws-1", "tenant-a", alice.Owner(), 1, "rt-1", clock.Now()))
	lease := leaseFor(t, b, gwA, "ws-1", false)
	fence := fenceOf(lease)

	e1, err := b.ClaimStream(ctx, gwA, lease.ID, fence)
	if err != nil || e1 != 1 {
		t.Fatalf("first ClaimStream = %d, %v; want 1", e1, err)
	}
	if err := b.ReportActivity(ctx, gwA, lease.ID, fence,
		broker.ActivityEvent{Type: broker.ActivityConnected, StreamEpoch: e1}); err != nil {
		t.Fatalf("connected@1: %v", err)
	}
	if open, _ := openStreamsOf(t, db, "ws-1", lease.RuntimeGeneration); open != 1 {
		t.Fatalf("open_streams after connected@1 = %d, want 1", open)
	}

	// No disconnect: the first replica was hard-killed. Another replica
	// rehydrates the session and claims the stream.
	e2, err := b.ClaimStream(ctx, gwA, lease.ID, fence)
	if err != nil || e2 != 2 {
		t.Fatalf("second ClaimStream = %d, %v; want 2", e2, err)
	}
	if err := b.ReportActivity(ctx, gwA, lease.ID, fence,
		broker.ActivityEvent{Type: broker.ActivityConnected, StreamEpoch: e2}); err != nil {
		t.Fatalf("connected@2: %v", err)
	}
	if open, _ := openStreamsOf(t, db, "ws-1", lease.RuntimeGeneration); open != 1 {
		t.Fatalf("open_streams after connected@2 = %d, want 1 (the dead stream is fenced)", open)
	}

	if err := b.ReportActivity(ctx, gwA, lease.ID, fence,
		broker.ActivityEvent{Type: broker.ActivityDisconnect, StreamEpoch: e2}); err != nil {
		t.Fatalf("disconnect@2: %v", err)
	}
	open, since := openStreamsOf(t, db, "ws-1", lease.RuntimeGeneration)
	if open != 0 {
		t.Fatalf("open_streams after disconnect@2 = %d, want 0", open)
	}
	if since == nil {
		t.Fatal("disconnected_since NULL after the last disconnect — the disconnect timeout could never fire")
	}
}

// TestStreams_StaleDisconnectIgnored: the late "disconnect" of a fenced
// older stream must not close the newer stream's accounting.
func TestStreams_StaleDisconnectIgnored(t *testing.T) {
	db, b, clock, src := setup(t)
	seedWorkspace(t, db, "tenant-a", alice.Owner(), "ws-1")
	src.set(readyBinding("ws-1", "tenant-a", alice.Owner(), 1, "rt-1", clock.Now()))
	lease := leaseFor(t, b, gwA, "ws-1", false)
	fence := fenceOf(lease)

	e1, _ := b.ClaimStream(ctx, gwA, lease.ID, fence)
	if err := b.ReportActivity(ctx, gwA, lease.ID, fence,
		broker.ActivityEvent{Type: broker.ActivityConnected, StreamEpoch: e1}); err != nil {
		t.Fatalf("connected@1: %v", err)
	}
	e2, _ := b.ClaimStream(ctx, gwA, lease.ID, fence)
	if err := b.ReportActivity(ctx, gwA, lease.ID, fence,
		broker.ActivityEvent{Type: broker.ActivityConnected, StreamEpoch: e2}); err != nil {
		t.Fatalf("connected@2: %v", err)
	}

	// The old stream's disconnect arrives late (a drained or partitioned
	// replica finally reaching the broker).
	if err := b.ReportActivity(ctx, gwA, lease.ID, fence,
		broker.ActivityEvent{Type: broker.ActivityDisconnect, StreamEpoch: e1}); err != nil {
		t.Fatalf("stale disconnect@1: %v", err)
	}
	open, since := openStreamsOf(t, db, "ws-1", lease.RuntimeGeneration)
	if open != 1 {
		t.Fatalf("open_streams after a stale disconnect = %d, want 1", open)
	}
	if since != nil {
		t.Fatal("a stale disconnect armed the disconnect grace window")
	}

	// A stale "connected" from the fenced stream is ignored the same way.
	if err := b.ReportActivity(ctx, gwA, lease.ID, fence,
		broker.ActivityEvent{Type: broker.ActivityConnected, StreamEpoch: e1}); err != nil {
		t.Fatalf("stale connected@1: %v", err)
	}
	if open, _ := openStreamsOf(t, db, "ws-1", lease.RuntimeGeneration); open != 1 {
		t.Fatalf("open_streams after a stale connected = %d, want 1", open)
	}
}

// TestClaimStream_ZeroesPreviousStream: claiming a stream fences the
// previous one by definition, so its slot is released in the same
// transaction — and disconnected_since is anchored until the new stream's
// "connected" report clears it.
func TestClaimStream_ZeroesPreviousStream(t *testing.T) {
	db, b, clock, src := setup(t)
	seedWorkspace(t, db, "tenant-a", alice.Owner(), "ws-1")
	src.set(readyBinding("ws-1", "tenant-a", alice.Owner(), 1, "rt-1", clock.Now()))
	lease := leaseFor(t, b, gwA, "ws-1", false)
	fence := fenceOf(lease)

	e1, _ := b.ClaimStream(ctx, gwA, lease.ID, fence)
	if err := b.ReportActivity(ctx, gwA, lease.ID, fence,
		broker.ActivityEvent{Type: broker.ActivityConnected, StreamEpoch: e1}); err != nil {
		t.Fatalf("connected@1: %v", err)
	}
	if _, err := b.ClaimStream(ctx, gwA, lease.ID, fence); err != nil {
		t.Fatalf("second ClaimStream: %v", err)
	}
	open, since := openStreamsOf(t, db, "ws-1", lease.RuntimeGeneration)
	if open != 0 || since == nil {
		t.Fatalf("after ClaimStream: open_streams=%d disconnected_since=%v, want 0 and set", open, since)
	}
}

// TestStreams_Epoch0KeepsCounting (R9d): without a session directory (split
// mode) the gateway never claims a stream, so every report carries epoch 0.
// There the +1/-1 arithmetic must hold: connected(old), connected(new),
// disconnect(old) leaves the new stream open. The set-to-1 rule belongs to
// epoch >= 1 reports only.
func TestStreams_Epoch0KeepsCounting(t *testing.T) {
	db, b, clock, src := setup(t)
	seedWorkspace(t, db, "tenant-a", alice.Owner(), "ws-1")
	src.set(readyBinding("ws-1", "tenant-a", alice.Owner(), 1, "rt-1", clock.Now()))
	lease := leaseFor(t, b, gwA, "ws-1", false)
	fence := fenceOf(lease)

	for i, typ := range []broker.ActivityEventType{broker.ActivityConnected, broker.ActivityConnected, broker.ActivityDisconnect} {
		if err := b.ReportActivity(ctx, gwA, lease.ID, fence, broker.ActivityEvent{Type: typ}); err != nil {
			t.Fatalf("report %d (%s): %v", i, typ, err)
		}
	}
	open, since := openStreamsOf(t, db, "ws-1", lease.RuntimeGeneration)
	if open != 1 {
		t.Fatalf("open_streams after connected(old), connected(new), disconnect(old) = %d, want 1", open)
	}
	if since != nil {
		t.Fatal("the surviving stream armed the disconnect grace window")
	}
}
