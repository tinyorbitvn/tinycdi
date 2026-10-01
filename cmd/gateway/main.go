// Command gateway is the tinycdi session gateway: it terminates browser
// desktop traffic on the public session origin (TLS), redeems launch tickets
// and enforces leases through the internal broker over mTLS, and reverse
// proxies HTTP/WebSocket to workspace runtimes with pinned-CA verification
// and server-side credential injection (design §6).
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
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

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/tinyorbitvn/tinycdi/internal/api"
	"github.com/tinyorbitvn/tinycdi/internal/broker"
	"github.com/tinyorbitvn/tinycdi/internal/gateway"
	"github.com/tinyorbitvn/tinycdi/internal/gateway/brokerclient"
	"github.com/tinyorbitvn/tinycdi/internal/observability"
	"github.com/tinyorbitvn/tinycdi/internal/sessionhost"
)

// stringList collects repeated flags and comma-separated values into one
// list (used for -portal-origin / TCDI_GW_PORTAL_ORIGINS).
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

type config struct {
	listen         string
	metricsListen  string
	tlsCert        string
	tlsKey         string
	publicOrigin   string
	portalOrigins  stringList
	allowedHosts   string // comma-separated
	gatewayID      string // CN of the client cert; flag overrides
	audience       string
	brokerURL      string
	brokerCA       string
	mtlsCert       string
	mtlsKey        string
	upstreamCA     string // optional default CA bundle PEM
	controlToken   string // env/file only — never a flag value
	controlTokenF  string
	tenantAllow    string
	renewInterval  time.Duration
	revokeDeadline time.Duration
}

// controlTokenUnsetWarning is logged at startup when no control token is
// configured (SEC-I2): the /v1/control surface fails CLOSED in that state —
// it is disabled, not unauthenticated.
const controlTokenUnsetWarning = "control token unset — /v1/control/* is disabled (fail closed); set -control-token-file or TCDI_GW_CONTROL_TOKEN to enable"

// publicServer builds the session listener (SEC-23). ReadHeaderTimeout
// bounds header parsing, IdleTimeout bounds idle keep-alive conns and
// MaxHeaderBytes caps header size. ReadTimeout and WriteTimeout must stay
// zero: Go applies them as absolute conn deadlines that survive Hijack()
// and would kill long-lived WebSocket desktop streams.
func publicServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
		TLSConfig:         &tls.Config{MinVersion: tls.VersionTLS12},
	}
}

// metricsServer builds the metrics listener (SEC-23): scrape-only, so every
// request phase is bounded.
func metricsServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func parseConfig() (config, error) {
	var c config
	flag.StringVar(&c.listen, "listen", envOr("TCDI_GW_LISTEN", ":8443"), "public HTTPS listen address")
	flag.StringVar(&c.metricsListen, "metrics-listen", envOr("TCDI_GW_METRICS_LISTEN", ""), "metrics listen address (empty disables)")
	flag.StringVar(&c.tlsCert, "tls-cert", envOr("TCDI_GW_TLS_CERT", ""), "public TLS certificate (PEM)")
	flag.StringVar(&c.tlsKey, "tls-key", envOr("TCDI_GW_TLS_KEY", ""), "public TLS key (PEM)")
	flag.StringVar(&c.publicOrigin, "public-origin", envOr("TCDI_GW_PUBLIC_ORIGIN", ""), "public session origin, e.g. https://session.example.dev")
	flag.Var(&c.portalOrigins, "portal-origin", "portal origin allowed to POST /v1/launch (repeatable or CSV; env TCDI_GW_PORTAL_ORIGINS)")
	flag.StringVar(&c.allowedHosts, "allowed-hosts", envOr("TCDI_GW_ALLOWED_HOSTS", ""), "comma-separated Host allowlist")
	flag.StringVar(&c.gatewayID, "gateway-id", envOr("TCDI_GW_ID", ""), "gateway identity (default: client cert CN)")
	flag.StringVar(&c.audience, "gateway-audience", envOr("TCDI_GW_AUDIENCE", ""), "ticket/lease audience this gateway serves")
	flag.StringVar(&c.brokerURL, "broker-url", envOr("TCDI_GW_BROKER_URL", ""), "internal broker base URL (https)")
	flag.StringVar(&c.brokerCA, "broker-ca", envOr("TCDI_GW_BROKER_CA", ""), "PEM CA bundle verifying the broker listener")
	flag.StringVar(&c.mtlsCert, "mtls-cert", envOr("TCDI_GW_MTLS_CERT", ""), "client certificate for the broker (PEM)")
	flag.StringVar(&c.mtlsKey, "mtls-key", envOr("TCDI_GW_MTLS_KEY", ""), "client key for the broker (PEM)")
	flag.StringVar(&c.upstreamCA, "upstream-ca", envOr("TCDI_GW_UPSTREAM_CA", ""), "optional default PEM CA for runtime upstreams")
	flag.StringVar(&c.controlTokenF, "control-token-file", envOr("TCDI_GW_CONTROL_TOKEN_FILE", ""), "file containing the control-endpoint bearer token")
	flag.StringVar(&c.tenantAllow, "tenant-allowlist", envOr("TCDI_TENANT_ALLOWLIST", ""), "bounded tenant label values, comma-separated")
	flag.DurationVar(&c.renewInterval, "renew-interval", gateway.LeaseRenewInterval, "lease renew cadence")
	flag.DurationVar(&c.revokeDeadline, "revoke-deadline", gateway.RevokeDeadline, "fail-closed budget after last successful renew")
	flag.Parse()
	if len(c.portalOrigins) == 0 {
		_ = c.portalOrigins.Set(envOr("TCDI_GW_PORTAL_ORIGINS", ""))
	}
	c.controlToken = os.Getenv("TCDI_GW_CONTROL_TOKEN")

	missing := []string{}
	for name, v := range map[string]string{
		"tls-cert": c.tlsCert, "tls-key": c.tlsKey, "public-origin": c.publicOrigin,
		"allowed-hosts": c.allowedHosts, "broker-url": c.brokerURL,
		"broker-ca": c.brokerCA, "mtls-cert": c.mtlsCert, "mtls-key": c.mtlsKey,
	} {
		if v == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return c, fmt.Errorf("required: %s", strings.Join(missing, ", "))
	}
	return c, nil
}

// gatewayCN extracts the gateway identity from the client certificate's
// Subject CN when -gateway-id is not set. It reads the first CERTIFICATE
// PEM block only — the file needs no private key.
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

func main() {
	cfg, err := parseConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		os.Exit(2)
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if cfg.controlTokenF != "" {
		b, err := os.ReadFile(cfg.controlTokenF)
		if err != nil {
			log.Error("control token file", "err", err)
			os.Exit(1)
		}
		cfg.controlToken = strings.TrimSpace(string(b))
	}
	if cfg.controlToken == "" {
		log.Warn(controlTokenUnsetWarning)
	}
	if cfg.gatewayID == "" {
		id, err := gatewayCN(cfg.mtlsCert)
		if err != nil {
			log.Error("gateway id from client cert", "err", err)
			os.Exit(1)
		}
		if id == "" {
			log.Error("client cert has no Subject CN; pass -gateway-id")
			os.Exit(2)
		}
		cfg.gatewayID = id
	}

	bc, err := brokerclient.New(brokerclient.Config{
		BaseURL:  cfg.brokerURL,
		CertFile: cfg.mtlsCert,
		KeyFile:  cfg.mtlsKey,
		CAFile:   cfg.brokerCA,
	})
	if err != nil {
		log.Error("broker client", "err", err)
		os.Exit(1)
	}

	var upCA *x509.CertPool
	if cfg.upstreamCA != "" {
		pemBytes, err := os.ReadFile(cfg.upstreamCA)
		if err != nil {
			log.Error("upstream CA", "err", err)
			os.Exit(1)
		}
		upCA = x509.NewCertPool()
		if !upCA.AppendCertsFromPEM(pemBytes) {
			log.Error("upstream CA contains no certificates")
			os.Exit(2)
		}
	}

	var metrics *observability.Metrics
	if cfg.metricsListen != "" {
		metrics = observability.NewMetrics(prometheus.DefaultRegisterer,
			strings.FieldsFunc(cfg.tenantAllow, func(r rune) bool { return r == ',' }))
	}

	// The gateway now serves each workspace on its own host under the
	// session domain (D9); -public-origin carries the domain as
	// https://<domain> until the merged backend binary replaces this
	// command's flags with -session-domain/-session-control-hosts.
	pubURL, err := url.Parse(cfg.publicOrigin)
	if err != nil || pubURL.Host == "" {
		log.Error("public-origin must be an https origin", "value", cfg.publicOrigin)
		os.Exit(2)
	}
	sessionDomain, err := sessionhost.ParseDomain(pubURL.Host)
	if err != nil {
		log.Error("public-origin is not a usable session domain", "err", err)
		os.Exit(2)
	}

	gw, err := gateway.New(gateway.Config{
		Identity:       broker.GatewayIdentity{ID: cfg.gatewayID, Audience: cfg.audience},
		SessionDomain:  sessionDomain,
		PortalOrigins:  []string(cfg.portalOrigins),
		ControlHosts:   strings.Split(cfg.allowedHosts, ","),
		Broker:         bc,
		UpstreamCA:     upCA,
		ControlToken:   cfg.controlToken,
		RenewInterval:  cfg.renewInterval,
		RevokeDeadline: cfg.revokeDeadline,
		Metrics:        metrics,
		Audit:          observability.NewJSONSink(os.Stdout),
		Logger:         log,
	})
	if err != nil {
		log.Error("gateway init", "err", err)
		os.Exit(1)
	}
	defer gw.Close()

	srv := publicServer(cfg.listen, api.RequestID(gw))
	if metrics != nil {
		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.Handler())
		msrv := metricsServer(cfg.metricsListen, mux)
		go func() {
			log.Info("metrics listening", "addr", cfg.metricsListen)
			if err := msrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Error("metrics serve", "err", err)
			}
		}()
		defer func() {
			shCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = msrv.Shutdown(shCtx)
		}()
	}
	go func() {
		<-ctx.Done()
		shCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shCtx)
	}()
	log.Info("gateway listening", "addr", cfg.listen, "origin", cfg.publicOrigin, "gateway", cfg.gatewayID)
	if err := srv.ListenAndServeTLS(cfg.tlsCert, cfg.tlsKey); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("serve", "err", err)
		os.Exit(1)
	}
}
