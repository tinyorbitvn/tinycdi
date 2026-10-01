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

// userUsage is one owner's usage row (openapi UserUsage).
type userUsage struct {
	Subject     string       `json:"subject"`
	DisplayName string       `json:"displayName"`
	Usage       quotaAmounts `json:"usage"`
}

// quotaView is the tenant quota snapshot (openapi QuotaView). userLimits is
// omitted: there is no per-user limit store in v0.2.
type quotaView struct {
	Tenant string           `json:"tenant"`
	Limits quotaAmounts     `json:"limits"`
	Usage  quotaAmounts     `json:"usage"`
	Users  []userUsage      `json:"users"`
}

// QuotaHandler implements GET /v1/quota per openapi.yaml.
type QuotaHandler struct {
	source QuotaSource
	dir    Directory
	tenants TenantResolver
}

// NewQuotaHandler wires the handler. dir may be nil; owner display names
// then fall back to subjects.
func NewQuotaHandler(src QuotaSource, dir Directory, t TenantResolver) *QuotaHandler {
	return &QuotaHandler{source: src, dir: dir, tenants: t}
}

// MountQuotaRoutes registers GET /v1/quota behind RequireAuth.
func MountQuotaRoutes(mux *http.ServeMux, authn *Authenticator, h *QuotaHandler) {
	mux.Handle("GET /v1/quota", authn.RequireAuth(http.HandlerFunc(h.Get)))
}

// Get handles GET /v1/quota: tenant admins see every owner's usage row;
// regular users see only their own.
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
		writeError(w, r, CodeInternal, "internal error")
		return
	}
	admin := p.InGroup(TenantAdminGroup)
	refs := make([]string, 0, len(rep.Owners))
	for _, o := range rep.Owners {
		if admin || o.OwnerRef == p.Owner() {
			refs = append(refs, o.OwnerRef)
		}
	}
	owners := resolveOwners(r.Context(), h.dir, p.TenantID, refs)
	out := quotaView{
		Tenant: p.TenantID,
		Limits: mapQuotaAmounts(rep.Limits),
		Usage:  mapQuotaAmounts(rep.Usage),
		Users:  make([]userUsage, 0, len(refs)),
	}
	for _, o := range rep.Owners {
		if !admin && o.OwnerRef != p.Owner() {
			continue
		}
		owner := owners[o.OwnerRef]
		out.Users = append(out.Users, userUsage{
			Subject:     owner.Subject,
			DisplayName: owner.DisplayName,
			Usage:       mapQuotaAmounts(o.Usage),
		})
	}
	respondJSON(w, out)
}
