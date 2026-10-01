package main

// Security-header and server-timeout contract tests for the portal
// (SEC-22 / SEC-23, security review 20261001).

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newTestHandler builds the portal handler over a scratch web root.
func newTestHandler(t *testing.T, sessionOrigin string) http.Handler {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "index.html"),
		[]byte("<html><body>portal</body></html>"), 0o600); err != nil {
		t.Fatal(err)
	}
	up, err := url.Parse("http://api.invalid")
	if err != nil {
		t.Fatal(err)
	}
	return newHandler(root, up, portalCSP(sessionOrigin), slog.New(slog.NewJSONHandler(os.Stderr, nil)))
}

// TestSecurityHeaders (SEC-22): every portal response — SPA, SPA-fallback
// and API proxy alike — carries CSP with frame-ancestors 'none', nosniff,
// Referrer-Policy and HSTS. The CSP is sized to the vite build output
// (external module script + stylesheet only, no inline code), verified
// against `npm --prefix web run build` artifacts.
func TestSecurityHeaders(t *testing.T) {
	h := newTestHandler(t, "https://session.test")

	for _, path := range []string{"/", "/workspaces", "/assets/app.js", "/healthz"} {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "https://portal.test"+path, nil)
		h.ServeHTTP(rr, req)
		res := rr.Result()

		csp := res.Header.Get("Content-Security-Policy")
		for _, want := range []string{
			"default-src 'self'", "frame-ancestors 'none'",
			"object-src 'none'", "base-uri 'none'",
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
		// must be strict-origin (or strict-origin-when-cross-origin),
		// never no-referrer/same-origin — those suppress the effective
		// referrer, so the browser sends Origin: null on the cross-site
		// launch form POST and the gateway rejects it as bad_origin
		// (ADR 0004): every launch 403s. strict-origin still sends only the
		// bare origin — never the path or query — on cross-site requests.
		if got := res.Header.Get("Referrer-Policy"); got != "strict-origin" {
			t.Fatalf("%s: Referrer-Policy = %q, want strict-origin", path, got)
		}
		if got := res.Header.Get("Strict-Transport-Security"); got == "" {
			t.Fatalf("%s: missing HSTS", path)
		}
	}
}

// TestPortalCSP (launch regression): the SPA opens a desktop by
// submitting a cross-origin form POST carrying the launch ticket to the
// session origin, so form-action must be 'self' PLUS the configured
// session origin — 'self' alone blocks every launch. With no session
// origin the directive must collapse to 'self' (fail closed) and no other
// source may be added.
func TestPortalCSP(t *testing.T) {
	const base = "default-src 'self'; base-uri 'none'; object-src 'none'; " +
		"frame-ancestors 'none'; frame-src 'none'; form-action 'self'"
	if got := portalCSP(""); got != base {
		t.Fatalf("portalCSP(\"\") = %q, want %q", got, base)
	}
	for origin, want := range map[string]string{
		"https://session.example.dev":      base + " https://session.example.dev",
		"https://session.example.dev:8443": base + " https://session.example.dev:8443",
	} {
		if got := portalCSP(origin); got != want {
			t.Fatalf("portalCSP(%q) = %q, want %q", origin, got, want)
		}
	}
}

// TestNormalizeSessionOrigin: same rule the API applies to -session-origin
// (SEC-26) — a bare https origin, no path/query/fragment/userinfo —
// serialized the way a browser serializes URL.origin.
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
	} {
		if got, err := normalizeSessionOrigin(bad); err == nil {
			t.Fatalf("normalizeSessionOrigin(%q) = %q, want error", bad, got)
		}
	}
}

// TestServerTimeouts (SEC-23): the portal listener bounds every phase of a
// request — it proxies only short JSON /v1 calls, so full read/write
// timeouts are safe (no long-lived WebSockets on this server).
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
}
