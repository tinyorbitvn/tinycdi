// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package api

import (
	"context"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/store"
)

func newAdminUsersEnv(t *testing.T, src UserDirectorySource, limits UserLimitSource) *testEnv {
	t.Helper()
	return newAdminMountEnv(t, func(mux *http.ServeMux, a *Authenticator) {
		MountAdminUserRoutes(mux, a, NewAdminUsersHandler(src, limits, defaultTenants()))
	})
}

const adminUsersPath = "/v1/admin/tenants/tenant-a/users"

// adminUserPath is the detail path of ownerRef, escaped like userLimitPath.
func adminUserPath(ownerRef string) string { return adminUsersPath + "/" + url.PathEscape(ownerRef) }

type fakeUserDirectory struct {
	users   []store.TenantUser
	gotPage store.UserPage
}

func (f *fakeUserDirectory) ListTenantUsers(_ context.Context, tenant string, p store.UserPage) ([]store.TenantUser, error) {
	f.gotPage = p
	if tenant != "tenant-a" {
		return nil, nil
	}
	if len(f.users) > p.Limit+1 {
		return f.users[:p.Limit+1], nil
	}
	return f.users, nil
}

func (f *fakeUserDirectory) GetTenantUser(_ context.Context, tenant, ref string) (store.TenantUser, bool, error) {
	for _, u := range f.users {
		if tenant == "tenant-a" && u.OwnerRef == ref {
			return u, true, nil
		}
	}
	return store.TenantUser{}, false, nil
}

func directoryUsers() []store.TenantUser {
	seen := time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)
	return []store.TenantUser{
		{OwnerRef: "https://idp|alice", Subject: "alice", DisplayName: "Alice", LastSeenAt: seen, Workspaces: 1},
		{OwnerRef: "https://idp|bob", Subject: "bob", DisplayName: "Bob", LastSeenAt: seen, Workspaces: 0},
		{OwnerRef: "https://idp|carol", Subject: "carol", DisplayName: "Carol", LastSeenAt: seen, Workspaces: 3},
	}
}

func TestAdminUsers_ListIncludesUsersWithoutWorkspacesOrLimits(t *testing.T) {
	limits := newFakeUserLimitSource()
	limits.seed("https://idp|carol", 2) // an override row; Report rebuilds UserLimits from these seeds
	env := newAdminUsersEnv(t, &fakeUserDirectory{users: directoryUsers()}, limits)
	sess, csrf := loginAdmin(t, env, "admin-a")
	res := doReq(t, env, sess, csrf, http.MethodGet, adminUsersPath+"?limit=2", "", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d", res.StatusCode)
	}
	page := decodeBody[Page[adminTenantUser]](t, res)
	if len(page.Items) != 2 || page.Items[1].OwnerRef != "https://idp|bob" || page.NextCursor == "" {
		t.Fatalf("page = %+v (bob has no workspace and no override, and must still be listed)", page)
	}
	res = doReq(t, env, sess, csrf, http.MethodGet, adminUserPath("https://idp|carol"), "", nil)
	u := decodeBody[adminTenantUser](t, res)
	if u.Limits.Limit == nil || *u.Limits.Limit != 2 || u.Limits.ETag == "" || u.Workspaces != 3 {
		t.Fatalf("detail = %+v", u)
	}
}

func TestAdminUsers_DetailNotFoundAndBadRef(t *testing.T) {
	env := newAdminUsersEnv(t, &fakeUserDirectory{users: directoryUsers()}, &fakeUserLimitSource{})
	sess, csrf := loginAdmin(t, env, "admin-a")
	if res := doReq(t, env, sess, csrf, http.MethodGet, adminUserPath("https://idp|nobody"), "", nil); res.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown user: %d", res.StatusCode)
	}
	if res := doReq(t, env, sess, csrf, http.MethodGet, adminUserPath("no-pipe"), "", nil); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad ref: %d", res.StatusCode)
	}
	if res := doReq(t, env, sess, csrf, http.MethodGet, adminUsersPath+"?sort=email", "", nil); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad sort: %d", res.StatusCode)
	}
}

func TestAdminUsers_CursorCarriesSortKey(t *testing.T) {
	dir := &fakeUserDirectory{users: directoryUsers()}
	env := newAdminUsersEnv(t, dir, newFakeUserLimitSource())
	sess, csrf := loginAdmin(t, env, "admin-a")
	res := doReq(t, env, sess, csrf, http.MethodGet, adminUsersPath+"?limit=2&sort=-lastSeen", "", nil)
	page := decodeBody[Page[adminTenantUser]](t, res)
	if dir.gotPage.Sort != "lastSeen" || !dir.gotPage.Desc || page.NextCursor == "" {
		t.Fatalf("page request = %+v, cursor %q", dir.gotPage, page.NextCursor)
	}
	res = doReq(t, env, sess, csrf, http.MethodGet, adminUsersPath+"?limit=2&sort=-lastSeen&cursor="+url.QueryEscape(page.NextCursor), "", nil)
	if res.StatusCode != http.StatusOK || dir.gotPage.After == nil || dir.gotPage.After.OwnerRef != "https://idp|bob" ||
		dir.gotPage.After.Key != "2026-10-13T09:00:00Z" {
		t.Fatalf("cursor not decoded: status=%d after=%+v", res.StatusCode, dir.gotPage.After)
	}
	// A cursor minted under another sort is refused.
	res = doReq(t, env, sess, csrf, http.MethodGet, adminUsersPath+"?limit=2&sort=ownerRef&cursor="+url.QueryEscape(page.NextCursor), "", nil)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("cross-sort cursor: %d, want 400", res.StatusCode)
	}
}
