// Package brokerclient is the gateway's remote BrokerClient: it speaks the
// fixed internal contract (design-review-owned) to the broker's internal mTLS
// listener:
//
//	POST /internal/v1/broker/redeem              {"ticket": "<opaque>"} -> 200 Lease | 401/403/409
//	POST /internal/v1/broker/leases/{id}/renew   {"fence": {...}}       -> 200 Lease | 409 | 410
//	GET  /internal/v1/broker/leases/{id}/target                       -> 200 Target | 404/409/410
//	POST /internal/v1/broker/leases/{id}/revoke                       -> 204
//	POST /internal/v1/broker/leases/{id}/activity {"fence":{...},"type":"input|connected|disconnect","streamEpoch":N} -> 204 | 400/403/409/410
//
// Identity is the mTLS client certificate (CN = gateway ID); the client only
// loads cert/key/CA from files and never logs credential material (Target
// credentials appear only in the target response body — kept out of errors).
package brokerclient

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/broker"
	"github.com/tinyorbitvn/tinycdi/internal/gateway"
)

// Endpoints of the internal broker contract (the backend's separate
// internal listener, never the public mux).
const (
	redeemPath = "/internal/v1/broker/redeem"
	leasesPath = "/internal/v1/broker/leases/" // + {id}/renew | {id}/target | {id}/revoke
)

// Config wires a Client. Identity material is file-based (mTLS client
// cert/key + the broker CA bundle); TLSConfig may instead be injected
// pre-built (tests).
type Config struct {
	// BaseURL is the internal broker listener, e.g. https://api-internal:9443.
	BaseURL string
	// CertFile/KeyFile/CAFile are PEM paths for the client certificate and
	// the CA that signed the broker's internal listener certificate.
	CertFile, KeyFile, CAFile string
	// TLSConfig, when set, is used verbatim and the file fields are ignored.
	TLSConfig *tls.Config
	// Timeout bounds each call; default 10 s.
	Timeout time.Duration
}

// Client implements gateway.BrokerClient over mTLS HTTPS.
type Client struct {
	hc   *http.Client
	base string
}

var _ gateway.BrokerClient = (*Client)(nil)

// New loads the mTLS material and returns the client.
func New(cfg Config) (*Client, error) {
	if cfg.BaseURL == "" {
		return nil, errors.New("brokerclient: BaseURL required")
	}
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, fmt.Errorf("brokerclient: BaseURL must be a valid https URL: %q", cfg.BaseURL)
	}
	tc := cfg.TLSConfig
	if tc == nil {
		if cfg.CertFile == "" || cfg.KeyFile == "" || cfg.CAFile == "" {
			return nil, errors.New("brokerclient: CertFile, KeyFile and CAFile are required")
		}
		cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("brokerclient: load client cert: %w", err)
		}
		caPEM, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("brokerclient: read CA: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caPEM) {
			return nil, errors.New("brokerclient: CA file contains no PEM certificates")
		}
		tc = &tls.Config{
			Certificates: []tls.Certificate{cert},
			RootCAs:      pool,
			MinVersion:   tls.VersionTLS12,
		}
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &Client{
		hc: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				TLSClientConfig:     tc,
				TLSHandshakeTimeout: 5 * time.Second,
			},
		},
		base: cfg.BaseURL,
	}, nil
}

// --- wire shapes (contract-fixed) -------------------------------------------

type leaseJSON struct {
	LeaseID           string    `json:"leaseId"`
	WorkspaceUID      string    `json:"workspaceUID"`
	TenantID          string    `json:"tenantID"`
	PrincipalSubject  string    `json:"principalSubject"`
	RuntimeGeneration uint64    `json:"runtimeGeneration"`
	RuntimeUID        string    `json:"runtimeUID"`
	FencingVersion    uint64    `json:"fencingVersion"`
	GatewayID         string    `json:"gatewayID"`
	ExpiresAt         time.Time `json:"expiresAt"`
}

func (l leaseJSON) lease() broker.Lease {
	return broker.Lease{
		ID:                l.LeaseID,
		WorkspaceUID:      l.WorkspaceUID,
		TenantID:          l.TenantID,
		PrincipalSubject:  l.PrincipalSubject,
		RuntimeGeneration: l.RuntimeGeneration,
		RuntimeUID:        l.RuntimeUID,
		FencingVersion:    l.FencingVersion,
		GatewayID:         l.GatewayID,
		ExpiresAt:         l.ExpiresAt,
	}
}

// fenceJSON mirrors broker.Fence; the fencing version travels as "version"
// per the contract {"fence": {"version": N, ...Fence fields}}.
type fenceJSON struct {
	WorkspaceUID      string `json:"workspaceUID"`
	RuntimeGeneration uint64 `json:"runtimeGeneration"`
	RuntimeUID        string `json:"runtimeUID"`
	Version           uint64 `json:"version"`
}

func fenceOf(f broker.Fence) fenceJSON {
	return fenceJSON{
		WorkspaceUID:      f.WorkspaceUID,
		RuntimeGeneration: f.RuntimeGeneration,
		RuntimeUID:        f.RuntimeUID,
		Version:           f.FencingVersion,
	}
}

// targetJSON mirrors the contract Target document. Credentials live only in
// this body — they are copied into broker.Target and never logged.
// The contract field for the pinned CA is "caPEM"; "tlsCA" is accepted as a
// decoding alias in case the server serializes broker.Target directly.
type targetJSON struct {
	UpstreamURL   string `json:"upstreamURL"`
	TLSServerName string `json:"tlsServerName"`
	ServiceDNS    string `json:"serviceDNS"`
	CAPEM         []byte `json:"caPEM"`
	TLSCA         []byte `json:"tlsCA"`
	Username      string `json:"username"`
	Password      string `json:"password"`
	Protocol      string `json:"protocol"`
}

func (t targetJSON) caPEM() []byte {
	if len(t.CAPEM) > 0 {
		return t.CAPEM
	}
	return t.TLSCA
}

func (t targetJSON) target() (broker.Target, error) {
	if t.UpstreamURL == "" {
		return broker.Target{}, errors.New("brokerclient: target response missing upstreamURL")
	}
	u, err := url.Parse(t.UpstreamURL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return broker.Target{}, fmt.Errorf("brokerclient: bad upstreamURL %q", t.UpstreamURL)
	}
	return broker.Target{
		Protocol:      t.Protocol,
		ServiceDNS:    t.ServiceDNS,
		UpstreamURL:   t.UpstreamURL,
		TLSServerName: t.TLSServerName,
		TLSCA:         t.caPEM(),
		Username:      t.Username,
		Password:      t.Password,
	}, nil
}

// --- error model ------------------------------------------------------------

// Error is a broker-side failure: HTTP status + the standard error body.
// Sentinel maps to a broker.* domain error so callers can errors.Is against
// the broker error set; Sentinel == nil marks a transient/retryable failure.
type Error struct {
	Op        string
	Status    int
	Code      string
	Message   string
	RequestID string
	Sentinel  error
}

func (e *Error) Error() string {
	return fmt.Sprintf("brokerclient: %s: http %d %s: %s", e.Op, e.Status, e.Code, e.Message)
}

func (e *Error) Unwrap() error { return e.Sentinel }

// apiError mirrors internal/api's error body (code/message/retryable/
// requestId) without importing the public API package's server helpers.
type apiError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
	RequestID string `json:"requestId"`
}

// sentinelFor maps (endpoint, status, error code) onto the broker domain
// errors. Terminal conditions (revoked/stale/denied/not-found) get a
// sentinel so the gateway fails the session; transient statuses stay
// sentinel-free.
func sentinelFor(op string, status int, code string) error {
	// 410 is unambiguous regardless of the body's code: the lease is revoked.
	if status == http.StatusGone {
		return broker.ErrRevoked
	}
	switch code {
	case "CONNECTION_IN_USE":
		return broker.ErrConnectionInUse
	case "INVALID_STATE":
		return broker.ErrStaleBinding
	case "FORBIDDEN", "CSRF_FAILED":
		return broker.ErrDenied
	case "NOT_FOUND":
		return broker.ErrNotFound
	}
	switch status {
	case http.StatusUnauthorized:
		if op == "redeem" {
			return broker.ErrTicketInvalid
		}
		return broker.ErrDenied
	case http.StatusForbidden:
		return broker.ErrDenied
	case http.StatusNotFound:
		if op == "redeem" {
			return broker.ErrTicketInvalid
		}
		return broker.ErrLeaseInvalid
	case http.StatusConflict:
		if op == "redeem" {
			return broker.ErrConnectionInUse
		}
		return broker.ErrStaleBinding
	}
	return nil // transient: 5xx, 429, unknown → gateway renew retry budget applies
}

// --- plumbing -----------------------------------------------------------------

func newReqID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "req-" + hex.EncodeToString(b[:])
}

// do issues one contract call. get+path or post+body; on success the JSON
// body decodes into out (nil for 204).
func (c *Client) do(ctx context.Context, op, method, path string, body any, out any) error {
	var rdr *bytes.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("brokerclient: %s: marshal: %w", op, err)
		}
		rdr = bytes.NewReader(buf)
	} else {
		rdr = &bytes.Reader{}
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return fmt.Errorf("brokerclient: %s: %w", op, err)
	}
	req.Header.Set("X-Request-Id", newReqID())
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return &Error{Op: op, Status: 0, Code: "TRANSPORT", Message: "broker unreachable"}
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if out == nil {
			return nil
		}
		return json.NewDecoder(resp.Body).Decode(out)
	}

	ae := apiError{}
	dec := json.NewDecoder(resp.Body)
	if err := dec.Decode(&ae); err != nil {
		ae.Code = fmt.Sprintf("HTTP_%d", resp.StatusCode)
	}
	// Messages from the broker error model are sanitized by construction;
	// response bodies are never logged elsewhere so credentials cannot leak.
	return &Error{
		Op:        op,
		Status:    resp.StatusCode,
		Code:      ae.Code,
		Message:   ae.Message,
		RequestID: ae.RequestID,
		Sentinel:  sentinelFor(op, resp.StatusCode, ae.Code),
	}
}

// --- gateway.BrokerClient -----------------------------------------------------

// RedeemTicket POSTs the opaque ticket; the gateway identity is implicit in
// the mTLS client certificate.
func (c *Client) RedeemTicket(ctx context.Context, _ broker.GatewayIdentity, opaque string) (broker.Lease, error) {
	var out leaseJSON
	err := c.do(ctx, "redeem", http.MethodPost, redeemPath,
		struct {
			Ticket string `json:"ticket"`
		}{Ticket: opaque}, &out)
	if err != nil {
		return broker.Lease{}, err
	}
	return out.lease(), nil
}

// RenewLease slides the lease expiry; fence pins the runtime incarnation.
func (c *Client) RenewLease(ctx context.Context, _ broker.GatewayIdentity, leaseID string, fence broker.Fence) (broker.Lease, error) {
	var out leaseJSON
	err := c.do(ctx, "renew", http.MethodPost,
		leasesPath+url.PathEscape(leaseID)+"/renew",
		struct {
			Fence fenceJSON `json:"fence"`
		}{Fence: fenceOf(fence)}, &out)
	if err != nil {
		return broker.Lease{}, err
	}
	return out.lease(), nil
}

// ResolveTarget fetches the internal upstream target (URL, pinned CA,
// credentials) for a live lease.
func (c *Client) ResolveTarget(ctx context.Context, _ broker.GatewayIdentity, leaseID string) (broker.Target, error) {
	var out targetJSON
	err := c.do(ctx, "target", http.MethodGet,
		leasesPath+url.PathEscape(leaseID)+"/target", nil, &out)
	if err != nil {
		return broker.Target{}, err
	}
	return out.target()
}

// RevokeLease revokes the lease server-side; broker-side teardown then
// propagates through renew failures.
func (c *Client) RevokeLease(ctx context.Context, leaseID string) error {
	return c.do(ctx, "revoke", http.MethodPost,
		leasesPath+url.PathEscape(leaseID)+"/revoke", struct{}{}, nil)
}

// RevokeLeaseChanged revokes the lease and reports whether a live lease was
// actually revoked. An empty 204 answer (a broker that predates the report)
// carries no information and counts as revoked.
func (c *Client) RevokeLeaseChanged(ctx context.Context, leaseID string) (bool, error) {
	var out struct {
		Revoked bool `json:"revoked"`
	}
	err := c.do(ctx, "revoke", http.MethodPost,
		leasesPath+url.PathEscape(leaseID)+"/revoke", struct{}{}, &out)
	if errors.Is(err, io.EOF) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return out.Revoked, nil
}

// ReportActivity posts one session signal; the broker stamps its own
// receipt time — no client timestamp is sent.
func (c *Client) ReportActivity(ctx context.Context, _ broker.GatewayIdentity, leaseID string, fence broker.Fence, ev broker.ActivityEvent) error {
	return c.do(ctx, "activity", http.MethodPost,
		leasesPath+url.PathEscape(leaseID)+"/activity",
		struct {
			Fence       fenceJSON                `json:"fence"`
			Type        broker.ActivityEventType `json:"type"`
			StreamEpoch uint64                   `json:"streamEpoch,omitempty"`
		}{Fence: fenceOf(fence), Type: ev.Type, StreamEpoch: ev.StreamEpoch}, nil)
}
