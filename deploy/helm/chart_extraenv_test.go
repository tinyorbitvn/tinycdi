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
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// deniedEnvNames mirrors tinycdi.staticDeniedEnv in templates/_helpers.tpl —
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
	"TCDI_BROKER_URL",
	"TCDI_BROKER_CA",
	"TCDI_MTLS_CERT",
	"TCDI_MTLS_KEY",
	"SSL_CERT_FILE",
	"SSL_CERT_DIR",
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

// documentedFlagOverrides are the extraArgs knobs that may repeat a
// chart-rendered backend flag (capacity.md, install.md).
var documentedFlagOverrides = map[string]bool{"rate-limit-replicas": true, "login-key-file": true}

// chartArgFlags scans templates/_args.tpl and returns, per component,
// every flag name tinycdi.args.<component> can emit — rendered by
// default or only on some values.
func chartArgFlags(t *testing.T) map[string][]string {
	t.Helper()
	src, err := os.ReadFile(filepath.Join("tinycdi", "templates", "_args.tpl"))
	if err != nil {
		t.Fatal(err)
	}
	def := regexp.MustCompile(`define "tinycdi\.args\.([a-z]+)"`)
	flag := regexp.MustCompile(`^\s*- (?:\{\{ printf ")?-{1,2}([a-z][a-z0-9-]*)`)
	out := map[string][]string{}
	seen := map[string]bool{}
	cur := ""
	for _, line := range strings.Split(string(src), "\n") {
		if m := def.FindStringSubmatch(line); m != nil {
			cur = m[1]
			continue
		}
		if m := flag.FindStringSubmatch(line); m != nil && cur != "" && !seen[cur+"/"+m[1]] {
			seen[cur+"/"+m[1]] = true
			out[cur] = append(out[cur], m[1])
		}
	}
	if len(out["backend"]) < 30 || len(out["operator"]) < 10 || len(out["frontend"]) < 5 {
		t.Fatalf("_args.tpl scan found too few flags: %v", out)
	}
	return out
}

// argFlagName strips an args entry to its flag name.
func argFlagName(a string) string {
	n := strings.TrimLeft(a, "-")
	if i := strings.IndexAny(n, "= \t"); i >= 0 {
		n = n[:i]
	}
	return n
}

// flagEnvName is the env form a TinyCDI binary reads a flag from.
func flagEnvName(flag string) string {
	return "TCDI_" + strings.ToUpper(strings.ReplaceAll(flag, "-", "_"))
}

// TestExtraArgsCannotOverrideRenderedFlags (R1-34, TM-42): extraArgs
// renders after the chart's own flags and the last value of a flag wins,
// so without dev.enabled no <component>.extraArgs entry may name a flag
// the chart can render for that component (except the documented
// backend knobs), nor the flag form of a denied extraEnv name; and the
// deny lists are symmetric — every such flag's TCDI_ env form fails in
// extraEnv. Every arg the minimal render emits must come from
// tinycdi.args.<component>, so the scan sees it.
func TestExtraArgsCannotOverrideRenderedFlags(t *testing.T) {
	flags := chartArgFlags(t)
	docs := renderArgs(t, "-f", minimalValues)
	for c, names := range flags {
		inSrc := map[string]bool{}
		for _, n := range names {
			inSrc[n] = true
		}
		for _, a := range firstContainerArgs(deployment(docs, c)) {
			if !inSrc[argFlagName(a)] {
				t.Errorf("%s renders %q outside tinycdi.args.%s — the extraArgs guard cannot see it", c, a, c)
			}
		}
	}

	type probe struct {
		label, want string
		sets        []string
	}
	var probes []probe
	for c, names := range flags {
		for _, n := range names {
			if c == "backend" && documentedFlagOverrides[n] {
				continue
			}
			arg := "--" + n + "=x"
			probes = append(probes, probe{
				label: c + "/flag/" + n,
				want:  fmt.Sprintf("%s.extraArgs entry %q", c, arg),
				sets:  []string{"--set", c + ".extraArgs[0]=" + arg},
			})
			env := flagEnvName(n)
			probes = append(probes, probe{
				label: c + "/env/" + env,
				want:  fmt.Sprintf("%s.extraEnv entry %q", c, env),
				sets:  []string{"--set", c + ".extraEnv[0].name=" + env, "--set", c + ".extraEnv[0].value=x"},
			})
		}
	}
	for _, env := range deniedEnvNames {
		if !strings.HasPrefix(env, "TCDI_") {
			continue
		}
		arg := "--" + strings.ToLower(strings.ReplaceAll(strings.TrimPrefix(env, "TCDI_"), "_", "-")) + "=x"
		for _, c := range []string{"backend", "operator", "frontend"} {
			probes = append(probes, probe{
				label: c + "/flagform/" + env,
				want:  fmt.Sprintf("%s.extraArgs entry %q", c, arg),
				sets:  []string{"--set", c + ".extraArgs[0]=" + arg},
			})
		}
	}
	sort.Slice(probes, func(i, j int) bool { return probes[i].label < probes[j].label })
	t.Run("denied", func(t *testing.T) {
		for _, p := range probes {
			t.Run(p.label, func(t *testing.T) {
				t.Parallel()
				out := renderErrArgs(t, append([]string{"-f", minimalValues}, p.sets...)...)
				if !strings.Contains(out, p.want) {
					t.Errorf("render must fail naming %s, got: %s", p.want, out)
				}
			})
		}
	})

	// The documented knobs and unrendered tuning flags still render, and
	// dev.enabled reopens a rendered flag.
	docs = renderArgs(t, "-f", minimalValues,
		"--set", "backend.extraArgs[0]=-rate-limit-replicas=4",
		"--set", "backend.extraArgs[1]=-login-key-file=/etc/login-keys/previous",
		"--set", "backend.extraArgs[2]=-login-rate=0",
		"--set", "backend.extraArgs[3]=--expiry-interval=45s")
	args := strings.Join(firstContainerArgs(deployment(docs, "backend")), " ")
	for _, want := range []string{"-rate-limit-replicas=4", "-login-key-file=/etc/login-keys/previous", "-login-rate=0"} {
		if !strings.Contains(args, want) {
			t.Errorf("documented extraArgs knob %s missing: %s", want, args)
		}
	}
	renderArgs(t, "-f", minimalValues, "--set", "dev.enabled=true", "--set", "backend.extraArgs[0]=-session-idle=1h")
}
