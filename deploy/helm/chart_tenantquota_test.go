package chart_test

// Declarative tenant quotas (FX-R17): managedNamespaces[].quota renders the
// backend -tenant-quotas flag; a tenant without one is warned about in the
// install NOTES; the schema rejects negative or unparsable quantities.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const quotaBase = `
portalHost: portal.lab.example.net
sessionDomain: session.lab.example.net
oidc: {issuer: "https://idp.lab.example.net/realms/tinycdi", clientID: tinycdi, existingSecret: tinycdi-oidc-client}
database:
  existingSecret: tinycdi-backend-db
  allowedPeers: [{ipBlock: {cidr: 10.20.30.40/32}}]
backend: {loginKeys: {existingSecret: tinycdi-backend-login-keys}}
runtime: {placement: {allowSharedNodes: true}}
`

// quotaValues writes a values file with the given managedNamespaces YAML.
func quotaValues(t *testing.T, managed string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "values.yaml")
	if err := os.WriteFile(p, []byte(quotaBase+"managedNamespaces:\n"+managed), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func tenantQuotasArg(t *testing.T, valuesFile string) (string, bool) {
	t.Helper()
	dep := deployment(renderArgs(t, "-f", valuesFile), "backend")
	if dep == nil {
		t.Fatal("no backend Deployment rendered")
	}
	for _, a := range firstContainerArgs(dep) {
		if v, ok := strings.CutPrefix(a, "-tenant-quotas="); ok {
			return v, true
		}
	}
	return "", false
}

func TestTenantQuotasRenderFlag(t *testing.T) {
	vf := quotaValues(t, `
  - name: tcdi-a
    tenant: tenant-a
    quota: {runningWorkspaces: 12, cpu: "16", memory: 64Gi, storage: 200Gi}
  - name: tcdi-b
    tenant: tenant-b
    quota: {runningWorkspaces: 0, cpu: 500m, memory: 107374182400, storage: 1Ti}
  - name: tcdi-c
    tenant: tenant-c
`)
	raw, ok := tenantQuotasArg(t, vf)
	if !ok {
		t.Fatal("backend args carry no -tenant-quotas")
	}
	var got []map[string]any
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("-tenant-quotas is not JSON: %v\n%s", err, raw)
	}
	if len(got) != 2 {
		t.Fatalf("-tenant-quotas lists %d tenants, want exactly the 2 with a quota block (tenant-c is untouched): %s", len(got), raw)
	}
	want := []map[string]any{
		{"tenant": "tenant-a", "runningWorkspaces": 12.0, "cpu": "16", "memory": "64Gi", "storage": "200Gi"},
		{"tenant": "tenant-b", "runningWorkspaces": 0.0, "cpu": "500m", "memory": 107374182400.0, "storage": "1Ti"},
	}
	for i := range want {
		for k, v := range want[i] {
			if got[i][k] != v {
				t.Errorf("entry %d %s = %v (%T), want %v", i, k, got[i][k], got[i][k], v)
			}
		}
		if len(got[i]) != len(want[i]) {
			t.Errorf("entry %d has unexpected keys: %v", i, got[i])
		}
	}
	if strings.Contains(raw, "e+") {
		t.Errorf("large quantity rendered in exponent form: %s", raw)
	}
}

// No quota block anywhere: no flag, so a chart upgrade never touches the
// tenant_quota table.
func TestTenantQuotasFlagAbsentWithoutQuota(t *testing.T) {
	if v, ok := tenantQuotasArg(t, filepath.Join("tinycdi", "ci", "minimal-values.yaml")); ok {
		t.Fatalf("minimal values (no quota blocks) rendered -tenant-quotas=%s", v)
	}
}

// The example values declare a quota, so the ci values matrix lints and
// renders the flag path.
func TestExampleValuesDeclareTenantQuotas(t *testing.T) {
	v, ok := tenantQuotasArg(t, filepath.Join("tinycdi", "ci", "example-values.yaml"))
	if !ok || !strings.Contains(v, `"tenant":"tenant-a"`) {
		t.Fatalf("example values do not render a tenant-a quota: %q ok=%v", v, ok)
	}
}

func TestTenantQuotasNotesWarnForTenantsWithoutQuota(t *testing.T) {
	vf := quotaValues(t, `
  - name: tcdi-a
    tenant: tenant-a
    quota: {runningWorkspaces: 1, cpu: "1", memory: 1Gi, storage: 1Gi}
  - name: tcdi-b
    tenant: tenant-b
`)
	notes := renderNotes(t, "-f", vf)
	if !strings.Contains(notes, "WARNING") || !strings.Contains(notes, "tenant-b") ||
		!strings.Contains(notes, "QUOTA_NOT_CONFIGURED") {
		t.Errorf("NOTES must warn that tenant-b has no quota (creates refused with QUOTA_NOT_CONFIGURED), got:\n%s", notes)
	}
	i := strings.Index(notes, "Tenant quotas")
	if i < 0 || strings.Contains(notes[i:], "tenant-a") {
		t.Errorf("NOTES must warn about tenant-b only, not tenant-a, got:\n%s", notes)
	}

	all := quotaValues(t, `
  - name: tcdi-a
    tenant: tenant-a
    quota: {runningWorkspaces: 1, cpu: "1", memory: 1Gi, storage: 1Gi}
`)
	if n := renderNotes(t, "-f", all); strings.Contains(n, "QUOTA_NOT_CONFIGURED") {
		t.Errorf("no tenant lacks a quota, yet NOTES warn:\n%s", n)
	}
}

func TestSchemaRejectsBadTenantQuotas(t *testing.T) {
	entry := func(quota string) string {
		return "\n  - name: tcdi-a\n    tenant: tenant-a\n    quota: " + quota + "\n"
	}
	good := `{runningWorkspaces: 1, cpu: "1", memory: 1Gi, storage: 1Gi}`
	renderArgs(t, "-f", quotaValues(t, entry(good))) // sanity: renders

	for name, bad := range map[string]string{
		"negative cpu":          `{runningWorkspaces: 1, cpu: "-1", memory: 1Gi, storage: 1Gi}`,
		"negative cpu number":   `{runningWorkspaces: 1, cpu: -1, memory: 1Gi, storage: 1Gi}`,
		"unparsable cpu":        `{runningWorkspaces: 1, cpu: lots, memory: 1Gi, storage: 1Gi}`,
		"binary cpu":            `{runningWorkspaces: 1, cpu: 1Gi, memory: 1Gi, storage: 1Gi}`,
		"negative memory":       `{runningWorkspaces: 1, cpu: "1", memory: -1Gi, storage: 1Gi}`,
		"unparsable memory":     `{runningWorkspaces: 1, cpu: "1", memory: 8GB, storage: 1Gi}`,
		"negative storage":      `{runningWorkspaces: 1, cpu: "1", memory: 1Gi, storage: "-5"}`,
		"unparsable storage":    `{runningWorkspaces: 1, cpu: "1", memory: 1Gi, storage: big}`,
		"empty storage":         `{runningWorkspaces: 1, cpu: "1", memory: 1Gi, storage: ""}`,
		"negative workspaces":   `{runningWorkspaces: -1, cpu: "1", memory: 1Gi, storage: 1Gi}`,
		"fractional workspaces": `{runningWorkspaces: 1.5, cpu: "1", memory: 1Gi, storage: 1Gi}`,
		"missing cpu":           `{runningWorkspaces: 1, memory: 1Gi, storage: 1Gi}`,
		"missing workspaces":    `{cpu: "1", memory: 1Gi, storage: 1Gi}`,
		"empty block":           `{}`,
		"unknown key":           `{runningWorkspaces: 1, cpu: "1", memory: 1Gi, storage: 1Gi, gpu: 1}`,
	} {
		out := renderErrArgs(t, "-f", quotaValues(t, entry(bad)))
		if !strings.Contains(out, "quota") {
			t.Errorf("%s: render failed but the error does not point at quota:\n%s", name, out)
		}
	}
}
