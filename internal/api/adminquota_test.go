// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package api

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/api/oidctest"
	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
	"github.com/tinyorbitvn/tinycdi/internal/store"
)

// fakeAdminQuotaSource keeps an in-memory limits row per tenant and serves
// a canned usage report; SetLimits mutates the row the way
// provisioning.UpsertQuota does (limits below usage are accepted).
type fakeAdminQuotaSource struct {
	mu      sync.Mutex
	hasRow  bool
	limits  store.QuotaAmounts
	usage   store.QuotaAmounts
	owners  []store.OwnerUsage
	setErr  error
	setCall int
}

func (f *fakeAdminQuotaSource) Report(_ context.Context, _ string) (store.QuotaReport, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return store.QuotaReport{
		HasLimits: f.hasRow,
		Limits:    f.limits,
		Usage:     f.usage,
		Owners:    append([]store.OwnerUsage(nil), f.owners...),
	}, nil
}

func (f *fakeAdminQuotaSource) SetLimits(_ context.Context, _ string, v provisioning.ResourceVector) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setCall++
	if f.setErr != nil {
		return f.setErr
	}
	f.hasRow = true
	f.limits = store.QuotaAmounts{
		RunningSlots: v.RunningSlots, CPUMillis: v.CPUMillis,
		MemoryBytes: v.MemoryBytes, DiskBytes: v.DiskBytes,
	}
	return nil
}

// newAdminQuotaEnv mounts GET/PUT /v1/admin/tenants/{tenant}/quota backed by
// src; managed lists the tenants declared via -tenant-quotas (config-owned).
func newAdminQuotaEnv(t *testing.T, src AdminQuotaSource, managed map[string]bool) *testEnv {
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
	MountAdminQuotaRoutes(mux, a, NewAdminQuotaHandler(src, newFakeDirectory(), defaultTenants(), managed))
	srv := httptest.NewServer(RequestID(Audit(logger)(mux)))
	env := &testEnv{issuer: iss, auth: a, store: sessions, server: srv, logs: logBuf}
	t.Cleanup(func() { srv.Close(); iss.Close() })
	return env
}

type adminQuotaViewJSON struct {
	Tenant     string           `json:"tenant"`
	Configured bool             `json:"configured"`
	Source     string           `json:"source"`
	Limits     quotaAmountsJSON `json:"limits"`
	Usage      quotaAmountsJSON `json:"usage"`
	Users      []userUsageJSON  `json:"users"`
}

const adminQuotaPath = "/v1/admin/tenants/tenant-a/quota"
const adminQuotaBody = `{"runningWorkspaces":4,"cpuMillicores":8000,"memoryMib":16384,"storageGib":200}`

// loginAdmin logs in a tenant-admin of the issuer's tenant (tenant-a).
func loginAdmin(t *testing.T, env *testEnv, subject string) (*http.Cookie, *http.Cookie) {
	t.Helper()
	env.issuer.Groups = []string{TenantAdminGroup}
	return login(t, env, subject)
}

// TestAdminQuota_Authz: the endpoint is admin-only and tenant-scoped — a
// regular user gets 403, a tenant admin naming another tenant gets 403,
// and no session gets 401.
func TestAdminQuota_Authz(t *testing.T) {
	src := &fakeAdminQuotaSource{hasRow: true, limits: store.QuotaAmounts{RunningSlots: 4}}
	env := newAdminQuotaEnv(t, src, nil)

	// Regular user (no tenant-admin group): read and write both denied.
	sess, csrf := login(t, env, "user-a")
	for _, m := range []string{http.MethodGet, http.MethodPut} {
		body := ""
		if m == http.MethodPut {
			body = adminQuotaBody
		}
		r := doReq(t, env, sess, csrf, m, adminQuotaPath, body, nil)
		e := decodeBody[Error](t, r)
		if r.StatusCode != http.StatusForbidden || e.Code != CodeForbidden {
			t.Fatalf("user %s: status=%d code=%q, want 403 FORBIDDEN", m, r.StatusCode, e.Code)
		}
	}

	// Tenant admin of tenant-a naming tenant-b: cross-tenant is denied.
	sessA, csrfA := loginAdmin(t, env, "admin-a")
	for _, m := range []string{http.MethodGet, http.MethodPut} {
		body := ""
		if m == http.MethodPut {
			body = adminQuotaBody
		}
		r := doReq(t, env, sessA, csrfA, m, "/v1/admin/tenants/tenant-b/quota", body, nil)
		e := decodeBody[Error](t, r)
		if r.StatusCode != http.StatusForbidden || e.Code != CodeForbidden {
			t.Fatalf("cross-tenant %s: status=%d code=%q, want 403 FORBIDDEN", m, r.StatusCode, e.Code)
		}
	}
	if src.setCall != 0 {
		t.Fatalf("denied requests reached SetLimits %d times", src.setCall)
	}

	// No session: 401.
	req, _ := http.NewRequest(http.MethodGet, env.server.URL+adminQuotaPath, nil)
	r, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated: status=%d, want 401", r.StatusCode)
	}
	r.Body.Close()
}

// TestAdminQuota_Source: source names the owning layer — config for
// -tenant-quotas tenants, api for a row no declaration claims, none for no
// row.
func TestAdminQuota_Source(t *testing.T) {
	src := &fakeAdminQuotaSource{}
	env := newAdminQuotaEnv(t, src, map[string]bool{"tenant-b": true})
	sess, csrf := loginAdmin(t, env, "admin-a")

	get := func(tenant string) adminQuotaViewJSON {
		r := doReq(t, env, sess, csrf, http.MethodGet, "/v1/admin/tenants/"+tenant+"/quota", "", nil)
		v := decodeBody[adminQuotaViewJSON](t, r)
		if r.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: status=%d, want 200", tenant, r.StatusCode)
		}
		return v
	}

	if v := get("tenant-a"); v.Source != QuotaSourceNone || v.Configured {
		t.Fatalf("no row: source=%q configured=%v, want none,false", v.Source, v.Configured)
	}
	src.mu.Lock()
	src.hasRow = true
	src.limits = store.QuotaAmounts{RunningSlots: 4, CPUMillis: 8000}
	src.mu.Unlock()
	if v := get("tenant-a"); v.Source != QuotaSourceAPI || !v.Configured {
		t.Fatalf("api row: source=%q configured=%v, want api,true", v.Source, v.Configured)
	}

	// tenant-b is config-declared: source is config even before the row
	// exists, and the admin can still read it — but tenant-b is not the
	// caller's tenant, so cross-tenant reads stay 403. Assert the mapping
	// via a tenant-b admin session instead.
	env2 := newAdminQuotaEnv(t, src, map[string]bool{"tenant-b": true})
	env2.issuer.TenantID = "tenant-b"
	sessB, csrfB := loginAdmin(t, env2, "admin-b")
	r := doReq(t, env2, sessB, csrfB, http.MethodGet, "/v1/admin/tenants/tenant-b/quota", "", nil)
	v := decodeBody[adminQuotaViewJSON](t, r)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("tenant-b GET: status=%d, want 200", r.StatusCode)
	}
	if v.Source != QuotaSourceConfig {
		t.Fatalf("config tenant: source=%q, want config", v.Source)
	}
}

// TestAdminQuota_PutValidation: malformed bodies and partial or negative
// limits are 400s; the row is untouched.
func TestAdminQuota_PutValidation(t *testing.T) {
	src := &fakeAdminQuotaSource{}
	env := newAdminQuotaEnv(t, src, nil)
	sess, csrf := loginAdmin(t, env, "admin-a")

	for name, body := range map[string]string{
		"not json":          `nope`,
		"empty object":      `{}`,
		"missing field":     `{"runningWorkspaces":4,"cpuMillicores":8000,"memoryMib":16384}`,
		"negative":          `{"runningWorkspaces":-1,"cpuMillicores":8000,"memoryMib":16384,"storageGib":200}`,
		"fractional":        `{"runningWorkspaces":1.5,"cpuMillicores":8000,"memoryMib":16384,"storageGib":200}`,
		"unknown field":     `{"runningWorkspaces":4,"cpuMillicores":8000,"memoryMib":16384,"storageGib":200,"workspaces":4}`,
		"string value":      `{"runningWorkspaces":"4","cpuMillicores":8000,"memoryMib":16384,"storageGib":200}`,
		"trailing document": adminQuotaBody + ` {}`,
	} {
		r := doReq(t, env, sess, csrf, http.MethodPut, adminQuotaPath, body, nil)
		e := decodeBody[Error](t, r)
		if r.StatusCode != http.StatusBadRequest || e.Code != CodeInvalidRequest {
			t.Fatalf("%s: status=%d code=%q, want 400 INVALID_REQUEST", name, r.StatusCode, e.Code)
		}
	}
	if src.setCall != 0 || src.hasRow {
		t.Fatalf("invalid writes reached the store: setCall=%d hasRow=%v", src.setCall, src.hasRow)
	}
}

// TestAdminQuota_ManagedByConfig: a tenant declared in -tenant-quotas cannot
// be written through the API — PUT answers 409 QUOTA_MANAGED_BY_CONFIG.
func TestAdminQuota_ManagedByConfig(t *testing.T) {
	src := &fakeAdminQuotaSource{hasRow: true, limits: store.QuotaAmounts{RunningSlots: 4}}
	env := newAdminQuotaEnv(t, src, map[string]bool{"tenant-a": true})
	sess, csrf := loginAdmin(t, env, "admin-a")

	r := doReq(t, env, sess, csrf, http.MethodPut, adminQuotaPath, adminQuotaBody, nil)
	e := decodeBody[Error](t, r)
	if r.StatusCode != http.StatusConflict || e.Code != CodeQuotaManagedByConfig {
		t.Fatalf("status=%d code=%q, want 409 QUOTA_MANAGED_BY_CONFIG", r.StatusCode, e.Code)
	}
	if e.Retryable {
		t.Fatal("QUOTA_MANAGED_BY_CONFIG must not be retryable")
	}
	if src.setCall != 0 {
		t.Fatalf("config-managed PUT reached SetLimits %d times", src.setCall)
	}
}

// TestAdminQuota_PutWrites: PUT stores the converted limits (display units
// → storage units) and answers the updated snapshot with source api.
func TestAdminQuota_PutWrites(t *testing.T) {
	src := &fakeAdminQuotaSource{}
	env := newAdminQuotaEnv(t, src, nil)
	sess, csrf := loginAdmin(t, env, "admin-a")

	r := doReq(t, env, sess, csrf, http.MethodPut, adminQuotaPath, adminQuotaBody, nil)
	v := decodeBody[adminQuotaViewJSON](t, r)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200", r.StatusCode)
	}
	if v.Source != QuotaSourceAPI || !v.Configured {
		t.Fatalf("after PUT: source=%q configured=%v, want api,true", v.Source, v.Configured)
	}
	if v.Limits.RunningWorkspaces != 4 || v.Limits.CPUMillicores != 8000 ||
		v.Limits.MemoryMib != 16384 || v.Limits.StorageGib != 200 {
		t.Fatalf("limits after PUT = %+v", v.Limits)
	}
	src.mu.Lock()
	stored := src.limits
	src.mu.Unlock()
	if stored.MemoryBytes != 16384<<20 || stored.DiskBytes != 200<<30 {
		t.Fatalf("stored bytes = %+v, want MiB/GiB converted to bytes", stored)
	}
}

// TestAdminQuota_LimitBelowUsageAccepted: lowering a limit under current
// usage is accepted — held reservations stay, new ones are refused — and the
// snapshot then reports usage above the limit.
func TestAdminQuota_LimitBelowUsageAccepted(t *testing.T) {
	src := &fakeAdminQuotaSource{
		hasRow: true,
		limits: store.QuotaAmounts{RunningSlots: 8, CPUMillis: 16000},
		usage:  store.QuotaAmounts{RunningSlots: 6, CPUMillis: 12000},
	}
	env := newAdminQuotaEnv(t, src, nil)
	sess, csrf := loginAdmin(t, env, "admin-a")

	r := doReq(t, env, sess, csrf, http.MethodPut, adminQuotaPath,
		`{"runningWorkspaces":2,"cpuMillicores":4000,"memoryMib":8192,"storageGib":100}`, nil)
	v := decodeBody[adminQuotaViewJSON](t, r)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200 (a lowered limit is accepted)", r.StatusCode)
	}
	if v.Usage.RunningWorkspaces <= v.Limits.RunningWorkspaces {
		t.Fatalf("usage %+v not above lowered limits %+v", v.Usage, v.Limits)
	}
}

// TestAdminQuota_Audit: the quota change is captured by the request audit —
// the http_request record carries the pseudonymous actor, the tenant and
// the mutating method+path+status.
func TestAdminQuota_Audit(t *testing.T) {
	src := &fakeAdminQuotaSource{}
	env := newAdminQuotaEnv(t, src, nil)
	sess, csrf := loginAdmin(t, env, "admin-a")

	r := doReq(t, env, sess, csrf, http.MethodPut, adminQuotaPath, adminQuotaBody, nil)
	r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200", r.StatusCode)
	}
	logs := env.logs.String()
	if !strings.Contains(logs, `"method":"PUT"`) ||
		!strings.Contains(logs, `"path":"`+adminQuotaPath+`"`) ||
		!strings.Contains(logs, `"status":200`) ||
		!strings.Contains(logs, `"actor":"oidc:`) ||
		!strings.Contains(logs, `"tenant":"tenant-a"`) {
		t.Fatalf("quota PUT not captured by the audit log:\n%s", logs)
	}
}

// TestAdminQuota_Contract: the handler's serialized view validates against
// the published AdminQuotaView schema in both source states.
func TestAdminQuota_Contract(t *testing.T) {
	usage := quotaAmounts{Workspaces: 2, RunningWorkspaces: 1, CPUMillicores: 4000, MemoryMib: 8192, StorageGib: 40}
	users := []userUsage{{Subject: "user-a", DisplayName: "User A", Usage: usage}}
	limits := quotaAmounts{RunningWorkspaces: 4, CPUMillicores: 16000, MemoryMib: 32768, StorageGib: 200}
	requireValid(t, "AdminQuotaView", adminQuotaView{
		Tenant: "acme", Configured: true, Source: QuotaSourceAPI,
		Limits: &limits, Usage: usage, Users: users,
	})
	requireValid(t, "AdminQuotaView", adminQuotaView{
		Tenant: "acme", Configured: false, Source: QuotaSourceNone,
		Usage: quotaAmounts{}, Users: []userUsage{},
	})
}
