// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package backend

// Listener-level tests: per-listener timeout budgets (SEC-23), distinct TLS
// identities per listener (D7), hot reload on file rotation (D21), and the
// gateway-id-from-client-cert derivation carried over from cmd/gateway.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/tlsreload"
)

// writeTestCert generates a self-signed certificate with the given CN,
// valid for 127.0.0.1, and writes cert+key PEM files under dir.
func writeTestCert(t *testing.T, dir, cn string) (certFile, keyFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	certFile = filepath.Join(dir, cn+".crt")
	keyFile = filepath.Join(dir, cn+".key")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile
}

// serveTLSOn binds addr (":0" allowed), wraps the listener in the reloader's
// certificate and serves h until the test ends. It returns the bound
// address.
func serveTLSOn(t *testing.T, addr string, h http.Handler, rel *tlsreload.Reloader) string {
	t.Helper()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := sessionServer(addr, h)
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: rel.GetCertificate}
	go func() { _ = serveTLS(srv, ln, tlsCfg) }()
	t.Cleanup(func() { _ = srv.Close() })
	return ln.Addr().String()
}

// handshakeCN dials addr over TLS (no verification) and returns the peer
// leaf certificate's CN.
func handshakeCN(t *testing.T, addr string) string {
	t.Helper()
	conn, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true}) // test only
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	defer conn.Close()
	return conn.ConnectionState().PeerCertificates[0].Subject.CommonName
}

// TestServerTimeouts (SEC-23): the app, internal and metrics listeners bound
// every request phase; none of them carries hijacked connections.
func TestServerTimeouts(t *testing.T) {
	for name, srv := range map[string]*http.Server{
		"app":      appServer(":0", nil),
		"internal": internalServer(":0", nil),
		"metrics":  metricsServer(":0", nil),
	} {
		if srv.ReadHeaderTimeout <= 0 || srv.ReadTimeout <= 0 ||
			srv.WriteTimeout <= 0 || srv.IdleTimeout <= 0 {
			t.Fatalf("%s server: all of ReadHeader/Read/Write/Idle timeouts must be > 0", name)
		}
		if srv.MaxHeaderBytes <= 0 {
			t.Fatalf("%s server: MaxHeaderBytes must be > 0", name)
		}
	}
}

// TestSessionServerTimeouts (SEC-23): the session listener bounds header
// parsing, keep-alive idleness and header size — but Read/WriteTimeout must
// stay zero. Go applies them as absolute conn deadlines that survive
// Hijack() and would kill long-lived WebSocket desktop streams.
func TestSessionServerTimeouts(t *testing.T) {
	srv := sessionServer(":0", nil)
	if srv.ReadHeaderTimeout <= 0 {
		t.Fatal("ReadHeaderTimeout must be > 0")
	}
	if srv.IdleTimeout <= 0 {
		t.Fatal("IdleTimeout must be > 0")
	}
	if srv.MaxHeaderBytes <= 0 {
		t.Fatal("MaxHeaderBytes must be > 0")
	}
	if srv.ReadTimeout != 0 || srv.WriteTimeout != 0 {
		t.Fatalf("Read/WriteTimeout must stay 0 for hijacked WebSockets, got %v/%v",
			srv.ReadTimeout, srv.WriteTimeout)
	}
}

// TestListeners_SeparateTLS: each listener presents the certificate
// configured for it — three distinct CNs on three listeners (D7).
func TestListeners_SeparateTLS(t *testing.T) {
	dir := t.TempDir()
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	for _, cn := range []string{"app.test", "session.test", "internal.test"} {
		cf, kf := writeTestCert(t, dir, cn)
		rel, err := tlsreload.New(cf, kf)
		if err != nil {
			t.Fatalf("reloader %s: %v", cn, err)
		}
		addr := serveTLSOn(t, "127.0.0.1:0", h, rel)
		if got := handshakeCN(t, addr); got != cn {
			t.Fatalf("listener for %s presented CN %q", cn, got)
		}
	}
}

// TestListeners_HotReload (D21): rotating the session listener's cert files
// makes new handshakes present the new CN; a connection opened before the
// rotation still works.
func TestListeners_HotReload(t *testing.T) {
	dir := t.TempDir()
	cf, kf := writeTestCert(t, dir, "session-old.test")
	rel, err := tlsreload.New(cf, kf, tlsreload.WithInterval(25*time.Millisecond))
	if err != nil {
		t.Fatalf("reloader: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go rel.Run(ctx)

	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	addr := serveTLSOn(t, "127.0.0.1:0", h, rel)

	// Connection opened before rotation.
	conn, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true}) // test only
	if err != nil {
		t.Fatalf("pre-rotation dial: %v", err)
	}
	defer conn.Close()
	if got := conn.ConnectionState().PeerCertificates[0].Subject.CommonName; got != "session-old.test" {
		t.Fatalf("pre-rotation CN = %q", got)
	}

	// Rotate the cert files in place (same paths, new keypair).
	ncf, nkf := writeTestCert(t, t.TempDir(), "session-new.test")
	copyFile(t, ncf, cf)
	copyFile(t, nkf, kf)

	deadline := time.Now().Add(5 * time.Second)
	for {
		if handshakeCN(t, addr) == "session-new.test" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("new handshake still presents the old certificate")
		}
		time.Sleep(25 * time.Millisecond)
	}

	// The pre-rotation connection still answers requests.
	if _, err := io.WriteString(conn, "GET /healthz HTTP/1.1\r\nHost: session.test\r\n\r\n"); err != nil {
		t.Fatalf("write on old conn: %v", err)
	}
	buf := make([]byte, 64)
	if _, err := conn.Read(buf); err != nil {
		t.Fatalf("read on old conn: %v", err)
	}
	if !strings.HasPrefix(string(buf), "HTTP/1.1 200") {
		t.Fatalf("old conn response: %q", strings.TrimSpace(string(buf)))
	}
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// gatewayCN regression tests (design-review finding): the identity
// derivation must read the PEM certificate only — it must not require the
// file to also be a valid private key.
func TestGatewayCN_FromCertOnlyPEM(t *testing.T) {
	dir := t.TempDir()
	cf, _ := writeTestCert(t, dir, "gw-edge-1")
	cn, err := gatewayCN(cf)
	if err != nil {
		t.Fatalf("gatewayCN on a cert-only PEM: %v", err)
	}
	if cn != "gw-edge-1" {
		t.Fatalf("CN = %q, want gw-edge-1", cn)
	}
}

func TestGatewayCN_FromCombinedPEM(t *testing.T) {
	dir := t.TempDir()
	cf, kf := writeTestCert(t, dir, "gw-edge-2")
	certPEM, err := os.ReadFile(cf)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM, err := os.ReadFile(kf)
	if err != nil {
		t.Fatal(err)
	}
	combined := filepath.Join(dir, "combined.pem")
	if err := os.WriteFile(combined, append(certPEM, keyPEM...), 0o600); err != nil {
		t.Fatal(err)
	}
	cn, err := gatewayCN(combined)
	if err != nil {
		t.Fatalf("gatewayCN on a cert+key PEM: %v", err)
	}
	if cn != "gw-edge-2" {
		t.Fatalf("CN = %q, want gw-edge-2", cn)
	}
}

func TestGatewayCN_Errors(t *testing.T) {
	if _, err := gatewayCN(filepath.Join(t.TempDir(), "missing.crt")); err == nil {
		t.Fatal("missing file must error")
	}
	f := filepath.Join(t.TempDir(), "garbage.pem")
	if err := os.WriteFile(f, []byte("not a pem file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := gatewayCN(f); err == nil {
		t.Fatal("non-PEM file must error")
	}
}
