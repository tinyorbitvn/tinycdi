// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/api/oidctest"
	"github.com/tinyorbitvn/tinycdi/internal/observability"
	"github.com/tinyorbitvn/tinycdi/internal/store"
)

const adminSessionsPath = "/v1/admin/tenants/tenant-a/sessions"

func adminRevokePath(ownerRef string) string {
	return "/v1/admin/tenants/tenant-a/users/" + url.PathEscape(ownerRef) + "/sessions:revoke-all"
}

type fakeLeaseSource struct {
	leases []store.LiveLease // newest first; returned whole, like the store's limit+1 read
	got    store.LeaseFilter
	calls  int
}

func (f *fakeLeaseSource) ListLiveLeases(_ context.Context, tenant string, _ time.Time, fl store.LeaseFilter) ([]store.LiveLease, error) {
	f.calls++
	f.got = fl
	if tenant != "tenant-a" {
		return nil, nil
	}
	return f.leases, nil
}

type fakeAdminRevoker struct {
	tenant, issuer, subject string
	calls                   int
	err                     error
}

func (f *fakeAdminRevoker) RevokePrincipalSessions(_ context.Context, tenant, issuer, subject string) (RevokeAllResult, error) {
	f.calls++
	f.tenant, f.issuer, f.subject = tenant, issuer, subject
	return RevokeAllResult{Sessions: 2, Tickets: 1, Leases: 1}, f.err
}

// newAdminSessionsEnv mounts the two F5 routes over RequestID(Audit(logger)(mux)),
// built like newSessionPolicyEnv (adminsessionpolicy_test.go).
func newAdminSessionsEnv(t *testing.T, src LiveLeaseSource, rev PrincipalRevoker, opts ...func(*AdminSessionsHandler)) *testEnv {
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
	h := NewAdminSessionsHandler(src, nil, rev, defaultTenants())
	for _, o := range opts {
		o(h)
	}
	mux.Handle("/auth/login", http.HandlerFunc(a.LoginHandler))
	mux.Handle("/auth/callback", http.HandlerFunc(a.CallbackHandler))
	MountAdminSessionRoutes(mux, a, h)
	srv := httptest.NewServer(RequestID(Audit(logger)(mux)))
	env := &testEnv{issuer: iss, auth: a, store: sessions, server: srv, logs: logBuf}
	t.Cleanup(func() { srv.Close(); iss.Close() })
	return env
}

func bodyOf(t *testing.T, res *http.Response) string {
	t.Helper()
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestAdminSessions_ListHidesLeaseIDs(t *testing.T) {
	now := time.Now()
	src := &fakeLeaseSource{leases: []store.LiveLease{{ID: "lease-secret-1", WorkspaceID: "ws_1", WorkspaceName: "dev", OwnerRef: "https://idp|alice", CreatedAt: now, ExpiresAt: now.Add(time.Minute)}}}
	env := newAdminSessionsEnv(t, src, &fakeAdminRevoker{})
	sess, csrf := loginAdmin(t, env, "admin-a")
	res := doReq(t, env, sess, csrf, http.MethodGet, adminSessionsPath+"?ownerRef="+url.QueryEscape("https://idp|alice"), "", nil)
	status, body := res.StatusCode, bodyOf(t, res)
	if status != http.StatusOK || strings.Contains(body, "lease-secret-1") {
		t.Fatalf("status %d body %s: the lease id must never be returned", status, body)
	}
	if !strings.Contains(body, leaseRef("lease-secret-1")) || src.got.OwnerRef != "https://idp|alice" {
		t.Fatalf("body %s filter %+v", body, src.got)
	}
}

// The cursor carries the keyset position (created_at, workspace id); the
// lease id appears neither in items nor in the cursor.
func TestAdminSessions_PagingCursorHasNoLeaseID(t *testing.T) {
	now := time.Now()
	src := &fakeLeaseSource{leases: []store.LiveLease{
		{ID: "lease-secret-3", WorkspaceID: "ws_3", OwnerRef: "https://idp|a", CreatedAt: now, ExpiresAt: now.Add(time.Minute)},
		{ID: "lease-secret-2", WorkspaceID: "ws_2", OwnerRef: "https://idp|a", CreatedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Minute)},
		{ID: "lease-secret-1", WorkspaceID: "ws_1", OwnerRef: "https://idp|a", CreatedAt: now.Add(-2 * time.Minute), ExpiresAt: now.Add(time.Minute)},
	}}
	env := newAdminSessionsEnv(t, src, &fakeAdminRevoker{})
	sess, csrf := loginAdmin(t, env, "admin-a")
	page := decodeBody[Page[adminLiveSession]](t, doReq(t, env, sess, csrf, http.MethodGet, adminSessionsPath+"?limit=2", "", nil))
	if len(page.Items) != 2 || page.NextCursor == "" || src.got.Limit != 2 {
		t.Fatalf("page = %+v filter %+v, want 2 items and a cursor", page, src.got)
	}
	raw, err := base64.RawURLEncoding.DecodeString(page.NextCursor)
	if err != nil || strings.Contains(string(raw), "lease-secret") || !strings.Contains(string(raw), "ws_2") {
		t.Fatalf("cursor %q decodes to %q, err %v: want the workspace id, never the lease id", page.NextCursor, raw, err)
	}
	decodeBody[Page[adminLiveSession]](t, doReq(t, env, sess, csrf, http.MethodGet, adminSessionsPath+"?limit=2&cursor="+page.NextCursor, "", nil))
	if src.got.After == nil || src.got.After.ID != "ws_2" {
		t.Fatalf("second read got After %+v, want workspace ws_2", src.got.After)
	}
}

func TestAdminSessions_ListRefusals(t *testing.T) {
	src := &fakeLeaseSource{}
	env := newAdminSessionsEnv(t, src, &fakeAdminRevoker{})
	sess, csrf := loginAdmin(t, env, "admin-a")
	cases := []struct {
		name, path string
		want       int
	}{
		{"other tenant", "/v1/admin/tenants/tenant-b/sessions", http.StatusForbidden},
		{"ascending sort", adminSessionsPath + "?sort=createdAt", http.StatusBadRequest},
		{"bad ownerRef", adminSessionsPath + "?ownerRef=bad", http.StatusBadRequest},
		{"bad cursor", adminSessionsPath + "?cursor=%21", http.StatusBadRequest},
	}
	for _, c := range cases {
		res := doReq(t, env, sess, csrf, http.MethodGet, c.path, "", nil)
		res.Body.Close()
		if res.StatusCode != c.want {
			t.Fatalf("%s: status %d, want %d", c.name, res.StatusCode, c.want)
		}
	}
	usess, ucsrf := loginGroups(t, env, "user-a") // no groups claim: not an administrator
	res := doReq(t, env, usess, ucsrf, http.MethodGet, adminSessionsPath, "", nil)
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("non-admin: %d", res.StatusCode)
	}
	if src.calls != 0 {
		t.Fatalf("the lease source was read %d times by refused requests", src.calls)
	}
}

func TestAdminSessions_RevokeAllTargetsTheNamedUser(t *testing.T) {
	rev := &fakeAdminRevoker{}
	env := newAdminSessionsEnv(t, &fakeLeaseSource{}, rev)
	sess, csrf := loginAdmin(t, env, "admin-a")
	res := doReq(t, env, sess, csrf, http.MethodPost, adminRevokePath("https://idp|alice"), "", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d", res.StatusCode)
	}
	if c := res.Header.Values("Set-Cookie"); len(c) != 0 {
		t.Fatalf("the admin's own cookie must not be cleared: %v", c)
	}
	got := decodeBody[adminRevokeResult](t, res)
	if rev.tenant != "tenant-a" || rev.issuer != "https://idp" || rev.subject != "alice" {
		t.Fatalf("revoked %s/%s/%s", rev.tenant, rev.issuer, rev.subject)
	}
	if got.Sessions != 2 || got.Tickets != 1 || got.Leases != 1 {
		t.Fatalf("result %+v", got)
	}
}

func TestAdminSessions_RevokeAllRefusals(t *testing.T) {
	rev := &fakeAdminRevoker{}
	env := newAdminSessionsEnv(t, &fakeLeaseSource{}, rev)
	sess, csrf := loginAdmin(t, env, "admin-a")
	path := adminRevokePath("https://idp|alice")
	wrong := &http.Cookie{Name: env.auth.CSRFHeader(), Value: "wrong"}
	if res := doReq(t, env, sess, wrong, http.MethodPost, path, "", nil); res.StatusCode != http.StatusForbidden {
		t.Fatalf("bad CSRF: %d", res.StatusCode)
	}
	other := "/v1/admin/tenants/tenant-b/users/" + url.PathEscape("https://idp|alice") + "/sessions:revoke-all"
	if res := doReq(t, env, sess, csrf, http.MethodPost, other, "", nil); res.StatusCode != http.StatusForbidden {
		t.Fatalf("other tenant: %d", res.StatusCode)
	}
	if res := doReq(t, env, sess, csrf, http.MethodPost, adminRevokePath("bad"), "", nil); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad ownerRef: %d", res.StatusCode)
	}
	usess, ucsrf := loginGroups(t, env, "user-a")
	if res := doReq(t, env, usess, ucsrf, http.MethodPost, path, "", nil); res.StatusCode != http.StatusForbidden {
		t.Fatalf("non-admin: %d", res.StatusCode)
	}
	if rev.calls != 0 {
		t.Fatalf("revoker called %d times by refused requests", rev.calls)
	}
}

func TestAdminSessions_RevokeFailureIs500(t *testing.T) {
	rev := &fakeAdminRevoker{err: context.DeadlineExceeded}
	env := newAdminSessionsEnv(t, &fakeLeaseSource{}, rev)
	sess, csrf := loginAdmin(t, env, "admin-a")
	res := doReq(t, env, sess, csrf, http.MethodPost, adminRevokePath("https://idp|alice"), "", nil)
	if e := decodeErr(t, res); res.StatusCode != http.StatusInternalServerError || e.Code != CodeInternal {
		t.Fatalf("status %d code %q, want 500 INTERNAL", res.StatusCode, e.Code)
	}
}

// The audit record for the revoke names the admin as actor, the tenant as
// target and the revoked principal only as a pseudonymous ref.
func TestAudit_AdminSessionRevoke(t *testing.T) {
	sink := &captureSink{}
	env := newAdminSessionsEnv(t, &fakeLeaseSource{}, &fakeAdminRevoker{},
		func(h *AdminSessionsHandler) { h.WithAuditSink(sink) })
	sess, csrf := loginAdmin(t, env, "admin-a")
	doReq(t, env, sess, csrf, http.MethodPost, adminRevokePath("https://idp|alice"), "", nil).Body.Close()
	e := requireEvent(t, sink, "admin.session.revoke")
	if e.Outcome != observability.OutcomeSuccess || e.Tenant != "tenant-a" || e.TargetUID != "tenant-a" {
		t.Fatalf("event %+v", e)
	}
	if e.Details["target"] != targetActorRef("https://idp|alice") || e.Details["counts"] != "sessions=2,tickets=1,leases=1" {
		t.Fatalf("details %+v", e.Details)
	}
	if strings.Contains(sink.raw(), "alice") {
		t.Fatalf("the revoked subject leaked into the audit record: %s", sink.raw())
	}
	usess, ucsrf := loginGroups(t, env, "user-a")
	doReq(t, env, usess, ucsrf, http.MethodPost, adminRevokePath("https://idp|alice"), "", nil).Body.Close()
	var denied int
	for _, ev := range sink.get() {
		if ev.Action == "admin.session.revoke" && ev.Outcome == observability.OutcomeDenied {
			denied++
		}
	}
	if denied != 1 {
		t.Fatalf("denied revoke events = %d, want 1: attempted privilege use must be auditable", denied)
	}
}
