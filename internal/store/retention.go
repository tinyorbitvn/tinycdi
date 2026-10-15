// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"fmt"
	"time"
)

// TerminalRowSweep counts the rows one retention pass removed, per table.
type TerminalRowSweep struct {
	// Leases is the number of connection_lease rows deleted.
	Leases int64
	// Tickets is the number of launch_ticket rows deleted.
	Tickets int64
}

// terminalSweepBatchPause is the pause between bounded DELETE batches
// inside one pass: each batch is a single statement over primary-key
// order, so the pause — not the batch size — is what leaves headroom for
// the redeem and sign-out paths sharing these tables.
const terminalSweepBatchPause = 100 * time.Millisecond

// PruneTerminalLeases deletes up to limit connection_lease rows that can
// never serve a session again, in one statement, and returns the count.
//
// Terminal is exactly the set the state machine cannot leave —
// 'revoked', 'expired', 'superseded'. Every writer of those states sets
// closed_at in the same statement (the broker transitions and the
// post-restore sweep alike), so closed_at is the row's terminal
// timestamp. COALESCE(closed_at, expires_at) keeps the predicate total
// for a terminal row whose closed_at was never recorded: expires_at is
// the last moment the row could still have served, so the fallback can
// only delay a delete, never advance it.
//
// 'active' rows are never touched, however old: a live lease whose
// gateway stopped renewing is marked 'expired' lazily by the next read
// that finds expires_at past, and the partial unique index bounds
// abandoned 'active' rows at one per workspace, so they cannot grow the
// table on their own.
//
// Each call is a single autocommit statement: its row locks exist only
// for the statement's duration and are bounded by limit, so a contested
// row can hold a redeem or revoke for at most one short delete — and if
// a cycle ever formed, Postgres' deadlock detector breaks it (an
// aborted sweep simply resumes on the next pass; the deletes are
// idempotent). A row deleted or re-shaped between the candidate scan
// and the lock is skipped or re-evaluated under READ COMMITTED, so
// concurrent sweepers split work harmlessly.
func (d *DB) PruneTerminalLeases(ctx context.Context, retention time.Duration, limit int64) (int64, error) {
	tag, err := d.pool.Exec(ctx, `
		DELETE FROM connection_lease
		WHERE id IN (
			SELECT id FROM connection_lease
			WHERE state IN ('revoked', 'expired', 'superseded')
			  AND COALESCE(closed_at, expires_at) < now() - $1 * interval '1 second'
			ORDER BY id
			LIMIT $2)`, int64(retention/time.Second), limit)
	if err != nil {
		return 0, fmt.Errorf("store: prune terminal leases %w", err)
	}
	return tag.RowsAffected(), nil
}

// PruneTerminalTickets deletes up to limit launch_ticket rows that can
// never redeem again, in one statement, and returns the count.
//
// A ticket's terminal timestamp is the moment its terminal shape was
// fixed: consumed_at for a redeemed ticket (set on the first redemption
// attempt, even an already-expired one), revoked_at for a denied one —
// the writers' predicates make the two mutually exclusive — and
// expires_at for a ticket that was never presented (issued +60 s). An
// unexpired outstanding ticket has expires_at in the future and never
// matches; an expired outstanding one is dead weight and prunes on the
// same clock. A presented dead ticket fails closed on the missing row
// exactly as it does on the recorded terminal state, and the 60 s TTL
// means anything the retention window reaches was unredeemable long
// before it could be removed.
//
// Bounded per-statement locking and concurrency semantics are identical
// to PruneTerminalLeases.
func (d *DB) PruneTerminalTickets(ctx context.Context, retention time.Duration, limit int64) (int64, error) {
	tag, err := d.pool.Exec(ctx, `
		DELETE FROM launch_ticket
		WHERE ticket_hash IN (
			SELECT ticket_hash FROM launch_ticket
			WHERE COALESCE(consumed_at, revoked_at, expires_at) < now() - $1 * interval '1 second'
			ORDER BY ticket_hash
			LIMIT $2)`, int64(retention/time.Second), limit)
	if err != nil {
		return 0, fmt.Errorf("store: prune terminal tickets %w", err)
	}
	return tag.RowsAffected(), nil
}

// SweepTerminalConnectionRows runs one full retention pass over the
// session-layer tables (SR-4-F5): every terminal connection_lease and
// launch_ticket row older than retention is deleted in bounded batches
// of limit rows per statement, pausing between batches so the pass never
// holds either table across a redeem or sign-out. The cutoff reads the
// database clock, the same domain the post-restore tool stamps on the
// rows it terminates, so application-clock skew cannot shift the window.
//
// retention <= 0 disables the sweep — the call is a no-op — and
// limit <= 0 deletes nothing. Leadership is the caller's concern: the
// deletes are idempotent, so overlapping sweepers only split the work,
// but the platform runs this on the leader replica to keep every
// replica from paying the scan.
func (d *DB) SweepTerminalConnectionRows(ctx context.Context, retention time.Duration, limit int64) (TerminalRowSweep, error) {
	var res TerminalRowSweep
	if retention <= 0 || limit <= 0 {
		return res, nil
	}
	var err error
	if res.Leases, err = pruneBatches(ctx, retention, limit, d.PruneTerminalLeases); err != nil {
		return res, err
	}
	res.Tickets, err = pruneBatches(ctx, retention, limit, d.PruneTerminalTickets)
	return res, err
}

// pruneBatches calls prune until a batch underfills, pausing between
// batches and stopping on ctx.
func pruneBatches(ctx context.Context, retention time.Duration, limit int64,
	prune func(context.Context, time.Duration, int64) (int64, error)) (int64, error) {
	var total int64
	for {
		n, err := prune(ctx, retention, limit)
		if err != nil {
			return total, err
		}
		total += n
		if n < limit {
			return total, nil
		}
		select {
		case <-ctx.Done():
			return total, ctx.Err()
		case <-time.After(terminalSweepBatchPause):
		}
	}
}

// PruneDeadSessions deletes up to limit session rows that no read can
// resolve any more — a rotated epoch, a passed absolute expiry, or (with
// idle > 0) a last_seen_at outside the idle window — in one statement, and
// returns the count. Session reads never delete (a forged-cookie spray must
// not turn into write traffic), so this leader-only sweep is what bounds
// the table.
func (d *DB) PruneDeadSessions(ctx context.Context, idle time.Duration, limit int64) (int64, error) {
	if limit <= 0 {
		return 0, nil
	}
	idleCond := "FALSE"
	args := []any{limit}
	if idle > 0 {
		idleCond = "last_seen_at <= now() - $2::interval"
		args = append(args, fmt.Sprintf("%dms", idle.Milliseconds()))
	}
	tag, err := d.pool.Exec(ctx, `
		DELETE FROM sessions
		WHERE id IN (
			SELECT id FROM sessions
			WHERE epoch <> `+currentEpochSQL+`
			   OR (expires_at IS NOT NULL AND expires_at <= now())
			   OR `+idleCond+`
			ORDER BY id
			LIMIT $1)`, args...)
	if err != nil {
		return 0, fmt.Errorf("store: prune dead sessions %w", err)
	}
	return tag.RowsAffected(), nil
}
