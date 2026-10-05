// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeWindowStore is the in-memory WindowStore double: per (route,key)
// counts, an injectable error, a fixed resetIn, and a call counter so
// tests can prove a locally-refused or circuit-skipped request never
// reached the store.
type fakeWindowStore struct {
	mu     sync.Mutex
	counts map[string]int64
	reset  time.Duration
	err    error
	calls  int
}

func newFakeWindowStore() *fakeWindowStore {
	return &fakeWindowStore{counts: map[string]int64{}, reset: 30 * time.Second}
}

func (f *fakeWindowStore) RateLimitWindowHit(_ context.Context, route, key string) (int64, time.Duration, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return 0, 0, f.err
	}
	f.counts[route+"/"+key]++
	return f.counts[route+"/"+key], f.reset, nil
}

func (f *fakeWindowStore) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// sleepyStore is the slow-but-alive store: it honours the call context
// (like pgx) and only answers once the deadline cancels the query.
type sleepyStore struct {
	calls atomic.Int32
}

func (s *sleepyStore) RateLimitWindowHit(ctx context.Context, _, _ string) (int64, time.Duration, error) {
	s.calls.Add(1)
	<-ctx.Done()
	return 0, 0, ctx.Err()
}

// blockedStore parks every check until release closes — the in-flight
// probe in the single-flight test.
type blockedStore struct {
	calls   atomic.Int32
	release chan struct{}
}

func (b *blockedStore) RateLimitWindowHit(ctx context.Context, _, _ string) (int64, time.Duration, error) {
	b.calls.Add(1)
	select {
	case <-b.release:
		return 1, 30 * time.Second, nil
	case <-ctx.Done():
		return 0, 0, ctx.Err()
	}
}

// panickyStore panics on every check until healed — a store double for
// the probe-panic case: the recovered panic must release the probe slot
// so the next cool-down probes again instead of wedging the breaker.
type panickyStore struct {
	calls  atomic.Int32
	hits   atomic.Int64
	healed atomic.Bool
}

func (p *panickyStore) RateLimitWindowHit(context.Context, string, string) (int64, time.Duration, error) {
	p.calls.Add(1)
	if !p.healed.Load() {
		panic("store exploded")
	}
	return p.hits.Add(1), 30 * time.Second, nil
}

// fakeClock is a hand-advanced clock for the breaker cool-down.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Unix(1_700_000_000, 0)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// logBuf captures slog output for the edge-triggered fallback assertions.
type logBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *logBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func testSharedLog(l *logBuf) *slog.Logger {
	return slog.New(slog.NewTextHandler(l, nil))
}

// TestShared_WindowBound: the shared window admits exactly limit hits —
// count <= limit passes, count > limit is refused with the store's
// reported reset as Retry-After, and a fresh window resets the budget.
func TestShared_WindowBound(t *testing.T) {
	ws := newFakeWindowStore()
	l := NewShared(New(6000, 100, 100, nil), ws, "login", 5, nil, nil, nil)

	for i := 0; i < 5; i++ {
		if ok, _ := l.Allow("k"); !ok {
			t.Fatalf("hit %d refused — window count %d <= limit 5 must pass", i+1, i+1)
		}
	}
	ok, retry := l.Allow("k")
	if ok {
		t.Fatal("6th hit allowed — count 6 > limit 5 must be refused")
	}
	if retry != 30*time.Second {
		t.Fatalf("retryAfter = %v, want the store's reset 30s", retry)
	}
	if got := RetryAfterSeconds(retry); got != 30 {
		t.Fatalf("Retry-After = %ds, want 30", got)
	}
	// A new minute window resets the counter: the same key is admitted.
	ws.mu.Lock()
	ws.counts = map[string]int64{}
	ws.mu.Unlock()
	if ok, _ := l.Allow("k"); !ok {
		t.Fatal("first hit of a fresh window refused")
	}
}

// TestShared_LocalCeilingFirst: the divided local bucket is the
// per-replica ceiling — its refusal is final and never spends a store
// write (min(shared, local), and the local ceiling bounds the upsert
// rate a key spray can cause).
func TestShared_LocalCeilingFirst(t *testing.T) {
	ws := newFakeWindowStore()
	// Local bucket: 1 token/min, burst 2 — exhausts after 2 hits. The
	// store would still have room (limit 100), so a denial can only
	// have come from the local ceiling.
	l := NewShared(New(1, 2, 100, nil), ws, "login", 100, nil, nil, nil)

	if ok, _ := l.Allow("k"); !ok {
		t.Fatal("first hit refused")
	}
	if ok, _ := l.Allow("k"); !ok {
		t.Fatal("second hit refused")
	}
	if ok, _ := l.Allow("k"); ok {
		t.Fatal("third hit allowed past the local ceiling — min() violated")
	}
	if got := ws.callCount(); got != 2 {
		t.Fatalf("store calls = %d, want 2 — a local refusal must not write", got)
	}
}

// TestShared_FailOpen: a store error degrades the check to the local
// bucket — never a lifted cap, never a hard refusal. The error metric
// counts REAL store failures only (requests skipped inside the
// cool-down never reach the store); the log gets ONE entry line and ONE
// exit line, not a line per request.
func TestShared_FailOpen(t *testing.T) {
	ws := newFakeWindowStore()
	logs := &logBuf{}
	var storeErrs int
	clock := newFakeClock()
	l := NewShared(New(60, 10, 100, nil), ws, "launch", 80,
		testSharedLog(logs), func(string) { storeErrs++ }, nil)
	l.now = clock.Now

	// Healthy: two window checks pass through the store.
	l.Allow("k")
	l.Allow("k")
	if got := ws.callCount(); got != 2 {
		t.Fatalf("store calls = %d, want 2", got)
	}
	if strings.Contains(logs.String(), "rate-limit store") {
		t.Fatalf("log written while healthy: %s", logs.String())
	}

	// Outage: the first check fails and opens the circuit — the rest of
	// the cool-down skips the store and answers on the local bucket, so
	// five outage requests count ONE real store error and log one entry.
	ws.err = errors.New("connection refused")
	for i := 0; i < 5; i++ {
		if ok, _ := l.Allow("k"); !ok {
			t.Fatalf("store error %d refused — fail-open must admit within the local ceiling", i+1)
		}
	}
	if storeErrs != 1 {
		t.Fatalf("store error metric = %d, want 1 (skipped checks never count)", storeErrs)
	}
	if got := ws.callCount(); got != 3 {
		t.Fatalf("store calls = %d, want 3 — the open circuit must skip the store", got)
	}
	if n := strings.Count(logs.String(), "enforcing local per-replica limit"); n != 1 {
		t.Fatalf("fallback entry logged %d times, want 1 (edge-triggered)", n)
	}
	if !strings.Contains(logs.String(), `"route":`) && !strings.Contains(logs.String(), "route=launch") {
		t.Fatalf("entry log missing route: %s", logs.String())
	}

	// Recovery: the first probe past the cool-down logs the exit, once.
	ws.err = nil
	clock.Advance(storeCooldown + time.Second)
	l.Allow("k")
	l.Allow("k")
	if n := strings.Count(logs.String(), "shared window limiting resumed"); n != 1 {
		t.Fatalf("recovery logged %d times, want 1 (edge-triggered)", n)
	}

	// A second outage enters degraded mode again — a fresh entry line.
	ws.err = errors.New("connection refused")
	l.Allow("k")
	if n := strings.Count(logs.String(), "enforcing local per-replica limit"); n != 2 {
		t.Fatalf("second outage logged %d entries, want 2", n)
	}
}

// TestShared_FailOpenHonoursLocalDeny: during a store outage a key past
// its local ceiling is still refused — degraded mode is the divided
// limiter, not an open gate.
func TestShared_FailOpenHonoursLocalDeny(t *testing.T) {
	ws := newFakeWindowStore()
	ws.err = errors.New("down")
	l := NewShared(New(1, 1, 100, nil), ws, "login", 100, nil, nil, nil)

	if ok, _ := l.Allow("k"); !ok {
		t.Fatal("first hit refused during outage — local burst must still serve")
	}
	ok, retry := l.Allow("k")
	if ok {
		t.Fatal("second hit allowed past the exhausted local bucket")
	}
	if retry <= 0 {
		t.Fatal("local denial returned no Retry-After")
	}
}

// TestShared_BrownOutStoreDeadline: a store that is alive but stalls
// every query must not stall the limited surface. The per-call deadline
// bounds the first hit to ~storeCallTimeout, the opened circuit keeps
// every later request inside the cool-down off the store entirely, and
// after the cool-down exactly one request probes — it pays the deadline
// once and reopens the circuit.
func TestShared_BrownOutStoreDeadline(t *testing.T) {
	ws := &sleepyStore{}
	clock := newFakeClock()
	l := NewShared(New(6000, 100, 100, nil), ws, "login", 5, nil, nil, nil)
	l.now = clock.Now

	start := time.Now()
	if ok, _ := l.Allow("k"); !ok {
		t.Fatal("stalled store hit refused — must fail open")
	}
	if d := time.Since(start); d > 2*storeCallTimeout {
		t.Fatalf("brown-out check took %v — the %v deadline did not fire", d, storeCallTimeout)
	}
	// Cool-down: ten requests answer on the local bucket with no store
	// contact at all — each far under the deadline.
	for i := 0; i < 10; i++ {
		start = time.Now()
		if ok, _ := l.Allow("k"); !ok {
			t.Fatalf("cool-down hit %d refused — the local verdict must stand", i+1)
		}
		if d := time.Since(start); d > storeCallTimeout {
			t.Fatalf("cool-down hit %d stalled %v — the store must be skipped", i+1, d)
		}
	}
	if got := ws.calls.Load(); got != 1 {
		t.Fatalf("store calls = %d, want 1 — the open circuit must not touch the store", got)
	}
	// Cool-down lapses: the single probe pays the deadline once (the
	// store is still slow) and reopens the circuit — the next requests
	// skip the store again.
	clock.Advance(storeCooldown + time.Second)
	start = time.Now()
	if ok, _ := l.Allow("k"); !ok {
		t.Fatal("probe refused — must fail open")
	}
	if d := time.Since(start); d > 2*storeCallTimeout {
		t.Fatalf("probe took %v — the %v deadline did not fire", d, storeCallTimeout)
	}
	for i := 0; i < 5; i++ {
		l.Allow("k")
	}
	if got := ws.calls.Load(); got != 2 {
		t.Fatalf("store calls = %d, want 2 — only the single probe touched the store", got)
	}
}

// TestShared_CircuitProbeRestoresShared: once the store heals, the first
// probe after the cool-down closes the circuit and the shared window is
// authoritative again — including inside the cool-down where a healed
// store is still skipped until the probe.
func TestShared_CircuitProbeRestoresShared(t *testing.T) {
	ws := newFakeWindowStore()
	clock := newFakeClock()
	l := NewShared(New(6000, 100, 100, nil), ws, "login", 5, nil, nil, nil)
	l.now = clock.Now

	ws.err = errors.New("down")
	l.Allow("k") // fails — opens the circuit
	for i := 0; i < 5; i++ {
		l.Allow("k")
	}
	if got := ws.callCount(); got != 1 {
		t.Fatalf("store calls = %d, want 1 — cool-down checks must skip the store", got)
	}

	// The store heals mid-cool-down: requests still skip it until a
	// probe — recovery is probe-triggered, not error-triggered.
	ws.err = nil
	l.Allow("k")
	if got := ws.callCount(); got != 1 {
		t.Fatalf("store calls = %d — a healed store is still skipped inside the cool-down", got)
	}
	clock.Advance(storeCooldown + time.Second)
	if ok, _ := l.Allow("k"); !ok {
		t.Fatal("recovery probe refused")
	}
	if got := ws.callCount(); got != 2 {
		t.Fatalf("store calls = %d, want 2 — the first probe after cool-down", got)
	}

	// The shared window is authoritative again: the probe was hit 1 of
	// 5, four more pass and the sixth is refused by the store's count.
	for i := 0; i < 4; i++ {
		if ok, _ := l.Allow("k"); !ok {
			t.Fatalf("hit %d refused — shared window must be live again", i+2)
		}
	}
	if ok, _ := l.Allow("k"); ok {
		t.Fatal("6th shared hit allowed — the window bound was lost after recovery")
	}
}

// TestShared_ProbeIsSingleFlight: with the circuit open and the
// cool-down lapsed, exactly ONE concurrent request probes the store —
// the rest take the local verdict while the probe is in flight. Run
// under -race: the probe flag must be race-free.
func TestShared_ProbeIsSingleFlight(t *testing.T) {
	ws := newFakeWindowStore()
	ws.err = errors.New("down")
	clock := newFakeClock()
	l := NewShared(New(6000, 100, 100, nil), ws, "login", 100, nil, nil, nil)
	l.now = clock.Now

	l.Allow("k") // fails — opens the circuit
	blocked := &blockedStore{release: make(chan struct{})}
	l.store = blocked
	clock.Advance(storeCooldown + time.Second)

	// The first request past the cool-down becomes THE probe and parks
	// in the store; requests during its flight must not touch the store.
	probeDone := make(chan bool, 1)
	go func() {
		ok, _ := l.Allow("k")
		probeDone <- ok
	}()
	deadline := time.Now().Add(5 * time.Second)
	for blocked.calls.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("probe never reached the store")
		}
		time.Sleep(time.Millisecond)
	}
	const concurrent = 16
	var wg sync.WaitGroup
	results := make(chan bool, concurrent)
	for i := 0; i < concurrent; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, _ := l.Allow("k")
			results <- ok
		}()
	}
	wg.Wait()
	close(results)
	for ok := range results {
		if !ok {
			t.Fatal("concurrent request refused during the probe — the local verdict must stand")
		}
	}
	if got := blocked.calls.Load(); got != 1 {
		t.Fatalf("store calls = %d, want 1 — the probe is single-flight", got)
	}
	close(blocked.release)
	if ok := <-probeDone; !ok {
		t.Fatal("probe refused on a healthy store")
	}
	// Probe success closed the circuit: the next request uses the store.
	if ok, _ := l.Allow("k"); !ok {
		t.Fatal("post-recovery hit refused")
	}
	if got := blocked.calls.Load(); got != 2 {
		t.Fatalf("store calls = %d, want 2 — the circuit stayed open after a good probe", got)
	}
}

// TestShared_DisabledSkipsStore: a zero configured rate disables the
// limiter entirely — nothing is written to the window store.
func TestShared_DisabledSkipsStore(t *testing.T) {
	ws := newFakeWindowStore()
	l := NewShared(New(0, 0, 100, nil), ws, "login", 0, nil, nil, nil)
	for i := 0; i < 10; i++ {
		if ok, _ := l.Allow(fmt.Sprintf("k%d", i)); !ok {
			t.Fatal("disabled limiter refused")
		}
	}
	if got := ws.callCount(); got != 0 {
		t.Fatalf("store calls = %d on a disabled limiter, want 0", got)
	}
}

// TestShared_NilStoreIsLocalOnly: split mode wires no WindowStore — the
// limiter then IS the divided local bucket.
func TestShared_NilStoreIsLocalOnly(t *testing.T) {
	l := NewShared(New(60, 2, 100, nil), nil, "launch", 80, nil, nil, nil)
	if ok, _ := l.Allow("k"); !ok {
		t.Fatal("first hit refused")
	}
	if ok, _ := l.Allow("k"); !ok {
		t.Fatal("second hit refused")
	}
	if ok, _ := l.Allow("k"); ok {
		t.Fatal("third hit allowed past the local-only bucket")
	}
}

// TestShared_PanicProbeReprobes: a panic inside the store check must not
// wedge the breaker. The recovered panic counts as a store error and
// re-arms a full cool-down — crucially it releases the probe slot, so
// the next cool-down probes again (a stuck probing flag would leave the
// call count frozen and the limiter degraded forever), and once the
// store heals the probe closes the circuit normally.
func TestShared_PanicProbeReprobes(t *testing.T) {
	ws := &panickyStore{}
	logs := &logBuf{}
	var storeErrs int
	degraded := false
	clock := newFakeClock()
	l := NewShared(New(6000, 100, 100, nil), ws, "login", 5,
		testSharedLog(logs), func(string) { storeErrs++ },
		func(_ string, d bool) { degraded = d })
	l.now = clock.Now

	// The very first check panics: the request fails open, the panic is
	// logged once and counted as a store error, and the circuit opens.
	if ok, _ := l.Allow("k"); !ok {
		t.Fatal("panicking store refused — must fail open")
	}
	if got := ws.calls.Load(); got != 1 {
		t.Fatalf("store calls = %d, want 1", got)
	}
	if storeErrs != 1 {
		t.Fatalf("store errors = %d, want 1 — a panic counts as a store error", storeErrs)
	}
	if !degraded {
		t.Fatal("circuit did not open on a panicking check")
	}
	if n := strings.Count(logs.String(), "panic="); n != 1 {
		t.Fatalf("panic logged %d times, want 1", n)
	}

	// Cool-down requests skip the store entirely.
	for i := 0; i < 5; i++ {
		l.Allow("k")
	}
	if got := ws.calls.Load(); got != 1 {
		t.Fatalf("store calls = %d, want 1 — cool-down must skip the store", got)
	}

	// The first probe past the cool-down panics too. The recover must
	// release the probe slot and re-arm a FULL cool-down: requests right
	// after it stay local, and the NEXT cool-down probes again — calls
	// reaching 3 proves probing did not wedge.
	clock.Advance(storeCooldown + time.Second)
	l.Allow("k") // probe — panics
	for i := 0; i < 3; i++ {
		l.Allow("k")
	}
	if got := ws.calls.Load(); got != 2 {
		t.Fatalf("store calls = %d, want 2 — one probe per cool-down, then skip", got)
	}
	clock.Advance(storeCooldown + time.Second)
	l.Allow("k") // second probe — panics again
	if got := ws.calls.Load(); got != 3 {
		t.Fatalf("store calls = %d, want 3 — the panic must release the probe slot", got)
	}
	if storeErrs != 3 {
		t.Fatalf("store errors = %d, want 3 — every panicking check counts", storeErrs)
	}
	if !degraded {
		t.Fatal("circuit closed while the store still panics")
	}

	// Heal the store: the next cool-down's probe succeeds, the breaker
	// closes (exit log + gauge) and the shared window is authoritative —
	// hits 2..5 pass, the 6th is refused by the store's count.
	ws.healed.Store(true)
	clock.Advance(storeCooldown + time.Second)
	if ok, _ := l.Allow("k"); !ok {
		t.Fatal("recovery probe refused on a healed store")
	}
	if got := ws.calls.Load(); got != 4 {
		t.Fatalf("store calls = %d, want 4 — the recovery probe", got)
	}
	if degraded {
		t.Fatal("gauge still degraded after a successful probe")
	}
	if n := strings.Count(logs.String(), "shared window limiting resumed"); n != 1 {
		t.Fatalf("recovery logged %d times, want 1", n)
	}
	for i := 0; i < 4; i++ {
		if ok, _ := l.Allow("k"); !ok {
			t.Fatalf("hit %d refused — shared window must be live again", i+2)
		}
	}
	if ok, _ := l.Allow("k"); ok {
		t.Fatal("6th shared hit allowed — the window bound was lost after recovery")
	}
}
