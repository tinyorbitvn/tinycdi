// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package api

// FX-R21: sign-out. POST /v1/logout destroys the portal session and, when
// the identity provider advertises end_session_endpoint (and oidc.endSession
// is on), answers 200 {"endSessionUrl"} so the portal can end the provider
// session too; otherwise 204. The URL is assembled only from discovery and
// configuration — never from the request.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/api/oidctest"
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
	// The session keeps no ID token, so there is no id_token_hint; and
	// without oidc.postLogoutRedirect there is no post_logout_redirect_uri.
	for _, k := range []string{"id_token_hint", "post_logout_redirect_uri"} {
		if q.Has(k) {
			t.Fatalf("%s present in %q", k, got)
		}
	}
	if len(q) != 1 {
		t.Fatalf("unexpected query parameters in %q", got)
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
	if got != baseline {
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
	for _, redirect := range []string{"/signed-out", "javascript:alert(1)", "portal.test/signed-out", "ftp://portal.test/x"} {
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

// A discovered endpoint that is not an absolute http(s) URL is not trusted:
// sign-out degrades to the plain 204 instead of handing the browser an
// attacker-shaped navigation target.
func TestLogout_UntrustedDiscoveredEndpointIs204(t *testing.T) {
	for _, endpoint := range []string{"javascript:alert(1)", "/logout", "//evil.example/logout", "https://user:pw@idp.example/logout"} {
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
