// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/observability"
	"github.com/tinyorbitvn/tinycdi/internal/store"
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
	auditActionSessionTouch     = "session.touch"
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
	auditActionAdminUserLimitDefaultGet   = "admin.user_limit.default.get"
	auditActionAdminUserLimitDefaultSet   = "admin.user_limit.default.set"
	auditActionAdminUserLimitDefaultClear = "admin.user_limit.default.clear"

	auditActionAdminSessionPolicyGet   = "admin.session_policy.get"
	auditActionAdminSessionPolicySet   = "admin.session_policy.set"
	auditActionAdminSessionPolicyClear = "admin.session_policy.clear"
)

// Audited route patterns — shared between mux.Handle and auditedRoutes so a
// pattern can never drift from its action.
const (
	routeLogout           = "POST /v1/logout"
	routeSessionTouch     = "POST /v1/session:touch"
	routeSessionRevokeAll = "POST /v1/me/sessions:revoke-all"
	routeWorkspaceCreate  = "POST /v1/workspaces"
	routeWorkspaceDelete  = "DELETE /v1/workspaces/{id}"
	routeConnectionCreate = "POST /v1/workspaces/{id}/connections"
	routeAdminQuotaGet    = "GET /v1/admin/tenants/{tenant}/quota"
	routeAdminQuotaSet    = "PUT /v1/admin/tenants/{tenant}/quota"

	routeAdminUserLimitsGet       = "GET /v1/admin/tenants/{tenant}/user-limits"
	routeAdminUserLimitPut        = "PUT /v1/admin/tenants/{tenant}/user-limits/{ownerRef}"
	routeAdminUserLimitDelete     = "DELETE /v1/admin/tenants/{tenant}/user-limits/{ownerRef}"
	routeAdminUserLimitDefaultGet = "GET /v1/admin/tenants/{tenant}/user-limits/default"
	routeAdminUserLimitDefault    = "PUT /v1/admin/tenants/{tenant}/user-limits/default"

	routeAdminSessionPolicyGet = "GET /v1/admin/tenants/{tenant}/session-policy"
	routeAdminSessionPolicyPut = "PUT /v1/admin/tenants/{tenant}/session-policy"
)

// :verb custom methods. ServeMux cannot match the verb labels (the verb
// shares the id's segment), so they are audit labels only: the mux pattern
// is the ActionRouter's routeWorkspaceActions / routeDataActions, and each
// verb handler is wrapped in audited() under its label.
const (
	routeWorkspaceActions = "POST /v1/workspaces/{ref}"
	routeWorkspaceStart   = "POST /v1/workspaces/{ref}:start"
	routeWorkspaceStop    = "POST /v1/workspaces/{ref}:stop"
	routeDataActions      = "POST /v1/data/{ref}"
	routeDataAttach       = "POST /v1/data/{ref}:attach"
	routeDataPurge        = "POST /v1/data/{ref}:purge"
)

// actionTarget is the auditedRoute.targetKey of the :verb routes: the
// target is the id the ActionRouter split off, not the raw {ref}.
const actionTarget = "ref"

// auditPersistTimeout bounds one audit_event insert or amend; it runs on a
// context detached from the client, so a stalled store cannot hold the
// handler goroutine forever.
const auditPersistTimeout = 5 * time.Second

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
	routeSessionTouch:     {auditActionSessionTouch, ""},
	routeSessionRevokeAll: {auditActionSessionRevokeAll, ""},
	routeWorkspaceCreate:  {auditActionWorkspaceCreate, ""},
	routeWorkspaceDelete:  {auditActionWorkspaceDelete, "id"},
	routeWorkspaceStart:   {auditActionWorkspaceStart, actionTarget},
	routeWorkspaceStop:    {auditActionWorkspaceStop, actionTarget},
	routeConnectionCreate: {auditActionConnectionCreate, "id"},
	routeDataAttach:       {auditActionDataAttach, actionTarget},
	routeDataPurge:        {auditActionDataPurge, actionTarget},
	routeAdminQuotaGet:    {auditActionAdminQuotaGet, "tenant"},
	routeAdminQuotaSet:    {auditActionAdminQuotaSet, "tenant"},
	// The default PUT carries both halves of a set/clear pair: the table
	// action is the attempt (what a pre-decode denial is audited as) and
	// the handler replaces it with the clear variant via auditSetAction
	// once a null limit decodes. An override is cleared by its DELETE.
	routeAdminUserLimitsGet:       {auditActionAdminUserLimitGet, "tenant"},
	routeAdminUserLimitPut:        {auditActionAdminUserLimitSet, "tenant"},
	routeAdminUserLimitDelete:     {auditActionAdminUserLimitClear, "tenant"},
	routeAdminUserLimitDefaultGet: {auditActionAdminUserLimitDefaultGet, "tenant"},
	routeAdminUserLimitDefault:    {auditActionAdminUserLimitDefaultSet, "tenant"},
	// As on the user-limit PUTs, the table action is the set attempt; an
	// all-FollowTemplate body resolves to the clear variant via
	// auditSetAction once it decodes.
	routeAdminSessionPolicyGet: {auditActionAdminSessionPolicyGet, "tenant"},
	routeAdminSessionPolicyPut: {auditActionAdminSessionPolicySet, "tenant"},
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
// With a durable sink (DurableAuditSink) the event is also persisted: inside
// the mutation's transaction through a store.TxHook, else before the
// buffered response is released; a failed insert answers 500 INTERNAL and
// nothing the handler wrote reaches the client (ADR 0011). A sink without
// persistence (and session.touch) keeps the pass-through status recorder.
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
		switch rt.targetKey {
		case "":
		case actionTarget:
			ra.target = ActionID(r)
		default:
			ra.target = r.PathValue(rt.targetKey)
		}
		ctx := context.WithValue(r.Context(), ctxKeyRouteAudit, ra)
		durable, _ := sink.(durableAudit)
		if durable != nil && !durable.Persists(rt.action) {
			durable = nil
		}
		event := func(status int, completed bool) observability.AuditEvent {
			var actor, tenant string
			if p, ok := PrincipalFromContext(ctx); ok {
				actor = observability.ActorRef(p.Issuer, p.Subject)
				tenant = p.TenantID
				if p.IsTenantAdmin() {
					auditSetDetail(ctx, "role", TenantAdminGroup)
				}
			}
			outcome := outcomeFor(statusOrOK(status))
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
			return observability.AuditEvent{
				Actor: actorOrAnonymous(actor), Action: action, TargetUID: ra.target, Tenant: tenant,
				RequestID: RequestIDFromContext(ctx), Outcome: outcome, ErrorCode: errCode, Details: ra.details,
			}
		}
		var hook *auditTxHook // nil for reads and non-persisted routes
		if durable != nil && r.Method != http.MethodGet {
			hook = &auditTxHook{sink: durable, build: func() observability.AuditEvent { return event(http.StatusOK, true) }}
			ctx = store.ContextWithTxHook(ctx, hook)
		}
		r = r.WithContext(ctx)

		// persist stores or amends the row for the final event e. It runs on a
		// context detached from the client connection: a hung-up client must
		// not turn a committed mutation into a lost or stale audit row.
		persist := func(e observability.AuditEvent) error {
			pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), auditPersistTimeout)
			defer cancel()
			if id := hook.committedID(); id != 0 {
				// The row committed with the mutation; the amend records the
				// final target and outcome. A failed amend leaves the
				// committed row as inserted.
				_ = durable.AmendAudit(pctx, id, e)
				return nil
			}
			_, err := durable.PersistAudit(pctx, nil, e)
			return err
		}

		var rw http.ResponseWriter
		var buf *bufferedResponse
		var rec *statusRecorder
		if durable != nil {
			buf = newBufferedResponse(w)
			rw = buf
		} else {
			rec = &statusRecorder{ResponseWriter: w}
			rw = rec
		}
		completed := false
		defer func() {
			if completed {
				return
			}
			e := event(0, false)
			if durable != nil {
				_ = persist(e)
			}
			_ = sink.WriteAudit(ctx, e)
		}()
		next.ServeHTTP(rw, r)
		completed = true
		var status int
		if buf != nil {
			status = buf.status
		} else {
			status = rec.status
		}
		e := event(status, true)
		var persistErr error
		if durable != nil {
			persistErr = persist(e)
			if persistErr != nil {
				e.Outcome, e.ErrorCode = observability.OutcomeFailure, string(CodeInternal)
			}
		}
		_ = sink.WriteAudit(ctx, e) // stdout is written even when the store is down
		if persistErr != nil {
			buf.discardForError()
			WriteError(w, r, NewError(CodeInternal, "audit record could not be stored"))
			return
		}
		if buf != nil {
			buf.flush()
		}
	})
}

// MountLogoutRoutes registers POST /v1/logout audited with the
// authenticator's own sink (logout also emits the dedicated session.revoke
// record for the lease-material revocation).
func MountLogoutRoute(mux RouteMux, authn *Authenticator) {
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

func (s lateAuditSink) durable() durableAudit {
	d, _ := s.get().(durableAudit)
	return d
}

func (s lateAuditSink) Persists(action string) bool {
	d := s.durable()
	return d != nil && d.Persists(action)
}

func (s lateAuditSink) PersistAudit(ctx context.Context, tx store.Tx, e observability.AuditEvent) (int64, error) {
	d := s.durable()
	if d == nil {
		return 0, errors.New("audit: no durable sink attached")
	}
	return d.PersistAudit(ctx, tx, e)
}

func (s lateAuditSink) AmendAudit(ctx context.Context, id int64, e observability.AuditEvent) error {
	d := s.durable()
	if d == nil {
		return errors.New("audit: no durable sink attached")
	}
	return d.AmendAudit(ctx, id, e)
}

// MountRevokeAllRoute registers POST /v1/me/sessions:revoke-all audited
// with the authenticator's sink — the handler records the per-kind revoke
// counts on the in-flight event via auditSetDetail, so exactly one
// session.revoke_all record is emitted per call (ADR 0007). The optional
// middleware wraps the whole chain (the production mount applies the
// session-digest-keyed login-family limiter: the op is self-scoped and
// idempotent, the limit only bounds repeat DB churn).
func MountRevokeAllRoute(mux RouteMux, authn *Authenticator, wrap ...func(http.Handler) http.Handler) {
	var h http.Handler = authn.RequireAuth(
		audited(lateAuditSink{func() observability.AuditSink { return authn.auditSink }}, routeSessionRevokeAll,
			authn.RequireCSRF(http.HandlerFunc(authn.RevokeAllSessionsHandler))))
	for _, w := range wrap {
		h = w(h)
	}
	mux.Handle(routeSessionRevokeAll, h)
}
