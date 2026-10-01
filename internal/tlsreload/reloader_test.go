// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package tlsreload_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/tlsreload"
)

func TestReloader_ServesNewCertAfterRotation(t *testing.T) {
	dir := t.TempDir()
	cert, key := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	writePair(t, cert, key, "first.example")
	r, err := tlsreload.New(cert, key, tlsreload.WithInterval(10*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)

	if got := leafCN(t, r); got != "first.example" {
		t.Fatalf("initial CN = %q", got)
	}
	writePair(t, cert, key, "second.example")
	eventually(t, time.Second, func() bool { return leafCN(t, r) == "second.example" })
}

func TestReloader_KeepsOldCertOnBadRotation(t *testing.T) {
	dir := t.TempDir()
	cert, key := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	writePair(t, cert, key, "good.example")
	r, _ := tlsreload.New(cert, key, tlsreload.WithInterval(10*time.Millisecond))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)

	if err := os.WriteFile(cert, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if got := leafCN(t, r); got != "good.example" {
		t.Fatalf("CN after bad rotation = %q, want good.example", got)
	}
}

func TestNew_FailsOnMissingFiles(t *testing.T) {
	if _, err := tlsreload.New("/nonexistent.crt", "/nonexistent.key"); err == nil {
		t.Fatal("want error")
	}
}

// writePair generates a self-signed ECDSA P-256 pair with the given CN and
// writes both files via temp-file + rename.
func writePair(t *testing.T, certFile, keyFile, cn string) {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	writeRename(t, certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	writeRename(t, keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
}

func writeRename(t *testing.T, path string, data []byte) {
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

// leafCN returns the CommonName of the leaf certificate the reloader serves.
func leafCN(t *testing.T, r *tlsreload.Reloader) string {
	t.Helper()
	c, err := r.GetCertificate(nil)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	return leaf.Subject.CommonName
}

// eventually polls cond every 5 ms until it returns true or the deadline
// passes, then fails the test.
func eventually(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met before deadline")
}
