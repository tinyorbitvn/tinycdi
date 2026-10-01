package main

// Regression tests for the session-origin flag (SEC-26) and the HTTP server
// timeout budget (SEC-23).

import (
	"crypto/tls"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func TestNormalizeSessionOrigin(t *testing.T) {
	ok := map[string]string{
		"https://session.example.dev":      "https://session.example.dev",
		"https://SESSION.Example.Dev:8443": "https://session.example.dev:8443",
		"https://session.example.dev:443":  "https://session.example.dev",
	}
	for in, want := range ok {
		got, err := normalizeSessionOrigin(in)
		if err != nil || got != want {
			t.Fatalf("normalizeSessionOrigin(%q) = %q, %v; want %q", in, got, err, want)
		}
	}

	for _, bad := range []string{
		"http://session.example.dev",       // plaintext must not carry tickets
		"javascript:alert(1)",              // non-http scheme
		"session.example.dev",              // missing scheme
		"https://session.example.dev/",     // path not allowed
		"https://session.example.dev/app",  // path not allowed
		"https://session.example.dev/?x=1", // query not allowed
		"https://session.example.dev#frag", // fragment not allowed
		"https://user@session.example.dev", // userinfo not allowed
		"https://user:pw@session.example.dev",
		"",
	} {
		if got, err := normalizeSessionOrigin(bad); err == nil {
			t.Fatalf("normalizeSessionOrigin(%q) = %q, want error", bad, got)
		}
	}
}

// TestServerTimeouts (SEC-23): both listeners bound every request phase and
// cap header size. The API serves no WebSocket or streaming endpoints, so
// Read/WriteTimeout must be non-zero here (unlike the gateway's public
// listener, where they must stay zero for hijacked desktop connections).
func TestServerTimeouts(t *testing.T) {
	for name, srv := range map[string]*http.Server{
		"public":   publicServer(":0", nil),
		"internal": internalServer(":0", nil, &tls.Config{}),
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

// --- -required-groups flag -------------------------------------------------

// The gate flag trims CSV entries and accumulates repeated flags, but
// rejects empty entries outright: a blank group name is always a typo and
// must not silently pass.
func TestGroupListValidation(t *testing.T) {
	var g groupList
	if err := g.Set("platform-admins, sec-ops"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := g.Set("devs"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	want := groupList{"platform-admins", "sec-ops", "devs"}
	if !reflect.DeepEqual(g, want) {
		t.Fatalf("groups = %v, want %v", g, want)
	}
	for _, bad := range []string{"", "a,,b", ",a", "a,", " , "} {
		var g groupList
		if err := g.Set(bad); err == nil {
			t.Fatalf("Set(%q) accepted an empty entry", bad)
		}
	}
}

// --- CHTR-8: refuse non-verifying PostgreSQL sslmodes ----------------------

// The api must not start unless the resolved pgx TLS config verifies the
// server certificate on every candidate connection. sslmode comes from the
// DSN (winning) or PGSSLMODE; neither set means libpq's "prefer" default,
// which is itself non-verifying.
func TestCheckDatabaseTLS(t *testing.T) {
	// Isolate ambient libpq env so the host cannot perturb the matrix.
	for _, k := range []string{"PGSSLMODE", "PGSSLROOTCERT", "PGSSLNEGOTIATION", "PGSERVICE", "PGSERVICEFILE"} {
		t.Setenv(k, "")
	}

	okDSN := "postgres://api:s3cr3t-pw@db.internal:5432/tinycdi"
	for _, tc := range []struct {
		name        string
		dsn         string
		env         string
		devInsecure bool
		wantErr     bool
	}{
		{"verify-full dsn", okDSN + "?sslmode=verify-full", "", false, false},
		{"verify-ca dsn", okDSN + "?sslmode=verify-ca", "", false, false},
		{"disable dsn", okDSN + "?sslmode=disable", "", false, true},
		{"allow dsn", okDSN + "?sslmode=allow", "", false, true},
		{"prefer dsn", okDSN + "?sslmode=prefer", "", false, true},
		{"require dsn", okDSN + "?sslmode=require", "", false, true},
		{"ssl=true alias", okDSN + "?ssl=true", "", false, true},
		{"unset defaults to prefer", okDSN, "", false, true},
		{"keyword verify-full", "host=db.internal user=api dbname=tinycdi sslmode=verify-full", "", false, false},
		{"keyword disable", "host=db.internal user=api dbname=tinycdi sslmode=disable", "", false, true},
		{"env verify-full", okDSN, "verify-full", false, false},
		{"env disable", okDSN, "disable", false, true},
		{"env require", okDSN, "require", false, true},
		{"dsn beats env secure", okDSN + "?sslmode=verify-full", "disable", false, false},
		{"dsn beats env insecure", okDSN + "?sslmode=disable", "verify-full", false, true},
		{"dev flag permits disable", okDSN + "?sslmode=disable", "", true, false},
		{"dev flag permits unset default", okDSN, "", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PGSSLMODE", tc.env)
			mode, err := checkDatabaseTLS(tc.dsn, tc.devInsecure)
			if tc.wantErr && err == nil {
				t.Fatalf("sslmode resolution passed: mode=%q, want refusal", mode)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("refused: %v", err)
			}
			// The DSN and anything in it (password, host) must never appear
			// in the refusal text.
			if err != nil && (strings.Contains(err.Error(), "s3cr3t-pw") ||
				strings.Contains(err.Error(), tc.dsn)) {
				t.Fatalf("error leaked DSN material: %v", err)
			}
		})
	}
}

// A malformed DSN is a refusal too, and the parse error must not be
// propagated — pgx's ParseConfigError embeds the (best-effort redacted)
// connection string.
func TestCheckDatabaseTLSMalformedDSN(t *testing.T) {
	for _, k := range []string{"PGSSLMODE"} {
		t.Setenv(k, "")
	}
	for _, dsn := range []string{
		"postgres://user:p w@:bad", // unparseable URL with secret-ish material
		"host=db.internal sslmode", // truncated keyword/value pair
		"not a dsn at all",
	} {
		if mode, err := checkDatabaseTLS(dsn, false); err == nil {
			t.Fatalf("dsn %q passed with mode %q", dsn, mode)
		} else if strings.Contains(err.Error(), dsn) {
			t.Fatalf("error echoed the DSN: %v", err)
		}
	}
}
