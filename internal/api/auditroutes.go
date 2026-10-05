// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package api

import (
	"context"
	"net/http"

	"github.com/tinyorbitvn/tinycdi/internal/observability"
)

// Domain audit actions emitted to the AuditSink next to the per-request
// "http.request" record. Every mutating route and every /v1/admin/ route in
// openapi.yaml emits exactly one dedicated event carrying the verified
// actor, the target, the outcome and the request id — admin operations
// (quota writes, cross-owner workspace and retained-data actions) must be
// reconstructable from the audit stream alone (threat model: audit-trail
// completeness). The table test in auditroutes_test.go fails when a new
// mutating or admin route is added to the spec without an entry here.
const (
	auditActionSessionLogout    = "session.logout"
	auditActionSessionRevokeAll = "session.revoke_all"
	auditActionWorkspaceCreate  = "workspace.create"
	auditActionWorkspaceStart   = "workspace.start"
	auditActionWorkspaceStop    = "workspace.stop"
	auditActionWorkspaceDelete  = "workspace.delete"
	auditActionConnectionCreate = "connection.create"
	auditActionDataAttach       = "data.attach"
	auditActionDataPurge        = "data.purge"
	auditActionAdminQuotaGet    = "admin.quota.get"
	auditActionAdminQuotaSet    = "admin.quota.set"

	auditActionAdminUserLimitGet          = "admin.user_limit.get"
	auditActionAdminUserLimitSet          = "admin.user_limit.set"
	auditActionAdminUserLimitClear        = "admin.user_limit.clear"
	auditActionAdminUserLimitDefaultSet   = "admin.user_limit.default.set"
	auditActionAdminUserLimitDefaultClear = "admin.user_limit.default.clear"
)

// Audited route patterns — shared between mux.Handle and auditedRoutes so a
// pattern can never drift from its action.
const (
	routeLogout           = "POST /v1/logout"
	routeSessionRevokeAll = "POST /v1/me/sessions:revoke-all"
	routeWorkspaceCreate  = "POST /v1/workspaces"
	routeWorkspaceDelete  = "DELETE /v1/workspaces/{id}"
	routeWorkspaceStart   = "POST /v1/workspaces/{id}/start"
	routeWorkspaceStop    = "POST /v1/workspaces/{id}/stop"
	routeConnectionCreate = "POST /v1/workspaces/{id}/connections"
	routeDataAttach       = "POST /v1/data/{dataId}/attach"
	routeDataPurge        = "POST /v1/data/{dataId}/purge"
	routeAdminQuotaGet    = "GET /v1/admin/tenants/{tenant}/quota"
	routeAdminQuotaSet    = "PUT /v1/admin/tenants/{tenant}/quota"

	routeAdminUserLimitsGet    = "GET /v1/admin/tenants/{tenant}/user-limits"
	routeAdminUserLimitsPut    = "PUT /v1/admin/tenants/{tenant}/user-limits"
	routeAdminUserLimitDefault = "PUT /v1/admin/tenants/{tenant}/user-limits/default"
)

// auditedRoute binds a route pattern to its audit action and the path-value
// key that names the target; "" means the handler fills the target on the
// request-scoped routeAudit cell once it is known (e.g. a created id).
type auditedRoute struct {
	action    string
	targetKey string
}

// auditedRoutes is the single source of truth for which app routes emit a
// dedicated audit event: every mutating (non-GET) route plus the admin read
// under /v1/admin/.
var auditedRoutes = map[string]auditedRoute{
	routeLogout:           {auditActionSessionLogout, ""},
	routeSessionRevokeAll: {auditActionSessionRevokeAll, ""},
	routeWorkspaceCreate:  {auditActionWorkspaceCreate, ""},
	routeWorkspaceDelete:  {auditActionWorkspaceDelete, "id"},
	routeWorkspaceStart:   {auditActionWorkspaceStart, "id"},
	routeWorkspaceStop:    {auditActionWorkspaceStop, "id"},
	routeConnectionCreate: {auditActionConnectionCreate, "id"},
	routeDataAttach:       {auditActionDataAttach, "dataId"},
	routeDataPurge:        {auditActionDataPurge, "dataId"},
	routeAdminQuotaGet:    {auditActionAdminQuotaGet, "tenant"},
	routeAdminQuotaSet:    {auditActionAdminQuotaSet, "tenant"},
	// The PUTs carry both halves of a set/clear pair: the table action is
	// the attempt (what a pre-decode denial is audited as) and the handler
	// replaces it with the clear variant via auditSetAction once the body
	// decodes.
	routeAdminUserLimitsGet:    {auditActionAdminUserLimitGet, "tenant"},
	routeAdminUserLimitsPut:    {auditActionAdminUserLimitSet, "tenant"},
	routeAdminUserLimitDefault: {auditActionAdminUserLimitDefaultSet, "tenant"},
}

// routeAudit is the request-scoped record the audited wrapper places in the
// context; WriteError and handlers fill errCode, target and details while
// the request runs, and the wrapper emits the completed event after the
// handler returns (same shared-cell convention as auditCollector).
type routeAudit struct {
	target  string
	action  string
	errCode string
	details map[string]string
}

func routeAuditFromContext(ctx context.Context) *routeAudit {
	ra, _ := ctx.Value(ctxKeyRouteAudit).(*routeAudit)
	return ra
}

// auditSetTarget records the target UID on the in-flight route audit (e.g.
// the id of the workspace a create just made). No-op without the wrapper.
func auditSetTarget(ctx context.Context, target string) {
	if ra := routeAuditFromContext(ctx); ra != nil {
		ra.target = target
	}
}

// auditSetAction replaces the audited-route table action on the in-flight
// record — for routes whose single method covers a set/clear pair (the
// resolved action is known only after the body decodes; requests denied
// earlier keep the table action). No-op without the wrapper.
func auditSetAction(ctx context.Context, action string) {
	if ra := routeAuditFromContext(ctx); ra != nil {
		ra.action = action
	}
}

// auditSetDetail records a non-secret detail key on the in-flight route
// audit; sensitive keys are redacted by the sink on write regardless.
func auditSetDetail(ctx context.Context, key, value string) {
	if ra := routeAuditFromContext(ctx); ra != nil {
		if ra.details == nil {
			ra.details = map[string]string{}
		}
		ra.details[key] = value
	}
}

// audited wraps the handler of a route in auditedRoutes so one audit event
// is emitted per request: actor is the verified principal's pseudonymous
// ref ("anonymous" when the request never authenticated), outcome is
// derived from the response status (401/403 denied, other 4xx/5xx failure),
// and the errorCode comes from the stable error model via WriteError.
// Requests denied before RequireAuth never reach the wrapper — they are
// covered by the "http.request" record. Wire inside RequireAuth so the
// principal is in context; a nil sink passes requests through unrecorded.
// The emit is deferred so a handler panic still produces its event
// (failure/"panic"): recover() is deliberately not called, the panic keeps
// unwinding to net/http's per-connection recovery unchanged.
// An unregistered pattern panics at mount time: every audited route must
// have a table entry (the coverage test enforces the reverse direction).
func audited(sink observability.AuditSink, pattern string, next http.Handler) http.Handler {
	rt, ok := auditedRoutes[pattern]
	if !ok {
		panic("audited: route " + pattern + " missing from auditedRoutes")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if sink == nil {
			next.ServeHTTP(w, r)
			return
		}
		ra := &routeAudit{}
		if rt.targetKey != "" {
			ra.target = r.PathValue(rt.targetKey)
		}
		r = r.WithContext(context.WithValue(r.Context(), ctxKeyRouteAudit, ra))
		rec := &statusRecorder{ResponseWriter: w}
		completed := false
		defer func() {
			var actor, tenant string
			if p, ok := PrincipalFromContext(r.Context()); ok {
				actor = observability.ActorRef(p.Issuer, p.Subject)
				tenant = p.TenantID
				if p.InGroup(TenantAdminGroup) {
					// Mark the elevated role explicitly: on shared endpoints the
					// record must distinguish an admin acting tenant-wide from
					// an owner acting on their own resource.
					auditSetDetail(r.Context(), "role", TenantAdminGroup)
				}
			}
			outcome := outcomeFor(statusOrOK(rec.status))
			errCode := ra.errCode
			if !completed {
				outcome = observability.OutcomeFailure
				if errCode == "" {
					errCode = "panic"
				}
			}
			action := rt.action
			if ra.action != "" {
				action = ra.action
			}
			_ = sink.WriteAudit(r.Context(), observability.AuditEvent{
				Actor:     actorOrAnonymous(actor),
				Action:    action,
				TargetUID: ra.target,
				Tenant:    tenant,
				RequestID: RequestIDFromContext(r.Context()),
				Outcome:   outcome,
				ErrorCode: errCode,
				Details:   ra.details,
			})
		}()
		next.ServeHTTP(rec, r)
		completed = true
	})
}

// MountLogoutRoutes registers POST /v1/logout audited with the
// authenticator's own sink (logout also emits the dedicated session.revoke
// record for the lease-material revocation).
func MountLogoutRoute(mux *http.ServeMux, authn *Authenticator) {
	mux.Handle(routeLogout, authn.RequireAuth(
		audited(authn.auditSink, routeLogout,
			authn.RequireCSRF(http.HandlerFunc(authn.LogoutHandler)))))
}

// lateAuditSink resolves the inner sink at write time: mounts capture the
// wrapper (and the auditedRoutes table entry) once, while the authenticator
// may have its sink attached any time before the first request — the same
// late binding the handlers' own audit writes use.
type lateAuditSink struct {
	get func() observability.AuditSink
}

// WriteAudit forwards to the currently attached sink, or drops the event
// when none is wired — matching audited()'s nil-sink pass-through.
func (s lateAuditSink) WriteAudit(ctx context.Context, e observability.AuditEvent) error {
	if inner := s.get(); inner != nil {
		return inner.WriteAudit(ctx, e)
	}
	return nil
}

// MountRevokeAllRoute registers POST /v1/me/sessions:revoke-all audited
// with the authenticator's sink — the handler records the per-kind revoke
// counts on the in-flight event via auditSetDetail, so exactly one
// session.revoke_all record is emitted per call (ADR 0007). The optional
// middleware wraps the whole chain (the production mount applies the
// session-digest-keyed login-family limiter: the op is self-scoped and
// idempotent, the limit only bounds repeat DB churn).
func MountRevokeAllRoute(mux *http.ServeMux, authn *Authenticator, wrap ...func(http.Handler) http.Handler) {
	var h http.Handler = authn.RequireAuth(
		audited(lateAuditSink{func() observability.AuditSink { return authn.auditSink }}, routeSessionRevokeAll,
			authn.RequireCSRF(http.HandlerFunc(authn.RevokeAllSessionsHandler))))
	for _, w := range wrap {
		h = w(h)
	}
	mux.Handle(routeSessionRevokeAll, h)
}
