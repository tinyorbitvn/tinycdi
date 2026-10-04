// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package ratelimit

import (
	"context"
	"log/slog"
	"sync"
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

// storeCallTimeout bounds one store hit and applies ALWAYS — healthy or
// probing — so a slow-but-alive store can stall a login/launch by at
// most this much before the local verdict stands. The expected latency
// is a few milliseconds; past the deadline the request is better served
// by the local limiter than by a hung query.
const storeCallTimeout = 500 * time.Millisecond

// storeCooldown is how long the circuit stays open after a store
// error/timeout. While open the store is skipped entirely — the divided
// local limiter decides — then exactly one request probes; success
// closes the circuit, failure reopens it for another cool-down. The
// breaker bounds the degraded-mode latency cost to ~one probe per
// cool-down instead of one stalled check per request.
const storeCooldown = 10 * time.Second

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
//   - a store error/timeout opens a circuit breaker for storeCooldown:
//     checks skip the store entirely until it lapses, then a single
//     request probes (single-flight — concurrent requests keep their
//     local verdict). This caps the added latency of a slow-but-alive
//     store at storeCallTimeout per probe instead of per request;
//   - the fallback is edge-triggered in the logs: one warn on entry, one
//     info on exit, never a line per request; every store error still
//     increments the onStoreError metric hook (skipped checks do not —
//     the counter measures real store failures, ~one per cool-down in a
//     sustained outage) and the onDegraded hook follows the open/closed
//     state for the gauge.
type SharedLimiter struct {
	local        *Limiter
	store        WindowStore
	route        string // 'login' | 'callback_ceiling' | 'launch'
	limit        int64  // window bound: undivided rate + burst per minute
	log          *slog.Logger
	onStoreError func(route string)
	onDegraded   func(route string, degraded bool)
	now          func() time.Time

	mu          sync.Mutex // guards the breaker state below
	degraded    bool       // circuit open: checks skip the store
	brokenUntil time.Time  // end of the current cool-down
	probing     bool       // a post-cool-down probe is in flight (single-flight)
}

// NewShared wraps local with the shared window. local is the per-replica
// ceiling/fallback built by New(rate/n, burst/n, ...); limit is the
// UNDIVIDED rate+burst the shared window enforces per minute; route is
// the window's route label and the metric label of both hooks. store nil
// degrades the limiter to the local bucket (used where no database
// exists — split mode). log nil uses the default logger; onStoreError
// nil skips the error metric, onDegraded nil skips the state gauge.
func NewShared(local *Limiter, store WindowStore, route string, limit int, log *slog.Logger, onStoreError func(string), onDegraded func(string, bool)) *SharedLimiter {
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
		onDegraded:   onDegraded,
		now:          time.Now,
	}
}

// Allow reports whether key may proceed, with the same contract as
// Limiter.Allow. Order: the local per-replica ceiling runs first — a
// refusal is final and never touches the store; then the shared window
// increments and the request passes iff its count is within limit. A
// store error degrades the answer to the local verdict (fail-open) and
// opens the circuit; while the circuit is open the store is skipped
// until a cool-down probe closes it again.
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
	if !s.mayUseStore() {
		// Circuit open inside the cool-down — or a probe already in
		// flight: the store stays untouched, the local verdict stands.
		return true, 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), storeCallTimeout)
	count, resetIn, err := s.store.RateLimitWindowHit(ctx, s.route, key)
	cancel()
	if err != nil {
		s.storeFailed(err)
		return true, 0
	}
	s.storeSucceeded()
	if count > s.limit {
		if resetIn <= 0 {
			resetIn = time.Second
		}
		return false, resetIn
	}
	return true, 0
}

// mayUseStore reports whether this request should consult the store:
// always while the circuit is closed, never inside the cool-down, and —
// once it lapses — for exactly the one request that wins the probe slot
// (single-flight; the rest keep their local verdict).
func (s *SharedLimiter) mayUseStore() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.degraded {
		return true
	}
	if s.probing || s.now().Before(s.brokenUntil) {
		return false
	}
	s.probing = true
	return true
}

// storeFailed records a check error or deadline: the error metric counts
// it (a skipped check never does — the counter measures real store
// failures, not degraded-mode traffic), the circuit opens for a fresh
// cool-down, and the first failure of an outage edge-triggers the warn
// and the gauge.
func (s *SharedLimiter) storeFailed(err error) {
	if s.onStoreError != nil {
		s.onStoreError(s.route)
	}
	s.mu.Lock()
	s.probing = false
	s.brokenUntil = s.now().Add(storeCooldown)
	entered := !s.degraded
	s.degraded = true
	s.mu.Unlock()
	if entered {
		s.log.Warn("rate-limit store unreachable — enforcing local per-replica limit",
			"route", s.route, "err", err)
		if s.onDegraded != nil {
			s.onDegraded(s.route, true)
		}
	}
}

// storeSucceeded closes the circuit on a healthy check; the first
// success after an outage edge-triggers the info and the gauge.
func (s *SharedLimiter) storeSucceeded() {
	s.mu.Lock()
	s.probing = false
	exited := s.degraded
	s.degraded = false
	s.mu.Unlock()
	if exited {
		s.log.Info("rate-limit store reachable — shared window limiting resumed", "route", s.route)
		if s.onDegraded != nil {
			s.onDegraded(s.route, false)
		}
	}
}
