package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/observability"
	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
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
// policy is the resolved session data-transfer vector (template policy ∧
// tenant ceiling, ADR 0010 §2): its clipboard feeds the ticket's
// clipboard_policy column so the gateway's redirect re-asserts the
// client's flags, and the whole vector is recorded verbatim on the ticket
// row. A zero-value policy records nothing.
type ConnectionIssuer interface {
	// IssueTicket mints a launch ticket. portalSessionID is the caller's
	// session id: the lease it produces records its digest so the stream's
	// owner tab is only ever reported back to this session (R-V3c).
	IssueTicket(ctx context.Context, p Principal, workspaceUID string, takeover bool, policy provisioning.SessionPolicy, portalSessionID string) (IssuedTicket, *Error)
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
	// clipboard resolves the workspace's session policy — template policy
	// ∧ tenant ceiling — so the ticket can record the resolved vector
	// (WithClipboardSource); nil records nothing.
	clipboard func(ctx context.Context, p Principal, workspaceUID string) (provisioning.SessionPolicy, error)
	maxBody   int64
	// audit is the dedicated audit-event sink the route emits through
	// (nil = no domain audit events).
	audit observability.AuditSink
	log   *slog.Logger
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
		log:     slog.Default(),
	}
}

// WithLogger attaches the logger the handler writes server-side detail to
// (e.g. the request-body decode detail that never reaches the client).
func (h *ConnectionHandler) WithLogger(l *slog.Logger) *ConnectionHandler {
	if l != nil {
		h.log = l
	}
	return h
}

// WithClipboardSource wires the resolver that supplies the workspace's
// resolved session policy for ticket recording (V3.24: the gateway
// redirect re-asserts the client flags from what the ticket recorded;
// ADR 0010: the recorded vector is the template policy ∧ tenant ceiling).
func (h *ConnectionHandler) WithClipboardSource(fn func(ctx context.Context, p Principal, workspaceUID string) (provisioning.SessionPolicy, error)) *ConnectionHandler {
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
	// SessionPolicy is the resolved policy the ticket recorded (the meet of
	// the template's policy and the tenant ceiling); the portal renders
	// the session iframe's allow from it — what the ticket carries, not
	// the template it happened to read earlier (ADR 0010 §2). Absent when
	// resolution failed — the ticket then records nothing and least
	// privilege applies.
	SessionPolicy *provisioning.SessionPolicy `json:"sessionPolicy,omitempty"`
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
		// SEC-I7: decode detail is logged server-side and never echoed —
		// the client gets the generic message every other handler uses.
		if err := decodeJSON(body, &req); err != nil {
			h.log.Warn("connection create: invalid request body",
				"request_id", RequestIDFromContext(r.Context()), "err", err)
			writeError(w, r, CodeInvalidRequest, "invalid request body")
			return
		}
	}
	// The resolved session policy rides the ticket: its redemption
	// redirect is the URL the session frame actually loads, so the gateway
	// — not the portal's iframe src — is what lands the client's clipboard
	// flags (V3.24). Resolution is best effort: a failure records nothing
	// and the ticket still issues (least privilege on the redirect).
	var policy provisioning.SessionPolicy
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
	var resolved *provisioning.SessionPolicy
	if policy.Clipboard != "" {
		resolved = &policy
	}
	_ = json.NewEncoder(w).Encode(launchTicketView{
		WorkspaceID:   tk.WorkspaceID,
		Ticket:        tk.Token,
		LaunchURL:     launchBase + LaunchPath,
		ExpiresAt:     tk.ExpiresAt,
		SessionPolicy: resolved,
	})
}
