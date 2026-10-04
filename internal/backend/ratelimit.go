// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package backend

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/ratelimit"
	"github.com/tinyorbitvn/tinycdi/internal/store"
)

// rateLimitSweepInterval is the cadence of the expired-window delete. The
// 15-minute row retention makes the exact cadence unimportant; a minute
// keeps the table at roughly active-keys × 15 rows.
const rateLimitSweepInterval = time.Minute

// Rate-limit window routes (ADR 0006): the limiter families the Postgres
// window backs. The shared login bucket covers /v1/login,
// /v1/auth/callback and GET /v1/session; the callback's per-IP spray
// ceiling and the launch budget are their own windows.
const (
	rateLimitRouteLogin           = "login"
	rateLimitRouteCallbackCeiling = "callback_ceiling"
	rateLimitRouteLaunch          = "launch"
)

// loginLimiters builds the login-family limiter pair for cfg — the shared
// bucket (login start, session probe, callback) and the per-IP callback
// ceiling at 10× the *divided* login budget so the FX-R30 multiplier
// stays exact per replica (RL-1).
func loginLimiters(cfg Config, now func() time.Time) (shared, ceiling *ratelimit.Limiter) {
	rate, burst := ratelimit.PerReplica(cfg.LoginRate, loginRateBurst, cfg.RateLimitReplicas)
	return ratelimit.New(rate, burst, rateLimitMaxKeys, now),
		ratelimit.New(10*rate, 10*burst, rateLimitMaxKeys, now)
}

// launchLimiter builds the divided per-replica /v1/launch limiter — the
// whole story in split mode, and the local ceiling/fallback under the
// shared window in merged mode.
func launchLimiter(cfg Config, now func() time.Time) *ratelimit.Limiter {
	rate, burst := ratelimit.PerReplica(cfg.LaunchRate, launchRateBurst, cfg.RateLimitReplicas)
	return ratelimit.New(rate, burst, rateLimitMaxKeys, now)
}

// rateLimitErrHook returns the store-error counter callback, or nil when
// the metrics listener is off.
func (b *Backend) rateLimitErrHook() func(route string) {
	if b.metrics == nil {
		return nil
	}
	return b.metrics.IncRateLimitStoreError
}

// rateLimitDegradedHook returns the circuit-breaker gauge callback, or
// nil when the metrics listener is off.
func (b *Backend) rateLimitDegradedHook() func(route string, degraded bool) {
	if b.metrics == nil {
		return nil
	}
	return b.metrics.SetRateLimitStoreDegraded
}

// sharedLoginLimiters wraps the divided local login pair in the Postgres
// fixed window (ADR 0006 option B): the window's bound is the UNDIVIDED
// rate+burst per minute (the token bucket's rate R/min + burst B maps to
// a fixed-window limit of R+B), while the divided local bucket stays on
// as the per-replica ceiling and the fail-open fallback — effective bound
// = min(shared window, local ceiling).
func sharedLoginLimiters(cfg Config, db *store.DB, log *slog.Logger, onStoreError func(string), onDegraded func(string, bool)) (shared, ceiling ratelimit.Allower) {
	localShared, localCeiling := loginLimiters(cfg, nil)
	windowLimit := cfg.LoginRate + loginRateBurst
	return ratelimit.NewShared(localShared, db, rateLimitRouteLogin, windowLimit, log, onStoreError, onDegraded),
		ratelimit.NewShared(localCeiling, db, rateLimitRouteCallbackCeiling, 10*windowLimit, log, onStoreError, onDegraded)
}

// sharedLaunchLimiter is the merged-mode launch limiter: the Postgres
// window bound to the undivided rate+burst, over the divided local
// ceiling/fallback.
func sharedLaunchLimiter(cfg Config, db *store.DB, log *slog.Logger, onStoreError func(string), onDegraded func(string, bool)) ratelimit.Allower {
	return ratelimit.NewShared(launchLimiter(cfg, nil), db, rateLimitRouteLaunch,
		cfg.LaunchRate+launchRateBurst, log, onStoreError, onDegraded)
}

// rateLimitWindowSweepLoop is the leader-gated singleton that deletes
// expired rate-limit window rows. Registered on b.singletons, it runs
// only on the replica holding the Postgres advisory lock — the delete is
// idempotent, so the gating exists to keep every replica from paying the
// scan, not for correctness. During a store outage the limiters fail
// open and the table simply waits for the next sweep.
func rateLimitWindowSweepLoop(db *store.DB, log *slog.Logger) func(ctx context.Context) {
	return func(ctx context.Context) {
		sweep := func() {
			n, err := db.SweepRateLimitWindows(ctx)
			switch {
			case err != nil && errors.Is(err, context.Canceled):
			case err != nil:
				log.Error("rate-limit window sweep", "err", err)
			case n > 0:
				log.Info("rate-limit windows swept", "rows", n)
			}
		}
		sweep()
		ticker := time.NewTicker(rateLimitSweepInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				sweep()
			}
		}
	}
}
