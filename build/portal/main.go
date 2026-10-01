// Command dev-portal serves the built portal SPA and reverse-proxies /v1
// to the platform API, so portal and API share one origin — the same model
// the vite dev server assumes ("portal is served on the portal origin
// alongside the API"). Terminates TLS with the dev-CA portal certificate.
//
// Served by the image built from build/portal/Dockerfile.
package main

import (
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
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
		upstream      string
		tlsCert       string
		tlsKey        string
		sessionOrigin string
	)
	flag.StringVar(&listen, "listen", envOr("TCDI_PORTAL_LISTEN", ":8443"), "HTTPS listen address")
	flag.StringVar(&webRoot, "web-root", envOr("TCDI_PORTAL_WEB_ROOT", "/srv/web"), "directory with built portal assets")
	flag.StringVar(&upstream, "api-upstream", envOr("TCDI_PORTAL_API", "http://api.tcdi-system.svc:8080"), "platform API base URL")
	flag.StringVar(&tlsCert, "tls-cert", envOr("TCDI_PORTAL_TLS_CERT", ""), "TLS cert file (required)")
	flag.StringVar(&tlsKey, "tls-key", envOr("TCDI_PORTAL_TLS_KEY", ""), "TLS key file (required)")
	flag.StringVar(&sessionOrigin, "session-origin", envOr("TCDI_SESSION_ORIGIN", ""),
		"public session origin (https://host[:port]) the launch form POSTs to; added to CSP form-action")
	flag.Parse()

	if tlsCert == "" || tlsKey == "" {
		fmt.Fprintln(os.Stderr, "config: -tls-cert and -tls-key are required")
		os.Exit(2)
	}
	up, err := url.Parse(upstream)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config: bad api-upstream:", err)
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
		// Fail closed: form-action stays 'self', so browser launches are
		// CSP-blocked until the session origin is configured.
		log.Warn("no session origin configured: CSP form-action is 'self' only; launches will be blocked")
	}

	srv := newServer(listen, newHandler(webRoot, up, portalCSP(sessionOrigin), log))
	log.Info("portal listening", "addr", listen, "api", upstream)
	if err := srv.ListenAndServeTLS(tlsCert, tlsKey); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("serve", "err", err)
		os.Exit(1)
	}
}

// portalCSPBase is sized to the vite build output (verified against
// `npm --prefix web run build`: external module script + stylesheet only —
// no inline scripts/styles, eval or workers), so nothing unsafe is needed.
const portalCSPBase = "default-src 'self'; base-uri 'none'; object-src 'none'; " +
	"frame-ancestors 'none'; frame-src 'none'; form-action 'self'"

// portalCSP extends form-action with the configured session origin: the
// SPA opens a session by submitting a cross-origin form POST carrying the
// launch ticket (web/src/workspaces/ConnectButton.tsx), so 'self' alone
// blocks every launch. sessionOrigin is normalized to a bare https origin;
// empty fails closed to 'self' only.
func portalCSP(sessionOrigin string) string {
	if sessionOrigin == "" {
		return portalCSPBase
	}
	return portalCSPBase + " " + sessionOrigin
}

// normalizeSessionOrigin validates that raw is a bare https origin and
// returns its normalized form: scheme://lowercased-host[:non-default-port]
// — the same rule the API applies to -session-origin (SEC-26).
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

// securityHeaders pins the browser policy on every portal response
// (SEC-22): strict CSP with framing denied, nosniff, Referrer-Policy and
// HSTS.
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

// newHandler builds the portal mux: /v1 proxies to the platform API, the
// rest serves the built SPA with the security headers applied to all of it.
func newHandler(webRoot string, up *url.URL, csp string, log *slog.Logger) http.Handler {
	proxy := httputil.NewSingleHostReverseProxy(up)
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, e error) {
		log.Error("api proxy", "err", e)
		http.Error(w, "api unreachable", http.StatusBadGateway)
	}

	mux := http.NewServeMux()
	mux.Handle("/v1/", proxy)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	// SPA fallback: real asset paths serve files; everything else (client
	// routes like /workspaces) serves index.html.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		p := filepath.Clean("/" + r.URL.Path) // force under webRoot
		full := filepath.Join(webRoot, p)
		if st, err := os.Stat(full); err == nil && st.Mode().IsRegular() {
			http.ServeFile(w, r, full)
			return
		}
		if strings.Contains(filepath.Base(p), ".") {
			http.NotFound(w, r) // looks like an asset that does not exist
			return
		}
		http.ServeFile(w, r, filepath.Join(webRoot, "index.html"))
	})
	return securityHeaders(csp, mux)
}

// newServer builds the portal listener (SEC-23): all request phases are
// bounded — this server proxies only short JSON /v1 calls and serves
// static files, so full read/write timeouts are safe (no WebSockets).
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
