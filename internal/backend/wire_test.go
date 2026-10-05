// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package backend

import (
	"context"
	"fmt"
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
	shared, ceiling := sharedLoginLimiters(cfg, nil, nil, nil, nil)
	if _, ok := shared.(*ratelimit.SharedLimiter); !ok {
		t.Fatalf("login limiter = %T, want *ratelimit.SharedLimiter", shared)
	}
	if _, ok := ceiling.(*ratelimit.SharedLimiter); !ok {
		t.Fatalf("callback ceiling = %T, want *ratelimit.SharedLimiter", ceiling)
	}
	l := sharedLaunchLimiter(cfg, nil, nil, nil, nil)
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

	// LoginRate 30 / 3 replicas -> rate 10/min, burst 20/3 = 6 per pod.
	shared, ceiling := loginLimiters(Config{LoginRate: 30, RateLimitReplicas: 3}, clock)
	for i := 0; i < 6; i++ {
		if ok, _ := shared.Allow("k"); !ok {
			t.Fatalf("shared burst %d refused — want burst 6 (20/3)", i+1)
		}
	}
	ok, retry := shared.Allow("k")
	if ok {
		t.Fatal("7th request inside the divided burst allowed — want burst 6")
	}
	if got := ratelimit.RetryAfterSeconds(retry); got != 6 {
		t.Fatalf("shared Retry-After = %ds, want 6 (10/min per replica)", got)
	}

	// The callback ceiling is 10x the *divided* login budget: rate
	// 100/min, burst 60 — the FX-R30 multiplier stays exact per replica.
	for i := 0; i < 60; i++ {
		if ok, _ := ceiling.Allow("k"); !ok {
			t.Fatalf("ceiling burst %d refused — want burst 60", i+1)
		}
	}
	if ok, _ := ceiling.Allow("k"); ok {
		t.Fatal("61st request inside the divided ceiling allowed — want burst 60")
	}

	// LaunchRate 60 / 2 replicas -> rate 30/min, burst 40/2 = 20 per pod.
	l := launchLimiter(Config{LaunchRate: 60, RateLimitReplicas: 2}, clock)
	for i := 0; i < 20; i++ {
		if ok, _ := l.Allow("k"); !ok {
			t.Fatalf("launch burst %d refused — want burst 20 (40/2)", i+1)
		}
	}
	ok, retry = l.Allow("k")
	if ok {
		t.Fatal("21st launch inside the divided burst allowed — want burst 20")
	}
	if got := ratelimit.RetryAfterSeconds(retry); got != 2 {
		t.Fatalf("launch Retry-After = %ds, want 2 (30/min per replica)", got)
	}
}

// countWindow is a WindowStore fake: every hit increments the per-route
// counter and reports it as the window count, so a SharedLimiter's bound
// is the highest call number it still admits.
type countWindow struct{ n map[string]int64 }

func (w *countWindow) RateLimitWindowHit(_ context.Context, route, _ string) (int64, time.Duration, error) {
	if w.n == nil {
		w.n = map[string]int64{}
	}
	w.n[route]++
	return w.n[route], time.Minute, nil
}

// TestRateLimitDefaults_V040 (RL-DEFAULT): the v0.4.0 defaults double
// the flag budgets — equal to v0.3.0's effective two-replica aggregate
// (each pod's undivided bucket: login 30/min + burst 10, launch 60/min +
// burst 20), before v0.3.1's RL-1 made the flags aggregate bounds.
// Under the exact shared window (ADR 0006) there is no per-replica
// slack: -login-rate 60/min + loginRateBurst 20 map to an 80/minute
// window, -launch-rate 120/min + launchRateBurst 40 to 160/minute, and
// the per-IP callback ceiling stays derived at 10x the login budget. At
// the chart's backend.replicas=2 each pod's local ceiling equals the
// share one v0.3.x pod enforced under RL-1's division.
func TestRateLimitDefaults_V040(t *testing.T) {
	cfg, err := ParseFlags(mergedArgs(), noEnv)
	if err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	if cfg.LoginRate != 60 {
		t.Fatalf("default -login-rate = %d, want 60 (2 x the v0.3.0 per-pod 30/min)", cfg.LoginRate)
	}
	if loginRateBurst != 20 {
		t.Fatalf("loginRateBurst = %d, want 20 (2 x the v0.3.0 per-pod burst 10)", loginRateBurst)
	}
	if cfg.LaunchRate != 120 {
		t.Fatalf("default -launch-rate = %d, want 120 (2 x the v0.3.0 per-pod 60/min)", cfg.LaunchRate)
	}
	if launchRateBurst != 40 {
		t.Fatalf("launchRateBurst = %d, want 40 (2 x the v0.3.0 per-pod burst 20)", launchRateBurst)
	}

	// Shared window bounds: rate+burst on each bucket — 80 login, 160
	// launch — and 10x login on the callback ceiling (800). A fresh key
	// per request clears the local ceiling (each new key starts with a
	// full burst), so the window count alone decides.
	shared, ceiling := sharedLoginLimiters(cfg, &countWindow{}, nil, nil, nil)
	for i := 1; i <= 80; i++ {
		if ok, _ := shared.Allow(fmt.Sprintf("k%d", i)); !ok {
			t.Fatalf("login window refused hit %d — want bound 80 (60+20)", i)
		}
	}
	if ok, _ := shared.Allow("k81"); ok {
		t.Fatal("hit 81 inside the login window allowed — want bound 80 (60+20)")
	}
	for i := 1; i <= 800; i++ {
		if ok, _ := ceiling.Allow(fmt.Sprintf("c%d", i)); !ok {
			t.Fatalf("callback ceiling refused hit %d — want bound 800 (10x login)", i)
		}
	}
	if ok, _ := ceiling.Allow("c801"); ok {
		t.Fatal("hit 801 inside the callback ceiling allowed — want bound 800 (10x login)")
	}
	launch := sharedLaunchLimiter(cfg, &countWindow{}, nil, nil, nil)
	for i := 1; i <= 160; i++ {
		if ok, _ := launch.Allow(fmt.Sprintf("l%d", i)); !ok {
			t.Fatalf("launch window refused hit %d — want bound 160 (120+40)", i)
		}
	}
	if ok, _ := launch.Allow("l161"); ok {
		t.Fatal("hit 161 inside the launch window allowed — want bound 160 (120+40)")
	}

	// Two replicas: the divided local ceilings equal one v0.3.x pod's
	// RL-1 share — login 30/min + burst 10, launch 60/min + burst 20.
	now := time.Unix(1_700_000_000, 0)
	clock := func() time.Time { return now }
	local, localCeiling := loginLimiters(Config{LoginRate: cfg.LoginRate, RateLimitReplicas: 2}, clock)
	for i := 0; i < 10; i++ {
		if ok, _ := local.Allow("k"); !ok {
			t.Fatalf("local burst %d refused — want the per-pod burst 10 (20/2)", i+1)
		}
	}
	if ok, retry := local.Allow("k"); ok {
		t.Fatal("11th local request allowed — want the per-pod burst 10")
	} else if got := ratelimit.RetryAfterSeconds(retry); got != 2 {
		t.Fatalf("local Retry-After = %ds, want 2 (30/min per pod)", got)
	}
	for i := 0; i < 100; i++ {
		if ok, _ := localCeiling.Allow("k"); !ok {
			t.Fatalf("local ceiling burst %d refused — want 100 (10x divided)", i+1)
		}
	}
	if ok, _ := localCeiling.Allow("k"); ok {
		t.Fatal("101st local ceiling request allowed — want burst 100 (10x divided)")
	}
	localLaunch := launchLimiter(Config{LaunchRate: cfg.LaunchRate, RateLimitReplicas: 2}, clock)
	for i := 0; i < 20; i++ {
		if ok, _ := localLaunch.Allow("k"); !ok {
			t.Fatalf("local launch burst %d refused — want the per-pod burst 20 (40/2)", i+1)
		}
	}
	if ok, retry := localLaunch.Allow("k"); ok {
		t.Fatal("21st local launch allowed — want the per-pod burst 20")
	} else if got := ratelimit.RetryAfterSeconds(retry); got != 1 {
		t.Fatalf("local launch Retry-After = %ds, want 1 (60/min per pod)", got)
	}
}
