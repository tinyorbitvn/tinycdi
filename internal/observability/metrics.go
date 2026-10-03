// Package observability defines the platform's Prometheus metric set and
// structured audit logging. Metrics are declared once here; later tasks only
// increment/set them through the bounded helper methods — they never create
// new labels, and the guard test in this package enforces the policy.
//
// LABEL POLICY: no user identity (email, subject, owner, actor), no workspace
// name/ID/UID, and no request IDs may appear as metric labels. The only
// tenant-scoped series use the `tenant` label whose values are bounded by the
// configured tenant allowlist — every value outside the allowlist collapses
// into "other", so cardinality is <= len(allowlist)+1. Bounded enums (code
// class, reason, result) likewise normalize unknown values to "other".
package observability

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// ForbiddenLabelNames enumerates label names that must never appear on any
// registered metric. metrics_test.go gathers the whole registry and fails if
// any of these names show up.
var ForbiddenLabelNames = []string{
	"user", "email", "subject", "sub", "owner", "actor", "principal",
	"workspace", "workspace_id", "workspace_name", "workspace_uid",
	"id", "uid", "request_id", "requestid",
}

const metricNamespace = "tinycdi"

// bounded enums — anything outside maps to "other".
var (
	leaseFailureReasons = map[string]struct{}{
		"expired": {}, "conflict": {}, "denied": {}, "unavailable": {},
	}
	provisioningResults = map[string]struct{}{
		"success": {}, "failure": {},
	}
	// httpListeners are the listeners the HTTP request metrics may name.
	httpListeners = map[string]struct{}{
		"app": {}, "session": {}, "internal": {},
	}
	// rehydrateResults are the session-directory lookup outcomes (E8):
	// ok = a live session was rebuilt, miss = definitively no session
	// (unknown/dead/foreign cookie), error = the directory could not answer.
	rehydrateResults = map[string]struct{}{
		"ok": {}, "miss": {}, "error": {},
	}
	// loginOutcomes are the /v1/auth/callback results: success = session
	// issued, denied = the caller failed a check (bad state, nonce, group
	// or tenant, dead code), error = a platform-side failure.
	loginOutcomes = map[string]struct{}{
		"success": {}, "denied": {}, "error": {},
	}
	// rateLimitRoutes are the bounded route templates whose per-client
	// limiter may refuse a request (E7) — the login family on the app
	// listener and launch on the session listener.
	rateLimitRoutes = map[string]struct{}{
		"/v1/login": {}, "/v1/auth/callback": {}, "/v1/session": {}, "/v1/launch": {},
	}
)

func boundValue(v string, allowed map[string]struct{}) string {
	if _, ok := allowed[v]; ok {
		return v
	}
	return "other"
}

// Metrics holds the full platform metric set. All label-bearing series route
// through bounded helpers; the struct exposes no raw *Vec so callers cannot
// bypass normalization.
type Metrics struct {
	httpRequests   *prometheus.CounterVec
	httpDuration   *prometheus.HistogramVec
	provisioning   *prometheus.HistogramVec
	running        *prometheus.GaugeVec
	reserved       *prometheus.GaugeVec
	leaseFailures  *prometheus.CounterVec
	stuckFinalizer prometheus.Gauge
	quotaDrift     *prometheus.GaugeVec
	pvcLeaks       prometheus.Gauge
	bootDeadline   prometheus.Counter
	sessionsActive prometheus.Gauge
	rehydrations   *prometheus.CounterVec
	streamsFenced  prometheus.Counter
	logins         *prometheus.CounterVec
	imageAge       *prometheus.GaugeVec
	rateLimited    *prometheus.CounterVec

	tenants map[string]struct{}
}

// NewMetrics registers the platform metric set on reg (nil → default
// registerer). tenantAllowlist is the bounded set of valid tenant label
// values; values outside it are recorded as "other".
func NewMetrics(reg prometheus.Registerer, tenantAllowlist []string) *Metrics {
	if reg == nil {
		reg = prometheus.DefaultRegisterer
	}
	m := &Metrics{
		httpRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricNamespace, Name: "http_requests_total",
			Help: "HTTP requests handled, by listener, route template, method and response code class.",
		}, []string{"listener", "route", "method", "code_class"}),
		httpDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: metricNamespace, Name: "http_request_duration_seconds",
			Help:    "HTTP request latency by listener, route template, method and code class.",
			Buckets: prometheus.DefBuckets,
		}, []string{"listener", "route", "method", "code_class"}),
		provisioning: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: metricNamespace, Name: "workspace_provisioning_seconds",
			Help:    "End-to-end workspace provisioning latency by result.",
			Buckets: []float64{0.5, 1, 2, 5, 10, 30, 60, 120, 300, 600},
		}, []string{"result"}),
		running: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: metricNamespace, Name: "workspaces_running",
			Help: "Workspaces currently in Running state, by bounded tenant.",
		}, []string{"tenant"}),
		reserved: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: metricNamespace, Name: "workspaces_reserved",
			Help: "Workspace slots reserved by quota, by bounded tenant.",
		}, []string{"tenant"}),
		leaseFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricNamespace, Name: "lease_failures_total",
			Help: "Connection lease acquire/renew failures by bounded reason.",
		}, []string{"reason"}),
		stuckFinalizer: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: metricNamespace, Name: "finalizers_stuck",
			Help: "Workspaces whose finalizer has been pending past the deadline.",
		}),
		quotaDrift: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: metricNamespace, Name: "quota_drift",
			Help: "Observed drift between quota reservations and actual usage, by bounded tenant.",
		}, []string{"tenant"}),
		pvcLeaks: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: metricNamespace, Name: "pvc_leaks",
			Help: "PersistentVolumeClaims detected without a owning workspace record.",
		}),
		bootDeadline: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: metricNamespace, Name: "boot_deadline_exceeded_total",
			Help: "Workspace runtimes that exceeded their boot deadline.",
		}),
		sessionsActive: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: metricNamespace, Name: "sessions_active",
			Help: "Live desktop sessions this gateway replica currently holds.",
		}),
		rehydrations: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricNamespace, Name: "gateway_rehydrations_total",
			Help: "Session-directory lookups for cookies this replica never saw, by bounded result.",
		}, []string{"result"}),
		streamsFenced: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: metricNamespace, Name: "gateway_streams_fenced_total",
			Help: "Streams closed because another replica claimed the lease's stream epoch.",
		}),
		logins: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricNamespace, Name: "logins_total",
			Help: "Completed /v1/auth/callback login attempts, by bounded outcome.",
		}, []string{"outcome"}),
		imageAge: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: metricNamespace, Name: "runtime_image_age_seconds",
			Help: "Age of the newest published template revision's runtime image, by catalog family.",
		}, []string{"family"}),
		rateLimited: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricNamespace, Name: "rate_limited_total",
			Help: "Requests refused by the per-client rate limiters, by bounded route template.",
		}, []string{"route"}),
		tenants: map[string]struct{}{},
	}
	for _, t := range tenantAllowlist {
		m.tenants[t] = struct{}{}
	}
	reg.MustRegister(
		m.httpRequests, m.httpDuration, m.provisioning, m.running, m.reserved,
		m.leaseFailures, m.stuckFinalizer, m.quotaDrift, m.pvcLeaks, m.bootDeadline,
		m.sessionsActive, m.rehydrations, m.streamsFenced, m.logins, m.imageAge,
		m.rateLimited,
	)
	return m
}

// tenant bounds the tenant label to the configured allowlist.
func (m *Metrics) tenant(v string) string {
	if _, ok := m.tenants[v]; ok {
		return v
	}
	return "other"
}

// ObserveHTTP records one HTTP request. Called by the InstrumentHTTP
// middleware and the session gateway with the listener name, the mux route
// template (never a concrete path), method and code class.
func (m *Metrics) ObserveHTTP(listener, route, method, codeClass string, d time.Duration) {
	l := prometheus.Labels{
		"listener":   boundValue(listener, httpListeners),
		"route":      route,
		"method":     method,
		"code_class": codeClass,
	}
	m.httpRequests.With(l).Inc()
	m.httpDuration.With(l).Observe(d.Seconds())
}

// ObserveProvisioningLatency records a provisioning run duration; result is
// bounded to {success, failure, other}.
func (m *Metrics) ObserveProvisioningLatency(result string, d time.Duration) {
	m.provisioning.WithLabelValues(boundValue(result, provisioningResults)).Observe(d.Seconds())
}

// SetRunningWorkspaces sets the running-workspace gauge for a bounded tenant.
func (m *Metrics) SetRunningWorkspaces(tenant string, v float64) {
	m.running.WithLabelValues(m.tenant(tenant)).Set(v)
}

// AddRunningWorkspaces adjusts the running-workspace gauge for a bounded tenant.
func (m *Metrics) AddRunningWorkspaces(tenant string, delta float64) {
	m.running.WithLabelValues(m.tenant(tenant)).Add(delta)
}

// SetReservedWorkspaces sets the reserved-quota gauge for a bounded tenant.
func (m *Metrics) SetReservedWorkspaces(tenant string, v float64) {
	m.reserved.WithLabelValues(m.tenant(tenant)).Set(v)
}

// AddReservedWorkspaces adjusts the reserved-quota gauge for a bounded tenant.
func (m *Metrics) AddReservedWorkspaces(tenant string, delta float64) {
	m.reserved.WithLabelValues(m.tenant(tenant)).Add(delta)
}

// IncLeaseFailure counts a lease failure; reason is bounded to
// {expired, conflict, denied, unavailable, other}.
func (m *Metrics) IncLeaseFailure(reason string) {
	m.leaseFailures.WithLabelValues(boundValue(reason, leaseFailureReasons)).Inc()
}

// SetStuckFinalizers sets the stuck-finalizer gauge.
func (m *Metrics) SetStuckFinalizers(v float64) { m.stuckFinalizer.Set(v) }

// SetQuotaDrift sets the quota-drift gauge for a bounded tenant.
func (m *Metrics) SetQuotaDrift(tenant string, v float64) {
	m.quotaDrift.WithLabelValues(m.tenant(tenant)).Set(v)
}

// SetPVCLeaks sets the leaked-PVC gauge.
func (m *Metrics) SetPVCLeaks(v float64) { m.pvcLeaks.Set(v) }

// IncBootDeadlineExceeded counts a runtime that exceeded its boot deadline.
func (m *Metrics) IncBootDeadlineExceeded() { m.bootDeadline.Inc() }

// AddSessionsActive adjusts the live-session gauge the session gateway
// maintains for this replica (E8).
func (m *Metrics) AddSessionsActive(delta float64) { m.sessionsActive.Add(delta) }

// IncRehydration counts one session-directory lookup for an unseen cookie;
// result is bounded to {ok, miss, error, other}.
func (m *Metrics) IncRehydration(result string) {
	m.rehydrations.WithLabelValues(boundValue(result, rehydrateResults)).Inc()
}

// IncStreamsFenced counts one cross-replica stream fence: a renew observed
// a stream epoch newer than the one this process claimed, so its streams
// closed.
func (m *Metrics) IncStreamsFenced() { m.streamsFenced.Inc() }

// IncLogin counts one completed login-callback outcome; outcome is bounded
// to {success, denied, error, other}.
func (m *Metrics) IncLogin(outcome string) {
	m.logins.WithLabelValues(boundValue(outcome, loginOutcomes)).Inc()
}

// SetRuntimeImageAge sets the runtime-image-age gauge for a template
// catalog family — values come from WorkspaceTemplate catalog names, which
// are administrator-controlled and bounded by the catalog size.
func (m *Metrics) SetRuntimeImageAge(family string, seconds float64) {
	m.imageAge.WithLabelValues(family).Set(seconds)
}

// DeleteRuntimeImageAge drops a family's series when the catalog no longer
// lists it, so deleted templates do not leave stale gauges behind.
func (m *Metrics) DeleteRuntimeImageAge(family string) {
	m.imageAge.DeleteLabelValues(family)
}

// IncRateLimited counts one rate-limiter refusal; route is bounded to the
// limited route templates {/v1/login, /v1/auth/callback, /v1/session,
// /v1/launch, other}.
func (m *Metrics) IncRateLimited(route string) {
	m.rateLimited.WithLabelValues(boundValue(route, rateLimitRoutes)).Inc()
}
