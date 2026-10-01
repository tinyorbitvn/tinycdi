package chart_test

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
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestPGConnHonoursSSEnv proves the premise the chart's database.tls wiring
// relies on: the api connects via pgxpool.New(ctx, dsn) where dsn is a URL
// (postgres://user:pass@host/db), and pgconn.ParseConfig merges the libpq
// environment variables PGSSLMODE / PGSSLROOTCERT underneath the connString
// (pgconn/config.go: settings = merge(defaults, env, connString)). So a
// DSN that does NOT carry sslmode picks up the chart-rendered env, while a
// DSN that sets sslmode explicitly still wins.
func TestPGConnHonoursSSEnv(t *testing.T) {
	// self-signed CA PEM for PGSSLROOTCERT.
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-db-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	caFile := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PGSSLMODE", "verify-full")
	t.Setenv("PGSSLROOTCERT", caFile)

	cfg, err := pgxpool.ParseConfig("postgres://tcdi:secret@pg.lab.example.net:5432/tcdi")
	if err != nil {
		t.Fatalf("ParseConfig with PGSSLMODE/PGSSLROOTCERT env: %v", err)
	}
	cc := cfg.ConnConfig.Config
	if cc.TLSConfig == nil {
		t.Fatal("PGSSLMODE=verify-full must yield a TLS config for a URL DSN without sslmode")
	}
	if cc.TLSConfig.InsecureSkipVerify {
		t.Error("verify-full must not set InsecureSkipVerify")
	}
	if cc.TLSConfig.ServerName != "pg.lab.example.net" {
		t.Errorf("verify-full must pin ServerName to the DSN host, got %q", cc.TLSConfig.ServerName)
	}
	if cc.TLSConfig.RootCAs == nil {
		t.Error("PGSSLROOTCERT must populate RootCAs")
	}

	// An explicit DSN sslmode beats the env (connString wins over env).
	cfg2, err := pgconn.ParseConfig("postgres://tcdi:secret@pg.lab.example.net:5432/tcdi?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	if cfg2.TLSConfig != nil {
		t.Error("DSN sslmode=disable must override PGSSLMODE env")
	}
}
