// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package api

// FX-R21: sign-out. POST /v1/logout destroys the portal session and, when
// the identity provider advertises end_session_endpoint (and oidc.endSession
// is on), answers 200 {"endSessionUrl"} so the portal can end the provider
// session too; otherwise 204. The URL is assembled only from discovery and
// configuration — never from the request.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
)

const testEndSessionPath = "/protocol/openid-connect/logout"

// newLogoutEnv starts a test env whose issuer advertises an end-session
// endpoint (set before discovery runs).
func newLogoutEnv(t *testing.T, mutate func(*AuthConfig)) *testEnv {
	t.Helper()
	return newTestEnvIssuer(t, func(i *oidctest.Issuer) {
		i.EndSessionEndpoint = i.URL() + testEndSessionPath
	}, func(c *AuthConfig) {
		c.EndSession = true
		if mutate != nil {
			mutate(c)
		}
	})
}

func (e *testEnv) postLogout(t *testing.T, sess *http.Cookie, csrf string, mutate func(*http.Request)) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, e.server.URL+"/auth/logout", nil)
	if sess != nil {
		req.AddCookie(sess)
	}
	if csrf != "" {
		req.Header.Set(e.auth.CSRFHeader(), csrf)
	}
	if mutate != nil {
		mutate(req)
	}
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func (e *testEnv) loginSession(t *testing.T) *http.Cookie {
	t.Helper()
	resp, cookies := e.login(t)
	resp.Body.Close()
	sess := findCookie(cookies, e.auth.SessionCookieName())
	if sess == nil {
		t.Fatal("login set no session cookie")
	}
	return sess
}

func decodeEndSession(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("logout status = %d, want 200 (%s)", resp.StatusCode, b)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Fatalf("Cache-Control = %q, want no-store", cc)
	}
	var body struct {
		EndSessionURL string `json:"endSessionUrl"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	return body.EndSessionURL
}

func TestLogout_ClearsSessionAndReturnsEndSessionURL(t *testing.T) {
	env := newLogoutEnv(t, nil)
	sess := env.loginSession(t)

	resp := env.postLogout(t, sess, csrfTokenFor(sess.Value), nil)
	setCookies := resp.Header.Values("Set-Cookie")
	got := decodeEndSession(t, resp)
	assertPortalCookies(t, setCookies)

	// The session cookie is deleted and the server-side session is gone.
	cleared := false
	for _, h := range setCookies {
		if strings.HasPrefix(h, env.auth.SessionCookieName()+"=") && strings.Contains(h, "Max-Age=0") {
			cleared = true
		}
	}
	if !cleared {
		t.Fatalf("session cookie not cleared: %v", setCookies)
	}
	r := env.authedGet(t, sess, "/v1/me")
	r.Body.Close()
	if r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("session valid after logout: %d", r.StatusCode)
	}

	u, err := url.Parse(got)
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme+"://"+u.Host+u.Path != env.issuer.URL()+testEndSessionPath {
		t.Fatalf("end-session URL = %q, want the discovery endpoint %q", got, env.issuer.URL()+testEndSessionPath)
	}
	q := u.Query()
	if q.Get("client_id") != env.issuer.ClientID {
		t.Fatalf("client_id = %q, want %q", q.Get("client_id"), env.issuer.ClientID)
	}
	// The session retains the ID token, so id_token_hint is exactly the
	// token the issuer signed at login — that is what makes the provider
	// skip its own confirmation page (V3.24).
	if q.Get("id_token_hint") != env.issuer.LastIDToken() {
		t.Fatalf("id_token_hint = %q, want the login ID token", q.Get("id_token_hint"))
	}
	// Without oidc.postLogoutRedirect there is no post_logout_redirect_uri.
	if q.Has("post_logout_redirect_uri") {
		t.Fatalf("post_logout_redirect_uri present in %q", got)
	}
	if len(q) != 2 {
		t.Fatalf("unexpected query parameters in %q", got)
	}
}

// A session without a retained ID token (pre-migration row, or one whose
// seal no longer opens) still signs out: client_id-only, no hint — the
// logout must never fail over the hint (V3.24).
func TestLogout_NoIDTokenFallsBackToClientID(t *testing.T) {
	env := newLogoutEnv(t, nil)
	sess := env.loginSession(t)
	// Wipe the retained token, like a legacy session row.
	if rec, err := env.auth.sessions.Peek(context.Background(), sess.Value); err == nil {
		rec.IDToken = ""
		if err := env.auth.sessions.Save(context.Background(), rec); err != nil {
			t.Fatal(err)
		}
	}
	got := decodeEndSession(t, env.postLogout(t, sess, csrfTokenFor(sess.Value), nil))
	u, _ := url.Parse(got)
	if u.Query().Has("id_token_hint") {
		t.Fatalf("id_token_hint present without a stored token in %q", got)
	}
	if u.Query().Get("client_id") != env.issuer.ClientID {
		t.Fatalf("client_id missing in %q", got)
	}
}

func TestLogout_PostLogoutRedirectOnlyWhenConfigured(t *testing.T) {
	env := newLogoutEnv(t, func(c *AuthConfig) {
		c.PostLogoutRedirect = "https://portal.test/signed-out"
	})
	sess := env.loginSession(t)
	got := decodeEndSession(t, env.postLogout(t, sess, csrfTokenFor(sess.Value), nil))
	u, _ := url.Parse(got)
	if v := u.Query().Get("post_logout_redirect_uri"); v != "https://portal.test/signed-out" {
		t.Fatalf("post_logout_redirect_uri = %q", v)
	}
	if u.Query().Get("client_id") != env.issuer.ClientID {
		t.Fatalf("client_id missing in %q", got)
	}
}

func TestLogout_KeepsEndpointQuery(t *testing.T) {
	env := newTestEnvIssuer(t, func(i *oidctest.Issuer) {
		i.EndSessionEndpoint = i.URL() + testEndSessionPath + "?ui_locales=en"
	}, func(c *AuthConfig) { c.EndSession = true })
	sess := env.loginSession(t)
	got := decodeEndSession(t, env.postLogout(t, sess, csrfTokenFor(sess.Value), nil))
	u, _ := url.Parse(got)
	if u.Query().Get("ui_locales") != "en" || u.Query().Get("client_id") == "" {
		t.Fatalf("end-session URL = %q", got)
	}
}

func TestLogout_NoEndSessionEndpointIs204(t *testing.T) {
	// EndSession is on but the provider does not advertise the endpoint.
	env := newTestEnv(t, func(c *AuthConfig) { c.EndSession = true })
	sess := env.loginSession(t)
	resp := env.postLogout(t, sess, csrfTokenFor(sess.Value), nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("logout status = %d, want 204", resp.StatusCode)
	}
	r := env.authedGet(t, sess, "/v1/me")
	r.Body.Close()
	if r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("session valid after logout: %d", r.StatusCode)
	}
}

func TestLogout_EndSessionDisabledIs204(t *testing.T) {
	// The provider advertises the endpoint but oidc.endSession=false.
	env := newLogoutEnv(t, func(c *AuthConfig) { c.EndSession = false })
	sess := env.loginSession(t)
	resp := env.postLogout(t, sess, csrfTokenFor(sess.Value), nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("logout status = %d, want 204", resp.StatusCode)
	}
	if b, _ := io.ReadAll(resp.Body); len(b) != 0 {
		t.Fatalf("204 carries a body: %q", b)
	}
}

// The end-session target comes only from discovery and configuration: no
// query parameter, header, form field or Host spoof can change it.
func TestLogout_NoOpenRedirect(t *testing.T) {
	env := newLogoutEnv(t, func(c *AuthConfig) {
		c.PostLogoutRedirect = "https://portal.test/signed-out"
	})
	baseline := func() string {
		sess := env.loginSession(t)
		return decodeEndSession(t, env.postLogout(t, sess, csrfTokenFor(sess.Value), nil))
	}()

	sess := env.loginSession(t)
	resp := env.postLogout(t, sess, csrfTokenFor(sess.Value), func(r *http.Request) {
		r.URL.RawQuery = url.Values{
			"redirect":                 {"https://evil.example/"},
			"returnTo":                 {"https://evil.example/"},
			"post_logout_redirect_uri": {"https://evil.example/"},
			"end_session_endpoint":     {"https://evil.example/logout"},
		}.Encode()
		r.Header.Set("Referer", "https://evil.example/")
		r.Header.Set("Origin", "https://evil.example")
		r.Header.Set("X-Forwarded-Host", "evil.example")
		r.Header.Set("X-Forwarded-Proto", "https")
		r.Host = "evil.example"
		r.Header.Set("Content-Type", "application/json")
		r.Body = io.NopCloser(strings.NewReader(`{"endSessionUrl":"https://evil.example/","post_logout_redirect_uri":"https://evil.example/"}`))
	})
	got := decodeEndSession(t, resp)
	// The id_token_hint legitimately differs between the two logins —
	// compare the URLs with it stripped.
	strip := func(raw string) string {
		u, _ := url.Parse(raw)
		q := u.Query()
		q.Del("id_token_hint")
		u.RawQuery = q.Encode()
		return u.String()
	}
	if strip(got) != strip(baseline) {
		t.Fatalf("request input changed the end-session URL:\n got  %q\n want %q", got, baseline)
	}
	if strings.Contains(got, "evil.example") {
		t.Fatalf("attacker value reached the end-session URL: %q", got)
	}
}

func TestLogout_RequiresCSRFAndSession(t *testing.T) {
	env := newLogoutEnv(t, nil)
	sess := env.loginSession(t)

	for name, csrf := range map[string]string{
		"missing": "",
		"wrong":   csrfTokenFor("another-session"),
	} {
		resp := env.postLogout(t, sess, csrf, nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s CSRF token: status = %d, want 403", name, resp.StatusCode)
		}
	}
	// A refused logout leaves the session alive.
	r := env.authedGet(t, sess, "/v1/me")
	r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("session killed by a CSRF-refused logout: %d", r.StatusCode)
	}

	// No session at all: 401, never an end-session URL.
	resp := env.postLogout(t, nil, "", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous logout: status = %d, want 401", resp.StatusCode)
	}
	// And a GET is not a logout (it would be CSRF-able).
	g, err := noRedirectClient().Get(env.server.URL + "/auth/logout")
	if err != nil {
		t.Fatal(err)
	}
	g.Body.Close()
	if g.StatusCode == http.StatusOK || g.StatusCode == http.StatusNoContent {
		t.Fatalf("GET /logout answered %d", g.StatusCode)
	}
}

func TestNewAuthenticator_RejectsBadPostLogoutRedirect(t *testing.T) {
	for _, redirect := range []string{
		"/signed-out",
		"javascript:alert(1)",
		"portal.test/signed-out",
		"ftp://portal.test/x",
		// Plain http is never a legitimate post-logout hop off-loopback —
		// the browser would follow it over cleartext. Rejected outright,
		// not downgraded.
		"http://portal.test/signed-out",
		"http://192.168.1.10/signed-out",
		// Hostnames that only look loopback-adjacent are not loopback.
		"http://localhost.evil.test/x",
		"http://127.0.0.1.evil.test/x",
		"http://2130706433/x", // dotted-quad decimal for 127.0.0.1 — strict parser rejects it
		// Credentials in the URL are rejected on any scheme.
		"https://user:pw@portal.test/x",
	} {
		iss, err := oidctest.NewIssuer()
		if err != nil {
			t.Fatal(err)
		}
		defer iss.Close()
		_, err = NewAuthenticator(context.Background(), AuthConfig{
			Issuer: iss.URL(), ClientID: iss.ClientID,
			RedirectURL:        "https://portal.test/auth/callback",
			LoginSealer:        testLoginSealer(t),
			EndSession:         true,
			PostLogoutRedirect: redirect,
		}, NewInMemorySessionStore(time.Minute), slog.Default())
		if err == nil {
			t.Fatalf("NewAuthenticator accepted PostLogoutRedirect %q", redirect)
		}
	}
}

// http stays acceptable on loopback only: the dev/test IdP convention is a
// plain-http issuer on loopback (oidctest, a dev Keycloak), so
// "http://localhost…" and "http://127.0.0.1…" redirect targets are dev
// reality — anything off-loopback must be https.
func TestNewAuthenticator_AllowsLoopbackPostLogoutRedirect(t *testing.T) {
	for _, redirect := range []string{
		"https://portal.test/signed-out",
		"http://localhost/signed-out",
		"http://localhost:8080/signed-out",
		"http://127.0.0.1:8080/signed-out",
		"http://[::1]:8080/signed-out",
	} {
		iss, err := oidctest.NewIssuer()
		if err != nil {
			t.Fatal(err)
		}
		_, err = NewAuthenticator(context.Background(), AuthConfig{
			Issuer: iss.URL(), ClientID: iss.ClientID,
			RedirectURL:        "https://portal.test/auth/callback",
			LoginSealer:        testLoginSealer(t),
			EndSession:         true,
			PostLogoutRedirect: redirect,
		}, NewInMemorySessionStore(time.Minute), slog.Default())
		iss.Close()
		if err != nil {
			t.Fatalf("NewAuthenticator rejected PostLogoutRedirect %q: %v", redirect, err)
		}
	}
}

// A discovered endpoint that is not an absolute https URL (or http on a
// loopback host) is not trusted: sign-out degrades to the plain 204 instead
// of handing the browser an attacker-shaped navigation target.
func TestLogout_UntrustedDiscoveredEndpointIs204(t *testing.T) {
	for _, endpoint := range []string{
		"javascript:alert(1)",
		"/logout",
		"//evil.example/logout",
		"https://user:pw@idp.example/logout",
		// Plain http off-loopback is not a sign-out target either — a
		// compromised or misconfigured discovery document cannot downgrade
		// the browser to cleartext.
		"http://idp.example/logout",
		"http://localhost.evil.example/logout",
	} {
		env := newTestEnvIssuer(t, func(i *oidctest.Issuer) { i.EndSessionEndpoint = endpoint },
			func(c *AuthConfig) { c.EndSession = true })
		sess := env.loginSession(t)
		resp := env.postLogout(t, sess, csrfTokenFor(sess.Value), nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("endpoint %q: logout status = %d, want 204", endpoint, resp.StatusCode)
		}
	}
}

func TestOpenAPIContract_LogoutResult(t *testing.T) {
	requireValid(t, "LogoutResult", LogoutResult{EndSessionURL: "https://idp.example/logout?client_id=tinycdi-portal"})
}

// --- S17: sign-out ends the session's reach over the desktop layer ------

// fakeSessionRevoker records the portal session IDs sign-out asked it to
// revoke and replays a scripted count/error.
type fakeSessionRevoker struct {
	calls []string
	n     int
	err   error
}

func (f *fakeSessionRevoker) RevokePortalSession(_ context.Context, id string) (int, error) {
	f.calls = append(f.calls, id)
	return f.n, f.err
}

// ctxProbingRevoker behaves like the real store implementation: when the
// ctx it is handed is dead the revocation transaction cannot commit, so it
// records the observed ctx error and returns it.
type ctxProbingRevoker struct {
	fakeSessionRevoker
	ctxErr  error
	bounded bool
}

func (f *ctxProbingRevoker) RevokePortalSession(ctx context.Context, id string) (int, error) {
	f.calls = append(f.calls, id)
	_, f.bounded = ctx.Deadline()
	if err := ctx.Err(); err != nil {
		f.ctxErr = err
		return 0, err
	}
	return f.n, f.err
}

// disconnectOnDeleteStore cancels the request's context the moment the
// session row is deleted — a client disconnect landing mid-logout, after
// the handler has started but before the revocation runs.
type disconnectOnDeleteStore struct {
	SessionStore
	cancel context.CancelFunc
}

func (s *disconnectOnDeleteStore) Delete(ctx context.Context, id string) error {
	err := s.SessionStore.Delete(ctx, id)
	s.cancel()
	return err
}

// TestLogout_RevokeSurvivesClientDisconnect: a client that disconnects
// mid-logout must not abort the revocation — the revoker runs on a context
// detached from the request's cancellation (still bounded by the 5 s
// revoke timeout), so the session's leases and tickets die even though the
// response can no longer be delivered.
func TestLogout_RevokeSurvivesClientDisconnect(t *testing.T) {
	env := newTestEnv(t, nil) // no end-session endpoint: the 204 path
	rv := &ctxProbingRevoker{fakeSessionRevoker: fakeSessionRevoker{n: 1}}
	env.auth.WithSessionRevoker(rv)
	sess := env.loginSession(t)

	reqCtx, cancel := context.WithCancel(context.Background())
	env.auth.sessions = &disconnectOnDeleteStore{SessionStore: env.store, cancel: cancel}

	req := httptest.NewRequest(http.MethodPost, "/auth/logout", nil).WithContext(reqCtx)
	req.AddCookie(sess)
	rec := httptest.NewRecorder()
	env.auth.LogoutHandler(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("logout status = %d, want 204", rec.Code)
	}
	if len(rv.calls) != 1 || rv.calls[0] != sess.Value {
		t.Fatalf("revocations = %v, want exactly [%q]", rv.calls, sess.Value)
	}
	if rv.ctxErr != nil {
		t.Fatalf("revocation ran on a cancelled ctx: %v — client disconnect aborted it", rv.ctxErr)
	}
	if !rv.bounded {
		t.Fatal("revocation ctx lost the 5 s timeout bound")
	}
}

// TestLogout_RevokesSessionBoundMaterial: sign-out revokes the leases and
// outstanding tickets bound to THIS session's credential digest — the
// revoker sees the session's own ID — and the action lands in the audit
// stream as session.revoke (the gateway's action name) with the lease
// count, never the session material.
func TestLogout_RevokesSessionBoundMaterial(t *testing.T) {
	var buf bytes.Buffer
	env := newLogoutEnv(t, nil)
	rv := &fakeSessionRevoker{n: 2}
	env.auth.WithSessionRevoker(rv).WithAuditSink(observability.NewJSONSink(&buf))
	sess := env.loginSession(t)

	resp := env.postLogout(t, sess, csrfTokenFor(sess.Value), nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("logout status = %d, want 200", resp.StatusCode)
	}
	if len(rv.calls) != 1 || rv.calls[0] != sess.Value {
		t.Fatalf("revocations = %v, want exactly [%q]", rv.calls, sess.Value)
	}
	out := buf.String()
	if !strings.Contains(out, `"action":"session.revoke"`) ||
		!strings.Contains(out, `"outcome":"success"`) ||
		!strings.Contains(out, `"leases_revoked":"2"`) {
		t.Fatalf("missing session.revoke audit event: %s", out)
	}
	if strings.Contains(out, sess.Value) {
		t.Fatal("audit event leaked the session ID")
	}
}

// TestLogout_RevokeFailureStillSignsOut: a lease-store failure must never
// keep the portal session or its cookie — it is logged, counted and
// audited, and the sign-out still completes.
func TestLogout_RevokeFailureStillSignsOut(t *testing.T) {
	var buf bytes.Buffer
	env := newTestEnv(t, nil) // no end-session endpoint: the 204 path
	rv := &fakeSessionRevoker{err: errors.New("lease store down")}
	env.auth.WithSessionRevoker(rv).WithAuditSink(observability.NewJSONSink(&buf))
	sess := env.loginSession(t)

	resp := env.postLogout(t, sess, csrfTokenFor(sess.Value), nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("logout status = %d, want 204", resp.StatusCode)
	}
	cleared := false
	for _, h := range resp.Header.Values("Set-Cookie") {
		if strings.HasPrefix(h, env.auth.SessionCookieName()+"=") && strings.Contains(h, "Max-Age=0") {
			cleared = true
		}
	}
	if !cleared {
		t.Fatal("session cookie not cleared on revoke failure")
	}
	r := env.authedGet(t, sess, "/v1/me")
	r.Body.Close()
	if r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("session valid after logout: %d", r.StatusCode)
	}
	if out := buf.String(); !strings.Contains(out, `"action":"session.revoke"`) ||
		!strings.Contains(out, `"outcome":"failure"`) {
		t.Fatalf("missing failure audit event: %s", out)
	}
}

// TestLogout_NoRevokerKeepsSignOut: with no revoker wired the handler is
// exactly the portal-session destroy it always was.
func TestLogout_NoRevokerKeepsSignOut(t *testing.T) {
	env := newTestEnv(t, nil)
	sess := env.loginSession(t)
	resp := env.postLogout(t, sess, csrfTokenFor(sess.Value), nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("logout status = %d, want 204", resp.StatusCode)
	}
}
