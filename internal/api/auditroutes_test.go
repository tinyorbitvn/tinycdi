// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	"github.com/tinyorbitvn/tinycdi/internal/observability"
	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
	"github.com/tinyorbitvn/tinycdi/internal/sessionhost"
	"github.com/tinyorbitvn/tinycdi/internal/store"
)

// captureSink is an AuditSink that stores every emitted event verbatim and
// the JSONL form for leak checks (the production JSONSink applies the same
// RedactDetails on write; redaction itself is covered in observability).
type captureSink struct {
	mu     sync.Mutex
	events []observability.AuditEvent
	buf    bytes.Buffer
}

func (c *captureSink) WriteAudit(_ context.Context, e observability.AuditEvent) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, e)
	return json.NewEncoder(&c.buf).Encode(e)
}

func (c *captureSink) get() []observability.AuditEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]observability.AuditEvent(nil), c.events...)
}

func (c *captureSink) raw() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

// requireEvent returns the single captured event matching action (the test
// asserts exactly one exists), checking the fields every audited route must
// fill: actor, action, outcome, request id.
func requireEvent(t *testing.T, sink *captureSink, action string) observability.AuditEvent {
	t.Helper()
	var found []observability.AuditEvent
	for _, e := range sink.get() {
		if e.Action == action {
			found = append(found, e)
		}
	}
	if len(found) != 1 {
		t.Fatalf("audit events for %q = %d, want exactly 1 (all: %+v)", action, len(found), sink.get())
	}
	e := found[0]
	if e.Actor == "" || e.RequestID == "" || e.Outcome == "" {
		t.Fatalf("%s event missing required fields: %+v", action, e)
	}
	if e.Outcome != observability.OutcomeSuccess &&
		e.Outcome != observability.OutcomeFailure &&
		e.Outcome != observability.OutcomeDenied {
		t.Fatalf("%s outcome = %q, want a bounded outcome", action, e.Outcome)
	}
	return e
}

// specOps returns the operation set of openapi.yaml as "METHOD /path" keys
// (lower-case method), ignoring non-operation entries like "parameters".
func specOps(t *testing.T) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile("openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Paths map[string]map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("parse openapi.yaml: %v", err)
	}
	out := map[string]bool{}
	for path, ops := range spec.Paths {
		for method := range ops {
			switch method {
			case "get", "put", "post", "delete", "patch", "head", "options", "trace":
				out[strings.ToUpper(method)+" "+normalizePath(path)] = true
			}
		}
	}
	return out
}

// normalizePath erases path-template variable names so the spec's
// {workspaceId} and the mux's {id} compare as the same route.
func normalizePath(path string) string {
	segs := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for i, seg := range segs {
		if strings.HasPrefix(seg, "{") {
			segs[i] = "{}"
		}
	}
	return "/" + strings.Join(segs, "/")
}

// normPattern applies normalizePath to the path half of a "METHOD /path"
// route pattern.
func normPattern(pattern string) string {
	space := strings.IndexByte(pattern, ' ')
	return pattern[:space+1] + normalizePath(pattern[space+1:])
}

// TestAuditCoverage_SpecMatchesTable is the guard rail for audit
// completeness: every mutating operation and every /v1/admin/ operation in
// the published contract must have a dedicated audit action in
// auditedRoutes — adding an admin route to the spec without one fails here
// — and every table entry must name a real spec operation.
func TestAuditCoverage_SpecMatchesTable(t *testing.T) {
	ops := specOps(t)
	covered := map[string]bool{}
	for pattern := range auditedRoutes {
		covered[normPattern(pattern)] = true
	}
	for op := range ops {
		space := strings.IndexByte(op, ' ')
		method, path := op[:space], op[space+1:]
		if method == http.MethodGet && !strings.HasPrefix(path, "/v1/admin/") {
			continue
		}
		if !covered[op] {
			t.Errorf("spec operation %s has no audit action in auditedRoutes", op)
		}
	}
	for pattern := range auditedRoutes {
		if !ops[normPattern(pattern)] {
			t.Errorf("auditedRoutes entry %q names no spec operation", pattern)
		}
	}
}

// coverageEnv builds ONE env whose mux carries every app-listener mount —
// the same Mount*/mux.Handle calls backend.appMux performs
// (internal/backend/wire.go) — on the in-memory fakes the per-route tests
// use, with the capturing sink attached to the authenticator and every
// audited handler. Keep the mount block in sync with appMux: a route
// mounted there but not replayed here answers 404 and produces no domain
// event, which the spec-set assertion below turns into a loud failure.
type coverageFixtures struct {
	env      *testEnv
	backend  *fakeWorkspaceBackend
	retained *fakeRetainedStore
}

func newCoverageEnv(t *testing.T, sink *captureSink) *coverageFixtures {
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
	a.WithAuditSink(sink).WithPrincipalRevoker(&fakePrincipalRevoker{res: RevokeAllResult{Sessions: 1}})

	tenants := defaultTenants()
	cat := defaultCatalog()
	be := newFakeBackend()
	fs := newFakeRetainedStore()
	d, err := sessionhost.ParseDomain("session.example.test")
	if err != nil {
		t.Fatalf("sessionhost.ParseDomain: %v", err)
	}
	connH := NewConnectionHandler(&fakeIssuer{ticket: IssuedTicket{
		WorkspaceID: "ws_0000000000000000000000000c",
		Token:       "tkt_coveragereplay0000000000000",
		ExpiresAt:   time.Now().Add(time.Minute).UTC(),
	}}, tenants, d).WithLogger(logger).WithAuditSink(sink)

	mux := http.NewServeMux()
	// env.login drives the /auth/* flow; everything below mirrors the
	// appMux mount block — keep in sync with it.
	mux.Handle("/auth/login", http.HandlerFunc(a.LoginHandler))
	mux.Handle("/auth/callback", http.HandlerFunc(a.CallbackHandler))
	noop := func(h http.Handler) http.Handler { return h }
	mux.Handle("GET /v1/login", noop(http.HandlerFunc(a.LoginHandler)))
	mux.Handle("GET /v1/auth/callback", noop(http.HandlerFunc(a.CallbackHandler)))
	MountLogoutRoute(mux, a)
	MountRevokeAllRoute(mux, a, noop)
	MountSessionTouchRoute(mux, a, noop)
	MountSessionProbeRoute(mux, a, noop)
	MountMeRoutes(mux, a, NewMeHandler(testSessionDomain))
	MountWorkspaceRoutes(mux, a, NewWorkspaceHandler(be, cat, tenants).WithAuditSink(sink), NewTemplateHandler(cat, tenants))
	MountConnectionRoutes(mux, a, connH)
	MountConnectionStatusRoutes(mux, a, NewConnectionStatusHandler(&fakeStater{}, &fakeWorkspaceGet{}, tenants))
	MountDataRoutes(mux, a, NewDataHandler(fs, cat, tenants).WithAuditSink(sink))
	MountQuotaRoutes(mux, a, NewQuotaHandler(&fakeQuotaSource{}, newFakeDirectory(), tenants))
	MountAdminQuotaRoutes(mux, a, NewAdminQuotaHandler(&fakeAdminQuotaSource{}, newFakeDirectory(), tenants, nil).WithAuditSink(sink))
	MountAdminUserLimitRoutes(mux, a, NewAdminUserLimitsHandler(&fakeUserLimitSource{overrides: map[string]int64{}}, newFakeDirectory(), tenants).WithAuditSink(sink))

	// Production wrap order (wire.wrapApp): RequestID → request audit →
	// trusted-origin gate → mux. The replay requests carry no Origin or
	// Sec-Fetch-Site, so they pass the gate as non-browser clients.
	srv := httptest.NewServer(RequestID(AuditWithSink(logger, sink)(
		RequireTrustedOrigin(a.SessionCookieName(), nil)(mux))))
	env := &testEnv{issuer: iss, auth: a, store: sessions, server: srv, logs: logBuf}
	t.Cleanup(func() { srv.Close(); iss.Close() })
	return &coverageFixtures{env: env, backend: be, retained: fs}
}

// requiredMutatingOps is the coverage domain: every openapi operation a
// domain audit event must exist for — every non-GET plus every
// /v1/admin/ operation, the same predicate TestAuditCoverage_SpecMatchesTable
// applies.
func requiredMutatingOps(t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for op := range specOps(t) {
		space := strings.IndexByte(op, ' ')
		method, path := op[:space], op[space+1:]
		if method == http.MethodGet && !strings.HasPrefix(path, "/v1/admin/") {
			continue
		}
		out[op] = true
	}
	return out
}

// TestAuditCoverage_MountedMuxEmitsEventPerRoute is the runtime pin for
// audit completeness: it replays the mounted mux — every route mount the
// app listener performs, wired the way wire.go wires them — with a
// capturing audit sink, then drives ONE request through every mutating
// and /v1/admin/ spec route and requires EXACTLY one domain audit event
// carrying the route's table action. The spec↔table pin proves a route
// NAMES an action; this proves the mounted handler EMITS it — a route
// mounted without audited() (or audited() invoked and discarded)
// produces no event and fails here. The driven set is asserted equal to
// the spec's mutating set, so a route can be neither unaudited nor
// unlisted.
func TestAuditCoverage_MountedMuxEmitsEventPerRoute(t *testing.T) {
	sink := &captureSink{}
	fx := newCoverageEnv(t, sink)
	env := fx.env
	sess, csrf := login(t, env, "user-a")

	const wsID = "ws_0000000000000000000000000c"
	fx.backend.recs[wsID] = provisioning.WorkspaceRecord{
		ID: wsID, TenantID: "tenant-a", Owner: envOwner(env, "user-a"),
		Phase: "Ready", DesiredState: "Running",
	}
	const wsStoppedID = "ws_0000000000000000000000000d"
	fx.backend.recs[wsStoppedID] = provisioning.WorkspaceRecord{
		ID: wsStoppedID, TenantID: "tenant-a", Owner: envOwner(env, "user-a"),
		Phase: "Stopped", DesiredState: "Stopped",
	}
	rec := seedRetained(fx.retained, "rd_cov00000001", "tenant-a", envOwner(env, "user-a"), "LinuxContainer")
	rec2 := seedRetained(fx.retained, "rd_cov00000002", "tenant-a", envOwner(env, "user-a"), "LinuxContainer")
	sessA, csrfA := loginAdmin(t, env, "admin-a")

	// One driver per required spec op, keyed by the normalized
	// "METHOD /path/{}" form specOps produces. Session-destroying routes
	// run last; the revoke-all gets a fresh login it may consume.
	routes := []struct {
		op     string
		action string
		run    func()
	}{
		{"POST /v1/session:touch", auditActionSessionTouch, func() {
			doReq(t, env, sess, csrf, http.MethodPost, "/v1/session:touch", "", nil).Body.Close()
		}},
		{"POST /v1/workspaces", auditActionWorkspaceCreate, func() {
			doReq(t, env, sess, csrf, http.MethodPost, "/v1/workspaces",
				`{"name":"coverage-ws","templateRef":"tpl_linuxdesktop"}`,
				map[string]string{"Idempotency-Key": "key-cov-create"}).Body.Close()
		}},
		{"POST /v1/workspaces/{}/start", auditActionWorkspaceStart, func() {
			doReq(t, env, sess, csrf, http.MethodPost, "/v1/workspaces/"+wsStoppedID+"/start", "", nil).Body.Close()
		}},
		{"POST /v1/workspaces/{}/stop", auditActionWorkspaceStop, func() {
			doReq(t, env, sess, csrf, http.MethodPost, "/v1/workspaces/"+wsID+"/stop", "", nil).Body.Close()
		}},
		{"POST /v1/workspaces/{}/connections", auditActionConnectionCreate, func() {
			doReq(t, env, sess, csrf, http.MethodPost,
				"/v1/workspaces/"+wsID+"/connections", `{"takeover":true}`, nil).Body.Close()
		}},
		{"DELETE /v1/workspaces/{}", auditActionWorkspaceDelete, func() {
			doReq(t, env, sess, csrf, http.MethodDelete, "/v1/workspaces/"+wsID, "", nil).Body.Close()
		}},
		{"POST /v1/data/{}/attach", auditActionDataAttach, func() {
			doReq(t, env, sess, csrf, http.MethodPost, "/v1/data/"+rec.ID+"/attach",
				`{"name":"restored","templateRef":"tpl_linuxdesktop"}`,
				map[string]string{"Idempotency-Key": "key-cov-attach"}).Body.Close()
		}},
		{"POST /v1/data/{}/purge", auditActionDataPurge, func() {
			got := decodeBody[retainedDataView](t,
				doReq(t, env, sess, csrf, http.MethodGet, "/v1/data/"+rec2.ID, "", nil))
			doReq(t, env, sess, csrf, http.MethodPost, "/v1/data/"+rec2.ID+"/purge",
				`{"confirmationNonce":"`+got.PurgeConfirmationNonce+`"}`, nil).Body.Close()
		}},
		{"GET /v1/admin/tenants/{}/quota", auditActionAdminQuotaGet, func() {
			doReq(t, env, sessA, csrfA, http.MethodGet, adminQuotaPath, "", nil).Body.Close()
		}},
		{"PUT /v1/admin/tenants/{}/quota", auditActionAdminQuotaSet, func() {
			doReq(t, env, sessA, csrfA, http.MethodPut, adminQuotaPath, adminQuotaBody,
				map[string]string{"If-Match": IfMatchCreate}).Body.Close()
		}},
		{"GET /v1/admin/tenants/{}/user-limits", auditActionAdminUserLimitGet, func() {
			doReq(t, env, sessA, csrfA, http.MethodGet, "/v1/admin/tenants/tenant-a/user-limits", "", nil).Body.Close()
		}},
		{"PUT /v1/admin/tenants/{}/user-limits", auditActionAdminUserLimitSet, func() {
			doReq(t, env, sessA, csrfA, http.MethodPut, "/v1/admin/tenants/tenant-a/user-limits",
				`{"ownerRef":"iss|sub","limit":3}`, nil).Body.Close()
		}},
		{"PUT /v1/admin/tenants/{}/user-limits/default", auditActionAdminUserLimitDefaultSet, func() {
			doReq(t, env, sessA, csrfA, http.MethodPut, "/v1/admin/tenants/tenant-a/user-limits/default",
				`{"limit":3}`, nil).Body.Close()
		}},
		{"POST /v1/logout", auditActionSessionLogout, func() {
			doReq(t, env, sess, csrf, http.MethodPost, "/v1/logout", "", nil).Body.Close()
		}},
		{"POST /v1/me/sessions:revoke-all", auditActionSessionRevokeAll, func() {
			s2, c2 := login(t, env, "user-b") // the revoke consumes this login
			doReq(t, env, s2, c2, http.MethodPost, "/v1/me/sessions:revoke-all", "", nil).Body.Close()
		}},
	}

	required := requiredMutatingOps(t)
	driven := map[string]bool{}
	for _, rt := range routes {
		if !required[rt.op] {
			t.Errorf("replay driver %s is not a mutating/admin spec operation", rt.op)
			continue
		}
		driven[rt.op] = true
		rt.run()
		requireEvent(t, sink, rt.action)
	}
	for op := range required {
		if !driven[op] {
			t.Errorf("spec operation %s has no replay driver — add one or the route is unaudited", op)
		}
	}
	// No extra domain events may fire anywhere else in the replay: every
	// non-"http.request" event must be one of the driven table actions.
	var extra []observability.AuditEvent
	domainActions := map[string]bool{}
	for pattern, rt := range auditedRoutes {
		domainActions[rt.action] = true
		_ = pattern
	}
	for _, e := range sink.get() {
		if e.Action != "http.request" && !domainActions[e.Action] {
			extra = append(extra, e)
		}
	}
	if len(extra) != 0 {
		t.Errorf("unexpected domain audit events: %+v", extra)
	}
}

// TestAudit_AdminQuotaSet: PUT /v1/admin/tenants/{tenant}/quota emits
// admin.quota.set with the verified admin actor, the tenant as target, the
// request id and the attempted limits in details.
func TestAudit_AdminQuotaSet(t *testing.T) {
	sink := &captureSink{}
	src := &fakeAdminQuotaSource{}
	env := newAdminQuotaEnv(t, src, nil,
		func(h *AdminQuotaHandler) { h.WithAuditSink(sink) })
	sess, csrf := loginAdmin(t, env, "admin-a")

	r := doReq(t, env, sess, csrf, http.MethodPut, adminQuotaPath, adminQuotaBody,
		map[string]string{"If-Match": IfMatchCreate, "X-Request-Id": "req-quota-1"})
	r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200", r.StatusCode)
	}
	e := requireEvent(t, sink, auditActionAdminQuotaSet)
	if e.Outcome != observability.OutcomeSuccess || e.ErrorCode != "" {
		t.Fatalf("success PUT: outcome=%q err=%q", e.Outcome, e.ErrorCode)
	}
	if !strings.HasPrefix(e.Actor, "oidc:") || e.Tenant != "tenant-a" || e.TargetUID != "tenant-a" {
		t.Fatalf("actor/target wrong: %+v", e)
	}
	if e.RequestID != "req-quota-1" {
		t.Fatalf("requestId=%q, want the propagated req-quota-1", e.RequestID)
	}
	for k, want := range map[string]string{
		"running_workspaces": "4", "cpu_millicores": "8000",
		"memory_mib": "16384", "storage_gib": "200",
	} {
		if e.Details[k] != want {
			t.Fatalf("details[%q]=%q, want %q: %+v", k, e.Details[k], want, e.Details)
		}
	}
}

// TestAudit_AdminQuotaDenied: a non-admin PUT is a denied event carrying the
// caller's actor ref and the FORBIDDEN code — attempted privilege use is
// auditable. A config-managed refusal records failure + the stable code.
func TestAudit_AdminQuotaDenied(t *testing.T) {
	sink := &captureSink{}
	src := &fakeAdminQuotaSource{hasRow: true, limits: store.QuotaAmounts{RunningSlots: 4}}
	env := newAdminQuotaEnv(t, src, map[string]bool{"tenant-a": true},
		func(h *AdminQuotaHandler) { h.WithAuditSink(sink) })

	// Regular user (no tenant-admin group): denied.
	sess, csrf := login(t, env, "user-a")
	r := doReq(t, env, sess, csrf, http.MethodPut, adminQuotaPath, adminQuotaBody, nil)
	r.Body.Close()
	if r.StatusCode != http.StatusForbidden {
		t.Fatalf("status=%d, want 403", r.StatusCode)
	}
	e := requireEvent(t, sink, auditActionAdminQuotaSet)
	if e.Outcome != observability.OutcomeDenied || e.ErrorCode != string(CodeForbidden) {
		t.Fatalf("non-admin PUT: outcome=%q err=%q, want denied/FORBIDDEN", e.Outcome, e.ErrorCode)
	}
	if !strings.HasPrefix(e.Actor, "oidc:") {
		t.Fatalf("denied event lacks the caller actor: %+v", e)
	}

	// Tenant admin on a config-managed tenant: refused, outcome failure.
	sessA, csrfA := loginAdmin(t, env, "admin-a")
	r = doReq(t, env, sessA, csrfA, http.MethodPut, adminQuotaPath, adminQuotaBody, nil)
	r.Body.Close()
	if r.StatusCode != http.StatusConflict {
		t.Fatalf("managed PUT: status=%d, want 409", r.StatusCode)
	}
	es := sink.get()
	var managed *observability.AuditEvent
	for i := range es {
		if es[i].Action == auditActionAdminQuotaSet && es[i].ErrorCode == string(CodeQuotaManagedByConfig) {
			managed = &es[i]
		}
	}
	if managed == nil || managed.Outcome != observability.OutcomeFailure {
		t.Fatalf("config-managed PUT audit missing: %+v", es)
	}
}

// TestAudit_AdminQuotaGet: the admin read is covered too — every /v1/admin/
// request leaves an event.
func TestAudit_AdminQuotaGet(t *testing.T) {
	sink := &captureSink{}
	env := newAdminQuotaEnv(t, &fakeAdminQuotaSource{}, nil,
		func(h *AdminQuotaHandler) { h.WithAuditSink(sink) })
	sess, csrf := loginAdmin(t, env, "admin-a")

	r := doReq(t, env, sess, csrf, http.MethodGet, adminQuotaPath, "", nil)
	r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200", r.StatusCode)
	}
	e := requireEvent(t, sink, auditActionAdminQuotaGet)
	if e.Outcome != observability.OutcomeSuccess || e.TargetUID != "tenant-a" {
		t.Fatalf("admin GET audit wrong: %+v", e)
	}
}

// TestAudit_WorkspaceAdminDelete: a tenant admin deleting another user's
// workspace emits workspace.delete marked with the elevated role.
func TestAudit_WorkspaceAdminDelete(t *testing.T) {
	sink := &captureSink{}
	be := newFakeBackend()
	env := newWorkspaceEnv(t, be, defaultCatalog(), defaultTenants(),
		func(h *WorkspaceHandler) { h.WithAuditSink(sink) })
	wsID := "ws_00000000000000000000000001"
	be.recs[wsID] = provisioning.WorkspaceRecord{
		ID: wsID, TenantID: "tenant-a", Owner: envOwner(env, "user-a"),
		Phase: "Ready", DesiredState: "Running",
	}
	env.issuer.Groups = []string{TenantAdminGroup}
	sess, csrf := login(t, env, "admin-1")

	r := doReq(t, env, sess, csrf, http.MethodDelete, "/v1/workspaces/"+wsID, "", nil)
	r.Body.Close()
	if r.StatusCode != http.StatusAccepted {
		t.Fatalf("admin delete: status=%d, want 202", r.StatusCode)
	}
	e := requireEvent(t, sink, auditActionWorkspaceDelete)
	if e.Outcome != observability.OutcomeSuccess || e.TargetUID != wsID {
		t.Fatalf("admin delete audit wrong: %+v", e)
	}
	if e.Details["role"] != TenantAdminGroup {
		t.Fatalf("admin use of a shared route must mark role=%q: %+v", TenantAdminGroup, e.Details)
	}
}

// TestAudit_WorkspaceStopOwner: an owner's own stop emits workspace.stop
// without the admin role marker; a foreign stop answers 404 → a failure
// event carrying NOT_FOUND.
func TestAudit_WorkspaceStopOwner(t *testing.T) {
	sink := &captureSink{}
	be := newFakeBackend()
	env := newWorkspaceEnv(t, be, defaultCatalog(), defaultTenants(),
		func(h *WorkspaceHandler) { h.WithAuditSink(sink) })
	wsID := "ws_00000000000000000000000002"
	be.recs[wsID] = provisioning.WorkspaceRecord{
		ID: wsID, TenantID: "tenant-a", Owner: envOwner(env, "user-a"),
		Phase: "Ready", DesiredState: "Running",
	}
	sess, csrf := login(t, env, "user-a")

	r := doReq(t, env, sess, csrf, http.MethodPost, "/v1/workspaces/"+wsID+"/stop", "", nil)
	r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("stop: status=%d, want 200", r.StatusCode)
	}
	e := requireEvent(t, sink, auditActionWorkspaceStop)
	if e.Outcome != observability.OutcomeSuccess || e.TargetUID != wsID || e.Details["role"] != "" {
		t.Fatalf("owner stop audit wrong: %+v", e)
	}

	// A workspace that does not exist for this caller: failure + code.
	r = doReq(t, env, sess, csrf, http.MethodPost, "/v1/workspaces/ws_00000000000000000000000099/stop", "", nil)
	r.Body.Close()
	if r.StatusCode != http.StatusNotFound {
		t.Fatalf("foreign stop: status=%d, want 404", r.StatusCode)
	}
	es := sink.get()
	var failed *observability.AuditEvent
	for i := range es {
		if es[i].Action == auditActionWorkspaceStop && es[i].Outcome == observability.OutcomeFailure {
			failed = &es[i]
			break
		}
	}
	if failed == nil || failed.ErrorCode != string(CodeNotFound) {
		t.Fatalf("failed stop audit missing: %+v", sink.get())
	}
}

// TestAudit_WorkspaceCreate: create emits workspace.create whose target is
// the new workspace id the handler recorded.
func TestAudit_WorkspaceCreate(t *testing.T) {
	sink := &captureSink{}
	be := newFakeBackend()
	env := newWorkspaceEnv(t, be, defaultCatalog(), defaultTenants(),
		func(h *WorkspaceHandler) { h.WithAuditSink(sink) })
	sess, csrf := login(t, env, "user-a")

	r := doReq(t, env, sess, csrf, http.MethodPost, "/v1/workspaces",
		`{"name":"audit-ws","templateRef":"tpl_linuxdesktop"}`,
		map[string]string{"Idempotency-Key": "key-audit-create"})
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("create: status=%d", r.StatusCode)
	}
	v := decodeBody[WorkspaceView](t, r)
	e := requireEvent(t, sink, auditActionWorkspaceCreate)
	if e.Outcome != observability.OutcomeSuccess || e.TargetUID != v.ID || v.ID == "" {
		t.Fatalf("create audit target=%q, want the new workspace id %q", e.TargetUID, v.ID)
	}
}

// TestAudit_CSRFDenied: a state-changing request whose CSRF token is wrong
// is denied inside the audited wrapper — the refusal lands in the audit
// stream with CSRF_FAILED, not just in the request log.
func TestAudit_CSRFDenied(t *testing.T) {
	sink := &captureSink{}
	be := newFakeBackend()
	env := newWorkspaceEnv(t, be, defaultCatalog(), defaultTenants(),
		func(h *WorkspaceHandler) { h.WithAuditSink(sink) })
	sess, _ := login(t, env, "user-a")

	r := doReq(t, env, sess, &http.Cookie{Name: "csrf", Value: "forged"},
		http.MethodPost, "/v1/workspaces/ws_00000000000000000000000002/stop", "", nil)
	r.Body.Close()
	if r.StatusCode != http.StatusForbidden {
		t.Fatalf("bad CSRF: status=%d, want 403", r.StatusCode)
	}
	e := requireEvent(t, sink, auditActionWorkspaceStop)
	if e.Outcome != observability.OutcomeDenied || e.ErrorCode != string(CodeCSRFFailed) {
		t.Fatalf("CSRF refusal audit: outcome=%q err=%q, want denied/CSRF_FAILED", e.Outcome, e.ErrorCode)
	}
}

// TestAudit_DataAttachPurge: the retained-data mutations emit data.attach /
// data.purge with the record id as target; an admin attach marks the role
// and records the consuming workspace id.
func TestAudit_DataAttachPurge(t *testing.T) {
	sink := &captureSink{}
	fs := newFakeRetainedStore()
	env := newDataEnv(t, fs,
		func(h *DataHandler) { h.WithAuditSink(sink) })
	rec := seedRetained(fs, "rd_audit000001", "tenant-a", envOwner(env, "user-a"), "LinuxContainer")

	// Owner attach.
	sess, csrf := login(t, env, "user-a")
	r := doDataReq(t, env, sess, csrf, http.MethodPost, "/v1/data/"+rec.ID+"/attach",
		`{"name":"restored","templateRef":"tpl_linuxdesktop"}`,
		map[string]string{"Idempotency-Key": "key-audit-attach"})
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("attach: status=%d", r.StatusCode)
	}
	ws := decodeBody[WorkspaceView](t, r)
	e := requireEvent(t, sink, auditActionDataAttach)
	if e.Outcome != observability.OutcomeSuccess || e.TargetUID != rec.ID ||
		e.Details["workspace"] != ws.ID {
		t.Fatalf("attach audit wrong: %+v", e)
	}

	// Admin purge of another user's record.
	fs2 := newFakeRetainedStore()
	env2 := newDataEnv(t, fs2,
		func(h *DataHandler) { h.WithAuditSink(sink) })
	rec2 := seedRetained(fs2, "rd_audit000002", "tenant-a", envOwner(env2, "user-a"), "LinuxContainer")
	env2.issuer.Groups = []string{TenantAdminGroup}
	sessAdm, csrfAdm := login(t, env2, "admin-1")
	// Read first to mint the caller-bound purge nonce.
	r = doDataReq(t, env2, sessAdm, csrfAdm, http.MethodGet, "/v1/data/"+rec2.ID, "", nil)
	got := decodeBody[retainedDataView](t, r)
	r = doDataReq(t, env2, sessAdm, csrfAdm, http.MethodPost, "/v1/data/"+rec2.ID+"/purge",
		`{"confirmationNonce":"`+got.PurgeConfirmationNonce+`"}`, nil)
	r.Body.Close()
	if r.StatusCode != http.StatusAccepted {
		t.Fatalf("admin purge: status=%d", r.StatusCode)
	}
	e = requireEvent(t, sink, auditActionDataPurge)
	if e.Outcome != observability.OutcomeSuccess || e.TargetUID != rec2.ID ||
		e.Details["role"] != TenantAdminGroup {
		t.Fatalf("admin purge audit wrong: %+v", e)
	}
}

// TestAudit_ConnectionCreate: minting a launch ticket emits
// connection.create with the workspace target — and the response's
// bearer-equivalent ticket value is nowhere in the audit output.
func TestAudit_ConnectionCreate(t *testing.T) {
	sink := &captureSink{}
	fi := &fakeIssuer{ticket: IssuedTicket{
		WorkspaceID: "ws_00000000000000000000000001",
		Token:       "tkt_secretvalue_mustneverleak_0123456789",
		ExpiresAt:   time.Now().Add(60 * time.Second).UTC(),
	}}
	env := newConnectionEnv(t, fi,
		func(h *ConnectionHandler) { h.WithAuditSink(sink) })
	sess, csrf := login(t, env, "alice")

	r := doReq(t, env, sess, csrf, http.MethodPost,
		"/v1/workspaces/ws_00000000000000000000000001/connections",
		`{"takeover":true}`, nil)
	r.Body.Close()
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("connections: status=%d, want 201", r.StatusCode)
	}
	e := requireEvent(t, sink, auditActionConnectionCreate)
	if e.Outcome != observability.OutcomeSuccess ||
		e.TargetUID != "ws_00000000000000000000000001" ||
		e.Details["takeover"] != "true" {
		t.Fatalf("connection audit wrong: %+v", e)
	}
	if strings.Contains(sink.raw(), fi.ticket.Token) {
		t.Fatal("audit output contains the launch ticket — tickets are bearer credentials")
	}
}

// TestAudit_Logout: sign-out emits session.logout through the
// authenticator's sink alongside the dedicated session.revoke record.
func TestAudit_Logout(t *testing.T) {
	sink := &captureSink{}
	iss, err := oidctest.NewIssuer()
	if err != nil {
		t.Fatalf("oidctest.NewIssuer: %v", err)
	}
	logger := slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil))
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
	a.WithAuditSink(sink)
	mux := http.NewServeMux()
	mux.Handle("/auth/login", http.HandlerFunc(a.LoginHandler))
	mux.Handle("/auth/callback", http.HandlerFunc(a.CallbackHandler))
	MountLogoutRoute(mux, a)
	srv := httptest.NewServer(RequestID(Audit(logger)(mux)))
	env := &testEnv{issuer: iss, auth: a, store: sessions, server: srv, logs: &bytes.Buffer{}}
	t.Cleanup(func() { srv.Close(); iss.Close() })

	sess, csrf := login(t, env, "user-a")
	r := doReq(t, env, sess, csrf, http.MethodPost, "/v1/logout", "", nil)
	r.Body.Close()
	if r.StatusCode != http.StatusNoContent && r.StatusCode != http.StatusOK {
		t.Fatalf("logout: status=%d", r.StatusCode)
	}
	e := requireEvent(t, sink, auditActionSessionLogout)
	if e.Outcome != observability.OutcomeSuccess || !strings.HasPrefix(e.Actor, "oidc:") {
		t.Fatalf("logout audit wrong: %+v", e)
	}
}

// failAuditSink fails every write — proves a broken sink cannot fail the
// request once wrapped, and that the failure is surfaced.
type failAuditSink struct{}

func (failAuditSink) WriteAudit(context.Context, observability.AuditEvent) error {
	return errors.New("sink down")
}

// TestAudit_SinkFailureStillSucceeds (R-V5a): with a failing sink behind
// GuardedSink the request succeeds, the failure is counted via the metric
// callback and one warn line is emitted — a broken sink means a loud,
// countable audit gap, never a failed request or silent loss.
func TestAudit_SinkFailureStillSucceeds(t *testing.T) {
	var failures int
	var logBuf bytes.Buffer
	sink := observability.NewGuardedSink(failAuditSink{},
		slog.New(slog.NewTextHandler(&logBuf, nil)),
		func(string) { failures++ })
	mux := http.NewServeMux()
	mux.Handle(routeWorkspaceStart, audited(sink, routeWorkspaceStart,
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		})))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/workspaces/ws_1/start", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status=%d, want 204 — audit failure must not fail the request", rec.Code)
	}
	if failures != 1 {
		t.Fatalf("audit error counter = %d, want 1", failures)
	}
	if !strings.Contains(logBuf.String(), "audit sink write failed") {
		t.Fatalf("no warn emitted for the sink failure: %q", logBuf.String())
	}
}

// TestAudit_HandlerPanicStillEmits (R-V5a): a panicking handler still
// produces exactly one event — failure/"panic" — emitted from the deferred
// path; the wrapper must not recover, the panic propagates unchanged.
func TestAudit_HandlerPanicStillEmits(t *testing.T) {
	sink := &captureSink{}
	h := RequestID(audited(sink, routeWorkspaceDelete, http.HandlerFunc(
		func(http.ResponseWriter, *http.Request) { panic("boom") })))
	req := httptest.NewRequest(http.MethodDelete, "/v1/workspaces/ws_9", nil)
	req.SetPathValue("id", "ws_9")
	panicked := false
	func() {
		defer func() { panicked = recover() != nil }()
		h.ServeHTTP(httptest.NewRecorder(), req)
	}()
	if !panicked {
		t.Fatal("the panic was swallowed — recover semantics changed")
	}
	e := requireEvent(t, sink, auditActionWorkspaceDelete)
	if e.Outcome != observability.OutcomeFailure || e.ErrorCode != "panic" {
		t.Fatalf("panic event outcome=%q err=%q, want failure/panic", e.Outcome, e.ErrorCode)
	}
	if e.TargetUID != "ws_9" || e.Actor != "anonymous" {
		t.Fatalf("panic event fields wrong: %+v", e)
	}
}
