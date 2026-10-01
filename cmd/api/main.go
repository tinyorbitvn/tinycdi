// Command api is the tinycdi public API server: OIDC login, session
// lifecycle, /v1/workspaces + /v1/templates handlers, and the outbox
// dispatcher that projects intents onto Workspace CRs.
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
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	crcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	workspacev1alpha1 "github.com/tinyorbitvn/tinycdi/api/v1alpha1"
	"github.com/tinyorbitvn/tinycdi/internal/api"
	"github.com/tinyorbitvn/tinycdi/internal/broker"
	"github.com/tinyorbitvn/tinycdi/internal/broker/httpapi"
	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
	"github.com/tinyorbitvn/tinycdi/internal/store"
)

// stringList collects repeated flags and comma-separated values into one
// list (used for -portal-origin / TCDI_PORTAL_ORIGINS).
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

type config struct {
	listen           string
	databaseURL      string
	oidcIssuer       string
	oidcClientID     string
	oidcClientSecret string // env only — never a flag value
	oidcRedirectURL  string
	requiredGroups   groupList // ID-token groups allowed to log in
	devInsecureDB    bool      // allow non-verifying DB sslmode (dev only)
	dbSSLMode        string    // resolved sslmode label, for logging
	tenantNamespaces string    // "tenant=ns,tenant2=ns2"
	sessionIdle      time.Duration
	kubeconfig       string
	portalOrigins    stringList // Origin allowlist for browser state changes

	sessionOrigin        string // public session origin (scheme://host[:port])
	gatewayAudience      string // audience tickets bind to; default = session origin host
	internalListen       string // mTLS broker listener for session gateways
	internalCert         string
	internalKey          string
	internalClientCA     string
	operatorCN           string // client-cert CN of the operator on internal routes
	expiryInterval       time.Duration
	retainedSyncInterval time.Duration
	recoveryInterval     time.Duration
	imageStaleAfter      time.Duration
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func parseConfig() (config, error) {
	var c config
	flag.StringVar(&c.listen, "listen", envOr("TCDI_LISTEN", ":8080"), "HTTP listen address")
	flag.StringVar(&c.databaseURL, "database-url", envOr("TCDI_DATABASE_URL", ""), "PostgreSQL DSN (env TCDI_DATABASE_URL)")
	flag.StringVar(&c.oidcIssuer, "oidc-issuer", envOr("TCDI_OIDC_ISSUER", ""), "OIDC issuer URL")
	flag.StringVar(&c.oidcClientID, "oidc-client-id", envOr("TCDI_OIDC_CLIENT_ID", ""), "OIDC client ID")
	flag.StringVar(&c.oidcRedirectURL, "oidc-redirect-url", envOr("TCDI_OIDC_REDIRECT_URL", ""), "OIDC redirect URL")
	flag.Var(&c.requiredGroups, "required-groups",
		"login requires the ID-token groups claim to carry one of these groups (repeatable or CSV; env TCDI_REQUIRED_GROUPS; empty disables the gate)")
	flag.BoolVar(&c.devInsecureDB, "dev-insecure-db", envOr("TCDI_DEV_INSECURE_DB", "") == "true",
		"allow non-verifying PostgreSQL sslmode (disable/allow/prefer/require); local development only")
	flag.StringVar(&c.tenantNamespaces, "tenant-namespaces", envOr("TCDI_TENANT_NAMESPACES", ""), "tenant=namespace pairs, comma-separated")
	flag.DurationVar(&c.sessionIdle, "session-idle", 30*time.Minute, "session idle timeout")
	flag.StringVar(&c.kubeconfig, "kubeconfig", envOr("KUBECONFIG", ""), "kubeconfig path (default: in-cluster)")
	flag.Var(&c.portalOrigins, "portal-origin",
		"portal Origin allowed to make cookie-authenticated state-changing requests (repeatable or CSV; env TCDI_PORTAL_ORIGINS; default: origin of -oidc-redirect-url)")
	flag.StringVar(&c.sessionOrigin, "session-origin", envOr("TCDI_SESSION_ORIGIN", "https://session.example.invalid"),
		"public session origin used to build launch URLs (a different registrable domain than the portal)")
	flag.StringVar(&c.gatewayAudience, "gateway-audience", envOr("TCDI_GATEWAY_AUDIENCE", ""),
		"audience launch tickets bind to (default: session-origin host)")
	flag.StringVar(&c.internalListen, "internal-listen", envOr("TCDI_INTERNAL_LISTEN", ""),
		"internal mTLS listener for session gateways (e.g. :9443); empty disables it")
	flag.StringVar(&c.internalCert, "internal-tls-cert", envOr("TCDI_INTERNAL_TLS_CERT", ""), "internal listener TLS cert file")
	flag.StringVar(&c.internalKey, "internal-tls-key", envOr("TCDI_INTERNAL_TLS_KEY", ""), "internal listener TLS key file")
	flag.StringVar(&c.internalClientCA, "internal-client-ca", envOr("TCDI_INTERNAL_CLIENT_CA", ""), "CA bundle verifying gateway client certs")
	flag.StringVar(&c.operatorCN, "operator-cn", envOr("TCDI_OPERATOR_CN", httpapi.DefaultOperatorCN),
		"client-certificate CN of the operator identity on the internal workspace routes")
	flag.DurationVar(&c.expiryInterval, "expiry-interval", 30*time.Second,
		"expiry planner sweep interval (idle/disconnect/max-duration checks)")
	retainedSyncDef := 30 * time.Second
	if v := os.Getenv("TCDI_RETAINED_SYNC_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			retainedSyncDef = d
		}
	}
	flag.DurationVar(&c.retainedSyncInterval, "retained-sync-interval", retainedSyncDef,
		"retained-inventory PVC->record sync interval; <=0 disables (env TCDI_RETAINED_SYNC_INTERVAL)")
	recoveryDef := 30 * time.Second
	if v := os.Getenv("TCDI_RECOVERY_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			recoveryDef = d
		}
	}
	flag.DurationVar(&c.recoveryInterval, "recovery-interval", recoveryDef,
		"quota/intent recovery pass interval; <=0 runs a single startup pass (env TCDI_RECOVERY_INTERVAL)")
	staleAfterDef := api.DefaultImageStaleAfter
	if v := os.Getenv("TCDI_IMAGE_STALE_AFTER"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			staleAfterDef = d
		}
	}
	flag.DurationVar(&c.imageStaleAfter, "image-stale-after", staleAfterDef,
		"runtime image age reported as imageStale on template/workspace views; advisory only (env TCDI_IMAGE_STALE_AFTER)")
	flag.Parse()
	if len(c.portalOrigins) == 0 {
		_ = c.portalOrigins.Set(envOr("TCDI_PORTAL_ORIGINS", ""))
	}
	if len(c.requiredGroups) == 0 {
		if v := os.Getenv("TCDI_REQUIRED_GROUPS"); v != "" {
			if err := c.requiredGroups.Set(v); err != nil {
				return c, fmt.Errorf("TCDI_REQUIRED_GROUPS: %w", err)
			}
		}
	}
	c.oidcClientSecret = os.Getenv("TCDI_OIDC_CLIENT_SECRET")
	if c.databaseURL == "" || c.oidcIssuer == "" || c.oidcClientID == "" || c.oidcRedirectURL == "" {
		return c, errors.New("required: database-url, oidc-issuer, oidc-client-id, oidc-redirect-url")
	}
	// CHTR-8: refuse to start when the effective sslmode does not verify
	// the PostgreSQL server certificate — whether it came from the DSN
	// (which wins) or PGSSLMODE. pgx defaults to "prefer", which silently
	// falls back to plaintext on TLS failure.
	mode, err := checkDatabaseTLS(c.databaseURL, c.devInsecureDB)
	if err != nil {
		return c, err
	}
	c.dbSSLMode = mode
	if c.internalListen != "" && (c.internalCert == "" || c.internalKey == "" || c.internalClientCA == "") {
		return c, errors.New("-internal-listen requires -internal-tls-cert, -internal-tls-key and -internal-client-ca")
	}
	// SEC-26: the session origin is the destination launch tickets are
	// POSTed to — it must be a bare https origin (no path/query/fragment/
	// userinfo). Anything else (http:, javascript:, or a path) could
	// redirect or confuse the launch flow.
	so, err := normalizeSessionOrigin(c.sessionOrigin)
	if err != nil {
		return c, err
	}
	c.sessionOrigin = so
	// Default the Origin allowlist to the redirect URL's origin: the chart
	// derives the callback from portalHost, so portal and API share it.
	if len(c.portalOrigins) == 0 {
		if u, err := url.Parse(c.oidcRedirectURL); err == nil {
			c.portalOrigins = stringList{u.Scheme + "://" + u.Host}
		}
	}
	for _, o := range c.portalOrigins {
		u, err := url.Parse(o)
		if err != nil || u.Scheme == "" || u.Host == "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
			return c, fmt.Errorf("bad portal-origin %q (want scheme://host[:port])", o)
		}
	}
	return c, nil
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

// sessionStoreAdapter converts between store.Session and api.Session so the
// Postgres store satisfies api.SessionStore without an import cycle.
type sessionStoreAdapter struct{ s *store.SessionStore }

func (a sessionStoreAdapter) Save(ctx context.Context, sess *api.Session) error {
	return a.s.Save(ctx, &store.Session{
		ID:         sess.ID,
		Issuer:     sess.Principal.Issuer,
		Subject:    sess.Principal.Subject,
		TenantID:   sess.Principal.TenantID,
		Groups:     sess.Principal.Groups,
		CSRFToken:  sess.CSRFToken,
		CreatedAt:  sess.CreatedAt,
		LastSeenAt: sess.LastSeenAt,
		ExpiresAt:  sess.ExpiresAt,
	})
}

func (a sessionStoreAdapter) Get(ctx context.Context, id string) (*api.Session, error) {
	rec, err := a.s.Get(ctx, id)
	if errors.Is(err, store.ErrSessionNotFound) {
		return nil, api.ErrSessionNotFound
	}
	if err != nil {
		return nil, err
	}
	return &api.Session{
		ID: rec.ID,
		Principal: api.Principal{
			Issuer: rec.Issuer, Subject: rec.Subject,
			TenantID: rec.TenantID, Groups: rec.Groups,
		},
		CSRFToken:  rec.CSRFToken,
		CreatedAt:  rec.CreatedAt,
		LastSeenAt: rec.LastSeenAt,
		ExpiresAt:  rec.ExpiresAt,
	}, nil
}

func (a sessionStoreAdapter) Delete(ctx context.Context, id string) error {
	return a.s.Delete(ctx, id)
}

// publicServer builds the public API listener (SEC-23): every request phase
// is bounded — header-only timeouts leave slow body/response/idle paths
// open. The API serves no streaming or WebSocket endpoints, so bounded
// Read/Write timeouts are safe.
func publicServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}
}

// internalServer builds the mTLS broker listener (SEC-23); same bounded
// budget as the public server.
func internalServer(addr string, h http.Handler, tlsCfg *tls.Config) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		TLSConfig:         tlsCfg,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}
}

func restConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	return rest.InClusterConfig()
}

func main() {
	cfg, err := parseConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		os.Exit(2)
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	db, err := store.Open(ctx, cfg.databaseURL)
	if err != nil {
		log.Error("connect postgres", "err", err)
		os.Exit(1)
	}
	defer db.Close()
	if err := db.Migrate(ctx); err != nil {
		log.Error("migrate", "err", err)
		os.Exit(1)
	}
	log.Info("database ready", "sslmode", cfg.dbSSLMode, "dev_insecure", cfg.devInsecureDB)
	tenants, err := parseTenants(cfg.tenantNamespaces)
	if err != nil {
		log.Error("tenant map", "err", err)
		os.Exit(2)
	}

	rcfg, err := restConfig(cfg.kubeconfig)
	if err != nil {
		log.Error("kubeconfig", "err", err)
		os.Exit(1)
	}
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		log.Error("scheme", "err", err)
		os.Exit(1)
	}
	if err := workspacev1alpha1.AddToScheme(scheme); err != nil {
		log.Error("scheme", "err", err)
		os.Exit(1)
	}
	kc, err := client.New(rcfg, client.Options{Scheme: scheme})
	if err != nil {
		log.Error("kube client", "err", err)
		os.Exit(1)
	}

	// Outbox dispatcher: delivers intents per workspace in revision order.
	svc := provisioning.NewService(db)
	outbox := provisioning.NewOutbox(db)
	retained := provisioning.NewRetainedStore(db)
	applier := provisioning.NewRetainedApplier(provisioning.NewK8sApplier(kc, tenants), kc, tenants, retained)
	disp := provisioning.NewDispatcher(outbox, applier,
		provisioning.WithPollInterval(250*time.Millisecond))
	go func() {
		if err := disp.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.Error("dispatcher exited", "err", err)
		}
	}()

	// Retained-data purge execution: deletes volumes whose records reached
	// Purging and completes the records (design §5 — the only platform
	// component allowed to delete persistent data).
	go func() {
		if err := provisioning.NewPurgeSweeper(retained, kc, log).Run(ctx); err != nil &&
			!errors.Is(err, context.Canceled) {
			log.Error("purge sweeper exited", "err", err)
		}
	}()

	// Retained-inventory sync: PVC metadata is the source of truth for
	// retained datasets — project retained-labelled volumes into
	// retained_data records so /v1/data reflects the cluster (idempotent,
	// deletes nothing).
	if cfg.retainedSyncInterval > 0 {
		syncer := provisioning.NewRetainedSync(kc, tenants, retained, nil, log)
		syncer.Poll = cfg.retainedSyncInterval
		go func() {
			if err := syncer.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
				log.Error("retained sync exited", "err", err)
			}
		}()
	}

	// Idempotency pruning (SEC-24): keys are single-scope and live 24 h per
	// the contract; without a sweep the table grows without bound.
	go func() {
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if n, err := svc.PruneIdempotency(ctx); err != nil {
					if !errors.Is(err, context.Canceled) {
						log.Error("idempotency prune", "err", err)
					}
				} else if n > 0 {
					log.Info("idempotency pruned", "rows", n)
				}
			}
		}
	}()

	// Recovery (design §8): level-based repair after restarts — re-drive
	// undispatched outbox intents and release held quota reservations, but
	// only on positive proof the runtime is gone (K8sRuntimeObserver reads
	// the Workspace CR and its labeled children; ambiguity never frees
	// quota). Runs once at startup, then every -recovery-interval.
	recovery := provisioning.NewRecovery(db, provisioning.NewK8sRuntimeObserver(kc, tenants))
	go func() {
		for {
			if pending, err := recovery.PendingRecovery(ctx); err != nil {
				log.Error("recovery pending scan", "err", err)
			} else if len(pending) > 0 {
				log.Info("recovery candidates", "pending", len(pending))
			}
			actions, err := recovery.Recover(ctx, applier)
			if err != nil && !errors.Is(err, context.Canceled) {
				log.Error("recovery pass", "err", err)
			} else if len(actions) > 0 {
				log.Info("recovery pass complete", "actions", len(actions))
			}
			if cfg.recoveryInterval <= 0 {
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(cfg.recoveryInterval):
			}
		}
	}()

	// Binding source: an informer cache over Workspace CRs, scoped to the
	// managed tenant namespaces. Its resync period doubles as the liveness
	// heartbeat — must stay well under broker.MaxBindingAge (15 s) so a
	// stalled watch stops issuance (fail closed).
	bindingResync := 5 * time.Second
	cacheOpts := crcache.Options{Scheme: scheme, SyncPeriod: &bindingResync}
	if len(tenants) > 0 {
		cacheOpts.DefaultNamespaces = map[string]crcache.Config{}
		for _, ns := range tenants {
			cacheOpts.DefaultNamespaces[ns] = crcache.Config{}
		}
	}
	kcache, err := crcache.New(rcfg, cacheOpts)
	if err != nil {
		log.Error("workspace cache", "err", err)
		os.Exit(1)
	}
	bindings, err := broker.NewK8sBindingSource(ctx, kcache)
	if err != nil {
		log.Error("binding source", "err", err)
		os.Exit(1)
	}
	// Workspace status view: shares the same informer cache so
	// /v1/workspaces projects observed CR phase + conditions (design §6 —
	// without it the API only ever reports the intent-side DB phase and the
	// portal can never offer Connect).
	statusView, err := api.NewK8sStatusView(ctx, kcache)
	if err != nil {
		log.Error("status view", "err", err)
		os.Exit(1)
	}
	go func() {
		if err := kcache.Start(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.Error("workspace cache exited", "err", err)
		}
	}()
	go func() {
		if kcache.WaitForCacheSync(ctx) {
			bindings.MarkSynced()
			statusView.MarkSynced()
		}
	}()

	audience := cfg.gatewayAudience
	if audience == "" {
		if u, err := url.Parse(cfg.sessionOrigin); err == nil && u.Hostname() != "" {
			audience = u.Hostname()
		}
	}
	if audience == "" {
		audience = broker.DefaultGatewayAudience
	}
	brk := broker.New(db, bindings,
		broker.WithGatewayAudience(audience),
		broker.WithCredentialSource(broker.NewK8sCredentialSource(kc, tenants)))

	// Expiry planner (design §8): periodically evaluate running workspaces
	// against recorded session activity and emit generation-fenced stop
	// intents through the outbox.
	expiry := broker.NewExpiryPlanner(brk)
	go expiry.Run(ctx, broker.NewK8sRunningSource(kcache), cfg.expiryInterval, log)

	authn, err := api.NewAuthenticator(ctx, api.AuthConfig{
		Issuer:         cfg.oidcIssuer,
		ClientID:       cfg.oidcClientID,
		ClientSecret:   cfg.oidcClientSecret,
		RedirectURL:    cfg.oidcRedirectURL,
		SessionOrigin:  cfg.sessionOrigin,
		RequiredGroups: cfg.requiredGroups,
	}, sessionStoreAdapter{s: store.NewSessionStore(db, cfg.sessionIdle)}, log)
	if err != nil {
		log.Error("oidc", "err", err)
		os.Exit(1)
	}

	wsHandler := api.NewWorkspaceHandler(svc, catalogAdapter{c: provisioning.NewK8sTemplateCatalog(kc, tenants)}, tenants).
		WithStatusView(statusView).
		WithImageStaleAfter(cfg.imageStaleAfter)
	tplHandler := api.NewTemplateHandler(catalogAdapter{c: provisioning.NewK8sTemplateCatalog(kc, tenants)}, tenants).
		WithImageStaleAfter(cfg.imageStaleAfter)
	connHandler := api.NewConnectionHandler(broker.PublicIssuer{B: brk}, tenants, cfg.sessionOrigin)
	dataHandler := api.NewDataHandler(retained, catalogAdapter{c: provisioning.NewK8sTemplateCatalog(kc, tenants)}, tenants)

	mux := http.NewServeMux()
	// Portal-facing auth paths (the SPA expects these).
	mux.Handle("GET /v1/login", http.HandlerFunc(authn.LoginHandler))
	mux.Handle("GET /v1/auth/callback", http.HandlerFunc(authn.CallbackHandler))
	mux.Handle("POST /v1/logout", authn.RequireAuth(authn.RequireCSRF(http.HandlerFunc(authn.LogoutHandler))))
	api.MountWorkspaceRoutes(mux, authn, wsHandler, tplHandler)
	api.MountConnectionRoutes(mux, authn, connHandler)
	api.MountDataRoutes(mux, authn, dataHandler)

	// Internal broker API on its own mTLS listener (ADR 0003): never shares
	// the public mux. The gateway authenticates with a client certificate;
	// the CN is the gateway identity, the audience is the session origin.
	if cfg.internalListen != "" {
		serverCert, err := tls.LoadX509KeyPair(cfg.internalCert, cfg.internalKey)
		if err != nil {
			log.Error("internal tls cert", "err", err)
			os.Exit(1)
		}
		caPEM, err := os.ReadFile(cfg.internalClientCA)
		if err != nil {
			log.Error("internal client ca", "err", err)
			os.Exit(1)
		}
		tlsCfg, err := httpapi.ServerTLSConfig(serverCert, caPEM)
		if err != nil {
			log.Error("internal tls config", "err", err)
			os.Exit(1)
		}
		isrv := internalServer(cfg.internalListen,
			httpapi.NewHandler(httpapi.Config{Broker: brk, Audience: audience, OperatorCN: cfg.operatorCN, Logger: log}),
			tlsCfg)
		go func() {
			<-ctx.Done()
			shCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = isrv.Shutdown(shCtx)
		}()
		go func() {
			log.Info("internal broker api listening", "addr", cfg.internalListen)
			if err := isrv.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Error("internal serve", "err", err)
				stop()
			}
		}()
	} else {
		log.Warn("internal broker api disabled (-internal-listen unset); gateways cannot redeem tickets")
	}

	srv := publicServer(cfg.listen, api.RequestID(api.Audit(log)(
		api.RequireTrustedOrigin(authn.SessionCookieName(), cfg.portalOrigins)(mux))))
	go func() {
		<-ctx.Done()
		shCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shCtx)
	}()
	log.Info("listening", "addr", cfg.listen)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("serve", "err", err)
		os.Exit(1)
	}
}

// catalogAdapter maps provisioning's K8s catalog onto api.TemplateCatalog.
type catalogAdapter struct {
	c *provisioning.K8sTemplateCatalog
}

func (a catalogAdapter) Resolve(ctx context.Context, tenantID, id string) (api.TemplateEntry, error) {
	e, err := a.c.Get(ctx, tenantID, id)
	if err != nil {
		return api.TemplateEntry{}, err
	}
	if e == nil {
		return api.TemplateEntry{}, api.ErrTemplateNotFound
	}
	return catalogEntry(*e), nil
}

func (a catalogAdapter) List(ctx context.Context, tenantID, runtimeFilter, _ string, _ int) ([]api.TemplateEntry, string, error) {
	list, err := a.c.List(ctx, tenantID, runtimeFilter)
	if err != nil {
		return nil, "", err
	}
	out := make([]api.TemplateEntry, 0, len(list))
	for _, e := range list {
		out = append(out, catalogEntry(e))
	}
	return out, "", nil
}

func catalogEntry(e provisioning.TemplateCatalogEntry) api.TemplateEntry {
	return api.TemplateEntry{
		ID:                     e.ID,
		Name:                   e.Name,
		Description:            e.Description,
		Revision:               e.Revision,
		Runtime:                e.Runtime,
		Experience:             e.Experience,
		CPUMillis:              e.CPUMillis,
		MemoryMiB:              e.MemoryBytes >> 20,
		StorageGiB:             e.DiskBytes >> 30,
		IdleTimeoutSeconds:     int64(e.IdleTimeout / time.Second),
		DisconnectGraceSeconds: int64(e.DisconnectGrace / time.Second),
		MaxRunningSeconds:      int64(e.MaxRunning / time.Second),
		DataPolicyDefault:      e.DataPolicyDefault,
		ClipboardPolicy:        e.ClipboardPolicy,
		PublishedAt:            e.PublishedAt,
		ImageBuiltAt:           e.ImageBuiltAt,
	}
}
