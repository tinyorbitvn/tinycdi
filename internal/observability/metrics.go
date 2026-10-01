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
			Help: "HTTP requests handled, by route template, method and response code class.",
		}, []string{"route", "method", "code_class"}),
		httpDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: metricNamespace, Name: "http_request_duration_seconds",
			Help:    "HTTP request latency by route template, method and code class.",
			Buckets: prometheus.DefBuckets,
		}, []string{"route", "method", "code_class"}),
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
		tenants: map[string]struct{}{},
	}
	for _, t := range tenantAllowlist {
		m.tenants[t] = struct{}{}
	}
	reg.MustRegister(
		m.httpRequests, m.httpDuration, m.provisioning, m.running, m.reserved,
		m.leaseFailures, m.stuckFinalizer, m.quotaDrift, m.pvcLeaks, m.bootDeadline,
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
// middleware with the mux route template, method and code class.
func (m *Metrics) ObserveHTTP(route, method, codeClass string, d time.Duration) {
	l := prometheus.Labels{"route": route, "method": method, "code_class": codeClass}
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
