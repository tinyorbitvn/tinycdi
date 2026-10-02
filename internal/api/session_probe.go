// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package api

import (
	"net/http"
)

// session_probe.go implements GET /v1/session — the anonymous, passive
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
// authentication, no CSRF token and sets no cookie.
func MountSessionProbeRoute(mux *http.ServeMux, authn *Authenticator) {
	mux.Handle("GET /v1/session", http.HandlerFunc(authn.SessionProbeHandler))
}

// SessionProbeHandler answers 200 {"authenticated": true|false}. The session
// is read with Peek, so the probe never slides the portal idle timer: an
// unattended tab polling it cannot keep a session alive (P4, D18).
func (a *Authenticator) SessionProbeHandler(w http.ResponseWriter, r *http.Request) {
	authenticated := false
	if c, err := r.Cookie(a.cfg.SessionCookieName); err == nil && c.Value != "" {
		_, err := a.sessions.Peek(r.Context(), c.Value)
		authenticated = err == nil
	}
	w.Header().Set("Cache-Control", "no-store")
	respondJSON(w, sessionProbeView{Authenticated: authenticated})
}
