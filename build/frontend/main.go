// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

// Command frontend serves the built portal SPA (web/dist) over HTTPS with
// the portal's browser security policy. It is a static file server only:
// the public API (/v1/*) is served by the backend on the same portal host,
// and the edge (Ingress / Gateway API) routes /v1/ there by path — this
// server never proxies it.
//
// Served by the image built from build/frontend/Dockerfile.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/tlsreload"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	var (
		listen        string
		webRoot       string
		tlsCert       string
		tlsKey        string
		sessionOrigin string
		brandingDir   string
	)
	flag.StringVar(&listen, "listen", envOr("TCDI_FRONTEND_LISTEN", ":8443"), "HTTPS listen address")
	flag.StringVar(&webRoot, "web-root", envOr("TCDI_FRONTEND_WEB_ROOT", "/srv/web"), "directory with the built SPA assets")
	flag.StringVar(&tlsCert, "tls-cert", envOr("TCDI_FRONTEND_TLS_CERT", ""), "TLS cert file (required)")
	flag.StringVar(&tlsKey, "tls-key", envOr("TCDI_FRONTEND_TLS_KEY", ""), "TLS key file (required)")
	flag.StringVar(&sessionOrigin, "session-origin", envOr("TCDI_SESSION_ORIGIN", ""),
		"public session origin (https://host[:port]) the in-portal session iframe loads and the launch form POSTs to; "+
			"added to CSP frame-src and form-action")
	flag.StringVar(&brandingDir, "branding-dir", envOr("TCDI_FRONTEND_BRANDING_DIR", ""),
		"optional directory with branding overrides (branding.json, tokens.css, logo files) served at /branding/")
	flag.Parse()

	if tlsCert == "" || tlsKey == "" {
		fmt.Fprintln(os.Stderr, "config: -tls-cert and -tls-key are required")
		os.Exit(2)
	}
	// The session origin goes verbatim into the CSP, so it must be a bare
	// https origin — anything else (http:, a path, userinfo) is a config
	// error, not a degraded startup.
	if sessionOrigin != "" {
		so, err := normalizeSessionOrigin(sessionOrigin)
		if err != nil {
			fmt.Fprintln(os.Stderr, "config:", err)
			os.Exit(2)
		}
		sessionOrigin = so
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if sessionOrigin == "" {
		// Fail closed: frame-src 'none' and form-action 'self', so sessions
		// are CSP-blocked until the session origin is configured.
		log.Warn("no session origin configured: CSP blocks the session frame and launch form; sessions will not open")
	}

	srv, reloader, err := newTLSServer(listen, tlsCert, tlsKey,
		newHandler(webRoot, brandingDir, frontendCSP(sessionOrigin)), tlsreload.WithLogger(log))
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		os.Exit(2)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go reloader.Run(ctx)
	log.Info("frontend listening", "addr", listen, "sessionOrigin", sessionOrigin)
	if err := srv.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("serve", "err", err)
		os.Exit(1)
	}
}

// cspBase is sized to the vite build output (external module script +
// stylesheet only — no inline scripts/styles, eval or workers), so nothing
// unsafe is needed. The portal itself is never framed (frame-ancestors).
const cspBase = "default-src 'self'; base-uri 'none'; object-src 'none'; frame-ancestors 'none'"

// frontendCSP returns the portal CSP for the configured session origin.
//
// The SPA shows a session inside the portal: it submits a form POST
// carrying the launch ticket to the session origin, targeted at an iframe
// that then stays on the session origin for the stream. So the session
// origin must be allowed by both form-action (the POST) and frame-src (the
// iframe navigation). sessionOrigin is a normalized bare https origin;
// empty fails closed — frame-src 'none', form-action 'self'.
func frontendCSP(sessionOrigin string) string {
	if sessionOrigin == "" {
		return cspBase + "; frame-src 'none'; form-action 'self'"
	}
	return cspBase + "; frame-src " + sessionOrigin + "; form-action 'self' " + sessionOrigin
}

// normalizeSessionOrigin validates that raw is a bare https origin and
// returns its normalized form: scheme://lowercased-host[:non-default-port]
// — the same rule the backend applies to -session-origin (SEC-26).
func normalizeSessionOrigin(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil ||
		u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.RawFragment != "" {
		return "", fmt.Errorf("bad session-origin %q (want https://host[:port])", raw)
	}
	host := strings.ToLower(u.Host)
	if u.Port() == "443" {
		host = strings.TrimSuffix(host, ":443")
	}
	return "https://" + host, nil
}

// securityHeaders pins the browser policy on every frontend response
// (SEC-22): strict CSP, framing of the portal denied, nosniff,
// Referrer-Policy and HSTS.
//
// Referrer-Policy must be strict-origin (or strict-origin-when-cross-origin),
// never no-referrer/same-origin: the SPA launches a desktop with a
// cross-site form POST to the session origin, and the browser derives that
// POST's Origin header from the effective referrer — a policy that
// suppresses the referrer makes it send Origin: null, which the gateway's
// launch gate rejects as bad_origin (ADR 0004). strict-origin keeps
// the privacy property: cross-site requests leak only the bare origin,
// never the path or query.
func securityHeaders(csp string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "strict-origin")
		h.Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
		next.ServeHTTP(w, r)
	})
}

// newHandler builds the frontend mux: the built SPA with the security
// headers applied to all of it, plus the optional branding directory at
// /branding/ (same origin, so the CSP is unchanged).
func newHandler(webRoot, brandingDir, csp string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	// /v1/ belongs to the backend; the edge routes it there by path. If a
	// misconfigured route sends it here, answer 404 rather than the SPA
	// fallback, so the API client sees a clear error instead of HTML.
	mux.HandleFunc("/v1/", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "the API is served by the backend; check the /v1/ route", http.StatusNotFound)
	})
	// SPA fallback: real asset paths serve files; everything else (client
	// routes like /workspaces/:id/session) serves index.html.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		p := filepath.Clean("/" + r.URL.Path) // force under webRoot
		full := filepath.Join(webRoot, p)
		if st, err := os.Stat(full); err == nil && st.Mode().IsRegular() && filepath.Base(p) != "index.html" {
			if strings.HasPrefix(p, "/assets/") {
				// vite content-hashes everything under assets/.
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			}
			http.ServeFile(w, r, full)
			return
		}
		if strings.Contains(filepath.Base(p), ".") && filepath.Base(p) != "index.html" {
			http.NotFound(w, r) // looks like an asset that does not exist
			return
		}
		// index.html references the hashed bundle names, so it must be
		// revalidated or a deploy leaves browsers on stale bundles.
		w.Header().Set("Cache-Control", "no-cache")
		serveIndex(w, r, filepath.Join(webRoot, "index.html"))
	})
	// /branding/ is intercepted on the raw request path, ahead of the mux:
	// ServeMux canonicalises ".."-bearing paths with a redirect, but the
	// branding contract answers them 404.
	return securityHeaders(csp, branding(brandingDir, mux))
}

// branding routes requests under /branding/ to the branding-directory
// handler and passes everything else through.
func branding(dir string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/branding" || strings.HasPrefix(r.URL.Path, "/branding/") {
			serveBranding(dir, w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// serveBranding serves regular files from the branding directory
// (-branding-dir / TCDI_FRONTEND_BRANDING_DIR; the chart mounts a
// ConfigMap there). GET/HEAD only, no directory listing, Cache-Control
// no-cache. Escapes are impossible: ".." segments are rejected outright,
// and a file is served only when its fully-resolved path stays inside the
// resolved branding dir — a ConfigMap mount makes every key a symlink
// into ..data, so containment is checked after EvalSymlinks, which also
// defeats symlinks pointing outside.
//
// tokens.css is special: index.html always links it, so when the dir or
// the file is absent it answers 200 with an empty text/css body — the
// console stays clean. Every other absent name, including branding.json
// without -branding-dir, answers 404 (never the SPA fallback, which would
// hand the app's loadBranding an HTML document).
func serveBranding(dir string, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name, under := strings.CutPrefix(r.URL.Path, "/branding/")
	if !under || name == "" || hasDotDotSegment(name) {
		http.NotFound(w, r) // /branding, the bare listing, or traversal
		return
	}
	full := ""
	if dir != "" {
		if root, err := filepath.EvalSymlinks(dir); err == nil {
			if resolved, err := filepath.EvalSymlinks(filepath.Join(dir, name)); err == nil &&
				strings.HasPrefix(resolved, root+string(filepath.Separator)) {
				if st, err := os.Stat(resolved); err == nil && st.Mode().IsRegular() {
					full = resolved
				}
			}
		}
	}
	if full == "" {
		if name == "tokens.css" {
			w.Header().Set("Content-Type", "text/css")
			w.Header().Set("Cache-Control", "no-cache")
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(full) // #nosec G304 -- resolved path proven inside -branding-dir above
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	// ServeContent (not ServeFile): the request URL is the branding path,
	// not the file's, so no "/index.html" redirect or second ".." check
	// can trigger.
	http.ServeContent(w, r, filepath.Base(full), st.ModTime(), f)
}

// hasDotDotSegment reports whether p contains a literal ".." path segment
// (the URL path is already percent-decoded, so %2e%2e lands here as "..").
func hasDotDotSegment(p string) bool {
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return true
		}
	}
	return false
}

// serveIndex writes index.html without http.ServeFile's redirect of
// ".../index.html" to "./", so every SPA route answers 200 in place.
func serveIndex(w http.ResponseWriter, r *http.Request, path string) {
	f, err := os.Open(path) // #nosec G304 -- fixed file under the configured web root
	if err != nil {
		http.Error(w, "index unavailable", http.StatusInternalServerError)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		http.Error(w, "index unavailable", http.StatusInternalServerError)
		return
	}
	http.ServeContent(w, r, "index.html", st.ModTime(), f)
}

// newTLSServer builds the frontend listener with certificate hot-reload
// (D21): the reloader re-reads certFile/keyFile when they change and
// TLSConfig.GetCertificate hands out the newest pair, so a rotated Secret
// takes effect on the next handshake without a restart. It also fails
// startup here if the pair is unusable. Callers run reloader.Run until
// shutdown; ServeTLS/ListenAndServeTLS are then called with empty
// cert/key paths.
func newTLSServer(addr, certFile, keyFile string, h http.Handler, opts ...tlsreload.Option) (*http.Server, *tlsreload.Reloader, error) {
	r, err := tlsreload.New(certFile, keyFile, opts...)
	if err != nil {
		return nil, nil, err
	}
	srv := newServer(addr, h)
	srv.TLSConfig.GetCertificate = r.GetCertificate
	return srv, r, nil
}

// newServer builds the frontend listener (SEC-23): all request phases are
// bounded — this server only serves static files, so full read/write
// timeouts are safe (no WebSockets, no proxying).
func newServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      120 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
		TLSConfig:         &tls.Config{MinVersion: tls.VersionTLS12},
	}
}
