// Package httpapi is the internal mTLS gateway↔broker API (fixed contract,
// ADR 0003). It serves only the broker surface the session gateways need —
// ticket redemption, lease renewal, target resolution, revocation — on a
// dedicated listener separate from the public API mux.
//
// Identity: the caller is authenticated by its verified client certificate.
// The Subject CN is the gateway ID; an optional URI SAN of the form
// spiffe://cdi.tinyorbit.vn/gateway/<id> must equal the CN when present. A
// request without a verified client certificate is answered 401 with the
// standard error model (api.WriteError). Ticket/lease material and runtime
// credentials are never logged.
package httpapi

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/tinyorbitvn/tinycdi/internal/api"
	"github.com/tinyorbitvn/tinycdi/internal/broker"
)

// SPIFFE trust-domain prefix for gateway identities.
const spiffePrefix = "spiffe://cdi.tinyorbit.vn/gateway/"

// codeRevoked is the internal-only error code paired with HTTP 410 for dead
// leases (revoked/superseded/expired). The public ErrorCode enum has no 410
// mapping; the body still uses the standard api.Error JSON shape.
const codeRevoked = api.ErrorCode("REVOKED")

// DefaultOperatorCN is the client-certificate CN identifying the operator
// on the workspace-scoped endpoints (revoke/drain). Those routes refuse any
// other identity, and the gateway-scoped routes refuse the operator CN —
// the two client populations are mutually exclusive.
const DefaultOperatorCN = "operator"

// BrokerAPI is the broker surface the internal API exposes.
type BrokerAPI interface {
	RedeemTicket(ctx context.Context, gw broker.GatewayIdentity, opaque string) (broker.Lease, error)
	RenewLease(ctx context.Context, gw broker.GatewayIdentity, leaseID string, fence broker.Fence) (broker.Lease, error)
	ResolveTarget(ctx context.Context, gw broker.GatewayIdentity, leaseID string) (broker.Target, error)
	RevokeLease(ctx context.Context, leaseID string) error
	// ReportActivity records a gateway-observed session signal (input /
	// connected / disconnect); the server stamps the receipt time.
	ReportActivity(ctx context.Context, gw broker.GatewayIdentity, leaseID string, fence broker.Fence, ev broker.ActivityEvent) error
	// RevokeWorkspaceLeases revokes every live lease of the workspace bound
	// to a runtime generation <= runtimeGeneration and blocks new
	// tickets/redeems for covered generations. Operator-only route.
	RevokeWorkspaceLeases(ctx context.Context, workspaceUID broker.PlatformID, runtimeGeneration uint64) (int, error)
	// DrainStatus reports gateway-reported open stream count for the
	// workspace. Operator-only route.
	DrainStatus(ctx context.Context, workspaceUID broker.PlatformID) (openStreams int, drained bool, err error)
}

// Config wires the internal API handler.
type Config struct {
	// Broker is required.
	Broker BrokerAPI
	// Audience is the session-gateway audience all verified client certs map
	// to (tickets bind to it at issuance). Required.
	Audience string
	// OperatorCN is the client-certificate CN of the operator identity
	// allowed on /workspaces/* routes (revoke/drain). Empty defaults to
	// DefaultOperatorCN.
	OperatorCN string
	// Logger receives request logs; nil means silent.
	Logger *slog.Logger
}

type ctxKey int

const ctxKeyGateway ctxKey = iota

// NewHandler builds the internal API handler. It must be served over TLS
// with tls.Config.ClientCAs set to the gateway trust bundle and ClientAuth
// VerifyClientCertIfGiven (the middleware turns an absent cert into a 401
// instead of a handshake abort; an untrusted cert still fails the handshake).
func NewHandler(cfg Config) http.Handler {
	if cfg.OperatorCN == "" {
		cfg.OperatorCN = DefaultOperatorCN
	}
	s := &server{cfg: cfg}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /internal/v1/broker/redeem", s.redeem)
	mux.HandleFunc("POST /internal/v1/broker/leases/{id}/renew", s.renew)
	mux.HandleFunc("GET /internal/v1/broker/leases/{id}/target", s.target)
	mux.HandleFunc("POST /internal/v1/broker/leases/{id}/revoke", s.revoke)
	mux.HandleFunc("POST /internal/v1/broker/leases/{id}/activity", s.activity)
	mux.HandleFunc("POST /internal/v1/broker/workspaces/{uid}/revoke", s.revokeWorkspace)
	mux.HandleFunc("GET /internal/v1/broker/workspaces/{uid}/drain", s.drainWorkspace)
	return api.RequestID(s.identify(s.logRequests(mux)))
}

type server struct{ cfg Config }

// identify maps the verified peer certificate onto broker.GatewayIdentity.
func (s *server) identify(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
			writeErr(w, r, http.StatusUnauthorized, api.CodeUnauthenticated, "client certificate required")
			return
		}
		cert := r.TLS.PeerCertificates[0]
		id := cert.Subject.CommonName
		if id == "" {
			writeErr(w, r, http.StatusUnauthorized, api.CodeUnauthenticated, "client certificate has no gateway CN")
			return
		}
		for _, u := range cert.URIs {
			if strings.HasPrefix(u.String(), spiffePrefix) &&
				strings.TrimPrefix(u.String(), spiffePrefix) != id {
				writeErr(w, r, http.StatusForbidden, api.CodeForbidden, "SPIFFE id does not match certificate CN")
				return
			}
		}
		gw := broker.GatewayIdentity{ID: id, Audience: s.cfg.Audience}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKeyGateway, gw)))
	})
}

func gatewayFrom(ctx context.Context) broker.GatewayIdentity {
	gw, _ := ctx.Value(ctxKeyGateway).(broker.GatewayIdentity)
	return gw
}

// logRequests emits a minimal per-request record: method, matched route,
// status, request id, gateway id. Bodies, tickets, cookies and credentials
// are never logged. SEC-41: the raw request path is never logged either —
// it embeds the lease id, which is bearer material on this listener (id +
// any gateway-CN cert resolves runtime credentials). The matched route
// pattern is logged instead, plus a truncated sha256 of the lease id so
// requests stay correlatable.
func (s *server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.Logger == nil {
			next.ServeHTTP(w, r)
			return
		}
		rec := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		path := "-"
		if p := r.Pattern; p != "" {
			path = strings.TrimPrefix(p, r.Method+" ")
		}
		attrs := []any{
			"request_id", api.RequestIDFromContext(r.Context()),
			"method", r.Method,
			"path", path,
			"status", rec.status,
			"gateway", gatewayFrom(r.Context()).ID,
		}
		if id := r.PathValue("id"); id != "" {
			attrs = append(attrs, "lease_id", redactID(id))
		}
		s.cfg.Logger.Info("internal_request", attrs...)
	})
}

// redactID renders a short hash of a sensitive identifier for logs
// (SEC-41): correlatable across requests, useless as a credential.
func redactID(id string) string {
	sum := sha256.Sum256([]byte(id))
	return "sha256:" + hex.EncodeToString(sum[:])[:12]
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// writeErr renders the standard api.Error JSON shape at an explicit status —
// needed for 410, which the public ErrorCode enum does not cover.
func writeErr(w http.ResponseWriter, r *http.Request, status int, code api.ErrorCode, msg string) {
	e := &api.Error{
		Code:      code,
		Message:   msg,
		Retryable: code.Retryable(),
		RequestID: api.RequestIDFromContext(r.Context()),
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(e)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeErr(w, r, http.StatusBadRequest, api.CodeInvalidRequest, "invalid request body")
		return false
	}
	return true
}

// brokerError maps domain errors onto the contract status table:
// 401 invalid/expired ticket, 403 denied/revoked-ticket, 404 unknown lease,
// 409 stale binding / connection-in-use, 410 dead lease, 503 stale view.
func brokerError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, broker.ErrTicketInvalid), errors.Is(err, broker.ErrTicketExpired):
		writeErr(w, r, http.StatusUnauthorized, api.CodeUnauthenticated, "ticket invalid, used or expired")
	case errors.Is(err, broker.ErrRevoked):
		writeErr(w, r, http.StatusGone, codeRevoked, "lease or ticket revoked")
	case errors.Is(err, broker.ErrDenied):
		writeErr(w, r, http.StatusForbidden, api.CodeForbidden, "denied")
	case errors.Is(err, broker.ErrLeaseInvalid):
		writeErr(w, r, http.StatusNotFound, api.CodeNotFound, "unknown lease")
	case errors.Is(err, broker.ErrStaleBinding):
		writeErr(w, r, http.StatusConflict, api.CodeInvalidState, "stale runtime binding")
	case errors.Is(err, broker.ErrConnectionInUse):
		writeErr(w, r, http.StatusConflict, api.CodeConnectionInUse, "workspace already has an active lease")
	case errors.Is(err, broker.ErrFreshness):
		writeErr(w, r, http.StatusServiceUnavailable, api.CodeUnavailable, "observed state too stale; retry")
	case errors.Is(err, broker.ErrNotReady):
		writeErr(w, r, http.StatusConflict, api.CodeInvalidState, "workspace not ready")
	case errors.Is(err, broker.ErrNotFound):
		writeErr(w, r, http.StatusNotFound, api.CodeNotFound, "not found")
	default:
		writeErr(w, r, http.StatusInternalServerError, api.CodeInternal, "internal error")
	}
}

type redeemRequest struct {
	Ticket string `json:"ticket"`
}

func (s *server) redeem(w http.ResponseWriter, r *http.Request) {
	if !s.requireGateway(w, r) {
		return
	}
	var req redeemRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Ticket == "" {
		writeErr(w, r, http.StatusBadRequest, api.CodeInvalidRequest, "ticket required")
		return
	}
	lease, err := s.cfg.Broker.RedeemTicket(r.Context(), gatewayFrom(r.Context()), req.Ticket)
	if err != nil {
		brokerError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, lease)
}

type renewRequest struct {
	Fence broker.Fence `json:"fence"`
}

func (s *server) renew(w http.ResponseWriter, r *http.Request) {
	if !s.requireGateway(w, r) {
		return
	}
	id := r.PathValue("id")
	if id == "" || len(id) > 128 || strings.ContainsAny(id, "/?#") {
		writeErr(w, r, http.StatusBadRequest, api.CodeInvalidRequest, "bad lease id")
		return
	}
	var req renewRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	lease, err := s.cfg.Broker.RenewLease(r.Context(), gatewayFrom(r.Context()), id, req.Fence)
	if err != nil {
		brokerError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, lease)
}

func (s *server) target(w http.ResponseWriter, r *http.Request) {
	if !s.requireGateway(w, r) {
		return
	}
	id := r.PathValue("id")
	if id == "" || len(id) > 128 {
		writeErr(w, r, http.StatusBadRequest, api.CodeInvalidRequest, "bad lease id")
		return
	}
	tgt, err := s.cfg.Broker.ResolveTarget(r.Context(), gatewayFrom(r.Context()), id)
	if err != nil {
		brokerError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, tgt)
}

func (s *server) revoke(w http.ResponseWriter, r *http.Request) {
	if !s.requireGateway(w, r) {
		return
	}
	id := r.PathValue("id")
	if id == "" || len(id) > 128 {
		writeErr(w, r, http.StatusBadRequest, api.CodeInvalidRequest, "bad lease id")
		return
	}
	if err := s.cfg.Broker.RevokeLease(r.Context(), id); err != nil {
		brokerError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// requireGateway refuses the operator identity on the lease-scoped routes:
// the operator cert is not a session gateway and must not mint or feed
// session state.
func (s *server) requireGateway(w http.ResponseWriter, r *http.Request) bool {
	if gatewayFrom(r.Context()).ID == s.cfg.OperatorCN {
		writeErr(w, r, http.StatusForbidden, api.CodeForbidden, "operator identity is not a gateway")
		return false
	}
	return true
}

// requireOperator admits only the operator identity on the
// workspace-scoped routes (revoke/drain) — a gateway cert gets 403.
func (s *server) requireOperator(w http.ResponseWriter, r *http.Request) bool {
	if gatewayFrom(r.Context()).ID != s.cfg.OperatorCN {
		writeErr(w, r, http.StatusForbidden, api.CodeForbidden, "operator identity required")
		return false
	}
	return true
}

// leaseID validates the {id} path value shared by the lease routes.
func leaseID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("id")
	if id == "" || len(id) > 128 || strings.ContainsAny(id, "/?#") {
		writeErr(w, r, http.StatusBadRequest, api.CodeInvalidRequest, "bad lease id")
		return "", false
	}
	return id, true
}

// workspaceUID validates the {uid} path value shared by the workspace routes.
func workspaceUID(w http.ResponseWriter, r *http.Request) (string, bool) {
	uid := r.PathValue("uid")
	if uid == "" || len(uid) > 128 || strings.ContainsAny(uid, "/?#") {
		writeErr(w, r, http.StatusBadRequest, api.CodeInvalidRequest, "bad workspace uid")
		return "", false
	}
	return uid, true
}

type activityRequest struct {
	Fence broker.Fence             `json:"fence"`
	Type  broker.ActivityEventType `json:"type"`
}

// activity records a gateway-reported session signal. The receipt time is
// stamped server-side by the broker; client-supplied times are ignored.
// Errors: 400 unknown event type, 403 foreign gateway, 409 fenced/stale,
// 410 dead lease, 503 stale binding view.
func (s *server) activity(w http.ResponseWriter, r *http.Request) {
	if !s.requireGateway(w, r) {
		return
	}
	id, ok := leaseID(w, r)
	if !ok {
		return
	}
	var req activityRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	err := s.cfg.Broker.ReportActivity(r.Context(), gatewayFrom(r.Context()), id,
		req.Fence, broker.ActivityEvent{Type: req.Type})
	if err != nil {
		switch {
		case errors.Is(err, broker.ErrActivityType):
			writeErr(w, r, http.StatusBadRequest, api.CodeInvalidRequest, "unknown activity type")
		case errors.Is(err, broker.ErrRevoked), errors.Is(err, broker.ErrLeaseInvalid):
			// A dead lease is 410 on this route per the contract — the
			// broker reports it as ErrLeaseInvalid.
			writeErr(w, r, http.StatusGone, codeRevoked, "lease revoked or expired")
		default:
			brokerError(w, r, err)
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type revokeWorkspaceRequest struct {
	RuntimeGeneration uint64 `json:"runtimeGeneration"`
}

// revokeWorkspace is the operator teardown seam: revoke the workspace's
// live leases for the observed generation and block new connects for it.
func (s *server) revokeWorkspace(w http.ResponseWriter, r *http.Request) {
	if !s.requireOperator(w, r) {
		return
	}
	uid, ok := workspaceUID(w, r)
	if !ok {
		return
	}
	var req revokeWorkspaceRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	n, err := s.cfg.Broker.RevokeWorkspaceLeases(r.Context(), broker.PlatformID(uid), req.RuntimeGeneration)
	if err != nil {
		brokerError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"revokedLeases": n})
}

// drainWorkspace reports the workspace's open interactive streams so the
// operator's drain step can observe gateway progress.
func (s *server) drainWorkspace(w http.ResponseWriter, r *http.Request) {
	if !s.requireOperator(w, r) {
		return
	}
	uid, ok := workspaceUID(w, r)
	if !ok {
		return
	}
	open, drained, err := s.cfg.Broker.DrainStatus(r.Context(), broker.PlatformID(uid))
	if err != nil {
		brokerError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"openStreams": open, "drained": drained})
}

// ServerTLSConfig builds the tls.Config for the internal listener: the
// server presents cert; client certs are verified against clientCAPEM when
// presented and the handler answers 401 when absent — the error model is
// preserved instead of aborting the handshake on a missing cert.
func ServerTLSConfig(cert tls.Certificate, clientCAPEM []byte) (*tls.Config, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(clientCAPEM) {
		return nil, errors.New("httpapi: client CA bundle has no usable certificates")
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.VerifyClientCertIfGiven,
		ClientCAs:    pool,
		MinVersion:   tls.VersionTLS13,
	}, nil
}
