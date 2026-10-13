// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package api

// Configurable tenant-admin groups (v1.2): the tenant-admin role used to be
// hard-wired to the literal group "tenant-admin"; AuthConfig.TenantAdminGroups
// now names the verified groups-claim values that grant it (flag
// -tenant-admin-groups, default exactly "tenant-admin"). These tests pin that
// EVERY admin gate — /v1/me roles, scope=tenant on both list endpoints,
// /v1/admin/* reads and writes, foreign-workspace get, retained attach/purge
// and the /v1/quota users[] view — honours a non-default group and rejects a
// member of only the default name (and of the Keycloak full-path form).

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/api/oidctest"
)

const testCustomAdminGroup = "platform-admins"

// adminGroupFixtures holds the fake backends behind one env so a test can
// seed and assert across every admin-gated surface.
type adminGroupFixtures struct {
	be  *fakeWorkspaceBackend
	fs  *fakeRetainedStore
	qs  *fakeQuotaSource
	aqs *fakeAdminQuotaSource
	uls *fakeUserLimitSource
	sps *fakeSessionPolicySource
}

// newAdminGroupEnv mounts every admin-gated surface (/v1/me, /v1/workspaces,
// /v1/data, /v1/quota, /v1/admin/*) behind an authenticator whose
// TenantAdminGroups is the given list — nil selects the default.
func newAdminGroupEnv(t *testing.T, adminGroups []string) (*testEnv, *adminGroupFixtures) {
	t.Helper()
	iss, err := oidctest.NewIssuer()
	if err != nil {
		t.Fatalf("oidctest.NewIssuer: %v", err)
	}
	logBuf := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(logBuf, nil))
	sessions := NewInMemorySessionStore(30 * time.Minute)
	a, err := NewAuthenticator(context.Background(), AuthConfig{
		Issuer:            iss.URL(),
		ClientID:          iss.ClientID,
		RedirectURL:       "https://portal.test/auth/callback",
		LoginSealer:       testLoginSealer(t),
		TenantAdminGroups: adminGroups,
	}, sessions, logger)
	if err != nil {
		t.Fatalf("NewAuthenticator: %v", err)
	}
	fx := &adminGroupFixtures{
		be:  newFakeBackend(),
		fs:  newFakeRetainedStore(),
		qs:  &fakeQuotaSource{},
		aqs: &fakeAdminQuotaSource{},
		uls: newFakeUserLimitSource(),
		sps: &fakeSessionPolicySource{},
	}
	mux := http.NewServeMux()
	mux.Handle("/auth/login", http.HandlerFunc(a.LoginHandler))
	mux.Handle("/auth/callback", http.HandlerFunc(a.CallbackHandler))
	MountMeRoutes(mux, a, NewMeHandler(testSessionDomain))
	MountWorkspaceRoutes(mux, a, NewWorkspaceHandler(fx.be, defaultCatalog(), defaultTenants()), nil)
	MountDataRoutes(mux, a, NewDataHandler(fx.fs, defaultCatalog(), defaultTenants()))
	MountQuotaRoutes(mux, a, NewQuotaHandler(fx.qs, defaultTenants()))
	MountAdminQuotaRoutes(mux, a, NewAdminQuotaHandler(fx.aqs, defaultTenants(), nil))
	MountAdminUserLimitRoutes(mux, a, NewAdminUserLimitsHandler(fx.uls, nil, defaultTenants()))
	MountAdminSessionPolicyRoutes(mux, a, NewAdminSessionPolicyHandler(fx.sps, defaultTenants()))
	srv := httptest.NewServer(RequestID(Audit(logger)(mux)))
	env := &testEnv{issuer: iss, auth: a, store: sessions, server: srv, logs: logBuf}
	t.Cleanup(func() { srv.Close(); iss.Close() })
	return env, fx
}

// loginGroups logs in subject carrying the given verified groups claim.
func loginGroups(t *testing.T, env *testEnv, subject string, groups ...string) (*http.Cookie, *http.Cookie) {
	t.Helper()
	env.issuer.Groups = groups
	return login(t, env, subject)
}

// TestTenantAdminGroups_AuthConfigValidation: nil selects the default and a
// custom list is accepted; an explicitly EMPTY list and blank entries are
// startup errors — there is no silent "nobody is admin" mode.
func TestTenantAdminGroups_AuthConfigValidation(t *testing.T) {
	mkCfg := func(groups []string) AuthConfig {
		return AuthConfig{
			Issuer:            "https://idp.test",
			ClientID:          "tinycdi",
			RedirectURL:       "https://portal.test/auth/callback",
			LoginSealer:       testLoginSealer(t),
			TenantAdminGroups: groups,
		}
	}
	sessions := NewInMemorySessionStore(30 * time.Minute)
	for _, bad := range [][]string{{}, {""}, {"  "}, {"admins", ""}} {
		if _, err := NewAuthenticator(context.Background(), mkCfg(bad), sessions, slog.Default()); err == nil {
			t.Fatalf("TenantAdminGroups %v accepted — an empty admin set must fail", bad)
		}
	}
	// A non-empty list still runs OIDC discovery — point it at the fake
	// issuer to prove the config itself validates.
	iss, err := oidctest.NewIssuer()
	if err != nil {
		t.Fatalf("oidctest.NewIssuer: %v", err)
	}
	defer iss.Close()
	cfg := mkCfg([]string{testCustomAdminGroup})
	cfg.Issuer = iss.URL()
	cfg.ClientID = iss.ClientID
	if _, err := NewAuthenticator(context.Background(), cfg, sessions, slog.Default()); err != nil {
		t.Fatalf("custom TenantAdminGroups rejected: %v", err)
	}
	cfg.TenantAdminGroups = nil
	a, err := NewAuthenticator(context.Background(), cfg, sessions, slog.Default())
	if err != nil {
		t.Fatalf("nil TenantAdminGroups must default, got: %v", err)
	}
	if len(a.cfg.TenantAdminGroups) != 1 || a.cfg.TenantAdminGroups[0] != TenantAdminGroup {
		t.Fatalf("default TenantAdminGroups = %v, want [%s]", a.cfg.TenantAdminGroups, TenantAdminGroup)
	}
}

// TestMe_CustomAdminGroups: the configured group grants the tenant-admin
// role name; a member of only the default name — or of the Keycloak
// full-path form "/platform-admins" — does not get it.
func TestMe_CustomAdminGroups(t *testing.T) {
	env, _ := newAdminGroupEnv(t, []string{testCustomAdminGroup})

	sess, _ := loginGroups(t, env, "admin-1", testCustomAdminGroup)
	r := env.authedGet(t, sess, "/v1/me")
	me := decodeMe(t, r)
	r.Body.Close()
	if len(me.Roles) != 2 || me.Roles[0] != "user" || me.Roles[1] != TenantAdminGroup {
		t.Fatalf("admin roles = %v, want [user tenant-admin]", me.Roles)
	}

	for _, tc := range []struct {
		name   string
		groups []string
	}{
		{"default group is not special under custom config", []string{TenantAdminGroup}},
		{"full path does not match", []string{"/" + testCustomAdminGroup}},
		{"unrelated group", []string{"devs"}},
	} {
		sess, _ := loginGroups(t, env, "user-x", tc.groups...)
		r := env.authedGet(t, sess, "/v1/me")
		me := decodeMe(t, r)
		r.Body.Close()
		if len(me.Roles) != 1 || me.Roles[0] != "user" {
			t.Fatalf("%s: roles = %v, want [user]", tc.name, me.Roles)
		}
	}
}

// TestScopeTenant_CustomAdminGroups: scope=tenant on both list endpoints
// honours the configured group (200) and rejects a member of only the
// default name (403).
func TestScopeTenant_CustomAdminGroups(t *testing.T) {
	env, _ := newAdminGroupEnv(t, []string{testCustomAdminGroup})

	sess, csrf := loginGroups(t, env, "admin-a", testCustomAdminGroup)
	for _, path := range []string{"/v1/workspaces?scope=tenant", "/v1/data?scope=tenant"} {
		r := doReq(t, env, sess, csrf, http.MethodGet, path, "", nil)
		r.Body.Close()
		if r.StatusCode != http.StatusOK {
			t.Fatalf("%s: status=%d, want 200", path, r.StatusCode)
		}
	}

	sess, csrf = loginGroups(t, env, "admin-b", TenantAdminGroup)
	for _, path := range []string{"/v1/workspaces?scope=tenant", "/v1/data?scope=tenant"} {
		r := doReq(t, env, sess, csrf, http.MethodGet, path, "", nil)
		r.Body.Close()
		if r.StatusCode != http.StatusForbidden {
			t.Fatalf("%s: status=%d, want 403 for a non-member of the configured group", path, r.StatusCode)
		}
	}
}

// TestAdminQuota_CustomAdminGroups: the quota admin gate honours the
// configured group — member reads and writes; a member of only the default
// name is denied and the write never reaches the store.
func TestAdminQuota_CustomAdminGroups(t *testing.T) {
	env, fx := newAdminGroupEnv(t, []string{testCustomAdminGroup})

	sess, csrf := loginGroups(t, env, "admin-a", testCustomAdminGroup)
	r := doReq(t, env, sess, csrf, http.MethodGet, adminQuotaPath, "", nil)
	r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("member GET: status=%d, want 200", r.StatusCode)
	}
	r = doReq(t, env, sess, csrf, http.MethodPut, adminQuotaPath, adminQuotaBody,
		map[string]string{"If-Match": IfMatchCreate})
	r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("member PUT: status=%d, want 200", r.StatusCode)
	}

	sess, csrf = loginGroups(t, env, "admin-b", TenantAdminGroup)
	fx.aqs.mu.Lock()
	setCalls := fx.aqs.setCall
	fx.aqs.mu.Unlock()
	for _, m := range []string{http.MethodGet, http.MethodPut} {
		body := ""
		hdrs := map[string]string{}
		if m == http.MethodPut {
			body = adminQuotaBody
			hdrs["If-Match"] = IfMatchCreate
		}
		r = doReq(t, env, sess, csrf, m, adminQuotaPath, body, hdrs)
		r.Body.Close()
		if r.StatusCode != http.StatusForbidden {
			t.Fatalf("non-member %s: status=%d, want 403", m, r.StatusCode)
		}
	}
	fx.aqs.mu.Lock()
	defer fx.aqs.mu.Unlock()
	if fx.aqs.setCall != setCalls {
		t.Fatalf("denied writes reached SetLimits: %d -> %d", setCalls, fx.aqs.setCall)
	}
}

// TestAdminUserLimits_CustomAdminGroups: the user-limits admin gate honours
// the configured group.
func TestAdminUserLimits_CustomAdminGroups(t *testing.T) {
	env, _ := newAdminGroupEnv(t, []string{testCustomAdminGroup})

	sess, csrf := loginGroups(t, env, "admin-a", testCustomAdminGroup)
	r := doReq(t, env, sess, csrf, http.MethodGet, userLimitsPath, "", nil)
	r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("member GET: status=%d, want 200", r.StatusCode)
	}

	sess, csrf = loginGroups(t, env, "admin-b", TenantAdminGroup)
	r = doReq(t, env, sess, csrf, http.MethodGet, userLimitsPath, "", nil)
	r.Body.Close()
	if r.StatusCode != http.StatusForbidden {
		t.Fatalf("non-member GET: status=%d, want 403", r.StatusCode)
	}
}

// TestAdminSessionPolicy_CustomAdminGroups: the session-policy admin gate
// honours the configured group — same tenantAdminPrincipal path as quota
// and user limits.
func TestAdminSessionPolicy_CustomAdminGroups(t *testing.T) {
	env, _ := newAdminGroupEnv(t, []string{testCustomAdminGroup})

	sess, csrf := loginGroups(t, env, "admin-a", testCustomAdminGroup)
	r := doReq(t, env, sess, csrf, http.MethodGet, adminSessionPolicyPath, "", nil)
	r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("member GET: status=%d, want 200", r.StatusCode)
	}

	sess, csrf = loginGroups(t, env, "admin-b", TenantAdminGroup)
	r = doReq(t, env, sess, csrf, http.MethodGet, adminSessionPolicyPath, "", nil)
	r.Body.Close()
	if r.StatusCode != http.StatusForbidden {
		t.Fatalf("non-member GET: status=%d, want 403", r.StatusCode)
	}
}

// TestWorkspaceForeignGet_CustomAdminGroups: a member of the configured
// group reads another owner's workspace (natural admin scope); a member of
// only the default name gets the same 404 as any other tenant user —
// existence is never leaked.
func TestWorkspaceForeignGet_CustomAdminGroups(t *testing.T) {
	env, _ := newAdminGroupEnv(t, []string{testCustomAdminGroup})

	sessB, csrfB := loginGroups(t, env, "user-b")
	r := doReq(t, env, sessB, csrfB, http.MethodPost, "/v1/workspaces",
		`{"name":"b-desktop","templateRef":"tpl_linuxdesktop"}`,
		map[string]string{"Idempotency-Key": "key-admingrp-1"})
	ws := decodeBody[WorkspaceView](t, r)
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("create status=%d", r.StatusCode)
	}

	sessA, csrfA := loginGroups(t, env, "admin-a", testCustomAdminGroup)
	r = doReq(t, env, sessA, csrfA, http.MethodGet, "/v1/workspaces/"+ws.ID, "", nil)
	r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("member GET foreign workspace: status=%d, want 200", r.StatusCode)
	}
	r = doReq(t, env, sessA, csrfA, http.MethodGet, "/v1/workspaces", "", nil)
	wl := decodeBody[Page[WorkspaceView]](t, r)
	if len(wl.Items) != 1 {
		t.Fatalf("member natural scope listed %d workspaces, want 1 (tenant-wide)", len(wl.Items))
	}

	sessC, csrfC := loginGroups(t, env, "admin-c", TenantAdminGroup)
	r = doReq(t, env, sessC, csrfC, http.MethodGet, "/v1/workspaces/"+ws.ID, "", nil)
	r.Body.Close()
	if r.StatusCode != http.StatusNotFound {
		t.Fatalf("non-member GET foreign workspace: status=%d, want 404", r.StatusCode)
	}
}

// TestDataForeignRecord_CustomAdminGroups: a member of the configured group
// reads, attaches and purges another owner's retained record (ownerScope
// ""); a member of only the default name is scoped out — the record is 404
// on every verb.
func TestDataForeignRecord_CustomAdminGroups(t *testing.T) {
	env, fx := newAdminGroupEnv(t, []string{testCustomAdminGroup})
	owner := envOwner(env, "user-b")
	attachRec := seedRetained(fx.fs, "rd_aaaaaaaaaaaaaaaaaaaaaaaaaa", "tenant-a", owner, "LinuxContainer")
	purgeRec := seedRetained(fx.fs, "rd_bbbbbbbbbbbbbbbbbbbbbbbbbb", "tenant-a", owner, "LinuxContainer")

	// Non-member of the configured group: 404 on read, attach and purge.
	sessC, csrfC := loginGroups(t, env, "admin-c", TenantAdminGroup)
	r := doReq(t, env, sessC, csrfC, http.MethodGet, "/v1/data/"+attachRec.ID, "", nil)
	r.Body.Close()
	if r.StatusCode != http.StatusNotFound {
		t.Fatalf("non-member GET foreign record: status=%d, want 404", r.StatusCode)
	}
	r = doReq(t, env, sessC, csrfC, http.MethodPost, "/v1/data/"+attachRec.ID+":attach",
		`{"name":"c-desktop","templateRef":"tpl_linuxdesktop"}`,
		map[string]string{"Idempotency-Key": "key-admingrp-2"})
	r.Body.Close()
	if r.StatusCode != http.StatusNotFound {
		t.Fatalf("non-member attach foreign record: status=%d, want 404", r.StatusCode)
	}
	r = doReq(t, env, sessC, csrfC, http.MethodPost, "/v1/data/"+purgeRec.ID+":purge",
		`{"confirmationNonce":"nonce-anything"}`,
		map[string]string{"Idempotency-Key": "key-admingrp-3"})
	r.Body.Close()
	if r.StatusCode != http.StatusNotFound {
		t.Fatalf("non-member purge foreign record: status=%d, want 404", r.StatusCode)
	}

	// Member: read the foreign record (fresh nonce), attach it, then purge
	// a second foreign record.
	sessA, csrfA := loginGroups(t, env, "admin-a", testCustomAdminGroup)
	r = doReq(t, env, sessA, csrfA, http.MethodGet, "/v1/data/"+attachRec.ID, "", nil)
	rec := decodeBody[retainedDataView](t, r)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("member GET foreign record: status=%d, want 200", r.StatusCode)
	}
	if rec.Owner.Subject != "user-b" {
		t.Fatalf("record owner = %q, want user-b", rec.Owner.Subject)
	}
	r = doReq(t, env, sessA, csrfA, http.MethodPost, "/v1/data/"+attachRec.ID+":attach",
		`{"name":"a-desktop","templateRef":"tpl_linuxdesktop"}`,
		map[string]string{"Idempotency-Key": "key-admingrp-4"})
	r.Body.Close()
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("member attach foreign record: status=%d, want 201", r.StatusCode)
	}

	r = doReq(t, env, sessA, csrfA, http.MethodGet, "/v1/data/"+purgeRec.ID, "", nil)
	rec = decodeBody[retainedDataView](t, r)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("member GET purge record: status=%d, want 200", r.StatusCode)
	}
	r = doReq(t, env, sessA, csrfA, http.MethodPost, "/v1/data/"+purgeRec.ID+":purge",
		`{"confirmationNonce":"`+rec.PurgeConfirmationNonce+`"}`,
		map[string]string{"Idempotency-Key": "key-admingrp-5"})
	r.Body.Close()
	if r.StatusCode != http.StatusAccepted {
		t.Fatalf("member purge foreign record: status=%d, want 202", r.StatusCode)
	}
}

// TestNoRawTenantAdminMembershipCheck: the tenant-admin role must resolve
// through Principal.IsTenantAdmin — a fresh raw membership probe
// (InGroup of the literal/default name) in the api package would bypass
// the configured groups, so the build fails on any reintroduction.
func TestNoRawTenantAdminMembershipCheck(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, pat := range []string{`InGroup(TenantAdminGroup)`, `InGroup("tenant-admin")`} {
			if strings.Contains(string(raw), pat) {
				t.Errorf("%s contains %s — resolve the admin role through Principal.IsTenantAdmin", name, pat)
			}
		}
	}
}
