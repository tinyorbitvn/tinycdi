// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

// Package backend wires the merged tinycdi backend binary: the public API,
// the session gateway and the in-process broker on four listeners (app,
// session, internal mTLS, metrics) with independent TLS configuration.
//
// Flag surface follows decisions-2 item 1: app flags carry their plain
// names, the session flags take a "session-" prefix, the internal/mTLS
// listener keeps its names, and -broker-url/-broker-ca/-mtls-cert/-mtls-key
// select split/test mode (session listener only, remote broker, no DB/OIDC/
// Kubernetes). Every flag's environment variable is TCDI_<UPPER_SNAKE>;
// pinned exceptions: TCDI_PORTAL_ORIGINS (repeatable flag), KUBECONFIG,
// and the env-only secrets TCDI_OIDC_CLIENT_SECRET and TCDI_CONTROL_TOKEN.
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
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/tinyorbitvn/tinycdi/internal/api"
	"github.com/tinyorbitvn/tinycdi/internal/broker/httpapi"
	"github.com/tinyorbitvn/tinycdi/internal/gateway"
	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
	"github.com/tinyorbitvn/tinycdi/internal/ratelimit"
	"github.com/tinyorbitvn/tinycdi/internal/sessionhost"
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

// minControlTokenBytes is the shortest control bearer the backend accepts
// (R1-26). On the session listener's control hosts the bearer is the only
// gate — the host class is a Host-header routing check, not a network
// boundary — so it must resist guessing on its own.
const minControlTokenBytes = 32

// checkControlToken refuses a configured control token shorter than
// minControlTokenBytes. An empty token is allowed: the control surface is
// then closed (fail closed).
func checkControlToken(tok string) error {
	if tok != "" && len(tok) < minControlTokenBytes {
		return fmt.Errorf("control token is %d bytes; it must be at least %d (for example: openssl rand -hex 32)", len(tok), minControlTokenBytes)
	}
	return nil
}

// controlTokenUnsetWarning is logged at startup when no control token is
// configured (SEC-I2): the /v1/control surface fails CLOSED in that state —
// it is disabled, not unauthenticated.
const controlTokenUnsetWarning = "control token unset — /v1/control/* is disabled (fail closed); set -control-token-file or TCDI_CONTROL_TOKEN to enable"

// trustedProxiesUnsetWarning is logged at startup when a per-client rate
// limit is enabled while -trusted-proxies is empty: behind a shared edge
// every client then keys on the ingress's own address — one bucket for
// the whole organisation (advisor review, E7).
const trustedProxiesUnsetWarning = "rate limits enabled with -trusted-proxies empty — every client behind one ingress shares the ingress address's bucket; set -trusted-proxies to the edge CIDRs"

// rateLimitsUntrusted reports whether a per-client rate limiter is enabled
// on a listener this config runs while no trusted proxy CIDRs are set —
// the shared-bucket misconfiguration the startup warning names.
func rateLimitsUntrusted(c Config) bool {
	if p, err := ratelimit.ParseTrustedProxies(c.TrustedProxies); err != nil || len(p) > 0 {
		return false
	}
	return (c.Listen != "" && c.LoginRate > 0) ||
		(c.SessionListen != "" && (c.LaunchRate > 0 || c.SessionLookupRate > 0))
}

// Config is the parsed flag set for the merged backend.
type Config struct {
	// App listener (the public REST API surface).
	Listen            string // empty disables the app listener
	TLSCert           string // optional; empty serves plain HTTP
	TLSKey            string
	DatabaseURL       string
	OIDCIssuer        string
	OIDCClientID      string
	OIDCClientSecret  string // env only — never a flag value
	OIDCRedirectURL   string
	OIDCEndSession    bool   // RP-initiated logout via the discovered end_session_endpoint
	OIDCPostLogoutURL string // post_logout_redirect_uri; empty omits it
	RequiredGroups    groupList
	// TenantAdminGroups is the set of verified groups-claim values that
	// grant the tenant-admin role (default "tenant-admin"). It must name
	// at least one group — an empty list is a startup error, never a
	// silent "nobody is admin".
	TenantAdminGroups    groupList
	DevInsecureDB        bool
	DBSSLMode            string // resolved sslmode label, for logging
	TenantNamespaces     string
	TenantQuotas         string // JSON; see provisioning.ParseTenantQuotas
	TenantQuotaCeilings  string // JSON, same shape; bounds tenant-admin quota writes
	SessionIdle          time.Duration
	Kubeconfig           string
	ExpiryInterval       time.Duration
	RetainedSyncInterval time.Duration
	RecoveryInterval     time.Duration
	// TerminalRetentionDays is how long a connection_lease/launch_ticket
	// row is kept after reaching a terminal state; the leader's hourly
	// sweep deletes older rows. 0 disables the sweep — the tables then
	// grow without bound (SR-4-F5).
	TerminalRetentionDays int
	AuditRetentionDays    int // days an audit_event row is kept before the leader's daily prune (ADR 0010); 1..3650
	// FileTransferMaxBytes is the platform per-file bound for session
	// file transfer (ADR 0009 §5): every resolved policy is clipped to
	// it and a tenant ceiling's maxFileBytes may only lower it. 1..10 GiB.
	FileTransferMaxBytes int64
	ImageStaleAfter      time.Duration
	ImageBlockAfter      time.Duration // 0 disables the stale-image admission block (E3)
	LoginRate            int           // per-client requests/min on the login-family routes; 0 disables

	// Session listener (the former cmd/gateway surface).
	SessionListen       string // empty disables the session listener
	SessionTLSCert      string // required when the session listener is on
	SessionTLSKey       string
	SessionControlHosts string // comma-separated Host allowlist for /healthz and /v1/control/*
	SessionCookieMode   string // lax | partitioned
	UpstreamCA          string
	ControlTokenFile    string
	ControlToken        string // env/file only — never a flag value
	TenantAllowlist     string // bounds metrics label cardinality
	RenewInterval       time.Duration
	RevokeDeadline      time.Duration
	// DrainWindow is the duration of the pre-stop gateway drain: on
	// SIGTERM the pod keeps serving reads while streams migrate to
	// sibling replicas and only new launches/upgrades are refused —
	// listeners shut down when the window ends, not when the shed
	// finishes; 0 shuts down immediately.
	DrainWindow time.Duration
	// DrainPropagationDelay is the wait between the readiness drop and
	// the stream close at drain start: /readyz fails at SIGTERM so the
	// pod's endpoints go terminating at once, but kube-proxy / ingress
	// endpoint watches need ~0.5-2 s to stop routing new work here.
	// Streams keep flowing and new launches/upgrades are still served
	// during the wait — the graceful 1001 close lands only once
	// clients' retries can no longer reach this replica. 0 restores
	// the close-first order.
	DrainPropagationDelay time.Duration
	LaunchRate            int // per-client launches/min on /v1/launch; 0 disables
	// SessionLookupRate is the per-client bound on session-store lookups
	// for cookie values that do not resolve — on the app listener (only
	// misses spend) and for cookies the session listener has never seen.
	// LOCAL per replica (its purpose is keeping the cookie spray off the
	// store, so there is no shared window); 0 disables.
	SessionLookupRate int
	// SessionStreamCap is the per-workspace bound on live upstream
	// connections — websocket streams and plain proxied requests alike —
	// served by THIS replica (V11, the runtime-fd crash defense; N
	// replicas admit N x cap). >= 1 — there is no unlimited mode; a
	// runtime without the fd bound tolerates a large value.
	SessionStreamCap     int
	UnattachedSessionTTL time.Duration // ghost-session reap TTL; <=0 uses the gateway default

	// Internal mTLS listener (the broker/operator surface, ADR 0003).
	InternalListen   string // empty disables the internal listener
	InternalTLSCert  string
	InternalTLSKey   string
	InternalClientCA string
	OperatorCN       string
	// InternalRevokedList is the ADR 0008 per-certificate revocation
	// list for the internal mTLS listener; empty disables the feature —
	// the handshake path is then byte-identical to an unlisted build.
	InternalRevokedList string

	// Metrics listener (scrape-only, plain HTTP).
	MetricsListen string // empty disables

	// Shared.
	SessionDomain   string // session domain for per-workspace hosts (host[:port])
	PortalOrigins   stringList
	GatewayID       string
	GatewayAudience string
	LoginKeyFiles   stringList // first file seals; all open (rotation)
	TrustedProxies  string     // CSV CIDRs whose X-Forwarded-For claims are trusted (E7/TM-32)
	// RateLimitReplicas divides each rate budget into the degraded-mode
	// local floor (RL-1, ADR 0006 + RL-CEILING amendment): while the
	// Postgres window is reachable it enforces -login-rate/-launch-rate
	// as the exact aggregate bound and every pod's local ceiling is the
	// undivided budget (a store-protection prefilter); during a store
	// outage each pod falls back to its 1/N share (fail-open). >= 1; the
	// chart sets it to backend.replicas.
	RateLimitReplicas int

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

func envInt(getenv func(string) string, key string, def int) int {
	if getenv != nil {
		if v := getenv(key); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				return n
			}
		}
	}
	return def
}

func envInt64(getenv func(string) string, key string, def int64) int64 {
	if getenv != nil {
		if v := getenv(key); v != "" {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				return n
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
	fs.BoolVar(&c.OIDCEndSession, "oidc-end-session", envOr(getenv, "TCDI_OIDC_END_SESSION", "true") != "false",
		"sign-out also ends the identity provider session when its discovery document has end_session_endpoint (env TCDI_OIDC_END_SESSION; default true)")
	fs.StringVar(&c.OIDCPostLogoutURL, "oidc-post-logout-redirect", envOr(getenv, "TCDI_OIDC_POST_LOGOUT_REDIRECT", ""),
		"post_logout_redirect_uri sent to the identity provider at sign-out; must be registered there (empty omits it)")
	fs.Var(&c.RequiredGroups, "required-groups",
		"login requires the ID-token groups claim to carry one of these groups (repeatable or CSV; env TCDI_REQUIRED_GROUPS; empty disables the gate)")
	fs.Var(&c.TenantAdminGroups, "tenant-admin-groups",
		"ID-token groups claim values that grant the tenant-admin role (repeatable or CSV; env TCDI_TENANT_ADMIN_GROUPS; default \""+api.TenantAdminGroup+"\"; exact match — a Keycloak full path \"/x\" does not match \"x\"; must name at least one group)")
	fs.BoolVar(&c.DevInsecureDB, "dev-insecure-db", envOr(getenv, "TCDI_DEV_INSECURE_DB", "") == "true",
		"allow non-verifying PostgreSQL sslmode (disable/allow/prefer/require); local development only")
	fs.StringVar(&c.TenantNamespaces, "tenant-namespaces", envOr(getenv, "TCDI_TENANT_NAMESPACES", ""), "tenant=namespace pairs, comma-separated")
	fs.StringVar(&c.TenantQuotas, "tenant-quotas", envOr(getenv, "TCDI_TENANT_QUOTAS", ""),
		"declared tenant quotas as JSON [{tenant,runningWorkspaces,cpu,memory,storage}]; the singleton leader upserts exactly the listed tenants at startup, unlisted tenants are untouched (env TCDI_TENANT_QUOTAS)")
	fs.StringVar(&c.TenantQuotaCeilings, "tenant-quota-ceilings", envOr(getenv, "TCDI_TENANT_QUOTA_CEILINGS", ""),
		"operator ceilings on tenant-admin quota writes as JSON [{tenant,runningWorkspaces,cpu,memory,storage}]; a tenant-admin quota write above its tenant's ceiling, or for a tenant without one, is refused (env TCDI_TENANT_QUOTA_CEILINGS)")
	fs.DurationVar(&c.SessionIdle, "session-idle", envDur(getenv, "TCDI_SESSION_IDLE", 30*time.Minute), "session idle timeout")
	fs.StringVar(&c.Kubeconfig, "kubeconfig", envOr(getenv, "KUBECONFIG", ""), "kubeconfig path (default: in-cluster)")
	fs.DurationVar(&c.ExpiryInterval, "expiry-interval", envDur(getenv, "TCDI_EXPIRY_INTERVAL", 30*time.Second),
		"expiry planner sweep interval (idle/disconnect/max-duration checks)")
	fs.DurationVar(&c.RetainedSyncInterval, "retained-sync-interval", envDur(getenv, "TCDI_RETAINED_SYNC_INTERVAL", 30*time.Second),
		"retained-inventory PVC->record sync interval; <=0 disables (env TCDI_RETAINED_SYNC_INTERVAL)")
	fs.IntVar(&c.TerminalRetentionDays, "terminal-retention-days", envInt(getenv, "TCDI_TERMINAL_RETENTION_DAYS", 30),
		"days a terminal connection_lease/launch_ticket row is kept before the leader's hourly sweep deletes it; 0 disables (env TCDI_TERMINAL_RETENTION_DAYS)")
	fs.IntVar(&c.AuditRetentionDays, "audit-retention-days", envInt(getenv, "TCDI_AUDIT_RETENTION_DAYS", 90),
		"days audit_event rows are kept; a daily leader task prunes older rows (env TCDI_AUDIT_RETENTION_DAYS)")
	fs.Int64Var(&c.FileTransferMaxBytes, "file-transfer-max-bytes", envInt64(getenv, "TCDI_FILE_TRANSFER_MAX_BYTES", 104857600),
		"platform per-file bound for session file transfer; a tenant maxFileBytes may only lower it (env TCDI_FILE_TRANSFER_MAX_BYTES)")
	fs.DurationVar(&c.RecoveryInterval, "recovery-interval", envDur(getenv, "TCDI_RECOVERY_INTERVAL", 30*time.Second),
		"quota/intent recovery pass interval; one pass runs at every leader acquisition, then every interval; "+
			"<=0 runs the single startup pass only. Quota settlement is event-driven (Workspace informer) with "+
			"this tick as the fallback — a reservation whose runtime absence is never proven waits indefinitely "+
			"by design (env TCDI_RECOVERY_INTERVAL)")
	fs.DurationVar(&c.ImageStaleAfter, "image-stale-after", envDur(getenv, "TCDI_IMAGE_STALE_AFTER", api.DefaultImageStaleAfter),
		"runtime image age reported as imageStale on template/workspace views; advisory only (env TCDI_IMAGE_STALE_AFTER)")
	fs.DurationVar(&c.ImageBlockAfter, "image-block-after", envDur(getenv, "TCDI_IMAGE_BLOCK_AFTER", api.DefaultImageBlockAfter),
		"runtime image age that blocks create/start with 409 IMAGE_STALE (E3); a missing imageBuiltAt never blocks; 0 disables (env TCDI_IMAGE_BLOCK_AFTER)")
	fs.IntVar(&c.LoginRate, "login-rate", envInt(getenv, "TCDI_LOGIN_RATE", 60),
		"per-client requests/minute on /v1/login, /v1/auth/callback and GET /v1/session (burst 20), aggregate across replicas via the shared Postgres window; over the limit answers 429 RATE_LIMITED with Retry-After — 0 disables (env TCDI_LOGIN_RATE)")

	// Session listener.
	fs.StringVar(&c.SessionListen, "session-listen", envOr(getenv, "TCDI_SESSION_LISTEN", ":8444"),
		"session listener address (empty disables it)")
	fs.StringVar(&c.SessionTLSCert, "session-tls-cert", envOr(getenv, "TCDI_SESSION_TLS_CERT", ""), "session listener TLS cert (PEM)")
	fs.StringVar(&c.SessionTLSKey, "session-tls-key", envOr(getenv, "TCDI_SESSION_TLS_KEY", ""), "session listener TLS key (PEM)")
	fs.StringVar(&c.SessionControlHosts, "session-control-hosts", envOr(getenv, "TCDI_SESSION_CONTROL_HOSTS", ""), "comma-separated Host allowlist for /healthz and /v1/control/* on the session listener (in-cluster Service names)")
	fs.StringVar(&c.SessionCookieMode, "session-cookie-mode", envOr(getenv, "TCDI_SESSION_COOKIE_MODE", "lax"),
		"session cookie mode: lax (same-site portal) or partitioned (CHIPS)")
	fs.StringVar(&c.UpstreamCA, "upstream-ca", envOr(getenv, "TCDI_UPSTREAM_CA", ""), "optional default PEM CA for runtime upstreams")
	fs.StringVar(&c.ControlTokenFile, "control-token-file", envOr(getenv, "TCDI_CONTROL_TOKEN_FILE", ""), "file containing the control-endpoint bearer token")
	fs.StringVar(&c.TenantAllowlist, "tenant-allowlist", envOr(getenv, "TCDI_TENANT_ALLOWLIST", ""), "bounded tenant label values, comma-separated")
	fs.DurationVar(&c.RenewInterval, "renew-interval", envDur(getenv, "TCDI_RENEW_INTERVAL", gateway.LeaseRenewInterval), "lease renew cadence")
	fs.DurationVar(&c.RevokeDeadline, "revoke-deadline", envDur(getenv, "TCDI_REVOKE_DEADLINE", gateway.RevokeDeadline), "fail-closed budget after last successful renew")
	fs.DurationVar(&c.DrainWindow, "drain-window", envDur(getenv, "TCDI_DRAIN_WINDOW", 8*time.Second), "pre-stop drain window: listeners keep serving reads and refuse new launches/upgrades until it ends (0 shuts down immediately)")
	fs.DurationVar(&c.DrainPropagationDelay, "drain-propagation-delay", envDur(getenv, "TCDI_DRAIN_PROPAGATION_DELAY", 5*time.Second), "wait between the readiness drop and the stream close at drain start, so endpoint removal propagates before clients are told to reconnect (0 closes streams at once)")
	fs.IntVar(&c.LaunchRate, "launch-rate", envInt(getenv, "TCDI_LAUNCH_RATE", 120),
		"per-client launches/minute on /v1/launch (burst 40), aggregate across replicas via the shared Postgres window (per-replica local limiter in split mode); over the limit answers 429 with Retry-After — 0 disables (env TCDI_LAUNCH_RATE)")
	fs.IntVar(&c.SessionLookupRate, "session-lookup-rate", envInt(getenv, "TCDI_SESSION_LOOKUP_RATE", gateway.SessionLookupRate),
		"per-client requests/minute carrying a session cookie that does not resolve to a live session — on the app listener and for cookies this replica has never seen on the session listener (burst "+strconv.Itoa(gateway.SessionLookupBurst)+"), enforced by a LOCAL per-replica bucket so a random-cookie spray cannot multiply Postgres reads; over the limit answers 429 with Retry-After — 0 disables (env TCDI_SESSION_LOOKUP_RATE)")
	fs.IntVar(&c.SessionStreamCap, "session-stream-cap", envInt(getenv, "TCDI_SESSION_STREAM_CAP", gateway.DefaultUpstreamStreamCap),
		"per-workspace bound on live upstream connections through THIS replica — websocket streams and plain proxied requests alike; over the cap the session origin answers 429 too_many_streams + Retry-After: 1. >= 1 — no unlimited mode; pick a large value when the runtime carries no fd bound (env TCDI_SESSION_STREAM_CAP)")
	fs.DurationVar(&c.UnattachedSessionTTL, "unattached-session-ttl", envDur(getenv, "TCDI_UNATTACHED_SESSION_TTL", gateway.UnattachedSessionTTL),
		"how long a redeemed session that never served a request may hold its lease before the gateway revokes it — bounds the workspace pin a launch abandoned between redeem and the 303 leaves behind (env TCDI_UNATTACHED_SESSION_TTL)")

	// Internal mTLS listener.
	fs.StringVar(&c.InternalListen, "internal-listen", envOr(getenv, "TCDI_INTERNAL_LISTEN", ":9443"),
		"internal mTLS listener for gateways and the operator (empty disables it)")
	fs.StringVar(&c.InternalTLSCert, "internal-tls-cert", envOr(getenv, "TCDI_INTERNAL_TLS_CERT", ""), "internal listener TLS cert file")
	fs.StringVar(&c.InternalTLSKey, "internal-tls-key", envOr(getenv, "TCDI_INTERNAL_TLS_KEY", ""), "internal listener TLS key file")
	fs.StringVar(&c.InternalClientCA, "internal-client-ca", envOr(getenv, "TCDI_INTERNAL_CLIENT_CA", ""), "CA bundle verifying internal client certs")
	fs.StringVar(&c.OperatorCN, "operator-cn", envOr(getenv, "TCDI_OPERATOR_CN", httpapi.DefaultOperatorCN),
		"client-certificate CN of the operator identity on the internal workspace routes")
	fs.StringVar(&c.InternalRevokedList, "internal-revoked-list", envOr(getenv, "TCDI_INTERNAL_REVOKED_LIST", ""),
		"per-certificate revocation list for the internal mTLS listener (ADR 0008: \"<serial-hex> <issuer-dn>\" per line; empty disables)")

	// Metrics listener.
	fs.StringVar(&c.MetricsListen, "metrics-listen", envOr(getenv, "TCDI_METRICS_LISTEN", ""), "metrics listen address (empty disables)")

	// Shared.
	fs.StringVar(&c.SessionDomain, "session-domain", envOr(getenv, "TCDI_SESSION_DOMAIN", ""),
		"session domain: workspaces are served on ws-<id>.<domain> (host[:port]; required when the session listener is on)")
	fs.Var(&c.PortalOrigins, "portal-origin",
		"portal Origin allowed to make cookie-authenticated state-changing requests (repeatable or CSV; env TCDI_PORTAL_ORIGINS; default: origin of -oidc-redirect-url)")
	fs.StringVar(&c.GatewayID, "gateway-id", envOr(getenv, "TCDI_GATEWAY_ID", ""),
		"session-gateway identity (merged mode default: backend; split mode default: client cert CN)")
	fs.StringVar(&c.GatewayAudience, "gateway-audience", envOr(getenv, "TCDI_GATEWAY_AUDIENCE", ""),
		"audience launch tickets bind to (default: session domain)")
	fs.Var(&c.LoginKeyFiles, "login-key-file",
		"OIDC login-state sealing key file (repeatable or CSV; env TCDI_LOGIN_KEY_FILE; first file seals, all open; required when the app listener is on)")
	fs.StringVar(&c.TrustedProxies, "trusted-proxies", envOr(getenv, "TCDI_TRUSTED_PROXIES", ""),
		"comma-separated CIDRs of reverse proxies whose X-Forwarded-For claims are trusted; empty trusts only the socket peer (env TCDI_TRUSTED_PROXIES)")
	fs.IntVar(&c.RateLimitReplicas, "rate-limit-replicas", envInt(getenv, "TCDI_RATE_LIMIT_REPLICAS", 1),
		"divisor for the degraded-mode rate-limit floor: the Postgres window enforces -login-rate/-launch-rate as exact aggregate bounds and while it is reachable each pod's local ceiling is the undivided budget (a store-protection prefilter); during a store outage each pod falls back to its 1/N share (fail-open floor), and split-mode launch is that share outright; set to the deployment replica count (env TCDI_RATE_LIMIT_REPLICAS)")

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
	if !set["tenant-admin-groups"] {
		// envOr's empty-string "unset" resolves to the
		// "tenant-admin" default; a set-but-empty env is indistinguishable
		// from unset through the getenv seam, exactly like required-groups.
		if err := c.TenantAdminGroups.Set(envOr(getenv, "TCDI_TENANT_ADMIN_GROUPS", api.TenantAdminGroup)); err != nil {
			return c, fmt.Errorf("TCDI_TENANT_ADMIN_GROUPS: %w", err)
		}
	}
	if !set["login-key-file"] {
		if v := envOr(getenv, "TCDI_LOGIN_KEY_FILE", ""); v != "" {
			_ = c.LoginKeyFiles.Set(v)
		}
	}

	// Env-only secrets.
	if getenv != nil {
		c.OIDCClientSecret = getenv("TCDI_OIDC_CLIENT_SECRET")
		c.ControlToken = envOr(getenv, "TCDI_CONTROL_TOKEN", "")
	}

	return c, c.validate()
}

func (c *Config) validate() error {
	if c.Listen == "" && c.SessionListen == "" && c.InternalListen == "" && c.MetricsListen == "" {
		return errors.New("no listeners enabled: at least one of -listen, -session-listen, -internal-listen, -metrics-listen must be set")
	}
	if err := validateTenantQuotaFlags(c.TenantQuotas, c.TenantQuotaCeilings); err != nil {
		return err
	}
	if len(c.TenantAdminGroups) == 0 {
		// Unreachable through ParseFlags (the default lands first and
		// groupList rejects empty entries) — kept so a future path can
		// never silence every admin gate at once.
		return errors.New("-tenant-admin-groups must name at least one group")
	}
	if c.SessionCookieMode != "lax" && c.SessionCookieMode != "partitioned" {
		return fmt.Errorf("-session-cookie-mode must be lax or partitioned, got %q", c.SessionCookieMode)
	}
	if c.LoginRate < 0 || c.LaunchRate < 0 || c.SessionLookupRate < 0 {
		return errors.New("-login-rate, -launch-rate and -session-lookup-rate must be >= 0 (0 disables the limit)")
	}
	if c.SessionStreamCap < 1 {
		return errors.New("-session-stream-cap must be >= 1 — the per-workspace upstream bound cannot be disabled (set a large value instead)")
	}
	if c.TerminalRetentionDays < 0 {
		return errors.New("-terminal-retention-days must be >= 0 (0 disables the retention sweep)")
	}
	if c.AuditRetentionDays < 1 || c.AuditRetentionDays > 3650 {
		return errors.New("-audit-retention-days must be 1..3650")
	}
	if c.FileTransferMaxBytes < 1 || c.FileTransferMaxBytes > 10<<30 {
		return errors.New("-file-transfer-max-bytes must be 1..10737418240")
	}
	if c.RateLimitReplicas < 1 {
		return errors.New("-rate-limit-replicas must be >= 1")
	}
	if c.DrainWindow < 0 {
		return errors.New("-drain-window must be >= 0 (0 shuts down immediately, without a drain hold)")
	}
	if c.DrainPropagationDelay < 0 {
		return errors.New("-drain-propagation-delay must be >= 0 (0 closes streams as soon as readiness drops)")
	}
	// Shutdown budget: the propagation wait and the drain window must
	// leave room for the parallel listener Shutdown and the background
	// stop inside the shared shutdownDeadline (24 s), which itself sits
	// under the pod's 30 s terminationGracePeriodSeconds.
	if c.DrainPropagationDelay+c.DrainWindow > shutdownDeadline-listenerShutdownReserve {
		return fmt.Errorf("-drain-propagation-delay + -drain-window = %s, must be <= %s so listener shutdown fits inside the %s shutdown deadline",
			c.DrainPropagationDelay+c.DrainWindow, shutdownDeadline-listenerShutdownReserve, shutdownDeadline)
	}
	if _, err := ratelimit.ParseTrustedProxies(c.TrustedProxies); err != nil {
		return fmt.Errorf("-trusted-proxies: %w", err)
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
			"session-domain": c.SessionDomain,
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
	if c.InternalRevokedList != "" && c.InternalListen == "" {
		// A configured revocation control that silently does nothing is a
		// fail-open misconfiguration — refuse it.
		return errors.New("-internal-revoked-list requires -internal-listen")
	}

	// The session domain is required by the session-listener check above;
	// when set without it the value is still validated so a typo fails fast.
	if c.SessionDomain != "" {
		if _, err := sessionhost.ParseDomain(c.SessionDomain); err != nil {
			return err
		}
	}

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

// validateTenantQuotaFlags parses -tenant-quotas and -tenant-quota-ceilings.
// A tenant may appear in only one: a declared quota is read-only, so a
// ceiling on it would never apply.
func validateTenantQuotaFlags(quotas, ceilings string) error {
	q, err := provisioning.ParseTenantQuotas(quotas)
	if err != nil {
		return fmt.Errorf("-tenant-quotas: %w", err)
	}
	c, err := provisioning.ParseTenantQuotas(ceilings)
	if err != nil {
		return fmt.Errorf("-tenant-quota-ceilings: %w", err)
	}
	declared := make(map[string]bool, len(q))
	for _, e := range q {
		declared[e.TenantID] = true
	}
	for _, e := range c {
		if declared[e.TenantID] {
			return fmt.Errorf("-tenant-quota-ceilings: tenant %q also has a quota in -tenant-quotas, which is read-only; declare one or the other", e.TenantID)
		}
	}
	return nil
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
