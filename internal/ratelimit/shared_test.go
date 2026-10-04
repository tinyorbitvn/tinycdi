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
	"testing"
	"time"
)

// fakeWindowStore is the in-memory WindowStore double: per (route,key)
// counts, an injectable error, a fixed resetIn, and a call counter so
// tests can prove a locally-refused request never reached the store.
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
	l := NewShared(New(6000, 100, 100, nil), ws, "login", 5, nil, nil)

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
	l := NewShared(New(1, 2, 100, nil), ws, "login", 100, nil, nil)

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
// fires per failure; the log gets ONE entry line and ONE exit line, not
// a line per request.
func TestShared_FailOpen(t *testing.T) {
	ws := newFakeWindowStore()
	logs := &logBuf{}
	var storeErrs int
	l := NewShared(New(60, 10, 100, nil), ws, "launch", 80,
		testSharedLog(logs), func(string) { storeErrs++ })

	// Healthy: two window checks pass through the store.
	l.Allow("k")
	l.Allow("k")
	if got := ws.callCount(); got != 2 {
		t.Fatalf("store calls = %d, want 2", got)
	}
	if strings.Contains(logs.String(), "rate-limit store") {
		t.Fatalf("log written while healthy: %s", logs.String())
	}

	// Outage: every check fails — the requests still pass on the local
	// bucket, the metric counts each failure, but the entry warn logs once.
	ws.err = errors.New("connection refused")
	for i := 0; i < 5; i++ {
		if ok, _ := l.Allow("k"); !ok {
			t.Fatalf("store error %d refused — fail-open must admit within the local ceiling", i+1)
		}
	}
	if storeErrs != 5 {
		t.Fatalf("store error metric = %d, want 5 (counted per failure)", storeErrs)
	}
	if n := strings.Count(logs.String(), "enforcing local per-replica limit"); n != 1 {
		t.Fatalf("fallback entry logged %d times, want 1 (edge-triggered)", n)
	}
	if !strings.Contains(logs.String(), `"route":`) && !strings.Contains(logs.String(), "route=launch") {
		t.Fatalf("entry log missing route: %s", logs.String())
	}

	// Recovery: the first successful check logs the exit, once.
	ws.err = nil
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
	l := NewShared(New(1, 1, 100, nil), ws, "login", 100, nil, nil)

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

// TestShared_DisabledSkipsStore: a zero configured rate disables the
// limiter entirely — nothing is written to the window store.
func TestShared_DisabledSkipsStore(t *testing.T) {
	ws := newFakeWindowStore()
	l := NewShared(New(0, 0, 100, nil), ws, "login", 0, nil, nil)
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
	l := NewShared(New(60, 2, 100, nil), nil, "launch", 80, nil, nil)
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
