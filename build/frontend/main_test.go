// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package main

// Security-header, routing and server-timeout contract tests for the
// frontend (SEC-22 / SEC-23).

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newTestHandler builds the frontend handler over a scratch web root with
// an index.html and one hashed asset.
func newTestHandler(t *testing.T, sessionOrigin string) http.Handler {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "index.html"),
		[]byte("<html><body>portal</body></html>"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "assets"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "assets", "app-abc123.js"),
		[]byte("console.log(1)"), 0o600); err != nil {
		t.Fatal(err)
	}
	return newHandler(root, frontendCSP(sessionOrigin))
}

func get(t *testing.T, h http.Handler, method, path string) *http.Response {
	t.Helper()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(method, "https://portal.test"+path, nil))
	return rr.Result()
}

// TestSecurityHeaders (SEC-22): every frontend response — SPA, SPA
// fallback, assets, health and errors alike — carries the CSP (portal never
// framed; session origin allowed as frame-src and form-action), nosniff,
// Referrer-Policy strict-origin and HSTS. The CSP is sized to the vite
// build output (external module script + stylesheet only, no inline code).
func TestSecurityHeaders(t *testing.T) {
	h := newTestHandler(t, "https://session.test")

	for _, path := range []string{"/", "/workspaces", "/workspaces/ws-1/session",
		"/assets/app-abc123.js", "/assets/missing.js", "/healthz", "/v1/workspaces"} {
		res := get(t, h, http.MethodGet, path)

		csp := res.Header.Get("Content-Security-Policy")
		for _, want := range []string{
			"default-src 'self'", "frame-ancestors 'none'",
			"object-src 'none'", "base-uri 'none'",
			"frame-src https://session.test",
			"form-action 'self' https://session.test",
		} {
			if !strings.Contains(csp, want) {
				t.Fatalf("%s: CSP %q missing %q", path, csp, want)
			}
		}
		if strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "unsafe-eval") {
			t.Fatalf("%s: CSP allows inline/eval, vite build does not need it: %q", path, csp)
		}
		if got := res.Header.Get("X-Content-Type-Options"); got != "nosniff" {
			t.Fatalf("%s: X-Content-Type-Options = %q", path, got)
		}
		if got := res.Header.Get("X-Frame-Options"); got != "DENY" {
			t.Fatalf("%s: X-Frame-Options = %q, want DENY (the portal is never framed)", path, got)
		}
		// must be strict-origin (or strict-origin-when-cross-origin),
		// never no-referrer/same-origin — those suppress the effective
		// referrer, so the browser sends Origin: null on the cross-site
		// launch form POST and the gateway rejects it as bad_origin
		// (ADR 0004): every launch 403s. strict-origin still sends only the
		// bare origin — never the path or query — on cross-site requests.
		if got := res.Header.Get("Referrer-Policy"); got != "strict-origin" {
			t.Fatalf("%s: Referrer-Policy = %q, want strict-origin", path, got)
		}
		if got := res.Header.Get("Strict-Transport-Security"); !strings.HasPrefix(got, "max-age=") {
			t.Fatalf("%s: HSTS = %q", path, got)
		}
	}
}

// TestFrontendCSP: the in-portal session view submits the launch form to
// the session origin targeted at an iframe, so the session origin must be
// in both form-action and frame-src — and nothing else may be. With no
// session origin both collapse (fail closed).
func TestFrontendCSP(t *testing.T) {
	const base = "default-src 'self'; base-uri 'none'; object-src 'none'; frame-ancestors 'none'"
	if got, want := frontendCSP(""), base+"; frame-src 'none'; form-action 'self'"; got != want {
		t.Fatalf("frontendCSP(\"\") = %q, want %q", got, want)
	}
	for _, origin := range []string{"https://session.example.dev", "https://session.example.dev:8443"} {
		want := base + "; frame-src " + origin + "; form-action 'self' " + origin
		if got := frontendCSP(origin); got != want {
			t.Fatalf("frontendCSP(%q) = %q, want %q", origin, got, want)
		}
	}
}

// TestNoAPIProxy: the frontend no longer proxies /v1 — the edge routes it
// to the backend. A /v1 request that reaches the frontend anyway must be a
// plain 404, never the SPA's index.html (which an API client would choke
// on as a 200).
func TestNoAPIProxy(t *testing.T) {
	h := newTestHandler(t, "https://session.test")
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		res := get(t, h, method, "/v1/workspaces")
		if res.StatusCode != http.StatusNotFound {
			t.Fatalf("%s /v1/workspaces = %d, want 404", method, res.StatusCode)
		}
		body, _ := io.ReadAll(res.Body)
		if strings.Contains(string(body), "<html>") {
			t.Fatalf("%s /v1/workspaces served the SPA: %q", method, body)
		}
	}
}

// TestSPARouting: client routes get index.html (200, revalidated), real
// assets are served with long-lived caching, missing assets 404 and
// non-GET methods are refused.
func TestSPARouting(t *testing.T) {
	h := newTestHandler(t, "https://session.test")

	for _, path := range []string{"/", "/workspaces", "/workspaces/ws-1/session", "/index.html", "/admin/templates"} {
		res := get(t, h, http.MethodGet, path)
		body, _ := io.ReadAll(res.Body)
		if res.StatusCode != http.StatusOK || !strings.Contains(string(body), "portal") {
			t.Fatalf("GET %s = %d %q, want 200 index.html", path, res.StatusCode, body)
		}
		if got := res.Header.Get("Cache-Control"); got != "no-cache" {
			t.Fatalf("GET %s: Cache-Control = %q, want no-cache", path, got)
		}
	}

	res := get(t, h, http.MethodGet, "/assets/app-abc123.js")
	if res.StatusCode != http.StatusOK || !strings.Contains(res.Header.Get("Cache-Control"), "immutable") {
		t.Fatalf("asset = %d, Cache-Control %q", res.StatusCode, res.Header.Get("Cache-Control"))
	}
	if res := get(t, h, http.MethodGet, "/assets/missing.js"); res.StatusCode != http.StatusNotFound {
		t.Fatalf("missing asset = %d, want 404", res.StatusCode)
	}
	// ServeMux redirects unclean paths to their clean form; the handler
	// itself re-cleans under the web root, so nothing outside it is served.
	if res := get(t, h, http.MethodGet, "/../../etc/passwd"); res.StatusCode == http.StatusOK {
		t.Fatalf("traversal = %d, want a redirect or 404", res.StatusCode)
	}
	if res := get(t, h, http.MethodPost, "/workspaces"); res.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST /workspaces = %d, want 405", res.StatusCode)
	}
	if res := get(t, h, http.MethodGet, "/healthz"); res.StatusCode != http.StatusOK {
		t.Fatalf("healthz = %d", res.StatusCode)
	}
}

// TestNormalizeSessionOrigin: same rule the backend applies to
// -session-origin (SEC-26) — a bare https origin, no path/query/fragment/
// userinfo — serialized the way a browser serializes URL.origin.
func TestNormalizeSessionOrigin(t *testing.T) {
	ok := map[string]string{
		"https://session.example.dev":      "https://session.example.dev",
		"https://SESSION.Example.Dev:8443": "https://session.example.dev:8443",
		"https://session.example.dev:443":  "https://session.example.dev",
	}
	for in, want := range ok {
		got, err := normalizeSessionOrigin(in)
		if err != nil || got != want {
			t.Fatalf("normalizeSessionOrigin(%q) = %q, %v; want %q", in, got, err, want)
		}
	}

	for _, bad := range []string{
		"http://session.example.dev",       // plaintext must not carry tickets
		"javascript:alert(1)",              // non-http scheme
		"session.example.dev",              // missing scheme
		"https://session.example.dev/",     // path not allowed
		"https://session.example.dev/app",  // path not allowed
		"https://session.example.dev/?x=1", // query not allowed
		"https://session.example.dev#frag", // fragment not allowed
		"https://user@session.example.dev", // userinfo not allowed
		"https://user:pw@session.example.dev",
		"https://a.example.dev; script-src *", // CSP injection
	} {
		if got, err := normalizeSessionOrigin(bad); err == nil {
			t.Fatalf("normalizeSessionOrigin(%q) = %q, want error", bad, got)
		}
	}
}

// TestServerTimeouts (SEC-23): the frontend listener bounds every phase of
// a request — it serves only static files, so full read/write timeouts are
// safe.
func TestServerTimeouts(t *testing.T) {
	srv := newServer(":0", http.NotFoundHandler())
	for name, v := range map[string]time.Duration{
		"ReadHeaderTimeout": srv.ReadHeaderTimeout,
		"ReadTimeout":       srv.ReadTimeout,
		"WriteTimeout":      srv.WriteTimeout,
		"IdleTimeout":       srv.IdleTimeout,
	} {
		if v <= 0 {
			t.Fatalf("%s = %v, want > 0", name, v)
		}
	}
	if srv.MaxHeaderBytes <= 0 {
		t.Fatal("MaxHeaderBytes must be set")
	}
	if srv.TLSConfig == nil || srv.TLSConfig.MinVersion < 0x0303 {
		t.Fatal("TLS 1.2 minimum must be set")
	}
}
