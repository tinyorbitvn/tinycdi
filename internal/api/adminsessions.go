// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/observability"
	"github.com/tinyorbitvn/tinycdi/internal/store"
)

// LiveLeaseSource lists live connection leases from Postgres (store.DB) —
// every replica answers the same, unlike gateway-local state.
type LiveLeaseSource interface {
	ListLiveLeases(ctx context.Context, tenantID string, now time.Time, f store.LeaseFilter) ([]store.LiveLease, error)
}

type adminLiveSession struct {
	LeaseRef      string     `json:"leaseRef"`
	WorkspaceID   string     `json:"workspaceId"`
	WorkspaceName string     `json:"workspaceName"`
	OwnerRef      string     `json:"ownerRef"`
	Owner         Owner      `json:"owner"`
	CreatedAt     time.Time  `json:"createdAt"`
	LastRenewedAt *time.Time `json:"lastRenewedAt,omitempty"`
	ExpiresAt     time.Time  `json:"expiresAt"`
}

// adminRevokeResult is the POST ...:revoke-all body (openapi AdminRevokeResult).
type adminRevokeResult struct {
	Sessions int `json:"sessions"`
	Tickets  int `json:"tickets"`
	Leases   int `json:"leases"`
}

// AdminSessionsHandler lists live sessions and signs a user out everywhere (F5).
type AdminSessionsHandler struct {
	leases  LiveLeaseSource
	dir     Directory
	revoker PrincipalRevoker
	tenants TenantResolver
	audit   observability.AuditSink
	now     func() time.Time
}

func NewAdminSessionsHandler(src LiveLeaseSource, dir Directory, revoker PrincipalRevoker, t TenantResolver) *AdminSessionsHandler {
	return &AdminSessionsHandler{leases: src, dir: dir, revoker: revoker, tenants: t, now: time.Now}
}

func (h *AdminSessionsHandler) WithAuditSink(s observability.AuditSink) *AdminSessionsHandler {
	h.audit = s
	return h
}

func MountAdminSessionRoutes(mux RouteMux, authn *Authenticator, h *AdminSessionsHandler) {
	mux.Handle(routeAdminSessionsList, authn.RequireAuthPassive(
		audited(h.audit, routeAdminSessionsList, http.HandlerFunc(h.List))))
	mux.Handle(routeAdminUserRevokeAll, authn.RequireAuth(
		audited(h.audit, routeAdminUserRevokeAll, authn.RequireCSRF(http.HandlerFunc(h.RevokeAll)))))
}

// leaseRef is the stable public reference of a lease; the id itself is a
// fencing credential the gateway presents and never leaves the backend.
func leaseRef(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:8])
}

func (h *AdminSessionsHandler) List(w http.ResponseWriter, r *http.Request) {
	if _, ok := tenantAdminPrincipal(h.tenants, w, r); !ok {
		return
	}
	page, perr := ParsePage(r, []string{"createdAt"}, "-createdAt")
	if perr != nil {
		WriteError(w, r, perr)
		return
	}
	if !page.Sort.Desc {
		writeError(w, r, CodeInvalidRequest, "sessions list newest first only (sort=-createdAt)")
		return
	}
	f := store.LeaseFilter{Limit: page.Limit}
	if ref := r.URL.Query().Get("ownerRef"); ref != "" {
		if msg := validOwnerRef(ref); msg != "" {
			writeError(w, r, CodeInvalidRequest, msg)
			return
		}
		f.OwnerRef = ref
	}
	if page.After != nil {
		at, err := time.Parse(time.RFC3339Nano, page.After.Key)
		if err != nil {
			writeError(w, r, CodeInvalidRequest, "bad cursor")
			return
		}
		f.After = &store.LeaseCursor{CreatedAt: at, ID: page.After.ID}
	}
	tenant := r.PathValue("tenant")
	rows, err := h.leases.ListLiveLeases(r.Context(), tenant, h.now(), f)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	out := Page[adminLiveSession]{Items: make([]adminLiveSession, 0, len(rows))}
	if len(rows) > page.Limit {
		last := rows[page.Limit-1]
		// The keyset tiebreak is the WORKSPACE id (one live lease per workspace),
		// so neither items nor the opaque cursor ever carry a lease id.
		out.NextCursor = NextCursor(page.Sort, last.CreatedAt.UTC().Format(time.RFC3339Nano), last.WorkspaceID)
		rows = rows[:page.Limit]
	}
	refs := make([]string, len(rows))
	for i, l := range rows {
		refs[i] = l.OwnerRef
	}
	owners := resolveOwners(r.Context(), h.dir, tenant, refs)
	for _, l := range rows {
		out.Items = append(out.Items, adminLiveSession{
			LeaseRef: leaseRef(l.ID), WorkspaceID: l.WorkspaceID, WorkspaceName: l.WorkspaceName,
			OwnerRef: l.OwnerRef, Owner: owners[l.OwnerRef], CreatedAt: l.CreatedAt,
			LastRenewedAt: l.LastRenewedAt, ExpiresAt: l.ExpiresAt,
		})
	}
	respondJSON(w, out)
}

func (h *AdminSessionsHandler) RevokeAll(w http.ResponseWriter, r *http.Request) {
	if _, ok := tenantAdminPrincipal(h.tenants, w, r); !ok {
		return
	}
	ref := r.PathValue("ownerRef")
	if msg := validOwnerRef(ref); msg != "" {
		writeError(w, r, CodeInvalidRequest, msg)
		return
	}
	ctx := r.Context()
	auditSetDetail(ctx, "target", targetActorRef(ref))
	if h.revoker == nil {
		writeError(w, r, CodeUnavailable, "sign-out unavailable")
		return
	}
	iss, sub, _ := strings.Cut(ref, "|")
	revCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sessionRevokeTimeout)
	res, err := h.revoker.RevokePrincipalSessions(revCtx, r.PathValue("tenant"), iss, sub)
	cancel()
	if err != nil {
		writeError(w, r, CodeInternal, "could not sign the user out")
		return
	}
	auditSetDetail(ctx, "counts", "sessions="+strconv.Itoa(res.Sessions)+
		",tickets="+strconv.Itoa(res.Tickets)+",leases="+strconv.Itoa(res.Leases))
	respondJSON(w, adminRevokeResult{Sessions: res.Sessions, Tickets: res.Tickets, Leases: res.Leases})
}
