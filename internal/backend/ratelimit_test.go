// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package backend

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// TestRateLimitSweep_LeaderOnly (ADR 0006): the expired-window DELETE runs
// only on the replica holding the Postgres advisory lock. Two contenders
// register the same sweep as a singleton; the lock admits exactly one —
// so the expired row is gone while only one replica's loop ever ran.
func TestRateLimitSweep_LeaderOnly(t *testing.T) {
	db := newDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// One expired row (outside retention) and one live row (current
	// window): the sweep must remove the first and keep the second.
	if _, err := db.Pool().Exec(ctx, `
		INSERT INTO rate_limit_window (route, bucket_key, window_start, count)
		VALUES ('login', 'old-key', now() - interval '1 hour', 7),
		       ('login', 'live-key', date_trunc('minute', now()), 3)`); err != nil {
		t.Fatalf("seed windows: %v", err)
	}

	var ranA, ranB atomic.Int64
	mk := func(ran *atomic.Int64) func(ctx context.Context) {
		sweep := rateLimitWindowSweepLoop(db, testLog())
		return func(ctx context.Context) {
			ran.Add(1)
			sweep(ctx)
		}
	}
	for _, b := range []*Backend{
		{log: testLog(), singletons: []func(context.Context){mk(&ranA)}},
		{log: testLog(), singletons: []func(context.Context){mk(&ranB)}},
	} {
		b.electSingletons(db, 100*time.Millisecond)
		for _, loop := range b.bg {
			go loop(ctx)
		}
	}

	deadline := time.Now().Add(15 * time.Second)
	for {
		var remaining int64
		if err := db.Pool().QueryRow(ctx,
			`SELECT count(*) FROM rate_limit_window
			 WHERE window_start < now() - interval '15 minutes'`).Scan(&remaining); err != nil {
			t.Fatalf("count expired windows: %v", err)
		}
		if remaining == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("expired window row never swept (ranA=%d ranB=%d)", ranA.Load(), ranB.Load())
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()

	// Exactly one contender held the lock and ran the sweep; the loser's
	// loop never started.
	if got := ranA.Load() + ranB.Load(); got != 1 {
		t.Fatalf("sweep loops ran on %d replicas, want exactly 1 (leader only)", got)
	}
	var live int64
	if err := db.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM rate_limit_window WHERE bucket_key = 'live-key'`).Scan(&live); err != nil {
		t.Fatalf("count live windows: %v", err)
	}
	if live != 1 {
		t.Fatalf("live window row deleted or duplicated: count = %d, want 1", live)
	}
}
