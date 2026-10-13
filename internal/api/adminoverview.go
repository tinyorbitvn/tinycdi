// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package api

import (
	"context"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/observability"
	"github.com/tinyorbitvn/tinycdi/internal/store"
)

// OverviewSource is the Postgres aggregate (store.DB).
type OverviewSource interface {
	TenantOverview(ctx context.Context, tenantID string, now time.Time) (store.TenantOverview, error)
}

// TenantCounter is the informer aggregate (*K8sStatusView).
type TenantCounter interface {
	TenantCounts(ctx context.Context, tenantID string, stuckAfter time.Duration) (TenantCounts, error)
}

const overviewCacheTTL = 15 * time.Second

type templateCount struct {
	Template string `json:"template"`
	Count    int    `json:"count"`
}

type overviewProvisioning struct {
	P95Seconds *float64 `json:"p95Seconds,omitempty"`
	Samples    int64    `json:"samples"`
}

type adminOverview struct {
	LiveSessions      int64                `json:"liveSessions"`
	RunningByTemplate []templateCount      `json:"runningByTemplate"`
	Phases            map[string]int       `json:"phases"`
	Provisioning      overviewProvisioning `json:"provisioning"`
	StuckFinalizers   int                  `json:"stuckFinalizers"`
	Fresh             bool                 `json:"fresh"`
	GeneratedAt       time.Time            `json:"generatedAt"`
}

// AdminOverviewHandler serves GET /v1/admin/tenants/{tenant}/overview (F7),
// cached per tenant for 15 s so a dashboard left open costs one query per
// 15 s per replica.
type AdminOverviewHandler struct {
	db      OverviewSource
	k8s     TenantCounter
	tenants TenantResolver
	audit   observability.AuditSink
	now     func() time.Time

	mu    sync.Mutex
	cache map[string]adminOverview
}

func NewAdminOverviewHandler(db OverviewSource, k8s TenantCounter, t TenantResolver) *AdminOverviewHandler {
	return &AdminOverviewHandler{db: db, k8s: k8s, tenants: t, now: time.Now, cache: map[string]adminOverview{}}
}

func (h *AdminOverviewHandler) WithAuditSink(s observability.AuditSink) *AdminOverviewHandler {
	h.audit = s
	return h
}

func MountAdminOverviewRoutes(mux RouteMux, authn *Authenticator, h *AdminOverviewHandler) {
	mux.Handle(routeAdminOverviewGet, authn.RequireAuthPassive(
		audited(h.audit, routeAdminOverviewGet, http.HandlerFunc(h.Get))))
}

func (h *AdminOverviewHandler) Get(w http.ResponseWriter, r *http.Request) {
	if _, ok := tenantAdminPrincipal(h.tenants, w, r); !ok {
		return
	}
	tenant := r.PathValue("tenant")
	now := h.now()
	h.mu.Lock()
	cached, ok := h.cache[tenant]
	h.mu.Unlock()
	if ok && now.Sub(cached.GeneratedAt) < overviewCacheTTL {
		respondJSON(w, cached)
		return
	}
	agg, err := h.db.TenantOverview(r.Context(), tenant, now)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	counts, err := h.k8s.TenantCounts(r.Context(), tenant, StuckFinalizerAfter)
	if err != nil {
		writeError(w, r, CodeUnavailable, "workspace status unavailable")
		return
	}
	out := adminOverview{
		LiveSessions: agg.LiveSessions, Phases: counts.Phases, StuckFinalizers: counts.StuckFinalizers,
		Provisioning: overviewProvisioning{P95Seconds: agg.ProvisioningP95Seconds, Samples: agg.ProvisioningSamples},
		Fresh:        counts.Fresh, GeneratedAt: now.UTC(), RunningByTemplate: []templateCount{},
	}
	for name, n := range counts.RunningByTemplate {
		out.RunningByTemplate = append(out.RunningByTemplate, templateCount{Template: name, Count: n})
	}
	sort.Slice(out.RunningByTemplate, func(i, j int) bool {
		a, b := out.RunningByTemplate[i], out.RunningByTemplate[j]
		return a.Count > b.Count || (a.Count == b.Count && a.Template < b.Template)
	})
	h.mu.Lock()
	h.cache[tenant] = out
	h.mu.Unlock()
	respondJSON(w, out)
}
