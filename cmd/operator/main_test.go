package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"flag"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseWatchNamespaces(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want []string
	}{
		{"empty", "", nil},
		{"single", "tcdi-system", []string{"tcdi-system"}},
		{"csv with spaces", "tcdi-system, tcdi-tenant-a ,tcdi-tenant-b",
			[]string{"tcdi-system", "tcdi-tenant-a", "tcdi-tenant-b"}},
		{"trailing comma", "a,,", []string{"a"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := parseWatchNamespaces(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v entries, want %d", got, len(tc.want))
			}
			for _, ns := range tc.want {
				if _, ok := got[ns]; !ok {
					t.Fatalf("missing namespace %q in %v", ns, got)
				}
			}
		})
	}
}

// writeBrokerPKI emits a self-signed cert usable as both the broker CA and
// the operator client identity for opclient construction tests.
func writeBrokerPKI(t *testing.T, cn string) (caFile, certFile, keyFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	dir := t.TempDir()
	caFile = filepath.Join(dir, "ca.crt")
	certFile = filepath.Join(dir, "tls.crt")
	keyFile = filepath.Join(dir, "tls.key")
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	for f, b := range map[string][]byte{
		caFile:   certPEM,
		certFile: certPEM,
		keyFile:  pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
	} {
		if err := os.WriteFile(f, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return caFile, certFile, keyFile
}

func TestBindBrokerFlags_EnvFallback(t *testing.T) {
	env := map[string]string{
		"TCDI_BROKER_INTERNAL_URL":     "https://api-internal:9443",
		"TCDI_BROKER_CA_FILE":          "/ca",
		"TCDI_BROKER_CLIENT_CERT_FILE": "/cert",
		"TCDI_BROKER_CLIENT_KEY_FILE":  "/key",
		"TCDI_DEV_ALLOW_NO_BROKER":     "true",
	}
	getenv := func(k string) string { return env[k] }
	var bf brokerFlags
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	bindBrokerFlags(fs, &bf, getenv)
	if err := fs.Parse([]string{}); err != nil {
		t.Fatal(err)
	}
	if bf.internalURL != "https://api-internal:9443" || bf.caFile != "/ca" ||
		bf.certFile != "/cert" || bf.keyFile != "/key" || !bf.devAllowNoBroker {
		t.Fatalf("env fallback not applied: %+v", bf)
	}

	// explicit flags win over env
	if err := fs.Parse([]string{"--broker-internal-url=https://other:1"}); err != nil {
		t.Fatal(err)
	}
	if bf.internalURL != "https://other:1" {
		t.Fatalf("flag did not override env: %q", bf.internalURL)
	}
}

func TestBrokerSeam_ProductionRequiresURL(t *testing.T) {
	_, err := brokerSeam(brokerFlags{})
	if err == nil {
		t.Fatal("production mode with no broker URL must fail fast (finalizer seam nil)")
	}
}

func TestBrokerSeam_DevAllowNoBroker(t *testing.T) {
	c, err := brokerSeam(brokerFlags{devAllowNoBroker: true})
	if err != nil {
		t.Fatalf("dev-allow-no-broker must not error: %v", err)
	}
	if c != nil {
		t.Fatal("dev-allow-no-broker must yield a nil client (standalone seams)")
	}
}

func TestBrokerSeam_RequiresAllFiles(t *testing.T) {
	for _, bf := range []brokerFlags{
		{internalURL: "https://api-internal:9443"},
		{internalURL: "https://api-internal:9443", caFile: "/ca"},
		{internalURL: "https://api-internal:9443", caFile: "/ca", certFile: "/cert"},
	} {
		if _, err := brokerSeam(bf); err == nil {
			t.Fatalf("incomplete cert material must fail: %+v", bf)
		}
	}
}

func TestBrokerSeam_ValidClient(t *testing.T) {
	ca, cert, key := writeBrokerPKI(t, "operator")
	c, err := brokerSeam(brokerFlags{
		internalURL: "https://api-internal.tcdi-system.svc:9443",
		caFile:      ca, certFile: cert, keyFile: key,
	})
	if err != nil {
		t.Fatalf("valid broker config failed: %v", err)
	}
	if c == nil {
		t.Fatal("expected a constructed opclient")
	}
}

func TestBrokerSeam_BadURL(t *testing.T) {
	ca, cert, key := writeBrokerPKI(t, "operator")
	for _, u := range []string{"http://insecure:9443", "notaurl", "https://"} {
		if _, err := brokerSeam(brokerFlags{
			internalURL: u, caFile: ca, certFile: cert, keyFile: key,
		}); err == nil {
			t.Fatalf("url %q must be rejected", u)
		}
	}
}

// SEC-02: the except list lands verbatim in an ipBlock under 0.0.0.0/0 —
// a malformed or IPv6 entry would make every boundary policy unappliable,
// so bad input is rejected at startup, not at first workspace.
func TestParseExceptCIDRs(t *testing.T) {
	for _, tc := range []struct {
		name    string
		in      string
		want    []string
		wantErr bool
	}{
		{"empty", "", nil, false},
		{"single", "10.0.0.0/8", []string{"10.0.0.0/8"}, false},
		{"csv", "10.0.0.0/8, 100.64.0.0/10 ,172.16.0.0/12",
			[]string{"10.0.0.0/8", "100.64.0.0/10", "172.16.0.0/12"}, false},
		{"normalized", "010.0.0.0/8", nil, true}, // leading zeros rejected by netip
		{"not a cidr", "abc", nil, true},
		{"bare ip", "10.1.2.3", nil, true},
		{"ipv6", "fd00::/8", nil, true},
		{"trailing comma", "10.0.0.0/8,", []string{"10.0.0.0/8"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseExceptCIDRs(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("input %q must be rejected, got %v", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("input %q: %v", tc.in, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}
