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
	"os"
	"path/filepath"
	"sync/atomic"
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
	if server {
		return p.issueSAN(t, cn, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
			nil, []net.IP{net.ParseIP("127.0.0.1")})
	}
	return p.issueSAN(t, cn, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, nil, nil)
}

// issueSAN issues a leaf with explicit SANs so tests can control the
// hostname the client must match.
func (p *pki) issueSAN(t *testing.T, cn string, eku []x509.ExtKeyUsage, dns []string, ips []net.IP) tls.Certificate {
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
		ExtKeyUsage:  eku,
		DNSNames:     dns,
		IPAddresses:  ips,
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

// TestOpClient_PresentsRotatedCert (E5): rotating the client cert files
// makes the next request on a fresh connection present the new
// certificate, with no client restart.
func TestOpClient_PresentsRotatedCert(t *testing.T) {
	p := newPKI(t)
	var seen atomic.Value // peer leaf serial, recorded per request
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(r.TLS.PeerCertificates) > 0 {
			seen.Store(r.TLS.PeerCertificates[0].SerialNumber.String())
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"openStreams":0,"drained":true}`))
	}))
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{p.issue(t, "broker-internal", true)},
		ClientAuth:   tls.VerifyClientCertIfGiven,
		ClientCAs:    p.pool,
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	certFile := filepath.Join(dir, "tls.crt")
	keyFile := filepath.Join(dir, "tls.key")
	caFile := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: p.caCert.Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	certA := p.issue(t, httpapi.DefaultOperatorCN, false)
	writeClientPair(t, certFile, keyFile, certA)

	c, err := opclient.New(opclient.Config{
		BaseURL:        srv.URL,
		CertFile:       certFile,
		KeyFile:        keyFile,
		CAFile:         caFile,
		ReloadInterval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("opclient.New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go c.Run(ctx)

	if _, _, err := c.DrainStatus(context.Background(), "ws-1"); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if got, want := seen.Load(), leafSerial(t, certA); got != want {
		t.Fatalf("initial peer serial = %v, want %v (certA)", got, want)
	}

	certB := p.issue(t, httpapi.DefaultOperatorCN, false)
	serialB := leafSerial(t, certB)
	writeClientPair(t, certFile, keyFile, certB)

	deadline := time.Now().Add(5 * time.Second)
	for {
		// Keep-alive conns keep their handshake identity, so a new
		// request on a fresh connection is what observes the rotation.
		srv.CloseClientConnections()
		if _, _, err := c.DrainStatus(context.Background(), "ws-1"); err != nil {
			t.Fatalf("drain after rotation: %v", err)
		}
		if seen.Load() == serialB {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("new connections still present the pre-rotation certificate")
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// writeClientPair PEM-encodes cert and writes cert+key via temp-file +
// rename so the reloader never observes a partial file.
func writeClientPair(t *testing.T, certFile, keyFile string, cert tls.Certificate) {
	t.Helper()
	var certPEM []byte
	for _, der := range cert.Certificate {
		certPEM = append(certPEM, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}
	keyDER, err := x509.MarshalECPrivateKey(cert.PrivateKey.(*ecdsa.PrivateKey))
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	writeAtomic(t, certFile, certPEM)
	writeAtomic(t, keyFile, keyPEM)
}

// writeAtomic replaces path with data via temp-file + rename, like a
// Kubernetes Secret volume swap.
func writeAtomic(t *testing.T, path string, data []byte) {
	t.Helper()
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tmp.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := tmp.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		t.Fatal(err)
	}
}

// leafSerial returns the serial of cert's leaf certificate.
func leafSerial(t *testing.T, cert tls.Certificate) string {
	t.Helper()
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	return leaf.SerialNumber.String()
}
