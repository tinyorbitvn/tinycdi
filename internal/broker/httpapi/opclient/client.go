// Package opclient is the operator's client for the workspace-scoped
// routes of the internal broker API (ADR 0003):
//
//	POST /internal/v1/broker/workspaces/{uid}/revoke {"runtimeGeneration":N} -> 200 {"revokedLeases":n}
//	GET  /internal/v1/broker/workspaces/{uid}/drain                        -> 200 {"openStreams":n,"drained":b}
//
// Identity is the mTLS client certificate (CN = the configured operator
// CN, default "operator"); the same listener refuses gateway certs on
// these routes.
//
// The client implements the operator's settled seams (
// internal/operator/finalizer.go):
//
//	LeaseRevoker.RevokeAllForWorkspace(ctx, workspaceUID, runtimeGeneration) (int, error)
//	StreamDrainer.DrainStatus(ctx, workspaceUID) (openStreams, drained, error)
//
// For full teardown the operator passes its observed runtimeGeneration;
// leases pinned to a NEWER generation survive by design (stale teardown
// never fences a restarted runtime).
package opclient

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
	"net/http"
	"net/url"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/operator"
	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
	"github.com/tinyorbitvn/tinycdi/internal/tlsreload"
)

// Compile-time conformance with the settled operator seams (
// internal/operator/finalizer.go).
var (
	_ operator.LeaseRevoker  = (*Client)(nil)
	_ operator.StreamDrainer = (*Client)(nil)
)

// Endpoints of the workspace-scoped operator contract.
const workspacesPath = "/internal/v1/broker/workspaces/" // + {uid}/revoke | {uid}/drain

// Config wires a Client. Identity material is file-based (operator mTLS
// cert/key + the broker CA bundle); TLSConfig may instead be injected
// pre-built (tests).
type Config struct {
	// BaseURL is the internal broker listener, e.g. https://api-internal:9443.
	BaseURL string
	// CertFile/KeyFile/CAFile are PEM paths for the operator client
	// certificate and the CA bundle that signs the broker's internal
	// listener; both hot-reload under Run (E5).
	CertFile, KeyFile, CAFile string
	// TLSConfig, when set, is used verbatim and the file fields are ignored
	// (and no hot-reload is wired).
	TLSConfig *tls.Config
	// Timeout bounds each call; default 10 s.
	Timeout time.Duration
	// ReloadInterval is how often the certificate and CA bundle files are
	// re-checked by Run; default 30 s.
	ReloadInterval time.Duration
}

// Client calls the workspace-scoped internal broker routes over mTLS.
type Client struct {
	hc   *http.Client
	base string
	rel  *tlsreload.Reloader // nil when TLSConfig was injected
	ca   *tlsreload.CAPool   // nil when TLSConfig was injected
}

// New loads the mTLS material and returns the client.
func New(cfg Config) (*Client, error) {
	if cfg.BaseURL == "" {
		return nil, errors.New("opclient: BaseURL required")
	}
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, fmt.Errorf("opclient: BaseURL must be a valid https URL: %q", cfg.BaseURL)
	}
	tc := cfg.TLSConfig
	var rel *tlsreload.Reloader
	var ca *tlsreload.CAPool
	if tc == nil {
		if cfg.CertFile == "" || cfg.KeyFile == "" || cfg.CAFile == "" {
			return nil, errors.New("opclient: CertFile, KeyFile and CAFile are required")
		}
		var opts []tlsreload.Option
		if cfg.ReloadInterval > 0 {
			opts = append(opts, tlsreload.WithInterval(cfg.ReloadInterval))
		}
		rel, err = tlsreload.New(cfg.CertFile, cfg.KeyFile, opts...)
		if err != nil {
			return nil, fmt.Errorf("opclient: load client cert: %w", err)
		}
		ca, err = tlsreload.NewCAPool(cfg.CAFile, opts...)
		if err != nil {
			return nil, fmt.Errorf("opclient: load CA bundle: %w", err)
		}
		serverName := u.Hostname()
		tc = &tls.Config{
			// GetClientCertificate re-reads the reloader's current pair at
			// every new handshake, so rotating the cert files presents the
			// new certificate without a restart (E5). Run must be started
			// for the reloader to pick up changes.
			GetClientCertificate: rel.GetClientCertificate,
			// InsecureSkipVerify hands verification to VerifyConnection:
			// the static RootCAs field is read once per Client build, but
			// the internal CA rotates on the same file. VerifyConnection
			// runs the standard chain + hostname check against the CAPool's
			// newest parsed bundle on every handshake, so trust follows
			// the file without a restart. InsecureSkipVerify is set only
			// as this handoff — never without VerifyConnection.
			InsecureSkipVerify: true,
			VerifyConnection: func(cs tls.ConnectionState) error {
				return verifyServerChain(cs.PeerCertificates, serverName, ca.Pool())
			},
			MinVersion: tls.VersionTLS12,
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
		rel:  rel,
		ca:   ca,
	}, nil
}

// Run reloads the client certificate files and the broker CA bundle until
// ctx is done (E5). It is a no-op when the client was built from an
// injected TLSConfig.
func (c *Client) Run(ctx context.Context) {
	if c.rel == nil {
		return
	}
	go c.ca.Run(ctx)
	c.rel.Run(ctx)
}

// verifyServerChain is the VerifyConnection check that InsecureSkipVerify
// hands off to: it re-runs the standard server verification — chain to a
// trusted root plus hostname, with the default ServerAuth EKU — against
// roots, the CAPool's newest parsed bundle. x509 is asked to verify
// against the pool read at this handshake, which is what a fixed
// tls.Config.RootCAs cannot express once the bundle file rotates.
func verifyServerChain(peers []*x509.Certificate, serverName string, roots *x509.CertPool) error {
	if len(peers) == 0 {
		return errors.New("opclient: broker presented no certificate")
	}
	intermediates := x509.NewCertPool()
	for _, cert := range peers[1:] {
		intermediates.AddCert(cert)
	}
	if _, err := peers[0].Verify(x509.VerifyOptions{
		DNSName:       serverName,
		Roots:         roots,
		Intermediates: intermediates,
	}); err != nil {
		return fmt.Errorf("opclient: verify broker certificate: %w", err)
	}
	return nil
}

// Error is a broker-side failure: HTTP status + the standard error body.
type Error struct {
	Op        string
	Status    int
	Code      string
	Message   string
	RequestID string
}

func (e *Error) Error() string {
	return fmt.Sprintf("opclient: %s: http %d %s: %s", e.Op, e.Status, e.Code, e.Message)
}

// apiError mirrors internal/api's error body without importing the public
// API package's server helpers.
type apiError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"requestId"`
}

func newReqID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "req-" + hex.EncodeToString(b[:])
}

// do issues one contract call; on success the JSON body decodes into out.
func (c *Client) do(ctx context.Context, op, method, path string, body any, out any) error {
	var rdr *bytes.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("opclient: %s: marshal: %w", op, err)
		}
		rdr = bytes.NewReader(buf)
	} else {
		rdr = &bytes.Reader{}
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return fmt.Errorf("opclient: %s: %w", op, err)
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
	if err := json.NewDecoder(resp.Body).Decode(&ae); err != nil {
		ae.Code = fmt.Sprintf("HTTP_%d", resp.StatusCode)
	}
	return &Error{
		Op:        op,
		Status:    resp.StatusCode,
		Code:      ae.Code,
		Message:   ae.Message,
		RequestID: ae.RequestID,
	}
}

// RevokeAllForWorkspace revokes every live lease of the workspace bound to
// a runtime generation <= runtimeGeneration and blocks new tickets/redeems
// for the covered generations. Returns the number of revoked leases.
func (c *Client) RevokeAllForWorkspace(ctx context.Context, workspaceUID provisioning.PlatformID, runtimeGeneration int64) (int, error) {
	var out struct {
		RevokedLeases int `json:"revokedLeases"`
	}
	err := c.do(ctx, "revoke", http.MethodPost,
		workspacesPath+url.PathEscape(string(workspaceUID))+"/revoke",
		struct {
			RuntimeGeneration int64 `json:"runtimeGeneration"`
		}{RuntimeGeneration: runtimeGeneration}, &out)
	if err != nil {
		return 0, err
	}
	return out.RevokedLeases, nil
}

// DrainStatus returns the workspace's open interactive stream count as
// reported by the gateways, and whether the workspace is fully drained.
func (c *Client) DrainStatus(ctx context.Context, workspaceUID provisioning.PlatformID) (openStreams int, drained bool, err error) {
	var out struct {
		OpenStreams int  `json:"openStreams"`
		Drained     bool `json:"drained"`
	}
	err = c.do(ctx, "drain", http.MethodGet,
		workspacesPath+url.PathEscape(string(workspaceUID))+"/drain", nil, &out)
	if err != nil {
		return 0, false, err
	}
	return out.OpenStreams, out.Drained, nil
}
