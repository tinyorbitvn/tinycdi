// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package api

import (
	"bytes"
	"context"
	"encoding/json"
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
