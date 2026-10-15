// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"fmt"
	"time"
)

// rateLimitWindowRetention is how long expired rate-limit windows are
// kept before the leader's sweep deletes them: long enough that a window
// boundary is never revisited, short enough that a sprayed key space
// cannot grow the table unboundedly.
const rateLimitWindowRetention = 15 * time.Minute

// RateLimitWindowHit performs the entire shared-window check in ONE
// statement (ADR 0006 option B): insert-or-increment the counter for
// (route, key) in the server's current minute window and report the
// resulting count plus the time until the window resets. Postgres
// serialises the upsert across replicas, so count is the exact aggregate
// — a request's ordinal inside the window. The window boundary comes
// from the database clock; application-clock skew cannot shift it.
//
// Callers compare count against their limit; a denied request still
// increments (the over-limit hit must be counted for the bound to hold).
// resetIn is reported by the server so Retry-After stays on the same
// clock domain as the window itself.
func (d *DB) RateLimitWindowHit(ctx context.Context, route, key string) (count int64, resetIn time.Duration, err error) {
	var reset float64
	err = d.pool.QueryRow(ctx, `
		INSERT INTO rate_limit_window (route, bucket_key, window_start, count)
		VALUES ($1, $2, date_trunc('minute', now()), 1)
		ON CONFLICT (route, bucket_key, window_start)
		DO UPDATE SET count = rate_limit_window.count + 1
		RETURNING count,
		          EXTRACT(EPOCH FROM window_start + interval '1 minute' - now())`,
		route, key).Scan(&count, &reset)
	if err != nil {
		return 0, 0, fmt.Errorf("store: rate-limit window hit %w", err)
	}
	if reset < 0 {
		reset = 0
	}
	return count, time.Duration(reset * float64(time.Second)), nil
}

// SweepRateLimitWindows deletes expired window rows. Runs on the leader
// replica only (a singleton under the Postgres advisory lock) — the
// delete is idempotent, so a second sweeper would only waste a scan.
// Returns the number of rows removed.
func (d *DB) SweepRateLimitWindows(ctx context.Context) (int64, error) {
	tag, err := d.pool.Exec(ctx,
		`DELETE FROM rate_limit_window
		 WHERE window_start < now() - $1 * interval '1 second'`,
		int64(rateLimitWindowRetention/time.Second))
	if err != nil {
		return 0, fmt.Errorf("store: rate-limit window sweep %w", err)
	}
	return tag.RowsAffected(), nil
}
