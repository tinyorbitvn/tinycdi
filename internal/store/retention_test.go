// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package store_test

// Retention-sweep contract tests (SR-4-F5): terminal connection_lease and
// launch_ticket rows older than the window are pruned in bounded batches,
// live/outstanding rows are never touched, and overlapping sweepers split
// work without deadlock. The tests drive the store directly — every
// reader of these tables (redeem, renew, cookie resolve, revokes, the
// post-restore tool) predicates on the live row shapes the sweep keeps.

import (
	"context"
	"encoding/binary"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/store"
)

const retention30d = 30 * 24 * time.Hour

// seedLease inserts a connection_lease row in an arbitrary lifecycle
// state with caller-chosen timestamps.
func seedLease(t *testing.T, db *store.DB, wsUID, leaseID, state string,
	createdAt, expiresAt time.Time, closedAt *time.Time) {
	t.Helper()
	_, err := db.Pool().Exec(context.Background(), `
		INSERT INTO connection_lease
			(id, workspace_id, tenant_id, principal_subject, runtime_generation,
			 runtime_uid, fencing_version, gateway_id, state, created_at,
			 expires_at, last_renewed_at, closed_at)
		VALUES ($1, $2, 'tenant-a', 'iss|sub', 1, 'rt-1', 1, 'gw-1', $3, $4, $5, $4, $6)`,
		leaseID, wsUID, state, createdAt, expiresAt, closedAt)
	if err != nil {
		t.Fatalf("seed lease %s: %v", leaseID, err)
	}
}

// seedTicket inserts a launch_ticket row with caller-chosen lifecycle
// timestamps. i differentiates the primary-key hash.
func seedTicket(t *testing.T, db *store.DB, wsUID string, i int,
	createdAt, expiresAt time.Time, consumedAt, revokedAt *time.Time) {
	t.Helper()
	var hash [32]byte
	binary.BigEndian.PutUint64(hash[:8], uint64(i))
	_, err := db.Pool().Exec(context.Background(), `
		INSERT INTO launch_ticket
			(ticket_hash, workspace_id, tenant_id, principal_subject,
			 runtime_generation, runtime_uid, audience, request_id,
			 created_at, expires_at, consumed_at, revoked_at)
		VALUES ($1, $2, 'tenant-a', 'iss|sub', 1, 'rt-1', 'aud', $3, $4, $5, $6, $7)`,
		hash[:], wsUID, fmt.Sprintf("req-t%d", i), createdAt, expiresAt, consumedAt, revokedAt)
	if err != nil {
		t.Fatalf("seed ticket %d: %v", i, err)
	}
}

func countLeases(t *testing.T, db *store.DB) int64 {
	t.Helper()
	var n int64
	if err := db.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM connection_lease`).Scan(&n); err != nil {
		t.Fatalf("count leases: %v", err)
	}
	return n
}

func countTickets(t *testing.T, db *store.DB) int64 {
	t.Helper()
	var n int64
	if err := db.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM launch_ticket`).Scan(&n); err != nil {
		t.Fatalf("count tickets: %v", err)
	}
	return n
}

func mustSweep(t *testing.T, db *store.DB, retention time.Duration, limit int64) store.TerminalRowSweep {
	t.Helper()
	res, err := db.SweepTerminalConnectionRows(context.Background(), retention, limit)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	return res
}

// Old terminal rows of every shape are deleted: each lease terminal
// state (including a terminal row with no closed_at, which falls back to
// expires_at) and each ticket terminal shape (consumed, revoked,
// expired-outstanding).
func TestSweepTerminalConnectionRows_PrunesOldTerminalRows(t *testing.T) {
	db := newDB(t)
	seedWorkspace(t, db, "tenant-a", "iss|sub", "ws-old")

	old := time.Now().Add(-45 * 24 * time.Hour)
	for _, state := range []string{"revoked", "expired", "superseded"} {
		closed := old
		seedLease(t, db, "ws-old", fmt.Sprintf("lease-%s", state), state,
			old.Add(-time.Hour), old, &closed)
	}
	// Terminal row whose closed_at was never recorded prunes on the
	// expires_at fallback — never early, only at its last-servable moment.
	seedLease(t, db, "ws-old", "lease-noclosed", "revoked",
		old.Add(-time.Hour), old, nil)

	consumed, revoked := old, old
	seedTicket(t, db, "ws-old", 1, old.Add(-time.Hour), old.Add(-time.Minute), &consumed, nil)
	seedTicket(t, db, "ws-old", 2, old.Add(-time.Hour), old.Add(-time.Minute), nil, &revoked)
	// Never presented and long past its 60 s TTL: terminal by expiry.
	seedTicket(t, db, "ws-old", 3, old.Add(-time.Hour), old.Add(-time.Minute), nil, nil)

	res := mustSweep(t, db, retention30d, 500)
	if res.Leases != 4 || res.Tickets != 3 {
		t.Fatalf("sweep deleted %+v, want {Leases:4 Tickets:3}", res)
	}
	if n := countLeases(t, db); n != 0 {
		t.Fatalf("%d lease rows left", n)
	}
	if n := countTickets(t, db); n != 0 {
		t.Fatalf("%d ticket rows left", n)
	}
}

// Rows inside the retention window stay, and nothing live is ever
// touched: a nominally-'active' lease is kept no matter how stale
// (lazily-expired rows are the readers' problem; the partial unique
// index bounds them at one per workspace), a recently-consumed ticket
// keeps its terminal timestamp in consumed_at even when expires_at is
// older than the window, an outstanding ticket is never terminal, and an
// outstanding ticket expired inside the window is kept.
func TestSweepTerminalConnectionRows_KeepsRecentAndLiveRows(t *testing.T) {
	db := newDB(t)
	seedWorkspace(t, db, "tenant-a", "iss|sub", "ws-recent")
	seedWorkspace(t, db, "tenant-a", "iss|sub", "ws-abandoned")
	seedWorkspace(t, db, "tenant-a", "iss|sub", "ws-live")

	now := time.Now()
	old := now.Add(-45 * 24 * time.Hour)
	recent := now.Add(-24 * time.Hour)

	closed := recent
	seedLease(t, db, "ws-recent", "lease-recent", "revoked",
		recent.Add(-time.Hour), recent, &closed)
	// Abandoned 'active' lease: the gateway vanished, expires_at is long
	// past — still never a sweep target. Its workspace can hold at most
	// one of these (partial unique index).
	seedLease(t, db, "ws-abandoned", "lease-abandoned", "active",
		old, old, nil)
	seedLease(t, db, "ws-live", "lease-live", "active",
		now.Add(-time.Hour), now.Add(time.Minute), nil)

	// Consumed inside the window: terminal ts is consumed_at — the old
	// expires_at must not pull it into the sweep.
	consumed := recent
	seedTicket(t, db, "ws-recent", 1, old, old, &consumed, nil)
	// Outstanding: unconsumed, unrevoked, TTL ahead.
	seedTicket(t, db, "ws-recent", 2, now.Add(-time.Minute), now.Add(time.Minute), nil, nil)
	// Outstanding but expired inside the window: dead at expires_at, yet
	// too recent to prune.
	seedTicket(t, db, "ws-recent", 3, now.Add(-2*time.Hour), recent, nil, nil)

	res := mustSweep(t, db, retention30d, 500)
	if res.Leases != 0 || res.Tickets != 0 {
		t.Fatalf("sweep deleted %+v, want nothing", res)
	}
	if n := countLeases(t, db); n != 3 {
		t.Fatalf("%d lease rows, want 3", n)
	}
	if n := countTickets(t, db); n != 3 {
		t.Fatalf("%d ticket rows, want 3", n)
	}
}

// The per-statement pruners honour the row limit; the pass loops batches
// until a batch underfills.
func TestPruneTerminal_BatchBoundary(t *testing.T) {
	db := newDB(t)
	seedWorkspace(t, db, "tenant-a", "iss|sub", "ws-batch")

	old := time.Now().Add(-45 * 24 * time.Hour)
	for i := 0; i < 7; i++ {
		closed := old
		seedLease(t, db, "ws-batch", fmt.Sprintf("lease-%02d", i), "expired",
			old.Add(-time.Hour), old, &closed)
		seedTicket(t, db, "ws-batch", i, old.Add(-time.Hour), old, nil, nil)
	}

	for i, want := range []int64{3, 3, 1, 0} {
		n, err := db.PruneTerminalLeases(context.Background(), retention30d, 3)
		if err != nil {
			t.Fatalf("prune batch %d: %v", i, err)
		}
		if n != want {
			t.Fatalf("batch %d deleted %d, want %d", i, n, want)
		}
	}
	for i, want := range []int64{3, 3, 1, 0} {
		n, err := db.PruneTerminalTickets(context.Background(), retention30d, 3)
		if err != nil {
			t.Fatalf("prune ticket batch %d: %v", i, err)
		}
		if n != want {
			t.Fatalf("ticket batch %d deleted %d, want %d", i, n, want)
		}
	}
}

// One sweep call walks every batch: a pass with a small limit still
// clears the whole backlog.
func TestSweepTerminalConnectionRows_BatchesUntilEmpty(t *testing.T) {
	db := newDB(t)
	seedWorkspace(t, db, "tenant-a", "iss|sub", "ws-batches")

	old := time.Now().Add(-45 * 24 * time.Hour)
	for i := 0; i < 9; i++ {
		closed := old
		seedLease(t, db, "ws-batches", fmt.Sprintf("lease-%02d", i), "revoked",
			old.Add(-time.Hour), old, &closed)
		seedTicket(t, db, "ws-batches", i, old.Add(-time.Hour), old, nil, nil)
	}

	res := mustSweep(t, db, retention30d, 4)
	if res.Leases != 9 || res.Tickets != 9 {
		t.Fatalf("sweep deleted %+v, want {Leases:9 Tickets:9}", res)
	}
}

// retention <= 0 disables the pass entirely.
func TestSweepTerminalConnectionRows_DisabledAtZero(t *testing.T) {
	db := newDB(t)
	seedWorkspace(t, db, "tenant-a", "iss|sub", "ws-off")

	old := time.Now().Add(-45 * 24 * time.Hour)
	closed := old
	seedLease(t, db, "ws-off", "lease-0", "expired", old.Add(-time.Hour), old, &closed)
	seedTicket(t, db, "ws-off", 1, old.Add(-time.Hour), old, nil, nil)

	res := mustSweep(t, db, 0, 500)
	if res.Leases != 0 || res.Tickets != 0 {
		t.Fatalf("disabled sweep deleted %+v", res)
	}
	if n := countLeases(t, db) + countTickets(t, db); n != 2 {
		t.Fatalf("disabled sweep removed rows")
	}
}

// Overlapping sweepers only split the work: the deletes are idempotent,
// so four passes over the same window end with empty tables and no
// deadlock. A concurrent revoker flipping live rows to terminal mid-sweep
// is the realistic interleaving — single-row locks taken in the same
// key order serialize instead of crossing waits.
func TestSweepTerminalConnectionRows_ConcurrentSweepers(t *testing.T) {
	db := newDB(t)
	seedWorkspace(t, db, "tenant-a", "iss|sub", "ws-race")

	old := time.Now().Add(-45 * 24 * time.Hour)
	for i := 0; i < 40; i++ {
		closed := old
		seedLease(t, db, "ws-race", fmt.Sprintf("lease-%02d", i), "expired",
			old.Add(-time.Hour), old, &closed)
		seedTicket(t, db, "ws-race", i, old.Add(-time.Hour), old, nil, nil)
	}
	// Live rows a concurrent revoker retires while the sweepers run —
	// some land inside the same key range the deletes are walking.
	for i := 40; i < 48; i++ {
		seedTicket(t, db, "ws-race", i, old.Add(-time.Hour), old.Add(90*24*time.Hour), nil, nil)
	}

	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := db.SweepTerminalConnectionRows(ctx, retention30d, 5)
			errs <- err
		}()
	}
	// The revoker: outstanding tickets die while the sweeps walk the
	// table. Each side holds only per-statement row locks — a contested
	// row just waits out one short statement, and Postgres' deadlock
	// detector would break any residual cycle.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 40; i < 48; i++ {
			var hash [32]byte
			binary.BigEndian.PutUint64(hash[:8], uint64(i))
			if _, err := db.Pool().Exec(ctx,
				`UPDATE launch_ticket SET revoked_at = now() WHERE ticket_hash = $1`, hash[:]); err != nil {
				errs <- fmt.Errorf("revoke ticket %d: %w", i, err)
				return
			}
		}
		errs <- nil
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent sweep: %v", err)
		}
	}
	if n := countLeases(t, db); n != 0 {
		t.Fatalf("%d lease rows left after concurrent sweeps", n)
	}
	// The 40 pre-dead tickets are gone; the 8 freshly revoked ones may or
	// may not have been swept depending on interleaving — never deleted
	// before their own revoke, though, so anything left is terminal.
	var live int64
	if err := db.Pool().QueryRow(ctx,
		`SELECT count(*) FROM launch_ticket WHERE consumed_at IS NULL AND revoked_at IS NULL`).Scan(&live); err != nil {
		t.Fatalf("count outstanding: %v", err)
	}
	if live != 0 {
		t.Fatalf("%d outstanding tickets left — a sweep must never delete a live row", live)
	}
}
