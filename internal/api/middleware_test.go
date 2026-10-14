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
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/tinyorbitvn/tinycdi/internal/observability"
	"github.com/tinyorbitvn/tinycdi/internal/ratelimit"
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
	body := decodeErr(t, r)
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
func (s failingSessionStore) TouchSessionDigest(context.Context, string) (int64, error) {
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
					Error struct {
						Code string `json:"code"`
					} `json:"error"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Fatalf("error body: %v", err)
				}
				if body.Error.Code != string(tc.wantCode) {
					t.Fatalf("passive=%v code=%q, want %s", passive, body.Error.Code, tc.wantCode)
				}
			}
		})
	}
}

// lookupCounter counts every session-store read (Get and Peek).
type lookupCounter struct {
	SessionStore
	reads atomic.Int64
}

func (s *lookupCounter) Get(ctx context.Context, id string) (*Session, error) {
	s.reads.Add(1)
	return s.SessionStore.Get(ctx, id)
}

func (s *lookupCounter) Peek(ctx context.Context, id string) (*Session, error) {
	s.reads.Add(1)
	return s.SessionStore.Peek(ctx, id)
}

// lookupSprayServer mounts the production shape of the two cookie-reading
// routes: the probe behind the session-keyed login limiter (burst 1, so
// almost every probe is refused 429) and GET /v1/me behind passive auth.
func lookupSprayServer(t *testing.T, counter *lookupCounter, lookups SessionLookupLimiter) *httptest.Server {
	t.Helper()
	a := sessionKeyTestAuth(counter)
	a.WithSessionLookupLimiter(lookups, nil)
	mux := http.NewServeMux()
	MountSessionProbeRoute(mux, a, RateLimitWithKey(ratelimit.New(1, 1, 1000, nil), nil, nil, a.SessionRateLimitKey()))
	MountMeRoutes(mux, a, NewMeHandler(testSessionDomain))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestAppListener_UnknownCookieSprayBounded (R1-03): one client sends 200
// requests, each with a distinct forged session cookie, across the probe
// and /v1/me. Store reads stay within the unknown-cookie burst, and the
// refused /v1/me requests answer 429 with Retry-After, not 401.
func TestAppListener_UnknownCookieSprayBounded(t *testing.T) {
	counter := &lookupCounter{SessionStore: NewInMemorySessionStore(30 * time.Minute)}
	frozen := time.Unix(1_700_000_000, 0)
	const burst = 10
	srv := lookupSprayServer(t, counter, ratelimit.New(60, burst, 1000, func() time.Time { return frozen }))

	const n = 200
	limited := 0
	for i := 0; i < n; i++ {
		path := "/v1/session"
		if i%2 == 1 {
			path = "/v1/me"
		}
		req, _ := http.NewRequest(http.MethodGet, srv.URL+path, nil)
		req.AddCookie(&http.Cookie{Name: "__Host-tcdi_session", Value: fmt.Sprintf("forged-%d", i)})
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("%s #%d: %v", path, i, err)
		}
		resp.Body.Close()
		if path == "/v1/me" && resp.StatusCode == http.StatusTooManyRequests {
			if resp.Header.Get("Retry-After") == "" {
				t.Fatalf("/v1/me #%d: 429 without Retry-After", i)
			}
			limited++
		}
	}
	if got := counter.reads.Load(); got > burst+2 {
		t.Fatalf("store reads = %d for %d forged cookies from one client, want <= %d", got, n, burst+2)
	}
	if limited == 0 {
		t.Fatal("no /v1/me request was refused 429 once the unknown-cookie budget was spent")
	}
}

// TestAppListener_StoreErrorsKeepLookupSpent (R1-03, task-6 fix round 1):
// only a hit gives the token back. A read that ended in any error —
// a deadline, a cancelled request, a server-side cancel — still cost the
// store, so a spray against a degraded pool stays inside the burst.
func TestAppListener_StoreErrorsKeepLookupSpent(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"deadline", fmt.Errorf("get session: %w", context.DeadlineExceeded)},
		{"canceled", fmt.Errorf("get session: %w", context.Canceled)},
		{"query canceled", &pgconn.PgError{Code: "57014", Message: "canceling statement due to statement timeout"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			counter := &lookupCounter{SessionStore: failingSessionStore{err: tc.err}}
			frozen := time.Unix(1_700_000_000, 0)
			const burst = 10
			srv := lookupSprayServer(t, counter, ratelimit.New(60, burst, 1000, func() time.Time { return frozen }))
			for i := 0; i < 100; i++ {
				path := "/v1/session"
				if i%2 == 1 {
					path = "/v1/me"
				}
				req, _ := http.NewRequest(http.MethodGet, srv.URL+path, nil)
				req.AddCookie(&http.Cookie{Name: "__Host-tcdi_session", Value: fmt.Sprintf("forged-%d", i)})
				resp, err := srv.Client().Do(req)
				if err != nil {
					t.Fatalf("%s #%d: %v", path, i, err)
				}
				resp.Body.Close()
			}
			if got := counter.reads.Load(); got > burst {
				t.Fatalf("store reads = %d for 100 forged cookies against a failing store, want <= %d", got, burst)
			}
		})
	}
}

// TestAppListener_ValidSessionsBehindOneClientNotThrottled (Review Focus
// 5): only misses spend the budget, so fifty signed-in users on one client
// address, five requests each, are all served by a lookup burst of 10.
func TestAppListener_ValidSessionsBehindOneClientNotThrottled(t *testing.T) {
	counter := &lookupCounter{SessionStore: NewInMemorySessionStore(30 * time.Minute)}
	frozen := time.Unix(1_700_000_000, 0)
	a := sessionKeyTestAuth(counter)
	a.WithSessionLookupLimiter(ratelimit.New(60, 10, 1000, func() time.Time { return frozen }), nil)
	mux := http.NewServeMux()
	MountSessionProbeRoute(mux, a, RateLimitWithKey(ratelimit.New(60, 20, 1000, nil), nil, nil, a.SessionRateLimitKey()))
	MountMeRoutes(mux, a, NewMeHandler(testSessionDomain))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	for u := 0; u < 50; u++ {
		sess := saveSession(t, counter, fmt.Sprintf("sess-nat-%d", u))
		for j := 0; j < 5; j++ {
			path := "/v1/me"
			if j%2 == 0 {
				path = "/v1/session"
			}
			req, _ := http.NewRequest(http.MethodGet, srv.URL+path, nil)
			req.AddCookie(sess)
			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Fatalf("user %d %s: %v", u, path, err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("user %d request %d %s = %d, want 200 — valid sessions must not spend the unknown-cookie budget", u, j, path, resp.StatusCode)
			}
		}
	}
}
