// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package store_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/store"
)

func seedLiveLease(t *testing.T, db *store.DB, id, ws, tenant, owner, state string, created, expires time.Time) {
	t.Helper()
	if _, err := db.Pool().Exec(context.Background(), `
		INSERT INTO connection_lease (id, workspace_id, tenant_id, principal_subject, runtime_generation, runtime_uid,
			fencing_version, gateway_id, state, created_at, expires_at, portal_session_digest)
		VALUES ($1, $2, $3, $4, 1, 'uid', 1, 'gw', $5, $6, $7, '\x00')`,
		id, ws, tenant, owner, state, created, expires); err != nil {
		t.Fatal(err)
	}
}

func TestLiveLeases_ListFiltersAndPages(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	now := time.Now()
	// lease-a1 and lease-a2 share created_at: the workspace id breaks the tie.
	created := []time.Time{now.Add(-3 * time.Minute), now.Add(-2 * time.Minute), now.Add(-2 * time.Minute)}
	for i := 0; i < 3; i++ {
		ws := fmt.Sprintf("ws-a%d", i)
		seedWorkspace(t, db, "t1", "https://idp|alice", ws)
		seedLiveLease(t, db, fmt.Sprintf("lease-a%d", i), ws, "t1", "https://idp|alice", "active", created[i], now.Add(time.Minute))
	}
	seedWorkspace(t, db, "t1", "https://idp|bob", "ws-b")
	seedLiveLease(t, db, "lease-b", "ws-b", "t1", "https://idp|bob", "active", now.Add(-10*time.Minute), now.Add(-time.Second)) // expired by time
	seedWorkspace(t, db, "t1", "https://idp|bob", "ws-b2")
	seedLiveLease(t, db, "lease-b2", "ws-b2", "t1", "https://idp|bob", "revoked", now, now.Add(time.Minute))
	seedWorkspace(t, db, "t2", "https://idp|eve", "ws-e")
	seedLiveLease(t, db, "lease-e", "ws-e", "t2", "https://idp|eve", "active", now, now.Add(time.Minute))

	page, err := db.ListLiveLeases(ctx, "t1", now, store.LeaseFilter{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 3 || page[0].ID != "lease-a2" || page[1].ID != "lease-a1" {
		t.Fatalf("page = %+v; want newest first, live only, tenant only", page)
	}
	rest, _ := db.ListLiveLeases(ctx, "t1", now, store.LeaseFilter{Limit: 2, After: &store.LeaseCursor{CreatedAt: page[1].CreatedAt, ID: page[1].WorkspaceID}})
	if len(rest) != 1 || rest[0].ID != "lease-a0" {
		t.Fatalf("rest = %+v", rest)
	}
	// A cursor parked on the first of two equal created_at rows still returns the second.
	tie, _ := db.ListLiveLeases(ctx, "t1", now, store.LeaseFilter{Limit: 5, After: &store.LeaseCursor{CreatedAt: page[0].CreatedAt, ID: page[0].WorkspaceID}})
	if len(tie) != 2 || tie[0].ID != "lease-a1" || tie[1].ID != "lease-a0" {
		t.Fatalf("tie = %+v; the (created_at, workspace_id) keyset must not skip equal timestamps", tie)
	}
	// The tenant filter is part of the SQL: t2's lease is invisible to t1 and the reverse.
	if eve, _ := db.ListLiveLeases(ctx, "t2", now, store.LeaseFilter{Limit: 10}); len(eve) != 1 || eve[0].ID != "lease-e" {
		t.Fatalf("t2 = %+v", eve)
	}
	if none, _ := db.ListLiveLeases(ctx, "t1", now, store.LeaseFilter{Limit: 10, OwnerRef: "https://idp|eve"}); len(none) != 0 {
		t.Fatalf("another tenant's principal listed under t1: %+v", none)
	}
	bob, _ := db.ListLiveLeases(ctx, "t1", now, store.LeaseFilter{Limit: 10, OwnerRef: "https://idp|bob"})
	if len(bob) != 0 {
		t.Fatalf("expired/revoked leases listed: %+v", bob)
	}
	if live, _ := db.HasLiveLease(ctx, "ws-a0", now); !live {
		t.Fatal("HasLiveLease(ws-a0) = false")
	}
	if live, _ := db.HasLiveLease(ctx, "ws-b", now); live {
		t.Fatal("HasLiveLease on an expired lease = true")
	}
}
