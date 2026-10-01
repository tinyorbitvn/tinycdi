// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package api

import (
	"net/http"
)

// me.go implements GET /v1/me — the portal bootstrap payload (D17): the
// verified identity, the caller's roles, the derived CSRF token (P1) and the
// session domain the portal builds workspace launch URLs under.

// MeHandler implements GET /v1/me.
type MeHandler struct {
	// sessionDomain is the configured session domain ("host[:port]") the
	// portal names in WorkspaceSessionHost addresses — sessionhost.Domain's
	// String() form.
	sessionDomain string
}

// NewMeHandler wires the handler; sessionDomain is the host[:port] the
// session listener answers on (sessionhost.Domain.String()).
func NewMeHandler(sessionDomain string) *MeHandler {
	return &MeHandler{sessionDomain: sessionDomain}
}

// MountMeRoutes registers GET /v1/me behind sliding auth (a portal page load
// counts as activity).
func MountMeRoutes(mux *http.ServeMux, authn *Authenticator, h *MeHandler) {
	mux.Handle("GET /v1/me", authn.RequireAuth(http.HandlerFunc(h.Get)))
}

// meView matches the Me schema in openapi.yaml.
type meView struct {
	Subject       string   `json:"subject"`
	DisplayName   string   `json:"displayName"`
	Email         string   `json:"email,omitempty"`
	Tenant        string   `json:"tenant"`
	Roles         []string `json:"roles"`
	CSRFToken     string   `json:"csrfToken"`
	SessionDomain string   `json:"sessionDomain"`
}

// Get handles GET /v1/me.
func (h *MeHandler) Get(w http.ResponseWriter, r *http.Request) {
	p, ok := PrincipalFromContext(r.Context())
	if !ok {
		writeError(w, r, CodeUnauthenticated, "authentication required")
		return
	}
	sess, ok := SessionFromContext(r.Context())
	if !ok {
		writeError(w, r, CodeUnauthenticated, "authentication required")
		return
	}
	// In G2 displayName equals subject; the principal directory fills it in
	// later (T3.4). Every authenticated principal carries the "user" role;
	// tenant-admin is added from the verified group claim.
	roles := []string{"user"}
	if p.InGroup(TenantAdminGroup) {
		roles = append(roles, TenantAdminGroup)
	}
	w.Header().Set("Cache-Control", "no-store")
	respondJSON(w, meView{
		Subject:       p.Subject,
		DisplayName:   p.Subject,
		Tenant:        p.TenantID,
		Roles:         roles,
		CSRFToken:     csrfTokenFor(sess.ID),
		SessionDomain: h.sessionDomain,
	})
}
