// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package api

import (
	"context"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/tinyorbitvn/tinycdi/internal/observability"
	"github.com/tinyorbitvn/tinycdi/internal/store"
)

// UserLimitSource is the read/write seam behind
// /v1/admin/tenants/{tenant}/user-limits; the Postgres implementation is
// adminUserLimitStore. Report shares the QuotaReader used by GET
// /v1/quota so the view counts exactly what admission counts.
type UserLimitSource interface {
	Report(ctx context.Context, tenantID string) (store.QuotaReport, error)
	// SetOverride/ClearOverride write the per-principal running limit;
	// clearing restores the tenant default (or unlimited). SetDefault/
	// ClearDefault write the tenant-wide fallback; clearing leaves every
	// principal without an override unlimited.
	SetOverride(ctx context.Context, tenantID, owner string, max int64) error
	ClearOverride(ctx context.Context, tenantID, owner string) error
	SetDefault(ctx context.Context, tenantID string, max int64) error
	ClearDefault(ctx context.Context, tenantID string) error
}

// adminUserLimitStore is the Postgres UserLimitSource.
type adminUserLimitStore struct {
	limits *store.UserLimitStore
	q      *store.QuotaReader
}

func (s *adminUserLimitStore) Report(ctx context.Context, tenantID string) (store.QuotaReport, error) {
	return s.q.Report(ctx, tenantID)
}
func (s *adminUserLimitStore) SetOverride(ctx context.Context, tenantID, owner string, max int64) error {
	return s.limits.SetOverride(ctx, tenantID, owner, max)
}
func (s *adminUserLimitStore) ClearOverride(ctx context.Context, tenantID, owner string) error {
	return s.limits.ClearOverride(ctx, tenantID, owner)
}
func (s *adminUserLimitStore) SetDefault(ctx context.Context, tenantID string, max int64) error {
	return s.limits.SetDefault(ctx, tenantID, max)
}
func (s *adminUserLimitStore) ClearDefault(ctx context.Context, tenantID string) error {
	return s.limits.ClearDefault(ctx, tenantID)
}

// NewAdminUserLimitSource wraps the Postgres stores.
func NewAdminUserLimitSource(db *store.DB) UserLimitSource {
	return &adminUserLimitStore{limits: store.NewUserLimitStore(db), q: store.NewQuotaReader(db)}
}

// adminUserLimitsView is the response of GET (and the two PUTs on)
// /v1/admin/tenants/{tenant}/user-limits (openapi AdminUserLimitsView):
// the tenant default (null = unlimited) and one row per principal who
// has usage or an override, ordered by owner reference.
type adminUserLimitsView struct {
	Tenant  string                `json:"tenant"`
	Default *int64                `json:"default"`
	Users   []adminUserLimitEntry `json:"users"`
}

// adminUserLimitEntry is one principal's row (openapi
// AdminUserLimitEntry): the stored override (null = inherit the default),
// the resolved effective limit (null = unlimited) and the running
// workspaces currently held.
type adminUserLimitEntry struct {
	OwnerRef    string `json:"ownerRef"`
	Subject     string `json:"subject"`
	DisplayName string `json:"displayName"`
	Limit       *int64 `json:"limit"`
	Effective   *int64 `json:"effective"`
	Running     int64  `json:"running"`
}

// setUserLimitRequest is the PUT /v1/admin/tenants/{tenant}/user-limits
// body: limit sets the override, null (or absent) clears it.
type setUserLimitRequest struct {
	OwnerRef string `json:"ownerRef"`
	Limit    *int64 `json:"limit"`
}

// setUserLimitDefaultRequest is the PUT
// /v1/admin/tenants/{tenant}/user-limits/default body: limit sets the
// tenant default, null (or absent) clears it back to unlimited.
type setUserLimitDefaultRequest struct {
	Limit *int64 `json:"limit"`
}

// AdminUserLimitsHandler implements GET and PUT on
// /v1/admin/tenants/{tenant}/user-limits[...]. Unlike tenant quota,
// per-principal limits are API-managed only — there is no config
// declaration — so writes are never refused as QUOTA_MANAGED_BY_CONFIG.
// Every request emits a dedicated audit record through the audited
// wrapper (see MountAdminUserLimitRoutes).
type AdminUserLimitsHandler struct {
	source  UserLimitSource
	dir     Directory
	tenants TenantResolver
	audit   observability.AuditSink
	maxBody int64
}

// NewAdminUserLimitsHandler wires the handler. dir may be nil; owner
// display names then fall back to subjects.
func NewAdminUserLimitsHandler(src UserLimitSource, dir Directory, t TenantResolver) *AdminUserLimitsHandler {
	return &AdminUserLimitsHandler{source: src, dir: dir, tenants: t, maxBody: 16 << 10}
}

// WithAuditSink attaches the audit sink the user-limit routes emit their
// dedicated audit events to (nil = no domain audit events).
func (h *AdminUserLimitsHandler) WithAuditSink(s observability.AuditSink) *AdminUserLimitsHandler {
	h.audit = s
	return h
}

// MountAdminUserLimitRoutes registers the routes audited (see
// MountAdminQuotaRoutes): RequireAuthPassive+audit on the read,
// RequireAuth+audit+RequireCSRF on the writes — denied (non-admin,
// cross-tenant) attempts emit the route's table action with outcome
// denied; a write that decodes resolves to the set or clear variant.
func MountAdminUserLimitRoutes(mux *http.ServeMux, authn *Authenticator, h *AdminUserLimitsHandler) {
	mux.Handle(routeAdminUserLimitsGet, authn.RequireAuthPassive(
		audited(h.audit, routeAdminUserLimitsGet, http.HandlerFunc(h.Get))))
	mux.Handle(routeAdminUserLimitsPut, authn.RequireAuth(
		audited(h.audit, routeAdminUserLimitsPut,
			authn.RequireCSRF(http.HandlerFunc(h.Put)))))
	mux.Handle(routeAdminUserLimitDefault, authn.RequireAuth(
		audited(h.audit, routeAdminUserLimitDefault,
			authn.RequireCSRF(http.HandlerFunc(h.PutDefault)))))
}

// Get handles GET /v1/admin/tenants/{tenant}/user-limits.
func (h *AdminUserLimitsHandler) Get(w http.ResponseWriter, r *http.Request) {
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

// Put handles PUT /v1/admin/tenants/{tenant}/user-limits: it sets (limit)
// or clears (null) the named principal's running-workspace override.
// ownerRef is the "issuer|sub" pair workspaces store; a limit on a
// principal with no usage is accepted — it applies at their next launch.
// Writes are upserts (last write wins) and audited.
func (h *AdminUserLimitsHandler) Put(w http.ResponseWriter, r *http.Request) {
	if _, ok := tenantAdminPrincipal(h.tenants, w, r); !ok {
		return
	}
	tenant := r.PathValue("tenant")
	var req setUserLimitRequest
	if !h.decodeBody(w, r, &req) {
		return
	}
	if msg := validOwnerRef(req.OwnerRef); msg != "" {
		writeError(w, r, CodeInvalidRequest, msg)
		return
	}
	if req.Limit != nil && *req.Limit < 0 {
		writeError(w, r, CodeInvalidRequest, "limit must be a non-negative integer")
		return
	}
	// Record the resolved action and the attempted write on the in-flight
	// audit event — success or refusal carries them alike.
	auditSetDetail(r.Context(), "owner", targetActorRef(req.OwnerRef))
	var err error
	if req.Limit == nil {
		auditSetAction(r.Context(), auditActionAdminUserLimitClear)
		err = h.source.ClearOverride(r.Context(), tenant, req.OwnerRef)
	} else {
		auditSetDetail(r.Context(), "max_running", strconv.FormatInt(*req.Limit, 10))
		err = h.source.SetOverride(r.Context(), tenant, req.OwnerRef, *req.Limit)
	}
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	h.respondView(w, r, tenant)
}

// PutDefault handles PUT /v1/admin/tenants/{tenant}/user-limits/default:
// it sets (limit) or clears (null) the tenant-wide fallback.
func (h *AdminUserLimitsHandler) PutDefault(w http.ResponseWriter, r *http.Request) {
	if _, ok := tenantAdminPrincipal(h.tenants, w, r); !ok {
		return
	}
	tenant := r.PathValue("tenant")
	var req setUserLimitDefaultRequest
	if !h.decodeBody(w, r, &req) {
		return
	}
	if req.Limit != nil && *req.Limit < 0 {
		writeError(w, r, CodeInvalidRequest, "limit must be a non-negative integer")
		return
	}
	var err error
	if req.Limit == nil {
		auditSetAction(r.Context(), auditActionAdminUserLimitDefaultClear)
		err = h.source.ClearDefault(r.Context(), tenant)
	} else {
		auditSetDetail(r.Context(), "max_running", strconv.FormatInt(*req.Limit, 10))
		err = h.source.SetDefault(r.Context(), tenant, *req.Limit)
	}
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	h.respondView(w, r, tenant)
}

func (h *AdminUserLimitsHandler) decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, h.maxBody))
	if err != nil {
		writeError(w, r, CodeInvalidRequest, "unreadable or oversized body")
		return false
	}
	if !decodeJSON(body, v) {
		writeError(w, r, CodeInvalidRequest, "invalid request body")
		return false
	}
	return true
}

func (h *AdminUserLimitsHandler) respondView(w http.ResponseWriter, r *http.Request, tenant string) {
	out, err := h.view(r.Context(), tenant)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	respondJSON(w, out)
}

// writeStoreError maps a store-level failure onto the honest status: a
// transport outage (the request never reached a store verdict) answers a
// retryable 503 UNAVAILABLE, everything else a plain 500.
func writeStoreError(w http.ResponseWriter, r *http.Request, err error) {
	if store.IsTransient(err) {
		writeError(w, r, CodeUnavailable, "service unavailable")
		return
	}
	writeError(w, r, CodeInternal, "internal error")
}

// view unions the principals with usage and the ones with a stored
// override so a limit row is always visible even before its owner runs
// anything.
func (h *AdminUserLimitsHandler) view(ctx context.Context, tenantID string) (adminUserLimitsView, error) {
	rep, err := h.source.Report(ctx, tenantID)
	if err != nil {
		return adminUserLimitsView{}, err
	}
	refs := make([]string, 0, len(rep.Owners)+len(rep.UserLimits))
	running := map[string]int64{}
	seen := map[string]bool{}
	for _, o := range rep.Owners {
		refs = append(refs, o.OwnerRef)
		running[o.OwnerRef] = o.Usage.RunningSlots
		seen[o.OwnerRef] = true
	}
	var extra []string
	for owner := range rep.UserLimits {
		if !seen[owner] {
			extra = append(extra, owner)
		}
	}
	slices.Sort(extra)
	refs = append(refs, extra...)
	owners := resolveOwners(ctx, h.dir, tenantID, refs)
	out := adminUserLimitsView{
		Tenant:  tenantID,
		Default: rep.DefaultUserLimit,
		Users:   make([]adminUserLimitEntry, 0, len(refs)),
	}
	for _, ref := range refs {
		owner := owners[ref]
		entry := adminUserLimitEntry{
			OwnerRef:    ref,
			Subject:     owner.Subject,
			DisplayName: owner.DisplayName,
			Effective:   effectiveUserLimit(rep, ref),
			Running:     running[ref],
		}
		if v, ok := rep.UserLimits[ref]; ok {
			l := v
			entry.Limit = &l
		}
		out.Users = append(out.Users, entry)
	}
	return out, nil
}

// targetActorRef pseudonymizes an "issuer|sub" owner reference for audit
// records — the raw subject never reaches the log.
func targetActorRef(ownerRef string) string {
	iss, sub, _ := strings.Cut(ownerRef, "|")
	return observability.ActorRef(iss, sub)
}

// validOwnerRef checks the "issuer|sub" shape admission stores in
// workspaces.owner_subject: exactly a non-empty issuer and sub separated
// by the first '|', bounded length.
func validOwnerRef(ref string) string {
	if len(ref) == 0 || len(ref) > 512 {
		return "ownerRef must be an issuer|sub reference (1..512 chars)"
	}
	i := strings.IndexByte(ref, '|')
	if i <= 0 || i == len(ref)-1 {
		return "ownerRef must be an issuer|sub reference"
	}
	return ""
}
