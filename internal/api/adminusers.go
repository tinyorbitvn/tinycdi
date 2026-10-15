// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package api

import (
	"context"
	"net/http"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/observability"
	"github.com/tinyorbitvn/tinycdi/internal/store"
)

// UserDirectorySource reads the tenant's principal directory (store.DB).
type UserDirectorySource interface {
	ListTenantUsers(ctx context.Context, tenantID string, p store.UserPage) ([]store.TenantUser, error)
	GetTenantUser(ctx context.Context, tenantID, ownerRef string) (store.TenantUser, bool, error)
}

type adminTenantUser struct {
	OwnerRef    string              `json:"ownerRef"`
	Subject     string              `json:"subject"`
	DisplayName string              `json:"displayName"`
	Email       string              `json:"email"`
	LastSeenAt  time.Time           `json:"lastSeenAt"`
	Workspaces  int64               `json:"workspaces"`
	Limits      adminUserLimitEntry `json:"limits"`
}

// AdminUsersHandler serves the tenant user directory (F8): everyone who
// signed in to the tenant, not only principals holding workspaces.
type AdminUsersHandler struct {
	dir     UserDirectorySource
	limits  UserLimitSource
	tenants TenantResolver
	audit   observability.AuditSink
}

func NewAdminUsersHandler(src UserDirectorySource, limits UserLimitSource, t TenantResolver) *AdminUsersHandler {
	return &AdminUsersHandler{dir: src, limits: limits, tenants: t}
}

func (h *AdminUsersHandler) WithAuditSink(s observability.AuditSink) *AdminUsersHandler {
	h.audit = s
	return h
}

func MountAdminUserRoutes(mux RouteMux, authn *Authenticator, h *AdminUsersHandler) {
	mux.Handle(routeAdminUsersList, authn.RequireAuthPassive(audited(h.audit, routeAdminUsersList, http.HandlerFunc(h.List))))
	mux.Handle(routeAdminUserGet, authn.RequireAuthPassive(audited(h.audit, routeAdminUserGet, http.HandlerFunc(h.Get))))
}

var userDirectorySorts = []string{"ownerRef", "displayName", "lastSeen"}

func (h *AdminUsersHandler) List(w http.ResponseWriter, r *http.Request) {
	if _, ok := tenantAdminPrincipal(h.tenants, w, r); !ok {
		return
	}
	page, perr := ParsePage(r, userDirectorySorts, "ownerRef")
	if perr != nil {
		WriteError(w, r, perr)
		return
	}
	// The lastSeen sort keys on a timestamp the store parses; a forged key
	// is the client's error, refused before any read (never a 500).
	if page.After != nil && page.Sort.Field == "lastSeen" {
		if _, err := time.Parse(time.RFC3339Nano, page.After.Key); err != nil {
			WriteError(w, r, NewError(CodeInvalidRequest, "bad cursor"))
			return
		}
	}
	tenant := r.PathValue("tenant")
	p := store.UserPage{Limit: page.Limit, Sort: page.Sort.Field, Desc: page.Sort.Desc}
	if page.After != nil {
		p.After = &store.UserCursor{Key: page.After.Key, OwnerRef: page.After.ID}
	}
	users, err := h.dir.ListTenantUsers(r.Context(), tenant, p)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	rep, err := h.limits.Report(r.Context(), tenant)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	out := Page[adminTenantUser]{Items: make([]adminTenantUser, 0, len(users))}
	if len(users) > page.Limit {
		last := users[page.Limit-1]
		out.NextCursor = NextCursor(page.Sort, userSortKey(page.Sort.Field, last), last.OwnerRef)
		users = users[:page.Limit]
	}
	for _, u := range users {
		out.Items = append(out.Items, tenantUserView(rep, u))
	}
	respondJSON(w, out)
}

func (h *AdminUsersHandler) Get(w http.ResponseWriter, r *http.Request) {
	if _, ok := tenantAdminPrincipal(h.tenants, w, r); !ok {
		return
	}
	ref := r.PathValue("ownerRef")
	if msg := validOwnerRef(ref); msg != "" {
		writeError(w, r, CodeInvalidRequest, msg)
		return
	}
	auditSetDetail(r.Context(), "target", targetActorRef(ref))
	tenant := r.PathValue("tenant")
	u, ok, err := h.dir.GetTenantUser(r.Context(), tenant, ref)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	if !ok {
		writeError(w, r, CodeNotFound, "user has not signed in to this tenant")
		return
	}
	rep, err := h.limits.Report(r.Context(), tenant)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	respondJSON(w, tenantUserView(rep, u))
}

func userSortKey(field string, u store.TenantUser) string {
	switch field {
	case "displayName":
		return u.DisplayName
	case "lastSeen":
		return u.LastSeenAt.UTC().Format(time.RFC3339Nano)
	}
	return u.OwnerRef
}

func tenantUserView(rep store.QuotaReport, u store.TenantUser) adminTenantUser {
	limits := userLimitEntry(rep, u.OwnerRef, Owner{Subject: u.Subject, DisplayName: u.DisplayName})
	return adminTenantUser{
		OwnerRef: u.OwnerRef, Subject: u.Subject, DisplayName: u.DisplayName, Email: u.Email,
		LastSeenAt: u.LastSeenAt, Workspaces: u.Workspaces, Limits: limits,
	}
}
