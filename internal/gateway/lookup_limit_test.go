// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package gateway_test

// Session-directory lookup limiting: a request carrying a cookie this
// replica has never seen is the only proxy-path request that costs
// Postgres reads before auth (a digest lookup plus an EXISTS probe on
// miss), and unique cookie values mint a fresh singleflight entry each —
// so an unauthenticated spray converts request rate directly into
// indexed reads. A per-client LOCAL bound on the unknown-cookie path
// answers 429 + Retry-After before any store read. Requests without a
// cookie, cookies a live local session owns and rehydrated sessions are
// never limited.

import (
	"net/http"
	"strconv"
	"testing"
)

// TestRehydrate_UnknownCookieSprayBounded: hundreds of distinct random
// cookies from one client must not each reach the session directory —
// the per-client bound turns the tail into 429s carrying Retry-After
// while the directory sees only the bounded prefix. Fails on v0.5.0:
// every sprayed cookie lands its lookup and every answer is 401.
func TestRehydrate_UnknownCookieSprayBounded(t *testing.T) {
	fb := newFakeBroker(t)
	gwB, srvB := newReplica(t, fb, "gw-B")

	// Well past the default per-client bound (300/min + burst 60).
	const requests = 420
	refused, retryAfter := 0, 0
	for i := 0; i < requests; i++ {
		resp := proxied(t, srvB, testHost, "/", "spray-"+strconv.Itoa(i), nil)
		switch resp.StatusCode {
		case http.StatusTooManyRequests:
			refused++
			if resp.Header.Get("Retry-After") != "" {
				retryAfter++
			}
		case http.StatusUnauthorized:
		default:
			drain(resp)
			t.Fatalf("spray request %d = %d, want 401 or 429", i, resp.StatusCode)
		}
		drain(resp)
	}
	if refused == 0 {
		t.Fatalf("no refusal in %d unknown-cookie requests — the spray reached the store unbounded", requests)
	}
	if retryAfter != refused {
		t.Fatalf("%d of %d refusals carried no Retry-After", refused-retryAfter, refused)
	}
	// Only the allowed prefix may reach the directory — the refusal tail
	// must never spend a lookup.
	if n := fb.lookupCount(); n > requests/2 {
		t.Fatalf("LeaseBySession calls = %d for %d sprayed cookies — the bound did not hold", n, requests)
	}
	// A refused lookup allocates nothing, exactly like a clean miss.
	if s, l, w, i := gwB.SessionMapCounts(); s+l+w+i != 0 {
		t.Fatalf("session state after the spray: sessions=%d byLease=%d byWorkspace=%d inflight=%d, want all 0",
			s, l, w, i)
	}
}

// TestRehydrate_LimiterSkipsKnownSession: the bound guards ONLY the
// unknown-cookie path — a cookie the replica already holds is an
// in-memory hit and must stay served with the client bucket drained.
func TestRehydrate_LimiterSkipsKnownSession(t *testing.T) {
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-known", testWSUID)
	_, srvA := newReplica(t, fb, "gw-A")
	cookie := launchOK(t, srvA, testHost, "tk-known")

	// Drain the per-client bucket with unknown cookies.
	for i := 0; i < 400; i++ {
		resp := proxied(t, srvA, testHost, "/", "flood-"+strconv.Itoa(i), nil)
		drain(resp)
	}
	// The live session's own requests never consult the limiter.
	resp := proxied(t, srvA, testHost, "/", cookie, nil)
	defer drain(resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("request on a known session = %d, want 200 — the lookup bound must not touch it", resp.StatusCode)
	}
}

// TestRehydrate_LimiterSkipsRehydratedSession: a session rebuilt on this
// replica is known from then on — requests after the first pay no lookup
// and are never limited.
func TestRehydrate_LimiterSkipsRehydratedSession(t *testing.T) {
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-rehy", testWSUID)
	_, srvA := newReplica(t, fb, "gw-A")
	_, srvB := newReplica(t, fb, "gw-B")
	cookie := launchOK(t, srvA, testHost, "tk-rehy")

	// B rebuilds the session once, then drains the client bucket on
	// unknown cookies.
	resp := proxied(t, srvB, testHost, "/", cookie, nil)
	drain(resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("rehydrating request = %d, want 200", resp.StatusCode)
	}
	for i := 0; i < 400; i++ {
		r := proxied(t, srvB, testHost, "/", "junk-"+strconv.Itoa(i), nil)
		drain(r)
	}
	resp = proxied(t, srvB, testHost, "/", cookie, nil)
	defer drain(resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("request on the rehydrated session = %d, want 200 — it is a local hit, not a lookup", resp.StatusCode)
	}
}

// TestRehydrate_LimiterSkipsNoCookie: a request without a session cookie
// never reaches the limiter — it stays a plain 401 even with the bucket
// drained.
func TestRehydrate_LimiterSkipsNoCookie(t *testing.T) {
	fb := newFakeBroker(t)
	_, srvB := newReplica(t, fb, "gw-B")
	for i := 0; i < 400; i++ {
		resp := proxied(t, srvB, testHost, "/", "x-"+strconv.Itoa(i), nil)
		drain(resp)
	}
	resp := proxied(t, srvB, testHost, "/", "", nil)
	defer drain(resp)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("cookie-less request = %d, want 401 — the lookup bound must not touch it", resp.StatusCode)
	}
}
