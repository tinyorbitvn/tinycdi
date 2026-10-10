// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package api

import (
	"errors"
	"net/http"

	"github.com/tinyorbitvn/tinycdi/internal/observability"
	"github.com/tinyorbitvn/tinycdi/internal/store"
)

// session_probe.go implements the session surface's two one-liners:
// GET /v1/session — the anonymous, passive
// "is there a live portal session?" probe (FX-R13b). The portal calls it
// before GET /v1/me so a signed-out load never issues a request that fails
// with 401: the browser logs every failed fetch to the console and page
// script cannot suppress that line.

// sessionProbeView matches the SessionProbe schema in openapi.yaml. It must
// stay exactly one key: no subject, tenant, roles or token ever leaves here.
type sessionProbeView struct {
	Authenticated bool `json:"authenticated"`
}

// MountSessionProbeRoute registers GET /v1/session. It needs no
// authentication, no CSRF token and sets no cookie. Optional middleware
// wraps the handler (the production mount applies the login rate limit —
// the probe is anonymous and needs a limit like the rest of the
// unauthenticated surface, backlog 12).
func MountSessionProbeRoute(mux *http.ServeMux, authn *Authenticator, wrap ...func(http.Handler) http.Handler) {
	var h http.Handler = http.HandlerFunc(authn.SessionProbeHandler)
	for _, w := range wrap {
		h = w(h)
	}
	mux.Handle("GET /v1/session", h)
}

// SessionProbeHandler answers 200 {"authenticated": true|false}. The session
// is read with Peek, so the probe never slides the portal idle timer: an
// unattended tab polling it cannot keep a session alive (P4, D18). A session
// the rate-limit middleware already verified rides in on the request context
// and is reused instead of a second store read (FX-R30).
func (a *Authenticator) SessionProbeHandler(w http.ResponseWriter, r *http.Request) {
	authenticated := false
	if _, ok := SessionFromContext(r.Context()); ok {
		authenticated = true
	} else if c, err := r.Cookie(a.cfg.SessionCookieName); err == nil && c.Value != "" {
		_, err := a.sessions.Peek(r.Context(), c.Value)
		authenticated = err == nil
	}
	w.Header().Set("Cache-Control", "no-store")
	respondJSON(w, sessionProbeView{Authenticated: authenticated})
}

// MountSessionTouchRoute registers POST /v1/session:touch — the explicit
// activity beat (FIX-IDLE): every cookie-authenticated GET is passive, so
// the portal sends this on real user interaction to slide the idle
// deadline. The chain is deliberately RequireAuthPassive + RequireCSRF:
// authentication must NOT slide, because a cookie-only POST that fails the
// token check would otherwise extend the window it could not earn — the
// slide happens explicitly inside the handler, after CSRF passed. The
// audited wrapper emits the session.touch event; optional middleware wraps
// the chain (the production mount applies the session-digest-keyed
// login-family limiter).
func MountSessionTouchRoute(mux *http.ServeMux, authn *Authenticator, wrap ...func(http.Handler) http.Handler) {
	var h http.Handler = authn.RequireAuthPassive(
		audited(lateAuditSink{func() observability.AuditSink { return authn.auditSink }}, routeSessionTouch,
			authn.RequireCSRF(http.HandlerFunc(authn.SessionTouchHandler))))
	for _, w := range wrap {
		h = w(h)
	}
	mux.Handle(routeSessionTouch, h)
}

// SessionTouchHandler answers 204 after sliding the session's idle
// deadline explicitly: Get is the only write, and it runs only here —
// after authentication and CSRF succeeded — so a denied request never
// extends the window.
func (a *Authenticator) SessionTouchHandler(w http.ResponseWriter, r *http.Request) {
	sess, ok := SessionFromContext(r.Context())
	if !ok || sess == nil {
		writeError(w, r, CodeUnauthenticated, "authentication required")
		return
	}
	if _, err := a.sessions.Get(r.Context(), sess.ID); err != nil {
		switch {
		case errors.Is(err, ErrSessionNotFound):
			writeError(w, r, CodeUnauthenticated, "session missing or expired")
		case store.IsTransient(err):
			writeError(w, r, CodeUnavailable, "session store unavailable")
		default:
			writeError(w, r, CodeInternal, "internal error")
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
