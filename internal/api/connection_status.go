// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
)

// connection_status.go implements GET /v1/workspaces/{id}/connection (P4):
// the passive endpoint the portal polls for the session's connection state
// (D15 — no postMessage exists between portal and session frame). It runs
// behind RequireAuthPassive so polling never extends the caller's idle
// window (D18).

// ConnectionStatus is the public view of a workspace's session connection
// (openapi.yaml ConnectionStatus). State is one of none | connected |
// disconnected | stale.
type ConnectionStatus struct {
	State         string     `json:"state"`
	LeaseActive   bool       `json:"leaseActive"`
	LastRenewedAt *time.Time `json:"lastRenewedAt,omitempty"`
	// LeaseRef lets a tab tell WHICH lease it is looking at: the first 16 hex
	// chars of SHA-256 of the active lease ID (never the ID itself). Absent
	// without an active lease.
	LeaseRef string `json:"leaseRef,omitempty"`
	// StreamEpoch is the active lease's stream_epoch: it advances each time a
	// stream is claimed on the lease. 0 without an active lease.
	StreamEpoch uint64 `json:"streamEpoch"`
	// StreamOwnerTab is the opaque per-tab id the current stream was claimed
	// with (migration 018): the portal compares it against its own tab id —
	// equal means the stream is ours even across a backend restart, different
	// means another tab holds it. Absent when the claim carried no id, and
	// NEVER served to a principal other than the lease's own.
	StreamOwnerTab string `json:"streamOwnerTab,omitempty"`
}

// ConnectionStater is the broker-facing surface the handler needs. Defined
// here (not in broker) so this package never imports internal/broker; the
// adapter lives in internal/broker/apishim.go.
type ConnectionStater interface {
	// ConnectionState reports the workspace's connection state. owner is the
	// caller's owner ref (issuer|subject): StreamOwnerTab is populated only
	// when it equals the lease's own principal.
	ConnectionState(ctx context.Context, workspaceUID, owner string) (ConnectionStatus, *Error)
}

// workspaceGetter is the ownership-check surface the endpoint shares with
// GET /v1/workspaces/{id} — a non-owner (and a tenant admin is "owner" of
// everything in scope) sees the same 404 that endpoint returns.
type workspaceGetter interface {
	GetWorkspace(ctx context.Context, tenantID, ownerScope, id string) (provisioning.WorkspaceRecord, error)
}

// ConnectionStatusHandler implements GET /v1/workspaces/{id}/connection.
type ConnectionStatusHandler struct {
	stater     ConnectionStater
	workspaces workspaceGetter
	tenants    TenantResolver
}

// NewConnectionStatusHandler wires the handler; all three dependencies are
// required.
func NewConnectionStatusHandler(stater ConnectionStater, workspaces workspaceGetter, tenants TenantResolver) *ConnectionStatusHandler {
	return &ConnectionStatusHandler{stater: stater, workspaces: workspaces, tenants: tenants}
}

// MountConnectionStatusRoutes registers the route behind passive auth:
// authenticated like everything else, but never sliding the idle timer.
func MountConnectionStatusRoutes(mux *http.ServeMux, authn *Authenticator, h *ConnectionStatusHandler) {
	mux.Handle("GET /v1/workspaces/{id}/connection",
		authn.RequireAuthPassive(http.HandlerFunc(h.Get)))
}

// Get handles GET /v1/workspaces/{id}/connection.
func (h *ConnectionStatusHandler) Get(w http.ResponseWriter, r *http.Request) {
	p, ok := PrincipalFromContext(r.Context())
	if !ok {
		writeError(w, r, CodeUnauthenticated, "authentication required")
		return
	}
	if _, ok := h.tenants.Namespace(p.TenantID); !ok {
		writeError(w, r, CodeForbidden, "tenant is not provisioned")
		return
	}
	id := r.PathValue("id")
	if !workspaceIDPattern.MatchString(id) {
		writeError(w, r, CodeInvalidRequest, "bad workspace id")
		return
	}
	// The ownership rule of GET /v1/workspaces/{id}: the lookup itself
	// enforces owner/tenant scope — a non-owner's workspace is
	// indistinguishable from a missing one (404), and no state is read for
	// a workspace the caller cannot see.
	if _, err := h.workspaces.GetWorkspace(r.Context(), p.TenantID, ownerScope(p), id); err != nil {
		if errors.Is(err, provisioning.ErrWorkspaceNotFound) {
			writeError(w, r, CodeNotFound, "workspace not found")
			return
		}
		writeError(w, r, CodeInternal, "internal error")
		return
	}
	st, apiErr := h.stater.ConnectionState(r.Context(), id, p.Owner())
	if apiErr != nil {
		WriteError(w, RequestIDFromContext(r.Context()), apiErr)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	respondJSON(w, st)
}
