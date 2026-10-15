// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package api

import (
	"container/list"
	"crypto/sha256"
	"errors"
	"net/http"
	"net/netip"
	"strconv"
	"sync"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/ratelimit"
)

// session_lookup.go — the app listener's per-client bound on session-store
// reads for cookie values (TM-06). A random-cookie spray would otherwise
// turn request rate directly into Postgres reads on a pool the broker
// shares. Only reads that miss spend the budget: a lookup takes a token
// first and gives it back only when it finds a live session. A cookie this
// replica recently confirmed live skips the budget altogether, so a spray
// from a shared client key (NAT, one IPv6 /64, a mis-set -trusted-proxies)
// cannot lock signed-in users out. The skip never skips the store read:
// logout, revocation and expiry still end the session on its next request.
// The bound is local to the replica, like the session listener's
// SessionLookupLimiter: a shared window would spend the reads it exists to
// prevent.

// SessionLookupRate and SessionLookupBurst are the default bound
// NewAuthenticator installs (per client, per replica) — the session
// listener's defaults. The backend replaces it with -session-lookup-rate.
const (
	SessionLookupRate    = 300
	SessionLookupBurst   = 120
	sessionLookupMaxKeys = 100_000
	// liveSessionTTL caps how long a confirmed-live cookie stays exempt
	// from the budget without a fresh hit; the session's own absolute
	// expiry caps it further.
	liveSessionTTL = 5 * time.Minute
)

// SessionLookupLimiter is the per-client budget for session-store lookups;
// *ratelimit.Limiter implements it.
type SessionLookupLimiter interface {
	Allow(key string) (ok bool, retryAfter time.Duration)
	Refund(key string)
}

// lookupLimitedError is lookupSession's refusal: the client spent its
// unknown-cookie budget, and the store was not read.
type lookupLimitedError struct{ retryAfter time.Duration }

func (e *lookupLimitedError) Error() string { return "api: session lookup rate limited" }

// WithSessionLookupLimiter replaces the default unknown-cookie bound. trusted
// is the -trusted-proxies list ratelimit.ClientKey uses to find the client.
// A nil limiter disables the bound.
func (a *Authenticator) WithSessionLookupLimiter(l SessionLookupLimiter, trusted []netip.Prefix) *Authenticator {
	a.lookups, a.trusted = l, trusted
	if a.live == nil {
		a.live = newLiveSessionCache(sessionLookupMaxKeys)
	}
	return a
}

// lookupSession reads the session a cookie value names — Get (sliding the
// idle deadline) or Peek — through the per-client budget. A cookie this
// replica confirmed live within liveSessionTTL skips the budget; every
// other cookie takes a token first. It returns *lookupLimitedError without
// a store read when the budget is spent, and otherwise the store's own
// answer: the cache decides only who pays, never who is signed in.
func (a *Authenticator) lookupSession(r *http.Request, id string, slide bool) (*Session, error) {
	now := time.Now
	if a.now != nil {
		now = a.now
	}
	var key string
	charged := false
	if a.lookups != nil && !a.live.contains(id, now()) {
		key = ratelimit.ClientKey(r, a.trusted)
		if ok, retry := a.lookups.Allow(key); !ok {
			if a.metrics != nil {
				a.metrics.IncRateLimited("session_lookup")
			}
			return nil, &lookupLimitedError{retryAfter: retry}
		}
		charged = true
	}
	var sess *Session
	var err error
	if slide {
		sess, err = a.sessions.Get(r.Context(), id)
	} else {
		sess, err = a.sessions.Peek(r.Context(), id)
	}
	switch {
	case err == nil && sess != nil:
		// Only a live hit gives the token back. A read that ended in any
		// error — not found, a deadline, a cancelled request, a server-side
		// cancel, a store fault — still cost the store, and a degraded
		// shared pool is the condition this bound exists for.
		if charged {
			a.lookups.Refund(key)
		}
		exp := now().Add(liveSessionTTL)
		if !sess.ExpiresAt.IsZero() && sess.ExpiresAt.Before(exp) {
			exp = sess.ExpiresAt
		}
		a.live.add(id, exp)
	case errors.Is(err, ErrSessionNotFound):
		a.live.remove(id)
	}
	return sess, err
}

// forgetLiveSession drops a cookie from the live-session cache; logout and
// re-login call it when they delete the row. The cache never authenticates
// on its own, so this only returns the cookie to the budget sooner.
func (a *Authenticator) forgetLiveSession(id string) { a.live.remove(id) }

// liveSessionCache is a bounded, per-replica LRU of cookie values recently
// confirmed live, keyed by their SHA-256 so the raw value is never held. A
// nil cache holds nothing. Entries enter only after a store hit, so a
// spray of forged cookies cannot fill it.
type liveSessionCache struct {
	mu    sync.Mutex
	max   int
	lru   *list.List // front = most recently confirmed
	items map[[sha256.Size]byte]*list.Element
}

type liveSessionEntry struct {
	digest  [sha256.Size]byte
	expires time.Time
}

func newLiveSessionCache(max int) *liveSessionCache {
	return &liveSessionCache{max: max, lru: list.New(), items: make(map[[sha256.Size]byte]*list.Element)}
}

func sessionDigest(id string) [sha256.Size]byte { return sha256.Sum256([]byte(id)) }

// contains reports whether id was confirmed live and has not expired at now.
func (c *liveSessionCache) contains(id string, now time.Time) bool {
	if c == nil {
		return false
	}
	d := sessionDigest(id)
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[d]
	if !ok {
		return false
	}
	if !now.Before(el.Value.(*liveSessionEntry).expires) {
		c.lru.Remove(el)
		delete(c.items, d)
		return false
	}
	return true
}

// add records id as live until expires, evicting the least recently
// confirmed entry past max.
func (c *liveSessionCache) add(id string, expires time.Time) {
	if c == nil || c.max <= 0 {
		return
	}
	d := sessionDigest(id)
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[d]; ok {
		el.Value.(*liveSessionEntry).expires = expires
		c.lru.MoveToFront(el)
		return
	}
	for c.lru.Len() >= c.max {
		back := c.lru.Back()
		c.lru.Remove(back)
		delete(c.items, back.Value.(*liveSessionEntry).digest)
	}
	c.items[d] = c.lru.PushFront(&liveSessionEntry{digest: d, expires: expires})
}

// remove forgets id.
func (c *liveSessionCache) remove(id string) {
	if c == nil {
		return
	}
	d := sessionDigest(id)
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[d]; ok {
		c.lru.Remove(el)
		delete(c.items, d)
	}
}

// writeLookupLimited answers a spent unknown-cookie budget: 429 with
// Retry-After, the same shape as every other limiter refusal.
func writeLookupLimited(w http.ResponseWriter, r *http.Request, e *lookupLimitedError) {
	w.Header().Set("Retry-After", strconv.Itoa(ratelimit.RetryAfterSeconds(e.retryAfter)))
	writeError(w, r, CodeRateLimited, "rate limit exceeded")
}

// sessionLookedUp reports whether SessionRateLimitKey already tried this
// request's cookie and found no session; the probe then answers without a
// second read.
func sessionLookedUp(r *http.Request) bool {
	v, _ := r.Context().Value(ctxKeySessionLookedUp).(bool)
	return v
}
