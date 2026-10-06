// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package api

import (
	"net/http"

	"github.com/tinyorbitvn/tinycdi/internal/observability"
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
// deadline. The mutation needs no body: RequireAuth's sliding session read
// IS the touch, RequireCSRF guards it like every other state change, and
// the audited wrapper emits the session.touch event. Optional middleware
// wraps the chain (the production mount applies the session-digest-keyed
// login-family limiter).
func MountSessionTouchRoute(mux *http.ServeMux, authn *Authenticator, wrap ...func(http.Handler) http.Handler) {
	var h http.Handler = authn.RequireAuth(
		audited(lateAuditSink{func() observability.AuditSink { return authn.auditSink }}, routeSessionTouch,
			authn.RequireCSRF(http.HandlerFunc(authn.SessionTouchHandler))))
	for _, w := range wrap {
		h = w(h)
	}
	mux.Handle(routeSessionTouch, h)
}

// SessionTouchHandler answers 204 — the request is authenticated and CSRF-
// checked already, and the sliding read inside RequireAuth extended the
// idle deadline; there is nothing else to do.
func (a *Authenticator) SessionTouchHandler(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}
