// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package provisioning_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
	"github.com/tinyorbitvn/tinycdi/internal/store"
)

func seedWorkspaceAt(t *testing.T, db *store.DB, id, owner, name string, created time.Time) {
	t.Helper()
	seedWorkspaceRow(t, db, id, "tenant-pg", owner, name)
	if _, err := db.Pool().Exec(context.Background(),
		`UPDATE workspaces SET created_at = $2 WHERE id = $1`, id, created); err != nil {
		t.Fatalf("set created_at %s: %v", id, err)
	}
}

// walkWorkspaces pages through ListWorkspaces, calling between after the
// first page, and returns the names in the order served.
func walkWorkspaces(t *testing.T, svc *provisioning.Service, p provisioning.ListPage, between func()) []string {
	t.Helper()
	var names []string
	for pages := 0; ; pages++ {
		if pages > 10 {
			t.Fatal("paging did not terminate")
		}
		recs, key, id, err := svc.ListWorkspaces(context.Background(), "tenant-pg", "", "", p)
		if err != nil {
			t.Fatalf("ListWorkspaces(%+v): %v", p, err)
		}
		for _, r := range recs {
			names = append(names, r.Name)
		}
		if pages == 0 && between != nil {
			between()
		}
		if id == "" {
			return names
		}
		p.AfterKey, p.AfterID = key, id
	}
}

// TestListWorkspaces_Keyset: every sort walks the rows exactly once in
// (key, id) order — ties on the key broken by id — and a row inserted before
// the cursor mid-walk neither duplicates nor shifts later rows.
func TestListWorkspaces_Keyset(t *testing.T) {
	db := recoveryDB(t)
	svc := provisioning.NewService(db)
	base := time.Date(2026, 10, 1, 0, 0, 0, 123456000, time.UTC)
	// "delta" twice (two owners): equal names must still page by id.
	names := []string{"bravo", "charlie", "delta", "delta", "echo", "foxtrot", "golf"}
	for i, n := range names {
		owner := "iss|sub-1"
		if i == 3 {
			owner = "iss|sub-2"
		}
		seedWorkspaceAt(t, db, fmt.Sprintf("ws_pg%08d", i), owner, n, base.Add(time.Duration(i)*time.Second))
	}

	got := walkWorkspaces(t, svc, provisioning.ListPage{Limit: 3, SortField: "name"}, func() {
		seedWorkspaceAt(t, db, "ws_pginserted", "iss|sub-1", "alpha", base.Add(time.Hour))
	})
	if fmt.Sprint(got) != fmt.Sprint(names) {
		t.Fatalf("name walk = %v, want %v", got, names)
	}

	got = walkWorkspaces(t, svc, provisioning.ListPage{Limit: 2, SortField: "createdAt", Desc: true}, nil)
	want := []string{"alpha", "golf", "foxtrot", "echo", "delta", "delta", "charlie", "bravo"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("-createdAt walk = %v, want %v", got, want)
	}

	got = walkWorkspaces(t, svc, provisioning.ListPage{Limit: 4, SortField: "createdAt"}, nil)
	if fmt.Sprint(got) != fmt.Sprint(append(append([]string{}, names...), "alpha")) {
		t.Fatalf("createdAt walk = %v", got)
	}
}

// TestListRetained_Keyset: the retained list pages the same way on
// retainedAt (default descending) and the source workspace name.
func TestListRetained_Keyset(t *testing.T) {
	db := recoveryDB(t)
	ctx := context.Background()
	st := provisioning.NewRetainedStore(db)
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for i, n := range []string{"src-c", "src-a", "src-b"} {
		src := fmt.Sprintf("ws_pgsrc%04d", i)
		seedWorkspaceRow(t, db, src, "tenant-pg", "iss|sub-1", n)
		rec, err := st.ImportRetained(ctx, provisioning.RetainedDiskInfo{
			TenantID: "tenant-pg", Owner: "iss|sub-1", SourceWorkspaceID: src,
			SourceWorkspaceName: n, Runtime: "LinuxContainer", SizeBytes: 1 << 30,
			PVCNamespace: "ns-pg", PVCName: "pvc-" + n, PVCUID: "uid-" + n,
		})
		if err != nil {
			t.Fatalf("ImportRetained: %v", err)
		}
		if _, err := db.Pool().Exec(ctx, `UPDATE retained_data SET retained_at = $2 WHERE id = $1`,
			rec.ID, base.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	walk := func(p provisioning.ListPage) []string {
		var out []string
		for pages := 0; pages < 10; pages++ {
			recs, key, id, err := st.ListRetained(ctx, "tenant-pg", "iss|sub-1", "", p)
			if err != nil {
				t.Fatalf("ListRetained(%+v): %v", p, err)
			}
			for _, r := range recs {
				if r.PurgeNonce == "" {
					t.Fatal("paged record lacks a purge nonce")
				}
				out = append(out, r.SourceWorkspaceName)
			}
			if id == "" {
				return out
			}
			p.AfterKey, p.AfterID = key, id
		}
		t.Fatal("paging did not terminate")
		return nil
	}
	if got := walk(provisioning.ListPage{Limit: 1, SortField: "retainedAt", Desc: true}); fmt.Sprint(got) != "[src-b src-a src-c]" {
		t.Fatalf("-retainedAt walk = %v", got)
	}
	if got := walk(provisioning.ListPage{Limit: 2, SortField: "name"}); fmt.Sprint(got) != "[src-a src-b src-c]" {
		t.Fatalf("name walk = %v", got)
	}
}
