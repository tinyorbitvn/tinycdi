// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package main

// Security-header, routing and server-timeout contract tests for the
// frontend (SEC-22 / SEC-23).

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/sessionhost"
	"github.com/tinyorbitvn/tinycdi/internal/tlsreload"
)

// newTestHandler builds the frontend handler over a scratch web root with
// an index.html and one hashed asset. sessionDomain is the raw
// -session-domain value (host[:port]); empty means unconfigured.
func newTestHandler(t *testing.T, sessionDomain string) http.Handler {
	t.Helper()
	var domain *sessionhost.Domain
	if sessionDomain != "" {
		d, err := sessionhost.ParseDomain(sessionDomain)
		if err != nil {
			t.Fatalf("ParseDomain(%q): %v", sessionDomain, err)
		}
		domain = &d
	}
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
	return newHandler(root, frontendCSP(domain))
}

func get(t *testing.T, h http.Handler, method, path string) *http.Response {
	t.Helper()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(method, "https://portal.test"+path, nil))
	return rr.Result()
}

// TestSecurityHeaders (SEC-22): every frontend response — SPA, SPA
// fallback, assets, health and errors alike — carries the CSP (portal never
// framed; the session-domain wildcard allowed as frame-src and form-action),
// COOP same-origin, nosniff, Referrer-Policy strict-origin and HSTS. The
// CSP is sized to the vite build output (external module script +
// stylesheet only, no inline code).
func TestSecurityHeaders(t *testing.T) {
	h := newTestHandler(t, "session.test")

	for _, path := range []string{"/", "/workspaces", "/workspaces/ws-1/session",
		"/assets/app-abc123.js", "/assets/missing.js", "/healthz", "/v1/workspaces"} {
		res := get(t, h, http.MethodGet, path)

		csp := res.Header.Get("Content-Security-Policy")
		for _, want := range []string{
			"default-src 'self'", "frame-ancestors 'none'",
			"object-src 'none'", "base-uri 'none'",
			"frame-src https://*.session.test",
			"form-action 'self' https://*.session.test",
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
// <label>.<sessionDomain> targeted at an iframe, so the wildcard session
// origin must be in both form-action and frame-src — and nothing else may
// be. With no session domain both collapse (fail closed).
func TestFrontendCSP(t *testing.T) {
	const base = "default-src 'self'; base-uri 'none'; object-src 'none'; frame-ancestors 'none'"
	if got, want := frontendCSP(nil), base+"; frame-src 'none'; form-action 'self'"; got != want {
		t.Fatalf("frontendCSP(nil) = %q, want %q", got, want)
	}
	for _, domain := range []string{"session.example.dev", "session.example.dev:8443"} {
		d, err := sessionhost.ParseDomain(domain)
		if err != nil {
			t.Fatal(err)
		}
		want := base + "; frame-src https://" + d.Wildcard() + "; form-action 'self' https://" + d.Wildcard()
		if got := frontendCSP(&d); got != want {
			t.Fatalf("frontendCSP(%q) = %q, want %q", domain, got, want)
		}
	}
}

// TestFrontendCSP_WildcardSessionDomain (D9/D14): every workspace lives on
// its own host under the session domain, so the portal CSP allows the
// whole wildcard — frame-src and form-action both name
// https://*.<sessionDomain>. A configured port is kept in both.
func TestFrontendCSP_WildcardSessionDomain(t *testing.T) {
	d, err := sessionhost.ParseDomain("session.example.com")
	if err != nil {
		t.Fatal(err)
	}
	csp := frontendCSP(&d)
	for _, want := range []string{
		"frame-src https://*.session.example.com",
		"form-action 'self' https://*.session.example.com",
	} {
		if !strings.Contains(csp, want) {
			t.Fatalf("CSP %q missing %q", csp, want)
		}
	}

	dp, err := sessionhost.ParseDomain("session.example.com:8443")
	if err != nil {
		t.Fatal(err)
	}
	csp = frontendCSP(&dp)
	for _, want := range []string{
		"frame-src https://*.session.example.com:8443",
		"form-action 'self' https://*.session.example.com:8443",
	} {
		if !strings.Contains(csp, want) {
			t.Fatalf("CSP %q missing %q", csp, want)
		}
	}
}

// TestFrontendHeaders_COOP (D14): the portal is a cross-origin opener for
// the session frame, so every response — SPA routes and hashed assets
// alike — carries Cross-Origin-Opener-Policy: same-origin.
func TestFrontendHeaders_COOP(t *testing.T) {
	h := newTestHandler(t, "session.example.com")
	for _, path := range []string{"/", "/assets/app-abc123.js"} {
		res := get(t, h, http.MethodGet, path)
		if got := res.Header.Get("Cross-Origin-Opener-Policy"); got != "same-origin" {
			t.Fatalf("%s: Cross-Origin-Opener-Policy = %q, want same-origin", path, got)
		}
	}
}

// TestNoAPIProxy: the frontend no longer proxies /v1 — the edge routes it
// to the backend. A /v1 request that reaches the frontend anyway must be a
// plain 404, never the SPA's index.html (which an API client would choke
// on as a 200).
func TestNoAPIProxy(t *testing.T) {
	h := newTestHandler(t, "session.test")
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
	h := newTestHandler(t, "session.test")

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

// TestSessionDomainParsing: -session-domain is validated by
// sessionhost.ParseDomain — a bare lower-case DNS domain with an optional
// port, never a wildcard, scheme, path or IP literal. The domain goes into
// the CSP via Wildcard(), so anything that could inject a directive or
// name a different host is a config error (SEC-26).
func TestSessionDomainParsing(t *testing.T) {
	ok := map[string]string{
		"session.example.dev":      "*.session.example.dev",
		"session.example.dev:8443": "*.session.example.dev:8443",
	}
	for in, want := range ok {
		d, err := sessionhost.ParseDomain(in)
		if err != nil || d.Wildcard() != want {
			t.Fatalf("ParseDomain(%q).Wildcard() = %q, %v; want %q", in, d.Wildcard(), err, want)
		}
	}

	for _, bad := range []string{
		"https://session.example.dev", // schemes not allowed
		"session.example.dev/app",     // path not allowed
		"*.session.example.dev",       // the wildcard is derived, not configured
		"SESSION.Example.Dev",         // DNS labels are lower case
		"session.example.dev.",        // trailing dot
		"10.0.0.1",                    // IP literal
		"session.example.dev:99999",   // port out of range
		"*.example.dev; script-src *", // CSP injection
		"example.dev' frame-src *",    // CSP injection
	} {
		if d, err := sessionhost.ParseDomain(bad); err == nil {
			t.Fatalf("ParseDomain(%q) = %q, want error", bad, d.Wildcard())
		}
	}
}

// TestHotReload (D21): the frontend terminates TLS through
// tlsreload.Reloader, so rotating the certificate files changes what a new
// handshake presents — while a connection opened before the rotation keeps
// working.
func TestHotReload(t *testing.T) {
	dir := t.TempDir()
	cert, key := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	writePair(t, cert, key, "first.example")

	srv, reloader, err := newTLSServer("127.0.0.1:0", cert, key,
		newTestHandler(t, "session.test"), tlsreload.WithInterval(10*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go reloader.Run(ctx)

	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		t.Fatal(err)
	}
	// ServeTLS with empty cert/key paths: GetCertificate supplies the pair.
	go srv.ServeTLS(ln, "", "")
	defer srv.Close()

	if got := handshakeCN(t, ln.Addr().String()); got != "first.example" {
		t.Fatalf("initial CN = %q", got)
	}
	old, err := tls.Dial("tcp", ln.Addr().String(), &tls.Config{InsecureSkipVerify: true}) // #nosec G402 -- test
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()

	writePair(t, cert, key, "second.example")
	eventually(t, 2*time.Second, func() bool {
		return handshakeCN(t, ln.Addr().String()) == "second.example"
	})

	// The pre-rotation connection must still answer (D21: streams stay up).
	if _, err := fmt.Fprintf(old, "GET /healthz HTTP/1.1\r\nHost: portal.test\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	res, err := http.ReadResponse(bufio.NewReader(old), nil)
	if err != nil {
		t.Fatalf("request on the pre-rotation connection: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /healthz on the pre-rotation connection = %d, want 200", res.StatusCode)
	}
}

// handshakeCN dials addr with TLS and returns the CommonName of the leaf
// certificate the server presents.
func handshakeCN(t *testing.T, addr string) string {
	t.Helper()
	c, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true}) // #nosec G402 -- test
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	return c.ConnectionState().PeerCertificates[0].Subject.CommonName
}

// writePair generates a self-signed ECDSA P-256 pair with the given CN and
// writes both files via temp-file + rename.
func writePair(t *testing.T, certFile, keyFile, cn string) {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	writeRename(t, certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	writeRename(t, keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
}

func writeRename(t *testing.T, path string, data []byte) {
	t.Helper()
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tmp.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := tmp.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		t.Fatal(err)
	}
}

// eventually polls cond every 5 ms until it returns true or the deadline
// passes, then fails the test.
func eventually(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met before deadline")
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
