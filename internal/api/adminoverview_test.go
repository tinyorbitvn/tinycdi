// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

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
	"github.com/tinyorbitvn/tinycdi/internal/store"
)

type fakeOverviewDB struct{ calls map[string]int }

func (f *fakeOverviewDB) TenantOverview(_ context.Context, tenant string, _ time.Time) (store.TenantOverview, error) {
	f.calls[tenant]++
	p := 42.5
	live := int64(3)
	if tenant == "tenant-b" {
		live = 7
	}
	return store.TenantOverview{LiveSessions: live, ProvisioningP95Seconds: &p, ProvisioningSamples: 9}, nil
}

type fakeCounter struct{}

func (fakeCounter) TenantCounts(context.Context, string, time.Duration) (TenantCounts, error) {
	return TenantCounts{Fresh: true, RunningByTemplate: map[string]int{"b": 1, "a": 2}, Phases: map[string]int{"Ready": 3}, StuckFinalizers: 1}, nil
}

// newAdminOverviewEnv mounts GET /v1/admin/tenants/{tenant}/overview
// backed by h, like newSessionPolicyEnv.
func newAdminOverviewEnv(t *testing.T, h *AdminOverviewHandler) *testEnv {
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
	mux := http.NewServeMux()
	mux.Handle("/auth/login", http.HandlerFunc(a.LoginHandler))
	mux.Handle("/auth/callback", http.HandlerFunc(a.CallbackHandler))
	MountAdminOverviewRoutes(mux, a, h)
	srv := httptest.NewServer(RequestID(Audit(logger)(mux)))
	env := &testEnv{issuer: iss, auth: a, store: sessions, server: srv, logs: logBuf}
	t.Cleanup(func() { srv.Close(); iss.Close() })
	return env
}

func TestAdminOverview_AggregatesAndCaches(t *testing.T) {
	db := &fakeOverviewDB{calls: map[string]int{}}
	h := NewAdminOverviewHandler(db, fakeCounter{}, defaultTenants())
	clock := time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)
	h.now = func() time.Time { return clock }
	env := newAdminOverviewEnv(t, h)
	sess, csrf := loginAdmin(t, env, "admin-a")
	get := func() adminOverview {
		res := doReq(t, env, sess, csrf, http.MethodGet, "/v1/admin/tenants/tenant-a/overview", "", nil)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("status %d", res.StatusCode)
		}
		return decodeBody[adminOverview](t, res)
	}
	o := get()
	if o.LiveSessions != 3 || o.StuckFinalizers != 1 || *o.Provisioning.P95Seconds != 42.5 ||
		len(o.RunningByTemplate) != 2 || o.RunningByTemplate[0].Template != "a" {
		t.Fatalf("overview = %+v", o)
	}
	clock = clock.Add(10 * time.Second)
	get()
	if db.calls["tenant-a"] != 1 {
		t.Fatalf("calls within 15 s = %d, want 1 (cached)", db.calls["tenant-a"])
	}
	clock = clock.Add(6 * time.Second)
	get()
	if db.calls["tenant-a"] != 2 {
		t.Fatalf("calls after 16 s = %d, want 2", db.calls["tenant-a"])
	}
}

// The cache key is the tenant: an administrator of tenant-b never gets
// tenant-a's cached answer.
func TestAdminOverview_CacheIsPerTenant(t *testing.T) {
	db := &fakeOverviewDB{calls: map[string]int{}}
	h := NewAdminOverviewHandler(db, fakeCounter{}, defaultTenants())
	env := newAdminOverviewEnv(t, h)
	sa, ca := loginAdmin(t, env, "admin-a")
	a := decodeBody[adminOverview](t, doReq(t, env, sa, ca, http.MethodGet, "/v1/admin/tenants/tenant-a/overview", "", nil))
	env.issuer.TenantID = "tenant-b"
	sb, cb := loginAdmin(t, env, "admin-b")
	b := decodeBody[adminOverview](t, doReq(t, env, sb, cb, http.MethodGet, "/v1/admin/tenants/tenant-b/overview", "", nil))
	if a.LiveSessions != 3 || b.LiveSessions != 7 || db.calls["tenant-b"] != 1 {
		t.Fatalf("a = %+v, b = %+v, calls = %v: each tenant needs its own cache entry", a, b, db.calls)
	}
	// tenant-b's administrator naming tenant-a is refused and never reaches the aggregate.
	before := db.calls["tenant-a"]
	if res := doReq(t, env, sb, cb, http.MethodGet, "/v1/admin/tenants/tenant-a/overview", "", nil); res.StatusCode != http.StatusForbidden || db.calls["tenant-a"] != before {
		t.Fatalf("cross-tenant overview: status %d, calls %v", res.StatusCode, db.calls)
	}
}

func TestAdminOverview_NonAdminRefused(t *testing.T) {
	db := &fakeOverviewDB{calls: map[string]int{}}
	env := newAdminOverviewEnv(t, NewAdminOverviewHandler(db, fakeCounter{}, defaultTenants()))
	sess, csrf := loginGroups(t, env, "user-a")
	if res := doReq(t, env, sess, csrf, http.MethodGet, "/v1/admin/tenants/tenant-a/overview", "", nil); res.StatusCode != http.StatusForbidden || len(db.calls) != 0 {
		t.Fatalf("non-admin: status %d, calls %v", res.StatusCode, db.calls)
	}
}
