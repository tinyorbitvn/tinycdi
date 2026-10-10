// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package gateway_test

// The SessionLookupLimiter injection point: a configured limiter decides
// the unknown-cookie bound, and a bucket that always allows disables it.

import (
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/gateway"
	"github.com/tinyorbitvn/tinycdi/internal/ratelimit"
)

// TestRehydrate_InjectedLookupLimiter: a tight injected bucket refuses
// after its burst with the limiter's own Retry-After hint and spends no
// directory lookup on refusals; the frozen clock then admits again.
func TestRehydrate_InjectedLookupLimiter(t *testing.T) {
	fb := newFakeBroker(t)
	now := time.Now()
	lim := ratelimit.New(60, 2, 100, func() time.Time { return now }) // 1/s, burst 2
	_, srv := newGatewayHandle(t, fb, func(c *gateway.Config) {
		c.Sessions = fb
		c.SessionLookupLimiter = lim
	})

	// Two unknown cookies pass the burst — each a 401 with one lookup.
	for i := 0; i < 2; i++ {
		resp := proxied(t, srv, testHost, "/", "ck-"+strconv.Itoa(i), nil)
		drain(resp)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("unknown cookie %d = %d, want 401", i, resp.StatusCode)
		}
	}
	if n := fb.lookupCount(); n != 2 {
		t.Fatalf("LeaseBySession calls = %d, want 2", n)
	}
	// The third is refused before any directory read.
	resp := proxied(t, srv, testHost, "/", "ck-2", nil)
	defer drain(resp)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("burst-exceeding unknown cookie = %d, want 429", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Fatal("429 on the lookup path carries no Retry-After")
	}
	if n := fb.lookupCount(); n != 2 {
		t.Fatalf("LeaseBySession calls = %d after the refusal, want 2 — a 429 spends no lookup", n)
	}
	// Refill and the next lookup passes.
	now = now.Add(2 * time.Second)
	resp2 := proxied(t, srv, testHost, "/", "ck-3", nil)
	defer drain(resp2)
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("post-refill unknown cookie = %d, want 401", resp2.StatusCode)
	}
	if n := fb.lookupCount(); n != 3 {
		t.Fatalf("LeaseBySession calls = %d after refill, want 3", n)
	}
}

// TestRehydrate_LookupLimiterDisabled: an Allower that always allows
// restores the pre-bound behaviour — every unseen cookie reaches the
// directory.
func TestRehydrate_LookupLimiterDisabled(t *testing.T) {
	fb := newFakeBroker(t)
	_, srv := newGatewayHandle(t, fb, func(c *gateway.Config) {
		c.Sessions = fb
		c.SessionLookupLimiter = ratelimit.New(0, 0, 1, nil) // rate 0 disables
	})
	for i := 0; i < 10; i++ {
		resp := proxied(t, srv, testHost, "/", "d-"+strconv.Itoa(i), nil)
		drain(resp)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("unknown cookie %d with disabled limiter = %d, want 401", i, resp.StatusCode)
		}
	}
	if n := fb.lookupCount(); n != 10 {
		t.Fatalf("LeaseBySession calls = %d, want 10", n)
	}
}
