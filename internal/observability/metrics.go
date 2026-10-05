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
	// httpMethods are the methods the HTTP request metrics may name — a
	// client-sent method must never mint a new label value.
	httpMethods = map[string]struct{}{
		"GET": {}, "HEAD": {}, "POST": {}, "PUT": {}, "PATCH": {},
		"DELETE": {}, "OPTIONS": {},
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
	// rateLimitWindowRoutes are the bounded limiter families whose
	// Postgres window check may fail (ADR 0006): the shared login bucket,
	// the callback's per-IP ceiling and the launch budget.
	rateLimitWindowRoutes = map[string]struct{}{
		"login": {}, "callback_ceiling": {}, "launch": {},
	}
	// frameReloadDests are the browsing-context destinations a counted
	// session-frame re-navigation may report (Sec-Fetch-Dest):
	// iframe = the portal's embedded session frame, document = a
	// top-level load (open-in-new-tab / full reload). Absent or
	// unrecognized values fold into "other".
	frameReloadDests = map[string]struct{}{
		"iframe": {}, "document": {},
	}
	// sessionRevokeResults are the sign-out revocation outcomes (S17):
	// ok = the lease store accepted the revoke (zero or more of the
	// session's leases and outstanding tickets ended); error = the store
	// could not answer — the sign-out still completed locally.
	sessionRevokeResults = map[string]struct{}{
		"ok": {}, "error": {},
	}
	// leaseSessionMissingReasons are the bound-portal-session check
	// outcomes that revoked a live lease (S17 defence-in-depth):
	// absent = the sessions row is gone; invalid = the row exists but
	// fails the epoch or absolute-expiry check (e.g. a restored dump).
	leaseSessionMissingReasons = map[string]struct{}{
		"absent": {}, "invalid": {},
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
	rateLimitStore *prometheus.CounterVec
	rateLimitDown  *prometheus.GaugeVec
	frameReloads   *prometheus.CounterVec
	sessionRevokes *prometheus.CounterVec
	leaseSessGone  *prometheus.CounterVec

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
		rateLimitStore: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricNamespace, Name: "rate_limit_store_errors_total",
			Help: "Postgres-backed rate-limit window check failures (fail-open to the local ceiling), by bounded limiter family. Counts real store errors only — checks skipped while the circuit breaker is open are not counted.",
		}, []string{"route"}),
		rateLimitDown: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: metricNamespace, Name: "rate_limit_store_degraded",
			Help: "Rate-limit window store circuit-breaker state by bounded limiter family: 1 while open (Postgres checks skipped, divided local limiter enforcing), 0 while closed.",
		}, []string{"route"}),
		frameReloads: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricNamespace, Name: "session_frame_reloads_total",
			Help: "Session-frame document loads re-navigating a session whose lease already had a stream, by bounded destination. Only same-tab reloads count: a load whose embedded claiming tab id differs from the lease's stream owner (second-tab takeover) is excluded, and the client's in-frame websocket retries never produce a document load.",
		}, []string{"dest"}),
		sessionRevokes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricNamespace, Name: "session_revocations_total",
			Help: "Sign-out revocations of session-bound leases/tickets, by bounded result.",
		}, []string{"result"}),
		leaseSessGone: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricNamespace, Name: "lease_session_missing_total",
			Help: "Leases revoked because the bound portal session row was absent or failed the epoch/expiry check (S17 defence-in-depth), by bounded reason.",
		}, []string{"reason"}),
		tenants: map[string]struct{}{},
	}
	for _, t := range tenantAllowlist {
		m.tenants[t] = struct{}{}
	}
	reg.MustRegister(
		m.httpRequests, m.httpDuration, m.provisioning, m.running, m.reserved,
		m.leaseFailures, m.stuckFinalizer, m.quotaDrift, m.pvcLeaks, m.bootDeadline,
		m.sessionsActive, m.rehydrations, m.streamsFenced, m.logins, m.imageAge,
		m.rateLimited, m.rateLimitStore, m.rateLimitDown, m.frameReloads, m.sessionRevokes,
		m.leaseSessGone,
	)
	// A state gauge reads "no data" until first touched — seed every
	// bounded family at 0 (closed) so dashboards see the healthy state.
	for r := range rateLimitWindowRoutes {
		m.rateLimitDown.WithLabelValues(r).Set(0)
	}
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
		"method":     boundValue(method, httpMethods),
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

// IncRateLimitStoreError counts one failed Postgres rate-limit window
// check; route is bounded to the limiter families {login,
// callback_ceiling, launch, other}. Only real store errors count —
// checks skipped while the circuit breaker is open never reach this
// counter, so a sustained outage costs ~one increment per 10 s
// cool-down per limiter, not one per request. Each failure falls back
// to the local per-replica limiter for that request (ADR 0006 fail-open).
func (m *Metrics) IncRateLimitStoreError(route string) {
	m.rateLimitStore.WithLabelValues(boundValue(route, rateLimitWindowRoutes)).Inc()
}

// SetRateLimitStoreDegraded reports a limiter family's circuit-breaker
// state: 1 while the breaker is open (Postgres checks skipped, the
// divided local limiter decides) and 0 once a probe closes it. route is
// bounded to {login, callback_ceiling, launch, other}.
func (m *Metrics) SetRateLimitStoreDegraded(route string, degraded bool) {
	v := 0.0
	if degraded {
		v = 1
	}
	m.rateLimitDown.WithLabelValues(boundValue(route, rateLimitWindowRoutes)).Set(v)
}

// IncFrameReload counts one session-frame re-navigation: a document load on
// the session host for a session whose lease already had a stream (the
// KasmVNC client's own websocket retries never touch it). dest is bounded
// to {iframe, document, other}.
func (m *Metrics) IncFrameReload(dest string) {
	m.frameReloads.WithLabelValues(boundValue(dest, frameReloadDests)).Inc()
}

// IncSessionRevocation counts one sign-out revocation of a portal session's
// session-bound leases/tickets (S17); result is bounded to {ok, error,
// other}.
func (m *Metrics) IncSessionRevocation(result string) {
	m.sessionRevokes.WithLabelValues(boundValue(result, sessionRevokeResults)).Inc()
}

// IncLeaseSessionMissing counts one lease revoked because its bound portal
// session no longer validates at renew/attach time (S17 defence-in-depth);
// reason is bounded to {absent, invalid, other}.
func (m *Metrics) IncLeaseSessionMissing(reason string) {
	m.leaseSessGone.WithLabelValues(boundValue(reason, leaseSessionMissingReasons)).Inc()
}
