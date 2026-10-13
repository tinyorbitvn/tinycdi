// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package api

import (
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
)

// seedWorkspace puts a record straight into the fake backend.
func seedWorkspace(be *fakeWorkspaceBackend, env *testEnv, subject, id, name string, created time.Time) {
	be.mu.Lock()
	defer be.mu.Unlock()
	be.recs[id] = provisioning.WorkspaceRecord{
		ID: id, TenantID: "tenant-a", Owner: envOwner(env, subject), Name: name,
		Phase: "Stopped", DesiredState: "Stopped", CreatedAt: created, UpdatedAt: created,
	}
}

func listWorkspacesPage(t *testing.T, env *testEnv, sess, csrf *http.Cookie, query string) Page[WorkspaceView] {
	t.Helper()
	r := doReq(t, env, sess, csrf, http.MethodGet, "/v1/workspaces?"+query, "", nil)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/workspaces?%s: status=%d body=%+v", query, r.StatusCode, decodeErr(t, r))
	}
	return decodeBody[Page[WorkspaceView]](t, r)
}

// TestWorkspacesList_PagingStableUnderInsert: walking the name-sorted list
// three at a time returns every seeded workspace exactly once and in order,
// even when a row that sorts before the cursor is inserted mid-walk.
func TestWorkspacesList_PagingStableUnderInsert(t *testing.T) {
	be := newFakeBackend()
	env := newWorkspaceEnv(t, be, defaultCatalog(), defaultTenants())
	sess, csrf := login(t, env, "user-a")
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	seeded := []string{"bravo", "charlie", "delta", "echo", "foxtrot", "golf", "hotel"}
	for i, n := range seeded {
		seedWorkspace(be, env, "user-a", fmt.Sprintf("ws_seed%08d", i), n, base.Add(time.Duration(i)*time.Minute))
	}

	var got []string
	q := "limit=3&sort=name"
	for pages := 0; ; pages++ {
		if pages > 5 {
			t.Fatal("paging did not terminate")
		}
		p := listWorkspacesPage(t, env, sess, csrf, q)
		for _, it := range p.Items {
			got = append(got, it.Name)
		}
		if pages == 0 {
			seedWorkspace(be, env, "user-a", "ws_inserted0001", "alpha", base.Add(time.Hour))
		}
		if p.NextCursor == "" {
			break
		}
		q = "limit=3&sort=name&cursor=" + url.QueryEscape(p.NextCursor)
	}
	if fmt.Sprint(got) != fmt.Sprint(seeded) {
		t.Fatalf("walk = %v, want %v (each seeded name once, in order)", got, seeded)
	}
}

// TestWorkspacesList_SortDescCreatedAt: sort=-createdAt lists newest first.
func TestWorkspacesList_SortDescCreatedAt(t *testing.T) {
	be := newFakeBackend()
	env := newWorkspaceEnv(t, be, defaultCatalog(), defaultTenants())
	sess, csrf := login(t, env, "user-a")
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	seedWorkspace(be, env, "user-a", "ws_old00000001", "old", base)
	seedWorkspace(be, env, "user-a", "ws_mid00000001", "mid", base.Add(time.Minute))
	seedWorkspace(be, env, "user-a", "ws_new00000001", "new", base.Add(2*time.Minute))

	p := listWorkspacesPage(t, env, sess, csrf, "sort=-createdAt")
	var got []string
	for _, it := range p.Items {
		got = append(got, it.Name)
	}
	if fmt.Sprint(got) != "[new mid old]" || p.NextCursor != "" {
		t.Fatalf("items=%v next=%q, want [new mid old] and no cursor", got, p.NextCursor)
	}
}

// TestWorkspaceActions_VerbPaths: start/stop are :verb custom methods; the
// old subpaths are gone and CSRF still guards the action.
func TestWorkspaceActions_VerbPaths(t *testing.T) {
	be := newFakeBackend()
	env := newWorkspaceEnv(t, be, defaultCatalog(), defaultTenants())
	sess, csrf := login(t, env, "user-a")
	const id = "ws_verb00000001"
	seedWorkspace(be, env, "user-a", id, "verbs", time.Now().UTC())

	for _, tc := range []struct{ verb, key string }{{"start", "key-verb-start1"}, {"stop", ""}} {
		hdrs := map[string]string{}
		if tc.key != "" {
			hdrs["Idempotency-Key"] = tc.key
		}
		r := doReq(t, env, sess, csrf, http.MethodPost, "/v1/workspaces/"+id+":"+tc.verb, "", hdrs)
		if r.StatusCode != http.StatusAccepted {
			t.Fatalf(":%s status=%d body=%+v, want 202", tc.verb, r.StatusCode, decodeErr(t, r))
		}
		v := decodeBody[WorkspaceView](t, r)
		if v.ID != id {
			t.Fatalf(":%s view id=%q, want %s", tc.verb, v.ID, id)
		}

		r = doReq(t, env, sess, csrf, http.MethodPost, "/v1/workspaces/"+id+"/"+tc.verb, "", hdrs)
		r.Body.Close()
		if r.StatusCode != http.StatusNotFound {
			t.Fatalf("old /%s subpath status=%d, want 404", tc.verb, r.StatusCode)
		}

		r = doReq(t, env, sess, &http.Cookie{Name: csrf.Name}, http.MethodPost, "/v1/workspaces/"+id+":"+tc.verb, "", hdrs)
		if e := decodeErr(t, r); r.StatusCode != http.StatusForbidden || e.Code != CodeCSRFFailed {
			t.Fatalf(":%s without CSRF: status=%d code=%s, want 403 CSRF_FAILED", tc.verb, r.StatusCode, e.Code)
		}
	}
}
