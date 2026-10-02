package api

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/tinyorbitvn/tinycdi/internal/api/oidctest"
	"github.com/tinyorbitvn/tinycdi/internal/store"
)

// fakeQuotaSource serves a canned store.QuotaReport; tests fill report
// after the issuer URL is known.
type fakeQuotaSource struct {
	report store.QuotaReport
	err    error
}

func (f *fakeQuotaSource) Report(_ context.Context, tenantID string) (store.QuotaReport, error) {
	if f.err != nil {
		return store.QuotaReport{}, f.err
	}
	return f.report, nil
}

// fakeDirectory is an in-memory Directory: Remember upserts on
// (tenant, ownerRef) like the Postgres directory; Lookup serves the rows.
type fakeDirectory struct {
	mu         sync.Mutex
	entries    map[string]store.DirectoryEntry // tenantID + "\x00" + ownerRef
	remembered []store.DirectoryEntry          // every Remember call, in order
}

func newFakeDirectory() *fakeDirectory {
	return &fakeDirectory{entries: map[string]store.DirectoryEntry{}}
}

func (f *fakeDirectory) Remember(_ context.Context, tenantID string, e store.DirectoryEntry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries[tenantID+"\x00"+e.OwnerRef] = e
	f.remembered = append(f.remembered, e)
	return nil
}

func (f *fakeDirectory) Lookup(_ context.Context, tenantID string, ownerRefs []string) (map[string]store.DirectoryEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]store.DirectoryEntry{}
	for _, r := range ownerRefs {
		if e, ok := f.entries[tenantID+"\x00"+r]; ok {
			out[r] = e
		}
	}
	return out, nil
}

func (f *fakeDirectory) entry(tenantID, ownerRef string) (store.DirectoryEntry, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.entries[tenantID+"\x00"+ownerRef]
	return e, ok
}

// newQuotaEnv builds the standard OIDC test stack and mounts GET /v1/quota
// backed by the given source and directory. dir is also wired into the
// authenticator so logins upsert it.
func newQuotaEnv(t *testing.T, src QuotaSource, dir Directory, tenants TenantResolver) *testEnv {
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
	if dir != nil {
		a.WithDirectory(dir)
	}
	mux := http.NewServeMux()
	mux.Handle("/auth/login", http.HandlerFunc(a.LoginHandler))
	mux.Handle("/auth/callback", http.HandlerFunc(a.CallbackHandler))
	MountQuotaRoutes(mux, a, NewQuotaHandler(src, dir, tenants))
	srv := httptest.NewServer(RequestID(Audit(logger)(mux)))
	env := &testEnv{issuer: iss, auth: a, store: sessions, server: srv, logs: logBuf}
	t.Cleanup(func() { srv.Close(); iss.Close() })
	return env
}

// quotaFixture: two owners in the caller's tenant with usage, configured
// limits. iss is the test issuer URL so owner refs match p.Owner().
func quotaFixture(iss string) store.QuotaReport {
	return store.QuotaReport{
		HasLimits: true,
		Limits: store.QuotaAmounts{
			RunningSlots: 4, CPUMillis: 8000,
			MemoryBytes: 16 << 30, DiskBytes: 200 << 30,
		},
		Usage: store.QuotaAmounts{
			Workspaces: 3, RunningSlots: 2, CPUMillis: 3000,
			MemoryBytes: 6 << 30, DiskBytes: 60 << 30,
		},
		Owners: []store.OwnerUsage{
			{OwnerRef: iss + "|user-a", Usage: store.QuotaAmounts{
				Workspaces: 2, RunningSlots: 1, CPUMillis: 2000,
				MemoryBytes: 4 << 30, DiskBytes: 40 << 30}},
			{OwnerRef: iss + "|user-b", Usage: store.QuotaAmounts{
				Workspaces: 1, RunningSlots: 1, CPUMillis: 1000,
				MemoryBytes: 2 << 30, DiskBytes: 20 << 30}},
		},
	}
}

type quotaAmountsJSON struct {
	Workspaces        int64 `json:"workspaces"`
	RunningWorkspaces int64 `json:"runningWorkspaces"`
	CPUMillicores     int64 `json:"cpuMillicores"`
	MemoryMib         int64 `json:"memoryMib"`
	StorageGib        int64 `json:"storageGib"`
}

type userUsageJSON struct {
	Subject     string           `json:"subject"`
	DisplayName string           `json:"displayName"`
	Usage       quotaAmountsJSON `json:"usage"`
}

type quotaViewJSON struct {
	Tenant string           `json:"tenant"`
	Limits quotaAmountsJSON `json:"limits"`
	Usage  quotaAmountsJSON `json:"usage"`
	Users  []userUsageJSON  `json:"users"`
}

// TestQuota_AdminSeesAllUsers: a tenant-admin's users[] lists every owner
// that has usage, each with subject + displayName (directory-backed, with
// subject fallback).
func TestQuota_AdminSeesAllUsers(t *testing.T) {
	src := &fakeQuotaSource{}
	dir := newFakeDirectory()
	env := newQuotaEnv(t, src, dir, defaultTenants())
	src.report = quotaFixture(env.issuer.URL())
	dir.entries["tenant-a\x00"+env.issuer.URL()+"|user-a"] = store.DirectoryEntry{
		OwnerRef: env.issuer.URL() + "|user-a", Subject: "user-a", DisplayName: "Alice A"}
	env.issuer.Groups = []string{TenantAdminGroup}
	sess, csrf := login(t, env, "admin-1")

	r := doReq(t, env, sess, csrf, http.MethodGet, "/v1/quota", "", nil)
	v := decodeBody[quotaViewJSON](t, r)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200", r.StatusCode)
	}
	if v.Tenant != "tenant-a" {
		t.Fatalf("tenant=%q, want tenant-a", v.Tenant)
	}
	if len(v.Users) != 2 {
		t.Fatalf("users len=%d, want 2 (every owner)", len(v.Users))
	}
	if v.Users[0].Subject != "user-a" || v.Users[1].Subject != "user-b" {
		t.Fatalf("subjects=%q,%q, want user-a,user-b", v.Users[0].Subject, v.Users[1].Subject)
	}
	if v.Users[0].DisplayName != "Alice A" {
		t.Fatalf("displayName=%q, want directory name", v.Users[0].DisplayName)
	}
	if v.Users[1].DisplayName != "user-b" {
		t.Fatalf("displayName=%q, want subject fallback", v.Users[1].DisplayName)
	}
}

// TestQuota_UserSeesSelf: a regular user's users[] contains only their own
// row even when other owners have usage.
func TestQuota_UserSeesSelf(t *testing.T) {
	src := &fakeQuotaSource{}
	env := newQuotaEnv(t, src, newFakeDirectory(), defaultTenants())
	src.report = quotaFixture(env.issuer.URL())
	sess, csrf := login(t, env, "user-b")

	r := doReq(t, env, sess, csrf, http.MethodGet, "/v1/quota", "", nil)
	v := decodeBody[quotaViewJSON](t, r)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200", r.StatusCode)
	}
	if len(v.Users) != 1 {
		t.Fatalf("users len=%d, want 1 (self only)", len(v.Users))
	}
	if v.Users[0].Subject != "user-b" {
		t.Fatalf("subject=%q, want user-b", v.Users[0].Subject)
	}
	if v.Users[0].Usage.Workspaces != 1 {
		t.Fatalf("self usage=%d workspaces, want 1", v.Users[0].Usage.Workspaces)
	}
	// Tenant-wide numbers are still visible to the caller.
	if v.Usage.Workspaces != 3 {
		t.Fatalf("tenant usage=%d, want 3", v.Usage.Workspaces)
	}
}

// TestQuota_UnitsMapped: storage units map to the contract's display units —
// CPU stays in millicores, memory converts bytes→MiB, disk bytes→GiB.
func TestQuota_UnitsMapped(t *testing.T) {
	src := &fakeQuotaSource{}
	env := newQuotaEnv(t, src, newFakeDirectory(), defaultTenants())
	src.report = quotaFixture(env.issuer.URL())
	sess, csrf := login(t, env, "user-a")

	r := doReq(t, env, sess, csrf, http.MethodGet, "/v1/quota", "", nil)
	v := decodeBody[quotaViewJSON](t, r)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200", r.StatusCode)
	}
	if v.Limits.CPUMillicores != 8000 {
		t.Fatalf("limits.cpuMillicores=%d, want 8000", v.Limits.CPUMillicores)
	}
	if v.Limits.MemoryMib != 16*1024 {
		t.Fatalf("limits.memoryMib=%d, want %d", v.Limits.MemoryMib, 16*1024)
	}
	if v.Limits.StorageGib != 200 {
		t.Fatalf("limits.storageGib=%d, want 200", v.Limits.StorageGib)
	}
	if v.Usage.Workspaces != 3 || v.Usage.RunningWorkspaces != 2 ||
		v.Usage.CPUMillicores != 3000 || v.Usage.MemoryMib != 6*1024 || v.Usage.StorageGib != 60 {
		t.Fatalf("usage units wrong: %+v", v.Usage)
	}
}

// TestDirectory_UpsertOnLogin exercises the login-time directory upsert at
// the API seam: a second login with a changed `name` claim overwrites the
// stored display identity. The Postgres-level row update is covered by the
// integration suite.
func TestDirectory_UpsertOnLogin(t *testing.T) {
	dir := newFakeDirectory()
	env := newQuotaEnv(t, &fakeQuotaSource{}, dir, defaultTenants())
	ownerRef := env.issuer.URL() + "|user-a"

	env.issuer.MutateTokenClaims(func(c map[string]any) { c["name"] = "Alice" })
	login(t, env, "user-a")
	e, ok := dir.entry("tenant-a", ownerRef)
	if !ok {
		t.Fatal("login did not upsert the principal directory")
	}
	if e.DisplayName != "Alice" {
		t.Fatalf("displayName=%q, want Alice", e.DisplayName)
	}

	env.issuer.MutateTokenClaims(func(c map[string]any) { c["name"] = "Alice Cooper" })
	login(t, env, "user-a")
	e, ok = dir.entry("tenant-a", ownerRef)
	if !ok {
		t.Fatal("directory row vanished")
	}
	if e.DisplayName != "Alice Cooper" {
		t.Fatalf("displayName=%q, want updated Alice Cooper", e.DisplayName)
	}
	if len(dir.remembered) != 2 {
		t.Fatalf("Remember called %d times, want 2", len(dir.remembered))
	}
}

// TestQuota_NoRowOmitsLimits: a tenant without a quota row (admission fails
// closed) must not report zero limits — the limits object is omitted, so the
// portal cannot render "0 of 0" as if a quota of zero had been configured.
func TestQuota_NoRowOmitsLimits(t *testing.T) {
	src := &fakeQuotaSource{}
	env := newQuotaEnv(t, src, newFakeDirectory(), defaultTenants())
	rep := quotaFixture(env.issuer.URL())
	rep.HasLimits = false
	rep.Limits = store.QuotaAmounts{}
	src.report = rep
	sess, csrf := login(t, env, "user-a")

	r := doReq(t, env, sess, csrf, http.MethodGet, "/v1/quota", "", nil)
	raw := decodeBody[map[string]any](t, r)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200", r.StatusCode)
	}
	if _, present := raw["limits"]; present {
		t.Fatalf("limits present without a quota row: %v", raw["limits"])
	}
	if _, present := raw["usage"]; !present {
		t.Fatal("usage missing: usage is reported with or without limits")
	}
}

// TestQuota_ZeroWorkspacesMeansUnlimited: with a quota row, limits.workspaces
// is 0 because there is no workspace-count limit; openapi documents that 0
// means "no count limit" so clients do not render it as a hard zero.
func TestQuota_ZeroWorkspacesMeansUnlimited(t *testing.T) {
	src := &fakeQuotaSource{}
	env := newQuotaEnv(t, src, newFakeDirectory(), defaultTenants())
	src.report = quotaFixture(env.issuer.URL())
	sess, csrf := login(t, env, "user-a")
	r := doReq(t, env, sess, csrf, http.MethodGet, "/v1/quota", "", nil)
	v := decodeBody[quotaViewJSON](t, r)
	if v.Limits.Workspaces != 0 {
		t.Fatalf("limits.workspaces=%d, want 0 (no count limit)", v.Limits.Workspaces)
	}

	rawSpec, err := os.ReadFile("openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Components struct {
			Schemas map[string]struct {
				Required   []string `yaml:"required"`
				Properties map[string]struct {
					Description string `yaml:"description"`
				} `yaml:"properties"`
			} `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(rawSpec, &spec); err != nil {
		t.Fatal(err)
	}
	amounts := spec.Components.Schemas["QuotaAmounts"].Properties["workspaces"].Description
	if !strings.Contains(amounts, "no count limit") {
		t.Fatalf("QuotaAmounts.workspaces description %q must document 0 as 'no count limit' within limits", amounts)
	}
	for _, req := range spec.Components.Schemas["QuotaView"].Required {
		if req == "limits" {
			t.Fatal("QuotaView.limits is required in openapi; it must be optional (omitted without a quota row)")
		}
	}
}

// TestQuota_NoRowConfiguredFalse (R9b): a tenant without a quota row refuses
// every create (ErrNoQuota), so the view says configured:false explicitly —
// "no limits" must never read as unlimited. With a row it says true.
func TestQuota_NoRowConfiguredFalse(t *testing.T) {
	src := &fakeQuotaSource{}
	env := newQuotaEnv(t, src, newFakeDirectory(), defaultTenants())
	sess, csrf := login(t, env, "user-a")
	get := func(rep store.QuotaReport) map[string]any {
		src.report = rep
		r := doReq(t, env, sess, csrf, http.MethodGet, "/v1/quota", "", nil)
		if r.StatusCode != http.StatusOK {
			t.Fatalf("status=%d, want 200", r.StatusCode)
		}
		return decodeBody[map[string]any](t, r)
	}

	noRow := quotaFixture(env.issuer.URL())
	noRow.HasLimits = false
	noRow.Limits = store.QuotaAmounts{}
	if got := get(noRow)["configured"]; got != false {
		t.Fatalf("configured without a quota row = %v, want false", got)
	}
	if got := get(quotaFixture(env.issuer.URL()))["configured"]; got != true {
		t.Fatalf("configured with a quota row = %v, want true", got)
	}
}
