// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package api

// ADR 0007: sign-out-everywhere. POST /v1/me/sessions:revoke-all destroys
// every portal session of the caller's principal in the caller's tenant —
// the calling session included — via one broker transaction; answers
// mirror logout (204, or 200 {"endSessionUrl"} for RP-initiated logout),
// and a store failure is a 500 that leaves every session intact.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/tinyorbitvn/tinycdi/internal/observability"
)

// fakePrincipalRevoker records the (tenant, issuer, subject) triple the
// handler derived from the caller's session and returns canned counts.
type fakePrincipalRevoker struct {
	calls  []string // "tenant|issuer|subject"
	res    RevokeAllResult
	err    error
	ctxErr error
	bound  bool // observed ctx had a deadline
}

func (f *fakePrincipalRevoker) RevokePrincipalSessions(ctx context.Context, tenantID, issuer, subject string) (RevokeAllResult, error) {
	f.calls = append(f.calls, tenantID+"|"+issuer+"|"+subject)
	_, f.bound = ctx.Deadline()
	f.ctxErr = ctx.Err()
	return f.res, f.err
}

func (e *testEnv) postRevokeAll(t *testing.T, sess *http.Cookie, csrf string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, e.server.URL+"/v1/me/sessions:revoke-all", nil)
	if sess != nil {
		req.AddCookie(sess)
	}
	if csrf != "" {
		req.Header.Set(e.auth.CSRFHeader(), csrf)
	}
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestRevokeAll_UnauthenticatedAndCSRF(t *testing.T) {
	env := newLogoutEnv(t, nil)
	rv := &fakePrincipalRevoker{}
	env.auth.WithPrincipalRevoker(rv)

	// No session cookie: 401, revoker never reached.
	resp := env.postRevokeAll(t, nil, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated = %d, want 401", resp.StatusCode)
	}
	// Session but no CSRF token: 403.
	sess := env.loginSession(t)
	resp = env.postRevokeAll(t, sess, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("no CSRF = %d, want 403", resp.StatusCode)
	}
	if len(rv.calls) != 0 {
		t.Fatalf("revoker reached on rejected requests: %v", rv.calls)
	}
}

// TestRevokeAll_RevokesEverySessionOfPrincipal: the handler hands the
// revoker the caller's (tenant, issuer, subject) — the caller's own
// session included by construction — expires the portal cookie, audits
// session.revoke_all with the per-kind counts, and answers the same
// endSessionUrl logout would.
func TestRevokeAll_RevokesEverySessionOfPrincipal(t *testing.T) {
	var buf bytes.Buffer
	env := newLogoutEnv(t, nil)
	rv := &fakePrincipalRevoker{res: RevokeAllResult{Sessions: 3, Tickets: 1, Leases: 2}}
	env.auth.WithPrincipalRevoker(rv).WithAuditSink(observability.NewJSONSink(&buf))
	sess := env.loginSession(t)

	resp := env.postRevokeAll(t, sess, csrfTokenFor(sess.Value))
	got := decodeEndSession(t, resp)

	want := env.issuer.TenantID + "|" + env.issuer.URL() + "|" + env.issuer.Subject
	if len(rv.calls) != 1 || rv.calls[0] != want {
		t.Fatalf("revocations = %v, want exactly [%q]", rv.calls, want)
	}
	if !rv.bound {
		t.Fatal("revocation ctx lost the timeout bound")
	}
	u, _ := url.Parse(got)
	if q := u.Query(); q.Get("id_token_hint") != env.issuer.LastIDToken() {
		t.Fatalf("id_token_hint = %q, want the login ID token", q.Get("id_token_hint"))
	}
	// The caller's cookie dies with the response — their session row was
	// part of the same transaction.
	cleared := false
	for _, h := range resp.Header.Values("Set-Cookie") {
		if strings.HasPrefix(h, env.auth.SessionCookieName()+"=") && strings.Contains(h, "Max-Age=0") {
			cleared = true
		}
	}
	if !cleared {
		t.Fatal("session cookie not cleared")
	}
	out := buf.String()
	for _, frag := range []string{
		`"action":"session.revoke_all"`, `"outcome":"success"`,
		`"counts":"sessions=3,tickets=1,leases=2"`,
	} {
		if !strings.Contains(out, frag) {
			t.Fatalf("audit event missing %s: %s", frag, out)
		}
	}
	if strings.Contains(out, sess.Value) {
		t.Fatal("audit event leaked the session ID")
	}
}

// TestRevokeAll_204WithoutEndSession: no end_session_endpoint advertised →
// plain 204, same as logout.
func TestRevokeAll_204WithoutEndSession(t *testing.T) {
	env := newTestEnv(t, nil)
	env.auth.WithPrincipalRevoker(&fakePrincipalRevoker{res: RevokeAllResult{Sessions: 1}})
	sess := env.loginSession(t)
	resp := env.postRevokeAll(t, sess, csrfTokenFor(sess.Value))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 204 (%s)", resp.StatusCode, b)
	}
}

// TestRevokeAll_StoreFailureIs500AndKeepsSession: the transaction is the
// operation — a failed revoke rolls back whole, so the caller's session
// and cookie must NOT be destroyed and the failure is audited, never
// hidden behind a 204.
func TestRevokeAll_StoreFailureIs500AndKeepsSession(t *testing.T) {
	var buf bytes.Buffer
	env := newTestEnv(t, nil)
	rv := &fakePrincipalRevoker{err: errors.New("lease store down")}
	env.auth.WithPrincipalRevoker(rv).WithAuditSink(observability.NewJSONSink(&buf))
	sess := env.loginSession(t)

	resp := env.postRevokeAll(t, sess, csrfTokenFor(sess.Value))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
	for _, h := range resp.Header.Values("Set-Cookie") {
		if strings.Contains(h, "Max-Age=0") {
			t.Fatal("session cookie cleared despite failed revoke")
		}
	}
	// The caller's session is still valid — nothing was destroyed.
	r := env.authedGet(t, sess, "/v1/me")
	r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("session gone after failed revoke-all: %d", r.StatusCode)
	}
	out := buf.String()
	if !strings.Contains(out, `"action":"session.revoke_all"`) ||
		!strings.Contains(out, `"outcome":"failure"`) {
		t.Fatalf("missing failure audit event: %s", out)
	}
}

// TestRevokeAll_NoRevokerIs503: without a store-level revoker the endpoint
// refuses rather than claiming a sign-out that never happened.
func TestRevokeAll_NoRevokerIs503(t *testing.T) {
	env := newTestEnv(t, nil)
	sess := env.loginSession(t)
	resp := env.postRevokeAll(t, sess, csrfTokenFor(sess.Value))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	r := env.authedGet(t, sess, "/v1/me")
	r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("session destroyed with no revoker: %d", r.StatusCode)
	}
}

// txPrincipalRevoker mirrors what the wired broker revoker actually does:
// on success the principal's session rows are gone for real (the in-memory
// store stands in for the transaction), on error nothing changes.
type txPrincipalRevoker struct {
	fakePrincipalRevoker
	store *InMemorySessionStore
}

func (f *txPrincipalRevoker) RevokePrincipalSessions(ctx context.Context, tenantID, issuer, subject string) (RevokeAllResult, error) {
	res, err := f.fakePrincipalRevoker.RevokePrincipalSessions(ctx, tenantID, issuer, subject)
	if err != nil {
		return res, err
	}
	f.store.mu.Lock()
	defer f.store.mu.Unlock()
	for key, s := range f.store.sessions {
		if s.Principal.Issuer == issuer && s.Principal.Subject == subject && s.Principal.TenantID == tenantID {
			delete(f.store.sessions, key)
			res.Sessions++
		}
	}
	return res, nil
}

// TestRevokeAll_StoreFailureThenRetrySignsOut: a revoke-all that fails
// answers a non-success status and destroys nothing — the caller stays
// signed in and retries. Once the store recovers, the same call commits:
// the caller's session no longer validates and the cookie expires.
func TestRevokeAll_StoreFailureThenRetrySignsOut(t *testing.T) {
	env := newTestEnv(t, nil)
	rv := &txPrincipalRevoker{fakePrincipalRevoker: fakePrincipalRevoker{err: errors.New("lease store down")}, store: env.store}
	env.auth.WithPrincipalRevoker(rv)
	sess := env.loginSession(t)

	resp := env.postRevokeAll(t, sess, csrfTokenFor(sess.Value))
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusNoContent {
		t.Fatalf("failed revocation answered success: %d", resp.StatusCode)
	}
	for _, h := range resp.Header.Values("Set-Cookie") {
		if strings.Contains(h, "Max-Age=0") {
			t.Fatal("session cookie cleared despite failed revoke")
		}
	}
	r := env.authedGet(t, sess, "/v1/me")
	r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("session gone after failed revoke-all: %d", r.StatusCode)
	}

	// The store recovers: a retry commits — the caller's session no
	// longer validates and the cookie dies with the response.
	rv.err = nil
	resp = env.postRevokeAll(t, sess, csrfTokenFor(sess.Value))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("retry status = %d, want 204", resp.StatusCode)
	}
	r = env.authedGet(t, sess, "/v1/me")
	r.Body.Close()
	if r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("session still valid after retried revoke-all: %d", r.StatusCode)
	}
}

// TestRevokeAll_RevokeSurvivesClientDisconnect: a client that disconnects
// mid-call must not abort the revocation — the revoker runs on a detached,
// bounded context.
func TestRevokeAll_RevokeSurvivesClientDisconnect(t *testing.T) {
	env := newTestEnv(t, nil)
	rv := &fakePrincipalRevoker{res: RevokeAllResult{Sessions: 1}}
	env.auth.WithPrincipalRevoker(rv)
	sess := env.loginSession(t)

	reqCtx, cancel := context.WithCancel(context.Background())
	cancel() // already-dead request ctx: the revoke must still run
	req := httptest.NewRequest(http.MethodPost, "/auth/revoke-all", nil).WithContext(reqCtx)
	req.AddCookie(sess)
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyPrincipal, Principal{
		Issuer: env.issuer.URL(), Subject: env.issuer.Subject, TenantID: env.issuer.TenantID,
	}))
	rec := httptest.NewRecorder()
	s := &Session{ID: sess.Value, Principal: Principal{
		Issuer: env.issuer.URL(), Subject: env.issuer.Subject, TenantID: env.issuer.TenantID,
	}}
	env.auth.RevokeAllSessionsHandler(rec,
		req.WithContext(context.WithValue(req.Context(), ctxKeySession, s)))

	if len(rv.calls) != 1 {
		t.Fatalf("revocations = %v, want 1", rv.calls)
	}
	if rv.ctxErr != nil {
		t.Fatalf("revocation ran on a cancelled ctx: %v", rv.ctxErr)
	}
	if !rv.bound {
		t.Fatal("revocation ctx lost the timeout bound")
	}
}
