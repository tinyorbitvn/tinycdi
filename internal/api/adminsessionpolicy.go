// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/tinyorbitvn/tinycdi/internal/observability"
	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
	"github.com/tinyorbitvn/tinycdi/internal/store"
)

// SessionPolicySource is the read/write seam behind
// /v1/admin/tenants/{tenant}/session-policy; the Postgres implementation
// is store.SessionPolicyStore.
type SessionPolicySource interface {
	Ceiling(ctx context.Context, tenantID string) (store.SessionPolicyCeiling, error)
	// SetCeiling writes the tenant's ceiling conditionally and returns
	// the row's new version ("" after a delete): create expects no row,
	// otherwise expectedVersion must be the row's current
	// SessionPolicyCeiling.Version. A miss answers
	// store.ErrVersionMismatch. An all-FollowTemplate write deletes the
	// row — "no row" and "all-defaults row" are the same state.
	SetCeiling(ctx context.Context, tenantID string, w store.SessionPolicyWrite, expectedVersion string, create bool) (string, error)
}

// adminSessionPolicyStore adapts the Postgres ceiling store to the
// handler's seam.
type adminSessionPolicyStore struct {
	s *store.SessionPolicyStore
}

func (a *adminSessionPolicyStore) Ceiling(ctx context.Context, tenantID string) (store.SessionPolicyCeiling, error) {
	return a.s.Ceiling(ctx, tenantID)
}

func (a *adminSessionPolicyStore) SetCeiling(ctx context.Context, tenantID string, w store.SessionPolicyWrite, expectedVersion string, create bool) (string, error) {
	return a.s.SetCeiling(ctx, tenantID, w, expectedVersion, create)
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
// mode narrows the file channel the same way clipboard does;
// maxFileBytes lowers the per-file bound below the platform bound.
type adminFileTransferCeiling struct {
	Mode         string `json:"mode"`
	MaxFileBytes *int64 `json:"maxFileBytes"`
}

// adminSessionPolicyView is the response of GET/PUT
// /v1/admin/tenants/{tenant}/session-policy (openapi
// AdminSessionPolicyView): the tenant's ceiling per channel
// (FollowTemplate = not narrowed) and the source (TenantRow |
// FollowTemplate). The row's version travels as the ETag header.
type adminSessionPolicyView struct {
	Tenant       string                   `json:"tenant"`
	Clipboard    string                   `json:"clipboard"`
	FileTransfer adminFileTransferCeiling `json:"fileTransfer"`
	Source       string                   `json:"source"`
}

// putSessionPolicyRequest is the PUT body (openapi
// PutAdminSessionPolicy). clipboard is required; fileTransfer narrows
// the file channel and lowers the per-file bound.
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
// ceiling of ADR 0009 §4. Unlike quota there is no config declaration
// for session policy: every row is API-managed, so writes are never
// refused as managed-by-config.
type AdminSessionPolicyHandler struct {
	source  SessionPolicySource
	tenants TenantResolver
	maxBody int64
	// fileBound is the platform per-file bound
	// (-file-transfer-max-bytes) a ceiling write's maxFileBytes is
	// checked against; 0 leaves it unchecked.
	fileBound int64
	// audit is the dedicated audit-event sink the admin routes emit
	// through (nil = no domain audit events).
	audit observability.AuditSink
	// apply is the provisioning/store seam :apply restarts through;
	// running is the informer view of applied policies; dir resolves owner
	// display names. nil apply/running makes :apply answer 503.
	apply   PolicyApplyBackend
	running RunningPolicySource
	dir     Directory
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

// WithFileTransferBound sets the platform per-file bound
// (-file-transfer-max-bytes) a PUT's fileTransfer.maxFileBytes may not
// exceed.
func (h *AdminSessionPolicyHandler) WithFileTransferBound(bound int64) *AdminSessionPolicyHandler {
	h.fileBound = bound
	return h
}

// MountAdminSessionPolicyRoutes registers the routes audited (see
// MountAdminQuotaRoutes): RequireAuthPassive+audit on the read,
// RequireAuth+audit+RequireCSRF on the write.
func MountAdminSessionPolicyRoutes(mux RouteMux, authn *Authenticator, h *AdminSessionPolicyHandler) {
	mux.Handle(routeAdminSessionPolicyGet, authn.RequireAuthPassive(
		audited(h.audit, routeAdminSessionPolicyGet, http.HandlerFunc(h.Get))))
	mux.Handle(routeAdminSessionPolicyPut, authn.RequireAuth(
		audited(h.audit, routeAdminSessionPolicyPut,
			authn.RequireCSRF(http.HandlerFunc(h.Put)))))
	mux.Handle(routeAdminSessionPolicyApply, authn.RequireAuth(
		audited(h.audit, routeAdminSessionPolicyApply,
			authn.RequireCSRF(http.HandlerFunc(h.Apply)))))
}

// view renders a ceiling row as the admin view.
func (h *AdminSessionPolicyHandler) view(tenantID string, c store.SessionPolicyCeiling) adminSessionPolicyView {
	out := adminSessionPolicyView{
		Tenant:    tenantID,
		Clipboard: provisioning.PolicyFollowTemplate,
		FileTransfer: adminFileTransferCeiling{
			Mode: provisioning.PolicyFollowTemplate,
		},
		Source: SessionPolicySourceFollowTemplate,
	}
	if !c.HasRow {
		return out
	}
	out.Source = SessionPolicySourceTenantRow
	if c.Clipboard != nil {
		out.Clipboard = *c.Clipboard
	}
	if c.FileTransfer != nil {
		out.FileTransfer.Mode = *c.FileTransfer
	}
	out.FileTransfer.MaxFileBytes = c.FileMaxBytes
	return out
}

// respond writes the tenant's current ceiling with its ETag (absent while
// no row exists).
func (h *AdminSessionPolicyHandler) respond(w http.ResponseWriter, r *http.Request, tenantID string) {
	c, err := h.source.Ceiling(r.Context(), tenantID)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	if c.HasRow {
		w.Header().Set("ETag", ETagOf(c.Version))
	}
	respondJSON(w, h.view(tenantID, c))
}

// Get handles GET /v1/admin/tenants/{tenant}/session-policy.
func (h *AdminSessionPolicyHandler) Get(w http.ResponseWriter, r *http.Request) {
	if _, ok := tenantAdminPrincipal(h.tenants, w, r); !ok {
		return
	}
	h.respond(w, r, r.PathValue("tenant"))
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
// the tenant's ceiling provided If-Match names the current ETag (428
// PRECONDITION_REQUIRED without it, 412 PRECONDITION_FAILED when stale;
// "*" creates only a missing row). A PUT whose every channel is FollowTemplate
// deletes the row — "no row" and "all-defaults row" are the same state.
// fileTransfer.mode narrows the file channel; maxFileBytes lowers the
// per-file bound and may not exceed the platform bound
// (-file-transfer-max-bytes).
func (h *AdminSessionPolicyHandler) Put(w http.ResponseWriter, r *http.Request) {
	if _, ok := tenantAdminPrincipal(h.tenants, w, r); !ok {
		return
	}
	tenant := r.PathValue("tenant")
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
	var write store.SessionPolicyWrite
	if clip != provisioning.PolicyFollowTemplate {
		write.Clipboard = &clip
	}
	if req.FileTransfer != nil {
		ft := req.FileTransfer
		if ft.Mode != nil && *ft.Mode != provisioning.PolicyFollowTemplate {
			if !validCeilingValue(*ft.Mode, provisioning.FileTransferUpload, provisioning.FileTransferDownload) {
				writeError(w, r, CodeInvalidRequest,
					"fileTransfer.mode must be one of FollowTemplate, Disabled, Upload, Download, Bidirectional")
				return
			}
			mode := *ft.Mode
			write.FileTransfer = &mode
			auditSetDetail(r.Context(), "policy", "fileTransfer="+mode)
		}
		if ft.MaxFileBytes != nil {
			if *ft.MaxFileBytes < 1 || (h.fileBound > 0 && *ft.MaxFileBytes > h.fileBound) {
				writeError(w, r, CodeInvalidRequest,
					fmt.Sprintf("fileTransfer.maxFileBytes must be 1..%d (the platform bound)", h.fileBound))
				return
			}
			n := *ft.MaxFileBytes
			write.FileMaxBytes = &n
			auditSetDetail(r.Context(), "bytes", strconv.FormatInt(n, 10))
		}
	}
	// Record the attempted write on the in-flight audit event — success
	// or refusal carries it alike.
	auditSetDetail(r.Context(), "clipboard", clip)
	if write.Defaults() {
		// Every channel follows the template: the write resolves to the
		// row's deletion, audited as the clear variant.
		auditSetAction(r.Context(), auditActionAdminSessionPolicyClear)
	}
	cur, err := h.source.Ceiling(r.Context(), tenant)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	if e := CheckIfMatch(r, cur.Version, cur.HasRow); e != nil {
		WriteError(w, r, e)
		return
	}
	if _, err := h.source.SetCeiling(r.Context(), tenant, write, cur.Version, !cur.HasRow); err != nil {
		if errors.Is(err, store.ErrVersionMismatch) {
			writeError(w, r, CodePreconditionFailed,
				"the session policy changed since it was read; reload and retry")
			return
		}
		writeStoreError(w, r, err)
		return
	}
	h.respond(w, r, tenant)
}
