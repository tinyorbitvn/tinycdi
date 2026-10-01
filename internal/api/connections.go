package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

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
type ConnectionIssuer interface {
	IssueTicket(ctx context.Context, p Principal, workspaceUID string, takeover bool) (IssuedTicket, *Error)
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
	maxBody int64
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

// MountConnectionRoutes registers the connections route with authn + CSRF.
func MountConnectionRoutes(mux *http.ServeMux, authn *Authenticator, h *ConnectionHandler) {
	mux.Handle("POST /v1/workspaces/{id}/connections",
		authn.RequireAuth(authn.RequireCSRF(http.HandlerFunc(h.Create))))
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
	tk, apiErr := h.issuer.IssueTicket(r.Context(), p, id, req.Takeover)
	if apiErr != nil {
		WriteError(w, RequestIDFromContext(r.Context()), apiErr)
		return
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
