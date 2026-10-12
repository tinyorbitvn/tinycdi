// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package api

import (
	"context"
	"errors"
	"io"
	"net/http"

	"github.com/tinyorbitvn/tinycdi/internal/observability"
	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
	"github.com/tinyorbitvn/tinycdi/internal/store"
)

// SessionPolicySource is the read/write seam behind
// /v1/admin/tenants/{tenant}/session-policy; the Postgres implementation
// is store.SessionPolicyStore.
type SessionPolicySource interface {
	Ceiling(ctx context.Context, tenantID string) (store.SessionPolicyCeiling, error)
	// SetCeiling writes the tenant's ceiling under optimistic
	// concurrency: ifMatch must name the row's current version
	// (SessionPolicyCeiling.Version) or be IfMatchCreate when no row
	// exists. A stale or wrong precondition answers
	// store.ErrSessionPolicyVersionMismatch. An all-FollowTemplate write
	// deletes the row — "no row" and "all-defaults row" are the same
	// state.
	SetCeiling(ctx context.Context, tenantID string, w store.SessionPolicyWrite, ifMatch string) error
}

// adminSessionPolicyStore adapts the Postgres ceiling store to the
// handler's seam.
type adminSessionPolicyStore struct {
	s *store.SessionPolicyStore
}

func (a *adminSessionPolicyStore) Ceiling(ctx context.Context, tenantID string) (store.SessionPolicyCeiling, error) {
	return a.s.Ceiling(ctx, tenantID)
}

func (a *adminSessionPolicyStore) SetCeiling(ctx context.Context, tenantID string, w store.SessionPolicyWrite, ifMatch string) error {
	return a.s.SetCeiling(ctx, tenantID, w, ifMatch)
}

// NewAdminSessionPolicySource wraps the Postgres session-policy store.
func NewAdminSessionPolicySource(db *store.DB) SessionPolicySource {
	return &adminSessionPolicyStore{s: store.NewSessionPolicyStore(db)}
}

// Session-policy source values carried by adminSessionPolicyView.Source:
// TenantRow when a ceiling row exists, FollowTemplate when none does —
// the two states the lattice resolves, named for the operator.
const (
	SessionPolicySourceTenantRow      = "TenantRow"
	SessionPolicySourceFollowTemplate = "FollowTemplate"
)

// adminFileTransferCeiling is the fileTransfer block of the admin
// session-policy view and PUT body (openapi AdminFileTransferCeiling):
// the inert channel shipped for T11 — mode is FollowTemplate and
// maxFileBytes is null until v1.3 makes them live.
type adminFileTransferCeiling struct {
	Mode         string `json:"mode"`
	MaxFileBytes *int64 `json:"maxFileBytes"`
}

// adminSessionPolicyView is the response of GET/PUT
// /v1/admin/tenants/{tenant}/session-policy (openapi
// AdminSessionPolicyView): the tenant's ceiling per channel
// (FollowTemplate = not narrowed), the source (TenantRow |
// FollowTemplate) and the version PUT echoes in If-Match — absent when
// the tenant has no row.
type adminSessionPolicyView struct {
	Tenant       string                   `json:"tenant"`
	Clipboard    string                   `json:"clipboard"`
	FileTransfer adminFileTransferCeiling `json:"fileTransfer"`
	Source       string                   `json:"source"`
	Version      string                   `json:"version,omitempty"`
}

// putSessionPolicyRequest is the PUT body (openapi
// PutAdminSessionPolicy). clipboard is required; fileTransfer is the
// inert T11 channel — accepted only at its defaults in this version.
type putSessionPolicyRequest struct {
	Clipboard    *string                 `json:"clipboard"`
	FileTransfer *putFileTransferCeiling `json:"fileTransfer"`
}

type putFileTransferCeiling struct {
	Mode         *string `json:"mode"`
	MaxFileBytes *int64  `json:"maxFileBytes"`
}

// AdminSessionPolicyHandler implements GET and PUT on
// /v1/admin/tenants/{tenant}/session-policy — the tenant clipboard
// ceiling of ADR 0010 (T10). Unlike quota there is no config declaration
// for session policy: every row is API-managed, so writes are never
// refused as managed-by-config.
type AdminSessionPolicyHandler struct {
	source  SessionPolicySource
	tenants TenantResolver
	maxBody int64
	// audit is the dedicated audit-event sink the admin routes emit
	// through (nil = no domain audit events).
	audit observability.AuditSink
}

// NewAdminSessionPolicyHandler wires the handler.
func NewAdminSessionPolicyHandler(src SessionPolicySource, t TenantResolver) *AdminSessionPolicyHandler {
	return &AdminSessionPolicyHandler{source: src, tenants: t, maxBody: 16 << 10}
}

// WithAuditSink attaches the audit sink the session-policy routes write
// their dedicated audit events to.
func (h *AdminSessionPolicyHandler) WithAuditSink(s observability.AuditSink) *AdminSessionPolicyHandler {
	h.audit = s
	return h
}

// MountAdminSessionPolicyRoutes registers the routes audited (see
// MountAdminQuotaRoutes): RequireAuthPassive+audit on the read,
// RequireAuth+audit+RequireCSRF on the write.
func MountAdminSessionPolicyRoutes(mux *http.ServeMux, authn *Authenticator, h *AdminSessionPolicyHandler) {
	mux.Handle(routeAdminSessionPolicyGet, authn.RequireAuthPassive(
		audited(h.audit, routeAdminSessionPolicyGet, http.HandlerFunc(h.Get))))
	mux.Handle(routeAdminSessionPolicyPut, authn.RequireAuth(
		audited(h.audit, routeAdminSessionPolicyPut,
			authn.RequireCSRF(http.HandlerFunc(h.Put)))))
}

func (h *AdminSessionPolicyHandler) view(ctx context.Context, tenantID string) (adminSessionPolicyView, error) {
	c, err := h.source.Ceiling(ctx, tenantID)
	if err != nil {
		return adminSessionPolicyView{}, err
	}
	out := adminSessionPolicyView{
		Tenant:    tenantID,
		Clipboard: provisioning.PolicyFollowTemplate,
		FileTransfer: adminFileTransferCeiling{
			Mode: provisioning.PolicyFollowTemplate,
		},
		Source: SessionPolicySourceFollowTemplate,
	}
	if !c.HasRow {
		return out, nil
	}
	out.Source = SessionPolicySourceTenantRow
	out.Version = c.Version
	if c.Clipboard != nil {
		out.Clipboard = *c.Clipboard
	}
	if c.FileTransfer != nil {
		out.FileTransfer.Mode = *c.FileTransfer
	}
	out.FileTransfer.MaxFileBytes = c.FileMaxBytes
	return out, nil
}

// Get handles GET /v1/admin/tenants/{tenant}/session-policy.
func (h *AdminSessionPolicyHandler) Get(w http.ResponseWriter, r *http.Request) {
	if _, ok := tenantAdminPrincipal(h.tenants, w, r); !ok {
		return
	}
	out, err := h.view(r.Context(), r.PathValue("tenant"))
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	respondJSON(w, out)
}

// validCeilingValue reports whether v is a storable ceiling value for a
// channel — the lattice's four real states; FollowTemplate is handled
// separately (it maps to NULL, never a stored value).
func validCeilingValue(v string, dirA, dirB string) bool {
	switch v {
	case provisioning.PolicyDisabled, provisioning.PolicyBidirectional, dirA, dirB:
		return true
	}
	return false
}

// Put handles PUT /v1/admin/tenants/{tenant}/session-policy: it writes
// the tenant's ceiling provided the caller names the current version in
// If-Match (optimistic concurrency — a stale version answers 412
// PRECONDITION_FAILED; the header is required, and If-Match "*" creates
// only a missing row). A PUT whose every channel is FollowTemplate
// deletes the row — "no row" and "all-defaults row" are the same state.
// The fileTransfer channel is inert in this version (T11, v1.3): the
// schema carries it so the write is already shaped correctly, but any
// non-default fileTransfer value is a 400 until the channel goes live.
func (h *AdminSessionPolicyHandler) Put(w http.ResponseWriter, r *http.Request) {
	if _, ok := tenantAdminPrincipal(h.tenants, w, r); !ok {
		return
	}
	tenant := r.PathValue("tenant")
	ifMatch := r.Header.Get("If-Match")
	if ifMatch == "" {
		writeError(w, r, CodeInvalidRequest, "If-Match header required")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, h.maxBody))
	if err != nil {
		writeError(w, r, CodeInvalidRequest, "unreadable or oversized body")
		return
	}
	var req putSessionPolicyRequest
	if decodeJSON(body, &req) != nil {
		writeError(w, r, CodeInvalidRequest, "invalid request body")
		return
	}
	if req.Clipboard == nil {
		writeError(w, r, CodeInvalidRequest, "clipboard is required")
		return
	}
	clip := *req.Clipboard
	if clip != provisioning.PolicyFollowTemplate &&
		!validCeilingValue(clip, provisioning.ClipboardSend, provisioning.ClipboardReceive) {
		writeError(w, r, CodeInvalidRequest,
			"clipboard must be one of FollowTemplate, Disabled, Send, Receive, Bidirectional")
		return
	}
	// fileTransfer is the inert T11 channel: the schema carries it so T11
	// is additive, but only the defaults write. A non-default mode or a
	// maxFileBytes bound is refused — a stored value would otherwise arm
	// silently for v1.3.
	if req.FileTransfer != nil {
		ft := req.FileTransfer
		mode := provisioning.PolicyFollowTemplate
		if ft.Mode != nil {
			mode = *ft.Mode
		}
		if mode != provisioning.PolicyFollowTemplate || ft.MaxFileBytes != nil {
			writeError(w, r, CodeInvalidRequest,
				"fileTransfer is inert in this version; only FollowTemplate is accepted")
			return
		}
	}
	// Record the attempted write on the in-flight audit event — success
	// or refusal carries it alike.
	auditSetDetail(r.Context(), "clipboard", clip)
	var write store.SessionPolicyWrite
	if clip != provisioning.PolicyFollowTemplate {
		write.Clipboard = &clip
	}
	if write.Defaults() {
		// Every channel follows the template: the write resolves to the
		// row's deletion, audited as the clear variant.
		auditSetAction(r.Context(), auditActionAdminSessionPolicyClear)
	}
	if err := h.source.SetCeiling(r.Context(), tenant, write, ifMatch); err != nil {
		if errors.Is(err, store.ErrSessionPolicyVersionMismatch) {
			writeError(w, r, CodePreconditionFailed,
				"the session policy changed since it was read; reload and retry")
			return
		}
		writeStoreError(w, r, err)
		return
	}
	out, err := h.view(r.Context(), tenant)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	respondJSON(w, out)
}
