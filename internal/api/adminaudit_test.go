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
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/api/oidctest"
	"github.com/tinyorbitvn/tinycdi/internal/observability"
	"github.com/tinyorbitvn/tinycdi/internal/store"
)

// newAdminMountEnv builds a testEnv whose mux carries /auth/login,
// /auth/callback and whatever mount adds, behind RequestID(Audit(logger)).
func newAdminMountEnv(t *testing.T, mount func(mux *http.ServeMux, a *Authenticator)) *testEnv {
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
	mount(mux, a)
	srv := httptest.NewServer(RequestID(Audit(logger)(mux)))
	env := &testEnv{issuer: iss, auth: a, store: sessions, server: srv, logs: logBuf}
	t.Cleanup(func() { srv.Close(); iss.Close() })
	return env
}

func newAdminAuditEnv(t *testing.T, src AuditSource) *testEnv {
	t.Helper()
	return newAdminMountEnv(t, func(mux *http.ServeMux, a *Authenticator) {
		MountAdminAuditRoutes(mux, a, NewAdminAuditHandler(src, defaultTenants()))
	})
}

const adminAuditPath = "/v1/admin/tenants/tenant-a/audit"

type fakeAuditSource struct {
	gotFilter store.AuditFilter
	gotAfter  *store.AuditCursor
	rows      []store.AuditRecord
}

func (f *fakeAuditSource) ListAudit(_ context.Context, fl store.AuditFilter, limit int, after *store.AuditCursor) ([]store.AuditRecord, error) {
	f.gotFilter, f.gotAfter = fl, after
	if len(f.rows) > limit+1 {
		return f.rows[:limit+1], nil
	}
	return f.rows, nil
}

func auditRows(n int) []store.AuditRecord {
	base := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	out := make([]store.AuditRecord, n)
	for i := range out {
		out[i] = store.AuditRecord{ID: int64(n - i), Tenant: "tenant-a", ActorRef: "oidc:0011223344556677",
			Action: "workspace.start", TargetKind: "workspace", TargetID: fmt.Sprintf("ws_%d", i),
			Outcome: "success", RequestID: "req", Details: map[string]string{}, CreatedAt: base.Add(-time.Duration(i) * time.Minute)}
	}
	return out
}

func TestAdminAudit_ListPagesAndFilters(t *testing.T) {
	src := &fakeAuditSource{rows: auditRows(3)}
	env := newAdminAuditEnv(t, src)
	sess, csrf := loginAdmin(t, env, "admin-a")
	res := doReq(t, env, sess, csrf, http.MethodGet,
		"/v1/admin/tenants/tenant-a/audit?limit=2&action=workspace.start&actor=https%3A%2F%2Fidp%7Calice&since=2026-10-01T00:00:00Z", "", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d", res.StatusCode)
	}
	page := decodeBody[Page[adminAuditEntry]](t, res)
	if len(page.Items) != 2 || page.NextCursor == "" {
		t.Fatalf("page = %+v, want 2 items and a cursor", page)
	}
	if src.gotFilter.Tenant != "tenant-a" || src.gotFilter.Action != "workspace.start" ||
		src.gotFilter.ActorRef != observability.ActorRef("https://idp", "alice") || src.gotFilter.Since == nil {
		t.Fatalf("filter = %+v", src.gotFilter)
	}
	res = doReq(t, env, sess, csrf, http.MethodGet, "/v1/admin/tenants/tenant-a/audit?limit=2&cursor="+url.QueryEscape(page.NextCursor), "", nil)
	if res.StatusCode != http.StatusOK || src.gotAfter == nil || src.gotAfter.ID != 2 {
		t.Fatalf("cursor not decoded: status=%d after=%+v", res.StatusCode, src.gotAfter)
	}
}

func TestAdminAudit_RejectsBadInput(t *testing.T) {
	env := newAdminAuditEnv(t, &fakeAuditSource{})
	sess, csrf := loginAdmin(t, env, "admin-a")
	for _, q := range []string{"sort=createdAt", "since=yesterday", "actor=nobody", "since=2026-10-02T00:00:00Z&until=2026-10-01T00:00:00Z", "action=" + strings.Repeat("a", 129)} {
		res := doReq(t, env, sess, csrf, http.MethodGet, "/v1/admin/tenants/tenant-a/audit?"+q, "", nil)
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", q, res.StatusCode)
		}
	}
}

func TestAdminAudit_Authz(t *testing.T) {
	env := newAdminAuditEnv(t, &fakeAuditSource{})
	sess, csrf := login(t, env, "user-a")
	if res := doReq(t, env, sess, csrf, http.MethodGet, "/v1/admin/tenants/tenant-a/audit", "", nil); res.StatusCode != http.StatusForbidden {
		t.Fatalf("non-admin: %d", res.StatusCode)
	}
	asess, acsrf := loginAdmin(t, env, "admin-a")
	if res := doReq(t, env, asess, acsrf, http.MethodGet, "/v1/admin/tenants/tenant-b/audit", "", nil); res.StatusCode != http.StatusForbidden {
		t.Fatalf("other tenant: %d", res.StatusCode)
	}
}
