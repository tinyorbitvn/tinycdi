// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/ratelimit"
)

// meStatus sends GET /v1/me with cookie and returns the status code.
func meStatus(t *testing.T, srv *httptest.Server, cookie *http.Cookie) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/me", nil)
	req.AddCookie(cookie)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("GET /v1/me: %v", err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// sprayUntilLimited sends forged cookies from the test client until the
// unknown-cookie budget refuses one with 429.
func sprayUntilLimited(t *testing.T, srv *httptest.Server) {
	t.Helper()
	for i := 0; i < 1000; i++ {
		c := &http.Cookie{Name: "__Host-tcdi_session", Value: fmt.Sprintf("forged-%d", i)}
		if meStatus(t, srv, c) == http.StatusTooManyRequests {
			return
		}
	}
	t.Fatal("the forged-cookie spray never spent the unknown-cookie budget")
}

// TestAppListener_LiveSessionSurvivesSharedKeySpray (R1-03 review): an
// attacker sharing the victim's client key (NAT, one IPv6 /64, a
// mis-set -trusted-proxies) spends the unknown-cookie budget with forged
// cookies. A signed-in user whose session this replica already confirmed
// live keeps working: confirmed sessions never draw on that budget.
func TestAppListener_LiveSessionSurvivesSharedKeySpray(t *testing.T) {
	counter := &lookupCounter{SessionStore: NewInMemorySessionStore(30 * time.Minute)}
	frozen := time.Unix(1_700_000_000, 0)
	srv := lookupSprayServer(t, counter, ratelimit.New(60, 10, 1000, func() time.Time { return frozen }))

	valid := saveSession(t, counter, "sess-shared-key")
	if got := meStatus(t, srv, valid); got != http.StatusOK {
		t.Fatalf("first /v1/me with a valid cookie = %d, want 200", got)
	}
	sprayUntilLimited(t, srv)
	for i := 0; i < 5; i++ {
		if got := meStatus(t, srv, valid); got != http.StatusOK {
			t.Fatalf("/v1/me #%d with a confirmed-live cookie after the spray = %d, want 200", i, got)
		}
	}
	// A cookie this replica never confirmed still pays: the budget is spent.
	if got := meStatus(t, srv, &http.Cookie{Name: "__Host-tcdi_session", Value: "forged-after"}); got != http.StatusTooManyRequests {
		t.Fatalf("/v1/me with a new forged cookie after the spray = %d, want 429", got)
	}
}

// TestAppListener_DeletedCachedSessionRefusedAndEvicted (R1-03 review):
// the live-session cache only exempts a cookie from the budget — it never
// authenticates on its own. A cached id whose row was deleted (logout,
// revocation, expiry) is refused by the store read, and the miss drops it
// from the cache, so its next request pays the (spent) budget again.
func TestAppListener_DeletedCachedSessionRefusedAndEvicted(t *testing.T) {
	counter := &lookupCounter{SessionStore: NewInMemorySessionStore(30 * time.Minute)}
	frozen := time.Unix(1_700_000_000, 0)
	srv := lookupSprayServer(t, counter, ratelimit.New(60, 10, 1000, func() time.Time { return frozen }))

	cookie := saveSession(t, counter, "sess-revoked")
	if got := meStatus(t, srv, cookie); got != http.StatusOK {
		t.Fatalf("first /v1/me = %d, want 200", got)
	}
	sprayUntilLimited(t, srv)
	if err := counter.Delete(context.Background(), cookie.Value); err != nil {
		t.Fatalf("delete session: %v", err)
	}
	before := counter.reads.Load()
	if got := meStatus(t, srv, cookie); got != http.StatusUnauthorized {
		t.Fatalf("/v1/me with a cached but deleted session = %d, want 401", got)
	}
	if counter.reads.Load() != before+1 {
		t.Fatal("a cached cookie skipped the store read; the cache must never substitute for it")
	}
	if got := meStatus(t, srv, cookie); got != http.StatusTooManyRequests {
		t.Fatalf("/v1/me with the deleted cookie after its miss = %d, want 429 (evicted, budget spent)", got)
	}
}

// TestLiveSessionCache_BoundedAndExpiring: the cache keeps at most max
// digests (least recently confirmed goes first), forgets an entry at its
// expiry, and forgets an entry on remove.
func TestLiveSessionCache_BoundedAndExpiring(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	c := newLiveSessionCache(2)
	c.add("a", now.Add(time.Minute))
	c.add("b", now.Add(time.Minute))
	c.add("c", now.Add(time.Minute))
	if c.contains("a", now) {
		t.Fatal("the oldest entry survived past the bound")
	}
	if !c.contains("b", now) || !c.contains("c", now) {
		t.Fatal("recent entries were dropped")
	}
	if c.contains("b", now.Add(time.Minute)) {
		t.Fatal("an entry outlived its expiry")
	}
	c.remove("c")
	if c.contains("c", now) {
		t.Fatal("remove left the entry")
	}
}
