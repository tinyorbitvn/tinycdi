// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package ratelimit

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"
)

// Allower is the Allow signature every throttle call site consumes —
// implemented by Limiter (per-replica token bucket) and by SharedLimiter
// (Postgres window + local ceiling), so a call site never needs to know
// which backend enforces the bound.
type Allower interface {
	Allow(key string) (ok bool, retryAfter time.Duration)
}

// WindowStore is the shared fixed-window backend SharedLimiter checks
// against — *store.DB implements it. RateLimitWindowHit is the ENTIRE
// check (one upsert per call, no second query): it increments the key's
// counter in the server's current minute window and returns the
// resulting count plus the time until the window resets, both on the
// store's clock.
type WindowStore interface {
	RateLimitWindowHit(ctx context.Context, route, key string) (count int64, resetIn time.Duration, err error)
}

// windowCheckTimeout bounds one store hit. The expected latency is a few
// milliseconds; past the timeout the request is better served by the
// local limiter than by a hung query.
const windowCheckTimeout = 3 * time.Second

// SharedLimiter enforces a per-key limit backed by a Postgres fixed
// minute window (ADR 0006 option B): one shared counter per key, checked
// by every replica. The configured rate and burst of the flag-bound
// token bucket map to a fixed-window limit of rate+burst per minute
// (the boundary effect admits at most ~2×limit inside any <2-minute
// span — the same overshoot class the per-replica bucket already had).
//
// Guardrails (advisor decision):
//   - the local divided limiter stays BOTH as the fail-open fallback AND
//     as a per-replica ceiling while the store is healthy: a request must
//     pass the local bucket before the shared window is consulted, so the
//     effective bound is min(shared window, divided local) and a
//     locally-refused key never reaches the store (which also bounds the
//     write rate one replica's key spray can cause to its local ceiling);
//   - a store error fails OPEN to the local bucket — every limited
//     route's real work needs Postgres anyway, so the outage degrades to
//     pre-window per-replica limiting rather than a lifted cap or a hard
//     refusal;
//   - the fallback is edge-triggered in the logs: one warn on entry, one
//     info on exit, never a line per request; every store error still
//     increments the onStoreError metric hook.
type SharedLimiter struct {
	local        *Limiter
	store        WindowStore
	route        string // 'login' | 'callback_ceiling' | 'launch'
	limit        int64  // window bound: undivided rate + burst per minute
	log          *slog.Logger
	onStoreError func(route string)

	degraded atomic.Bool
}

// NewShared wraps local with the shared window. local is the per-replica
// ceiling/fallback built by New(rate/n, burst/n, ...); limit is the
// UNDIVIDED rate+burst the shared window enforces per minute; route is
// the window's route label and the onStoreError metric label. store nil
// degrades the limiter to the local bucket (used where no database
// exists — split mode). log nil uses the default logger; onStoreError
// nil skips the metric.
func NewShared(local *Limiter, store WindowStore, route string, limit int, log *slog.Logger, onStoreError func(string)) *SharedLimiter {
	if log == nil {
		log = slog.Default()
	}
	return &SharedLimiter{
		local:        local,
		store:        store,
		route:        route,
		limit:        int64(limit),
		log:          log,
		onStoreError: onStoreError,
	}
}

// Allow reports whether key may proceed, with the same contract as
// Limiter.Allow. Order: the local per-replica ceiling runs first — a
// refusal is final and never touches the store; then the shared window
// increments and the request passes iff its count is within limit. A
// store error degrades the answer to the local verdict (fail-open) and
// is counted/logged edge-triggered.
func (s *SharedLimiter) Allow(key string) (bool, time.Duration) {
	if s == nil || s.local == nil || s.local.perSec <= 0 || s.limit <= 0 {
		// A zero configured rate disables the limiter entirely — the
		// disabled case must never touch the store.
		return true, 0
	}
	ok, retry := s.local.Allow(key)
	if !ok || s.store == nil {
		// Locally refused (or no store): the divided bucket's answer is
		// final — min(shared, local) can only tighten it.
		return ok, retry
	}
	ctx, cancel := context.WithTimeout(context.Background(), windowCheckTimeout)
	count, resetIn, err := s.store.RateLimitWindowHit(ctx, s.route, key)
	cancel()
	if err != nil {
		if s.onStoreError != nil {
			s.onStoreError(s.route)
		}
		if s.degraded.CompareAndSwap(false, true) {
			s.log.Warn("rate-limit store unreachable — enforcing local per-replica limit",
				"route", s.route, "err", err)
		}
		return true, 0
	}
	if s.degraded.CompareAndSwap(true, false) {
		s.log.Info("rate-limit store reachable — shared window limiting resumed", "route", s.route)
	}
	if count > s.limit {
		if resetIn <= 0 {
			resetIn = time.Second
		}
		return false, resetIn
	}
	return true, 0
}
