// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package api

import (
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
	"github.com/tinyorbitvn/tinycdi/internal/store"
)

// ErrQuotaVersionMismatch is returned by AdminQuotaSource.SetLimits when
// the If-Match version does not name the current limits row — the quota
// changed between the client's read and its write.
var ErrQuotaVersionMismatch = errors.New("quota version mismatch")

// IfMatchCreate is the If-Match value a client sends to create a quota row
// for a tenant that has none (GET then reports no version). It is the only
// accepted precondition on a missing row — an existing row requires its
// exact current version, so a bare "*" can never skip the concurrency check.
const IfMatchCreate = "*"

// AdminQuotaSource is the read/write seam behind
// /v1/admin/tenants/{tenant}/quota; the Postgres implementation is
// adminQuotaStore.
type AdminQuotaSource interface {
	Report(ctx context.Context, tenantID string) (store.QuotaReport, error)
	// SetLimits writes the tenant's configured limits under optimistic
	// concurrency: ifMatch must name the row's current version (QuotaReport.
	// Version) or be IfMatchCreate when no row exists. A stale or wrong
	// precondition answers ErrQuotaVersionMismatch. Limits below current
	// usage are accepted: held reservations stay, new ones are refused
	// (same rule as provisioning.UpsertQuota).
	SetLimits(ctx context.Context, tenantID string, v provisioning.ResourceVector, ifMatch string) error
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
// check and the write cannot be raced: an existing row updates only when its
// updated_at still equals If-Match (the token GET reported, RFC3339Nano);
// "*" inserts only when no row exists. Writes stamp updated_at with
// clock_timestamp() — wall-clock per statement, never the transaction's
// start time — so back-to-back writes in one transaction cannot share a
// version. Zero rows affected means the precondition failed, whatever the
// reason.
func (s *adminQuotaStore) SetLimits(ctx context.Context, tenantID string, v provisioning.ResourceVector, ifMatch string) error {
	return s.db.WithTx(ctx, func(tx store.Tx) error {
		var tag pgconn.CommandTag
		var err error
		if ifMatch == IfMatchCreate {
			tag, err = tx.Exec(ctx, `
				INSERT INTO tenant_quota (tenant_id, max_running_slots, max_cpu_millis, max_memory_bytes, max_disk_bytes)
				VALUES ($1, $2, $3, $4, $5)
				ON CONFLICT (tenant_id) DO NOTHING`,
				tenantID, v.RunningSlots, v.CPUMillis, v.MemoryBytes, v.DiskBytes)
		} else {
			// An If-Match that is not a timestamp can never equal a row's
			// updated_at — refuse it as a precondition failure rather than
			// letting Postgres's cast error out.
			since, perr := time.Parse(time.RFC3339Nano, ifMatch)
			if perr != nil {
				return ErrQuotaVersionMismatch
			}
			tag, err = tx.Exec(ctx, `
				UPDATE tenant_quota SET
					max_running_slots = $2, max_cpu_millis = $3,
					max_memory_bytes  = $4, max_disk_bytes = $5,
					updated_at        = clock_timestamp()
				WHERE tenant_id = $1 AND updated_at = $6`,
				tenantID, v.RunningSlots, v.CPUMillis, v.MemoryBytes, v.DiskBytes, since)
		}
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrQuotaVersionMismatch
		}
		return nil
	})
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
// QuotaView plus `source`, which names the layer that owns the limits row,
// and `version`, the opaque change token PUT echoes in If-Match (absent
// when the tenant has no quota row).
type adminQuotaView struct {
	Tenant     string        `json:"tenant"`
	Configured bool          `json:"configured"`
	Source     string        `json:"source"`
	Version    string        `json:"version,omitempty"`
	Limits     *quotaAmounts `json:"limits,omitempty"`
	Usage      quotaAmounts  `json:"usage"`
	Users      []userUsage   `json:"users"`
}

// AdminQuotaHandler implements GET and PUT on
// /v1/admin/tenants/{tenant}/quota. managed is the set of tenant IDs
// declared in -tenant-quotas: their rows are config-owned, so writes are
// refused with QUOTA_MANAGED_BY_CONFIG.
type AdminQuotaHandler struct {
	source  AdminQuotaSource
	dir     Directory
	tenants TenantResolver
	managed map[string]bool
	maxBody int64
}

// NewAdminQuotaHandler wires the handler. dir may be nil; owner display
// names then fall back to subjects. managed may be nil (no config-owned
// tenants — every row is API-managed).
func NewAdminQuotaHandler(src AdminQuotaSource, dir Directory, t TenantResolver, managed map[string]bool) *AdminQuotaHandler {
	return &AdminQuotaHandler{source: src, dir: dir, tenants: t, managed: managed, maxBody: 16 << 10}
}

// MountAdminQuotaRoutes registers the admin quota routes: RequireAuth on
// the read, RequireAuth+RequireCSRF on the write.
func MountAdminQuotaRoutes(mux *http.ServeMux, authn *Authenticator, h *AdminQuotaHandler) {
	mux.Handle("GET /v1/admin/tenants/{tenant}/quota", authn.RequireAuth(http.HandlerFunc(h.Get)))
	mux.Handle("PUT /v1/admin/tenants/{tenant}/quota", authn.RequireAuth(authn.RequireCSRF(http.HandlerFunc(h.Put))))
}

// adminPrincipal gates the endpoint: the caller must be a tenant
// administrator of the tenant named in the path — the only admin role the
// platform defines — and that tenant must be provisioned. Cross-tenant
// reads and writes answer the same 403 as every other tenant boundary.
func (h *AdminQuotaHandler) adminPrincipal(w http.ResponseWriter, r *http.Request) (Principal, bool) {
	p, ok := PrincipalFromContext(r.Context())
	if !ok {
		writeError(w, r, CodeUnauthenticated, "authentication required")
		return p, false
	}
	tenant := r.PathValue("tenant")
	if !p.InGroup(TenantAdminGroup) || tenant != p.TenantID {
		writeError(w, r, CodeForbidden, "tenant quota administration requires the tenant-admin role")
		return p, false
	}
	if _, ok := h.tenants.Namespace(p.TenantID); !ok {
		writeError(w, r, CodeForbidden, "tenant is not provisioned")
		return p, false
	}
	return p, true
}

func (h *AdminQuotaHandler) view(ctx context.Context, tenantID string) (adminQuotaView, error) {
	rep, err := h.source.Report(ctx, tenantID)
	if err != nil {
		return adminQuotaView{}, err
	}
	source := QuotaSourceNone
	if h.managed[tenantID] {
		source = QuotaSourceConfig
	} else if rep.HasLimits {
		source = QuotaSourceAPI
	}
	refs := make([]string, 0, len(rep.Owners))
	for _, o := range rep.Owners {
		refs = append(refs, o.OwnerRef)
	}
	owners := resolveOwners(ctx, h.dir, tenantID, refs)
	out := adminQuotaView{
		Tenant:     tenantID,
		Configured: rep.HasLimits,
		Source:     source,
		Version:    rep.Version,
		Usage:      mapQuotaAmounts(rep.Usage),
		Users:      make([]userUsage, 0, len(rep.Owners)),
	}
	if rep.HasLimits {
		limits := mapQuotaAmounts(rep.Limits)
		out.Limits = &limits
	}
	for _, o := range rep.Owners {
		owner := owners[o.OwnerRef]
		out.Users = append(out.Users, userUsage{
			Subject:     owner.Subject,
			DisplayName: owner.DisplayName,
			Usage:       mapQuotaAmounts(o.Usage),
		})
	}
	return out, nil
}

// Get handles GET /v1/admin/tenants/{tenant}/quota.
func (h *AdminQuotaHandler) Get(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.adminPrincipal(w, r); !ok {
		return
	}
	out, err := h.view(r.Context(), r.PathValue("tenant"))
	if err != nil {
		writeError(w, r, CodeInternal, "internal error")
		return
	}
	respondJSON(w, out)
}

// Put handles PUT /v1/admin/tenants/{tenant}/quota: it replaces the
// tenant's limits, provided the tenant is not declared in -tenant-quotas
// (config-managed rows answer 409 QUOTA_MANAGED_BY_CONFIG) and the caller
// names the current version in If-Match (optimistic concurrency — a stale
// version answers 412 PRECONDITION_FAILED; the header is required, and
// IfMatchCreate "*" creates only a missing row). Limits below current
// usage are accepted — held reservations stay, new ones are refused — so
// a lowered limit takes effect immediately without force.
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
	var req adminQuotaLimits
	if !decodeJSON(body, &req) {
		writeError(w, r, CodeInvalidRequest, "invalid request body")
		return
	}
	v, field := req.toVector()
	if field != "" {
		writeError(w, r, CodeInvalidRequest,
			"limits must set non-negative runningWorkspaces, cpuMillicores, memoryMib and storageGib ("+field+")")
		return
	}
	if err := h.source.SetLimits(r.Context(), tenant, v, ifMatch); err != nil {
		if errors.Is(err, ErrQuotaVersionMismatch) {
			writeError(w, r, CodePreconditionFailed,
				"the quota changed since it was read; reload and retry")
			return
		}
		writeError(w, r, CodeInternal, "internal error")
		return
	}
	out, err := h.view(r.Context(), tenant)
	if err != nil {
		writeError(w, r, CodeInternal, "internal error")
		return
	}
	respondJSON(w, out)
}
