package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
	csrf := findCookie(cookies, env.auth.CSRFCookieName())

	req, _ := http.NewRequest(http.MethodPost, env.server.URL+"/v1/echo-owner", strings.NewReader("{}"))
	req.AddCookie(sess)
	req.Header.Set(env.auth.CSRFHeader(), csrf.Value)
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
	csrf := findCookie(cookies, env.auth.CSRFCookieName())

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
		csrf.Value,
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
		`# HELP tinycdi_http_requests_total HTTP requests handled, by route template, method and response code class.
# TYPE tinycdi_http_requests_total counter
tinycdi_http_requests_total{code_class="3xx",method="GET",route="/auth/login"} 1
tinycdi_http_requests_total{code_class="3xx",method="GET",route="/auth/callback"} 1
tinycdi_http_requests_total{code_class="2xx",method="GET",route="/v1/me"} 1
tinycdi_http_requests_total{code_class="4xx",method="GET",route="/v1/me"} 1
tinycdi_http_requests_total{code_class="4xx",method="GET",route="unmatched"} 1
`), "tinycdi_http_requests_total")
	if err != nil {
		t.Fatalf("http request metrics mismatch:\n%v", err)
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
	csrf := findCookie(cookies, env.auth.CSRFCookieName())

	srv := httptest.NewServer(RequestID(RequireTrustedOrigin(env.auth.SessionCookieName(), []string{"https://portal.test"})(
		env.auth.RequireAuth(env.auth.RequireCSRF(http.HandlerFunc(echoOwnerHandler))))))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/echo-owner", strings.NewReader("{}"))
	req.AddCookie(sess)
	req.Header.Set(env.auth.CSRFHeader(), csrf.Value)
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
