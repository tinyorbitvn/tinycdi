package main

// gatewayCN regression tests (design-review finding): the identity derivation
// must read the PEM certificate only — it must not require the file to
// also be a valid private key (tls.LoadX509KeyPair(certFile, certFile)
// fails on a cert-only PEM, so the gateway only started with -gateway-id).

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeTestCert generates a self-signed certificate with the given CN and
// writes it as PEM. It returns the cert PEM bytes and the PKCS8 key PEM.
func writeTestCert(t *testing.T, cn string) (certPEM, keyPEM []byte) {
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
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
}

func TestGatewayCN_FromCertOnlyPEM(t *testing.T) {
	certPEM, _ := writeTestCert(t, "gw-edge-1")
	f := filepath.Join(t.TempDir(), "tls.crt")
	if err := os.WriteFile(f, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	cn, err := gatewayCN(f)
	if err != nil {
		t.Fatalf("gatewayCN on a cert-only PEM: %v", err)
	}
	if cn != "gw-edge-1" {
		t.Fatalf("CN = %q, want gw-edge-1", cn)
	}
}

func TestGatewayCN_FromCombinedPEM(t *testing.T) {
	certPEM, keyPEM := writeTestCert(t, "gw-edge-2")
	f := filepath.Join(t.TempDir(), "tls.crt")
	if err := os.WriteFile(f, append(certPEM, keyPEM...), 0o600); err != nil {
		t.Fatal(err)
	}
	cn, err := gatewayCN(f)
	if err != nil {
		t.Fatalf("gatewayCN on a cert+key PEM: %v", err)
	}
	if cn != "gw-edge-2" {
		t.Fatalf("CN = %q, want gw-edge-2", cn)
	}
}

// TestStringList: -portal-origin is repeatable and each value may itself
// be comma-separated; blanks are dropped.
func TestStringList(t *testing.T) {
	var l stringList
	for _, v := range []string{
		"https://portal.example.dev",
		"https://p2.example.dev, https://p3.example.dev:8443",
		" ,",
	} {
		if err := l.Set(v); err != nil {
			t.Fatalf("Set(%q): %v", v, err)
		}
	}
	want := stringList{
		"https://portal.example.dev",
		"https://p2.example.dev",
		"https://p3.example.dev:8443",
	}
	if len(l) != len(want) {
		t.Fatalf("stringList = %v, want %v", l, want)
	}
	for i := range want {
		if l[i] != want[i] {
			t.Fatalf("stringList[%d] = %q, want %q", i, l[i], want[i])
		}
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

// TestPublicServerTimeouts (SEC-23): the session listener must bound header
// parsing, keep-alive idleness and header size — but Read/WriteTimeout must
// stay zero. Go applies them as absolute conn deadlines that survive
// Hijack() and would kill long-lived WebSocket desktop streams.
func TestPublicServerTimeouts(t *testing.T) {
	srv := publicServer(":0", nil)
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

// TestControlTokenUnsetWarning (SEC-I2): with no token configured the
// /v1/control surface fails CLOSED — the startup warning must say it is
// disabled, not claim it is unauthenticated.
func TestControlTokenUnsetWarning(t *testing.T) {
	if strings.Contains(controlTokenUnsetWarning, "UNAUTHENTICATED") {
		t.Fatalf("warning claims control endpoints are unauthenticated: %q", controlTokenUnsetWarning)
	}
	if !strings.Contains(controlTokenUnsetWarning, "disabled") {
		t.Fatalf("warning must state /v1/control is disabled: %q", controlTokenUnsetWarning)
	}
}
