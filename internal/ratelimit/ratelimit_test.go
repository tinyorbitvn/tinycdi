// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package ratelimit

import (
	"fmt"
	"net/http"
	"net/netip"
	"testing"
	"time"
)

func TestLimiter_BurstThenRefill(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	l := New(60, 3, 100, func() time.Time { return now }) // 1 token/s, burst 3

	for i := 0; i < 3; i++ {
		if ok, _ := l.Allow("k"); !ok {
			t.Fatalf("burst request %d refused", i+1)
		}
	}
	ok, retry := l.Allow("k")
	if ok {
		t.Fatal("request past the burst was allowed")
	}
	if retry <= 0 {
		t.Fatalf("retryAfter = %v, want positive", retry)
	}
	// A refused attempt consumes nothing: waiting the reported interval
	// must admit the next request.
	now = now.Add(retry)
	if ok, _ := l.Allow("k"); !ok {
		t.Fatalf("request after %v still refused", retry)
	}
}

func TestLimiter_BoundedMemory(t *testing.T) {
	l := New(60, 1, 1024, nil)
	for i := 0; i < 1_000_000; i++ {
		l.Allow(fmt.Sprintf("k%d", i))
	}
	l.mu.Lock()
	n := len(l.buckets)
	l.mu.Unlock()
	if n > 1024 {
		t.Fatalf("buckets = %d, want <= maxKeys 1024", n)
	}
}

func TestClientKey_IgnoresXFFFromUntrustedPeer(t *testing.T) {
	r := &http.Request{
		RemoteAddr: "203.0.113.7:5150",
		Header:     http.Header{"X-Forwarded-For": {"1.1.1.1, 2.2.2.2"}},
	}
	if k := ClientKey(r, nil); k != "203.0.113.7" {
		t.Fatalf("key = %q, want socket peer 203.0.113.7 — spoofed XFF must not shift it", k)
	}
	// A configured trusted set that does not contain the peer changes
	// nothing.
	trusted := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	if k := ClientKey(r, trusted); k != "203.0.113.7" {
		t.Fatalf("key = %q, want socket peer 203.0.113.7", k)
	}
}

func TestClientKey_RightmostUntrusted(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	r := &http.Request{
		RemoteAddr: "10.1.2.3:443", // trusted peer
		Header:     http.Header{"X-Forwarded-For": {"9.9.9.9, 8.8.8.8, 10.2.2.2"}},
	}
	if k := ClientKey(r, trusted); k != "8.8.8.8" {
		t.Fatalf("key = %q, want right-most untrusted entry 8.8.8.8", k)
	}

	// No XFF under a trusted peer: the peer itself is the client.
	r2 := &http.Request{RemoteAddr: "10.1.2.3:443", Header: http.Header{}}
	if k := ClientKey(r2, trusted); k != "10.1.2.3" {
		t.Fatalf("key = %q, want peer 10.1.2.3", k)
	}

	// Every hop trusted: the left-most entry is the closest claim.
	r3 := &http.Request{
		RemoteAddr: "10.1.2.3:443",
		Header:     http.Header{"X-Forwarded-For": {"10.9.9.9, 10.8.8.8"}},
	}
	if k := ClientKey(r3, trusted); k != "10.9.9.9" {
		t.Fatalf("key = %q, want left-most entry 10.9.9.9", k)
	}
}

// TestClientKey_IPv6FoldedTo64 pins the per-client budget at the /64 —
// the prefix one subscriber controls: every address inside it (temporary
// privacy addresses, deliberate rotation) shares one bucket, while a
// different /64 is a different client. Covers both the socket-peer path
// and the right-most-untrusted XFF path.
func TestClientKey_IPv6FoldedTo64(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	xf := func(xff string) *http.Request {
		return &http.Request{
			RemoteAddr: "10.1.2.3:443",
			Header:     http.Header{"X-Forwarded-For": {xff}},
		}
	}
	k1 := ClientKey(xf("2001:db8::1"), trusted)
	k2 := ClientKey(xf("2001:db8::ffff"), trusted)
	if k1 != k2 {
		t.Fatalf("addresses in one /64 keyed separately: %q vs %q", k1, k2)
	}
	if k3 := ClientKey(xf("2001:db8:0:1::1"), trusted); k3 == k1 {
		t.Fatalf("different /64 shares a key: %q", k3)
	}
	// A direct (untrusted peer) IPv6 client folds the same way.
	peer := &http.Request{RemoteAddr: "[2001:db8::5]:5150", Header: http.Header{}}
	if k := ClientKey(peer, nil); k != k1 {
		t.Fatalf("socket peer key = %q, want the same /64 bucket %q", k, k1)
	}

	// The folded key is what the bucket sees: 101 addresses inside one
	// /64 draw from ONE budget, not 101.
	l := New(1, 1, 100000, nil) // 1/min, burst 1
	mk := func(xff string) string { return ClientKey(xf(xff), trusted) }
	if ok, _ := l.Allow(mk("2001:db8::1")); !ok {
		t.Fatal("first request refused")
	}
	for i := 2; i <= 101; i++ {
		if ok, _ := l.Allow(mk(fmt.Sprintf("2001:db8::%x", i))); ok {
			t.Fatalf("rotation to 2001:db8::%x escaped the /64 budget", i)
		}
	}
	// A different /64 is a different client and gets its own budget.
	if ok, _ := l.Allow(mk("2001:db8:0:1::1")); !ok {
		t.Fatal("first request from a different /64 refused")
	}
}

// TestClientKey_MappedFormsUnified: the IPv4-mapped spellings of one
// address — "::ffff:1.2.3.4", "0:0:0:0:0:ffff:1.2.3.4" and the native
// "1.2.3.4" — must land on the single IPv4 key, so a dual-stack client
// cannot double its budget by switching spelling.
func TestClientKey_MappedFormsUnified(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	mk := func(xff string) string {
		return ClientKey(&http.Request{
			RemoteAddr: "10.1.2.3:443",
			Header:     http.Header{"X-Forwarded-For": {xff}},
		}, trusted)
	}
	k1 := mk("1.2.3.4")
	for _, form := range []string{"::ffff:1.2.3.4", "0:0:0:0:0:ffff:1.2.3.4"} {
		if k := mk(form); k != k1 {
			t.Fatalf("mapped form %q keyed %q, want %q", form, k, k1)
		}
	}
	// The mapped spelling of a direct socket peer unifies too.
	if k := ClientKey(&http.Request{RemoteAddr: "[::ffff:1.2.3.4]:443", Header: http.Header{}}, nil); k != k1 {
		t.Fatalf("mapped socket peer keyed %q, want %q", k, k1)
	}
}

func TestClientKey_MultipleHeadersAndNonIP(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	r := &http.Request{
		RemoteAddr: "10.0.0.1:443",
		Header: http.Header{
			"X-Forwarded-For": {"1.1.1.1, 10.0.0.9", "2.2.2.2"},
		},
	}
	if k := ClientKey(r, trusted); k != "2.2.2.2" {
		t.Fatalf("key = %q, want right-most untrusted 2.2.2.2 across header lines", k)
	}
	// A non-IP entry is never trusted, so it can be the key.
	r.Header.Set("X-Forwarded-For", "unknown")
	if k := ClientKey(r, trusted); k != "unknown" {
		t.Fatalf("key = %q, want \"unknown\"", k)
	}
}

func TestLimiter_DisabledAllows(t *testing.T) {
	l := New(0, 10, 100, nil)
	for i := 0; i < 1000; i++ {
		if ok, _ := l.Allow("k"); !ok {
			t.Fatal("disabled limiter refused a request")
		}
	}
}

func TestLimiter_KeysAreIndependent(t *testing.T) {
	l := New(60, 1, 100, nil)
	if ok, _ := l.Allow("a"); !ok {
		t.Fatal("first request for a refused")
	}
	if ok, _ := l.Allow("b"); !ok {
		t.Fatal("first request for b refused — buckets must be independent")
	}
	if ok, _ := l.Allow("a"); ok {
		t.Fatal("second request for a inside burst allowed")
	}
}

func TestPerReplica(t *testing.T) {
	for _, tc := range []struct {
		rate, burst, n int
		wantR, wantB   int
	}{
		{30, 10, 2, 15, 5},   // even split
		{30, 10, 3, 10, 3},   // rounds down, never up
		{60, 20, 1, 60, 20},  // one replica: full budget
		{60, 20, 0, 60, 20},  // n < 1: no division
		{30, 10, -2, 30, 10}, // n < 1: no division
		{1, 2, 5, 1, 1},      // positive rate clamps to 1, not 0
		{0, 10, 2, 0, 5},     // disabled stays disabled
		{300, 100, 3, 100, 33},
	} {
		if r, b := PerReplica(tc.rate, tc.burst, tc.n); r != tc.wantR || b != tc.wantB {
			t.Errorf("PerReplica(%d, %d, %d) = (%d, %d), want (%d, %d)",
				tc.rate, tc.burst, tc.n, r, b, tc.wantR, tc.wantB)
		}
	}
}

func TestParseTrustedProxies(t *testing.T) {
	p, err := ParseTrustedProxies(" 10.0.0.0/8 ,, 192.168.0.0/16,fd00::/8")
	if err != nil {
		t.Fatalf("ParseTrustedProxies: %v", err)
	}
	if len(p) != 3 {
		t.Fatalf("prefixes = %v, want 3", p)
	}
	if _, err := ParseTrustedProxies("not-a-cidr"); err == nil {
		t.Fatal("bad CIDR accepted")
	}
}
