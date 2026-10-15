// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/store"
)

func seedSession(t *testing.T, db *store.DB, id, tenant, issuer, subject string, seen time.Time) {
	t.Helper()
	if _, err := db.Pool().Exec(context.Background(),
		`INSERT INTO sessions (id, issuer, subject, tenant_id, created_at, last_seen_at) VALUES ($1, $2, $3, $4, $5, $5)`,
		id, issuer, subject, tenant, seen); err != nil {
		t.Fatal(err)
	}
}

func TestTenantUsers_ListSortsPagesAndCounts(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	dir := store.NewPrincipalDirectory(db)
	for _, e := range []store.DirectoryEntry{
		{OwnerRef: "https://idp|alice", Subject: "alice", DisplayName: "Alice", Email: "a@x"},
		{OwnerRef: "https://idp|bob", Subject: "bob", DisplayName: "Bob"},
		{OwnerRef: "https://idp|carol", Subject: "carol", DisplayName: "Carol"},
	} {
		if err := dir.Remember(ctx, "t1", e); err != nil {
			t.Fatal(err)
		}
	}
	if err := dir.Remember(ctx, "t2", store.DirectoryEntry{OwnerRef: "https://idp|mallory", Subject: "mallory"}); err != nil {
		t.Fatal(err)
	}
	seedWorkspace(t, db, "t1", "https://idp|bob", "ws-b1")
	seedWorkspace(t, db, "t1", "https://idp|bob", "ws-b2")
	future := time.Now().Add(time.Hour)
	seedSession(t, db, "s1", "t1", "https://idp", "carol", future)

	page, err := db.ListTenantUsers(ctx, "t1", store.UserPage{Limit: 2, Sort: "ownerRef"})
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 3 || page[0].OwnerRef != "https://idp|alice" || page[1].OwnerRef != "https://idp|bob" {
		t.Fatalf("page 1 = %+v", page)
	}
	if page[1].Workspaces != 2 || page[0].Email != "a@x" {
		t.Fatalf("counts/fields: %+v", page[:2])
	}
	next, err := db.ListTenantUsers(ctx, "t1", store.UserPage{Limit: 2, Sort: "ownerRef", After: &store.UserCursor{Key: page[1].OwnerRef, OwnerRef: page[1].OwnerRef}})
	if err != nil || len(next) != 1 || next[0].OwnerRef != "https://idp|carol" {
		t.Fatalf("page 2 = %+v, %v", next, err)
	}
	byLastSeen, _ := db.ListTenantUsers(ctx, "t1", store.UserPage{Limit: 10, Sort: "lastSeen", Desc: true})
	if byLastSeen[0].OwnerRef != "https://idp|carol" || byLastSeen[0].LastSeenAt.Sub(future).Abs() > time.Millisecond {
		t.Fatalf("lastSeen must fold in session activity: %+v", byLastSeen[0])
	}
	u, ok, err := db.GetTenantUser(ctx, "t1", "https://idp|bob")
	if err != nil || !ok || u.Workspaces != 2 {
		t.Fatalf("GetTenantUser = %+v %v %v", u, ok, err)
	}
	if _, ok, _ := db.GetTenantUser(ctx, "t1", "https://idp|mallory"); ok {
		t.Fatal("cross-tenant principal visible")
	}
}

func TestTenantUsers_DisplayNameFallsBackToSubject(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	if err := store.NewPrincipalDirectory(db).Remember(ctx, "t1", store.DirectoryEntry{OwnerRef: "https://idp|dave", Subject: "dave"}); err != nil {
		t.Fatal(err)
	}
	u, ok, err := db.GetTenantUser(ctx, "t1", "https://idp|dave")
	if err != nil || !ok || u.DisplayName != "dave" || u.Workspaces != 0 {
		t.Fatalf("GetTenantUser = %+v %v %v, want display name = subject and no workspaces", u, ok, err)
	}
}
