// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package api

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	cdiv1 "github.com/tinyorbitvn/tinycdi/api/v1"
	"github.com/tinyorbitvn/tinycdi/internal/api/oidctest"
	"github.com/tinyorbitvn/tinycdi/internal/observability"
	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
	"github.com/tinyorbitvn/tinycdi/internal/store"
)

// fakeSessionPolicySource scripts the SessionPolicySource the handler
// reads/writes through — optimistic-concurrency If-Match like the store's.
type fakeSessionPolicySource struct {
	mu         sync.Mutex
	hasRow     bool
	version    int
	clip       string
	setCall    int
	lastWrite  store.SessionPolicyWrite
	lastCreate bool
}

func (f *fakeSessionPolicySource) Ceiling(_ context.Context, _ string) (store.SessionPolicyCeiling, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := store.SessionPolicyCeiling{HasRow: f.hasRow}
	if f.hasRow {
		out.Version = fmt.Sprintf("v%d", f.version)
		out.Clipboard = &f.clip
	}
	return out, nil
}

func (f *fakeSessionPolicySource) SetCeiling(_ context.Context, _ string, w store.SessionPolicyWrite, expectedVersion string, create bool) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setCall++
	f.lastWrite = w
	f.lastCreate = create
	switch {
	case create && f.hasRow:
		return "", store.ErrVersionMismatch
	case !create && (!f.hasRow || expectedVersion != fmt.Sprintf("v%d", f.version)):
		return "", store.ErrVersionMismatch
	}
	f.version++
	if w.Defaults() {
		f.hasRow = false
		return "", nil
	}
	f.hasRow = true
	if w.Clipboard != nil {
		f.clip = *w.Clipboard
	}
	return fmt.Sprintf("v%d", f.version), nil
}

// newSessionPolicyEnv mounts GET/PUT /v1/admin/tenants/{tenant}/
// session-policy backed by src, like newAdminQuotaEnv.
func newSessionPolicyEnv(t *testing.T, src SessionPolicySource, opts ...func(*AdminSessionPolicyHandler)) *testEnv {
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
	h := NewAdminSessionPolicyHandler(src, defaultTenants())
	for _, o := range opts {
		o(h)
	}
	mux.Handle("/auth/login", http.HandlerFunc(a.LoginHandler))
	mux.Handle("/auth/callback", http.HandlerFunc(a.CallbackHandler))
	MountAdminSessionPolicyRoutes(mux, a, h)
	srv := httptest.NewServer(RequestID(Audit(logger)(mux)))
	env := &testEnv{issuer: iss, auth: a, store: sessions, server: srv, logs: logBuf}
	t.Cleanup(func() { srv.Close(); iss.Close() })
	return env
}

const adminSessionPolicyPath = "/v1/admin/tenants/tenant-a/session-policy"

type adminSessionPolicyViewJSON struct {
	Tenant       string `json:"tenant"`
	Clipboard    string `json:"clipboard"`
	FileTransfer struct {
		Mode         string `json:"mode"`
		MaxFileBytes *int64 `json:"maxFileBytes"`
	} `json:"fileTransfer"`
	Source string `json:"source"`
}

// TestAdminSessionPolicy_Authz: admin-only and tenant-scoped — a regular
// user gets 403 on both verbs, a tenant admin naming another tenant gets
// 403, and no session gets 401.
func TestAdminSessionPolicy_Authz(t *testing.T) {
	src := &fakeSessionPolicySource{}
	env := newSessionPolicyEnv(t, src)

	sess, csrf := login(t, env, "user-a")
	for _, m := range []string{http.MethodGet, http.MethodPut} {
		body := ""
		if m == http.MethodPut {
			body = `{"clipboard":"Disabled"}`
		}
		r := doReq(t, env, sess, csrf, m, adminSessionPolicyPath, body,
			map[string]string{"If-Match": "*"})
		e := decodeErr(t, r)
		if r.StatusCode != http.StatusForbidden || e.Code != CodeForbidden {
			t.Fatalf("user %s: status=%d code=%q, want 403 FORBIDDEN", m, r.StatusCode, e.Code)
		}
	}

	sessA, csrfA := loginAdmin(t, env, "admin-a")
	for _, m := range []string{http.MethodGet, http.MethodPut} {
		body := ""
		if m == http.MethodPut {
			body = `{"clipboard":"Disabled"}`
		}
		r := doReq(t, env, sessA, csrfA, m, "/v1/admin/tenants/tenant-b/session-policy", body,
			map[string]string{"If-Match": "*"})
		e := decodeErr(t, r)
		if r.StatusCode != http.StatusForbidden || e.Code != CodeForbidden {
			t.Fatalf("cross-tenant %s: status=%d code=%q, want 403 FORBIDDEN", m, r.StatusCode, e.Code)
		}
	}
	if src.setCall != 0 {
		t.Fatalf("denied writes reached SetCeiling %d times", src.setCall)
	}

	req, _ := http.NewRequest(http.MethodGet, env.server.URL+adminSessionPolicyPath, nil)
	r, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated: status=%d, want 401", r.StatusCode)
	}
	r.Body.Close()
}

// TestAdminSessionPolicy_CSRF: the PUT requires the session CSRF token —
// a wrong one answers 403 CSRF_FAILED before the handler runs.
func TestAdminSessionPolicy_CSRF(t *testing.T) {
	src := &fakeSessionPolicySource{}
	env := newSessionPolicyEnv(t, src)
	sess, _ := loginAdmin(t, env, "admin-a")

	r := doReq(t, env, sess, &http.Cookie{Value: "wrong-token"}, http.MethodPut,
		adminSessionPolicyPath, `{"clipboard":"Disabled"}`, map[string]string{"If-Match": "*"})
	e := decodeErr(t, r)
	if r.StatusCode != http.StatusForbidden || e.Code != CodeCSRFFailed {
		t.Fatalf("status=%d code=%q, want 403 CSRF_FAILED", r.StatusCode, e.Code)
	}
	if src.setCall != 0 {
		t.Fatalf("CSRF-refused write reached SetCeiling %d times", src.setCall)
	}
}

// TestAdminSessionPolicy_GetNoRow: no row reads FollowTemplate on every
// channel with source FollowTemplate and no version.
func TestAdminSessionPolicy_GetNoRow(t *testing.T) {
	src := &fakeSessionPolicySource{}
	env := newSessionPolicyEnv(t, src)
	sess, csrf := loginAdmin(t, env, "admin-a")

	r := doReq(t, env, sess, csrf, http.MethodGet, adminSessionPolicyPath, "", nil)
	v := decodeBody[adminSessionPolicyViewJSON](t, r)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200", r.StatusCode)
	}
	if v.Source != SessionPolicySourceFollowTemplate || v.Clipboard != provisioning.PolicyFollowTemplate ||
		v.FileTransfer.Mode != provisioning.PolicyFollowTemplate || r.Header.Get("ETag") != "" {
		t.Fatalf("no-row view: %+v", v)
	}
}

// TestAdminSessionPolicy_PutValidation: bad enums, a missing clipboard and
// a non-default fileTransfer (the inert T11 channel) are 400s; the store
// is untouched.
func TestAdminSessionPolicy_PutValidation(t *testing.T) {
	src := &fakeSessionPolicySource{}
	env := newSessionPolicyEnv(t, src)
	sess, csrf := loginAdmin(t, env, "admin-a")

	for name, body := range map[string]string{
		"not json":           `nope`,
		"empty object":       `{}`,
		"bad enum":           `{"clipboard":"HalfDuplex"}`,
		"fileTransfer mode":  `{"clipboard":"Disabled","fileTransfer":{"mode":"Upload","maxFileBytes":null}}`,
		"fileTransfer bound": `{"clipboard":"Disabled","fileTransfer":{"mode":"FollowTemplate","maxFileBytes":1024}}`,
	} {
		r := doReq(t, env, sess, csrf, http.MethodPut, adminSessionPolicyPath, body,
			map[string]string{"If-Match": "*"})
		e := decodeErr(t, r)
		if r.StatusCode != http.StatusBadRequest || e.Code != CodeInvalidRequest {
			t.Fatalf("%s: status=%d code=%q, want 400 INVALID_REQUEST", name, r.StatusCode, e.Code)
		}
	}
	if src.setCall != 0 || src.hasRow {
		t.Fatalf("invalid writes reached the store: setCall=%d hasRow=%v", src.setCall, src.hasRow)
	}
}

// TestSessionPolicy_IfMatchMatrix: the ceiling is a strong-ETag resource.
// GET carries the ETag only while a row exists; no If-Match is 428, "*"
// creates a missing row and is 412 on an existing one, a stale tag is 412,
// the current tag writes and answers a new tag; an all-defaults PUT deletes
// the row, after which "*" is valid again.
func TestSessionPolicy_IfMatchMatrix(t *testing.T) {
	src := &fakeSessionPolicySource{}
	env := newSessionPolicyEnv(t, src)
	sess, csrf := loginAdmin(t, env, "admin-a")
	put := func(body string, hdrs map[string]string) *http.Response {
		return doReq(t, env, sess, csrf, http.MethodPut, adminSessionPolicyPath, body, hdrs)
	}

	requireErr(t, put(`{"clipboard":"Disabled"}`, nil), http.StatusPreconditionRequired, CodePreconditionRequired, "no If-Match")
	requireErr(t, put(`{"clipboard":"Disabled"}`, map[string]string{"If-Match": ETagOf("v1")}),
		http.StatusPreconditionFailed, CodePreconditionFailed, "tag on a missing row")
	if src.setCall != 0 {
		t.Fatalf("refused preconditions reached the store %d times", src.setCall)
	}

	r := put(`{"clipboard":"Disabled"}`, map[string]string{"If-Match": "*"})
	v := decodeBody[adminSessionPolicyViewJSON](t, r)
	tag := r.Header.Get("ETag")
	if r.StatusCode != http.StatusOK || v.Source != SessionPolicySourceTenantRow || tag == "" {
		t.Fatalf("create: status=%d view=%+v ETag=%q", r.StatusCode, v, tag)
	}
	if !src.lastCreate || src.lastWrite.Clipboard == nil || *src.lastWrite.Clipboard != "Disabled" {
		t.Fatalf("store saw: create=%v write=%+v", src.lastCreate, src.lastWrite)
	}
	g := doReq(t, env, sess, csrf, http.MethodGet, adminSessionPolicyPath, "", nil)
	body := decodeBody[map[string]any](t, g)
	if g.Header.Get("ETag") != tag {
		t.Fatalf("GET ETag = %q, want the write's %q", g.Header.Get("ETag"), tag)
	}
	if _, ok := body["version"]; ok {
		t.Fatalf("GET body still carries version: %v", body)
	}

	requireErr(t, put(`{"clipboard":"Send"}`, map[string]string{"If-Match": "*"}),
		http.StatusPreconditionFailed, CodePreconditionFailed, "* on an existing row")
	requireErr(t, put(`{"clipboard":"Send"}`, map[string]string{"If-Match": ETagOf("v0")}),
		http.StatusPreconditionFailed, CodePreconditionFailed, "stale tag")
	r = put(`{"clipboard":"Send"}`, map[string]string{"If-Match": tag})
	v2 := decodeBody[adminSessionPolicyViewJSON](t, r)
	next := r.Header.Get("ETag")
	if r.StatusCode != http.StatusOK || v2.Clipboard != "Send" || next == "" || next == tag {
		t.Fatalf("update: status=%d view=%+v ETag=%q (was %q)", r.StatusCode, v2, next, tag)
	}

	// All-defaults deletes the row: no ETag after, and "*" is valid again.
	r = put(`{"clipboard":"FollowTemplate"}`, map[string]string{"If-Match": next})
	v3 := decodeBody[adminSessionPolicyViewJSON](t, r)
	if r.StatusCode != http.StatusOK || v3.Source != SessionPolicySourceFollowTemplate || r.Header.Get("ETag") != "" {
		t.Fatalf("delete-on-defaults: status=%d view=%+v ETag=%q", r.StatusCode, v3, r.Header.Get("ETag"))
	}
	r = put(`{"clipboard":"Receive"}`, map[string]string{"If-Match": "*"})
	r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("* after the delete: status=%d, want 200", r.StatusCode)
	}

	requireAdminGuards(t, env, http.MethodPut, adminSessionPolicyPath, `{"clipboard":"Disabled"}`,
		map[string]string{"If-Match": "*"},
		func() int { src.mu.Lock(); defer src.mu.Unlock(); return src.setCall })
	requireAdminGuards(t, env, http.MethodGet, adminSessionPolicyPath, "", nil, func() int { return 0 })
}

// TestAdminSessionPolicy_DeleteOnDefault: an all-FollowTemplate PUT
// resolves to the row's deletion — the next GET reports FollowTemplate
// source again.
func TestAdminSessionPolicy_DeleteOnDefault(t *testing.T) {
	src := &fakeSessionPolicySource{hasRow: true, version: 3, clip: "Send"}
	env := newSessionPolicyEnv(t, src)
	sess, csrf := loginAdmin(t, env, "admin-a")

	r := doReq(t, env, sess, csrf, http.MethodPut, adminSessionPolicyPath,
		`{"clipboard":"FollowTemplate","fileTransfer":{"mode":"FollowTemplate","maxFileBytes":null}}`,
		map[string]string{"If-Match": ETagOf("v3")})
	v := decodeBody[adminSessionPolicyViewJSON](t, r)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200", r.StatusCode)
	}
	if v.Source != SessionPolicySourceFollowTemplate || r.Header.Get("ETag") != "" {
		t.Fatalf("post-delete view: %+v", v)
	}
	if !src.lastWrite.Defaults() {
		t.Fatalf("all-FollowTemplate PUT must arrive as an empty write: %+v", src.lastWrite)
	}
	if src.hasRow {
		t.Fatal("row still present after delete-on-default")
	}
}

// TestAdminSessionPolicy_Audit: the write is captured by the dedicated
// audit stream — an ordinary ceiling write emits
// admin.session_policy.set, an all-FollowTemplate PUT the clear variant;
// both carry the pseudonymous actor and the tenant as target.
func TestAdminSessionPolicy_Audit(t *testing.T) {
	sink := &captureSink{}
	src := &fakeSessionPolicySource{hasRow: true, version: 1, clip: "Disabled"}
	env := newSessionPolicyEnv(t, src,
		func(h *AdminSessionPolicyHandler) { h.WithAuditSink(sink) })
	sess, csrf := loginAdmin(t, env, "admin-a")

	r := doReq(t, env, sess, csrf, http.MethodPut, adminSessionPolicyPath,
		`{"clipboard":"Send"}`, map[string]string{"If-Match": ETagOf("v1")})
	r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("set: status=%d, want 200", r.StatusCode)
	}
	r = doReq(t, env, sess, csrf, http.MethodPut, adminSessionPolicyPath,
		`{"clipboard":"FollowTemplate"}`, map[string]string{"If-Match": ETagOf("v2")})
	r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("clear: status=%d, want 200", r.StatusCode)
	}

	set := requireEvent(t, sink, auditActionAdminSessionPolicySet)
	if set.Outcome != observability.OutcomeSuccess || set.TargetUID != "tenant-a" ||
		set.Details["clipboard"] != "Send" {
		t.Fatalf("set audit: %+v", set)
	}
	clear := requireEvent(t, sink, auditActionAdminSessionPolicyClear)
	if clear.Outcome != observability.OutcomeSuccess || clear.TargetUID != "tenant-a" {
		t.Fatalf("clear audit: %+v", clear)
	}
}

// TestAdminSessionPolicy_Contract: the handler's serialized view
// validates against the published AdminSessionPolicyView schema in both
// source states, and the launch ticket's sessionPolicy against
// ResolvedSessionPolicy / LaunchTicket.
func TestAdminSessionPolicy_Contract(t *testing.T) {
	requireValid(t, "AdminSessionPolicyView", adminSessionPolicyView{
		Tenant:    "acme",
		Clipboard: "FollowTemplate",
		FileTransfer: adminFileTransferCeiling{
			Mode: "FollowTemplate",
		},
		Source: SessionPolicySourceFollowTemplate,
	})
	requireValid(t, "AdminSessionPolicyView", adminSessionPolicyView{
		Tenant:    "acme",
		Clipboard: "Disabled",
		FileTransfer: adminFileTransferCeiling{
			Mode: "FollowTemplate",
		},
		Source: SessionPolicySourceTenantRow,
	})

	resolved := provisioning.SessionPolicy{
		Clipboard:    provisioning.PolicyDisabled,
		FileTransfer: cdiv1.FileTransferDisabled,
	}
	requireValid(t, "ResolvedSessionPolicy", resolved)
	requireValid(t, "LaunchTicket", launchTicketView{
		WorkspaceID:   "ws_01J4Z8KQ2M9XNBV3T7YH0R6D5E",
		Ticket:        "tkt_0123456789abcdef0123456789abcdef",
		LaunchURL:     "https://ws.session.example.invalid/v1/launch",
		ExpiresAt:     time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC),
		SessionPolicy: &resolved,
	})
}
