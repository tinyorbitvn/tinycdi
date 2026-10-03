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
	"path/filepath"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/tlsreload"
)

func TestCAPool_ReloadsOnChange(t *testing.T) {
	dir := t.TempDir()
	caFile := filepath.Join(dir, "ca.pem")
	first := caCertPEM(t, "ca-one.test")
	writeRename(t, caFile, first)
	p, err := tlsreload.NewCAPool(caFile, tlsreload.WithInterval(10*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	if got := len(p.Pool().Subjects()); got != 1 {
		t.Fatalf("initial subjects = %d, want 1", got)
	}
	writeRename(t, caFile, append(first, caCertPEM(t, "ca-two.test")...))
	eventually(t, time.Second, func() bool { return len(p.Pool().Subjects()) == 2 })
}

func TestCAPool_KeepsOldOnBadFile(t *testing.T) {
	dir := t.TempDir()
	caFile := filepath.Join(dir, "ca.pem")
	writeRename(t, caFile, caCertPEM(t, "ca-one.test"))
	p, err := tlsreload.NewCAPool(caFile, tlsreload.WithInterval(10*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	writeRename(t, caFile, []byte("not a certificate"))
	time.Sleep(100 * time.Millisecond)
	if got := len(p.Pool().Subjects()); got != 1 {
		t.Fatalf("subjects after bad rotation = %d, want 1", got)
	}
}

// caCertPEM returns a self-signed CA certificate PEM with the given CN.
func caCertPEM(t *testing.T, cn string) []byte {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}
