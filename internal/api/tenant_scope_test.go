package api

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/api/oidctest"
	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
)

// newScopeEnv mounts both list endpoints (/v1/workspaces, /v1/data) plus the
// login flow, so scope= tests exercise the shared rule on both.
func newScopeEnv(t *testing.T, be workspaceBackend, ds RetainedDataStore, dir Directory) *testEnv {
	t.Helper()
	iss, err := oidctest.NewIssuer()
	if err != nil {
		t.Fatalf("oidctest.NewIssuer: %v", err)
	}
	logBuf := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(logBuf, nil))
	sessions := NewInMemorySessionStore(30 * time.Minute)
	a, err := NewAuthenticator(context.Background(), AuthConfig{
		Issuer:      iss.URL(),
		ClientID:    iss.ClientID,
		RedirectURL: "https://portal.test/auth/callback",
		LoginSealer: testLoginSealer(t),
	}, sessions, logger)
	if err != nil {
		t.Fatalf("NewAuthenticator: %v", err)
	}
	wh := NewWorkspaceHandler(be, defaultCatalog(), defaultTenants())
	if dir != nil {
		wh.WithDirectory(dir)
	}
	dh := NewDataHandler(ds, defaultCatalog(), defaultTenants())
	if dir != nil {
		dh.WithDirectory(dir)
	}
	mux := http.NewServeMux()
	mux.Handle("/auth/login", http.HandlerFunc(a.LoginHandler))
	mux.Handle("/auth/callback", http.HandlerFunc(a.CallbackHandler))
	MountWorkspaceRoutes(mux, a, wh, nil)
	MountDataRoutes(mux, a, dh)
	srv := httptest.NewServer(RequestID(Audit(logger)(mux)))
	env := &testEnv{issuer: iss, auth: a, store: sessions, server: srv, logs: logBuf}
	t.Cleanup(func() { srv.Close(); iss.Close() })
	return env
}

// TestScopeTenant_ForbiddenForUser: scope=tenant on either list endpoint
// requires the tenant-admin role; a plain user gets 403 FORBIDDEN (D34).
func TestScopeTenant_ForbiddenForUser(t *testing.T) {
	env := newScopeEnv(t, newFakeBackend(), newFakeRetainedStore(), nil)
	sess, csrf := login(t, env, "user-a")

	for _, path := range []string{"/v1/workspaces?scope=tenant", "/v1/data?scope=tenant"} {
		r := doReq(t, env, sess, csrf, http.MethodGet, path, "", nil)
		body := decodeBody[Error](t, r)
		if r.StatusCode != http.StatusForbidden {
			t.Fatalf("%s: status=%d, want 403", path, r.StatusCode)
		}
		if body.Code != CodeForbidden {
			t.Fatalf("%s: code=%q, want FORBIDDEN", path, body.Code)
		}
	}
}

// TestScopeTenant_CrossTenantNeverLeaks: an admin of tenant A with
// scope=tenant sees only tenant A's rows — tenant B's workspaces and
// retained records are never listed.
func TestScopeTenant_CrossTenantNeverLeaks(t *testing.T) {
	be := newFakeBackend()
	fs := newFakeRetainedStore()
	env := newScopeEnv(t, be, fs, nil)

	// Seed tenant-b rows straight into the fakes (login is fixed to
	// tenant-a; foreign rows must still never appear).
	be.mu.Lock()
	be.recs["ws_bbbbbbbbbbbbbbbbbbbbbbbbbb"] = provisioning.WorkspaceRecord{
		ID: "ws_bbbbbbbbbbbbbbbbbbbbbbbbbb", TenantID: "tenant-b",
		Owner: env.issuer.URL() + "|user-b", OwnerSub: "user-b",
		Name: "b-desktop", Phase: "Ready",
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	be.mu.Unlock()
	fs.seed(RetainedRecord{
		ID: "rd_bbbbbbbbbbbbbbbbbbbbbbbbbb", TenantID: "tenant-b",
		Owner: env.issuer.URL() + "|user-b", State: RetainedStateRetained,
		SizeBytes: 1 << 30, Runtime: "LinuxContainer",
		SourceWorkspaceName: "b-desktop", RetainedAt: time.Now().UTC(),
	})

	env.issuer.Groups = []string{TenantAdminGroup}
	sess, csrf := login(t, env, "admin-a")

	r := doReq(t, env, sess, csrf, http.MethodPost, "/v1/workspaces",
		`{"name":"a-desktop","templateRef":"tpl_linuxdesktop"}`,
		map[string]string{"Idempotency-Key": "key-scope-1000"})
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("create status=%d", r.StatusCode)
	}
	r.Body.Close()
	fs.seed(RetainedRecord{
		ID: "rd_aaaaaaaaaaaaaaaaaaaaaaaaaa", TenantID: "tenant-a",
		Owner: env.issuer.URL() + "|admin-a", State: RetainedStateRetained,
		SizeBytes: 1 << 30, Runtime: "LinuxContainer",
		SourceWorkspaceName: "a-desktop", RetainedAt: time.Now().UTC(),
	})

	r = doReq(t, env, sess, csrf, http.MethodGet, "/v1/workspaces?scope=tenant", "", nil)
	wl := decodeBody[WorkspaceList](t, r)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("workspaces status=%d", r.StatusCode)
	}
	for _, it := range wl.Items {
		if it.ID == "ws_bbbbbbbbbbbbbbbbbbbbbbbbbb" || it.Name == "b-desktop" {
			t.Fatalf("tenant-b workspace leaked: %+v", it)
		}
	}
	if len(wl.Items) != 1 {
		t.Fatalf("tenant scope listed %d workspaces, want 1 (tenant-a only)", len(wl.Items))
	}

	r = doReq(t, env, sess, csrf, http.MethodGet, "/v1/data?scope=tenant", "", nil)
	dl := decodeBody[retainedDataList](t, r)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("data status=%d", r.StatusCode)
	}
	for _, it := range dl.Items {
		if it.ID == "rd_bbbbbbbbbbbbbbbbbbbbbbbbbb" {
			t.Fatalf("tenant-b retained record leaked: %+v", it)
		}
	}
	if len(dl.Items) != 1 {
		t.Fatalf("tenant scope listed %d records, want 1 (tenant-a only)", len(dl.Items))
	}
}

// TestScopeMine_RestrictsAdmin: scope=mine narrows even a tenant-admin to
// their own rows; an unknown scope value is a 400.
func TestScopeMine_RestrictsAdmin(t *testing.T) {
	be := newFakeBackend()
	env := newScopeEnv(t, be, newFakeRetainedStore(), nil)

	sessB, csrfB := login(t, env, "user-b")
	r := doReq(t, env, sessB, csrfB, http.MethodPost, "/v1/workspaces",
		`{"name":"b-desktop","templateRef":"tpl_linuxdesktop"}`,
		map[string]string{"Idempotency-Key": "key-scope-2000"})
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("create status=%d", r.StatusCode)
	}
	r.Body.Close()

	env.issuer.Groups = []string{TenantAdminGroup}
	sessA, csrfA := login(t, env, "admin-a")
	r = doReq(t, env, sessA, csrfA, http.MethodPost, "/v1/workspaces",
		`{"name":"a-desktop","templateRef":"tpl_linuxdesktop"}`,
		map[string]string{"Idempotency-Key": "key-scope-3000"})
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("create status=%d", r.StatusCode)
	}
	r.Body.Close()

	r = doReq(t, env, sessA, csrfA, http.MethodGet, "/v1/workspaces?scope=mine", "", nil)
	wl := decodeBody[WorkspaceList](t, r)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", r.StatusCode)
	}
	for _, it := range wl.Items {
		if it.Name == "b-desktop" {
			t.Fatalf("scope=mine leaked another owner's workspace: %+v", it)
		}
	}
	if len(wl.Items) != 1 || wl.Items[0].Name != "a-desktop" {
		t.Fatalf("scope=mine items=%+v", wl.Items)
	}

	r = doReq(t, env, sessA, csrfA, http.MethodGet, "/v1/workspaces?scope=bogus", "", nil)
	decodeBody[Error](t, r)
	if r.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad scope: status=%d, want 400", r.StatusCode)
	}
}
