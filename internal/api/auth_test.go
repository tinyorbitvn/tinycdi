package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/api/loginstate"
	"github.com/tinyorbitvn/tinycdi/internal/api/oidctest"
	"github.com/tinyorbitvn/tinycdi/internal/observability"
)

// testLoginKeys is the login-state keyring every test Authenticator shares;
// cross-replica tests build a second Authenticator from the same keys.
var testLoginKeys = [][]byte{bytes.Repeat([]byte{0x1a}, 32)}

func testLoginSealer(t *testing.T) *loginstate.Sealer {
	t.Helper()
	s, err := loginstate.NewSealer(testLoginKeys...)
	if err != nil {
		t.Fatalf("loginstate.NewSealer: %v", err)
	}
	return s
}

type testEnv struct {
	issuer *oidctest.Issuer
	auth   *Authenticator
	store  *InMemorySessionStore
	server *httptest.Server
	logs   *bytes.Buffer
}

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newTestEnv(t *testing.T, mutate func(*AuthConfig)) *testEnv {
	return newTestEnvOpts(t, mutate, nil, nil)
}

// newTestEnvOpts additionally accepts an observability AuditSink and Metrics
// (nil = omit) wired around the mux as
// RequestID → AuditWithSink → InstrumentHTTP → mux.
func newTestEnvOpts(t *testing.T, mutate func(*AuthConfig), sink observability.AuditSink, metrics *observability.Metrics) *testEnv {
	return newTestEnvFull(t, nil, mutate, sink, metrics)
}

// newTestEnvIssuer configures the fake issuer (before discovery runs) as
// well as the AuthConfig.
func newTestEnvIssuer(t *testing.T, issuer func(*oidctest.Issuer), mutate func(*AuthConfig)) *testEnv {
	return newTestEnvFull(t, issuer, mutate, nil, nil)
}

func newTestEnvFull(t *testing.T, issuer func(*oidctest.Issuer), mutate func(*AuthConfig), sink observability.AuditSink, metrics *observability.Metrics) *testEnv {
	t.Helper()
	iss, err := oidctest.NewIssuer()
	if err != nil {
		t.Fatalf("oidctest.NewIssuer: %v", err)
	}
	if issuer != nil {
		issuer(iss)
	}
	logBuf := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(logBuf, nil))
	store := NewInMemorySessionStore(30 * time.Minute)
	cfg := AuthConfig{
		Issuer:      iss.URL(),
		ClientID:    iss.ClientID,
		RedirectURL: "https://portal.test/auth/callback",
		LoginSealer: testLoginSealer(t),
	}
	if mutate != nil {
		mutate(&cfg)
	}
	a, err := NewAuthenticator(context.Background(), cfg, store, logger)
	if err != nil {
		t.Fatalf("NewAuthenticator: %v", err)
	}
	a.WithMetrics(metrics)
	mux := http.NewServeMux()
	mux.Handle("/auth/login", http.HandlerFunc(a.LoginHandler))
	mux.Handle("/auth/callback", http.HandlerFunc(a.CallbackHandler))
	mux.Handle("/auth/logout", a.RequireAuth(a.RequireCSRF(http.HandlerFunc(a.LogoutHandler))))
	mux.Handle("/auth/revoke-all", a.RequireAuth(a.RequireCSRF(http.HandlerFunc(a.RevokeAllSessionsHandler))))
	MountMeRoutes(mux, a, NewMeHandler(testSessionDomain))
	MountSessionProbeRoute(mux, a)
	mux.Handle("/v1/echo-owner", a.RequireAuth(a.RequireCSRF(http.HandlerFunc(echoOwnerHandler))))

	var h http.Handler = mux
	if metrics != nil {
		h = InstrumentHTTP(metrics, "app")(h)
	}
	srv := httptest.NewServer(RequestID(AuditWithSink(logger, sink)(h)))
	env := &testEnv{issuer: iss, auth: a, store: store, server: srv, logs: logBuf}
	t.Cleanup(func() { srv.Close(); iss.Close() })
	return env
}

// spawnReplica builds a second Authenticator over the same fake issuer and
// the same login-state keyring as e, but with its own session store and HTTP
// server — a stand-in for another backend replica.
func (e *testEnv) spawnReplica(t *testing.T) *testEnv {
	t.Helper()
	a, err := NewAuthenticator(context.Background(), AuthConfig{
		Issuer:      e.issuer.URL(),
		ClientID:    e.issuer.ClientID,
		RedirectURL: "https://portal.test/auth/callback",
		LoginSealer: testLoginSealer(t),
	}, NewInMemorySessionStore(30*time.Minute), slog.Default())
	if err != nil {
		t.Fatalf("replica NewAuthenticator: %v", err)
	}
	mux := http.NewServeMux()
	mux.Handle("/auth/login", http.HandlerFunc(a.LoginHandler))
	mux.Handle("/auth/callback", http.HandlerFunc(a.CallbackHandler))
	MountMeRoutes(mux, a, NewMeHandler(testSessionDomain))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &testEnv{issuer: e.issuer, auth: a, server: srv}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func echoOwnerHandler(w http.ResponseWriter, r *http.Request) {
	p, _ := PrincipalFromContext(r.Context())
	var body struct {
		Owner string `json:"owner"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	writeJSON(w, map[string]any{"owner": p.Owner(), "body_owner": body.Owner})
}

func noRedirectClient() *http.Client {
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
}

// login drives the full OIDC round trip and returns the callback response
// plus all cookies it set. The browser-bound login cookie set by
// /auth/login is carried to the callback, like a real browser.
func (e *testEnv) login(t *testing.T, extraCookies ...*http.Cookie) (*http.Response, []*http.Cookie) {
	t.Helper()
	client := noRedirectClient()

	req, _ := http.NewRequest(http.MethodGet, e.server.URL+"/auth/login", nil)
	for _, c := range extraCookies {
		req.AddCookie(c)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET /auth/login: %v", err)
	}
	loc := resp.Header.Get("Location")
	loginCookie := findCookie(resp.Cookies(), e.auth.LoginCookieName())
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound || loc == "" {
		t.Fatalf("login: status=%d location=%q", resp.StatusCode, loc)
	}
	if !strings.HasPrefix(loc, e.issuer.URL()+"/authorize") {
		t.Fatalf("login did not redirect to issuer authorize: %q", loc)
	}
	if loginCookie == nil {
		t.Fatal("login did not set the browser-bound login cookie")
	}

	resp2, err := client.Get(loc)
	if err != nil {
		t.Fatalf("GET authorize: %v", err)
	}
	cbLoc := resp2.Header.Get("Location")
	resp2.Body.Close()
	cb, err := url.Parse(cbLoc)
	if err != nil || cb.Query().Get("code") == "" || cb.Query().Get("state") == "" {
		t.Fatalf("authorize redirect malformed: %q err=%v", cbLoc, err)
	}

	// The browser carries the login cookie and any pre-existing session
	// cookie to the callback — this is where fixation deletion must happen.
	req3, _ := http.NewRequest(http.MethodGet, e.server.URL+"/auth/callback?"+cb.Query().Encode(), nil)
	req3.AddCookie(loginCookie)
	for _, c := range extraCookies {
		req3.AddCookie(c)
	}
	resp3, err := client.Do(req3)
	if err != nil {
		t.Fatalf("GET callback: %v", err)
	}
	return resp3, resp3.Cookies()
}

func findCookie(cookies []*http.Cookie, name string) *http.Cookie {
	for _, c := range cookies {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func (e *testEnv) authedGet(t *testing.T, session *http.Cookie, path string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, e.server.URL+path, nil)
	if session != nil {
		req.AddCookie(session)
	}
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	return resp
}

func decodeError(t *testing.T, resp *http.Response) (code string) {
	t.Helper()
	var body Error
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	return string(body.Code)
}

func TestOIDCLoginFlowSucceeds(t *testing.T) {
	env := newTestEnv(t, nil)
	resp, cookies := env.login(t)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusFound {
		t.Fatalf("callback status = %d, want 302", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/" {
		t.Fatalf("post-login redirect = %q, want /", loc)
	}

	sess := findCookie(cookies, env.auth.SessionCookieName())
	if sess == nil {
		t.Fatal("no session cookie set")
	}
	if !sess.HttpOnly || !sess.Secure {
		t.Fatalf("session cookie flags wrong: HttpOnly=%v Secure=%v", sess.HttpOnly, sess.Secure)
	}
	if sess.Domain != "" {
		t.Fatalf("session cookie must be host-only, got Domain=%q", sess.Domain)
	}
	if sess.SameSite != http.SameSiteLaxMode {
		t.Fatalf("session cookie SameSite = %v, want Lax", sess.SameSite)
	}
	if sess.Path != "/" {
		t.Fatalf("session cookie Path = %q, want /", sess.Path)
	}
	// The CSRF token is derived from the session ID and read from
	// GET /v1/me (P1) — the v0.1 tcdi_csrf cookie name is gone entirely
	// (E14): no live cookie and no Max-Age=0 deletion for it.
	if csrf := findCookie(cookies, "tcdi_csrf"); csrf != nil {
		t.Fatalf("legacy CSRF cookie touched at login: %q", csrf.Name)
	}

	resp2 := env.authedGet(t, sess, "/v1/me")
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("/v1/me status = %d", resp2.StatusCode)
	}
	me := decodeMe(t, resp2)
	if me.Subject != env.issuer.Subject || me.Tenant != env.issuer.TenantID {
		t.Fatalf("principal wrong: %+v", me)
	}
	if me.CSRFToken != csrfTokenFor(sess.Value) {
		t.Fatalf("csrfToken = %q, want the session-derived token", me.CSRFToken)
	}
}

func callbackStatus(t *testing.T, env *testEnv) (int, string) {
	t.Helper()
	resp, _ := env.login(t)
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusFound {
		return resp.StatusCode, ""
	}
	return resp.StatusCode, decodeError(t, resp)
}

func TestCallbackRejectsWrongIssuer(t *testing.T) {
	env := newTestEnv(t, nil)
	env.issuer.MutateTokenClaims(func(c map[string]any) { c["iss"] = "https://evil.example" })
	if status, code := callbackStatus(t, env); status != http.StatusUnauthorized || code != string(CodeUnauthenticated) {
		t.Fatalf("status=%d code=%q", status, code)
	}
}

func TestCallbackRejectsWrongAudience(t *testing.T) {
	env := newTestEnv(t, nil)
	env.issuer.MutateTokenClaims(func(c map[string]any) { c["aud"] = "someone-else" })
	if status, code := callbackStatus(t, env); status != http.StatusUnauthorized || code != string(CodeUnauthenticated) {
		t.Fatalf("status=%d code=%q", status, code)
	}
}

func TestCallbackRejectsExpiredToken(t *testing.T) {
	env := newTestEnv(t, nil)
	env.issuer.MutateTokenClaims(func(c map[string]any) {
		c["exp"] = time.Now().Add(-time.Hour).Unix()
		c["iat"] = time.Now().Add(-2 * time.Hour).Unix()
	})
	if status, code := callbackStatus(t, env); status != http.StatusUnauthorized || code != string(CodeUnauthenticated) {
		t.Fatalf("status=%d code=%q", status, code)
	}
}

func TestCallbackRejectsNonceMismatch(t *testing.T) {
	env := newTestEnv(t, nil)
	env.issuer.MutateTokenClaims(func(c map[string]any) { c["nonce"] = "attacker-nonce" })
	if status, code := callbackStatus(t, env); status != http.StatusUnauthorized || code != string(CodeUnauthenticated) {
		t.Fatalf("status=%d code=%q", status, code)
	}
}

func TestCallbackRejectsUnknownState(t *testing.T) {
	env := newTestEnv(t, nil)
	resp, err := noRedirectClient().Get(env.server.URL + "/auth/callback?code=x&state=forged")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || decodeError(t, resp) != string(CodeUnauthenticated) {
		t.Fatalf("status=%d", resp.StatusCode)
	}
}

func TestCallbackRejectsStateReplay(t *testing.T) {
	env := newTestEnv(t, nil)
	client := noRedirectClient()

	resp, err := client.Get(env.server.URL + "/auth/login")
	if err != nil {
		t.Fatal(err)
	}
	loc := resp.Header.Get("Location")
	loginCookie := findCookie(resp.Cookies(), env.auth.LoginCookieName())
	resp.Body.Close()
	resp2, _ := client.Get(loc)
	cbURL := resp2.Header.Get("Location")
	resp2.Body.Close()
	cb, _ := url.Parse(cbURL)
	cbPath := env.server.URL + "/auth/callback?" + cb.Query().Encode()

	req, _ := http.NewRequest(http.MethodGet, cbPath, nil)
	req.AddCookie(loginCookie)
	first, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	first.Body.Close()
	if first.StatusCode != http.StatusFound {
		t.Fatalf("first callback status = %d", first.StatusCode)
	}
	// Replay the exact same callback: the code is single-use at the IdP and
	// the login cookie was expired on first use — must reject.
	req, _ = http.NewRequest(http.MethodGet, cbPath, nil)
	req.AddCookie(loginCookie)
	second, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Body.Close()
	if second.StatusCode != http.StatusUnauthorized || decodeError(t, second) != string(CodeUnauthenticated) {
		t.Fatalf("replay status = %d, want 401 UNAUTHENTICATED", second.StatusCode)
	}
}

func TestSessionFixationPrevented(t *testing.T) {
	env := newTestEnv(t, nil)
	// Attacker plants a session with a known ID on the victim's browser.
	planted := &Session{
		ID:         "attacker-fixed-session-id",
		Principal:  Principal{Issuer: env.issuer.URL(), Subject: "nobody", TenantID: "tenant-a"},
		CreatedAt:  time.Now(),
		LastSeenAt: time.Now(),
		ExpiresAt:  time.Now().Add(time.Hour),
	}
	if err := env.store.Save(context.Background(), planted); err != nil {
		t.Fatal(err)
	}
	resp, cookies := env.login(t, &http.Cookie{Name: env.auth.SessionCookieName(), Value: planted.ID})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("callback status = %d", resp.StatusCode)
	}
	sess := findCookie(cookies, env.auth.SessionCookieName())
	if sess == nil || sess.Value == planted.ID {
		t.Fatal("session ID was not rotated at login")
	}
	// The planted session must be gone server-side.
	r := env.authedGet(t, &http.Cookie{Name: env.auth.SessionCookieName(), Value: planted.ID}, "/v1/me")
	defer r.Body.Close()
	if r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("planted session still valid: status=%d", r.StatusCode)
	}
}

func TestSessionIdleAndAbsoluteExpiry(t *testing.T) {
	env := newTestEnv(t, func(c *AuthConfig) {
		c.IdleTimeout = time.Minute
		c.AbsoluteTimeout = 10 * time.Minute
	})
	fc := &fakeClock{now: time.Now()}
	env.store.WithClock(fc.Now)
	env.auth.now = fc.Now
	env.store.idle = time.Minute

	resp, cookies := env.login(t)
	resp.Body.Close()
	sess := findCookie(cookies, env.auth.SessionCookieName())

	r := env.authedGet(t, sess, "/v1/me")
	r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("fresh session rejected: %d", r.StatusCode)
	}

	fc.Advance(2 * time.Minute) // past idle
	r = env.authedGet(t, sess, "/v1/me")
	r.Body.Close()
	if r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("idle-expired session accepted: %d", r.StatusCode)
	}

	// Absolute expiry: new login, stay under idle, pass absolute.
	resp, cookies = env.login(t)
	resp.Body.Close()
	sess = findCookie(cookies, env.auth.SessionCookieName())
	fc.Advance(30 * time.Second)
	r = env.authedGet(t, sess, "/v1/me") // touches idle
	r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("second session rejected early: %d", r.StatusCode)
	}
	fc.Advance(11 * time.Minute) // past absolute
	r = env.authedGet(t, sess, "/v1/me")
	r.Body.Close()
	if r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("absolute-expired session accepted: %d", r.StatusCode)
	}
}

func TestTenantMembershipRequired(t *testing.T) {
	env := newTestEnv(t, nil)
	env.issuer.TenantID = "" // token carries no tenant claim
	if status, code := callbackStatus(t, env); status != http.StatusForbidden || code != string(CodeForbidden) {
		t.Fatalf("status=%d code=%q", status, code)
	}
}

func TestTenantNotInAllowlistRejected(t *testing.T) {
	env := newTestEnv(t, func(c *AuthConfig) { c.AllowedTenants = []string{"tenant-b"} })
	if status, code := callbackStatus(t, env); status != http.StatusForbidden || code != string(CodeForbidden) {
		t.Fatalf("status=%d code=%q", status, code)
	}
}

// --- The login request must carry a PKCE S256 challenge -------------------

// The authorization request must send code_challenge + method S256 so the
// IdP client can be pinned to PKCE-only (XClient sets
// pkceCodeChallengeMethod=S256). The issuer's token endpoint independently
// verifies verifier-vs-challenge, so a tampered challenge also fails.
func TestLoginSendsPKCES256(t *testing.T) {
	env := newTestEnv(t, nil)
	resp, err := noRedirectClient().Get(env.server.URL + "/auth/login")
	if err != nil {
		t.Fatal(err)
	}
	loc := resp.Header.Get("Location")
	resp.Body.Close()
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("authorize URL unparseable: %v", err)
	}
	q := u.Query()
	if m := q.Get("code_challenge_method"); m != "S256" {
		t.Fatalf("code_challenge_method = %q, want S256", m)
	}
	raw, err := base64.RawURLEncoding.DecodeString(q.Get("code_challenge"))
	if err != nil || len(raw) != sha256.Size {
		t.Fatalf("code_challenge is not a base64url SHA-256 digest: %q err=%v", q.Get("code_challenge"), err)
	}
}

// --- Required-group login gate -------------------------------------------

// With no RequiredGroups configured there is no gate: even a token without
// any groups claim logs in.
func TestRequiredGroupsEmptyConfigDisablesGate(t *testing.T) {
	env := newTestEnv(t, nil)
	env.issuer.Groups = nil // ID token carries no groups claim at all
	resp, cookies := env.login(t)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("callback status = %d, want 302", resp.StatusCode)
	}
	if findCookie(cookies, env.auth.SessionCookieName()) == nil {
		t.Fatal("no session cookie set")
	}
}

func TestRequiredGroupsAllowsMember(t *testing.T) {
	env := newTestEnv(t, func(c *AuthConfig) {
		c.RequiredGroups = []string{"platform-admins", "sec-ops"}
	})
	env.issuer.Groups = []string{"devs", "platform-admins"}
	resp, cookies := env.login(t)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("callback status = %d, want 302", resp.StatusCode)
	}
	sess := findCookie(cookies, env.auth.SessionCookieName())
	if sess == nil {
		t.Fatal("no session cookie set")
	}
	r := env.authedGet(t, sess, "/v1/me")
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("session not usable after group-gated login: %d", r.StatusCode)
	}
}

// A configured gate must fail closed: no session, a clear 403 error body,
// and a denial log that names only the pseudonymous actor.
func TestRequiredGroupsDeniesNonMember(t *testing.T) {
	env := newTestEnv(t, func(c *AuthConfig) { c.RequiredGroups = []string{"platform-admins"} })
	env.issuer.Groups = []string{"devs"}
	resp, cookies := env.login(t)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || decodeError(t, resp) != string(CodeForbidden) {
		t.Fatalf("status=%d, want 403 FORBIDDEN", resp.StatusCode)
	}
	if findCookie(cookies, env.auth.SessionCookieName()) != nil {
		t.Fatal("session cookie issued to a denied login")
	}
	logs := env.logs.String()
	if !strings.Contains(logs, observability.ActorRef(env.issuer.URL(), env.issuer.Subject)) {
		t.Fatalf("denial not logged with pseudonymous actor: %s", logs)
	}
	for _, leaked := range []string{env.issuer.Subject, "devs", env.issuer.LastIDToken()} {
		if leaked != "" && strings.Contains(logs, leaked) {
			t.Fatalf("denial log leaked %q: %s", leaked, logs)
		}
	}
}

// Groups the gate does not list do not open it, including near-misses such
// as Keycloak path syntax ("/platform-admins" != "platform-admins") and
// prefix matches — the claim match is exact.
func TestRequiredGroupsExactMatchOnly(t *testing.T) {
	for _, groups := range [][]string{
		{"/platform-admins"}, // Keycloak full-path form must not match
		{"platform-admins-eu"},
		{"PLATFORM-ADMINS"},
		nil, // claim absent entirely
	} {
		env := newTestEnv(t, func(c *AuthConfig) { c.RequiredGroups = []string{"platform-admins"} })
		env.issuer.Groups = groups
		resp, _ := env.login(t)
		status := resp.StatusCode
		resp.Body.Close()
		if status != http.StatusForbidden {
			t.Fatalf("groups=%v: status=%d, want 403", groups, status)
		}
	}
}

// A groups claim that is not a list of strings yields no matchable group and
// must be denied — the gate never errors open on malformed claim types.
func TestRequiredGroupsMalformedClaimDenied(t *testing.T) {
	for name, claim := range map[string]any{
		"number":       42.0,
		"bool":         true,
		"object":       map[string]any{"name": "platform-admins"},
		"number array": []any{1, 2},
		"null":         nil,
	} {
		t.Run(name, func(t *testing.T) {
			env := newTestEnv(t, func(c *AuthConfig) { c.RequiredGroups = []string{"platform-admins"} })
			env.issuer.MutateTokenClaims(func(c map[string]any) { c["groups"] = claim })
			resp, _ := env.login(t)
			status := resp.StatusCode
			resp.Body.Close()
			if status != http.StatusForbidden {
				t.Fatalf("status=%d, want 403", status)
			}
		})
	}
}

// RequiredGroups entries must be non-empty: a blank entry can only come from
// a config typo and would silently mislead operators about what is gated.
func TestNewAuthenticatorRejectsEmptyRequiredGroup(t *testing.T) {
	iss, err := oidctest.NewIssuer()
	if err != nil {
		t.Fatal(err)
	}
	defer iss.Close()
	_, err = NewAuthenticator(context.Background(), AuthConfig{
		Issuer:         iss.URL(),
		ClientID:       iss.ClientID,
		RedirectURL:    "https://portal.test/auth/callback",
		LoginSealer:    testLoginSealer(t),
		RequiredGroups: []string{"platform-admins", " "},
	}, NewInMemorySessionStore(time.Minute), nil)
	if err == nil {
		t.Fatal("NewAuthenticator accepted a blank RequiredGroups entry")
	}
}

// The login sealer is required: without it an Authenticator could not bind
// in-flight logins to the initiating browser (SEC-03).
func TestNewAuthenticatorRequiresLoginSealer(t *testing.T) {
	iss, err := oidctest.NewIssuer()
	if err != nil {
		t.Fatal(err)
	}
	defer iss.Close()
	_, err = NewAuthenticator(context.Background(), AuthConfig{
		Issuer:      iss.URL(),
		ClientID:    iss.ClientID,
		RedirectURL: "https://portal.test/auth/callback",
	}, NewInMemorySessionStore(time.Minute), nil)
	if err == nil {
		t.Fatal("NewAuthenticator accepted a config without LoginSealer")
	}
}

func TestLogoutDestroysSession(t *testing.T) {
	env := newTestEnv(t, nil)
	resp, cookies := env.login(t)
	resp.Body.Close()
	sess := findCookie(cookies, env.auth.SessionCookieName())

	req, _ := http.NewRequest(http.MethodPost, env.server.URL+"/auth/logout", nil)
	req.AddCookie(sess)
	req.Header.Set(env.auth.CSRFHeader(), csrfTokenFor(sess.Value))
	lr, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	lr.Body.Close()
	if lr.StatusCode != http.StatusNoContent {
		t.Fatalf("logout status = %d", lr.StatusCode)
	}

	r := env.authedGet(t, sess, "/v1/me")
	defer r.Body.Close()
	if r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("session valid after logout: %d", r.StatusCode)
	}
}

func TestOwnerDerivedFromPrincipalNotBody(t *testing.T) {
	env := newTestEnv(t, nil)
	resp, cookies := env.login(t)
	resp.Body.Close()
	sess := findCookie(cookies, env.auth.SessionCookieName())

	req, _ := http.NewRequest(http.MethodPost, env.server.URL+"/v1/echo-owner",
		strings.NewReader(`{"owner":"mallory@evil"}`))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(sess)
	req.Header.Set(env.auth.CSRFHeader(), csrfTokenFor(sess.Value))
	r, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	var out map[string]string
	_ = json.NewDecoder(r.Body).Decode(&out)
	want := env.issuer.URL() + "|" + env.issuer.Subject
	if out["owner"] != want {
		t.Fatalf("owner = %q, want principal-derived %q (body owner %q must be ignored)", out["owner"], want, out["body_owner"])
	}
}

func TestUnauthenticatedRequestRejected(t *testing.T) {
	env := newTestEnv(t, nil)
	r := env.authedGet(t, nil, "/v1/me")
	defer r.Body.Close()
	if r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", r.StatusCode)
	}
	var body Error
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body.Code != CodeUnauthenticated || body.RequestID == "" {
		t.Fatalf("error body malformed: %+v", body)
	}
}

// --- SEC-03: the login state must be bound to the initiating browser ------

// callbackQueryForLogin starts a login and returns the authorize redirect's
// callback query (code+state) plus the Set-Cookie list from /auth/login.
func (e *testEnv) callbackQueryForLogin(t *testing.T) (string, []*http.Cookie) {
	t.Helper()
	client := noRedirectClient()
	resp, err := client.Get(e.server.URL + "/auth/login")
	if err != nil {
		t.Fatalf("GET /auth/login: %v", err)
	}
	loc := resp.Header.Get("Location")
	cookies := resp.Cookies()
	resp.Body.Close()
	resp2, err := client.Get(loc)
	if err != nil {
		t.Fatalf("GET authorize: %v", err)
	}
	cbLoc := resp2.Header.Get("Location")
	resp2.Body.Close()
	cb, err := url.Parse(cbLoc)
	if err != nil || cb.Query().Get("code") == "" {
		t.Fatalf("authorize redirect malformed: %q", cbLoc)
	}
	return cb.Query().Encode(), cookies
}

// SEC-03 regression: a victim browser
// that never started a login must not be able to complete one — the state
// is bound to a __Host-tcdi_login cookie only the initiator holds.
func TestLogin_MissingCookie(t *testing.T) {
	env := newTestEnv(t, nil)
	query, _ := env.callbackQueryForLogin(t)

	resp, err := noRedirectClient().Get(env.server.URL + "/auth/callback?" + query)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("callback without login cookie: status=%d, want 401", resp.StatusCode)
	}
	if findCookie(resp.Cookies(), env.auth.SessionCookieName()) != nil {
		t.Fatal("session cookie issued to a browser that never started login")
	}
}

// The OAuth state the IdP echoes back must equal the state sealed into the
// login cookie — a callback carrying a different state is rejected even
// when the cookie itself is authentic.
func TestLogin_StateMismatch(t *testing.T) {
	env := newTestEnv(t, nil)
	query, cookies := env.callbackQueryForLogin(t)
	loginCookie := findCookie(cookies, env.auth.LoginCookieName())
	if loginCookie == nil {
		t.Fatal("login did not set the login cookie")
	}

	q, err := url.ParseQuery(query)
	if err != nil {
		t.Fatal(err)
	}
	q.Set("state", "a-different-state-value")
	req, _ := http.NewRequest(http.MethodGet, env.server.URL+"/auth/callback?"+q.Encode(), nil)
	req.AddCookie(loginCookie)
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("state mismatch: status=%d, want 401", resp.StatusCode)
	}
	if findCookie(resp.Cookies(), env.auth.SessionCookieName()) != nil {
		t.Fatal("session cookie issued on state mismatch")
	}
}

// The login TTL is sealed into the cookie itself: once it has passed, the
// callback rejects the login — no server-side pending entry is required.
func TestLogin_ExpiredCookie(t *testing.T) {
	env := newTestEnv(t, func(c *AuthConfig) { c.PendingTTL = time.Minute })
	fc := &fakeClock{now: time.Now()}
	env.auth.now = fc.Now

	query, cookies := env.callbackQueryForLogin(t)
	loginCookie := findCookie(cookies, env.auth.LoginCookieName())
	if loginCookie == nil {
		t.Fatal("login did not set the login cookie")
	}

	fc.Advance(2 * time.Minute) // past the login TTL sealed into the cookie
	req, _ := http.NewRequest(http.MethodGet, env.server.URL+"/auth/callback?"+query, nil)
	req.AddCookie(loginCookie)
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expired login cookie: status=%d, want 401", resp.StatusCode)
	}
}

// D20: in-flight logins live in the sealed cookie, not in process memory —
// a login started on one replica completes on any replica sharing the keys.
func TestLogin_CallbackOnOtherReplica(t *testing.T) {
	envA := newTestEnv(t, nil)
	envB := envA.spawnReplica(t)
	client := noRedirectClient()

	resp, err := client.Get(envA.server.URL + "/auth/login")
	if err != nil {
		t.Fatalf("GET /auth/login on replica A: %v", err)
	}
	loc := resp.Header.Get("Location")
	loginCookie := findCookie(resp.Cookies(), envA.auth.LoginCookieName())
	resp.Body.Close()
	if loginCookie == nil {
		t.Fatal("replica A did not set the login cookie")
	}

	resp2, err := client.Get(loc)
	if err != nil {
		t.Fatalf("GET authorize: %v", err)
	}
	cbLoc := resp2.Header.Get("Location")
	resp2.Body.Close()
	cb, err := url.Parse(cbLoc)
	if err != nil || cb.Query().Get("code") == "" {
		t.Fatalf("authorize redirect malformed: %q", cbLoc)
	}

	// The callback lands on replica B carrying the cookie A sealed.
	req, _ := http.NewRequest(http.MethodGet, envB.server.URL+"/auth/callback?"+cb.Query().Encode(), nil)
	req.AddCookie(loginCookie)
	resp3, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET callback on replica B: %v", err)
	}
	defer resp3.Body.Close()
	if resp3.StatusCode != http.StatusFound {
		t.Fatalf("callback on replica B: status=%d, want 302", resp3.StatusCode)
	}
	if loc := resp3.Header.Get("Location"); loc != "/" {
		t.Fatalf("post-login redirect = %q, want /", loc)
	}
	sess := findCookie(resp3.Cookies(), envB.auth.SessionCookieName())
	if sess == nil {
		t.Fatal("replica B did not issue a __Host-tcdi_session cookie")
	}
	r := envB.authedGet(t, sess, "/v1/me")
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("/v1/me on replica B: status=%d", r.StatusCode)
	}
}

func TestCallbackRejectsWrongLoginCookie(t *testing.T) {
	env := newTestEnv(t, nil)
	query, _ := env.callbackQueryForLogin(t)

	req, _ := http.NewRequest(http.MethodGet, env.server.URL+"/auth/callback?"+query, nil)
	req.AddCookie(&http.Cookie{Name: env.auth.LoginCookieName(), Value: "not-the-login-proof"})
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("callback with wrong login cookie: status=%d, want 401", resp.StatusCode)
	}
}

func TestLoginCookieAttributes(t *testing.T) {
	env := newTestEnv(t, nil)
	resp, err := noRedirectClient().Get(env.server.URL + "/auth/login")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	c := findCookie(resp.Cookies(), env.auth.LoginCookieName())
	if c == nil {
		t.Fatal("no login cookie set")
	}
	if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode ||
		c.Path != "/" || c.Domain != "" || c.MaxAge <= 0 || c.MaxAge > 600 {
		t.Fatalf("login cookie attributes wrong: %+v", c)
	}
}

// The login cookie is single-use together with the login attempt: a
// successful callback expires it, so a stale value cannot linger in the jar.
func TestLogin_CookieClearedAfterUse(t *testing.T) {
	env := newTestEnv(t, nil)
	resp, _ := env.login(t)
	defer resp.Body.Close()
	c := findCookie(resp.Cookies(), env.auth.LoginCookieName())
	if c == nil || c.MaxAge >= 0 {
		t.Fatalf("login cookie not expired on callback: %+v", c)
	}
}

// --- SEC-I3: raw OIDC subjects never reach the logs ------------------------

func TestLogsNeverContainRawSubject(t *testing.T) {
	env := newTestEnv(t, nil)
	resp, cookies := env.login(t)
	resp.Body.Close()
	sess := findCookie(cookies, env.auth.SessionCookieName())
	r := env.authedGet(t, sess, "/v1/me")
	r.Body.Close()

	out := env.logs.String()
	if strings.Contains(out, env.issuer.Subject) {
		t.Fatalf("logs contain raw OIDC subject %q", env.issuer.Subject)
	}
	want := observability.ActorRef(env.issuer.URL(), env.issuer.Subject)
	if !strings.Contains(out, want) {
		t.Fatalf("logs missing pseudonymous actor %q", want)
	}
}
