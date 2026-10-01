package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"flag"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

// --kasm-adapter-image is operator-supplied infrastructure injected into
// runtime pods: a mutable (tag-only) reference would let an image swap
// change what runs inside every adapter=kasm workspace, so only a
// digest-pinned ref is accepted.
func TestParseKasmAdapterImage(t *testing.T) {
	digest := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	for _, tc := range []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{"empty stays empty", "", "", false},
		{"whitespace stays empty", "   ", "", false},
		{"digest-pinned", "ghcr.io/tinyorbitvn/tinycdi-kasm-adapter@sha256:" + digest,
			"ghcr.io/tinyorbitvn/tinycdi-kasm-adapter@sha256:" + digest, false},
		{"single-segment repo", "tinycdi-kasm-adapter@sha256:" + digest,
			"tinycdi-kasm-adapter@sha256:" + digest, false},
		{"tag only", "ghcr.io/tinyorbitvn/tinycdi-kasm-adapter:1.2.3", "", true},
		{"truncated digest", "ghcr.io/tinyorbitvn/x@sha256:abc", "", true},
		{"uppercase repo", "GHCR.io/tinyorbitvn/x@sha256:" + digest, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseKasmAdapterImage(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("input %q must be rejected, got %q", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("input %q: %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// The --runtime-* flags carry the operator-wide runtime pod placement
// defaults (the chart's runtime.placement values). Valid JSON must parse
// into the placement the linux backend receives; invalid JSON must make
// the operator exit non-zero and name the offending flag — a silently
// dropped selector on a dedicated pool would strand every pod Pending.
func TestOperatorFlags_Placement(t *testing.T) {
	// Helper-process pattern: re-exec the test binary so the real exit
	// code is observable — main() turns a parse error into os.Exit(1),
	// which cannot be invoked in-process.
	if os.Getenv("TCDI_TEST_PLACEMENT_HELPER") == "1" {
		var pf runtimePlacementFlags
		fs := flag.NewFlagSet("operator", flag.ContinueOnError)
		bindRuntimePlacementFlags(fs, &pf)
		if err := fs.Parse(flag.Args()); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		if _, err := pf.parse(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}

	t.Run("valid JSON parses", func(t *testing.T) {
		var pf runtimePlacementFlags
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		bindRuntimePlacementFlags(fs, &pf)
		err := fs.Parse([]string{
			`--runtime-node-selector={"cdi.tinyorbit.vn/workspace":"true"}`,
			`--runtime-tolerations=[{"key":"cdi.tinyorbit.vn/workspace","operator":"Exists","effect":"NoSchedule"}]`,
			"--runtime-host-users=false",
		})
		if err != nil {
			t.Fatalf("flag parse: %v", err)
		}
		p, err := pf.parse()
		if err != nil {
			t.Fatalf("valid flags rejected: %v", err)
		}
		if got := p.nodeSelector["cdi.tinyorbit.vn/workspace"]; got != "true" {
			t.Fatalf("nodeSelector[cdi.tinyorbit.vn/workspace] = %q, want true", got)
		}
		if len(p.tolerations) != 1 {
			t.Fatalf("tolerations = %v, want 1 entry", p.tolerations)
		}
		tol := p.tolerations[0]
		if tol.Key != "cdi.tinyorbit.vn/workspace" || tol.Operator != "Exists" || tol.Effect != "NoSchedule" {
			t.Fatalf("toleration = %+v", tol)
		}
		if p.hostUsers == nil || *p.hostUsers != false {
			t.Fatalf("hostUsers = %v, want *false", p.hostUsers)
		}
	})

	t.Run("empty flags leave defaults unset", func(t *testing.T) {
		p, err := (runtimePlacementFlags{}).parse()
		if err != nil {
			t.Fatal(err)
		}
		if p.nodeSelector != nil || p.tolerations != nil || p.hostUsers != nil {
			t.Fatalf("empty flags must yield empty placement, got %+v", p)
		}
	})

	t.Run("hostUsers values", func(t *testing.T) {
		for _, tc := range []struct {
			in   string
			want *bool
		}{
			{"", nil},
			{"true", ptr(true)},
			{"false", ptr(false)},
		} {
			p, err := (runtimePlacementFlags{hostUsers: tc.in}).parse()
			if err != nil {
				t.Fatalf("hostUsers=%q: %v", tc.in, err)
			}
			if (p.hostUsers == nil) != (tc.want == nil) ||
				(p.hostUsers != nil && *p.hostUsers != *tc.want) {
				t.Fatalf("hostUsers=%q parsed %v, want %v", tc.in, p.hostUsers, tc.want)
			}
		}
	})

	// Invalid input: the operator process exits non-zero and the message
	// names the flag (helper-process re-exec).
	t.Run("invalid input exits non-zero naming the flag", func(t *testing.T) {
		for _, args := range [][]string{
			{`--runtime-node-selector=[not-an-object`},
			{`--runtime-node-selector={"a":1}`},               // non-string value
			{`--runtime-tolerations={"not":"array"}`},          // not an array
			{`--runtime-tolerations=[{"operator":"Exists"}]`},  // empty key tolerates all
			{`--runtime-tolerations=[{"key":"k","operator":"Sometimes"}]`},
			{`--runtime-tolerations=[{"key":"k","effect":"Never"}]`},
			{"--runtime-host-users=maybe"},
		} {
			cmd := exec.Command(os.Args[0],
				"-test.run=^TestOperatorFlags_Placement$", "--")
			cmd.Args = append(cmd.Args, args...)
			cmd.Env = append(os.Environ(), "TCDI_TEST_PLACEMENT_HELPER=1")
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("args %v: expected non-zero exit, got 0\n%s", args, out)
			}
			wantFlag := "--" + strings.SplitN(strings.TrimPrefix(args[0], "--"), "=", 2)[0]
			if !strings.Contains(string(out), wantFlag) {
				t.Fatalf("args %v: output must name %s, got\n%s", args, wantFlag, out)
			}
		}
	})

	t.Run("valid input exits zero", func(t *testing.T) {
		cmd := exec.Command(os.Args[0],
			"-test.run=^TestOperatorFlags_Placement$", "--",
			`--runtime-node-selector={"pool":"ws"}`,
			`--runtime-tolerations=[{"key":"pool","operator":"Equal","value":"ws","effect":"NoExecute"}]`)
		cmd.Env = append(os.Environ(), "TCDI_TEST_PLACEMENT_HELPER=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("valid flags must exit 0: %v\n%s", err, out)
		}
	})
}

func ptr[T any](v T) *T { return &v }
