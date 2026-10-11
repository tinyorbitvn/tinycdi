// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package chart_test

// SR-3-F2 / CHTR-3: <component>.extraEnv used to render verbatim AFTER the
// chart's managed env block, so it carried the dev-surface flags'
// environment equivalents (the backend reads every flag as
// TCDI_<UPPER_SNAKE>, the operator the same plus POD_NAMESPACE/libpq
// names) and could silently duplicate a chart-managed name — the later
// entry wins. The render must now refuse both shapes unless dev.enabled,
// deriving the managed set from the rendered env blocks (never a
// hand-maintained copy that could drift).

import (
	"path/filepath"
	"strings"
	"testing"
)

// extraEnvSet builds the --set pair for one extraEnv entry.
func extraEnvSet(component, name string) []string {
	return []string{
		component + ".extraEnv[0].name=" + name,
		component + ".extraEnv[0].value=x",
	}
}

// TestExtraEnvDevSurfaceNamesDenied: every dev-surface env name on the
// SR-3-F2 list fails the render on EVERY component — the error names the
// component, the env var and the dev.enabled gate. Each name is exercised
// on the binary that honours it, plus one cross-component case to pin that
// the deny-list is not scoped to the owning component.
func TestExtraEnvDevSurfaceNamesDenied(t *testing.T) {
	vf := filepath.Join("tinycdi", "ci", "minimal-values.yaml")
	cases := []struct {
		name      string
		component string
		env       string
	}{
		{"backend dev-insecure-db env", "backend", "TCDI_DEV_INSECURE_DB"},
		{"operator dev-allow-no-broker env", "operator", "TCDI_DEV_ALLOW_NO_BROKER"},
		{"backend metrics-listen env", "backend", "TCDI_METRICS_LISTEN"},
		{"operator kasm adapter image env", "operator", "TCDI_KASM_ADAPTER_IMAGE"},
		{"backend libpq sslmode env", "backend", "PGSSLMODE"},
		{"backend operator-cn env", "backend", "TCDI_OPERATOR_CN"},
		{"operator watch-namespaces env", "operator", "TCDI_WATCH_NAMESPACES"},
		{"backend required-groups env", "backend", "TCDI_REQUIRED_GROUPS"},
		{"cross-component: operator flag env on frontend", "frontend", "TCDI_DEV_ALLOW_NO_BROKER"},
	}
	for _, tc := range cases {
		sets := append([]string{"-f", vf}, nil...)
		for _, s := range extraEnvSet(tc.component, tc.env) {
			sets = append(sets, "--set", s)
		}
		out := renderErrArgs(t, sets...)
		for _, want := range []string{tc.component + ".extraEnv", `"` + tc.env + `"`, "dev.enabled"} {
			if !strings.Contains(out, want) {
				t.Errorf("%s: render error must contain %s, got: %s", tc.name, want, out)
			}
		}
	}
}

// TestExtraEnvManagedDuplicateDenied: an extraEnv name the chart itself
// manages for that component fails the render — a duplicate later in the
// env list silently wins, so the managed set (derived from the rendered
// tinycdi.env.<component> blocks, conditional entries included) is denied
// too.
func TestExtraEnvManagedDuplicateDenied(t *testing.T) {
	vf := filepath.Join("tinycdi", "ci", "minimal-values.yaml")
	cases := []struct {
		name      string
		component string
		env       string
		extra     []string
	}{
		{"backend managed OIDC issuer", "backend", "TCDI_OIDC_ISSUER", nil},
		{"backend managed database URL", "backend", "TCDI_DATABASE_URL", nil},
		{"backend managed tenant allowlist", "backend", "TCDI_TENANT_ALLOWLIST", nil},
		{"operator managed POD_NAMESPACE", "operator", "POD_NAMESPACE", nil},
		// Conditional managed entries count too: SSL_CERT_FILE only
		// renders when backend.extraCA is wired, PGSSLROOTCERT only when
		// database.tls.caSecret is named — the managed set must follow
		// the values, not a static list.
		{"backend conditional SSL_CERT_FILE", "backend", "SSL_CERT_FILE",
			[]string{"backend.extraCA.existingSecret=corp-ca"}},
		{"backend conditional PGSSLROOTCERT", "backend", "PGSSLROOTCERT",
			[]string{"database.tls.caSecret.name=db-ca"}},
	}
	for _, tc := range cases {
		sets := append([]string{"-f", vf}, nil...)
		for _, s := range append(tc.extra, extraEnvSet(tc.component, tc.env)...) {
			sets = append(sets, "--set", s)
		}
		out := renderErrArgs(t, sets...)
		for _, want := range []string{tc.component + ".extraEnv", `"` + tc.env + `"`, "dev.enabled"} {
			if !strings.Contains(out, want) {
				t.Errorf("%s: render error must contain %s, got: %s", tc.name, want, out)
			}
		}
	}
	// A name the chart manages on a DIFFERENT component is not a duplicate
	// here — the managed set is per-component. The frontend manages no env
	// at all, so another component's chart-owned name renders freely.
	docs := renderArgs(t, "-f", vf,
		"--set-json", `frontend.extraEnv=[{"name":"TCDI_OIDC_ISSUER","value":"https://idp"}]`)
	if got := firstContainerEnv(deployment(docs, "frontend"))["TCDI_OIDC_ISSUER"]; got != "https://idp" {
		t.Errorf("cross-component managed name must still render on frontend, got %q", got)
	}
}

// TestExtraEnvDevGateEscape: dev.enabled=true keeps the escape hatch —
// both the dev-surface names and a managed duplicate render verbatim.
func TestExtraEnvDevGateEscape(t *testing.T) {
	vf := filepath.Join("tinycdi", "ci", "minimal-values.yaml")
	docs := renderArgs(t, "-f", vf,
		"--set", "dev.enabled=true",
		"--set-json", `backend.extraEnv=[{"name":"TCDI_DEV_INSECURE_DB","value":"1"}]`,
		"--set-json", `operator.extraEnv=[{"name":"TCDI_DEV_ALLOW_NO_BROKER","value":"true"},{"name":"POD_NAMESPACE","value":"other-ns"}]`)
	if got := firstContainerEnv(deployment(docs, "backend"))["TCDI_DEV_INSECURE_DB"]; got != "1" {
		t.Errorf("dev.enabled: denied env name must render, got %q", got)
	}
	env := firstContainerEnv(deployment(docs, "operator"))
	if env["TCDI_DEV_ALLOW_NO_BROKER"] != "true" {
		t.Errorf("dev.enabled: denied operator env name must render, got %q", env["TCDI_DEV_ALLOW_NO_BROKER"])
	}
	if env["POD_NAMESPACE"] != "other-ns" {
		t.Errorf("dev.enabled: managed duplicate must render (later entry wins), got %q", env["POD_NAMESPACE"])
	}
}

// TestExtraEnvBenignPasses: a name that is neither dev-surface nor
// chart-managed renders into the component's env untouched.
func TestExtraEnvBenignPasses(t *testing.T) {
	vf := filepath.Join("tinycdi", "ci", "minimal-values.yaml")
	docs := renderArgs(t, "-f", vf,
		"--set-json", `backend.extraEnv=[{"name":"TCDI_LOGIN_RATE","value":"10"}]`)
	if got := firstContainerEnv(deployment(docs, "backend"))["TCDI_LOGIN_RATE"]; got != "10" {
		t.Errorf("benign extraEnv must render, got %q", got)
	}
}
