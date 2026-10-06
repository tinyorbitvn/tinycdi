package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/tinyorbitvn/tinycdi/internal/observability"
)

func TestCSRFPostWithoutTokenRejected(t *testing.T) {
	env := newTestEnv(t, nil)
	resp, cookies := env.login(t)
	resp.Body.Close()
	sess := findCookie(cookies, env.auth.SessionCookieName())

	req, _ := http.NewRequest(http.MethodPost, env.server.URL+"/v1/echo-owner", strings.NewReader("{}"))
	req.AddCookie(sess)
	r, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusForbidden || decodeError(t, r) != string(CodeCSRFFailed) {
		t.Fatalf("status=%d, want 403 CSRF_FAILED", r.StatusCode)
	}
}

func TestCSRFWrongTokenRejected(t *testing.T) {
	env := newTestEnv(t, nil)
	resp, cookies := env.login(t)
	resp.Body.Close()
	sess := findCookie(cookies, env.auth.SessionCookieName())

	req, _ := http.NewRequest(http.MethodPost, env.server.URL+"/v1/echo-owner", strings.NewReader("{}"))
	req.AddCookie(sess)
	req.Header.Set(env.auth.CSRFHeader(), "forged-token")
	r, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusForbidden {
		t.Fatalf("status=%d, want 403", r.StatusCode)
	}
}

func TestCSRFValidTokenAccepted(t *testing.T) {
	env := newTestEnv(t, nil)
	resp, cookies := env.login(t)
	resp.Body.Close()
	sess := findCookie(cookies, env.auth.SessionCookieName())
	req, _ := http.NewRequest(http.MethodPost, env.server.URL+"/v1/echo-owner", strings.NewReader("{}"))
	req.AddCookie(sess)
	req.Header.Set(env.auth.CSRFHeader(), csrfTokenFor(sess.Value))
	r, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200", r.StatusCode)
	}
}

func TestCSRFGetAllowedWithoutToken(t *testing.T) {
	env := newTestEnv(t, nil)
	resp, cookies := env.login(t)
	resp.Body.Close()
	sess := findCookie(cookies, env.auth.SessionCookieName())

	r := env.authedGet(t, sess, "/v1/me")
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("GET without CSRF token status=%d, want 200", r.StatusCode)
	}
}

func TestCSRFUnauthenticatedPostRejected(t *testing.T) {
	env := newTestEnv(t, nil)
	req, _ := http.NewRequest(http.MethodPost, env.server.URL+"/v1/echo-owner", strings.NewReader("{}"))
	r, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status=%d, want 401", r.StatusCode)
	}
}

func TestRequestIDPresentOnEveryResponse(t *testing.T) {
	env := newTestEnv(t, nil)

	// Error response (401) must still carry X-Request-Id.
	r := env.authedGet(t, nil, "/v1/me")
	rid := r.Header.Get(RequestIDHeader)
	var body Error
	_ = json.NewDecoder(r.Body).Decode(&body)
	r.Body.Close()
	if rid == "" {
		t.Fatal("error response missing X-Request-Id")
	}
	if body.RequestID != rid {
		t.Fatalf("error body requestId %q != header %q", body.RequestID, rid)
	}

	// Valid inbound ID is propagated.
	req, _ := http.NewRequest(http.MethodGet, env.server.URL+"/v1/me", nil)
	req.Header.Set(RequestIDHeader, "incoming-req-42")
	r2, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r2.Body.Close()
	if got := r2.Header.Get(RequestIDHeader); got != "incoming-req-42" {
		t.Fatalf("propagated request id = %q", got)
	}

	// Malformed inbound ID is replaced, never echoed.
	req, _ = http.NewRequest(http.MethodGet, env.server.URL+"/v1/me", nil)
	req.Header.Set(RequestIDHeader, "bad id with spaces")
	r3, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r3.Body.Close()
	got := r3.Header.Get(RequestIDHeader)
	if got == "" || got == "bad id with spaces" || !strings.HasPrefix(got, "req-") {
		t.Fatalf("malformed inbound id not replaced: %q", got)
	}
}

func TestAuditLogNeverContainsSecrets(t *testing.T) {
	env := newTestEnv(t, nil)
	resp, cookies := env.login(t)
	resp.Body.Close()
	sess := findCookie(cookies, env.auth.SessionCookieName())
	csrf := csrfTokenFor(sess.Value) // the derived token is credential-equivalent

	// Drive a request carrying an Authorization bearer and the session
	// cookie through the audit middleware.
	req, _ := http.NewRequest(http.MethodGet, env.server.URL+"/v1/me", nil)
	req.AddCookie(sess)
	req.Header.Set("Authorization", "Bearer ultra-secret-bearer-value-123")
	r, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()

	out := env.logs.String()
	for _, secret := range []string{
		sess.Value,
		csrf,
		"ultra-secret-bearer-value-123",
		env.issuer.LastIDToken(),
		env.issuer.LastAccessToken(),
	} {
		if secret != "" && strings.Contains(out, secret) {
			t.Fatalf("audit log leaked secret material: %q...", secret[:8])
		}
	}
	// The OIDC callback carries code+state in the query — path-only logging
	// must keep them out.
	if strings.Contains(out, "code=") || strings.Contains(out, "state=") {
		t.Fatal("audit log contains raw callback query parameters")
	}
	if !strings.Contains(out, "http_request") {
		t.Fatal("expected audit records")
	}
}

func TestInstrumentHTTPEmitsMetricsPerRequest(t *testing.T) {
	reg := prometheus.NewRegistry()
	metrics := observability.NewMetrics(reg, []string{"tenant-a"})
	env := newTestEnvOpts(t, nil, nil, metrics)

	// Authenticated GET → 2xx.
	resp, cookies := env.login(t)
	resp.Body.Close()
	sess := findCookie(cookies, env.auth.SessionCookieName())
	r := env.authedGet(t, sess, "/v1/me")
	r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("authed GET status=%d", r.StatusCode)
	}

	// Unauthenticated GET → 4xx, still counted under its route template.
	r = env.authedGet(t, nil, "/v1/me")
	r.Body.Close()
	if r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauth GET status=%d", r.StatusCode)
	}

	// Unknown route → "unmatched", still counted.
	r = env.authedGet(t, nil, "/no-such-route")
	r.Body.Close()

	err := testutil.GatherAndCompare(reg, strings.NewReader(
		`# HELP tinycdi_http_requests_total HTTP requests handled, by listener, route template, method and response code class.
# TYPE tinycdi_http_requests_total counter
tinycdi_http_requests_total{code_class="3xx",listener="app",method="GET",route="/auth/login"} 1
tinycdi_http_requests_total{code_class="3xx",listener="app",method="GET",route="/auth/callback"} 1
tinycdi_http_requests_total{code_class="2xx",listener="app",method="GET",route="/v1/me"} 1
tinycdi_http_requests_total{code_class="4xx",listener="app",method="GET",route="/v1/me"} 1
tinycdi_http_requests_total{code_class="4xx",listener="app",method="GET",route="unmatched"} 1
`), "tinycdi_http_requests_total")
	if err != nil {
		t.Fatalf("http request metrics mismatch:\n%v", err)
	}

	// E8: the completed callback counted one successful login.
	if err := testutil.GatherAndCompare(reg, strings.NewReader(
		`# HELP tinycdi_logins_total Completed /v1/auth/callback login attempts, by bounded outcome.
# TYPE tinycdi_logins_total counter
tinycdi_logins_total{outcome="success"} 1
`), "tinycdi_logins_total"); err != nil {
		t.Fatalf("login metrics mismatch:\n%v", err)
	}
}

func TestAuditSinkReceivesRedactedRequestEvents(t *testing.T) {
	var buf bytes.Buffer
	sink := observability.NewJSONSink(&buf)
	env := newTestEnvOpts(t, nil, sink, nil)

	// Authenticated request carrying an Authorization header value that must
	// never appear in the audit stream.
	resp, cookies := env.login(t)
	resp.Body.Close()
	sess := findCookie(cookies, env.auth.SessionCookieName())
	req, _ := http.NewRequest(http.MethodGet, env.server.URL+"/v1/me", nil)
	req.AddCookie(sess)
	req.Header.Set("Authorization", "Bearer audit-secret-zzz")
	r, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()

	out := buf.String()
	if !strings.Contains(out, `"action":"http.request"`) {
		t.Fatalf("no request audit event emitted: %s", out)
	}
	want := `"actor":"` + observability.ActorRef(env.issuer.URL(), env.issuer.Subject) + `"`
	if !strings.Contains(out, want) {
		t.Fatalf("audit actor not the hashed principal ref; want %s in %s", want, out)
	}
	for _, leak := range []string{"audit-secret-zzz", sess.Value} {
		if strings.Contains(out, leak) {
			t.Fatalf("audit event leaked %q", leak[:12])
		}
	}
}

// --- Origin / Sec-Fetch-Site defence in depth (hardening) -------------------

func originProbeServer(t *testing.T, allowed []string) *httptest.Server {
	t.Helper()
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(RequestID(RequireTrustedOrigin("__Host-tcdi_session", allowed)(ok)))
	t.Cleanup(srv.Close)
	return srv
}

func originReq(t *testing.T, srv *httptest.Server, method string, cookie bool, hdrs map[string]string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, srv.URL+"/v1/x", strings.NewReader("{}"))
	if cookie {
		req.AddCookie(&http.Cookie{Name: "__Host-tcdi_session", Value: "sess-id"})
	}
	for k, v := range hdrs {
		req.Header.Set(k, v)
	}
	r, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestOriginAllowlist(t *testing.T) {
	allowed := []string{"https://portal.test", "https://portal.test:8443"}
	cases := []struct {
		name   string
		method string
		cookie bool
		hdrs   map[string]string
		want   int
	}{
		{"allowed portal origin", "POST", true, map[string]string{"Origin": "https://portal.test"}, http.StatusNoContent},
		{"allowed portal origin w/ port", "POST", true, map[string]string{"Origin": "https://portal.test:8443"}, http.StatusNoContent},
		{"foreign origin with valid session", "POST", true, map[string]string{"Origin": "https://evil.example"}, http.StatusForbidden},
		{"null origin", "POST", true, map[string]string{"Origin": "null"}, http.StatusForbidden},
		{"origin missing but sec-fetch-site cross-site", "POST", true, map[string]string{"Sec-Fetch-Site": "cross-site"}, http.StatusForbidden},
		{"origin missing, sec-fetch-site same-site", "POST", true, map[string]string{"Sec-Fetch-Site": "same-site"}, http.StatusForbidden},
		{"sec-fetch-site same-origin", "POST", true, map[string]string{"Sec-Fetch-Site": "same-origin"}, http.StatusNoContent},
		{"no provenance headers", "POST", true, nil, http.StatusNoContent},
		{"no cookie: not browser-authenticated, skipped", "POST", false, map[string]string{"Origin": "https://evil.example"}, http.StatusNoContent},
		{"GET unaffected by foreign origin", "GET", true, map[string]string{"Origin": "https://evil.example"}, http.StatusNoContent},
		{"DELETE state-changing also guarded", "DELETE", true, map[string]string{"Origin": "https://evil.example"}, http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := originProbeServer(t, allowed)
			r := originReq(t, srv, tc.method, tc.cookie, tc.hdrs)
			defer r.Body.Close()
			if r.StatusCode != tc.want {
				t.Fatalf("status=%d, want %d", r.StatusCode, tc.want)
			}
			if tc.want == http.StatusForbidden && decodeError(t, r) != string(CodeCSRFFailed) {
				t.Fatalf("body code = %q, want CSRF_FAILED", decodeError(t, r))
			}
		})
	}
}

func TestOriginMultipleHeadersRejected(t *testing.T) {
	srv := originProbeServer(t, []string{"https://portal.test"})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/x", strings.NewReader("{}"))
	req.AddCookie(&http.Cookie{Name: "__Host-tcdi_session", Value: "sess-id"})
	req.Header.Add("Origin", "https://portal.test")
	req.Header.Add("Origin", "https://evil.example")
	r, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusForbidden {
		t.Fatalf("status=%d, want 403", r.StatusCode)
	}
}

// end-to-end through the real middleware chain: RequireTrustedOrigin must sit
// inside the cookie-authenticated chain and reject a forged Origin even when
// the CSRF token is valid.
func TestCSRFChainForgedOriginRejected(t *testing.T) {
	env := newTestEnv(t, nil)
	resp, cookies := env.login(t)
	resp.Body.Close()
	sess := findCookie(cookies, env.auth.SessionCookieName())
	csrf := csrfTokenFor(sess.Value)

	srv := httptest.NewServer(RequestID(RequireTrustedOrigin(env.auth.SessionCookieName(), []string{"https://portal.test"})(
		env.auth.RequireAuth(env.auth.RequireCSRF(http.HandlerFunc(echoOwnerHandler))))))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/echo-owner", strings.NewReader("{}"))
	req.AddCookie(sess)
	req.Header.Set(env.auth.CSRFHeader(), csrf)
	req.Header.Set("Origin", "https://evil.example")
	r, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusForbidden || decodeError(t, r) != string(CodeCSRFFailed) {
		t.Fatalf("status=%d, want 403 CSRF_FAILED", r.StatusCode)
	}
}

// failingSessionStore is a SessionStore whose reads return err — the shape
// RequireAuth sees while Postgres is down (E6 drill).
type failingSessionStore struct{ err error }

func (s failingSessionStore) Save(context.Context, *Session) error           { return s.err }
func (s failingSessionStore) Get(context.Context, string) (*Session, error)  { return nil, s.err }
func (s failingSessionStore) Peek(context.Context, string) (*Session, error) { return nil, s.err }
func (s failingSessionStore) TouchPrincipal(context.Context, string) (int64, error) {
	return 0, s.err
}
func (s failingSessionStore) Delete(context.Context, string) error { return s.err }

// TestRequireAuth_SessionStoreErrors: a store error is not "no session".
// Not-found stays 401; a store that cannot answer (outage, failover) must
// be 503 UNAVAILABLE so the portal retries instead of forcing re-login;
// anything else is a bug — 500.
func TestRequireAuth_SessionStoreErrors(t *testing.T) {
	for _, tc := range []struct {
		name       string
		err        error
		wantStatus int
		wantCode   ErrorCode
	}{
		{"not found stays 401", ErrSessionNotFound, http.StatusUnauthorized, CodeUnauthenticated},
		{"store outage is 503", fmt.Errorf("get session: %w", context.DeadlineExceeded), http.StatusServiceUnavailable, CodeUnavailable},
		{"store bug is 500", errors.New("scan: column count mismatch"), http.StatusInternalServerError, CodeInternal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			auth := &Authenticator{
				cfg:      &AuthConfig{SessionCookieName: "__Host-tcdi_session"},
				sessions: failingSessionStore{err: tc.err},
			}
			h := auth.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			}))
			for _, passive := range []bool{false, true} {
				handler := h
				if passive {
					handler = auth.RequireAuthPassive(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
						w.WriteHeader(http.StatusNoContent)
					}))
				}
				req := httptest.NewRequest(http.MethodGet, "/v1/workspaces", nil)
				req.AddCookie(&http.Cookie{Name: "__Host-tcdi_session", Value: "sess-x"})
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, req)
				if rec.Code != tc.wantStatus {
					t.Fatalf("passive=%v status=%d, want %d", passive, rec.Code, tc.wantStatus)
				}
				var body struct {
					Code string `json:"code"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Fatalf("error body: %v", err)
				}
				if body.Code != string(tc.wantCode) {
					t.Fatalf("passive=%v code=%q, want %s", passive, body.Code, tc.wantCode)
				}
			}
		})
	}
}

// TestRequireAuth_BackgroundPollMarker (FIX-IDLE): a request carrying
// X-TCDI-Poll: background authenticates but never slides the idle window,
// so the portal's interval polls cannot keep a visible-but-unattended
// session alive. An unmarked request on the same RequireAuth route still
// slides; once the window lapses both shapes answer 401.
func TestRequireAuth_BackgroundPollMarker(t *testing.T) {
	fc := &fakeClock{now: time.Now()}
	store := NewInMemorySessionStore(time.Minute).WithClock(fc.Now)
	auth := &Authenticator{
		cfg:      &AuthConfig{SessionCookieName: "__Host-tcdi_session"},
		sessions: store,
		now:      fc.Now,
	}
	save := func(id string) {
		if err := store.Save(context.Background(), &Session{
			ID:         id,
			Principal:  Principal{Issuer: "iss", Subject: "sub", TenantID: "tenant-a"},
			CreatedAt:  fc.Now(),
			LastSeenAt: fc.Now(),
			ExpiresAt:  fc.Now().Add(time.Hour),
		}); err != nil {
			t.Fatal(err)
		}
	}
	handler := auth.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	do := func(id string, marked bool) int {
		req := httptest.NewRequest(http.MethodGet, "/v1/workspaces", nil)
		req.AddCookie(&http.Cookie{Name: "__Host-tcdi_session", Value: id})
		if marked {
			req.Header.Set(PollHeader, PollHeaderValue)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec.Code
	}

	// Marked polls authenticate but never slide: created at t=0, the poll
	// at +50s succeeds yet the window still lapses at +60s.
	save("sess-poll")
	fc.Advance(50 * time.Second)
	if code := do("sess-poll", true); code != http.StatusNoContent {
		t.Fatalf("marked poll inside the window rejected: %d", code)
	}
	fc.Advance(15 * time.Second) // t=65s — past idle despite the +50s poll
	if code := do("sess-poll", true); code != http.StatusUnauthorized {
		t.Fatalf("marked poll past idle: %d, want 401", code)
	}

	// The marker is opt-out only: a wrong value is ordinary activity.
	save("sess-other-value")
	req := httptest.NewRequest(http.MethodGet, "/v1/workspaces", nil)
	req.AddCookie(&http.Cookie{Name: "__Host-tcdi_session", Value: "sess-other-value"})
	req.Header.Set(PollHeader, "1")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("other marker value rejected: %d", rec.Code)
	}
	sess, err := store.Get(context.Background(), "sess-other-value")
	if err != nil {
		t.Fatal(err)
	}
	if !sess.LastSeenAt.Equal(fc.Now()) {
		t.Fatalf("unrecognized marker value did not slide: LastSeenAt=%v now=%v", sess.LastSeenAt, fc.Now())
	}

	// Without the marker the same cadence slides the window as before.
	save("sess-active")
	fc.Advance(50 * time.Second)
	if code := do("sess-active", false); code != http.StatusNoContent {
		t.Fatalf("unmarked request rejected: %d", code)
	}
	fc.Advance(50 * time.Second) // 50s since the slide — still inside
	if code := do("sess-active", false); code != http.StatusNoContent {
		t.Fatalf("unmarked request past one window: %d", code)
	}

	// Idle expiry answers 401 to marked and unmarked requests alike.
	fc.Advance(61 * time.Second)
	for _, marked := range []bool{false, true} {
		if code := do("sess-active", marked); code != http.StatusUnauthorized {
			t.Fatalf("expired session marked=%v: %d, want 401", marked, code)
		}
	}
}
