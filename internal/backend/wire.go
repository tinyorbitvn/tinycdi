// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package backend

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	crcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	workspacev1alpha1 "github.com/tinyorbitvn/tinycdi/api/v1alpha1"
	"github.com/tinyorbitvn/tinycdi/internal/api"
	"github.com/tinyorbitvn/tinycdi/internal/api/loginstate"
	"github.com/tinyorbitvn/tinycdi/internal/broker"
	"github.com/tinyorbitvn/tinycdi/internal/broker/httpapi"
	"github.com/tinyorbitvn/tinycdi/internal/gateway"
	"github.com/tinyorbitvn/tinycdi/internal/gateway/brokerclient"
	"github.com/tinyorbitvn/tinycdi/internal/observability"
	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
	"github.com/tinyorbitvn/tinycdi/internal/store"
	"github.com/tinyorbitvn/tinycdi/internal/tlsreload"
)

// defaultMergedGatewayID is the session-gateway identity when -gateway-id
// is unset in merged mode. All replicas must share one identity so a lease
// redeemed on one can be renewed on another (restart-safe sessions); the
// per-replica cert-CN derivation only exists in split mode.
const defaultMergedGatewayID = "backend"

// wire resolves secrets, builds every handler, binds every enabled listener
// and records the background loops and closers on b. On error the caller
// runs closeAll.
func (b *Backend) wire(ctx context.Context) error {
	cfg := b.cfg

	if cfg.ControlTokenFile != "" {
		raw, err := os.ReadFile(cfg.ControlTokenFile)
		if err != nil {
			return fmt.Errorf("control token file: %w", err)
		}
		cfg.ControlToken = strings.TrimSpace(string(raw))
	}
	if cfg.ControlToken == "" {
		b.log.Warn(controlTokenUnsetWarning)
	}
	b.cfg = cfg

	var metrics *observability.Metrics
	if cfg.MetricsListen != "" {
		metrics = observability.NewMetrics(prometheus.DefaultRegisterer,
			strings.FieldsFunc(cfg.TenantAllowlist, func(r rune) bool { return r == ',' }))
	}

	// The gateway identity and ticket audience are shared by the session
	// listener and the internal broker API.
	id, err := resolveGatewayIdentity(cfg)
	if err != nil {
		return err
	}
	if cfg.SplitMode() {
		if err := b.wireSplit(cfg, metrics, id); err != nil {
			return err
		}
	} else {
		if err := b.wireMerged(ctx, cfg, id, metrics); err != nil {
			return err
		}
	}
	return b.bind(cfg)
}

// resolveGatewayIdentity derives the session-gateway identity: -gateway-id
// wins; merged mode defaults to the shared "backend" identity so every
// replica owns the same leases; split mode falls back to the client
// certificate's Subject CN (the v0.1 behaviour).
func resolveGatewayIdentity(cfg Config) (broker.GatewayIdentity, error) {
	audience := cfg.GatewayAudience
	if audience == "" {
		if u, err := url.Parse(cfg.SessionOrigin); err == nil && u.Hostname() != "" {
			audience = u.Hostname()
		}
	}
	if audience == "" {
		audience = broker.DefaultGatewayAudience
	}
	gwID := cfg.GatewayID
	if gwID == "" {
		if cfg.SplitMode() {
			id, err := gatewayCN(cfg.MTLSCert)
			if err != nil {
				return broker.GatewayIdentity{}, fmt.Errorf("gateway id from client cert: %w", err)
			}
			if id == "" {
				return broker.GatewayIdentity{}, errors.New("client cert has no Subject CN; pass -gateway-id")
			}
			gwID = id
		} else {
			gwID = defaultMergedGatewayID
		}
	}
	return broker.GatewayIdentity{ID: gwID, Audience: audience}, nil
}

// localGateway builds the pinned-identity in-process broker client the
// merged session gateway drives — as BrokerClient and as the session
// directory (P3). The operator CN is reserved so it can never act as a
// gateway.
func (b *Backend) localGateway(brk *broker.Broker, id broker.GatewayIdentity) (*broker.LocalGateway, error) {
	return broker.NewLocalGateway(brk, id, b.cfg.OperatorCN)
}

// wireMerged builds the full control-plane stack: Postgres, Kubernetes
// client and informer cache, provisioning services, broker, OIDC
// authenticator and the in-process session gateway.
func (b *Backend) wireMerged(ctx context.Context, cfg Config, id broker.GatewayIdentity, metrics *observability.Metrics) error {
	log := b.log

	db, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("connect postgres: %w", err)
	}
	b.closers = append(b.closers, func() { db.Close() })
	if err := db.Migrate(ctx); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	log.Info("database ready", "sslmode", cfg.DBSSLMode, "dev_insecure", cfg.DevInsecureDB)
	tenants, err := parseTenants(cfg.TenantNamespaces)
	if err != nil {
		return fmt.Errorf("tenant map: %w", err)
	}

	rcfg, err := restConfig(cfg.Kubeconfig)
	if err != nil {
		return fmt.Errorf("kubeconfig: %w", err)
	}
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return fmt.Errorf("scheme: %w", err)
	}
	if err := workspacev1alpha1.AddToScheme(scheme); err != nil {
		return fmt.Errorf("scheme: %w", err)
	}
	kc, err := client.New(rcfg, client.Options{Scheme: scheme})
	if err != nil {
		return fmt.Errorf("kube client: %w", err)
	}

	// Outbox dispatcher: delivers intents per workspace in revision order.
	svc := provisioning.NewService(db)
	outbox := provisioning.NewOutbox(db)
	retained := provisioning.NewRetainedStore(db)
	applier := provisioning.NewRetainedApplier(provisioning.NewK8sApplier(kc, tenants), kc, tenants, retained)
	disp := provisioning.NewDispatcher(outbox, applier,
		provisioning.WithPollInterval(250*time.Millisecond))
	b.bg = append(b.bg, func(ctx context.Context) {
		if err := disp.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.Error("dispatcher exited", "err", err)
		}
	})

	// Retained-data purge execution: deletes volumes whose records reached
	// Purging and completes the records (design §5 — the only platform
	// component allowed to delete persistent data).
	b.bg = append(b.bg, func(ctx context.Context) {
		if err := provisioning.NewPurgeSweeper(retained, kc, log).Run(ctx); err != nil &&
			!errors.Is(err, context.Canceled) {
			log.Error("purge sweeper exited", "err", err)
		}
	})

	// Retained-inventory sync: PVC metadata is the source of truth for
	// retained datasets — project retained-labelled volumes into
	// retained_data records so /v1/data reflects the cluster (idempotent,
	// deletes nothing).
	if cfg.RetainedSyncInterval > 0 {
		syncer := provisioning.NewRetainedSync(kc, tenants, retained, nil, log)
		syncer.Poll = cfg.RetainedSyncInterval
		b.bg = append(b.bg, func(ctx context.Context) {
			if err := syncer.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
				log.Error("retained sync exited", "err", err)
			}
		})
	}

	// Idempotency pruning (SEC-24): keys are single-scope and live 24 h per
	// the contract; without a sweep the table grows without bound.
	b.bg = append(b.bg, func(ctx context.Context) {
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
	})

	// Recovery (design §8): level-based repair after restarts — re-drive
	// undispatched outbox intents and release held quota reservations, but
	// only on positive proof the runtime is gone (K8sRuntimeObserver reads
	// the Workspace CR and its labeled children; ambiguity never frees
	// quota). Runs once at startup, then every -recovery-interval.
	recovery := provisioning.NewRecovery(db, provisioning.NewK8sRuntimeObserver(kc, tenants))
	b.bg = append(b.bg, func(ctx context.Context) {
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
			if cfg.RecoveryInterval <= 0 {
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(cfg.RecoveryInterval):
			}
		}
	})

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
		return fmt.Errorf("workspace cache: %w", err)
	}
	bindings, err := broker.NewK8sBindingSource(ctx, kcache)
	if err != nil {
		return fmt.Errorf("binding source: %w", err)
	}
	// Workspace status view: shares the same informer cache so
	// /v1/workspaces projects observed CR phase + conditions (design §6 —
	// without it the API only ever reports the intent-side DB phase and the
	// portal can never offer Connect).
	statusView, err := api.NewK8sStatusView(ctx, kcache)
	if err != nil {
		return fmt.Errorf("status view: %w", err)
	}
	b.bg = append(b.bg, func(ctx context.Context) {
		if err := kcache.Start(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.Error("workspace cache exited", "err", err)
		}
	})
	b.bg = append(b.bg, func(ctx context.Context) {
		if kcache.WaitForCacheSync(ctx) {
			bindings.MarkSynced()
			statusView.MarkSynced()
		}
	})

	brk := broker.New(db, bindings,
		broker.WithGatewayAudience(id.Audience),
		broker.WithCredentialSource(broker.NewK8sCredentialSource(kc, tenants)))

	// Expiry planner (design §8): periodically evaluate running workspaces
	// against recorded session activity and emit generation-fenced stop
	// intents through the outbox.
	expiry := broker.NewExpiryPlanner(brk)
	b.bg = append(b.bg, func(ctx context.Context) {
		expiry.Run(ctx, broker.NewK8sRunningSource(kcache), cfg.ExpiryInterval, log)
	})

	// Internal broker API on its own mTLS listener (ADR 0003): never shares
	// the public mux. Client certs are verified against the CA bundle; an
	// absent cert gets the 401 error model rather than a handshake abort.
	if cfg.InternalListen != "" {
		rel, err := tlsreload.New(cfg.InternalTLSCert, cfg.InternalTLSKey, tlsreload.WithLogger(log))
		if err != nil {
			return fmt.Errorf("internal tls cert: %w", err)
		}
		b.reloaders = append(b.reloaders, rel)
		caPEM, err := os.ReadFile(cfg.InternalClientCA)
		if err != nil {
			return fmt.Errorf("internal client ca: %w", err)
		}
		cert, err := rel.GetCertificate(nil)
		if err != nil {
			return fmt.Errorf("internal tls cert: %w", err)
		}
		tlsCfg, err := httpapi.ServerTLSConfig(*cert, caPEM)
		if err != nil {
			return fmt.Errorf("internal tls config: %w", err)
		}
		// Serve the cert through the reloader so rotation lands without a
		// restart (D21).
		tlsCfg.Certificates = nil
		tlsCfg.GetCertificate = rel.GetCertificate
		b.internalTLSCfg = tlsCfg
		b.internalHandler = httpapi.NewHandler(httpapi.Config{
			Broker: brk, Audience: id.Audience, OperatorCN: cfg.OperatorCN, Logger: log,
		})
	} else {
		log.Warn("internal broker api disabled (-internal-listen unset); remote gateways cannot redeem tickets")
	}

	// Session gateway over the in-process broker: the LocalGateway pins the
	// identity, applies the lease-id bounds the old mTLS hop enforced, and
	// doubles as the session directory (cookie→lease rehydration, P3/P6).
	if cfg.SessionListen != "" {
		lg, err := b.localGateway(brk, id)
		if err != nil {
			return err
		}
		if err := b.newGateway(cfg, lg, id, metrics, lg); err != nil {
			return err
		}
	}

	// App listener: OIDC login + the public API mux.
	if cfg.Listen != "" {
		if err := b.newAppHandler(ctx, cfg, db, svc, statusView, kc, tenants, retained, brk); err != nil {
			return err
		}
	}
	return nil
}

// wireSplit builds split/test mode: a session listener driving a remote
// broker over mTLS. No DB, OIDC or Kubernetes wiring exists in this mode
// (decisions-2 item 1 option A), so the gateway's session directory stays
// nil (P6).
func (b *Backend) wireSplit(cfg Config, metrics *observability.Metrics, id broker.GatewayIdentity) error {
	bc, err := brokerclient.New(brokerclient.Config{
		BaseURL:  cfg.BrokerURL,
		CertFile: cfg.MTLSCert,
		KeyFile:  cfg.MTLSKey,
		CAFile:   cfg.BrokerCA,
	})
	if err != nil {
		return fmt.Errorf("broker client: %w", err)
	}
	// P6: split/test mode has no session directory — sessions live in this
	// process, exactly as in v0.1.
	return b.newGateway(cfg, bc, id, metrics, nil)
}

// newGateway builds the session gateway handler and records it for Drain.
// sessions is the optional session directory (nil in split mode).
func (b *Backend) newGateway(cfg Config, bc gateway.BrokerClient, id broker.GatewayIdentity, metrics *observability.Metrics, sessions gateway.SessionDirectory) error {
	upCA, err := upstreamCAPool(cfg.UpstreamCA)
	if err != nil {
		return err
	}
	gw, err := gateway.New(gateway.Config{
		Identity:       id,
		PublicOrigin:   cfg.SessionOrigin,
		PortalOrigins:  []string(cfg.PortalOrigins),
		AllowedHosts:   strings.Split(cfg.SessionAllowedHosts, ","),
		Broker:         bc,
		Sessions:       sessions,
		UpstreamCA:     upCA,
		ControlToken:   cfg.ControlToken,
		RenewInterval:  cfg.RenewInterval,
		RevokeDeadline: cfg.RevokeDeadline,
		Metrics:        metrics,
		Audit:          observability.NewJSONSink(os.Stdout),
		Logger:         b.log,
	})
	if err != nil {
		return fmt.Errorf("gateway init: %w", err)
	}
	b.gw = gw
	b.sessionHandler = b.readyz(api.RequestID(gw))
	b.closers = append(b.closers, gw.Close)
	return nil
}

// newAppHandler builds the public API mux (OIDC + workspaces + templates +
// connections + data) and records it on b.
func (b *Backend) newAppHandler(ctx context.Context, cfg Config, db *store.DB,
	svc *provisioning.Service, statusView *api.K8sStatusView, kc client.Client,
	tenants provisioning.TenantNamespaces, retained *provisioning.RetainedStore, brk *broker.Broker) error {

	// Pending OIDC logins ride in an AEAD-sealed cookie (D20): the first
	// key file seals, every configured key opens, so a login can start on
	// one replica and finish on another.
	keys, err := loginstate.LoadKeyFiles(cfg.LoginKeyFiles)
	if err != nil {
		return fmt.Errorf("login key files: %w", err)
	}
	sealer, err := loginstate.NewSealer(keys...)
	if err != nil {
		return fmt.Errorf("login key files: %w", err)
	}
	authn, err := api.NewAuthenticator(ctx, api.AuthConfig{
		Issuer:         cfg.OIDCIssuer,
		ClientID:       cfg.OIDCClientID,
		ClientSecret:   cfg.OIDCClientSecret,
		RedirectURL:    cfg.OIDCRedirectURL,
		SessionOrigin:  cfg.SessionOrigin,
		RequiredGroups: cfg.RequiredGroups,
		LoginSealer:    sealer,
	}, sessionStoreAdapter{s: store.NewSessionStore(db, cfg.SessionIdle)}, b.log)
	if err != nil {
		return fmt.Errorf("oidc: %w", err)
	}

	catalog := catalogAdapter{c: provisioning.NewK8sTemplateCatalog(kc, tenants)}
	wsHandler := api.NewWorkspaceHandler(svc, catalog, tenants).
		WithStatusView(statusView).
		WithImageStaleAfter(cfg.ImageStaleAfter)
	tplHandler := api.NewTemplateHandler(catalog, tenants).
		WithImageStaleAfter(cfg.ImageStaleAfter)
	connHandler := api.NewConnectionHandler(broker.PublicIssuer{B: brk}, tenants, cfg.SessionOrigin)
	dataHandler := api.NewDataHandler(retained, catalog, tenants)

	mux := appMux(authn, wsHandler, tplHandler, connHandler, dataHandler)
	b.appHandler = b.wrapApp(authn, mux, cfg.PortalOrigins)
	return nil
}

// appMux assembles the public API route table. The session surface
// (launch, control, desktop proxy) is deliberately absent: API routes must
// not exist on the session listener and vice versa (D7).
func appMux(authn *api.Authenticator, ws *api.WorkspaceHandler, tpl *api.TemplateHandler,
	conn *api.ConnectionHandler, data *api.DataHandler) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("GET /v1/login", http.HandlerFunc(authn.LoginHandler))
	mux.Handle("GET /v1/auth/callback", http.HandlerFunc(authn.CallbackHandler))
	mux.Handle("POST /v1/logout", authn.RequireAuth(authn.RequireCSRF(http.HandlerFunc(authn.LogoutHandler))))
	api.MountWorkspaceRoutes(mux, authn, ws, tpl)
	api.MountConnectionRoutes(mux, authn, conn)
	api.MountDataRoutes(mux, authn, data)
	return mux
}

// wrapApp applies the production middleware stack to the app mux: request
// ID, audit, the trusted-origin CSRF gate and the readiness endpoint.
func (b *Backend) wrapApp(authn *api.Authenticator, mux http.Handler, portalOrigins []string) http.Handler {
	return b.readyz(api.RequestID(api.Audit(b.log)(
		api.RequireTrustedOrigin(authn.SessionCookieName(), portalOrigins)(mux))))
}

// bind opens every enabled listener and records the servers.
func (b *Backend) bind(cfg Config) error {
	add := func(name, addr string, srv *http.Server, tlsCfg *tls.Config) error {
		if addr == "" {
			return nil
		}
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return fmt.Errorf("%s listener %s: %w", name, addr, err)
		}
		b.servers = append(b.servers, namedServer{name: name, srv: srv, ln: ln, tlsCfg: tlsCfg})
		return nil
	}

	// Session listener TLS is required and hot-reloaded (D21); the app
	// listener TLSes only when cert+key are configured (an ingress may
	// terminate instead); the internal listener is always mTLS.
	var sessionTLS, appTLS *tls.Config
	if cfg.SessionListen != "" {
		rel, err := tlsreload.New(cfg.SessionTLSCert, cfg.SessionTLSKey, tlsreload.WithLogger(b.log))
		if err != nil {
			return fmt.Errorf("session tls cert: %w", err)
		}
		b.reloaders = append(b.reloaders, rel)
		sessionTLS = &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: rel.GetCertificate}
	}
	if cfg.Listen != "" && cfg.TLSCert != "" {
		rel, err := tlsreload.New(cfg.TLSCert, cfg.TLSKey, tlsreload.WithLogger(b.log))
		if err != nil {
			return fmt.Errorf("app tls cert: %w", err)
		}
		b.reloaders = append(b.reloaders, rel)
		appTLS = &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: rel.GetCertificate}
	}

	if err := add("app", cfg.Listen, appServer(cfg.Listen, b.appHandler), appTLS); err != nil {
		return err
	}
	if err := add("session", cfg.SessionListen, sessionServer(cfg.SessionListen, b.sessionHandler), sessionTLS); err != nil {
		return err
	}
	if err := add("internal", cfg.InternalListen, internalServer(cfg.InternalListen, b.internalHandler), b.internalTLSCfg); err != nil {
		return err
	}
	if cfg.MetricsListen != "" {
		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.Handler())
		if err := add("metrics", cfg.MetricsListen, metricsServer(cfg.MetricsListen, mux), nil); err != nil {
			return err
		}
	}
	return nil
}

func restConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	return rest.InClusterConfig()
}

func upstreamCAPool(path string) (*x509.CertPool, error) {
	if path == "" {
		return nil, nil
	}
	pemBytes, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("upstream CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemBytes) {
		return nil, errors.New("upstream CA contains no certificates")
	}
	return pool, nil
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
	}
}
