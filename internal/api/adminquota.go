// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package api

import (
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tinyorbitvn/tinycdi/internal/observability"
	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
	"github.com/tinyorbitvn/tinycdi/internal/store"
)

// IfMatchCreate is the If-Match value a client sends to create a resource
// that does not exist yet (GET reported no ETag). It is the only accepted
// precondition on a missing row, and it is refused (412) on an existing
// one — "*" can never skip the concurrency check.
const IfMatchCreate = "*"

// AdminQuotaSource is the read/write seam behind
// /v1/admin/tenants/{tenant}/quota; the Postgres implementation is
// adminQuotaStore.
type AdminQuotaSource interface {
	Report(ctx context.Context, tenantID string) (store.QuotaReport, error)
	// SetLimits writes the tenant's configured limits conditionally and
	// returns the row's new version: create expects no row, otherwise
	// expectedVersion must be the row's current QuotaReport.Version. A
	// miss answers store.ErrVersionMismatch. Limits below current usage
	// are accepted: held reservations stay, new ones are refused (same
	// rule as provisioning.UpsertQuota).
	SetLimits(ctx context.Context, tenantID string, v provisioning.ResourceVector, expectedVersion string, create bool) (string, error)
}

// adminQuotaStore is the Postgres AdminQuotaSource: reads share the
// QuotaReader used by GET /v1/quota so the admin view counts exactly what
// admission counts (held reservations — including disk-only holds — plus
// active workspaces).
type adminQuotaStore struct {
	db *store.DB
	q  *store.QuotaReader
}

func (s *adminQuotaStore) Report(ctx context.Context, tenantID string) (store.QuotaReport, error) {
	return s.q.Report(ctx, tenantID)
}

// SetLimits performs the conditional write in one statement so the version
// check and the write cannot be raced: an update matches only while the
// row's updated_at still equals expectedVersion; a create inserts only when
// no row exists. Writes stamp updated_at with clock_timestamp() — wall-clock
// per statement, never the transaction's start time — so back-to-back
// writes in one transaction cannot share a version. No row returned means
// the precondition failed, whatever the reason.
func (s *adminQuotaStore) SetLimits(ctx context.Context, tenantID string, v provisioning.ResourceVector, expectedVersion string, create bool) (string, error) {
	var row pgx.Row
	if create {
		row = s.db.Pool().QueryRow(ctx, `
			INSERT INTO tenant_quota (tenant_id, max_running_slots, max_cpu_millis, max_memory_bytes, max_disk_bytes, updated_at)
			VALUES ($1, $2, $3, $4, $5, clock_timestamp())
			ON CONFLICT (tenant_id) DO NOTHING
			RETURNING updated_at`,
			tenantID, v.RunningSlots, v.CPUMillis, v.MemoryBytes, v.DiskBytes)
	} else {
		since, err := time.Parse(time.RFC3339Nano, expectedVersion)
		if err != nil {
			return "", store.ErrVersionMismatch
		}
		row = s.db.Pool().QueryRow(ctx, `
			UPDATE tenant_quota SET
				max_running_slots = $2, max_cpu_millis = $3,
				max_memory_bytes  = $4, max_disk_bytes = $5,
				updated_at        = clock_timestamp()
			WHERE tenant_id = $1 AND updated_at = $6
			RETURNING updated_at`,
			tenantID, v.RunningSlots, v.CPUMillis, v.MemoryBytes, v.DiskBytes, since)
	}
	var at time.Time
	if err := row.Scan(&at); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", store.ErrVersionMismatch
		}
		return "", err
	}
	return store.FormatVersion(at), nil
}

// NewAdminQuotaSource wraps the Postgres quota store.
func NewAdminQuotaSource(db *store.DB) AdminQuotaSource {
	return &adminQuotaStore{db: db, q: store.NewQuotaReader(db)}
}

// Quota source values carried by adminQuotaView.Source.
const (
	// QuotaSourceConfig: the tenant is declared in -tenant-quotas, so the
	// leader-owned startup upsert owns the row and PUT is refused.
	QuotaSourceConfig = "config"
	// QuotaSourceAPI: the row exists and no config declaration claims it —
	// it was written (and stays writable) through this API.
	QuotaSourceAPI = "api"
	// QuotaSourceNone: no declaration and no row; admission fails closed.
	QuotaSourceNone = "none"
)

// adminQuotaLimits is the PUT body (openapi AdminQuotaLimits): the four
// enforced dimensions in display units — running slots, CPU millicores,
// memory MiB, storage GiB. There is no workspace-count limit; `workspaces`
// is read-only in views and rejected here. Pointers keep a missing field
// (400) distinct from an explicit zero.
type adminQuotaLimits struct {
	RunningWorkspaces *int64 `json:"runningWorkspaces"`
	CPUMillicores     *int64 `json:"cpuMillicores"`
	MemoryMib         *int64 `json:"memoryMib"`
	StorageGib        *int64 `json:"storageGib"`
}

// toVector validates the limits and converts display units to the stored
// vector. Every field is required and non-negative; the upper bounds only
// guard against int64 overflow on the MiB/GiB shifts — they are far above
// any real quota.
func (l adminQuotaLimits) toVector() (provisioning.ResourceVector, string) {
	for f, p := range map[string]*int64{
		"runningWorkspaces": l.RunningWorkspaces,
		"cpuMillicores":     l.CPUMillicores,
		"memoryMib":         l.MemoryMib,
		"storageGib":        l.StorageGib,
	} {
		if p == nil {
			return provisioning.ResourceVector{}, f + " is required"
		}
	}
	bounds := map[string]int64{
		"runningWorkspaces": math.MaxInt64,
		"cpuMillicores":     math.MaxInt64,
		"memoryMib":         math.MaxInt64 >> 20,
		"storageGib":        math.MaxInt64 >> 30,
	}
	for f, p := range map[string]*int64{
		"runningWorkspaces": l.RunningWorkspaces,
		"cpuMillicores":     l.CPUMillicores,
		"memoryMib":         l.MemoryMib,
		"storageGib":        l.StorageGib,
	} {
		if *p < 0 || *p > bounds[f] {
			return provisioning.ResourceVector{}, f + " must be a non-negative integer"
		}
	}
	return provisioning.ResourceVector{
		RunningSlots: *l.RunningWorkspaces,
		CPUMillis:    *l.CPUMillicores,
		MemoryBytes:  *l.MemoryMib << 20,
		DiskBytes:    *l.StorageGib << 30,
	}, ""
}

// adminQuotaView is the admin quota snapshot (openapi AdminQuotaView) —
// QuotaView plus `source`, which names the layer that owns the limits row.
// The row's version travels as the response's ETag, not in the body.
type adminQuotaView struct {
	Tenant     string        `json:"tenant"`
	Configured bool          `json:"configured"`
	Source     string        `json:"source"`
	Limits     *quotaAmounts `json:"limits,omitempty"`
	Usage      quotaAmounts  `json:"usage"`
}

// AdminQuotaHandler implements GET and PUT on
// /v1/admin/tenants/{tenant}/quota. managed is the set of tenant IDs
// declared in -tenant-quotas: their rows are config-owned, so writes are
// refused with QUOTA_MANAGED_BY_CONFIG.
type AdminQuotaHandler struct {
	source  AdminQuotaSource
	tenants TenantResolver
	managed map[string]bool
	maxBody int64
	// audit is the dedicated audit-event sink the admin routes emit
	// through (nil = no domain audit events).
	audit observability.AuditSink
}

// WithAuditSink attaches the audit sink the admin quota routes write their
// dedicated audit events to.
func (h *AdminQuotaHandler) WithAuditSink(s observability.AuditSink) *AdminQuotaHandler {
	h.audit = s
	return h
}

// NewAdminQuotaHandler wires the handler. managed may be nil (no
// config-owned tenants — every row is API-managed).
func NewAdminQuotaHandler(src AdminQuotaSource, t TenantResolver, managed map[string]bool) *AdminQuotaHandler {
	return &AdminQuotaHandler{source: src, tenants: t, managed: managed, maxBody: 16 << 10}
}

// MountAdminQuotaRoutes registers the admin quota routes audited (see
// MountWorkspaceRoutes): RequireAuthPassive+audit on the read,
// RequireAuth+audit+RequireCSRF on the write — the read is inside the wrapper too, so admin
// API coverage is total: every /v1/admin/ request leaves an audit event.
func MountAdminQuotaRoutes(mux RouteMux, authn *Authenticator, h *AdminQuotaHandler) {
	mux.Handle(routeAdminQuotaGet, authn.RequireAuthPassive(
		audited(h.audit, routeAdminQuotaGet, http.HandlerFunc(h.Get))))
	mux.Handle(routeAdminQuotaSet, authn.RequireAuth(
		audited(h.audit, routeAdminQuotaSet,
			authn.RequireCSRF(http.HandlerFunc(h.Put)))))
}

// tenantAdminPrincipal gates a tenant-admin endpoint: the caller must be a
// tenant administrator of the tenant named in the path — the only admin
// role the platform defines — and that tenant must be provisioned.
// Cross-tenant reads and writes answer the same 403 as every other tenant
// boundary.
func tenantAdminPrincipal(tenants TenantResolver, w http.ResponseWriter, r *http.Request) (Principal, bool) {
	p, ok := PrincipalFromContext(r.Context())
	if !ok {
		writeError(w, r, CodeUnauthenticated, "authentication required")
		return p, false
	}
	tenant := r.PathValue("tenant")
	if !p.IsTenantAdmin() || tenant != p.TenantID {
		writeError(w, r, CodeForbidden, "tenant administration requires the tenant-admin role")
		return p, false
	}
	if _, ok := tenants.Namespace(p.TenantID); !ok {
		writeError(w, r, CodeForbidden, "tenant is not provisioned")
		return p, false
	}
	return p, true
}

// adminPrincipal gates the endpoint; see tenantAdminPrincipal.
func (h *AdminQuotaHandler) adminPrincipal(w http.ResponseWriter, r *http.Request) (Principal, bool) {
	return tenantAdminPrincipal(h.tenants, w, r)
}

// view renders a quota report as the admin snapshot.
func (h *AdminQuotaHandler) view(tenantID string, rep store.QuotaReport) adminQuotaView {
	source := QuotaSourceNone
	if h.managed[tenantID] {
		source = QuotaSourceConfig
	} else if rep.HasLimits {
		source = QuotaSourceAPI
	}
	out := adminQuotaView{
		Tenant:     tenantID,
		Configured: rep.HasLimits,
		Source:     source,
		Usage:      mapQuotaAmounts(rep.Usage),
	}
	if rep.HasLimits {
		limits := mapQuotaAmounts(rep.Limits)
		out.Limits = &limits
	}
	return out
}

// respond writes the tenant's current snapshot with its ETag (absent while
// the tenant has no quota row).
func (h *AdminQuotaHandler) respond(w http.ResponseWriter, r *http.Request, tenantID string) {
	rep, err := h.source.Report(r.Context(), tenantID)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	if rep.HasLimits {
		w.Header().Set("ETag", ETagOf(rep.Version))
	}
	respondJSON(w, h.view(tenantID, rep))
}

// effectiveUserLimit resolves an owner's per-principal running limit from
// a quota report: stored override, else the tenant default, else nil
// (unlimited). Shared by the admin quota view and the user-limits view.
func effectiveUserLimit(rep store.QuotaReport, ownerRef string) *int64 {
	if v, ok := rep.UserLimits[ownerRef]; ok {
		l := v
		return &l
	}
	return rep.DefaultUserLimit
}

// Get handles GET /v1/admin/tenants/{tenant}/quota.
func (h *AdminQuotaHandler) Get(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.adminPrincipal(w, r); !ok {
		return
	}
	h.respond(w, r, r.PathValue("tenant"))
}

// Put handles PUT /v1/admin/tenants/{tenant}/quota: it replaces the
// tenant's limits, provided the tenant is not declared in -tenant-quotas
// (config-managed rows answer 409 QUOTA_MANAGED_BY_CONFIG) and If-Match
// names the current ETag (428 PRECONDITION_REQUIRED without it, 412
// PRECONDITION_FAILED when stale; "*" creates only a missing row). The
// store re-checks the version in the UPDATE, so a write racing past the
// handler's check is a 412 too. Limits below current usage are accepted —
// held reservations stay, new ones are refused — so a lowered limit takes
// effect immediately without force.
func (h *AdminQuotaHandler) Put(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.adminPrincipal(w, r); !ok {
		return
	}
	tenant := r.PathValue("tenant")
	if h.managed[tenant] {
		writeError(w, r, CodeQuotaManagedByConfig,
			"quota for this tenant is managed by configuration; change it through the platform configuration")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, h.maxBody))
	if err != nil {
		writeError(w, r, CodeInvalidRequest, "unreadable or oversized body")
		return
	}
	var req adminQuotaLimits
	if decodeJSON(body, &req) != nil {
		writeError(w, r, CodeInvalidRequest, "invalid request body")
		return
	}
	v, field := req.toVector()
	if field != "" {
		writeError(w, r, CodeInvalidRequest,
			"limits must set non-negative runningWorkspaces, cpuMillicores, memoryMib and storageGib ("+field+")")
		return
	}
	// Record the requested limits in display units so the audit event —
	// success or refusal — carries what the write attempted.
	auditSetDetail(r.Context(), "running_workspaces", strconv.FormatInt(v.RunningSlots, 10))
	auditSetDetail(r.Context(), "cpu_millicores", strconv.FormatInt(v.CPUMillis, 10))
	auditSetDetail(r.Context(), "memory_mib", strconv.FormatInt(v.MemoryBytes>>20, 10))
	auditSetDetail(r.Context(), "storage_gib", strconv.FormatInt(v.DiskBytes>>30, 10))
	cur, err := h.source.Report(r.Context(), tenant)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	if e := CheckIfMatch(r, cur.Version, cur.HasLimits); e != nil {
		WriteError(w, r, e)
		return
	}
	if _, err := h.source.SetLimits(r.Context(), tenant, v, cur.Version, !cur.HasLimits); err != nil {
		if errors.Is(err, store.ErrVersionMismatch) {
			writeError(w, r, CodePreconditionFailed,
				"the quota changed since it was read; reload and retry")
			return
		}
		writeStoreError(w, r, err)
		return
	}
	h.respond(w, r, tenant)
}
