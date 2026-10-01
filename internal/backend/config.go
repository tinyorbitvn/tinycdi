// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

// Package backend wires the merged tinycdi backend binary: the public API,
// the session gateway and the in-process broker on four listeners (app,
// session, internal mTLS, metrics) with independent TLS configuration.
//
// Flag surface follows decisions-2 item 1: app flags are the former cmd/api
// set, the session flags take a "session-" prefix, the internal/mTLS
// listener keeps its names, and -broker-url/-broker-ca/-mtls-cert/-mtls-key
// select split/test mode (session listener only, remote broker, no DB/OIDC/
// Kubernetes). Every flag's environment variable is TCDI_<UPPER_SNAKE>;
// pinned exceptions: TCDI_PORTAL_ORIGINS (repeatable flag), KUBECONFIG,
// and the env-only secrets TCDI_OIDC_CLIENT_SECRET and TCDI_CONTROL_TOKEN
// (legacy TCDI_GW_CONTROL_TOKEN still read).
package backend

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/tinyorbitvn/tinycdi/internal/api"
	"github.com/tinyorbitvn/tinycdi/internal/broker/httpapi"
	"github.com/tinyorbitvn/tinycdi/internal/gateway"
	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
)

// stringList collects repeated flags and comma-separated values into one
// list (used for -portal-origin / TCDI_PORTAL_ORIGINS and -login-key-file).
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }

func (s *stringList) Set(v string) error {
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			*s = append(*s, p)
		}
	}
	return nil
}

// groupList collects -required-groups values (repeatable or CSV). Unlike
// stringList it rejects empty entries outright: a blank group name is a
// config typo and must not silently alter what the login gate requires.
type groupList []string

func (g *groupList) String() string { return strings.Join(*g, ",") }

func (g *groupList) Set(v string) error {
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p == "" {
			return errors.New("empty group name")
		}
		*g = append(*g, p)
	}
	return nil
}

// controlTokenUnsetWarning is logged at startup when no control token is
// configured (SEC-I2): the /v1/control surface fails CLOSED in that state —
// it is disabled, not unauthenticated.
const controlTokenUnsetWarning = "control token unset — /v1/control/* is disabled (fail closed); set -control-token-file or TCDI_CONTROL_TOKEN to enable"

// Config is the parsed flag set for the merged backend.
type Config struct {
	// App listener (the former cmd/api surface).
	Listen               string // empty disables the app listener
	TLSCert              string // optional; empty serves plain HTTP
	TLSKey               string
	DatabaseURL          string
	OIDCIssuer           string
	OIDCClientID         string
	OIDCClientSecret     string // env only — never a flag value
	OIDCRedirectURL      string
	RequiredGroups       groupList
	DevInsecureDB        bool
	DBSSLMode            string // resolved sslmode label, for logging
	TenantNamespaces     string
	SessionIdle          time.Duration
	Kubeconfig           string
	ExpiryInterval       time.Duration
	RetainedSyncInterval time.Duration
	RecoveryInterval     time.Duration
	ImageStaleAfter      time.Duration

	// Session listener (the former cmd/gateway surface).
	SessionListen       string // empty disables the session listener
	SessionTLSCert      string // required when the session listener is on
	SessionTLSKey       string
	SessionAllowedHosts string // comma-separated Host allowlist
	SessionCookieMode   string // lax | partitioned
	UpstreamCA          string
	ControlTokenFile    string
	ControlToken        string // env/file only — never a flag value
	TenantAllowlist     string // bounds metrics label cardinality
	RenewInterval       time.Duration
	RevokeDeadline      time.Duration

	// Internal mTLS listener (the broker/operator surface, ADR 0003).
	InternalListen   string // empty disables the internal listener
	InternalTLSCert  string
	InternalTLSKey   string
	InternalClientCA string
	OperatorCN       string

	// Metrics listener (scrape-only, plain HTTP).
	MetricsListen string // empty disables

	// Shared.
	SessionOrigin   string // public session origin (scheme://host[:port])
	PortalOrigins   stringList
	GatewayID       string
	GatewayAudience string
	LoginKeyFiles   stringList // first file seals; all open (rotation)

	// Split/test mode: session listener over a remote mTLS broker.
	BrokerURL string
	BrokerCA  string
	MTLSCert  string
	MTLSKey   string
}

// SplitMode reports whether the config selects split/test mode (any of the
// remote-broker flags set).
func (c Config) SplitMode() bool {
	return c.BrokerURL != "" || c.BrokerCA != "" || c.MTLSCert != "" || c.MTLSKey != ""
}

func envOr(getenv func(string) string, key, def string) string {
	if getenv != nil {
		if v := getenv(key); v != "" {
			return v
		}
	}
	return def
}

func envDur(getenv func(string) string, key string, def time.Duration) time.Duration {
	if getenv != nil {
		if v := getenv(key); v != "" {
			if d, err := time.ParseDuration(v); err == nil {
				return d
			}
		}
	}
	return def
}

// ParseFlags fills a Config from args (flag values) and getenv (TCDI_* env
// fallback: a flag's env is its name uppercased with '-' → '_'). A flag
// always wins over its env.
func ParseFlags(args []string, getenv func(string) string) (Config, error) {
	var c Config
	fs := flag.NewFlagSet("backend", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	// App listener.
	fs.StringVar(&c.Listen, "listen", envOr(getenv, "TCDI_LISTEN", ":8443"),
		"app listener address (empty disables it)")
	fs.StringVar(&c.TLSCert, "tls-cert", envOr(getenv, "TCDI_TLS_CERT", ""), "app listener TLS cert (PEM; empty serves plain HTTP)")
	fs.StringVar(&c.TLSKey, "tls-key", envOr(getenv, "TCDI_TLS_KEY", ""), "app listener TLS key (PEM)")
	fs.StringVar(&c.DatabaseURL, "database-url", envOr(getenv, "TCDI_DATABASE_URL", ""), "PostgreSQL DSN (env TCDI_DATABASE_URL)")
	fs.StringVar(&c.OIDCIssuer, "oidc-issuer", envOr(getenv, "TCDI_OIDC_ISSUER", ""), "OIDC issuer URL")
	fs.StringVar(&c.OIDCClientID, "oidc-client-id", envOr(getenv, "TCDI_OIDC_CLIENT_ID", ""), "OIDC client ID")
	fs.StringVar(&c.OIDCRedirectURL, "oidc-redirect-url", envOr(getenv, "TCDI_OIDC_REDIRECT_URL", ""), "OIDC redirect URL")
	fs.Var(&c.RequiredGroups, "required-groups",
		"login requires the ID-token groups claim to carry one of these groups (repeatable or CSV; env TCDI_REQUIRED_GROUPS; empty disables the gate)")
	fs.BoolVar(&c.DevInsecureDB, "dev-insecure-db", envOr(getenv, "TCDI_DEV_INSECURE_DB", "") == "true",
		"allow non-verifying PostgreSQL sslmode (disable/allow/prefer/require); local development only")
	fs.StringVar(&c.TenantNamespaces, "tenant-namespaces", envOr(getenv, "TCDI_TENANT_NAMESPACES", ""), "tenant=namespace pairs, comma-separated")
	fs.DurationVar(&c.SessionIdle, "session-idle", envDur(getenv, "TCDI_SESSION_IDLE", 30*time.Minute), "session idle timeout")
	fs.StringVar(&c.Kubeconfig, "kubeconfig", envOr(getenv, "KUBECONFIG", ""), "kubeconfig path (default: in-cluster)")
	fs.DurationVar(&c.ExpiryInterval, "expiry-interval", envDur(getenv, "TCDI_EXPIRY_INTERVAL", 30*time.Second),
		"expiry planner sweep interval (idle/disconnect/max-duration checks)")
	fs.DurationVar(&c.RetainedSyncInterval, "retained-sync-interval", envDur(getenv, "TCDI_RETAINED_SYNC_INTERVAL", 30*time.Second),
		"retained-inventory PVC->record sync interval; <=0 disables (env TCDI_RETAINED_SYNC_INTERVAL)")
	fs.DurationVar(&c.RecoveryInterval, "recovery-interval", envDur(getenv, "TCDI_RECOVERY_INTERVAL", 30*time.Second),
		"quota/intent recovery pass interval; <=0 runs a single startup pass (env TCDI_RECOVERY_INTERVAL)")
	fs.DurationVar(&c.ImageStaleAfter, "image-stale-after", envDur(getenv, "TCDI_IMAGE_STALE_AFTER", api.DefaultImageStaleAfter),
		"runtime image age reported as imageStale on template/workspace views; advisory only (env TCDI_IMAGE_STALE_AFTER)")

	// Session listener.
	fs.StringVar(&c.SessionListen, "session-listen", envOr(getenv, "TCDI_SESSION_LISTEN", ":8444"),
		"session listener address (empty disables it)")
	fs.StringVar(&c.SessionTLSCert, "session-tls-cert", envOr(getenv, "TCDI_SESSION_TLS_CERT", ""), "session listener TLS cert (PEM)")
	fs.StringVar(&c.SessionTLSKey, "session-tls-key", envOr(getenv, "TCDI_SESSION_TLS_KEY", ""), "session listener TLS key (PEM)")
	fs.StringVar(&c.SessionAllowedHosts, "session-allowed-hosts", envOr(getenv, "TCDI_SESSION_ALLOWED_HOSTS", ""), "comma-separated Host allowlist for the session listener")
	fs.StringVar(&c.SessionCookieMode, "session-cookie-mode", envOr(getenv, "TCDI_SESSION_COOKIE_MODE", "lax"),
		"session cookie mode: lax (same-site portal) or partitioned (CHIPS)")
	fs.StringVar(&c.UpstreamCA, "upstream-ca", envOr(getenv, "TCDI_UPSTREAM_CA", ""), "optional default PEM CA for runtime upstreams")
	fs.StringVar(&c.ControlTokenFile, "control-token-file", envOr(getenv, "TCDI_CONTROL_TOKEN_FILE", ""), "file containing the control-endpoint bearer token")
	fs.StringVar(&c.TenantAllowlist, "tenant-allowlist", envOr(getenv, "TCDI_TENANT_ALLOWLIST", ""), "bounded tenant label values, comma-separated")
	fs.DurationVar(&c.RenewInterval, "renew-interval", envDur(getenv, "TCDI_RENEW_INTERVAL", gateway.LeaseRenewInterval), "lease renew cadence")
	fs.DurationVar(&c.RevokeDeadline, "revoke-deadline", envDur(getenv, "TCDI_REVOKE_DEADLINE", gateway.RevokeDeadline), "fail-closed budget after last successful renew")

	// Internal mTLS listener.
	fs.StringVar(&c.InternalListen, "internal-listen", envOr(getenv, "TCDI_INTERNAL_LISTEN", ":9443"),
		"internal mTLS listener for gateways and the operator (empty disables it)")
	fs.StringVar(&c.InternalTLSCert, "internal-tls-cert", envOr(getenv, "TCDI_INTERNAL_TLS_CERT", ""), "internal listener TLS cert file")
	fs.StringVar(&c.InternalTLSKey, "internal-tls-key", envOr(getenv, "TCDI_INTERNAL_TLS_KEY", ""), "internal listener TLS key file")
	fs.StringVar(&c.InternalClientCA, "internal-client-ca", envOr(getenv, "TCDI_INTERNAL_CLIENT_CA", ""), "CA bundle verifying internal client certs")
	fs.StringVar(&c.OperatorCN, "operator-cn", envOr(getenv, "TCDI_OPERATOR_CN", httpapi.DefaultOperatorCN),
		"client-certificate CN of the operator identity on the internal workspace routes")

	// Metrics listener.
	fs.StringVar(&c.MetricsListen, "metrics-listen", envOr(getenv, "TCDI_METRICS_LISTEN", ""), "metrics listen address (empty disables)")

	// Shared.
	fs.StringVar(&c.SessionOrigin, "session-origin", envOr(getenv, "TCDI_SESSION_ORIGIN", ""),
		"public session origin used to build launch URLs (required, https://host[:port])")
	fs.Var(&c.PortalOrigins, "portal-origin",
		"portal Origin allowed to make cookie-authenticated state-changing requests (repeatable or CSV; env TCDI_PORTAL_ORIGINS; default: origin of -oidc-redirect-url)")
	fs.StringVar(&c.GatewayID, "gateway-id", envOr(getenv, "TCDI_GATEWAY_ID", ""),
		"session-gateway identity (merged mode default: backend; split mode default: client cert CN)")
	fs.StringVar(&c.GatewayAudience, "gateway-audience", envOr(getenv, "TCDI_GATEWAY_AUDIENCE", ""),
		"audience launch tickets bind to (default: session-origin host)")
	fs.Var(&c.LoginKeyFiles, "login-key-file",
		"OIDC login-state sealing key file (repeatable or CSV; env TCDI_LOGIN_KEY_FILE; first file seals, all open; required when the app listener is on)")

	// Split/test mode.
	fs.StringVar(&c.BrokerURL, "broker-url", envOr(getenv, "TCDI_BROKER_URL", ""), "remote broker base URL (https); split/test mode only — requires -listen= and -internal-listen=")
	fs.StringVar(&c.BrokerCA, "broker-ca", envOr(getenv, "TCDI_BROKER_CA", ""), "PEM CA bundle verifying the remote broker listener")
	fs.StringVar(&c.MTLSCert, "mtls-cert", envOr(getenv, "TCDI_MTLS_CERT", ""), "client certificate for the remote broker (PEM)")
	fs.StringVar(&c.MTLSKey, "mtls-key", envOr(getenv, "TCDI_MTLS_KEY", ""), "client key for the remote broker (PEM)")

	if err := fs.Parse(args); err != nil {
		return c, err
	}
	if fs.NArg() != 0 {
		return c, fmt.Errorf("unexpected positional args: %s", strings.Join(fs.Args(), " "))
	}

	// Env fallback for the list flags (only when no flag value was given).
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if !set["portal-origin"] {
		_ = c.PortalOrigins.Set(envOr(getenv, "TCDI_PORTAL_ORIGINS", ""))
	}
	if !set["required-groups"] {
		if v := envOr(getenv, "TCDI_REQUIRED_GROUPS", ""); v != "" {
			if err := c.RequiredGroups.Set(v); err != nil {
				return c, fmt.Errorf("TCDI_REQUIRED_GROUPS: %w", err)
			}
		}
	}
	if !set["login-key-file"] {
		// TCDI_LOGIN_KEY_FILES is the legacy plural the interim cmd/api used.
		if v := envOr(getenv, "TCDI_LOGIN_KEY_FILE", envOr(getenv, "TCDI_LOGIN_KEY_FILES", "")); v != "" {
			_ = c.LoginKeyFiles.Set(v)
		}
	}

	// Env-only secrets.
	if getenv != nil {
		c.OIDCClientSecret = getenv("TCDI_OIDC_CLIENT_SECRET")
		c.ControlToken = envOr(getenv, "TCDI_CONTROL_TOKEN", "")
		if c.ControlToken == "" {
			c.ControlToken = getenv("TCDI_GW_CONTROL_TOKEN") // legacy name
		}
	}

	return c, c.validate()
}

func (c *Config) validate() error {
	if c.Listen == "" && c.SessionListen == "" && c.InternalListen == "" && c.MetricsListen == "" {
		return errors.New("no listeners enabled: at least one of -listen, -session-listen, -internal-listen, -metrics-listen must be set")
	}
	if c.SessionCookieMode != "lax" && c.SessionCookieMode != "partitioned" {
		return fmt.Errorf("-session-cookie-mode must be lax or partitioned, got %q", c.SessionCookieMode)
	}
	if (c.TLSCert == "") != (c.TLSKey == "") {
		return errors.New("-tls-cert and -tls-key must be set together")
	}
	if (c.SessionTLSCert == "") != (c.SessionTLSKey == "") {
		return errors.New("-session-tls-cert and -session-tls-key must be set together")
	}

	// Split/test mode: remote broker, local listeners limited to session.
	if c.SplitMode() {
		if c.Listen != "" || c.InternalListen != "" {
			return errors.New("-broker-url selects split mode and requires -listen= and -internal-listen= to be empty")
		}
		missing := []string{}
		for name, v := range map[string]string{
			"broker-url": c.BrokerURL, "broker-ca": c.BrokerCA,
			"mtls-cert": c.MTLSCert, "mtls-key": c.MTLSKey,
		} {
			if v == "" {
				missing = append(missing, "-"+name)
			}
		}
		if len(missing) > 0 {
			return fmt.Errorf("split mode requires: %s", strings.Join(missing, ", "))
		}
	} else {
		// Merged mode: the in-process broker needs the database whenever any
		// listener that touches sessions or workspaces is on.
		if c.DatabaseURL == "" {
			return errors.New("required: -database-url")
		}
		// CHTR-8: refuse a non-verifying sslmode unless -dev-insecure-db.
		mode, err := checkDatabaseTLS(c.DatabaseURL, c.DevInsecureDB)
		if err != nil {
			return err
		}
		c.DBSSLMode = mode
	}

	if c.SessionListen != "" {
		missing := []string{}
		for name, v := range map[string]string{
			"session-tls-cert": c.SessionTLSCert, "session-tls-key": c.SessionTLSKey,
			"session-allowed-hosts": c.SessionAllowedHosts,
		} {
			if v == "" {
				missing = append(missing, "-"+name)
			}
		}
		if len(missing) > 0 {
			return fmt.Errorf("-session-listen requires: %s", strings.Join(missing, ", "))
		}
	}

	if c.Listen != "" {
		missing := []string{}
		for name, v := range map[string]string{
			"oidc-issuer": c.OIDCIssuer, "oidc-client-id": c.OIDCClientID,
			"oidc-redirect-url": c.OIDCRedirectURL,
		} {
			if v == "" {
				missing = append(missing, "-"+name)
			}
		}
		if len(c.LoginKeyFiles) == 0 {
			missing = append(missing, "-login-key-file")
		}
		if len(missing) > 0 {
			return fmt.Errorf("-listen requires: %s", strings.Join(missing, ", "))
		}
	}

	if c.InternalListen != "" {
		if c.InternalTLSCert == "" || c.InternalTLSKey == "" || c.InternalClientCA == "" {
			return errors.New("-internal-listen requires -internal-tls-cert, -internal-tls-key and -internal-client-ca")
		}
	}

	if c.SessionOrigin == "" {
		return errors.New("required: -session-origin")
	}
	// SEC-26: the session origin is the destination launch tickets are
	// POSTed to — it must be a bare https origin (no path/query/fragment/
	// userinfo).
	so, err := normalizeSessionOrigin(c.SessionOrigin)
	if err != nil {
		return err
	}
	c.SessionOrigin = so

	// Default the Origin allowlist to the redirect URL's origin: the chart
	// derives the callback from portalHost, so portal and API share it.
	if len(c.PortalOrigins) == 0 && c.OIDCRedirectURL != "" {
		if u, err := url.Parse(c.OIDCRedirectURL); err == nil && u.Scheme != "" && u.Host != "" {
			c.PortalOrigins = stringList{u.Scheme + "://" + u.Host}
		}
	}
	for _, o := range c.PortalOrigins {
		u, err := url.Parse(o)
		if err != nil || u.Scheme == "" || u.Host == "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
			return fmt.Errorf("bad portal-origin %q (want scheme://host[:port])", o)
		}
	}
	return nil
}

// normalizeSessionOrigin validates that raw is a bare https origin and
// returns its normalized form: scheme://lowercased-host[:non-default-port]
// — the same serialization a browser produces for URL.origin, so the
// SPA's launchUrl origin check byte-compares correctly.
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

// checkDatabaseTLS resolves the TLS configuration pgx will actually apply
// for dsn under the current PG* environment — libpq-style DSN keys override
// PGSSLMODE — and refuses every candidate connection that would not verify
// the server certificate (sslmode disable/allow/prefer/require, including
// the "prefer" default and the plaintext fallbacks prefer/allow keep).
// devInsecure bypasses the refusal for local development. The returned
// string is the effective mode label for startup logging.
//
// The DSN is never included in errors or logs: it carries credentials, and
// pgx's ParseConfigError redaction is only best-effort, so the parse error
// is deliberately not propagated.
func checkDatabaseTLS(dsn string, devInsecure bool) (string, error) {
	cfg, err := pgconn.ParseConfig(dsn)
	if err != nil {
		return "", errors.New("invalid database DSN")
	}
	mode := dbTLSModeName(cfg)
	verifies := tlsConfigVerifies(cfg.TLSConfig)
	for _, fb := range cfg.Fallbacks {
		verifies = verifies && tlsConfigVerifies(fb.TLSConfig)
	}
	if verifies || devInsecure {
		return mode, nil
	}
	return mode, fmt.Errorf("database sslmode=%s does not verify the PostgreSQL server certificate; set sslmode=verify-ca/verify-full or pass -dev-insecure-db for local development", mode)
}

// tlsConfigVerifies reports whether a resolved pgconn TLS config
// cryptographically verifies the PostgreSQL server: verify-ca installs a
// VerifyPeerCertificate chain check, verify-full uses the standard
// chain+hostname verification. Nil (plaintext) and InsecureSkipVerify
// configs verify nothing.
func tlsConfigVerifies(c *tls.Config) bool {
	switch {
	case c == nil:
		return false
	case c.VerifyPeerCertificate != nil:
		return true
	default:
		return !c.InsecureSkipVerify
	}
}

// dbTLSModeName derives a libpq-style sslmode label from the resolved pgconn
// configuration — pgx does not retain the mode string. A nil primary
// TLSConfig means disable (or a Unix-socket DSN, where TLS does not apply);
// nil primary with fallbacks is allow; InsecureSkipVerify with a plaintext
// fallback is prefer, otherwise require.
func dbTLSModeName(cfg *pgconn.Config) string {
	switch {
	case cfg.TLSConfig == nil && len(cfg.Fallbacks) == 0:
		return "disable"
	case cfg.TLSConfig == nil:
		return "allow"
	case cfg.TLSConfig.VerifyPeerCertificate != nil:
		return "verify-ca"
	case !cfg.TLSConfig.InsecureSkipVerify:
		return "verify-full"
	}
	for _, fb := range cfg.Fallbacks {
		if fb.TLSConfig == nil {
			return "prefer"
		}
	}
	return "require"
}

func parseTenants(s string) (provisioning.TenantNamespaces, error) {
	out := provisioning.TenantNamespaces{}
	if s == "" {
		return out, nil
	}
	for _, pair := range strings.Split(s, ",") {
		t, ns, ok := strings.Cut(strings.TrimSpace(pair), "=")
		if !ok || t == "" || ns == "" {
			return nil, fmt.Errorf("bad tenant mapping %q (want tenant=namespace)", pair)
		}
		out[t] = ns
	}
	return out, nil
}

// gatewayCN extracts the gateway identity from the client certificate's
// Subject CN when -gateway-id is not set in split mode. It reads the first
// CERTIFICATE PEM block only — the file needs no private key.
func gatewayCN(certFile string) (string, error) {
	raw, err := os.ReadFile(certFile)
	if err != nil {
		return "", err
	}
	for {
		var block *pem.Block
		block, raw = pem.Decode(raw)
		if block == nil {
			return "", fmt.Errorf("no CERTIFICATE PEM block in %s", certFile)
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		leaf, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return "", err
		}
		return leaf.Subject.CommonName, nil
	}
}
