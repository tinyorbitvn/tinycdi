package httpapi_test

// Contract tests for the internal gateway↔broker mTLS API (ADR 0003):
// identity from the verified client cert, the fixed route/error table, and
// the rule that ticket/secret material never crosses into logs.

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/broker"
	"github.com/tinyorbitvn/tinycdi/internal/broker/httpapi"
)

// fakeBroker scripts the broker surface; err maps let tests inject the
// domain errors the handler must translate.
type fakeBroker struct {
	redeemLease broker.Lease
	redeemErr   error
	renewLease  broker.Lease
	renewErr    error
	target      broker.Target
	targetErr   error
	revokeErr   error

	activityErr   error
	revokedLeases int
	revokeWsErr   error
	openStreams   int
	drained       bool
	drainErr      error

	gotGW      broker.GatewayIdentity
	gotTicket  string
	gotFence   broker.Fence
	gotLeaseID string
	gotEvent   broker.ActivityEvent
	gotWsUID   broker.PlatformID
	gotWsGen   uint64
}

func (f *fakeBroker) RedeemTicket(_ context.Context, gw broker.GatewayIdentity, opaque string) (broker.Lease, error) {
	f.gotGW, f.gotTicket = gw, opaque
	return f.redeemLease, f.redeemErr
}

func (f *fakeBroker) RenewLease(_ context.Context, gw broker.GatewayIdentity, leaseID string, fence broker.Fence) (broker.Lease, error) {
	f.gotGW, f.gotLeaseID, f.gotFence = gw, leaseID, fence
	return f.renewLease, f.renewErr
}

func (f *fakeBroker) ResolveTarget(_ context.Context, gw broker.GatewayIdentity, leaseID string) (broker.Target, error) {
	f.gotGW, f.gotLeaseID = gw, leaseID
	return f.target, f.targetErr
}

func (f *fakeBroker) RevokeLease(_ context.Context, leaseID string) error {
	f.gotLeaseID = leaseID
	return f.revokeErr
}

func (f *fakeBroker) ReportActivity(_ context.Context, gw broker.GatewayIdentity, leaseID string, fence broker.Fence, ev broker.ActivityEvent) error {
	f.gotGW, f.gotLeaseID, f.gotFence, f.gotEvent = gw, leaseID, fence, ev
	return f.activityErr
}

func (f *fakeBroker) RevokeWorkspaceLeases(_ context.Context, wsUID broker.PlatformID, gen uint64) (int, error) {
	f.gotWsUID, f.gotWsGen = wsUID, gen
	return f.revokedLeases, f.revokeWsErr
}

func (f *fakeBroker) DrainStatus(_ context.Context, wsUID broker.PlatformID) (int, bool, error) {
	f.gotWsUID = wsUID
	return f.openStreams, f.drained, f.drainErr
}

// pki is a scratch CA + cert factory for the mTLS fixture.
type pki struct {
	caCert *x509.Certificate
	caKey  *ecdsa.PrivateKey
	pool   *x509.CertPool
}

func newPKI(t *testing.T) *pki {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "test-internal-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &pki{caCert: cert, caKey: key, pool: pool}
}

func (p *pki) issue(t *testing.T, cn string, uriSANs []string, server bool) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	if server {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		tmpl.DNSNames = []string{"127.0.0.1", "localhost"}
		tmpl.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
	} else {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
		for _, u := range uriSANs {
			parsed, err := url.Parse(u)
			if err != nil {
				t.Fatal(err)
			}
			tmpl.URIs = append(tmpl.URIs, parsed)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, p.caCert, &key.PublicKey, p.caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

// env wires the mTLS test server + a client factory.
type env struct {
	srv   *httptest.Server
	caPEM []byte
	pki   *pki
	fb    *fakeBroker
	logs  *bytes.Buffer
}

func newEnv(t *testing.T, fb *fakeBroker) *env {
	t.Helper()
	p := newPKI(t)
	logs := &bytes.Buffer{}
	h := httpapi.NewHandler(httpapi.Config{
		Broker:   fb,
		Audience: "session.example.dev",
		Logger:   slog.New(slog.NewJSONHandler(logs, nil)),
	})
	srv := httptest.NewUnstartedServer(h)
	srvCert := p.issue(t, "broker-internal", nil, true)
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{srvCert},
		ClientAuth:   tls.VerifyClientCertIfGiven,
		ClientCAs:    p.pool,
	}
	srv.StartTLS()
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: p.caCert.Raw})
	e := &env{srv: srv, caPEM: caPEM, pki: p, fb: fb, logs: logs}
	t.Cleanup(srv.Close)
	return e
}

func (e *env) client(t *testing.T, cert *tls.Certificate) *http.Client {
	t.Helper()
	cfg := &tls.Config{RootCAs: e.pki.pool}
	if cert != nil {
		cfg.Certificates = []tls.Certificate{*cert}
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: cfg}}
}

var sampleLease = broker.Lease{
	ID:                "lease-abc",
	WorkspaceUID:      "ws-1",
	TenantID:          "tenant-a",
	PrincipalSubject:  "iss|alice",
	RuntimeGeneration: 3,
	RuntimeUID:        "rt-3",
	FencingVersion:    2,
	GatewayID:         "gw-1",
	ExpiresAt:         time.Now().Add(30 * time.Second).UTC(),
}

func TestRedeem_OK(t *testing.T) {
	fb := &fakeBroker{redeemLease: sampleLease}
	e := newEnv(t, fb)
	cert := e.pki.issue(t, "gw-1", nil, false)
	resp, err := e.client(t, &cert).Post(e.srv.URL+"/internal/v1/broker/redeem",
		"application/json", bytes.NewReader([]byte(`{"ticket":"opaque-1"}`)))
	if err != nil {
		t.Fatalf("redeem: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("redeem status=%d body=%s", resp.StatusCode, b)
	}
	var lease broker.Lease
	if err := json.NewDecoder(resp.Body).Decode(&lease); err != nil {
		t.Fatalf("decode lease: %v", err)
	}
	if lease.ID != "lease-abc" || lease.FencingVersion != 2 {
		t.Fatalf("bad lease json: %+v", lease)
	}
	if fb.gotGW.ID != "gw-1" || fb.gotGW.Audience != "session.example.dev" {
		t.Fatalf("gateway identity = %+v, want gw-1/session.example.dev", fb.gotGW)
	}
	if fb.gotTicket != "opaque-1" {
		t.Fatalf("ticket not forwarded: %q", fb.gotTicket)
	}
}

func TestNoClientCert_401(t *testing.T) {
	e := newEnv(t, &fakeBroker{})
	resp, err := e.client(t, nil).Post(e.srv.URL+"/internal/v1/broker/redeem",
		"application/json", bytes.NewReader([]byte(`{"ticket":"x"}`)))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status=%d, want 401", resp.StatusCode)
	}
	var body struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Code != "UNAUTHENTICATED" {
		t.Fatalf("code=%q, want UNAUTHENTICATED", body.Code)
	}
}

func TestWrongClientCA_Rejected(t *testing.T) {
	e := newEnv(t, &fakeBroker{})
	foreign := newPKI(t)
	cert := foreign.issue(t, "gw-evil", nil, false)
	_, err := e.client(t, &cert).Post(e.srv.URL+"/internal/v1/broker/redeem",
		"application/json", bytes.NewReader([]byte(`{"ticket":"x"}`)))
	if err == nil {
		t.Fatal("request with foreign-CA client cert succeeded — handshake must fail")
	}
}

func TestSPIFFESAN_MismatchRejected(t *testing.T) {
	e := newEnv(t, &fakeBroker{})
	cert := e.pki.issue(t, "gw-1", []string{"spiffe://cdi.tinyorbit.vn/gateway/gw-9"}, false)
	resp, err := e.client(t, &cert).Post(e.srv.URL+"/internal/v1/broker/redeem",
		"application/json", bytes.NewReader([]byte(`{"ticket":"x"}`)))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status=%d, want 403 for CN/SPIFFE mismatch", resp.StatusCode)
	}
}

func TestSPIFFESAN_MatchAccepted(t *testing.T) {
	fb := &fakeBroker{redeemLease: sampleLease}
	e := newEnv(t, fb)
	cert := e.pki.issue(t, "gw-1", []string{"spiffe://cdi.tinyorbit.vn/gateway/gw-1"}, false)
	resp, err := e.client(t, &cert).Post(e.srv.URL+"/internal/v1/broker/redeem",
		"application/json", bytes.NewReader([]byte(`{"ticket":"opaque-1"}`)))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200", resp.StatusCode)
	}
}

func TestRedeem_ErrorMapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"invalid", broker.ErrTicketInvalid, http.StatusUnauthorized},
		{"expired", broker.ErrTicketExpired, http.StatusUnauthorized},
		{"revoked", broker.ErrRevoked, http.StatusGone},
		{"denied", broker.ErrDenied, http.StatusForbidden},
		{"in-use", broker.ErrConnectionInUse, http.StatusConflict},
		{"stale", broker.ErrStaleBinding, http.StatusConflict},
		{"freshness", broker.ErrFreshness, http.StatusServiceUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, &fakeBroker{redeemErr: tc.err})
			cert := e.pki.issue(t, "gw-1", nil, false)
			resp, err := e.client(t, &cert).Post(e.srv.URL+"/internal/v1/broker/redeem",
				"application/json", bytes.NewReader([]byte(`{"ticket":"x"}`)))
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Fatalf("status=%d, want %d", resp.StatusCode, tc.want)
			}
		})
	}
}

func TestRenew_OK_AndErrorMapping(t *testing.T) {
	fb := &fakeBroker{renewLease: sampleLease}
	e := newEnv(t, fb)
	cert := e.pki.issue(t, "gw-1", nil, false)
	body := `{"fence":{"version":2,"workspaceUID":"ws-1","runtimeGeneration":3,"runtimeUID":"rt-3"}}`
	resp, err := e.client(t, &cert).Post(
		e.srv.URL+"/internal/v1/broker/leases/lease-abc/renew",
		"application/json", bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatalf("renew: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("renew status=%d", resp.StatusCode)
	}
	if fb.gotFence.FencingVersion != 2 || fb.gotLeaseID != "lease-abc" {
		t.Fatalf("renew args: id=%q fence=%+v", fb.gotLeaseID, fb.gotFence)
	}

	// fenced/stale -> 409, revoked -> 410, unknown -> 404
	for _, tc := range []struct {
		err  error
		want int
	}{
		{broker.ErrStaleBinding, http.StatusConflict},
		{broker.ErrRevoked, http.StatusGone},
		{broker.ErrLeaseInvalid, http.StatusNotFound},
		{broker.ErrDenied, http.StatusForbidden},
	} {
		fb.renewErr = tc.err
		resp, err := e.client(t, &cert).Post(
			e.srv.URL+"/internal/v1/broker/leases/lease-abc/renew",
			"application/json", bytes.NewReader([]byte(body)))
		if err != nil {
			t.Fatalf("renew: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Fatalf("renew(%v) status=%d, want %d", tc.err, resp.StatusCode, tc.want)
		}
	}
}

func TestTarget_OK_AndRevoked(t *testing.T) {
	fb := &fakeBroker{target: broker.Target{
		Protocol:      "kasmvnc-websocket",
		ServiceDNS:    "ws-1.ns-a.svc.cluster.local:8443",
		UpstreamURL:   "https://ws-1.ns-a.svc.cluster.local:8443",
		TLSServerName: "ws-1",
		TLSCA:         []byte("pem-ca"),
		Username:      "kasm_user",
		Password:      "pw",
	}}
	e := newEnv(t, fb)
	cert := e.pki.issue(t, "gw-1", nil, false)
	resp, err := e.client(t, &cert).Get(e.srv.URL + "/internal/v1/broker/leases/lease-abc/target")
	if err != nil {
		t.Fatalf("target: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("target status=%d", resp.StatusCode)
	}
	var tgt broker.Target
	if err := json.NewDecoder(resp.Body).Decode(&tgt); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if tgt.UpstreamURL == "" || tgt.TLSServerName != "ws-1" || tgt.Password != "pw" {
		t.Fatalf("bad target json: %+v", tgt)
	}

	// revoked/expired lease -> 410
	fb.targetErr = broker.ErrRevoked
	resp2, err := e.client(t, &cert).Get(e.srv.URL + "/internal/v1/broker/leases/lease-abc/target")
	if err != nil {
		t.Fatalf("target: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusGone {
		t.Fatalf("revoked target status=%d, want 410", resp2.StatusCode)
	}
}

func TestRevoke_204(t *testing.T) {
	fb := &fakeBroker{}
	e := newEnv(t, fb)
	cert := e.pki.issue(t, "gw-1", nil, false)
	resp, err := e.client(t, &cert).Post(
		e.srv.URL+"/internal/v1/broker/leases/lease-abc/revoke", "application/json", nil)
	if err != nil {
		t.Fatalf("revoke: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke status=%d, want 204", resp.StatusCode)
	}
	if fb.gotLeaseID != "lease-abc" {
		t.Fatalf("revoke id=%q", fb.gotLeaseID)
	}
}

// TestNoSecretsInLogs: ticket and credential material never reaches the
// request log even on the paths that carry them.
func TestNoSecretsInLogs(t *testing.T) {
	fb := &fakeBroker{redeemLease: sampleLease, target: broker.Target{Password: "s3cr3t-pw"}}
	e := newEnv(t, fb)
	cert := e.pki.issue(t, "gw-1", nil, false)
	resp, err := e.client(t, &cert).Post(e.srv.URL+"/internal/v1/broker/redeem",
		"application/json", bytes.NewReader([]byte(`{"ticket":"tkt_super_secret_value"}`)))
	if err != nil {
		t.Fatalf("redeem: %v", err)
	}
	resp.Body.Close()
	resp2, err := e.client(t, &cert).Get(e.srv.URL + "/internal/v1/broker/leases/lease-abc/target")
	if err != nil {
		t.Fatalf("target: %v", err)
	}
	resp2.Body.Close()
	if logs := e.logs.String(); bytes.Contains([]byte(logs), []byte("tkt_super_secret_value")) ||
		bytes.Contains([]byte(logs), []byte("s3cr3t-pw")) {
		t.Fatalf("secrets leaked into logs: %s", logs)
	}
}

// --- activity + operator revocation/drain ---------------------------------

func TestActivity_204_AndForwards(t *testing.T) {
	fb := &fakeBroker{}
	e := newEnv(t, fb)
	cert := e.pki.issue(t, "gw-1", nil, false)
	body := `{"fence":{"version":2,"workspaceUID":"ws-1","runtimeGeneration":3,"runtimeUID":"rt-3"},"type":"connected","streamEpoch":7}`
	resp, err := e.client(t, &cert).Post(
		e.srv.URL+"/internal/v1/broker/leases/lease-abc/activity",
		"application/json", bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatalf("activity: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("activity status=%d, want 204", resp.StatusCode)
	}
	if fb.gotEvent.Type != broker.ActivityConnected || fb.gotEvent.StreamEpoch != 7 || fb.gotLeaseID != "lease-abc" ||
		fb.gotFence.FencingVersion != 2 || fb.gotGW.ID != "gw-1" {
		t.Fatalf("activity args: ev=%+v lease=%q fence=%+v gw=%+v",
			fb.gotEvent, fb.gotLeaseID, fb.gotFence, fb.gotGW)
	}
}

func TestActivity_ErrorMapping(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want int
	}{
		{broker.ErrStaleBinding, http.StatusConflict},
		{broker.ErrRevoked, http.StatusGone},
		{broker.ErrLeaseInvalid, http.StatusGone},
		{broker.ErrDenied, http.StatusForbidden},
		{broker.ErrActivityType, http.StatusBadRequest},
	} {
		e := newEnv(t, &fakeBroker{activityErr: tc.err})
		cert := e.pki.issue(t, "gw-1", nil, false)
		resp, err := e.client(t, &cert).Post(
			e.srv.URL+"/internal/v1/broker/leases/lease-abc/activity",
			"application/json", bytes.NewReader([]byte(`{"fence":{},"type":"input"}`)))
		if err != nil {
			t.Fatalf("activity: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Fatalf("activity(%v) status=%d, want %d", tc.err, resp.StatusCode, tc.want)
		}
	}
}

// Identity split: the operator cert cannot report gateway activity, and a
// gateway cert cannot drive the workspace-scoped operator routes.
func TestActivity_OperatorIdentityForbidden(t *testing.T) {
	e := newEnv(t, &fakeBroker{})
	cert := e.pki.issue(t, httpapi.DefaultOperatorCN, nil, false)
	resp, err := e.client(t, &cert).Post(
		e.srv.URL+"/internal/v1/broker/leases/lease-abc/activity",
		"application/json", bytes.NewReader([]byte(`{"fence":{},"type":"input"}`)))
	if err != nil {
		t.Fatalf("activity: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("operator activity status=%d, want 403", resp.StatusCode)
	}
}

func TestOperatorEndpoints_GatewayForbidden(t *testing.T) {
	fb := &fakeBroker{revokedLeases: 2, openStreams: 1}
	e := newEnv(t, fb)
	gw := e.pki.issue(t, "gw-1", nil, false)

	resp, err := e.client(t, &gw).Post(
		e.srv.URL+"/internal/v1/broker/workspaces/ws-1/revoke",
		"application/json", bytes.NewReader([]byte(`{"runtimeGeneration":3}`)))
	if err != nil {
		t.Fatalf("revoke: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("gateway revoke status=%d, want 403", resp.StatusCode)
	}

	resp, err = e.client(t, &gw).Get(e.srv.URL + "/internal/v1/broker/workspaces/ws-1/drain")
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("gateway drain status=%d, want 403", resp.StatusCode)
	}

	// And the operator cert cannot redeem (vice-versa check).
	op := e.pki.issue(t, httpapi.DefaultOperatorCN, nil, false)
	resp, err = e.client(t, &op).Post(e.srv.URL+"/internal/v1/broker/redeem",
		"application/json", bytes.NewReader([]byte(`{"ticket":"x"}`)))
	if err != nil {
		t.Fatalf("redeem: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("operator redeem status=%d, want 403", resp.StatusCode)
	}
}

func TestRevokeWorkspace_OK(t *testing.T) {
	fb := &fakeBroker{revokedLeases: 2}
	e := newEnv(t, fb)
	op := e.pki.issue(t, httpapi.DefaultOperatorCN, nil, false)
	resp, err := e.client(t, &op).Post(
		e.srv.URL+"/internal/v1/broker/workspaces/ws-1/revoke",
		"application/json", bytes.NewReader([]byte(`{"runtimeGeneration":3}`)))
	if err != nil {
		t.Fatalf("revoke: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("revoke status=%d, want 200", resp.StatusCode)
	}
	var out struct {
		RevokedLeases int `json:"revokedLeases"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.RevokedLeases != 2 || fb.gotWsUID != "ws-1" || fb.gotWsGen != 3 {
		t.Fatalf("revoke: n=%d uid=%q gen=%d", out.RevokedLeases, fb.gotWsUID, fb.gotWsGen)
	}
}

func TestDrainWorkspace_OK(t *testing.T) {
	fb := &fakeBroker{openStreams: 1, drained: false}
	e := newEnv(t, fb)
	op := e.pki.issue(t, httpapi.DefaultOperatorCN, nil, false)
	resp, err := e.client(t, &op).Get(e.srv.URL + "/internal/v1/broker/workspaces/ws-1/drain")
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("drain status=%d, want 200", resp.StatusCode)
	}
	var out struct {
		OpenStreams int  `json:"openStreams"`
		Drained     bool `json:"drained"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.OpenStreams != 1 || out.Drained {
		t.Fatalf("drain: %+v", out)
	}
}

// TestNoLeaseIDsInLogs (SEC-41): a lease id is bearer material on this
// listener — lease id + any gateway-CN cert resolves runtime credentials —
// so the request log must carry the matched route pattern plus a truncated
// hash of the id, never the raw id itself.
func TestNoLeaseIDsInLogs(t *testing.T) {
	fb := &fakeBroker{renewLease: sampleLease}
	e := newEnv(t, fb)
	cert := e.pki.issue(t, "gw-1", nil, false)
	resp, err := e.client(t, &cert).Post(
		e.srv.URL+"/internal/v1/broker/leases/lease-abc/renew",
		"application/json",
		bytes.NewReader([]byte(`{"fence":{"workspaceUID":"ws-1","runtimeGeneration":3,"runtimeUID":"rt-3","fencingVersion":2}}`)))
	if err != nil {
		t.Fatalf("renew: %v", err)
	}
	resp.Body.Close()
	resp2, err := e.client(t, &cert).Get(e.srv.URL + "/internal/v1/broker/leases/lease-abc/target")
	if err != nil {
		t.Fatalf("target: %v", err)
	}
	resp2.Body.Close()

	logs := e.logs.String()
	if strings.Contains(logs, "lease-abc") {
		t.Fatalf("raw lease id leaked into request logs: %s", logs)
	}
	if !strings.Contains(logs, "/internal/v1/broker/leases/{id}") {
		t.Fatalf("request log lost the route pattern: %s", logs)
	}
}
