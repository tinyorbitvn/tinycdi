package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/observability"
	"github.com/tinyorbitvn/tinycdi/internal/sessionhost"
)

// connections.go implements POST /v1/workspaces/{id}/connections
// (openapi.yaml createConnection): mint a 60 s single-use launch ticket plus
// the session-origin launch URL. Ownership and tenant checks are enforced by
// the broker against the workspace record — never by request input.

// IssuedTicket is the minted launch material handed to the response writer.
// Defined here (not in broker) so this package never imports internal/broker;
// the adapter lives in internal/broker/apishim.go.
type IssuedTicket struct {
	WorkspaceID string
	Token       string // opaque; response body only, never logged
	ExpiresAt   time.Time
}

// ConnectionIssuer is the ticket-issuing surface the handler needs. The
// adapter translates domain errors into *Error with the stable codes.
// clipboardPolicy is the workspace template's policy, recorded on the
// ticket so the gateway's redirect can re-assert the client's flags; ""
// records nothing.
type ConnectionIssuer interface {
	// IssueTicket mints a launch ticket. portalSessionID is the caller's
	// session id: the lease it produces records its digest so the stream's
	// owner tab is only ever reported back to this session (R-V3c).
	IssueTicket(ctx context.Context, p Principal, workspaceUID string, takeover bool, clipboardPolicy, portalSessionID string) (IssuedTicket, *Error)
}

// LaunchPath is the session-origin endpoint the browser POSTs the ticket to
// (mirrors gateway.LaunchPath; duplicated to keep this package free of a
// broker/gateway import).
const LaunchPath = "/v1/launch"

// ConnectionHandler implements POST /v1/workspaces/{id}/connections.
type ConnectionHandler struct {
	issuer  ConnectionIssuer
	tenants TenantResolver
	domain  sessionhost.Domain
	// clipboard resolves the workspace's template clipboard policy so the
	// ticket can record it (WithClipboardSource); nil records nothing.
	clipboard func(ctx context.Context, p Principal, workspaceUID string) (string, error)
	maxBody   int64
	// audit is the dedicated audit-event sink the route emits through
	// (nil = no domain audit events).
	audit observability.AuditSink
}

// NewConnectionHandler wires the handler. domain is the session domain the
// per-workspace launch URL is built under ("ws-<suffix>.<domain>[:port]");
// issuer and tenants are required.
func NewConnectionHandler(issuer ConnectionIssuer, tenants TenantResolver, domain sessionhost.Domain) *ConnectionHandler {
	return &ConnectionHandler{
		issuer:  issuer,
		tenants: tenants,
		domain:  domain,
		maxBody: 16 << 10,
	}
}

// WithClipboardSource wires the resolver that supplies the workspace's
// template clipboard policy for ticket recording (V3.24: the gateway
// redirect re-asserts the client flags from what the ticket recorded).
func (h *ConnectionHandler) WithClipboardSource(fn func(ctx context.Context, p Principal, workspaceUID string) (string, error)) *ConnectionHandler {
	h.clipboard = fn
	return h
}

// WithAuditSink attaches the audit sink the ticket-issue route writes its
// dedicated audit event to. The minted ticket is a bearer credential and
// is never recorded — only the action, actor, target and outcome.
func (h *ConnectionHandler) WithAuditSink(s observability.AuditSink) *ConnectionHandler {
	h.audit = s
	return h
}

// MountConnectionRoutes registers the connections route with authn + CSRF
// inside the dedicated audit wrapper (see MountWorkspaceRoutes).
func MountConnectionRoutes(mux *http.ServeMux, authn *Authenticator, h *ConnectionHandler) {
	mux.Handle(routeConnectionCreate,
		authn.RequireAuth(audited(h.audit, routeConnectionCreate,
			authn.RequireCSRF(http.HandlerFunc(h.Create)))))
}

type createConnectionRequest struct {
	Takeover bool `json:"takeover"`
}

// launchTicketView matches the LaunchTicket schema in openapi.yaml.
type launchTicketView struct {
	WorkspaceID string    `json:"workspaceId"`
	Ticket      string    `json:"ticket"`
	LaunchURL   string    `json:"launchUrl"`
	ExpiresAt   time.Time `json:"expiresAt"`
}

// Create handles POST /v1/workspaces/{id}/connections.
func (h *ConnectionHandler) Create(w http.ResponseWriter, r *http.Request) {
	p, ok := PrincipalFromContext(r.Context())
	if !ok {
		writeError(w, r, CodeUnauthenticated, "authentication required")
		return
	}
	if _, ok := h.tenants.Namespace(p.TenantID); !ok {
		writeError(w, r, CodeForbidden, "tenant is not provisioned")
		return
	}
	id := r.PathValue("id")
	// The launch URL is per-workspace: the session listener serves each
	// workspace on its own host under the session domain (D9). An ID that
	// cannot map to a session host is INVALID_STATE — it can never launch.
	launchBase, err := h.domain.Origin(id)
	if err != nil {
		writeError(w, r, CodeInvalidState,
			"workspace id must have the form ws_<8-60 lowercase alnum suffix>")
		return
	}
	var req createConnectionRequest
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, h.maxBody))
	if err != nil {
		writeError(w, r, CodeInvalidRequest, "unreadable or oversized body")
		return
	}
	if len(bytes.TrimSpace(body)) > 0 {
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			writeError(w, r, CodeInvalidRequest, "invalid request body: "+err.Error())
			return
		}
	}
	// The template's clipboard policy rides the ticket: its redemption
	// redirect is the URL the session frame actually loads, so the gateway
	// — not the portal's iframe src — is what lands the client's clipboard
	// flags (V3.24). Resolution is best effort: a failure records nothing
	// and the ticket still issues (least privilege on the redirect).
	var policy string
	if h.clipboard != nil {
		policy, _ = h.clipboard(r.Context(), p, id)
	}
	var sessionID string
	if sess, ok := SessionFromContext(r.Context()); ok && sess != nil {
		sessionID = sess.ID
	}
	tk, apiErr := h.issuer.IssueTicket(r.Context(), p, id, req.Takeover, policy, sessionID)
	if apiErr != nil {
		WriteError(w, r, apiErr)
		return
	}
	if req.Takeover {
		auditSetDetail(r.Context(), "takeover", "true")
	}
	// The response carries a bearer-equivalent ticket: it must never be
	// stored by a shared/private cache (SEC-I7).
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(launchTicketView{
		WorkspaceID: tk.WorkspaceID,
		Ticket:      tk.Token,
		LaunchURL:   launchBase + LaunchPath,
		ExpiresAt:   tk.ExpiresAt,
	})
}
