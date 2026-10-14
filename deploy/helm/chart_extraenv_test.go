// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package chart_test

// SR-3-F2 / CHTR-3: <component>.extraEnv used to render verbatim AFTER the
// chart's managed env block, so it carried the dev-surface flags'
// environment equivalents (the backend reads every flag as
// TCDI_<UPPER_SNAKE>, the operator the same plus POD_NAMESPACE/libpq
// names) — including the env names of security-relevant flags the chart
// wires as first-class values, which stay live whenever the flag is not
// rendered — and could silently duplicate a chart-managed name (the later
// entry wins). The render must now refuse every shape unless dev.enabled,
// deriving the managed set from the rendered env blocks (never a
// hand-maintained copy that could drift).

import (
	"path/filepath"
	"strings"
	"testing"
)

// deniedEnvNames mirrors the $deniedEnv map in templates/_helpers.tpl —
// dev-surface env names plus the env equivalents of security-relevant
// flags the chart exposes as first-class values (the env path bypasses
// the chart's wiring/validation whenever the flag is unset).
var deniedEnvNames = []string{
	"TCDI_DEV_INSECURE_DB",
	"TCDI_DEV_ALLOW_NO_BROKER",
	"TCDI_METRICS_LISTEN",
	"TCDI_KASM_ADAPTER_IMAGE",
	"PGSSLMODE",
	"TCDI_OPERATOR_CN",
	"TCDI_WATCH_NAMESPACES",
	"TCDI_REQUIRED_GROUPS",
	"TCDI_TENANT_ADMIN_GROUPS",
	"TCDI_CONTROL_TOKEN",
	"TCDI_CONTROL_TOKEN_FILE",
	"TCDI_TRUSTED_PROXIES",
	"TCDI_UPSTREAM_CA",
	"TCDI_OIDC_POST_LOGOUT_REDIRECT",
	"TCDI_TENANT_QUOTAS",
	"TCDI_TENANT_QUOTA_CEILINGS",
}

// extraEnvSet builds the --set pair for one extraEnv entry.
func extraEnvSet(component, name string) []string {
	return []string{
		component + ".extraEnv[0].name=" + name,
		component + ".extraEnv[0].value=x",
	}
}

// TestExtraEnvDevSurfaceNamesDenied: every denied env name fails the
// render on EVERY component — the deny-list is never component-scoped.
// The error names the component, the env var and the gate.
func TestExtraEnvDevSurfaceNamesDenied(t *testing.T) {
	vf := filepath.Join("tinycdi", "ci", "minimal-values.yaml")
	for _, component := range []string{"backend", "operator", "frontend"} {
		for _, env := range deniedEnvNames {
			sets := append([]string{"-f", vf}, nil...)
			for _, s := range extraEnvSet(component, env) {
				sets = append(sets, "--set", s)
			}
			out := renderErrArgs(t, sets...)
			for _, want := range []string{component + ".extraEnv", `"` + env + `"`, "dev.enabled"} {
				if !strings.Contains(out, want) {
					t.Errorf("%s.extraEnv %s: render error must contain %s, got: %s", component, env, want, out)
				}
			}
		}
	}
}

// TestExtraEnvDeniedNamesSuggestValue: each denied name's error names the
// first-class chart value to use instead — the flag's env path must not
// leave the operator guessing (spot-checked per name on its own
// component).
func TestExtraEnvDeniedNamesSuggestValue(t *testing.T) {
	vf := filepath.Join("tinycdi", "ci", "minimal-values.yaml")
	cases := []struct {
		component string
		env       string
		value     string
	}{
		{"backend", "TCDI_DEV_INSECURE_DB", "database.tls.mode"},
		{"operator", "TCDI_DEV_ALLOW_NO_BROKER", "devAllowNoBroker"},
		{"backend", "TCDI_METRICS_LISTEN", "backend.metrics"},
		{"operator", "TCDI_KASM_ADAPTER_IMAGE", "kasmAdapter"},
		{"backend", "PGSSLMODE", "database.tls.mode"},
		{"backend", "TCDI_OPERATOR_CN", "backend.operatorCN"},
		{"operator", "TCDI_WATCH_NAMESPACES", "managedNamespaces"},
		{"backend", "TCDI_REQUIRED_GROUPS", "oidc.requiredGroups"},
		{"backend", "TCDI_TENANT_ADMIN_GROUPS", "oidc.tenantAdminGroups"},
		{"backend", "TCDI_CONTROL_TOKEN", "backend.controlToken.existingSecret"},
		{"backend", "TCDI_CONTROL_TOKEN_FILE", "backend.controlToken.existingSecret"},
		{"backend", "TCDI_TRUSTED_PROXIES", "backend.trustedProxies"},
		{"backend", "TCDI_UPSTREAM_CA", "backend.upstreamCA"},
		{"backend", "TCDI_OIDC_POST_LOGOUT_REDIRECT", "oidc.postLogoutRedirect"},
		{"backend", "TCDI_TENANT_QUOTAS", "managedNamespaces"},
		{"backend", "TCDI_TENANT_QUOTA_CEILINGS", "managedNamespaces"},
	}
	for _, tc := range cases {
		sets := append([]string{"-f", vf}, nil...)
		for _, s := range extraEnvSet(tc.component, tc.env) {
			sets = append(sets, "--set", s)
		}
		out := renderErrArgs(t, sets...)
		if !strings.Contains(out, tc.value) {
			t.Errorf("%s on %s: render error must name the value %q, got: %s", tc.env, tc.component, tc.value, out)
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

// TestExtraEnvMalformedEntryFails: an entry that is not a map or has no
// name cannot slip past the guard — it fails the render with a clear
// message instead of producing a manifest the API server would reject.
func TestExtraEnvMalformedEntryFails(t *testing.T) {
	vf := filepath.Join("tinycdi", "ci", "minimal-values.yaml")
	for _, tc := range []struct {
		name string
		sets []string
	}{
		{"non-map entry", []string{"backend.extraEnv[0]=just-a-string"}},
		{"nameless map", []string{"backend.extraEnv[0].value=x"}},
		{"empty name", []string{"backend.extraEnv[0].name=", "backend.extraEnv[0].value=x"}},
	} {
		sets := append([]string{"-f", vf}, nil...)
		for _, s := range tc.sets {
			sets = append(sets, "--set", s)
		}
		out := renderErrArgs(t, sets...)
		if !strings.Contains(out, "backend.extraEnv") {
			t.Errorf("%s: render error must name backend.extraEnv, got: %s", tc.name, out)
		}
	}
}

// TestExtraEnvDevGateEscape: dev.enabled=true keeps the escape hatch —
// both the dev-surface names and a managed duplicate render verbatim.
func TestExtraEnvDevGateEscape(t *testing.T) {
	vf := filepath.Join("tinycdi", "ci", "minimal-values.yaml")
	docs := renderArgs(t, "-f", vf,
		"--set", "dev.enabled=true",
		"--set-json", `backend.extraEnv=[{"name":"TCDI_DEV_INSECURE_DB","value":"1"},{"name":"TCDI_CONTROL_TOKEN","value":"dev-token"}]`,
		"--set-json", `operator.extraEnv=[{"name":"TCDI_DEV_ALLOW_NO_BROKER","value":"true"},{"name":"POD_NAMESPACE","value":"other-ns"}]`)
	env := firstContainerEnv(deployment(docs, "backend"))
	if env["TCDI_DEV_INSECURE_DB"] != "1" {
		t.Errorf("dev.enabled: denied env name must render, got %q", env["TCDI_DEV_INSECURE_DB"])
	}
	if env["TCDI_CONTROL_TOKEN"] != "dev-token" {
		t.Errorf("dev.enabled: denied env name must render, got %q", env["TCDI_CONTROL_TOKEN"])
	}
	env = firstContainerEnv(deployment(docs, "operator"))
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
