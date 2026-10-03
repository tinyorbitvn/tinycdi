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
