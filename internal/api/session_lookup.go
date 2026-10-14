// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package api

import (
	"errors"
	"net/http"
	"net/netip"
	"strconv"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/ratelimit"
)

// session_lookup.go — the app listener's per-client bound on session-store
// reads for cookie values (TM-06). A random-cookie spray would otherwise
// turn request rate directly into Postgres reads on a pool the broker
// shares. Only reads that miss spend the budget: a lookup takes a token
// first and gives it back when it finds a live session (or the store
// fails), so signed-in users behind one address are never throttled by it.
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
	return a
}

// lookupSession reads the session a cookie value names — Get (sliding the
// idle deadline) or Peek — through the per-client budget. It returns
// *lookupLimitedError without a store read when the budget is spent, and
// otherwise the store's own answer.
func (a *Authenticator) lookupSession(r *http.Request, id string, slide bool) (*Session, error) {
	var key string
	if a.lookups != nil {
		key = ratelimit.ClientKey(r, a.trusted)
		if ok, retry := a.lookups.Allow(key); !ok {
			if a.metrics != nil {
				a.metrics.IncRateLimited("session_lookup")
			}
			return nil, &lookupLimitedError{retryAfter: retry}
		}
	}
	var sess *Session
	var err error
	if slide {
		sess, err = a.sessions.Get(r.Context(), id)
	} else {
		sess, err = a.sessions.Peek(r.Context(), id)
	}
	if a.lookups != nil && !errors.Is(err, ErrSessionNotFound) {
		// A hit, or a store that could not answer: not the client's spray.
		a.lookups.Refund(key)
	}
	return sess, err
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
