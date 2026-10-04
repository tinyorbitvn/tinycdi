// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package backend

import (
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/ratelimit"
)

// TestWiredLimiters_SharedWindow (ADR 0006): the merged-mode builders must
// wrap the divided local buckets in the Postgres-window limiter — if the
// wiring ever hands a bare *Limiter to a listener, the aggregate bound
// silently reverts to per-replica enforcement. (db is nil here only to
// keep the test store-free; nothing calls Allow on it.)
func TestWiredLimiters_SharedWindow(t *testing.T) {
	cfg := Config{LoginRate: 30, LaunchRate: 60, RateLimitReplicas: 2}
	shared, ceiling := sharedLoginLimiters(cfg, nil, nil, nil)
	if _, ok := shared.(*ratelimit.SharedLimiter); !ok {
		t.Fatalf("login limiter = %T, want *ratelimit.SharedLimiter", shared)
	}
	if _, ok := ceiling.(*ratelimit.SharedLimiter); !ok {
		t.Fatalf("callback ceiling = %T, want *ratelimit.SharedLimiter", ceiling)
	}
	l := sharedLaunchLimiter(cfg, nil, nil, nil)
	if _, ok := l.(*ratelimit.SharedLimiter); !ok {
		t.Fatalf("launch limiter = %T, want *ratelimit.SharedLimiter", l)
	}
}

// TestWiredLimiters_ReplicaDivision (RL-1) guards the PerReplica call in
// the limiter builders: it asserts the *effective* rate and burst of the
// limiters wire hands to the listeners, so dropping the division (which
// would leave the undivided budget) fails the test. The clock never
// advances, so Allow counts show the burst and Retry-After the rate.
func TestWiredLimiters_ReplicaDivision(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	clock := func() time.Time { return now }

	// LoginRate 30 / 3 replicas -> rate 10/min, burst 10/3 = 3 per pod.
	shared, ceiling := loginLimiters(Config{LoginRate: 30, RateLimitReplicas: 3}, clock)
	for i := 0; i < 3; i++ {
		if ok, _ := shared.Allow("k"); !ok {
			t.Fatalf("shared burst %d refused — want burst 3 (10/3)", i+1)
		}
	}
	ok, retry := shared.Allow("k")
	if ok {
		t.Fatal("4th request inside the divided burst allowed — want burst 3")
	}
	if got := ratelimit.RetryAfterSeconds(retry); got != 6 {
		t.Fatalf("shared Retry-After = %ds, want 6 (10/min per replica)", got)
	}

	// The callback ceiling is 10x the *divided* login budget: rate
	// 100/min, burst 30 — the FX-R30 multiplier stays exact per replica.
	for i := 0; i < 30; i++ {
		if ok, _ := ceiling.Allow("k"); !ok {
			t.Fatalf("ceiling burst %d refused — want burst 30", i+1)
		}
	}
	if ok, _ := ceiling.Allow("k"); ok {
		t.Fatal("31st request inside the divided ceiling allowed — want burst 30")
	}

	// LaunchRate 60 / 2 replicas -> rate 30/min, burst 20/2 = 10 per pod.
	l := launchLimiter(Config{LaunchRate: 60, RateLimitReplicas: 2}, clock)
	for i := 0; i < 10; i++ {
		if ok, _ := l.Allow("k"); !ok {
			t.Fatalf("launch burst %d refused — want burst 10 (20/2)", i+1)
		}
	}
	ok, retry = l.Allow("k")
	if ok {
		t.Fatal("11th launch inside the divided burst allowed — want burst 10")
	}
	if got := ratelimit.RetryAfterSeconds(retry); got != 2 {
		t.Fatalf("launch Retry-After = %ds, want 2 (30/min per replica)", got)
	}
}
