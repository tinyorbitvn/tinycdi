// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/tinyorbitvn/tinycdi/internal/observability"
	"github.com/tinyorbitvn/tinycdi/internal/store"
)

// UserLimitSource is the read/write seam behind
// /v1/admin/tenants/{tenant}/user-limits[...]; the Postgres implementation
// is adminUserLimitStore. Report shares the QuotaReader used by GET
// /v1/quota so the view counts exactly what admission counts, and carries
// each row's version (UserLimitVersions, DefaultUserLimitVersion).
type UserLimitSource interface {
	Report(ctx context.Context, tenantID string) (store.QuotaReport, error)
	// The writes are conditional (see store.UserLimitStore): create
	// expects no row, otherwise expectedVersion must name the current
	// one; a miss is store.ErrVersionMismatch. Set* return the row's new
	// version. Clearing an override restores the tenant default (or
	// unlimited); clearing the default leaves every principal without an
	// override unlimited.
	SetOverride(ctx context.Context, tenantID, owner string, max int64, expectedVersion string, create bool) (string, error)
	ClearOverride(ctx context.Context, tenantID, owner, expectedVersion string, create bool) (string, error)
	SetDefault(ctx context.Context, tenantID string, max int64, expectedVersion string, create bool) (string, error)
	ClearDefault(ctx context.Context, tenantID, expectedVersion string, create bool) (string, error)
}

// adminUserLimitStore is the Postgres UserLimitSource.
type adminUserLimitStore struct {
	*store.UserLimitStore
	q *store.QuotaReader
}

func (s *adminUserLimitStore) Report(ctx context.Context, tenantID string) (store.QuotaReport, error) {
	return s.q.Report(ctx, tenantID)
}

// NewAdminUserLimitSource wraps the Postgres stores.
func NewAdminUserLimitSource(db *store.DB) UserLimitSource {
	return &adminUserLimitStore{UserLimitStore: store.NewUserLimitStore(db), q: store.NewQuotaReader(db)}
}

// adminUserLimitEntry is one principal's row of the paged collection GET
// /v1/admin/tenants/{tenant}/user-limits (openapi AdminUserLimitEntry):
// the stored override (null = inherit the default), the resolved effective
// limit (null = unlimited), the running workspaces currently held, and the
// override row's own strong ETag — absent while the principal has no
// override (PUT then creates it with If-Match "*").
type adminUserLimitEntry struct {
	OwnerRef    string `json:"ownerRef"`
	Subject     string `json:"subject"`
	DisplayName string `json:"displayName"`
	Limit       *int64 `json:"limit"`
	Effective   *int64 `json:"effective"`
	Running     int64  `json:"running"`
	ETag        string `json:"etag,omitempty"`
}

// adminUserLimitDefault is the tenant default resource (openapi
// AdminUserLimitDefault): limit null = unlimited.
type adminUserLimitDefault struct {
	Tenant string `json:"tenant"`
	Limit  *int64 `json:"limit"`
}

// setUserLimitRequest is the PUT
// /v1/admin/tenants/{tenant}/user-limits/{ownerRef} body; the limit is
// required (an override is removed by DELETE, not by a null).
type setUserLimitRequest struct {
	Limit *int64 `json:"limit"`
}

// setUserLimitDefaultRequest is the PUT
// /v1/admin/tenants/{tenant}/user-limits/default body: limit sets the
// tenant default, null (or absent) clears it back to unlimited.
type setUserLimitDefaultRequest struct {
	Limit *int64 `json:"limit"`
}

// userLimitSorts is the sort allowlist of the user-limits collection.
var userLimitSorts = []string{"ownerRef"}

// AdminUserLimitsHandler implements /v1/admin/tenants/{tenant}/user-limits
// (paged list), .../user-limits/{ownerRef} (PUT, DELETE) and
// .../user-limits/default (GET, PUT). Every mutation is a strong-ETag
// precondition write. Unlike tenant quota, per-principal limits are
// API-managed only — there is no config declaration — so writes are never
// refused as QUOTA_MANAGED_BY_CONFIG. Every request emits a dedicated
// audit record through the audited wrapper (see MountAdminUserLimitRoutes).
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
// MountAdminQuotaRoutes): RequireAuthPassive+audit on the reads,
// RequireAuth+audit+RequireCSRF on the writes — denied (non-admin,
// cross-tenant) attempts emit the route's table action with outcome
// denied.
func MountAdminUserLimitRoutes(mux RouteMux, authn *Authenticator, h *AdminUserLimitsHandler) {
	mux.Handle(routeAdminUserLimitsGet, authn.RequireAuthPassive(
		audited(h.audit, routeAdminUserLimitsGet, http.HandlerFunc(h.List))))
	mux.Handle(routeAdminUserLimitPut, authn.RequireAuth(
		audited(h.audit, routeAdminUserLimitPut,
			authn.RequireCSRF(http.HandlerFunc(h.Put)))))
	mux.Handle(routeAdminUserLimitDelete, authn.RequireAuth(
		audited(h.audit, routeAdminUserLimitDelete,
			authn.RequireCSRF(http.HandlerFunc(h.Delete)))))
	mux.Handle(routeAdminUserLimitDefaultGet, authn.RequireAuthPassive(
		audited(h.audit, routeAdminUserLimitDefaultGet, http.HandlerFunc(h.GetDefault))))
	mux.Handle(routeAdminUserLimitDefault, authn.RequireAuth(
		audited(h.audit, routeAdminUserLimitDefault,
			authn.RequireCSRF(http.HandlerFunc(h.PutDefault)))))
}

// List handles GET /v1/admin/tenants/{tenant}/user-limits: one keyset page
// of the principals with usage or an override, ordered by ownerRef.
func (h *AdminUserLimitsHandler) List(w http.ResponseWriter, r *http.Request) {
	if _, ok := tenantAdminPrincipal(h.tenants, w, r); !ok {
		return
	}
	page, perr := ParsePage(r, userLimitSorts, "ownerRef")
	if perr != nil {
		WriteError(w, r, perr)
		return
	}
	tenant := r.PathValue("tenant")
	rep, err := h.source.Report(r.Context(), tenant)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	refs := userLimitRefs(rep)
	byRef := func(e adminUserLimitEntry) string { return e.OwnerRef }
	// Page over the bare refs first so the directory lookup covers only
	// the rows this page returns.
	stubs := make([]adminUserLimitEntry, len(refs))
	for i, ref := range refs {
		stubs[i] = adminUserLimitEntry{OwnerRef: ref}
	}
	out := pageInMemory(stubs, page, byRef, byRef)
	pageRefs := make([]string, len(out.Items))
	for i, it := range out.Items {
		pageRefs[i] = it.OwnerRef
	}
	owners := resolveOwners(r.Context(), h.dir, tenant, pageRefs)
	for i, ref := range pageRefs {
		out.Items[i] = h.entry(rep, ref, owners[ref])
	}
	respondJSON(w, out)
}

// Put handles PUT /v1/admin/tenants/{tenant}/user-limits/{ownerRef}: it
// writes the principal's running-workspace override under If-Match ("*"
// creates it). ownerRef is the "issuer|sub" pair workspaces store; a limit
// on a principal with no usage is accepted — it applies at their next
// launch. The response is the item with its new ETag.
func (h *AdminUserLimitsHandler) Put(w http.ResponseWriter, r *http.Request) {
	if _, ok := tenantAdminPrincipal(h.tenants, w, r); !ok {
		return
	}
	tenant, owner := r.PathValue("tenant"), r.PathValue("ownerRef")
	if msg := validOwnerRef(owner); msg != "" {
		writeError(w, r, CodeInvalidRequest, msg)
		return
	}
	var req setUserLimitRequest
	if !h.decodeBody(w, r, &req) {
		return
	}
	if req.Limit == nil || *req.Limit < 0 {
		writeError(w, r, CodeInvalidRequest, "limit must be a non-negative integer")
		return
	}
	// Record the attempted write on the in-flight audit event — success
	// or refusal carries it alike.
	auditSetDetail(r.Context(), "owner", targetActorRef(owner))
	auditSetDetail(r.Context(), "max_running", strconv.FormatInt(*req.Limit, 10))
	rep, err := h.source.Report(r.Context(), tenant)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	cur, exists := rep.UserLimitVersions[owner]
	if e := CheckIfMatch(r, cur, exists); e != nil {
		WriteError(w, r, e)
		return
	}
	if _, err := h.source.SetOverride(r.Context(), tenant, owner, *req.Limit, cur, !exists); err != nil {
		h.writeWriteError(w, r, err)
		return
	}
	rep, err = h.source.Report(r.Context(), tenant)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	it := h.entry(rep, owner, resolveOwners(r.Context(), h.dir, tenant, []string{owner})[owner])
	if it.ETag != "" {
		w.Header().Set("ETag", it.ETag)
	}
	respondJSON(w, it)
}

// Delete handles DELETE /v1/admin/tenants/{tenant}/user-limits/{ownerRef}:
// it removes the principal's override under If-Match, restoring the tenant
// default (or unlimited). A principal without an override is 404.
func (h *AdminUserLimitsHandler) Delete(w http.ResponseWriter, r *http.Request) {
	if _, ok := tenantAdminPrincipal(h.tenants, w, r); !ok {
		return
	}
	tenant, owner := r.PathValue("tenant"), r.PathValue("ownerRef")
	if msg := validOwnerRef(owner); msg != "" {
		writeError(w, r, CodeInvalidRequest, msg)
		return
	}
	auditSetDetail(r.Context(), "owner", targetActorRef(owner))
	rep, err := h.source.Report(r.Context(), tenant)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	cur, exists := rep.UserLimitVersions[owner]
	if e := CheckIfMatch(r, cur, exists); e != nil && (exists || e.Code == CodePreconditionRequired) {
		WriteError(w, r, e)
		return
	}
	if !exists {
		writeError(w, r, CodeNotFound, "no limit override for this principal")
		return
	}
	if _, err := h.source.ClearOverride(r.Context(), tenant, owner, cur, false); err != nil {
		h.writeWriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// GetDefault handles GET /v1/admin/tenants/{tenant}/user-limits/default.
func (h *AdminUserLimitsHandler) GetDefault(w http.ResponseWriter, r *http.Request) {
	if _, ok := tenantAdminPrincipal(h.tenants, w, r); !ok {
		return
	}
	h.respondDefault(w, r, r.PathValue("tenant"))
}

// PutDefault handles PUT /v1/admin/tenants/{tenant}/user-limits/default:
// under If-Match ("*" when no default exists) it sets (limit) or clears
// (null) the tenant-wide fallback.
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
	if req.Limit == nil {
		auditSetAction(r.Context(), auditActionAdminUserLimitDefaultClear)
	} else {
		auditSetDetail(r.Context(), "max_running", strconv.FormatInt(*req.Limit, 10))
	}
	rep, err := h.source.Report(r.Context(), tenant)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	cur, exists := rep.DefaultUserLimitVersion, rep.DefaultUserLimit != nil
	if e := CheckIfMatch(r, cur, exists); e != nil {
		WriteError(w, r, e)
		return
	}
	if req.Limit == nil {
		_, err = h.source.ClearDefault(r.Context(), tenant, cur, !exists)
	} else {
		_, err = h.source.SetDefault(r.Context(), tenant, *req.Limit, cur, !exists)
	}
	if err != nil {
		h.writeWriteError(w, r, err)
		return
	}
	h.respondDefault(w, r, tenant)
}

func (h *AdminUserLimitsHandler) respondDefault(w http.ResponseWriter, r *http.Request, tenant string) {
	rep, err := h.source.Report(r.Context(), tenant)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	if rep.DefaultUserLimit != nil {
		w.Header().Set("ETag", ETagOf(rep.DefaultUserLimitVersion))
	}
	respondJSON(w, adminUserLimitDefault{Tenant: tenant, Limit: rep.DefaultUserLimit})
}

func (h *AdminUserLimitsHandler) decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, h.maxBody))
	if err != nil {
		writeError(w, r, CodeInvalidRequest, "unreadable or oversized body")
		return false
	}
	if decodeJSON(body, v) != nil {
		writeError(w, r, CodeInvalidRequest, "invalid request body")
		return false
	}
	return true
}

// writeWriteError maps a conditional write's failure: a version miss
// (a write raced past the handler's If-Match check) is 412, the rest
// goes through writeStoreError.
func (h *AdminUserLimitsHandler) writeWriteError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, store.ErrVersionMismatch) {
		writeError(w, r, CodePreconditionFailed, "the limit changed since it was read; reload and retry")
		return
	}
	writeStoreError(w, r, err)
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

// userLimitRefs unions the principals with usage and the ones with a
// stored override, so a limit row is visible even before its owner runs
// anything.
func userLimitRefs(rep store.QuotaReport) []string {
	seen := map[string]bool{}
	var refs []string
	for _, o := range rep.Owners {
		if !seen[o.OwnerRef] {
			seen[o.OwnerRef] = true
			refs = append(refs, o.OwnerRef)
		}
	}
	for owner := range rep.UserLimits {
		if !seen[owner] {
			seen[owner] = true
			refs = append(refs, owner)
		}
	}
	return refs
}

// entry resolves one principal's row from a quota report.
func (h *AdminUserLimitsHandler) entry(rep store.QuotaReport, ref string, owner Owner) adminUserLimitEntry {
	return userLimitEntry(rep, ref, owner)
}

// userLimitEntry resolves one principal's row from a quota report.
func userLimitEntry(rep store.QuotaReport, ref string, owner Owner) adminUserLimitEntry {
	e := adminUserLimitEntry{
		OwnerRef:    ref,
		Subject:     owner.Subject,
		DisplayName: owner.DisplayName,
		Effective:   effectiveUserLimit(rep, ref),
	}
	for _, o := range rep.Owners {
		if o.OwnerRef == ref {
			e.Running = o.Usage.RunningSlots
		}
	}
	if v, ok := rep.UserLimits[ref]; ok {
		l := v
		e.Limit = &l
		e.ETag = ETagOf(rep.UserLimitVersions[ref])
	}
	return e
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
