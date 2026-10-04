// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package main

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/netip"
	"os"
	"regexp"
	"strings"

	corev1 "k8s.io/api/core/v1"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	workspacesv1alpha1 "github.com/tinyorbitvn/tinycdi/api/v1alpha1"
	"github.com/tinyorbitvn/tinycdi/internal/broker/httpapi/opclient"
	"github.com/tinyorbitvn/tinycdi/internal/operator"
	linuxruntime "github.com/tinyorbitvn/tinycdi/internal/runtime/linux"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

// parseWatchNamespaces turns a comma-separated namespace list into the
// cache.Options.DefaultNamespaces value (each entry an empty cache.Config).
func parseWatchNamespaces(csv string) map[string]cache.Config {
	out := map[string]cache.Config{}
	for _, ns := range strings.Split(csv, ",") {
		if ns = strings.TrimSpace(ns); ns != "" {
			out[ns] = cache.Config{}
		}
	}
	return out
}

// parseExceptCIDRs validates the --internet-except-cidrs list: every entry
// must be a well-formed IPv4 prefix — they land in the except list of an
// ipBlock whose parent is 0.0.0.0/0, so a typo or a v6 prefix would make
// EVERY workspace boundary policy unappliable (the apiserver would reject
// it), silently leaving runtime pods without their egress boundary.
func parseExceptCIDRs(csv string) ([]string, error) {
	if csv == "" {
		return nil, nil
	}
	var out []string
	for _, c := range strings.Split(csv, ",") {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		p, err := netip.ParsePrefix(c)
		if err != nil || !p.Addr().Is4() {
			return nil, fmt.Errorf("invalid IPv4 CIDR %q in --internet-except-cidrs", c)
		}
		out = append(out, p.String())
	}
	return out, nil
}

// kasmAdapterImagePattern enforces a digest-pinned OCI reference — the
// same shape spec.linux.image requires. The adapter init image is
// operator-supplied infrastructure; a mutable tag would let an image
// swap change what runs inside every kasm-template pod.
var kasmAdapterImagePattern = regexp.MustCompile(workspacesv1alpha1.DigestPattern)

// parseKasmAdapterImage validates --kasm-adapter-image: empty stays empty
// (adapter=kasm templates are then rejected by the backend); a set value
// must be digest-pinned.
func parseKasmAdapterImage(v string) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", nil
	}
	if !kasmAdapterImagePattern.MatchString(v) {
		return "", fmt.Errorf("--kasm-adapter-image %q is not a digest-pinned reference "+
			"(<repo>@sha256:<64 hex>)", v)
	}
	return v, nil
}

// runtimePlacement carries the operator-wide scheduling defaults for
// runtime pods, parsed from the --runtime-* flags. The linux backend
// applies them per field when a template leaves the matching
// spec.placement field unset.
type runtimePlacement struct {
	nodeSelector map[string]string
	tolerations  []corev1.Toleration
	hostUsers    *bool
}

// runtimePlacementFlags are the raw --runtime-* flag strings; parse()
// validates them into a runtimePlacement.
type runtimePlacementFlags struct {
	nodeSelector string
	tolerations  string
	hostUsers    string
}

// bindRuntimePlacementFlags registers the runtime placement flags on fs.
// The chart renders them from runtime.placement (dedicated pool by
// default); an empty flag leaves the backend default unset.
func bindRuntimePlacementFlags(fs *flag.FlagSet, pf *runtimePlacementFlags) {
	fs.StringVar(&pf.nodeSelector, "runtime-node-selector", "",
		"JSON object of node labels every runtime pod selects "+
			"(e.g. {\"cdi.tinyorbit.vn/workspace\":\"true\"}). Default for "+
			"spec.placement.nodeSelector — a template may override it.")
	fs.StringVar(&pf.tolerations, "runtime-tolerations", "",
		"JSON array of tolerations every runtime pod carries "+
			"(e.g. [{\"key\":\"cdi.tinyorbit.vn/workspace\",\"operator\":\"Exists\","+
			"\"effect\":\"NoSchedule\"}]). Default for spec.placement.tolerations.")
	fs.StringVar(&pf.hostUsers, "runtime-host-users", "",
		"Default hostUsers for runtime pods: \"true\" or \"false\"; empty "+
			"leaves pod.spec.hostUsers unset. Default for spec.linux.hostUsers.")
}

// bindRuntimeAppArmorFlag registers --runtime-apparmor-require-default. True
// (the default) keeps today's behaviour: every runtime container carries an
// explicit RuntimeDefault AppArmor profile, which a node without AppArmor
// refuses. False omits it (a Localhost profile from a template is still
// set) — the supported setting for kind and SELinux-based distributions.
func bindRuntimeAppArmorFlag(fs *flag.FlagSet, requireDefault *bool) {
	fs.BoolVar(requireDefault, "runtime-apparmor-require-default", true,
		"Set securityContext.appArmorProfile=RuntimeDefault on runtime containers. "+
			"Set false on nodes without AppArmor (kind, SELinux-based distributions); "+
			"a Localhost profile requested by a template is always still set.")
}

// parse validates the --runtime-* flag strings. Every error names the
// offending flag — main() exits non-zero on any of them, so a typo can
// never silently drop placement (a missing selector on a dedicated pool
// would strand every runtime pod Pending).
func (pf runtimePlacementFlags) parse() (runtimePlacement, error) {
	var p runtimePlacement
	if s := strings.TrimSpace(pf.nodeSelector); s != "" {
		if err := json.Unmarshal([]byte(s), &p.nodeSelector); err != nil {
			return p, fmt.Errorf("--runtime-node-selector is not a JSON string map: %w", err)
		}
	}
	if s := strings.TrimSpace(pf.tolerations); s != "" {
		if err := json.Unmarshal([]byte(s), &p.tolerations); err != nil {
			return p, fmt.Errorf("--runtime-tolerations is not a JSON array of tolerations: %w", err)
		}
		for i, tol := range p.tolerations {
			if err := validToleration(tol); err != nil {
				return p, fmt.Errorf("--runtime-tolerations entry %d: %w", i, err)
			}
		}
	}
	switch s := strings.TrimSpace(pf.hostUsers); s {
	case "":
	case "true":
		v := true
		p.hostUsers = &v
	case "false":
		v := false
		p.hostUsers = &v
	default:
		return p, fmt.Errorf("--runtime-host-users must be \"true\" or \"false\", got %q", pf.hostUsers)
	}
	return p, nil
}

// validToleration rejects the two ways a JSON toleration can silently do
// the wrong thing: an empty key tolerates every taint under operator
// Exists (control-plane included), and unenumed operator/effect strings
// fail only at pod-admission time, per workspace, far from the flag.
func validToleration(t corev1.Toleration) error {
	if t.Key == "" {
		return errors.New("empty key")
	}
	switch t.Operator {
	case "", corev1.TolerationOpExists, corev1.TolerationOpEqual:
	default:
		return fmt.Errorf("invalid operator %q", t.Operator)
	}
	switch t.Effect {
	case "", corev1.TaintEffectNoSchedule, corev1.TaintEffectPreferNoSchedule, corev1.TaintEffectNoExecute:
	default:
		return fmt.Errorf("invalid effect %q", t.Effect)
	}
	return nil
}

// brokerFlags carries the operator -> internal-broker wiring (ADR 0003):
// the mTLS client certificate whose CN is the operator identity on the
// workspace-scoped revoke/drain routes.
type brokerFlags struct {
	internalURL      string
	caFile           string
	certFile         string
	keyFile          string
	devAllowNoBroker bool
}

// bindBrokerFlags registers the broker flags on fs; getenv supplies the
// env fallbacks (injectable for tests).
func bindBrokerFlags(fs *flag.FlagSet, bf *brokerFlags, getenv func(string) string) {
	fs.StringVar(&bf.internalURL, "broker-internal-url", getenv("TCDI_BROKER_INTERNAL_URL"),
		"Internal broker base URL (https, env TCDI_BROKER_INTERNAL_URL). Required in production: "+
			"the teardown finalizer's lease/stream seams live behind it.")
	fs.StringVar(&bf.caFile, "broker-ca-file", getenv("TCDI_BROKER_CA_FILE"),
		"PEM CA bundle verifying the broker internal listener (env TCDI_BROKER_CA_FILE).")
	fs.StringVar(&bf.certFile, "broker-client-cert-file", getenv("TCDI_BROKER_CLIENT_CERT_FILE"),
		"Operator mTLS client certificate, CN=operator (env TCDI_BROKER_CLIENT_CERT_FILE).")
	fs.StringVar(&bf.keyFile, "broker-client-key-file", getenv("TCDI_BROKER_CLIENT_KEY_FILE"),
		"Operator mTLS client key (env TCDI_BROKER_CLIENT_KEY_FILE).")
	fs.BoolVar(&bf.devAllowNoBroker, "dev-allow-no-broker", getenv("TCDI_DEV_ALLOW_NO_BROKER") == "true",
		"DEV/ENVTEST ONLY: run with no broker — teardown uses the standalone no-op seams "+
			"instead of failing fast.")
}

// brokerSeam builds the operator's client for the internal broker API. A
// nil client is returned only in dev-allow-no-broker mode; any other
// misconfiguration is a hard error — the finalizer must never run its
// teardown order silently past a missing broker seam in production.
func brokerSeam(bf brokerFlags) (*opclient.Client, error) {
	if bf.internalURL == "" {
		if bf.devAllowNoBroker {
			return nil, nil
		}
		return nil, errors.New("--broker-internal-url is required: the teardown finalizer " +
			"cannot run without the broker lease/stream seam (use --dev-allow-no-broker for envtest)")
	}
	if bf.caFile == "" || bf.certFile == "" || bf.keyFile == "" {
		return nil, errors.New("--broker-internal-url requires --broker-ca-file, " +
			"--broker-client-cert-file and --broker-client-key-file")
	}
	return opclient.New(opclient.Config{
		BaseURL:  bf.internalURL,
		CertFile: bf.certFile,
		KeyFile:  bf.keyFile,
		CAFile:   bf.caFile,
	})
}

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(workspacesv1alpha1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

// nolint:gocyclo
func main() {
	var metricsAddr string
	var metricsCertPath, metricsCertName, metricsCertKey string
	var webhookCertPath, webhookCertName, webhookCertKey string
	var webhookPort int
	var enableLeaderElection bool
	var probeAddr string
	var secureMetrics bool
	var enableHTTP2 bool
	var internetExceptCIDRs string
	var disableBuiltinExcepts bool
	var gatewayNamespace string
	var kasmAdapterImage string
	var watchNamespaces string
	var leaderElectionNamespace string
	var bf brokerFlags
	var pf runtimePlacementFlags
	var appArmorRequireDefault bool
	var tlsOpts []func(*tls.Config)
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&secureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flag.StringVar(&webhookCertPath, "webhook-cert-path", "", "The directory that contains the webhook certificate.")
	flag.StringVar(&webhookCertName, "webhook-cert-name", "tls.crt", "The name of the webhook certificate file.")
	flag.StringVar(&webhookCertKey, "webhook-cert-key", "tls.key", "The name of the webhook key file.")
	flag.IntVar(&webhookPort, "webhook-port", 9443, "Port the webhook server listens on. "+
		"Defaults to 9443. Set -1 to disable the webhook server.")
	flag.StringVar(&metricsCertPath, "metrics-cert-path", "",
		"The directory that contains the metrics server certificate.")
	flag.StringVar(&metricsCertName, "metrics-cert-name", "tls.crt", "The name of the metrics server certificate file.")
	flag.StringVar(&metricsCertKey, "metrics-cert-key", "tls.key", "The name of the metrics server key file.")
	flag.StringVar(&internetExceptCIDRs, "internet-except-cidrs", "",
		"Comma-separated IPv4 CIDRs subtracted from the 0.0.0.0/0 allow for "+
			"networkProfile=InternetOnly — the cluster pod/service/node ranges. "+
			"Applied ON TOP of the built-in private/reserved excepts "+
			"(10/8, 172.16/12, 192.168/16, 100.64/10, 127/8, 0/8, 169.254/16, 224/4, 240/4).")
	flag.BoolVar(&disableBuiltinExcepts, "disable-builtin-egress-excepts", false,
		"Disable the built-in InternetOnly egress excepts (break-glass only: without them "+
			"workspace users can reach private/loopback/link-local/multicast ranges — "+
			"on flat CNIs that includes the rest of the cluster).")
	flag.StringVar(&kasmAdapterImage, "kasm-adapter-image", os.Getenv("TCDI_KASM_ADAPTER_IMAGE"),
		"Digest-pinned image ref (<repo>@sha256:<64 hex>) of the kasm runtime adapter "+
			"(tinycdi-kasm-adapter) — the initContainer image injected into pods of "+
			"templates with spec.linux.adapter=kasm (env TCDI_KASM_ADAPTER_IMAGE). "+
			"Empty rejects kasm templates.")
	flag.StringVar(&gatewayNamespace, "gateway-namespace", os.Getenv("POD_NAMESPACE"),
		"Namespace the session gateway pods run in; per-workspace NetworkPolicies admit "+
			"ingress only from pods labeled workspaces.cdi.tinyorbit.vn/role=gateway there "+
			"(env POD_NAMESPACE). Empty scopes ingress to each workspace's own namespace.")
	flag.BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")
	flag.StringVar(&watchNamespaces, "watch-namespaces", os.Getenv("TCDI_WATCH_NAMESPACES"),
		"Comma-separated namespaces the informer cache watches (env TCDI_WATCH_NAMESPACES). "+
			"Set to the managed namespaces when the operator's ClusterRole is bound via "+
			"RoleBindings only; empty means watch all namespaces (requires cluster-wide RBAC).")
	flag.StringVar(&leaderElectionNamespace, "leader-election-namespace", os.Getenv("POD_NAMESPACE"),
		"Namespace for the leader-election Lease (env POD_NAMESPACE). Required when leader "+
			"election is enabled and the operator is bound via RoleBindings only.")
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	bindBrokerFlags(flag.CommandLine, &bf, os.Getenv)
	bindRuntimePlacementFlags(flag.CommandLine, &pf)
	bindRuntimeAppArmorFlag(flag.CommandLine, &appArmorRequireDefault)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// if the enable-http2 flag is false (the default), http/2 should be disabled
	// due to its vulnerabilities. More specifically, disabling http/2 will
	// prevent from being vulnerable to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("Disabling HTTP/2")
		c.NextProtos = []string{"http/1.1"}
	}

	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	// Initial webhook TLS options
	webhookTLSOpts := tlsOpts
	webhookServerOptions := webhook.Options{
		TLSOpts: webhookTLSOpts,
		Port:    webhookPort,
	}

	if len(webhookCertPath) > 0 {
		setupLog.Info("Initializing webhook certificate watcher using provided certificates",
			"webhook-cert-path", webhookCertPath, "webhook-cert-name", webhookCertName, "webhook-cert-key", webhookCertKey)

		webhookServerOptions.CertDir = webhookCertPath
		webhookServerOptions.CertName = webhookCertName
		webhookServerOptions.KeyName = webhookCertKey
	}

	webhookServer := webhook.NewServer(webhookServerOptions)

	// Metrics endpoint is enabled in 'config/default/kustomization.yaml'. The Metrics options configure the server.
	// More info:
	// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.25.0/pkg/metrics/server
	// - https://book.kubebuilder.io/reference/metrics.html
	metricsServerOptions := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
		TLSOpts:       tlsOpts,
	}

	if secureMetrics {
		// FilterProvider is used to protect the metrics endpoint with authn/authz.
		// These configurations ensure that only authorized users and service accounts
		// can access the metrics endpoint. The RBAC are configured in 'config/rbac/kustomization.yaml'. More info:
		// https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.25.0/pkg/metrics/filters#WithAuthenticationAndAuthorization
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	// If the certificate is not specified, controller-runtime will automatically
	// generate self-signed certificates for the metrics server. While convenient for development and testing,
	// this setup is not recommended for production.
	//
	// TODO(user): If you enable certManager, uncomment the following lines:
	// - [METRICS-WITH-CERTS] at config/default/kustomization.yaml to generate and use certificates
	// managed by cert-manager for the metrics server.
	// - [PROMETHEUS-WITH-CERTS] at config/prometheus/kustomization.yaml for TLS certification.
	if len(metricsCertPath) > 0 {
		setupLog.Info("Initializing metrics certificate watcher using provided certificates",
			"metrics-cert-path", metricsCertPath, "metrics-cert-name", metricsCertName, "metrics-cert-key", metricsCertKey)

		metricsServerOptions.CertDir = metricsCertPath
		metricsServerOptions.CertName = metricsCertName
		metricsServerOptions.KeyName = metricsCertKey
	}

	cacheOpts := cache.Options{}
	if watchNamespaces != "" {
		cacheOpts.DefaultNamespaces = parseWatchNamespaces(watchNamespaces)
		setupLog.Info("Informers restricted to managed namespaces", "namespaces", watchNamespaces)
	} else {
		setupLog.Info("WARNING: --watch-namespaces unset; watching ALL namespaces requires cluster-wide RBAC")
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                  scheme,
		Metrics:                 metricsServerOptions,
		WebhookServer:           webhookServer,
		HealthProbeBindAddress:  probeAddr,
		LeaderElection:          enableLeaderElection,
		LeaderElectionID:        "b6b73984.cdi.tinyorbit.vn",
		LeaderElectionNamespace: leaderElectionNamespace,
		Cache:                   cacheOpts,
		// LeaderElectionReleaseOnCancel defines if the leader should step down voluntarily
		// when the Manager ends. This requires the binary to immediately end when the
		// Manager is stopped, otherwise, this setting is unsafe. Setting this significantly
		// speeds up voluntary leader transitions as the new leader don't have to wait
		// LeaseDuration time first.
		//
		// In the default scaffold provided, the program ends immediately after
		// the manager stops, so would be fine to enable this option. However,
		// if you are doing or is intended to do any operation such as perform cleanups
		// after the manager stops then its usage might be unsafe.
		// LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		setupLog.Error(err, "Failed to start manager")
		os.Exit(1)
	}

	exceptCIDRs, err := parseExceptCIDRs(internetExceptCIDRs)
	if err != nil {
		setupLog.Error(err, "invalid egress except configuration")
		os.Exit(1)
	}
	kasmAdapter, err := parseKasmAdapterImage(kasmAdapterImage)
	if err != nil {
		setupLog.Error(err, "invalid kasm adapter image")
		os.Exit(1)
	}
	placement, err := pf.parse()
	if err != nil {
		setupLog.Error(err, "invalid runtime placement flags")
		os.Exit(1)
	}
	if gatewayNamespace == "" {
		setupLog.Info("WARNING: --gateway-namespace unset (POD_NAMESPACE empty); " +
			"runtime ingress is scoped to each workspace's own namespace — " +
			"gateway pods elsewhere cannot reach the runtimes")
	}

	// Internal-broker seam for the teardown finalizer (ADR 0003): the
	// operator cert (CN=operator) is the only identity the API's internal
	// listener admits on the workspace revoke/drain routes. Fail fast when
	// it cannot be built — a nil seam must never silently reach the
	// finalizer in production.
	bkc, err := brokerSeam(bf)
	if err != nil {
		setupLog.Error(err, "broker client configuration invalid")
		os.Exit(1)
	}
	if bkc == nil {
		setupLog.Info("no internal broker configured (--dev-allow-no-broker): " +
			"teardown runs with the standalone seams — envtest/dev only")
	}

	workspaceReconciler := &operator.WorkspaceReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		Backend: linuxruntime.New(mgr.GetClient(), linuxruntime.Options{
			InternetExceptCIDRs:         exceptCIDRs,
			DisableBuiltinEgressExcepts: disableBuiltinExcepts,
			GatewayNamespace:            gatewayNamespace,
			KasmAdapterImage:            kasmAdapter,
			DefaultPlacement: workspacesv1alpha1.PlacementSpec{
				NodeSelector: placement.nodeSelector,
				Tolerations:  placement.tolerations,
			},
			DefaultHostUsers:    placement.hostUsers,
			AppArmorNotRequired: !appArmorRequireDefault,
		}),
		// Retention is explicit: dataPolicy Retain stamps persistent PVCs
		// into the controller-owned inventory, Ephemeral destroys them.
		Retention: &operator.RetentionApplier{
			Client:    mgr.GetClient(),
			Inventory: operator.NewRetentionInventory(mgr.GetClient()),
		},
	}
	if bkc != nil {
		workspaceReconciler.Leases = bkc
		workspaceReconciler.Drainer = bkc
	}
	if err := workspaceReconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Workspace")
		os.Exit(1)
	}

	// +kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("Starting manager")
	ctx := ctrl.SetupSignalHandler()
	if bkc != nil {
		// Hot-reload the operator mTLS client certificate and the
		// broker server CA bundle (E5); the loops end when the
		// manager's signal context is cancelled.
		go bkc.Run(ctx)
	}
	if err := mgr.Start(ctx); err != nil {
		setupLog.Error(err, "Failed to run manager")
		os.Exit(1)
	}
}
