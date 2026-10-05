// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/api/oidctest"
	"github.com/tinyorbitvn/tinycdi/internal/observability"
	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
	"github.com/tinyorbitvn/tinycdi/internal/store"
)

// fakeUserLimitSource serves the admin user-limits seam from memory:
// Report merges the stored overrides/default into the canned quota report
// the way the Postgres-backed source does, and the write methods mutate
// that state so GET-after-PUT assertions read what admission would.
type fakeUserLimitSource struct {
	mu        sync.Mutex
	report    store.QuotaReport
	overrides map[string]int64
	def       *int64
	setErr    error
	writes    []string // "set:ref:n" | "clear:ref" | "default:n" | "default:clear"
}

func (f *fakeUserLimitSource) Report(_ context.Context, _ string) (store.QuotaReport, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rep := f.report
	rep.DefaultUserLimit = f.def
	rep.UserLimits = map[string]int64{}
	for k, v := range f.overrides {
		rep.UserLimits[k] = v
	}
	return rep, nil
}

func (f *fakeUserLimitSource) SetOverride(_ context.Context, _ string, owner string, max int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.setErr != nil {
		return f.setErr
	}
	f.overrides[owner] = max
	f.writes = append(f.writes, "set:"+owner)
	return nil
}

func (f *fakeUserLimitSource) ClearOverride(_ context.Context, _ string, owner string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.setErr != nil {
		return f.setErr
	}
	delete(f.overrides, owner)
	f.writes = append(f.writes, "clear:"+owner)
	return nil
}

func (f *fakeUserLimitSource) SetDefault(_ context.Context, _ string, max int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.setErr != nil {
		return f.setErr
	}
	v := max
	f.def = &v
	f.writes = append(f.writes, "default")
	return nil
}

func (f *fakeUserLimitSource) ClearDefault(_ context.Context, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.setErr != nil {
		return f.setErr
	}
	f.def = nil
	f.writes = append(f.writes, "default:clear")
	return nil
}

// newUserLimitsEnv mounts GET/PUT /v1/admin/tenants/{tenant}/user-limits
// backed by src; sink may be nil.
func newUserLimitsEnv(t *testing.T, src UserLimitSource, sink observability.AuditSink) *testEnv {
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
	h := NewAdminUserLimitsHandler(src, newFakeDirectory(), defaultTenants())
	if sink != nil {
		h.WithAuditSink(sink)
	}
	MountAdminUserLimitRoutes(mux, a, h)
	srv := httptest.NewServer(RequestID(Audit(logger)(mux)))
	env := &testEnv{issuer: iss, auth: a, store: sessions, server: srv, logs: logBuf}
	t.Cleanup(func() { srv.Close(); iss.Close() })
	return env
}

type userLimitEntryJSON struct {
	OwnerRef    string `json:"ownerRef"`
	Subject     string `json:"subject"`
	DisplayName string `json:"displayName"`
	Limit       *int64 `json:"limit"`
	Effective   *int64 `json:"effective"`
	Running     int64  `json:"running"`
}

type userLimitsViewJSON struct {
	Tenant  string               `json:"tenant"`
	Default *int64               `json:"default"`
	Users   []userLimitEntryJSON `json:"users"`
}

const userLimitsPath = "/v1/admin/tenants/tenant-a/user-limits"

// ulFixture: two owners with usage (caller is user-a by default login).
func ulFixture(iss string) store.QuotaReport {
	return store.QuotaReport{
		HasLimits: true,
		Owners: []store.OwnerUsage{
			{OwnerRef: iss + "|user-a", Usage: store.QuotaAmounts{RunningSlots: 2}},
			{OwnerRef: iss + "|user-b", Usage: store.QuotaAmounts{RunningSlots: 1}},
		},
	}
}

// TestAdminUserLimits_Authz: tenant-admin of the named tenant only — a
// regular user gets 403, a tenant admin naming another tenant gets 403,
// and no session gets 401.
func TestAdminUserLimits_Authz(t *testing.T) {
	src := &fakeUserLimitSource{report: ulFixture("x"), overrides: map[string]int64{}}
	env := newUserLimitsEnv(t, src, nil)

	// Regular user: read and writes all denied.
	sess, csrf := login(t, env, "user-a")
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodGet, userLimitsPath, ""},
		{http.MethodPut, userLimitsPath, `{"ownerRef":"iss|sub","limit":3}`},
		{http.MethodPut, userLimitsPath + "/default", `{"limit":3}`},
	} {
		r := doReq(t, env, sess, csrf, tc.method, tc.path, tc.body, nil)
		e := decodeBody[Error](t, r)
		if r.StatusCode != http.StatusForbidden || e.Code != CodeForbidden {
			t.Fatalf("user %s %s: status=%d code=%q, want 403 FORBIDDEN", tc.method, tc.path, r.StatusCode, e.Code)
		}
	}

	// Tenant admin of tenant-a naming tenant-b: cross-tenant denied.
	sessA, csrfA := loginAdmin(t, env, "admin-a")
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodGet, "/v1/admin/tenants/tenant-b/user-limits", ""},
		{http.MethodPut, "/v1/admin/tenants/tenant-b/user-limits", `{"ownerRef":"iss|sub","limit":3}`},
		{http.MethodPut, "/v1/admin/tenants/tenant-b/user-limits/default", `{"limit":3}`},
	} {
		r := doReq(t, env, sessA, csrfA, tc.method, tc.path, tc.body, nil)
		e := decodeBody[Error](t, r)
		if r.StatusCode != http.StatusForbidden || e.Code != CodeForbidden {
			t.Fatalf("cross-tenant %s %s: status=%d code=%q, want 403 FORBIDDEN", tc.method, tc.path, r.StatusCode, e.Code)
		}
	}
	if len(src.writes) != 0 {
		t.Fatalf("denied requests reached the store: %v", src.writes)
	}

	// No session: 401.
	req, _ := http.NewRequest(http.MethodGet, env.server.URL+userLimitsPath, nil)
	r, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated: status=%d, want 401", r.StatusCode)
	}
	r.Body.Close()
}

// TestAdminUserLimits_GetView: the view carries the tenant default and one
// row per principal with usage or an override — including an override on a
// principal with no usage yet.
func TestAdminUserLimits_GetView(t *testing.T) {
	src := &fakeUserLimitSource{overrides: map[string]int64{}}
	env := newUserLimitsEnv(t, src, nil)
	src.report = ulFixture(env.issuer.URL())
	def := int64(4)
	src.def = &def
	src.overrides[env.issuer.URL()+"|user-b"] = 2
	src.overrides[env.issuer.URL()+"|ghost"] = 7
	sess, csrf := loginAdmin(t, env, "admin-a")

	r := doReq(t, env, sess, csrf, http.MethodGet, userLimitsPath, "", nil)
	v := decodeBody[userLimitsViewJSON](t, r)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200", r.StatusCode)
	}
	if v.Default == nil || *v.Default != 4 {
		t.Fatalf("default=%v, want 4", v.Default)
	}
	if len(v.Users) != 3 {
		t.Fatalf("users len=%d, want 3 (usage owners + override-only)", len(v.Users))
	}
	by := map[string]userLimitEntryJSON{}
	for _, u := range v.Users {
		by[u.Subject] = u
	}
	a := by["user-a"]
	if a.Running != 2 || a.Limit != nil || a.Effective == nil || *a.Effective != 4 {
		t.Fatalf("user-a row=%+v, want running=2 limit=null effective=4", a)
	}
	b := by["user-b"]
	if b.Limit == nil || *b.Limit != 2 || b.Effective == nil || *b.Effective != 2 || b.Running != 1 {
		t.Fatalf("user-b row=%+v, want limit=2 effective=2 running=1", b)
	}
	g := by["ghost"]
	if g.Limit == nil || *g.Limit != 7 || g.Effective == nil || *g.Effective != 7 || g.Running != 0 {
		t.Fatalf("ghost row=%+v, want override 7, running 0", g)
	}
}

// TestAdminUserLimits_PutOverrideAndDefault: PUT sets or clears rows and
// the response is the refreshed view.
func TestAdminUserLimits_PutOverrideAndDefault(t *testing.T) {
	src := &fakeUserLimitSource{overrides: map[string]int64{}}
	env := newUserLimitsEnv(t, src, nil)
	src.report = ulFixture(env.issuer.URL())
	sess, csrf := loginAdmin(t, env, "admin-a")
	me := env.issuer.URL() + "|user-a"

	// Set an override on the caller.
	r := doReq(t, env, sess, csrf, http.MethodPut, userLimitsPath,
		`{"ownerRef":"`+me+`","limit":5}`, nil)
	v := decodeBody[userLimitsViewJSON](t, r)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("PUT override: status=%d, want 200", r.StatusCode)
	}
	var meRow *userLimitEntryJSON
	for i := range v.Users {
		if v.Users[i].OwnerRef == me {
			meRow = &v.Users[i]
		}
	}
	if meRow == nil || meRow.Limit == nil || *meRow.Limit != 5 || meRow.Effective == nil || *meRow.Effective != 5 {
		t.Fatalf("view after set=%+v, want override+effective 5", meRow)
	}

	// Set the tenant default; the override still wins for the caller.
	r = doReq(t, env, sess, csrf, http.MethodPut, userLimitsPath+"/default", `{"limit":1}`, nil)
	v = decodeBody[userLimitsViewJSON](t, r)
	if r.StatusCode != http.StatusOK || v.Default == nil || *v.Default != 1 {
		t.Fatalf("PUT default: status=%d default=%v, want 200 limit 1", r.StatusCode, v.Default)
	}
	for _, u := range v.Users {
		if u.Subject == "user-b" && (u.Effective == nil || *u.Effective != 1) {
			t.Fatalf("user-b effective=%v, want default 1", u.Effective)
		}
	}

	// Clear the override — back to inherit; clear the default — unlimited.
	r = doReq(t, env, sess, csrf, http.MethodPut, userLimitsPath,
		`{"ownerRef":"`+me+`","limit":null}`, nil)
	v = decodeBody[userLimitsViewJSON](t, r)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("PUT clear: status=%d", r.StatusCode)
	}
	for _, u := range v.Users {
		if u.OwnerRef == me && (u.Limit != nil || u.Effective == nil || *u.Effective != 1) {
			t.Fatalf("after clear=%+v, want limit=null effective=1 (default)", u)
		}
	}
	r = doReq(t, env, sess, csrf, http.MethodPut, userLimitsPath+"/default", `{"limit":null}`, nil)
	v = decodeBody[userLimitsViewJSON](t, r)
	if r.StatusCode != http.StatusOK || v.Default != nil {
		t.Fatalf("PUT clear default: status=%d default=%v, want null", r.StatusCode, v.Default)
	}
	wantWrites := []string{"set:" + me, "default", "clear:" + me, "default:clear"}
	if len(src.writes) != len(wantWrites) {
		t.Fatalf("writes=%v, want %v", src.writes, wantWrites)
	}
	for i := range wantWrites {
		if src.writes[i] != wantWrites[i] {
			t.Fatalf("writes=%v, want %v", src.writes, wantWrites)
		}
	}
}

// TestAdminUserLimits_Validation: malformed bodies, bad owner refs and
// negative limits are 400s that never reach the store.
func TestAdminUserLimits_Validation(t *testing.T) {
	src := &fakeUserLimitSource{overrides: map[string]int64{}}
	env := newUserLimitsEnv(t, src, nil)
	sess, csrf := loginAdmin(t, env, "admin-a")

	for name, tc := range map[string]struct {
		path, body string
	}{
		"not json":         {userLimitsPath, `nope`},
		"missing ownerRef": {userLimitsPath, `{"limit":3}`},
		"empty ownerRef":   {userLimitsPath, `{"ownerRef":"","limit":3}`},
		"no separator":     {userLimitsPath, `{"ownerRef":"justsub","limit":3}`},
		"empty issuer":     {userLimitsPath, `{"ownerRef":"|sub","limit":3}`},
		"empty subject":    {userLimitsPath, `{"ownerRef":"iss|","limit":3}`},
		"negative limit":   {userLimitsPath, `{"ownerRef":"iss|sub","limit":-1}`},
		"unknown field":    {userLimitsPath, `{"ownerRef":"iss|sub","limit":3,"x":1}`},
		"negative default": {userLimitsPath + "/default", `{"limit":-2}`},
		"fractional":       {userLimitsPath + "/default", `{"limit":1.5}`},
		"trailing doc":     {userLimitsPath + "/default", `{"limit":1} {}`},
	} {
		r := doReq(t, env, sess, csrf, http.MethodPut, tc.path, tc.body, nil)
		e := decodeBody[Error](t, r)
		if r.StatusCode != http.StatusBadRequest || e.Code != CodeInvalidRequest {
			t.Fatalf("%s: status=%d code=%q, want 400 INVALID_REQUEST", name, r.StatusCode, e.Code)
		}
	}
	if len(src.writes) != 0 {
		t.Fatalf("invalid writes reached the store: %v", src.writes)
	}
}

// userLimitAuditEvents returns the dedicated user-limit events a sink
// captured, in emission order.
func userLimitAuditEvents(t *testing.T, buf *bytes.Buffer) []observability.AuditEvent {
	t.Helper()
	var events []observability.AuditEvent
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var e observability.AuditEvent
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("audit line %q: %v", line, err)
		}
		if strings.HasPrefix(e.Action, "admin.user_limit") {
			events = append(events, e)
		}
	}
	return events
}

// TestAdminUserLimits_Audit: every request emits a dedicated audit record
// through the route wrapper — the read and each write, with the resolved
// set/clear action, the pseudonymous actor and target owner, the tenant
// as target.
func TestAdminUserLimits_Audit(t *testing.T) {
	src := &fakeUserLimitSource{overrides: map[string]int64{}}
	buf := &bytes.Buffer{}
	env := newUserLimitsEnv(t, src, observability.NewJSONSink(buf))
	sess, csrf := loginAdmin(t, env, "admin-a")
	me := env.issuer.URL() + "|user-a"

	r := doReq(t, env, sess, csrf, http.MethodGet, userLimitsPath, "", nil)
	r.Body.Close()
	r = doReq(t, env, sess, csrf, http.MethodPut, userLimitsPath,
		`{"ownerRef":"`+me+`","limit":2}`, nil)
	r.Body.Close()
	r = doReq(t, env, sess, csrf, http.MethodPut, userLimitsPath+"/default", `{"limit":3}`, nil)
	r.Body.Close()
	r = doReq(t, env, sess, csrf, http.MethodPut, userLimitsPath,
		`{"ownerRef":"`+me+`","limit":null}`, nil)
	r.Body.Close()

	events := userLimitAuditEvents(t, buf)
	if len(events) != 4 {
		t.Fatalf("audit events=%d, want 4: %v", len(events), events)
	}
	want := []string{
		auditActionAdminUserLimitGet,
		auditActionAdminUserLimitSet,
		auditActionAdminUserLimitDefaultSet,
		auditActionAdminUserLimitClear,
	}
	for i, e := range events {
		if e.Action != want[i] {
			t.Fatalf("event[%d].action=%q, want %q", i, e.Action, want[i])
		}
		if e.Outcome != observability.OutcomeSuccess || e.Tenant != "tenant-a" || e.RequestID == "" {
			t.Fatalf("event[%d]=%+v, want success on tenant-a with request id", i, e)
		}
		if e.TargetUID != "tenant-a" {
			t.Fatalf("event[%d].target=%q, want the tenant", i, e.TargetUID)
		}
		if !strings.HasPrefix(e.Actor, "oidc:") {
			t.Fatalf("event[%d].actor=%q, want pseudonymous ref", i, e.Actor)
		}
		if e.Details["role"] != TenantAdminGroup {
			t.Fatalf("event[%d] lacks role=%q: %+v", i, TenantAdminGroup, e)
		}
	}
	if events[1].Details["owner"] == me {
		t.Fatal("audit must pseudonymize the target owner, not store the raw ref")
	}
	if events[1].Details["max_running"] != "2" || events[2].Details["max_running"] != "3" {
		t.Fatalf("details=%v %v, want max_running recorded", events[1].Details, events[2].Details)
	}
}

// TestAdminUserLimits_AuditDenied: non-admin and cross-tenant attempts
// land in the audit stream as denied events carrying the caller's actor
// and FORBIDDEN — like admin quota, not only the http.request record.
func TestAdminUserLimits_AuditDenied(t *testing.T) {
	src := &fakeUserLimitSource{overrides: map[string]int64{}}
	buf := &bytes.Buffer{}
	env := newUserLimitsEnv(t, src, observability.NewJSONSink(buf))

	// Non-admin PUT: denied under the route's attempt action (the body
	// never decoded, so the set/clear variant does not resolve).
	sess, csrf := login(t, env, "user-a")
	r := doReq(t, env, sess, csrf, http.MethodPut, userLimitsPath,
		`{"ownerRef":"iss|sub","limit":3}`, nil)
	r.Body.Close()
	if r.StatusCode != http.StatusForbidden {
		t.Fatalf("non-admin PUT: status=%d, want 403", r.StatusCode)
	}

	// Tenant admin of tenant-a reading tenant-b: cross-tenant denied.
	sessA, csrfA := loginAdmin(t, env, "admin-a")
	r = doReq(t, env, sessA, csrfA, http.MethodGet,
		"/v1/admin/tenants/tenant-b/user-limits", "", nil)
	r.Body.Close()
	if r.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-tenant GET: status=%d, want 403", r.StatusCode)
	}

	events := userLimitAuditEvents(t, buf)
	if len(events) != 2 {
		t.Fatalf("audit events=%d, want 2: %v", len(events), events)
	}
	if e := events[0]; e.Action != auditActionAdminUserLimitSet ||
		e.Outcome != observability.OutcomeDenied || e.ErrorCode != string(CodeForbidden) ||
		e.TargetUID != "tenant-a" {
		t.Fatalf("non-admin PUT audit=%+v, want denied admin.user_limit.set FORBIDDEN on tenant-a", e)
	}
	if e := events[1]; e.Action != auditActionAdminUserLimitGet ||
		e.Outcome != observability.OutcomeDenied || e.ErrorCode != string(CodeForbidden) ||
		e.TargetUID != "tenant-b" {
		t.Fatalf("cross-tenant GET audit=%+v, want denied admin.user_limit.get FORBIDDEN on tenant-b", e)
	}
	for _, e := range events {
		if !strings.HasPrefix(e.Actor, "oidc:") {
			t.Fatalf("denied event lacks the caller actor: %+v", e)
		}
	}
}

// TestAdminUserLimits_StoreErrors: store-level failures map to honest
// codes — a transport outage is retryable 503 UNAVAILABLE, a permanent
// failure 500 INTERNAL — and the audited event carries the real code.
func TestAdminUserLimits_StoreErrors(t *testing.T) {
	src := &fakeUserLimitSource{overrides: map[string]int64{}}
	buf := &bytes.Buffer{}
	env := newUserLimitsEnv(t, src, observability.NewJSONSink(buf))
	sess, csrf := loginAdmin(t, env, "admin-a")
	me := env.issuer.URL() + "|user-a"

	src.setErr = fmt.Errorf("dial postgres: %w", context.DeadlineExceeded)
	r := doReq(t, env, sess, csrf, http.MethodPut, userLimitsPath,
		`{"ownerRef":"`+me+`","limit":2}`, nil)
	e := decodeBody[Error](t, r)
	r.Body.Close()
	if r.StatusCode != http.StatusServiceUnavailable || e.Code != CodeUnavailable {
		t.Fatalf("transient store error: status=%d code=%q, want 503 UNAVAILABLE", r.StatusCode, e.Code)
	}

	src.setErr = errors.New("pg: check constraint violated")
	r = doReq(t, env, sess, csrf, http.MethodPut, userLimitsPath+"/default", `{"limit":3}`, nil)
	e = decodeBody[Error](t, r)
	r.Body.Close()
	if r.StatusCode != http.StatusInternalServerError || e.Code != CodeInternal {
		t.Fatalf("permanent store error: status=%d code=%q, want 500 INTERNAL", r.StatusCode, e.Code)
	}

	events := userLimitAuditEvents(t, buf)
	if len(events) != 2 {
		t.Fatalf("audit events=%d, want 2: %v", len(events), events)
	}
	if events[0].ErrorCode != string(CodeUnavailable) || events[0].Outcome != observability.OutcomeFailure {
		t.Fatalf("transient audit=%+v, want failure/UNAVAILABLE", events[0])
	}
	if events[1].ErrorCode != string(CodeInternal) || events[1].Outcome != observability.OutcomeFailure {
		t.Fatalf("permanent audit=%+v, want failure/INTERNAL", events[1])
	}
}

// TestUserLimitReachedMapping: a per-principal refusal maps to 409
// QUOTA_EXHAUSTED + details.reason UserLimitReached + params limit/current
// — retryable only when teardown-pending holds cover the shortfall.
func TestUserLimitReachedMapping(t *testing.T) {
	be := newFakeBackend()
	env := newWorkspaceEnv(t, be, defaultCatalog(), defaultTenants(), func(h *WorkspaceHandler) {
		h.WithReleaseRetryAfter(func() int { return 7 })
	})
	sess, csrf := login(t, env, "user-a")
	body := `{"name":"x","templateRef":"tpl_linuxdesktop"}`

	// Hard refusal: not retryable, params carry the limit and current.
	be.createErr = &provisioning.UserLimitError{
		TenantID: "tenant-a", Owner: "iss|user-a", Limit: 1, Current: 1, Requested: 1,
	}
	r := doReq(t, env, sess, csrf, http.MethodPost, "/v1/workspaces", body,
		map[string]string{"Idempotency-Key": "key-ul-00001"})
	if r.StatusCode != http.StatusConflict {
		t.Fatalf("user-limit create: status=%d, want 409", r.StatusCode)
	}
	if got := r.Header.Get("Retry-After"); got != "" {
		t.Fatalf("hard refusal must not set Retry-After, got %q", got)
	}
	e := errBody(t, r)
	if e.Code != CodeQuotaExhausted || e.Retryable {
		t.Fatalf("body=%+v, want non-retryable QUOTA_EXHAUSTED", e)
	}
	if e.Details == nil || e.Details.Reason != ReasonUserLimitReached {
		t.Fatalf("details=%+v, want reason %q", e.Details, ReasonUserLimitReached)
	}
	if e.Details.Params[ParamKeyLimit] != "1" || e.Details.Params[ParamKeyCurrent] != "1" {
		t.Fatalf("params=%v, want limit=1 current=1", e.Details.Params)
	}

	// Teardown-pending variant: same code and reason, retryable with a
	// Retry-After estimate.
	be.createErr = &provisioning.UserLimitError{
		TenantID: "tenant-a", Owner: "iss|user-a", Limit: 1, Current: 1, Requested: 1,
		ReleasePending: true,
	}
	r = doReq(t, env, sess, csrf, http.MethodPost, "/v1/workspaces", body,
		map[string]string{"Idempotency-Key": "key-ul-00002"})
	if r.StatusCode != http.StatusConflict || r.Header.Get("Retry-After") != "7" {
		t.Fatalf("pending refusal: status=%d Retry-After=%q", r.StatusCode, r.Header.Get("Retry-After"))
	}
	e = errBody(t, r)
	if e.Code != CodeQuotaExhausted || !e.Retryable ||
		e.Details == nil || e.Details.Reason != ReasonUserLimitReached {
		t.Fatalf("pending body=%+v, want retryable QUOTA_EXHAUSTED UserLimitReached", e)
	}
}

// TestQuota_UserLimitFields: GET /v1/quota reports the caller's effective
// limit in userLimits and each owner's in users[].limit.
func TestQuota_UserLimitFields(t *testing.T) {
	src := &fakeQuotaSource{}
	env := newQuotaEnv(t, src, newFakeDirectory(), defaultTenants())
	rep := quotaFixture(env.issuer.URL())
	def := int64(4)
	rep.DefaultUserLimit = &def
	rep.UserLimits = map[string]int64{env.issuer.URL() + "|user-a": 2}
	src.report = rep
	sess, csrf := login(t, env, "user-a")

	r := doReq(t, env, sess, csrf, http.MethodGet, "/v1/quota", "", nil)
	var v struct {
		Tenant     string `json:"tenant"`
		UserLimits *struct {
			RunningWorkspaces int64 `json:"runningWorkspaces"`
		} `json:"userLimits"`
		Users []struct {
			Subject string `json:"subject"`
			Limit   *int64 `json:"limit"`
		} `json:"users"`
	}
	defer r.Body.Close()
	if err := json.NewDecoder(r.Body).Decode(&v); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if v.UserLimits == nil || v.UserLimits.RunningWorkspaces != 2 {
		t.Fatalf("userLimits=%+v, want runningWorkspaces=2 (caller override)", v.UserLimits)
	}
	if len(v.Users) != 1 || v.Users[0].Subject != "user-a" || v.Users[0].Limit == nil || *v.Users[0].Limit != 2 {
		t.Fatalf("users=%+v, want only caller with limit 2", v.Users)
	}
}
