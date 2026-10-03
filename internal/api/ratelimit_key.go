// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package api

import (
	"context"
	"crypto/subtle"
	"net/http"

	"github.com/tinyorbitvn/tinycdi/internal/ratelimit"
)

// ratelimit_key.go — the authenticated rate-limit keys (FX-R30). The
// client-IP limits exist for the unauthenticated surface; a shared client
// address is the norm behind NAT, so a request that proves a live session
// (or a validated OIDC login state on the callback) is keyed by a digest
// of that credential instead: many users behind one office address keep
// their own buckets, and one abusive session starves only itself.
// Validity is always checked before the key is minted — an unverifiable
// value falls back to the client-IP key, so spraying forged cookies or
// states can neither grow the key space nor escape the per-IP budget.

// SessionRateLimitKey keys a request carrying a VALID session cookie by a
// digest of the session ID; anything else yields "" so the caller falls
// back to the client-IP key. Validity is checked with Peek — no idle
// slide, no write — so this resolver is wired only where the read is
// already owed: the session probe Peeks itself, and the verified session
// is parked on the request context where RequireAuth would put it, so the
// handler reuses the lookup instead of reading the store twice. /v1/login
// deliberately does NOT use this resolver: it is the anonymous login
// start, and keying it by cookie would buy every forged value a store
// read before the refusal.
func (a *Authenticator) SessionRateLimitKey() RateLimitKeyFunc {
	return func(r *http.Request) string {
		c, err := r.Cookie(a.cfg.SessionCookieName)
		if err != nil || c.Value == "" {
			return ""
		}
		sess, err := a.sessions.Peek(r.Context(), c.Value)
		if err != nil {
			// Unknown, expired — and a store outage: all mean "no
			// authenticated key". The IP fallback keeps the limiter
			// fail-closed without changing what the handler answers
			// (the probe reports authenticated:false; RequireAuth
			// still returns its own 503 on transient errors).
			return ""
		}
		// The assignment mutates the request the middleware passes on —
		// reuse of the auth path's lookup, not a second store read.
		*r = *r.WithContext(context.WithValue(r.Context(), ctxKeySession, sess))
		return ratelimit.KeyDigest("sess:", sess.ID)
	}
}

// CallbackRateLimitKey keys GET /v1/auth/callback by the validated OIDC
// state: the ?state parameter must equal the state sealed into the
// browser-bound login cookie (SEC-03), so a state the IdP never issued to
// this browser cannot mint a bucket. An absent, unsealable, expired or
// mismatched state yields "" — the caller falls back to the client-IP
// key. Open is a pure AEAD decode: no store read, no write, and the
// handler re-opens and consumes the same cookie itself. A validated state
// IS mintable — it costs its holder one IP-limited login start — so the
// caller pairs this resolver with a per-IP ceiling
// (RateLimitWithCeiling): the key spray buys at most a fixed multiple of
// the IP budget, never an unbounded one.
func (a *Authenticator) CallbackRateLimitKey() RateLimitKeyFunc {
	return func(r *http.Request) string {
		state := r.URL.Query().Get("state")
		if state == "" {
			return ""
		}
		c, err := r.Cookie(a.cfg.LoginCookieName)
		if err != nil || c.Value == "" {
			return ""
		}
		st, err := a.cfg.LoginSealer.Open(c.Value, a.now())
		if err != nil ||
			subtle.ConstantTimeCompare([]byte(st.OAuthState), []byte(state)) != 1 {
			return ""
		}
		return ratelimit.KeyDigest("oidc:", state)
	}
}
