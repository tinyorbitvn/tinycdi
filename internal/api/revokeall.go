// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package api

// revokeall.go implements POST /v1/me/sessions:revoke-all — "sign out
// everywhere" (ADR 0007, threat-model S17's principal-scoped sibling): one
// store transaction destroys every portal session the caller's principal
// holds in the caller's tenant — the calling session included — together
// with the session-layer material minted under them (active connection
// leases and outstanding launch tickets).

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/tinyorbitvn/tinycdi/internal/observability"
)

// RevokeAllResult counts what a principal-scoped revocation destroyed —
// the session.revoke_all audit record carries these numbers. Never used
// for control flow: a re-run legitimately reports all zero.
type RevokeAllResult struct {
	Sessions int
	Tickets  int
	Leases   int
}

// PrincipalRevoker destroys the session-layer material of EVERY portal
// session a principal holds in one tenant — broker.PublicRevoker in the
// wired backend. Revocation is the whole point of the endpoint, so unlike
// SessionRevoker a nil revoker does not degrade the route to a no-op: the
// handler refuses instead of claiming a sign-out that never happened.
type PrincipalRevoker interface {
	// RevokePrincipalSessions deletes the principal's session rows in
	// tenantID and revokes their leases and outstanding tickets in one
	// transaction, returning the per-kind counts.
	RevokePrincipalSessions(ctx context.Context, tenantID, issuer, subject string) (RevokeAllResult, error)
}

// WithPrincipalRevoker attaches the store-level revoker sign-out-everywhere
// calls (ADR 0007) — broker.PublicRevoker in the wired backend. Nil leaves
// the endpoint answering 503.
func (a *Authenticator) WithPrincipalRevoker(r PrincipalRevoker) *Authenticator {
	a.principalRevoker = r
	return a
}

// RevokeAllSessionsHandler destroys every portal session of the caller's
// principal in the caller's tenant, including the calling session — "sign
// out everywhere" keeps no opt-out: the action exists for the "a session
// may be compromised" case, where silently keeping this browser signed in
// is the surprising, weaker semantics. Route behind RequireAuth +
// RequireCSRF (and the session-keyed rate limit in production).
//
// Unlike logout the revoke is not best-effort past the local destroy: the
// transaction IS the operation. A store failure rolls everything back —
// the caller stays signed in, gets 500 and can retry; a "cookie cleared
// but the other sessions still live" answer would claim a sign-out that
// never happened. On success the portal cookie expires and the response
// mirrors logout: 204, or 200 {"endSessionUrl"} when the provider
// advertises end_session_endpoint so the browser can end the provider
// session too (the caller's retained ID token supplies id_token_hint).
func (a *Authenticator) RevokeAllSessionsHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sess, ok := SessionFromContext(ctx)
	if !ok || sess == nil {
		writeError(w, r, CodeUnauthenticated, "authentication required")
		return
	}
	if a.principalRevoker == nil {
		writeError(w, r, CodeUnavailable, "sign-out-everywhere unavailable")
		return
	}
	p := sess.Principal
	// Detached from the request's cancellation, bounded like the
	// per-session revoke: a client that disconnects mid-call must not abort
	// a transaction the user already committed to.
	revCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sessionRevokeTimeout)
	res, err := a.principalRevoker.RevokePrincipalSessions(revCtx, p.TenantID, p.Issuer, p.Subject)
	cancel()
	actor := observability.ActorRef(p.Issuer, p.Subject)
	if err != nil {
		a.log.Warn("sign-out-everywhere: principal revocation failed",
			"request_id", RequestIDFromContext(ctx),
			"actor", actor, "err", err)
		if a.metrics != nil {
			a.metrics.IncSessionRevokeAll("error")
		}
		a.writeRevokeAllAudit(r, actor, p.TenantID, observability.OutcomeFailure, "revoke_failed", RevokeAllResult{})
		writeError(w, r, CodeInternal, "could not sign out everywhere")
		return
	}
	if a.metrics != nil {
		a.metrics.IncSessionRevokeAll("ok")
	}
	a.writeRevokeAllAudit(r, actor, p.TenantID, observability.OutcomeSuccess, "", res)

	http.SetCookie(w, a.sessionCookie("", -1))
	if end := a.endSessionURL(sess.IDToken); end != "" {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(LogoutResult{EndSessionURL: end})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeRevokeAllAudit emits the session.revoke_all audit record — the
// principal-scoped sibling of session.revoke — with the per-kind counts on
// success. Session material never reaches the record (Detail keys still
// pass through observability.RedactDetails on write).
func (a *Authenticator) writeRevokeAllAudit(r *http.Request, actor, tenant string, outcome observability.AuditOutcome, errCode string, res RevokeAllResult) {
	if a.auditSink == nil {
		return
	}
	var details map[string]string
	if outcome == observability.OutcomeSuccess {
		// Detail keys naming sessions/tickets would be redacted on write
		// (observability.sensitiveFieldRe matches the substring), so the
		// per-kind counts travel inside one neutral key's value.
		details = map[string]string{
			"counts": "sessions=" + strconv.Itoa(res.Sessions) +
				",tickets=" + strconv.Itoa(res.Tickets) +
				",leases=" + strconv.Itoa(res.Leases),
		}
	}
	_ = a.auditSink.WriteAudit(r.Context(), observability.AuditEvent{
		Actor:     actorOrAnonymous(actor),
		Action:    "session.revoke_all",
		Tenant:    tenant,
		RequestID: RequestIDFromContext(r.Context()),
		Outcome:   outcome,
		ErrorCode: errCode,
		Details:   details,
	})
}
