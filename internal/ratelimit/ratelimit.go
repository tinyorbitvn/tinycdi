// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

// Package ratelimit implements the per-client token buckets behind E7
// (application rate limits) and the trusted-proxy client-address
// derivation shared with S18 (forwarded-header hygiene): the app listener
// throttles /v1/login, /v1/auth/callback and GET /v1/session, the session
// gateway throttles /v1/launch, and both derive the client key the same
// way — the socket peer, or the right-most untrusted X-Forwarded-For entry
// when the peer sits inside the configured trusted CIDRs. One selection
// feeds two renders: rate-limit keys (ClientKey) fold harder — mapped
// spellings unmapped, IPv6 collapsed to its /64 — so address-spelling and
// in-prefix rotation cannot multiply one client's budget, while the
// address consumers (ClientAddr: the gateway's forwarded headers) carry
// the real, unmapped-but-unfolded client address.
package ratelimit

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// bucket is one key's token state; the LRU list orders buckets most-
// recently-used first so the coldest entry is evicted at maxKeys.
type bucket struct {
	key    string
	tokens float64
	last   time.Time
}

// Limiter is a per-key token bucket with bounded memory (LRU of maxKeys).
// It is safe for concurrent use.
type Limiter struct {
	perSec  float64
	burst   float64
	maxKeys int
	now     func() time.Time

	mu      sync.Mutex
	lru     *list.List               // front = most recently used
	buckets map[string]*list.Element // key -> *bucket element
}

// New builds a Limiter allowing ratePerMinute tokens per minute per key
// with bursts up to burst. ratePerMinute <= 0 disables the limiter (Allow
// always reports ok); a burst < 1 is clamped to 1 (a zero-capacity bucket
// would be a permanent block, not a limit). Keys live in an LRU capped at
// maxKeys (clamped to >= 1) so a sprayed key space cannot grow memory
// without bound. now injects a clock for tests; nil means time.Now.
func New(ratePerMinute, burst, maxKeys int, now func() time.Time) *Limiter {
	if now == nil {
		now = time.Now
	}
	if burst < 1 {
		burst = 1
	}
	if maxKeys < 1 {
		maxKeys = 1
	}
	return &Limiter{
		perSec:  float64(ratePerMinute) / 60,
		burst:   float64(burst),
		maxKeys: maxKeys,
		now:     now,
		lru:     list.New(),
		buckets: map[string]*list.Element{},
	}
}

// Allow reports whether key may proceed. On refusal it returns how long
// the caller should wait before the next attempt is likely to pass — the
// value callers put on Retry-After. Refills are continuous at the
// configured rate; a refused attempt does not consume a token.
func (l *Limiter) Allow(key string) (ok bool, retryAfter time.Duration) {
	if l == nil || l.perSec <= 0 {
		return true, 0
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()

	el, ok := l.buckets[key]
	if !ok {
		if len(l.buckets) >= l.maxKeys {
			back := l.lru.Back()
			delete(l.buckets, back.Value.(*bucket).key)
			l.lru.Remove(back)
		}
		el = l.lru.PushFront(&bucket{key: key, tokens: l.burst, last: now})
		l.buckets[key] = el
	} else {
		l.lru.MoveToFront(el)
	}
	b := el.Value.(*bucket)
	if elapsed := now.Sub(b.last); elapsed > 0 {
		b.tokens += l.perSec * elapsed.Seconds()
		if b.tokens > l.burst {
			b.tokens = l.burst
		}
	}
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	// Ceil so a caller that waits exactly retryAfter always lands at or
	// above one token regardless of float rounding.
	return false, time.Duration((1-b.tokens)/l.perSec*float64(time.Second) + 0.5)
}

// PerReplica divides a configured per-key budget across n backend
// replicas so the aggregate over an even spread approximates the
// configured bound: every replica gets rate/n tokens per minute and
// burst/n (integer division rounds down, so the aggregate lands at or
// just under the flag, never above). A configured rate of 0 stays 0 —
// the limiter stays disabled — while any positive rate divides to at
// least 1 (a 0 per-replica rate would silently disable the limit, so a
// rate smaller than n yields an aggregate of ~n/min, not 0); burst
// likewise clamps to 1. n < 1 means no division (a single replica's
// full budget).
func PerReplica(rate, burst, n int) (int, int) {
	if n < 1 {
		n = 1
	}
	r, b := rate/n, burst/n
	if rate > 0 && r < 1 {
		r = 1
	}
	if b < 1 {
		b = 1
	}
	return r, b
}

// KeyDigest derives a stable, non-secret bucket key from secret-bearing
// material — a session ID or a validated OIDC state (FX-R30): the SHA-256
// rendered in hex and namespaced with prefix so a derived key can never
// collide with a client-IP key. The raw value never enters the key, so it
// stays unexposed even if a key were ever logged.
func KeyDigest(prefix, raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return prefix + hex.EncodeToString(sum[:])
}

// RetryAfterSeconds renders a refusal wait for the Retry-After header:
// whole seconds, rounded up, never below 1.
func RetryAfterSeconds(d time.Duration) int {
	secs := int(d / time.Second)
	if d%time.Second != 0 {
		secs++
	}
	if secs < 1 {
		secs = 1
	}
	return secs
}

// PeerIP extracts the host part of a socket address (r.RemoteAddr),
// tolerating a missing port.
func PeerIP(remoteAddr string) string {
	if h, _, err := net.SplitHostPort(remoteAddr); err == nil {
		return h
	}
	return strings.TrimSpace(remoteAddr)
}

// ClientKey returns the rate-limit key for r: the socket peer IP, unless
// the peer sits inside trusted — then the right-most X-Forwarded-For entry
// outside the trusted set stands in (the last hop the trusted chain did
// not claim). An entry or peer that does not parse as an IP is never
// trusted, so client-supplied bytes cannot claim a proxy's place; when
// every hop is trusted the left-most entry is the closest observable
// claim. A selected entry that is not a plain IP address is NEVER a key:
// it collapses to the socket peer, so a client whose own bytes reach the
// selected slot cannot mint a fresh key per request. The selected claim
// is canonicalised by canon: mapped forms unify with the native IPv4 key
// and IPv6 folds to its /64. The one string keys every enforcement layer
// — the local buckets and the shared Postgres window rows — so the
// layers always agree on which client a hit belongs to.
func ClientKey(r *http.Request, trusted []netip.Prefix) string {
	return canon(selectClient(r, trusted))
}

// ClientAddr returns the SAME selected client claim as ClientKey but
// rendered as the real address — IPv4-mapped forms unmapped, IPv6 NOT
// folded — for consumers that need the literal client address rather than
// a billing bucket: the gateway's forwarded-header rebuild toward the
// runtime (S18). Like the key path, a non-IP claim can never reach this
// render — it collapses to the socket peer — so the value is always a
// parseable address the runtime can key on.
func ClientAddr(r *http.Request, trusted []netip.Prefix) string {
	return canonAddr(selectClient(r, trusted))
}

// selectClient picks the raw client claim both renders share: the socket
// peer, or under a trusted peer the right-most XFF entry outside the
// trusted set — else the left-most claim when every hop is trusted, else
// the peer. A right-most untrusted entry that is not a plain IP address
// (unparseable, or a zoned IPv6 literal) collapses to the peer rather
// than standing in as the claim: it is client-supplied bytes, and using
// it verbatim would let that client mint an unbounded key per request —
// and feed garbage into forwarded headers.
func selectClient(r *http.Request, trusted []netip.Prefix) string {
	peer := PeerIP(r.RemoteAddr)
	if !inTrusted(peer, trusted) {
		return peer
	}
	var leftmost string
	entries := xffEntries(r)
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		leftmost = e
		if !inTrusted(e, trusted) {
			if !isPlainAddr(e) {
				return peer
			}
			return e
		}
	}
	if leftmost != "" {
		return leftmost
	}
	return peer
}

// isPlainAddr reports whether s is a bare IP address — parseable and
// zone-free. A zoned IPv6 literal ("fe80::1%eth0") parses but names an
// interface scope a proxy chain cannot have verified, and every zone
// spelling would mint its own key, so it selects like any other non-IP
// token.
func isPlainAddr(s string) bool {
	a, err := netip.ParseAddr(s)
	return err == nil && a.Zone() == ""
}

// canon renders a rate-limit key canonically — canonAddr plus the IPv6
// /64 fold: the address collapses to the prefix's base address, so every
// address inside the prefix one subscriber controls (SLAAC, privacy /
// temporary addresses, deliberate rotation) shares one budget; /64 is
// the smallest prefix an end site is delegated, hence the granularity a
// single client can still rotate inside. IPv4 stays the address itself
// (/32). Anything that does not parse passes through verbatim — reachable
// only from a non-IP socket peer, one fixed value per connection rather
// than a client claim — so it still discriminates one key from another.
func canon(s string) string {
	a, err := netip.ParseAddr(s)
	if err != nil {
		return s
	}
	a = a.Unmap()
	if a.Is6() {
		return netip.PrefixFrom(a, 64).Masked().Addr().String()
	}
	return a.String()
}

// canonAddr renders the real client address canonically: a parseable
// address is Unmap'd — the "::ffff:a.b.c.d" spellings become the native
// "a.b.c.d" — and printed in netip canonical form, never folded. Anything
// else passes through verbatim — reachable only from a non-IP socket
// peer, never from an XFF claim (selectClient folds those to the peer).
func canonAddr(s string) string {
	if a, err := netip.ParseAddr(s); err == nil {
		return a.Unmap().String()
	}
	return s
}

// inTrusted reports whether s parses as an address inside the trusted
// prefixes. A non-IP token is never trusted.
func inTrusted(s string, trusted []netip.Prefix) bool {
	a, err := netip.ParseAddr(s)
	if err != nil {
		return false
	}
	a = a.Unmap()
	for _, p := range trusted {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// xffEntries flattens every X-Forwarded-For header line into ordered
// left-to-right entries (client-claimed first, nearest proxy last).
func xffEntries(r *http.Request) []string {
	var out []string
	for _, line := range r.Header.Values("X-Forwarded-For") {
		for _, e := range strings.Split(line, ",") {
			if e = strings.TrimSpace(e); e != "" {
				out = append(out, e)
			}
		}
	}
	return out
}

// ParseTrustedProxies parses the -trusted-proxies CSV of CIDR prefixes.
// Entries are masked to canonical form; anything that is not a CIDR is a
// config error, never silently dropped.
func ParseTrustedProxies(csv string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, e := range strings.Split(csv, ",") {
		if e = strings.TrimSpace(e); e == "" {
			continue
		}
		p, err := netip.ParsePrefix(e)
		if err != nil {
			return nil, fmt.Errorf("bad CIDR %q", e)
		}
		out = append(out, p.Masked())
	}
	return out, nil
}
