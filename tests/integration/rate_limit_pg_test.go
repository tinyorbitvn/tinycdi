//go:build integration

// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package integration

// ADR 0006 option B (+ RL-CEILING amendment): the login/launch rate
// limits are backed by a shared Postgres fixed-minute window (migration
// 021); while the store is healthy each pod's local ceiling is the
// undivided budget — a store-protection prefilter — and only in degraded
// mode does the divided bucket decide (the fail-open floor). These tests
// are the B6 gate: two backend replicas sharing one Postgres must admit
// one hammered key at most R+B times inside a window however uneven the
// spread, and a Postgres outage must degrade to the divided local
// limiter and back.

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tinyorbitvn/tinycdi/internal/store"
)

// loginHit issues one GET /v1/login against replica r keyed by clientIP
// (the replicas run -trusted-proxies=127.0.0.0/8, so the XFF value is the
// bucket key). 302 = admitted (redirect to the IdP), 429 = refused.
func loginHit(t *testing.T, r *replica, clientIP string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, r.appURL+"/v1/login", nil)
	if err != nil {
		t.Fatal(err)
	}
	if clientIP != "" {
		req.Header.Set("X-Forwarded-For", clientIP)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		t.Fatalf("GET /v1/login on %s: %v", r.name, err)
	}
	defer drainBody(resp)
	return resp.StatusCode
}

// loginHammer drives n login requests round-robin across reps on one key
// and counts admits vs rate-limit refusals. Any other status fails.
func loginHammer(t *testing.T, reps []*replica, clientIP string, n int) (allowed, denied int) {
	t.Helper()
	for i := 0; i < n; i++ {
		switch code := loginHit(t, reps[i%len(reps)], clientIP); code {
		case http.StatusFound:
			allowed++
		case http.StatusTooManyRequests:
			denied++
		default:
			t.Fatalf("GET /v1/login on %s: status %d, want 302 or 429", reps[i%len(reps)].name, code)
		}
	}
	return allowed, denied
}

// waitReplicaServing waits until the replica's app listener answers
// /healthz — handled by the readyz wrapper before the limiter, so a 200
// means the listener is up without spending a rate-limit token.
func waitReplicaServing(t *testing.T, r *replica) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		resp, err := r.client.Get(r.appURL + "/healthz")
		if err == nil {
			drainBody(resp)
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("replica %s never served /healthz", r.name)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// waitForFreshRateWindow blocks until at least minLeft of the database's
// current minute window remains, so a hammer cannot straddle a window
// boundary. The boundary is measured on the Postgres clock — the same
// clock the windows use. Transient errors are retried: a pool that just
// reconnected after an outage may surface one dead connection.
func waitForFreshRateWindow(t *testing.T, db *store.DB, minLeft time.Duration) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	var lastErr error
	for {
		var now time.Time
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		lastErr = db.Pool().QueryRow(ctx, `SELECT now()`).Scan(&now)
		cancel()
		if lastErr == nil {
			if left := now.Truncate(time.Minute).Add(time.Minute).Sub(now); left >= minLeft {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no fresh rate-limit window inside 90s (last err: %v)", lastErr)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// windowCount reads the key's counter in the current minute window (0
// when no row exists yet); transient read errors are retried for a few
// seconds so a reconnecting pool does not flake the test.
func windowCount(t *testing.T, db *store.DB, route, key string) int64 {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var n int64
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		err := db.Pool().QueryRow(ctx,
			`SELECT count FROM rate_limit_window
			 WHERE route = $1 AND bucket_key = $2
			   AND window_start = date_trunc('minute', now())`,
			route, key).Scan(&n)
		cancel()
		if err == nil {
			return n
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return 0
		}
		if time.Now().After(deadline) {
			t.Fatalf("window count %s/%s: %v", route, key, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// TestRateLimitWindow_HitAndSweep is the store contract: one upsert per
// check, per-(route,key) counters inside a fixed minute window, and the
// leader sweep deleting only expired rows.
func TestRateLimitWindow_HitAndSweep(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()

	// Repeated hits on one key count up inside the current window; the
	// reset hint stays inside (0, 60s] on the database clock.
	for want := int64(1); want <= 3; want++ {
		got, reset, err := db.RateLimitWindowHit(ctx, "login", "203.0.113.1")
		if err != nil {
			t.Fatalf("hit %d: %v", want, err)
		}
		if got != want {
			t.Fatalf("hit %d returned count %d", want, got)
		}
		if reset <= 0 || reset > time.Minute {
			t.Fatalf("resetIn = %v, want in (0, 60s]", reset)
		}
	}
	// A different key and a different route are independent counters.
	if got, _, err := db.RateLimitWindowHit(ctx, "login", "203.0.113.2"); err != nil || got != 1 {
		t.Fatalf("fresh key counted %d, %v — want 1", got, err)
	}
	if got, _, err := db.RateLimitWindowHit(ctx, "launch", "203.0.113.1"); err != nil || got != 1 {
		t.Fatalf("fresh route counted %d, %v — want 1", got, err)
	}

	// A row stamped in an older window never leaks into the current one:
	// seed a stale window for the same key — the hit still lands in the
	// current window's counter (4, continuing the earlier hits), and the
	// stale row is a separate row that only the sweep removes.
	if _, err := db.Pool().Exec(ctx, `
		INSERT INTO rate_limit_window (route, bucket_key, window_start, count)
		VALUES ('login', '203.0.113.1', now() - interval '1 hour', 9)`); err != nil {
		t.Fatalf("seed stale window: %v", err)
	}
	if got, _, err := db.RateLimitWindowHit(ctx, "login", "203.0.113.1"); err != nil || got != 4 {
		t.Fatalf("hit after stale window = %d, %v — want 4 in the current window (stale row is separate)", got, err)
	}
	var stale int64
	if err := db.Pool().QueryRow(ctx,
		`SELECT count FROM rate_limit_window
		 WHERE route = 'login' AND bucket_key = '203.0.113.1'
		   AND window_start < now() - interval '15 minutes'`).Scan(&stale); err != nil || stale != 9 {
		t.Fatalf("stale window row = %d, %v — want untouched count 9", stale, err)
	}

	// The sweep removes only expired rows (retention 15 min).
	n, err := db.SweepRateLimitWindows(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 1 {
		t.Fatalf("sweep deleted %d rows, want 1 (only the stale window)", n)
	}
	var remaining int64
	if err := db.Pool().QueryRow(ctx, `SELECT count(*) FROM rate_limit_window`).Scan(&remaining); err != nil {
		t.Fatalf("count windows: %v", err)
	}
	if remaining != 3 {
		t.Fatalf("%d window rows after sweep, want 3 live rows", remaining)
	}
}

// TestRateLimit_SharedWindowAcrossReplicas (B6 gate): two backend
// replicas share one Postgres window — one key driven at 3x the bound
// across both pods is admitted exactly R+B times inside a window. With
// -rate-limit-replicas=1 the healthy local ceiling equals the aggregate
// bound either way, so the shared window is what stops the second
// replica's traffic: without it the two pods would admit 2x(R+B).
func TestRateLimit_SharedWindowAcrossReplicas(t *testing.T) {
	f := newRestartFixture(t)
	extra := []string{"-login-rate", "5", "-rate-limit-replicas", "1", "-trusted-proxies", "127.0.0.0/8"}
	a := f.startReplicaWith(t, "a", extra...)
	defer a.cleanup(t)
	b := f.startReplicaWith(t, "b", extra...)
	defer b.cleanup(t)
	waitReplicaServing(t, a)
	waitReplicaServing(t, b)

	const ip = "203.0.113.50"
	const windowLimit = 5 + 20 // -login-rate + loginRateBurst
	waitForFreshRateWindow(t, f.db, 20*time.Second)
	allowed, denied := loginHammer(t, []*replica{a, b}, ip, 45)

	if allowed != windowLimit || denied != 45-windowLimit {
		t.Fatalf("shared window: allowed %d / denied %d, want %d/%d — the Postgres bound did not hold across replicas",
			allowed, denied, windowLimit, 45-windowLimit)
	}
	// Every admitted or store-refused hit landed in the one shared row;
	// locally-ceilinged refusals never reached the store.
	if got := windowCount(t, f.db, "login", ip); got < int64(windowLimit) {
		t.Fatalf("window count = %d, want >= %d", got, windowLimit)
	}
	t.Logf("shared window: %d allowed + %d refused across two replicas, window count %d",
		allowed, denied, windowCount(t, f.db, "login", ip))
}

// TestRateLimit_UnevenSplitUsesWindowBound (RL-CEILING regression): the
// E2E-V050-RAMP run-3 finding — a 20-lane sign-in ramp split 11/9 across
// two pods drew a local 429 on the busier pod at an aggregate of 19/80
// because the healthy local ceiling was the DIVIDED share. Now the
// healthy ceiling is the undivided budget (a store-protection prefilter)
// and only the window binds: -login-rate 5 + burst 20 → bound 25, two
// replicas (floor 2/min + burst 10 each). A 15/5 split admits all 20 —
// the old divided ceiling would have refused pod a's 11th hit — a 20/0
// split is likewise served by one pod alone, and the window itself
// refuses the hit past 25.
func TestRateLimit_UnevenSplitUsesWindowBound(t *testing.T) {
	f := newRestartFixture(t)
	extra := []string{"-login-rate", "5", "-rate-limit-replicas", "2", "-trusted-proxies", "127.0.0.0/8"}
	a := f.startReplicaWith(t, "a", extra...)
	defer a.cleanup(t)
	b := f.startReplicaWith(t, "b", extra...)
	defer b.cleanup(t)
	waitReplicaServing(t, a)
	waitReplicaServing(t, b)

	waitForFreshRateWindow(t, f.db, 20*time.Second)
	const ip = "203.0.113.60"
	// 15 hits on pod a + 5 on pod b: every hit must pass — under the
	// divided ceiling pod a's burst would have been 10.
	for i := 0; i < 15; i++ {
		if code := loginHit(t, a, ip); code != http.StatusFound {
			t.Fatalf("pod-a hit %d of the 15/5 split = %d, want 302 — a 429 here is the divided-ceiling regression", i+1, code)
		}
	}
	for i := 0; i < 5; i++ {
		if code := loginHit(t, b, ip); code != http.StatusFound {
			t.Fatalf("pod-b hit %d of the 15/5 split = %d, want 302", i+1, code)
		}
	}
	// The next 5 hits on pod b reach the bound (25); the 26th is refused
	// by the window's own count — not by a pod's share (pod b's ceiling
	// still has burst left).
	for i := 0; i < 5; i++ {
		if code := loginHit(t, b, ip); code != http.StatusFound {
			t.Fatalf("hit %d of 25 = %d, want 302 — the bound is exactly rate+burst", 21+i, code)
		}
	}
	if code := loginHit(t, b, ip); code != http.StatusTooManyRequests {
		t.Fatalf("26th aggregate hit = %d, want 429 — the window bound did not hold", code)
	}
	if got := windowCount(t, f.db, "login", ip); got != 26 {
		t.Fatalf("window count = %d, want 26 — every admitted/refused hit reached the store", got)
	}

	// 20/0 on a fresh key in the same window: one pod alone serves the
	// whole burst — the ceiling is undivided.
	const ipSolo = "203.0.113.61"
	for i := 0; i < 20; i++ {
		if code := loginHit(t, a, ipSolo); code != http.StatusFound {
			t.Fatalf("single-pod hit %d of the 20/0 split = %d, want 302", i+1, code)
		}
	}
	if got := windowCount(t, f.db, "login", ipSolo); got != 20 {
		t.Fatalf("window count = %d, want 20", got)
	}
}

// TestRateLimit_PGOutageFailsOpenAndRecovers (B6 gate, G1): with Postgres
// down the limiters fail open to the divided local buckets — bounded by
// each pod's 1/N share, never lifted — and the shared window resumes on
// recovery. Entry and exit are edge-triggered in the replica logs and
// counted on tinycdi_rate_limit_store_errors_total.
func TestRateLimit_PGOutageFailsOpenAndRecovers(t *testing.T) {
	f, pg := newOutageFixture(t)
	// login-rate 2 + burst 20 → shared window bound 22 and undivided
	// healthy ceilings of 2/min + burst 20; replicas 2 → the
	// degraded-mode floor is 1/min + burst 10 per pod. Only replica a
	// gets a metrics listener — two in-process backends share the
	// default Prometheus registry, so a second set would double-register.
	flags := func(extra ...string) []string {
		return append([]string{"-login-rate", "2", "-rate-limit-replicas", "2",
			"-trusted-proxies", "127.0.0.0/8"}, extra...)
	}
	a, logsA := f.startReplicaLogged(t, "a", flags("-metrics-listen", "127.0.0.1:0")...)
	defer a.cleanup(t)
	b, _ := f.startReplicaLogged(t, "b", flags()...)
	defer b.cleanup(t)
	waitReplicaServing(t, a)
	waitReplicaServing(t, b)

	// Baseline: one key hammered across both pods is bounded by the
	// shared window (22) — deterministic now that the healthy ceiling is
	// the undivided budget (burst 20/pod covers the 15-hit half), so all
	// 30 requests reach the store and it refuses exactly hits 23-30.
	waitForFreshRateWindow(t, f.db, 20*time.Second)
	allowed, denied := loginHammer(t, []*replica{a, b}, "203.0.113.10", 30)
	if allowed != 22 || denied != 8 {
		t.Fatalf("healthy shared window allowed %d / denied %d, want 22/8 (window bound 22)", allowed, denied)
	}
	if base := windowCount(t, f.db, "login", "203.0.113.10"); base < int64(allowed) {
		t.Fatalf("window count %d < allowed %d — the store did not count admitted hits", base, allowed)
	}

	// Outage: docker stop returns once the container is down, so every
	// later check fails fast (connection refused). A fresh key hammered
	// on ONE pod gets exactly that pod's divided share — fail-open, not
	// fail-closed and not the shared window's 22. Replica b sees no
	// traffic during the outage, so only a's log carries the entry line.
	pg.stop(t)
	allowed, denied = loginHammer(t, []*replica{a}, "203.0.113.20", 20)
	if allowed < 1 {
		t.Fatalf("outage allowed 0 — the limiter must fail OPEN to the local bucket")
	}
	if allowed > 12 {
		t.Fatalf("outage allowed %d on one pod — above the divided local ceiling (1/min + burst 10)", allowed)
	}
	if denied < 8 {
		t.Fatalf("outage denied %d of 20 — the local ceiling did not bound the flood", denied)
	}
	if !strings.Contains(logsA.String(), "enforcing local per-replica limit") {
		t.Fatalf("replica a never logged fallback entry:\n%s", logsA.String())
	}

	// Recovery: the outage's first failure opened the limiter's circuit
	// for a 10 s cool-down, so wait it out — only then does a request
	// probe the store and close the circuit. In the next window the
	// shared counter is authoritative again — new hits land in Postgres
	// and the exit line logs once.
	pg.start(t)
	time.Sleep(11 * time.Second) // store circuit cool-down (10 s) + slack
	waitForFreshRateWindow(t, f.db, 20*time.Second)
	allowed, _ = loginHammer(t, []*replica{a, b}, "203.0.113.30", 30)
	// Normally exactly 22 (the window bound). The slack above covers a
	// fail-open leg: a first post-reconnect check that meets a dead
	// pooled connection admits via the ceiling and opens the circuit,
	// and requests inside that cool-down are bounded by the divided
	// floor (~11/pod) instead of the window — so a fully-unlucky hammer
	// can legitimately admit up to ~22 floor + a couple of fail-opens.
	if allowed < 15 || allowed > 26 {
		t.Fatalf("post-recovery allowed %d, want 15..26 (window bound 22 + floor-bounded fail-opens)", allowed)
	}
	// Hits land in Postgres again. The slack covers the first
	// post-reconnect requests, which can legitimately fail open on a dead
	// pooled connection — counted in `allowed` but never written.
	if got := windowCount(t, f.db, "login", "203.0.113.30"); got < int64(allowed)/2 {
		t.Fatalf("post-recovery window count %d vs allowed %d — hits are not reaching Postgres again", got, allowed)
	}
	// The exit line logs on replica a's first successful probe — keep
	// hitting so a probe that met a dead pooled connection gets its next
	// cool-down's chance inside the bound.
	deadline := time.Now().Add(45 * time.Second)
	for !strings.Contains(logsA.String(), "shared window limiting resumed") {
		if time.Now().After(deadline) {
			t.Fatalf("replica a never logged fallback exit:\n%s", logsA.String())
		}
		loginHit(t, a, "203.0.113.31")
		time.Sleep(time.Second)
	}

	// The store-error counter exists on the metrics listener with the
	// bounded route label, and the refusal metric kept its labels.
	_, _, _, mAddr := a.b.Addrs()
	if mAddr == "" {
		t.Fatal("replica a has no metrics listener address")
	}
	resp, err := a.client.Get("http://" + mAddr + "/metrics")
	if err != nil {
		t.Fatalf("scrape metrics: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	drainBody(resp)
	if err != nil {
		t.Fatalf("read metrics: %v", err)
	}
	metrics := string(body)
	if !strings.Contains(metrics, `tinycdi_rate_limit_store_errors_total{route="login"}`) {
		t.Fatalf("rate_limit_store_errors_total{route=login} absent from /metrics")
	}
	if !strings.Contains(metrics, `tinycdi_rate_limited_total{route="/v1/login"}`) {
		t.Fatalf("rate_limited_total{route=/v1/login} absent — refusal metric label regressed")
	}
}
