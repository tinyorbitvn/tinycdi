package opclient_test

// Contract tests for the operator-side client: real mTLS against the real
// httpapi handler (stubbed broker), covering the revoke/drain wire shapes
// and the gateway-vs-operator identity split.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/broker"
	"github.com/tinyorbitvn/tinycdi/internal/broker/httpapi"
	"github.com/tinyorbitvn/tinycdi/internal/broker/httpapi/opclient"
)

// stubBroker implements httpapi.BrokerAPI with canned answers.
type stubBroker struct {
	revokedLeases int
	openStreams   int
	drained       bool
	gotUID        broker.PlatformID
	gotGen        uint64
}

func (s *stubBroker) RedeemTicket(context.Context, broker.GatewayIdentity, string) (broker.Lease, error) {
	return broker.Lease{}, broker.ErrNotFound
}
func (s *stubBroker) RenewLease(context.Context, broker.GatewayIdentity, string, broker.Fence) (broker.Lease, error) {
	return broker.Lease{}, broker.ErrLeaseInvalid
}
func (s *stubBroker) ResolveTarget(context.Context, broker.GatewayIdentity, string) (broker.Target, error) {
	return broker.Target{}, broker.ErrLeaseInvalid
}
func (s *stubBroker) RevokeLease(context.Context, string) error                { return nil }
func (s *stubBroker) RevokeLeaseChanged(context.Context, string) (bool, error) { return false, nil }
func (s *stubBroker) ReportActivity(context.Context, broker.GatewayIdentity, string, broker.Fence, broker.ActivityEvent) error {
	return nil
}
func (s *stubBroker) RevokeWorkspaceLeases(_ context.Context, uid broker.PlatformID, gen uint64) (int, error) {
	s.gotUID, s.gotGen = uid, gen
	return s.revokedLeases, nil
}
func (s *stubBroker) DrainStatus(_ context.Context, uid broker.PlatformID) (int, bool, error) {
	s.gotUID = uid
	return s.openStreams, s.drained, nil
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

func (p *pki) issue(t *testing.T, cn string, server bool) tls.Certificate {
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
		tmpl.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
	} else {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
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

// env wires the real mTLS handler over a stub broker.
type env struct {
	srv  *httptest.Server
	pki  *pki
	stub *stubBroker
}

func newEnv(t *testing.T, stub *stubBroker) *env {
	t.Helper()
	p := newPKI(t)
	srv := httptest.NewUnstartedServer(httpapi.NewHandler(httpapi.Config{
		Broker:   stub,
		Audience: "session.example.dev",
	}))
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{p.issue(t, "broker-internal", true)},
		ClientAuth:   tls.VerifyClientCertIfGiven,
		ClientCAs:    p.pool,
	}
	srv.StartTLS()
	e := &env{srv: srv, pki: p, stub: stub}
	t.Cleanup(srv.Close)
	return e
}

func (e *env) client(t *testing.T, cn string) *opclient.Client {
	t.Helper()
	cert := e.pki.issue(t, cn, false)
	c, err := opclient.New(opclient.Config{
		BaseURL: e.srv.URL,
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{cert},
			RootCAs:      e.pki.pool,
		},
	})
	if err != nil {
		t.Fatalf("opclient.New: %v", err)
	}
	return c
}

func TestRevokeAndDrain(t *testing.T) {
	stub := &stubBroker{revokedLeases: 2, openStreams: 1}
	e := newEnv(t, stub)
	c := e.client(t, httpapi.DefaultOperatorCN)

	n, err := c.RevokeAllForWorkspace(context.Background(), "ws-1", 7)
	if err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if n != 2 || stub.gotUID != "ws-1" || stub.gotGen != 7 {
		t.Fatalf("revoke: n=%d uid=%q gen=%d", n, stub.gotUID, stub.gotGen)
	}

	open, drained, err := c.DrainStatus(context.Background(), "ws-1")
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if open != 1 || drained {
		t.Fatalf("drain: open=%d drained=%v", open, drained)
	}
}

// TestOperatorSeams covers the operator-side call shapes
// (internal/operator.Finalizer's LeaseRevoker/StreamDrainer): the
// workspace UID and generation reach the wire endpoints unchanged.
func TestOperatorSeams(t *testing.T) {
	stub := &stubBroker{revokedLeases: 1, drained: true}
	e := newEnv(t, stub)
	c := e.client(t, httpapi.DefaultOperatorCN)

	n, err := c.RevokeAllForWorkspace(context.Background(), "ws-9", 12)
	if err != nil || n != 1 {
		t.Fatalf("RevokeAllForWorkspace: n=%d err=%v", n, err)
	}
	if stub.gotUID != "ws-9" || stub.gotGen != 12 {
		t.Fatalf("revoke uid=%q gen=%d", stub.gotUID, stub.gotGen)
	}
	open, drained, err := c.DrainStatus(context.Background(), "ws-9")
	if err != nil || open != 0 || !drained {
		t.Fatalf("DrainStatus: open=%d drained=%v err=%v", open, drained, err)
	}
}

// TestGatewayCertRejected: a gateway client cert gets 403 on the
// operator-only routes; the error carries the HTTP status.
func TestGatewayCertRejected(t *testing.T) {
	e := newEnv(t, &stubBroker{})
	c := e.client(t, "gw-1")
	_, err := c.RevokeAllForWorkspace(context.Background(), "ws-1", 1)
	if err == nil {
		t.Fatal("gateway cert revoked a workspace")
	}
	var oe *opclient.Error
	if !errors.As(err, &oe) || oe.Status != http.StatusForbidden {
		t.Fatalf("revoke err = %v, want 403", err)
	}
}
