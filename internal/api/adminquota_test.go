// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package api

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"math"
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
// a canned usage report; SetLimits mutates the row the way the Postgres
// conditional write does (limits below usage are accepted; the version
// bumps on every successful write; a stale If-Match fails).
type fakeAdminQuotaSource struct {
	mu      sync.Mutex
	hasRow  bool
	version int
	limits  store.QuotaAmounts
	usage   store.QuotaAmounts
	owners  []store.OwnerUsage
	setErr  error
	setCall int
}

func (f *fakeAdminQuotaSource) Report(_ context.Context, _ string) (store.QuotaReport, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rep := store.QuotaReport{
		HasLimits: f.hasRow,
		Limits:    f.limits,
		Usage:     f.usage,
		Owners:    append([]store.OwnerUsage(nil), f.owners...),
	}
	if f.hasRow {
		rep.Version = fmt.Sprintf("v%d", f.version)
	}
	return rep, nil
}

func (f *fakeAdminQuotaSource) SetLimits(_ context.Context, _ string, v provisioning.ResourceVector, expectedVersion string, create bool) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setCall++
	if f.setErr != nil {
		return "", f.setErr
	}
	switch {
	case create && f.hasRow:
		return "", store.ErrVersionMismatch
	case !create && (!f.hasRow || expectedVersion != fmt.Sprintf("v%d", f.version)):
		return "", store.ErrVersionMismatch
	}
	f.hasRow = true
	f.version++
	f.limits = store.QuotaAmounts{
		RunningSlots: v.RunningSlots, CPUMillis: v.CPUMillis,
		MemoryBytes: v.MemoryBytes, DiskBytes: v.DiskBytes,
	}
	return fmt.Sprintf("v%d", f.version), nil
}

// set limits directly, bypassing the If-Match check (test setup).
func (f *fakeAdminQuotaSource) seedLimits(v store.QuotaAmounts) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hasRow = true
	f.version++
	f.limits = v
}

func (f *fakeAdminQuotaSource) currentVersion() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return fmt.Sprintf("v%d", f.version)
}

// newAdminQuotaEnv mounts GET/PUT /v1/admin/tenants/{tenant}/quota backed by
// src; managed lists the tenants declared via -tenant-quotas (config-owned).
// opts apply With* knobs (e.g. WithAuditSink) to the handler before mount.
func newAdminQuotaEnv(t *testing.T, src AdminQuotaSource, managed map[string]bool, opts ...func(*AdminQuotaHandler)) *testEnv {
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
	h := NewAdminQuotaHandler(src, defaultTenants(), managed).WithQuotaCeilings(testQuotaCeilings)
	for _, o := range opts {
		o(h)
	}
	mux.Handle("/auth/login", http.HandlerFunc(a.LoginHandler))
	mux.Handle("/auth/callback", http.HandlerFunc(a.CallbackHandler))
	MountAdminQuotaRoutes(mux, a, h)
	srv := httptest.NewServer(RequestID(Audit(logger)(mux)))
	env := &testEnv{issuer: iss, auth: a, store: sessions, server: srv, logs: logBuf}
	t.Cleanup(func() { srv.Close(); iss.Close() })
	return env
}

type adminQuotaViewJSON struct {
	Tenant     string            `json:"tenant"`
	Configured bool              `json:"configured"`
	Source     string            `json:"source"`
	Limits     quotaAmountsJSON  `json:"limits"`
	Usage      quotaAmountsJSON  `json:"usage"`
	Ceiling    *quotaAmountsJSON `json:"ceiling"`
}

const adminQuotaPath = "/v1/admin/tenants/tenant-a/quota"
const adminQuotaBody = `{"runningWorkspaces":4,"cpuMillicores":8000,"memoryMib":16384,"storageGib":200}`

// testQuotaCeilings is the operator ceiling newAdminQuotaEnv declares for
// tenant-a: far above every body the other tests send, so only the
// ceiling tests see it bite.
var testQuotaCeilings = map[string]provisioning.ResourceVector{
	"tenant-a": {RunningSlots: 1000, CPUMillis: 1_000_000, MemoryBytes: 1 << 40, DiskBytes: 1 << 50},
}

// withCeilings replaces the default ceilings (nil = none declared).
func withCeilings(c map[string]provisioning.ResourceVector) func(*AdminQuotaHandler) {
	return func(h *AdminQuotaHandler) { h.WithQuotaCeilings(c) }
}

// ceilingAtBody is the operator ceiling that equals adminQuotaBody.
var ceilingAtBody = map[string]provisioning.ResourceVector{
	"tenant-a": {RunningSlots: 4, CPUMillis: 8000, MemoryBytes: 16384 << 20, DiskBytes: 200 << 30},
}

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
		e := decodeErr(t, r)
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
		e := decodeErr(t, r)
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
		r := doReq(t, env, sess, csrf, http.MethodPut, adminQuotaPath, body,
			map[string]string{"If-Match": IfMatchCreate})
		e := decodeErr(t, r)
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
	e := decodeErr(t, r)
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

	r := doReq(t, env, sess, csrf, http.MethodPut, adminQuotaPath, adminQuotaBody,
		map[string]string{"If-Match": IfMatchCreate})
	v := decodeBody[adminQuotaViewJSON](t, r)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200", r.StatusCode)
	}
	if v.Source != QuotaSourceAPI || !v.Configured {
		t.Fatalf("after PUT: source=%q configured=%v, want api,true", v.Source, v.Configured)
	}
	if et := r.Header.Get("ETag"); et == "" || et != ETagOf(src.currentVersion()) {
		t.Fatalf("PUT response ETag = %q, want the new row's strong tag", et)
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
		`{"runningWorkspaces":2,"cpuMillicores":4000,"memoryMib":8192,"storageGib":100}`,
		map[string]string{"If-Match": ETagOf(src.currentVersion())})
	v := decodeBody[adminQuotaViewJSON](t, r)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200 (a lowered limit is accepted)", r.StatusCode)
	}
	if v.Usage.RunningWorkspaces <= v.Limits.RunningWorkspaces {
		t.Fatalf("usage %+v not above lowered limits %+v", v.Usage, v.Limits)
	}
}

// requireAdminGuards asserts the security behavior every admin route keeps
// on top of its precondition: a mutation without the CSRF token is 403
// CSRF_FAILED, a non-admin principal is 403 FORBIDDEN, and an admin of
// another tenant is 403 FORBIDDEN. stored reports how many writes reached
// the store; none of the refused requests may add one.
func requireAdminGuards(t *testing.T, env *testEnv, method, path, body string, hdrs map[string]string, stored func() int) {
	t.Helper()
	groups, tenant := env.issuer.Groups, env.issuer.TenantID
	defer func() { env.issuer.Groups, env.issuer.TenantID = groups, tenant }()
	before := stored()

	if method != http.MethodGet {
		sess, _ := loginAdmin(t, env, "guard-admin")
		r := doReq(t, env, sess, &http.Cookie{Value: "wrong-token"}, method, path, body, hdrs)
		if e := decodeErr(t, r); r.StatusCode != http.StatusForbidden || e.Code != CodeCSRFFailed {
			t.Fatalf("%s %s without CSRF: status=%d code=%q, want 403 CSRF_FAILED", method, path, r.StatusCode, e.Code)
		}
	}

	env.issuer.Groups = nil
	sess, csrf := login(t, env, "guard-user")
	r := doReq(t, env, sess, csrf, method, path, body, hdrs)
	if e := decodeErr(t, r); r.StatusCode != http.StatusForbidden || e.Code != CodeForbidden {
		t.Fatalf("%s %s as non-admin: status=%d code=%q, want 403 FORBIDDEN", method, path, r.StatusCode, e.Code)
	}

	env.issuer.TenantID = "tenant-b"
	sessB, csrfB := loginAdmin(t, env, "guard-admin-b")
	r = doReq(t, env, sessB, csrfB, method, path, body, hdrs)
	if e := decodeErr(t, r); r.StatusCode != http.StatusForbidden || e.Code != CodeForbidden {
		t.Fatalf("%s %s as another tenant's admin: status=%d code=%q, want 403 FORBIDDEN", method, path, r.StatusCode, e.Code)
	}
	if n := stored(); n != before {
		t.Fatalf("refused %s %s reached the store (%d -> %d writes)", method, path, before, n)
	}
}

// requireErr asserts a refusal's status and stable code.
func requireErr(t *testing.T, r *http.Response, status int, code ErrorCode, what string) {
	t.Helper()
	e := decodeErr(t, r)
	if r.StatusCode != status || e.Code != code {
		t.Fatalf("%s: status=%d code=%q, want %d %s", what, r.StatusCode, e.Code, status, code)
	}
}

// TestAdminQuota_IfMatchMatrix: PUT is a strong-ETag precondition write.
// GET carries the ETag (and no version in the body); no If-Match is 428
// PRECONDITION_REQUIRED, a stale tag 412, the current tag 200 with a new,
// different ETag; "*" is 412 on an existing row and 200 on a fresh tenant.
func TestAdminQuota_IfMatchMatrix(t *testing.T) {
	src := &fakeAdminQuotaSource{}
	src.seedLimits(store.QuotaAmounts{RunningSlots: 8, CPUMillis: 16000})
	env := newAdminQuotaEnv(t, src, nil)
	sess, csrf := loginAdmin(t, env, "admin-a")
	put := func(hdrs map[string]string) *http.Response {
		return doReq(t, env, sess, csrf, http.MethodPut, adminQuotaPath, adminQuotaBody, hdrs)
	}

	r := doReq(t, env, sess, csrf, http.MethodGet, adminQuotaPath, "", nil)
	tag := r.Header.Get("ETag")
	body := decodeBody[map[string]any](t, r)
	if tag != ETagOf(src.currentVersion()) || !strings.HasPrefix(tag, `"`) {
		t.Fatalf("GET ETag = %q, want the strong tag of the row version", tag)
	}
	for _, k := range []string{"version", "users"} {
		if _, ok := body[k]; ok {
			t.Fatalf("GET body still carries %q: %v", k, body)
		}
	}

	requireErr(t, put(nil), http.StatusPreconditionRequired, CodePreconditionRequired, "no If-Match")
	requireErr(t, put(map[string]string{"If-Match": ETagOf("v999")}), http.StatusPreconditionFailed, CodePreconditionFailed, "stale ETag")
	requireErr(t, put(map[string]string{"If-Match": "*"}), http.StatusPreconditionFailed, CodePreconditionFailed, "* on an existing row")
	if src.setCall != 0 || src.limits.RunningSlots != 8 {
		t.Fatalf("refused preconditions reached the store: setCall=%d limits=%+v", src.setCall, src.limits)
	}

	r = put(map[string]string{"If-Match": tag})
	r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("matching ETag: status=%d, want 200", r.StatusCode)
	}
	next := r.Header.Get("ETag")
	if next == "" || next == tag || next != ETagOf(src.currentVersion()) {
		t.Fatalf("ETag after write = %q, want a new tag (was %q)", next, tag)
	}
	requireErr(t, put(map[string]string{"If-Match": tag}), http.StatusPreconditionFailed, CodePreconditionFailed, "replayed ETag")

	// A fresh tenant has no row: GET has no ETag and "*" creates.
	fresh := &fakeAdminQuotaSource{}
	env2 := newAdminQuotaEnv(t, fresh, nil)
	sess2, csrf2 := loginAdmin(t, env2, "admin-a")
	r = doReq(t, env2, sess2, csrf2, http.MethodGet, adminQuotaPath, "", nil)
	r.Body.Close()
	if r.Header.Get("ETag") != "" {
		t.Fatalf("GET without a row carries ETag %q", r.Header.Get("ETag"))
	}
	r = doReq(t, env2, sess2, csrf2, http.MethodPut, adminQuotaPath, adminQuotaBody, map[string]string{"If-Match": "*"})
	r.Body.Close()
	if r.StatusCode != http.StatusOK || r.Header.Get("ETag") == "" {
		t.Fatalf("* on a fresh tenant: status=%d ETag=%q, want 200 with a tag", r.StatusCode, r.Header.Get("ETag"))
	}

	requireAdminGuards(t, env, http.MethodPut, adminQuotaPath, adminQuotaBody,
		map[string]string{"If-Match": ETagOf(src.currentVersion())},
		func() int { src.mu.Lock(); defer src.mu.Unlock(); return src.setCall })
	requireAdminGuards(t, env, http.MethodGet, adminQuotaPath, "", nil, func() int { return 0 })
}

// TestAdminQuota_ConcurrentIfMatchOneWins: two writers holding the same
// ETag race behind a barrier; exactly one lands (200), the other is 412,
// and a GET returns the winner's limits.
func TestAdminQuota_ConcurrentIfMatchOneWins(t *testing.T) {
	src := &fakeAdminQuotaSource{}
	src.seedLimits(store.QuotaAmounts{RunningSlots: 1})
	env := newAdminQuotaEnv(t, src, nil)
	sess, csrf := loginAdmin(t, env, "admin-a")
	tag := ETagOf(src.currentVersion())

	bodies := []string{
		`{"runningWorkspaces":2,"cpuMillicores":2000,"memoryMib":2048,"storageGib":20}`,
		`{"runningWorkspaces":3,"cpuMillicores":3000,"memoryMib":3072,"storageGib":30}`,
	}
	status := make([]int, len(bodies))
	var ready, done sync.WaitGroup
	start := make(chan struct{})
	for i, b := range bodies {
		ready.Add(1)
		done.Add(1)
		go func() {
			defer done.Done()
			ready.Done()
			<-start
			// No t.Fatal off the test goroutine: a transport error
			// records status 0, which the check below rejects.
			req, _ := http.NewRequest(http.MethodPut, env.server.URL+adminQuotaPath, strings.NewReader(b))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set(env.auth.CSRFHeader(), csrf.Value)
			req.Header.Set("If-Match", tag)
			req.AddCookie(sess)
			r, err := noRedirectClient().Do(req)
			if err != nil {
				return
			}
			r.Body.Close()
			status[i] = r.StatusCode
		}()
	}
	ready.Wait()
	close(start)
	done.Wait()

	winner := -1
	for i, st := range status {
		switch st {
		case http.StatusOK:
			if winner >= 0 {
				t.Fatalf("both writers won: %v", status)
			}
			winner = i
		case http.StatusPreconditionFailed:
		default:
			t.Fatalf("statuses=%v, want one 200 and one 412", status)
		}
	}
	if winner < 0 {
		t.Fatalf("no writer won: %v", status)
	}
	v := decodeBody[adminQuotaViewJSON](t, doReq(t, env, sess, csrf, http.MethodGet, adminQuotaPath, "", nil))
	if want := int64(winner + 2); v.Limits.RunningWorkspaces != want {
		t.Fatalf("limits after the race = %+v, want the winner's runningWorkspaces=%d", v.Limits, want)
	}
}

// TestAdminQuota_Audit: the quota change is captured by the request audit —
// the http_request record carries the pseudonymous actor, the tenant and
// the mutating method+route+status.
func TestAdminQuota_Audit(t *testing.T) {
	src := &fakeAdminQuotaSource{}
	env := newAdminQuotaEnv(t, src, nil)
	sess, csrf := loginAdmin(t, env, "admin-a")

	r := doReq(t, env, sess, csrf, http.MethodPut, adminQuotaPath, adminQuotaBody,
		map[string]string{"If-Match": IfMatchCreate})
	r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200", r.StatusCode)
	}
	logs := env.logs.String()
	if !strings.Contains(logs, `"method":"PUT"`) ||
		!strings.Contains(logs, `"route":"/v1/admin/tenants/{tenant}/quota"`) ||
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
	limits := quotaAmounts{RunningWorkspaces: 4, CPUMillicores: 16000, MemoryMib: 32768, StorageGib: 200}
	requireValid(t, "AdminQuotaView", adminQuotaView{
		Tenant: "acme", Configured: true, Source: QuotaSourceAPI,
		Limits: &limits, Usage: usage,
	})
	requireValid(t, "AdminQuotaView", adminQuotaView{
		Tenant: "acme", Configured: false, Source: QuotaSourceNone,
		Usage: quotaAmounts{},
	})
	requireValid(t, "AdminQuotaView", adminQuotaView{
		Tenant: "acme", Configured: true, Source: QuotaSourceAPI,
		Limits: &limits, Usage: usage,
		Ceiling: &quotaCeilingView{RunningWorkspaces: 8, CPUMillicores: 32000, MemoryMib: 65536, StorageGib: 400},
	})
}

// TestAdminQuota_PutAboveOperatorCeilingRefused (R1-02): a tenant admin
// cannot raise the tenant quota past the operator-declared ceiling — one
// dimension above it is 409 QUOTA_ABOVE_CEILING and nothing is stored.
func TestAdminQuota_PutAboveOperatorCeilingRefused(t *testing.T) {
	for name, body := range map[string]string{
		"running": `{"runningWorkspaces":5,"cpuMillicores":8000,"memoryMib":16384,"storageGib":200}`,
		"cpu":     `{"runningWorkspaces":4,"cpuMillicores":8001,"memoryMib":16384,"storageGib":200}`,
		"memory":  `{"runningWorkspaces":4,"cpuMillicores":8000,"memoryMib":16385,"storageGib":200}`,
		"storage": `{"runningWorkspaces":4,"cpuMillicores":8000,"memoryMib":16384,"storageGib":201}`,
		"maxint": fmt.Sprintf(`{"runningWorkspaces":%d,"cpuMillicores":%d,"memoryMib":%d,"storageGib":%d}`,
			int64(math.MaxInt64), int64(math.MaxInt64), int64(math.MaxInt64>>20), int64(math.MaxInt64>>30)),
	} {
		t.Run(name, func(t *testing.T) {
			src := &fakeAdminQuotaSource{}
			env := newAdminQuotaEnv(t, src, nil, withCeilings(ceilingAtBody))
			sess, csrf := loginAdmin(t, env, "admin-a")
			r := doReq(t, env, sess, csrf, http.MethodPut, adminQuotaPath, body,
				map[string]string{"If-Match": IfMatchCreate})
			requireErr(t, r, http.StatusConflict, CodeQuotaAboveCeiling, name+" above the ceiling")
			if src.setCall != 0 {
				t.Fatalf("a PUT above the ceiling reached SetLimits %d times", src.setCall)
			}
		})
	}
}

// TestAdminQuota_UndeclaredTenantCreateRefused (R1-02): with no ceiling
// declared for the tenant, a tenant admin can neither create the quota row
// (the fail-closed QUOTA_NOT_CONFIGURED default stays) nor change an
// existing API-managed row.
func TestAdminQuota_UndeclaredTenantCreateRefused(t *testing.T) {
	for name, tc := range map[string]struct {
		src     *fakeAdminQuotaSource
		ifMatch func(*fakeAdminQuotaSource) string
	}{
		"create": {&fakeAdminQuotaSource{}, func(*fakeAdminQuotaSource) string { return IfMatchCreate }},
		"update": {&fakeAdminQuotaSource{hasRow: true, limits: store.QuotaAmounts{RunningSlots: 1}},
			func(s *fakeAdminQuotaSource) string { return ETagOf(s.currentVersion()) }},
	} {
		t.Run(name, func(t *testing.T) {
			env := newAdminQuotaEnv(t, tc.src, nil, withCeilings(nil))
			sess, csrf := loginAdmin(t, env, "admin-a")
			r := doReq(t, env, sess, csrf, http.MethodPut, adminQuotaPath, adminQuotaBody,
				map[string]string{"If-Match": tc.ifMatch(tc.src)})
			e := decodeErr(t, r)
			if r.StatusCode != http.StatusConflict || e.Code != CodeQuotaAboveCeiling {
				t.Fatalf("status=%d code=%q, want 409 QUOTA_ABOVE_CEILING", r.StatusCode, e.Code)
			}
			if e.Retryable {
				t.Fatal("QUOTA_ABOVE_CEILING must not be retryable")
			}
			if tc.src.setCall != 0 {
				t.Fatalf("a PUT without a declared ceiling reached SetLimits %d times", tc.src.setCall)
			}
		})
	}
}

// TestAdminQuota_PutAtCeilingAccepted (R1-02): the ceiling is inclusive — a
// write exactly at it is stored — and lowering the quota below current
// usage under a ceiling is still accepted. The view reports the ceiling.
func TestAdminQuota_PutAtCeilingAccepted(t *testing.T) {
	src := &fakeAdminQuotaSource{}
	env := newAdminQuotaEnv(t, src, nil, withCeilings(ceilingAtBody))
	sess, csrf := loginAdmin(t, env, "admin-a")

	r := doReq(t, env, sess, csrf, http.MethodPut, adminQuotaPath, adminQuotaBody,
		map[string]string{"If-Match": IfMatchCreate})
	v := decodeBody[adminQuotaViewJSON](t, r)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("PUT at the ceiling: status=%d, want 200", r.StatusCode)
	}
	if v.Ceiling == nil || v.Ceiling.RunningWorkspaces != 4 || v.Ceiling.CPUMillicores != 8000 ||
		v.Ceiling.MemoryMib != 16384 || v.Ceiling.StorageGib != 200 {
		t.Fatalf("view ceiling = %+v, want the declared ceiling in display units", v.Ceiling)
	}

	src.mu.Lock()
	src.usage = store.QuotaAmounts{RunningSlots: 3, CPUMillis: 6000, MemoryBytes: 12 << 30, DiskBytes: 150 << 30}
	src.mu.Unlock()
	r = doReq(t, env, sess, csrf, http.MethodPut, adminQuotaPath,
		`{"runningWorkspaces":1,"cpuMillicores":1000,"memoryMib":1024,"storageGib":10}`,
		map[string]string{"If-Match": ETagOf(src.currentVersion())})
	r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("lowering below usage under a ceiling: status=%d, want 200", r.StatusCode)
	}
	if src.setCall != 2 {
		t.Fatalf("SetLimits calls = %d, want 2", src.setCall)
	}
}
