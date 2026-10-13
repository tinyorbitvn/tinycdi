// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package api

import (
	"context"
	"net/http"

	"github.com/tinyorbitvn/tinycdi/internal/store"
)

// QuotaSource is the read seam behind GET /v1/quota; the Postgres
// implementation is store.QuotaReader.
type QuotaSource interface {
	Report(ctx context.Context, tenantID string) (store.QuotaReport, error)
}

// NewQuotaSource wraps the Postgres quota reader.
func NewQuotaSource(db *store.DB) QuotaSource { return store.NewQuotaReader(db) }

// ---------------------------------------------------------------------------
// Public JSON shapes (must match openapi.yaml exactly).
// ---------------------------------------------------------------------------

// quotaAmounts is the contract's resource vector in display units
// (openapi QuotaAmounts): CPU in millicores, memory in MiB, disk in GiB.
type quotaAmounts struct {
	Workspaces        int64 `json:"workspaces"`
	RunningWorkspaces int64 `json:"runningWorkspaces"`
	CPUMillicores     int64 `json:"cpuMillicores"`
	MemoryMib         int64 `json:"memoryMib"`
	StorageGib        int64 `json:"storageGib"`
}

func mapQuotaAmounts(v store.QuotaAmounts) quotaAmounts {
	return quotaAmounts{
		Workspaces:        v.Workspaces,
		RunningWorkspaces: v.RunningSlots,
		CPUMillicores:     v.CPUMillis,
		MemoryMib:         v.MemoryBytes / (1 << 20),
		StorageGib:        v.DiskBytes / (1 << 30),
	}
}

// quotaView is the tenant quota snapshot (openapi QuotaView). userLimits is
// present only when the caller has an effective per-principal
// running-workspace limit; inside it only runningWorkspaces is meaningful.
// Limits is omitted for a tenant without a quota row (admission fails
// closed there); inside Limits, workspaces 0 means "no count limit".
// Per-principal rows live in the paged admin user-limits list, not here.
type quotaView struct {
	Tenant string `json:"tenant"`
	// Configured is false when the tenant has no quota row: admission then
	// refuses every create (ErrNoQuota), so an absent Limits must not read
	// as "unlimited".
	Configured bool          `json:"configured"`
	Limits     *quotaAmounts `json:"limits,omitempty"`
	Usage      quotaAmounts  `json:"usage"`
	// UserLimits carries the caller's effective per-principal limit; only
	// RunningWorkspaces is meaningful, the other fields stay zero. Absent
	// when the caller is unlimited.
	UserLimits *quotaAmounts `json:"userLimits,omitempty"`
}

// QuotaHandler implements GET /v1/quota per openapi.yaml.
type QuotaHandler struct {
	source  QuotaSource
	tenants TenantResolver
}

// NewQuotaHandler wires the handler.
func NewQuotaHandler(src QuotaSource, t TenantResolver) *QuotaHandler {
	return &QuotaHandler{source: src, tenants: t}
}

// MountQuotaRoutes registers GET /v1/quota behind passive auth (D18).
func MountQuotaRoutes(mux RouteMux, authn *Authenticator, h *QuotaHandler) {
	mux.Handle("GET /v1/quota", authn.RequireAuthPassive(http.HandlerFunc(h.Get)))
}

// Get handles GET /v1/quota: the caller's tenant limits and usage, plus
// the caller's own effective per-principal running limit.
func (h *QuotaHandler) Get(w http.ResponseWriter, r *http.Request) {
	p, ok := PrincipalFromContext(r.Context())
	if !ok {
		writeError(w, r, CodeUnauthenticated, "authentication required")
		return
	}
	if _, ok := h.tenants.Namespace(p.TenantID); !ok {
		writeError(w, r, CodeForbidden, "tenant is not provisioned")
		return
	}
	rep, err := h.source.Report(r.Context(), p.TenantID)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	out := quotaView{
		Tenant:     p.TenantID,
		Configured: rep.HasLimits,
		Usage:      mapQuotaAmounts(rep.Usage),
	}
	if rep.HasLimits {
		limits := mapQuotaAmounts(rep.Limits)
		out.Limits = &limits
	}
	if eff := effectiveUserLimit(rep, p.Owner()); eff != nil {
		out.UserLimits = &quotaAmounts{RunningWorkspaces: *eff}
	}
	respondJSON(w, out)
}
