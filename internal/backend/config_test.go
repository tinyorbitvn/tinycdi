// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package backend

// Flag-surface tests for the merged backend binary (decisions-2 item 1).
// ParseFlags is pure: it takes args and an env lookup so tests never touch
// the process environment. pgconn still reads PG* variables itself, so the
// DB-TLS cases isolate those explicitly.

import (
	"reflect"
	"strings"
	"testing"
)

// noEnv is a getenv that answers empty for everything.
func noEnv(string) string { return "" }

// envMap builds a getenv over a fixed map.
func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// mergedArgs is the minimal flag set that satisfies every required-input
// check of merged mode (app + session + internal listeners all on their
// defaults). Tests mutate copies of it.
func mergedArgs() []string {
	return []string{
		"-session-origin", "https://session.example.test",
		"-database-url", "postgres://api:pw@db.internal:5432/tinycdi?sslmode=verify-full",
		"-oidc-issuer", "https://idp.example.test",
		"-oidc-client-id", "tinycdi",
		"-oidc-redirect-url", "https://portal.example.test/auth/callback",
		"-login-key-file", "/run/secrets/login.key",
		"-session-tls-cert", "/run/secrets/session.crt",
		"-session-tls-key", "/run/secrets/session.key",
		"-session-allowed-hosts", "session.example.test",
		"-internal-tls-cert", "/run/secrets/internal.crt",
		"-internal-tls-key", "/run/secrets/internal.key",
		"-internal-client-ca", "/run/secrets/clients.ca",
	}
}

// splitArgs is the minimal flag set for split/test mode: session listener
// fronting a remote broker over mTLS, no app/internal listener, no DB.
func splitArgs() []string {
	return []string{
		"-listen=", "-internal-listen=",
		"-session-origin", "https://session.example.test",
		"-session-tls-cert", "/run/secrets/session.crt",
		"-session-tls-key", "/run/secrets/session.key",
		"-session-allowed-hosts", "session.example.test",
		"-broker-url", "https://broker.internal:9443",
		"-broker-ca", "/run/secrets/broker.ca",
		"-mtls-cert", "/run/secrets/client.crt",
		"-mtls-key", "/run/secrets/client.key",
	}
}

// withArg replaces (or appends) a flag=value pair in args.
func withArg(args []string, flag, val string) []string {
	out := append([]string{}, args...)
	for i := 0; i < len(out)-1; i += 2 {
		if out[i] == flag {
			out[i+1] = val
			return out
		}
	}
	return append(out, flag, val)
}

// dropArg removes a flag/value pair from args.
func dropArg(args []string, flag string) []string {
	out := []string{}
	for i := 0; i < len(args); i++ {
		if args[i] == flag {
			i++ // skip the value too
			continue
		}
		out = append(out, args[i])
	}
	return out
}

func TestParseFlags_Defaults(t *testing.T) {
	cfg, err := ParseFlags(mergedArgs(), noEnv)
	if err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	if cfg.Listen != ":8443" {
		t.Fatalf("Listen = %q, want :8443", cfg.Listen)
	}
	if cfg.SessionListen != ":8444" {
		t.Fatalf("SessionListen = %q, want :8444", cfg.SessionListen)
	}
	if cfg.InternalListen != ":9443" {
		t.Fatalf("InternalListen = %q, want :9443", cfg.InternalListen)
	}
	if cfg.MetricsListen != "" {
		t.Fatalf("MetricsListen = %q, want empty (disabled)", cfg.MetricsListen)
	}
	if cfg.SessionCookieMode != "lax" {
		t.Fatalf("SessionCookieMode = %q, want lax", cfg.SessionCookieMode)
	}
}

func TestParseFlags_SplitModeRejectsMixed(t *testing.T) {
	// -broker-url together with the default (non-empty) -listen must fail,
	// and the error must name both flags so the operator can fix the config.
	_, err := ParseFlags([]string{"-broker-url", "https://broker.internal:9443"}, noEnv)
	if err == nil {
		t.Fatal("broker-url with default -listen must fail")
	}
	if !strings.Contains(err.Error(), "broker-url") || !strings.Contains(err.Error(), "listen") {
		t.Fatalf("error must name -broker-url and -listen, got: %v", err)
	}

	// Same for -internal-listen left at its default.
	_, err = ParseFlags([]string{"-listen=", "-broker-url", "https://broker.internal:9443"}, noEnv)
	if err == nil {
		t.Fatal("broker-url with default -internal-listen must fail")
	}

	// A pure split-mode invocation parses cleanly.
	cfg, err := ParseFlags(splitArgs(), noEnv)
	if err != nil {
		t.Fatalf("split mode ParseFlags: %v", err)
	}
	if cfg.Listen != "" || cfg.InternalListen != "" {
		t.Fatalf("split mode: app/internal listeners must be off, got %q/%q", cfg.Listen, cfg.InternalListen)
	}
	if cfg.BrokerURL != "https://broker.internal:9443" {
		t.Fatalf("BrokerURL = %q", cfg.BrokerURL)
	}
}

func TestParseFlags_LoginKeyRequired(t *testing.T) {
	args := dropArg(mergedArgs(), "-login-key-file")
	_, err := ParseFlags(args, noEnv)
	if err == nil {
		t.Fatal("app listener on without -login-key-file must fail")
	}
	if !strings.Contains(err.Error(), "login-key-file") {
		t.Fatalf("error must name -login-key-file, got: %v", err)
	}

	// With the app listener disabled the key is not required. A
	// session-listener-only merged backend still needs the DB (the broker
	// lives in-process), so keep -database-url and drop the OIDC flags.
	args = dropArg(mergedArgs(), "-login-key-file")
	args = append(args, "-listen=", "-internal-listen=")
	for _, f := range []string{"-oidc-issuer", "-oidc-client-id", "-oidc-redirect-url",
		"-internal-tls-cert", "-internal-tls-key", "-internal-client-ca"} {
		args = dropArg(args, f)
	}
	if _, err := ParseFlags(args, noEnv); err != nil {
		t.Fatalf("session-only merged mode should not need -login-key-file: %v", err)
	}
}

func TestParseFlags_EnvFallback(t *testing.T) {
	env := envMap(map[string]string{"TCDI_SESSION_COOKIE_MODE": "partitioned"})
	cfg, err := ParseFlags(mergedArgs(), env)
	if err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	if cfg.SessionCookieMode != "partitioned" {
		t.Fatalf("env SessionCookieMode = %q, want partitioned", cfg.SessionCookieMode)
	}
	// The flag beats the env.
	cfg, err = ParseFlags(withArg(mergedArgs(), "-session-cookie-mode", "lax"), env)
	if err != nil {
		t.Fatalf("ParseFlags with flag override: %v", err)
	}
	if cfg.SessionCookieMode != "lax" {
		t.Fatalf("flag SessionCookieMode = %q, want lax", cfg.SessionCookieMode)
	}
}

func TestParseFlags_CookieModeValues(t *testing.T) {
	for _, mode := range []string{"lax", "partitioned"} {
		if _, err := ParseFlags(withArg(mergedArgs(), "-session-cookie-mode", mode), noEnv); err != nil {
			t.Fatalf("cookie mode %q rejected: %v", mode, err)
		}
	}
	if _, err := ParseFlags(withArg(mergedArgs(), "-session-cookie-mode", "strict"), noEnv); err == nil {
		t.Fatal("cookie mode 'strict' must be rejected")
	}
}

func TestParseFlags_RequiredInputs(t *testing.T) {
	for _, flag := range []string{
		"-session-origin", "-database-url", "-oidc-issuer",
		"-oidc-client-id", "-oidc-redirect-url",
		"-session-tls-cert", "-session-tls-key", "-session-allowed-hosts",
		"-internal-tls-cert", "-internal-tls-key", "-internal-client-ca",
	} {
		if _, err := ParseFlags(dropArg(mergedArgs(), flag), noEnv); err == nil {
			t.Fatalf("dropping %s must fail", flag)
		}
	}
}

// ---------------------------------------------------------------------------
// Ported from cmd/api/main_test.go and cmd/gateway/main_test.go.
// ---------------------------------------------------------------------------

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

// The group gate trims CSV entries and accumulates repeated flags, but
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

// CHTR-8: refuse non-verifying PostgreSQL sslmodes. The api must not start
// unless the resolved pgx TLS config verifies the server certificate on
// every candidate connection.
func TestCheckDatabaseTLS(t *testing.T) {
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
		"postgres://user:p w@:bad",
		"host=db.internal sslmode",
		"not a dsn at all",
	} {
		if mode, err := checkDatabaseTLS(dsn, false); err == nil {
			t.Fatalf("dsn %q passed with mode %q", dsn, mode)
		} else if strings.Contains(err.Error(), dsn) {
			t.Fatalf("error echoed the DSN: %v", err)
		}
	}
}

// -portal-origin / -login-key-file are repeatable and each value may itself
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

// SEC-I2: with no token configured the /v1/control surface fails CLOSED —
// the startup warning must say it is disabled, not claim it is
// unauthenticated.
func TestControlTokenUnsetWarning(t *testing.T) {
	if strings.Contains(controlTokenUnsetWarning, "UNAUTHENTICATED") {
		t.Fatalf("warning claims control endpoints are unauthenticated: %q", controlTokenUnsetWarning)
	}
	if !strings.Contains(controlTokenUnsetWarning, "disabled") {
		t.Fatalf("warning must state /v1/control is disabled: %q", controlTokenUnsetWarning)
	}
}
