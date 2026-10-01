// Package chart_test renders deploy/helm/tinycdi with the pinned helm
// binary and asserts the install-time invariants the official chart fixes:
//
//   - the operator ClusterRole is bound ONLY via RoleBindings in the release
//     namespace and the managed namespace allowlist (never a
//     ClusterRoleBinding),
//   - runtime ServiceAccounts have automountServiceAccountToken=false,
//   - a default-deny NetworkPolicy exists in every namespace the chart owns,
//   - every container in every Deployment has resources, a readiness probe
//     and a non-root securityContext,
//   - no plaintext Secret objects (secrets are referenced by name only),
//   - KubeVirt/CDI (or any other cluster-wide dependency) is never rendered,
//   - the default values render with NO dev components (no Postgres, no
//     dev-OIDC, no dev-allow-no-broker),
//   - the control plane is exactly three Deployments (backend, frontend,
//     operator); the removed api/gateway/portal values fail with a
//     migration hint,
//   - portal and session hosts are different registrable hosts; the render
//     FAILS when they are equal,
//   - values.schema.json rejects malformed values,
//   - exposure is Ingress XOR Gateway API HTTPRoute (both on = render fails),
//   - cert-manager Certificates render only when certManager.enabled,
//   - images resolve through global.imageRegistry / per-image registry /
//     tag / digest with tag defaulting to Chart.AppVersion.
//
// Helm is resolved via $TCDI_HELM, then bin/helm, then PATH.
package chart_test

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// doc is one rendered manifest. An ALIAS (not a named type) so yaml.v3
// decodes nested maps as plain map[string]any and type assertions work.
type doc = map[string]any

// lintValues are the ci files expected to lint+render cleanly.
var lintValues = []string{
	"minimal-values.yaml",
	"example-values.yaml",
	"ingress-certmanager-values.yaml",
	"gateway-api-values.yaml",
	"template-revision-values.yaml",
	"node-profiles-values.yaml",
	"security-values.yaml",
}

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Clean(filepath.Join(wd, "..", ".."))
}

func helmBin(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("TCDI_HELM"); p != "" {
		return p
	}
	pinned := filepath.Join(repoRoot(t), "bin", "helm")
	if _, err := os.Stat(pinned); err == nil {
		return pinned
	}
	if p, err := exec.LookPath("helm"); err == nil {
		return p
	}
	t.Skip("no helm binary: set TCDI_HELM or put helm on PATH")
	return ""
}

// render runs helm template with a ci values file and decodes every
// rendered YAML document.
func render(t *testing.T, valuesFile string) []doc {
	t.Helper()
	return renderArgs(t, "-f", filepath.Join("tinycdi", "ci", valuesFile))
}

// renderBad renders a deliberately-invalid values file from
// tinycdi/testdata (NOT ci/ — every ci/*.yaml file must lint+render
// cleanly for the CI values matrix).
func renderBad(t *testing.T, valuesFile string) string {
	t.Helper()
	return renderErrArgs(t, "-f", filepath.Join("tinycdi", "testdata", valuesFile))
}

// renderDefaults renders with the chart's default values only.
func renderDefaults(t *testing.T) []doc {
	t.Helper()
	return renderArgs(t)
}

func renderArgs(t *testing.T, extra ...string) []doc {
	t.Helper()
	out, err := helmTemplate(t, extra...)
	if err != nil {
		t.Fatalf("helm template %v: %v\n%s", extra, err, out)
	}
	return decodeDocs(t, out)
}

func renderErrArgs(t *testing.T, extra ...string) string {
	t.Helper()
	out, err := helmTemplate(t, extra...)
	if err == nil {
		t.Fatalf("helm template %v: expected failure, render succeeded", extra)
	}
	return string(out)
}

func helmTemplate(t *testing.T, extra ...string) ([]byte, error) {
	t.Helper()
	args := []string{"template", "tcdi", "./tinycdi",
		"--namespace", "tcdi-system", "--include-crds"}
	args = append(args, extra...)
	return exec.Command(helmBin(t), args...).CombinedOutput()
}

func decodeDocs(t *testing.T, data []byte) []doc {
	t.Helper()
	var docs []doc
	dec := yaml.NewDecoder(bytes.NewReader(data))
	for {
		var d doc
		err := dec.Decode(&d)
		if err != nil {
			if err.Error() == "EOF" {
				break
			}
			t.Fatalf("decode rendered manifest: %v", err)
		}
		if len(d) == 0 {
			continue
		}
		docs = append(docs, d)
	}
	return docs
}

func selectDocs(docs []doc, kind string) []doc {
	var out []doc
	for _, d := range docs {
		if d["kind"] == kind {
			out = append(out, d)
		}
	}
	return out
}

func meta(d doc) (name, namespace string) {
	m, _ := d["metadata"].(map[string]any)
	name, _ = m["name"].(string)
	namespace, _ = m["namespace"].(string)
	return name, namespace
}

// walk finds every nested map containing all of keys (shallow match).
func walk(v any, keys []string, hit func(map[string]any)) {
	switch t := v.(type) {
	case map[string]any:
		ok := true
		for _, k := range keys {
			if _, has := t[k]; !has {
				ok = false
				break
			}
		}
		if ok {
			hit(t)
		}
		for _, child := range t {
			walk(child, keys, hit)
		}
	case []any:
		for _, child := range t {
			walk(child, keys, hit)
		}
	}
}

// managedNamespaces mirrors ci/example-values.yaml (and ci/minimal-values.yaml).
var managedNamespaces = []string{"tinycdi-tenant-a", "tinycdi-tenant-b"}

func allowedBindingNS(ns string) bool {
	if ns == "tcdi-system" {
		return true
	}
	for _, m := range managedNamespaces {
		if ns == m {
			return true
		}
	}
	return false
}

// TestHelmLintStrict: `helm lint --strict` must pass with the chart
// defaults and every valid ci values set.
func TestHelmLintStrict(t *testing.T) {
	sets := append([]string{""}, lintValues...)
	for _, vf := range sets {
		args := []string{"lint", "--strict", "./tinycdi"}
		if vf != "" {
			args = append(args, "-f", filepath.Join("tinycdi", "ci", vf))
		}
		out, err := exec.Command(helmBin(t), args...).CombinedOutput()
		if err != nil {
			t.Errorf("helm lint --strict -f %s: %v\n%s", vf, err, out)
		}
	}
}

// TestDefaultsRenderWithoutDevComponents: the production default render
// must contain no dev conveniences — no Postgres, no dev OIDC provider,
// no --dev-allow-no-broker — and must default images to
// ghcr.io/tinyorbitvn/tinycdi-*:<appVersion>.
// Rendered on top of ci/minimal-values.yaml: since CHTR-7 the stock
// values.yaml can no longer render alone — database.allowedPeers must be
// set to a real peer (the shipped 0.0.0.0/32 placeholder fails closed).
func TestDefaultsRenderWithoutDevComponents(t *testing.T) {
	docs := render(t, "minimal-values.yaml")
	raw, _ := yaml.Marshal(docs)
	s := string(raw)
	for _, banned := range []string{"dev-oidc", "postgres", "keycloak", "dev-allow-no-broker"} {
		if strings.Contains(strings.ToLower(s), banned) {
			t.Errorf("default render contains dev component marker %q", banned)
		}
	}
	deps := selectDocs(docs, "Deployment")
	if len(deps) != 3 {
		t.Fatalf("expected exactly 3 Deployments by default, got %d", len(deps))
	}
	for _, want := range []struct{ dep, img string }{
		{"backend", "ghcr.io/tinyorbitvn/tinycdi-backend:0.2.0"},
		{"operator", "ghcr.io/tinyorbitvn/tinycdi-operator:0.2.0"},
		{"frontend", "ghcr.io/tinyorbitvn/tinycdi-frontend:0.2.0"},
	} {
		d := deployment(docs, want.dep)
		if d == nil {
			t.Fatalf("no %s Deployment in default render", want.dep)
		}
		if got := firstContainerImage(d); got != want.img {
			t.Errorf("default image for %s = %q, want %q (repository/tag must default to ghcr.io/tinyorbitvn + appVersion)", want.dep, got, want.img)
		}
	}
	// No exposure objects, no cert-manager objects, no monitors by default.
	for _, kind := range []string{"Ingress", "HTTPRoute", "Certificate", "Issuer", "ClusterIssuer", "ServiceMonitor"} {
		if n := len(selectDocs(docs, kind)); n > 0 {
			t.Errorf("default render must not contain %s (%d found)", kind, n)
		}
	}
}

// TestSchemaRejectsBadValues: values.schema.json must reject a malformed
// digest (and any other constraint violation) before render.
func TestSchemaRejectsBadValues(t *testing.T) {
	out := renderBad(t, "schema-violation-values.yaml")
	if !strings.Contains(out, "digest") && !strings.Contains(out, "schema") {
		t.Errorf("schema violation should mention the failing field/schema, got: %s", out)
	}
}

// TestExposureToggles: ingress renders Ingress and no HTTPRoute; gatewayApi
// renders HTTPRoute and no Ingress; both enabled is a render-time error.
func TestExposureToggles(t *testing.T) {
	ing := render(t, "ingress-certmanager-values.yaml")
	if n := len(selectDocs(ing, "Ingress")); n != 2 {
		t.Errorf("ingress render: expected 2 Ingresses (portal+session), got %d", n)
	}
	if n := len(selectDocs(ing, "HTTPRoute")); n != 0 {
		t.Errorf("ingress render must not contain HTTPRoutes, got %d", n)
	}

	gw := render(t, "gateway-api-values.yaml")
	routes := selectDocs(gw, "HTTPRoute")
	if len(routes) != 2 {
		t.Fatalf("gatewayApi render: expected 2 HTTPRoutes, got %d", len(routes))
	}
	for _, r := range routes {
		spec, _ := r["spec"].(map[string]any)
		if len(toSlice(spec["parentRefs"])) == 0 {
			t.Errorf("HTTPRoute %v has no parentRefs", r["metadata"])
		}
	}
	if n := len(selectDocs(gw, "Ingress")); n != 0 {
		t.Errorf("gatewayApi render must not contain Ingresses, got %d", n)
	}

	out := renderErrArgs(t,
		"-f", filepath.Join("tinycdi", "ci", "minimal-values.yaml"),
		"--set", "ingress.enabled=true", "--set", "gatewayApi.enabled=true")
	if !strings.Contains(out, "mutually exclusive") {
		t.Errorf("enabling ingress+gatewayApi should fail with a mutual-exclusion error, got: %s", out)
	}
}

// TestCertManagerToggle: no Certificate objects by default; with
// certManager.enabled+selfSigned the render carries the bootstrap CA chain
// plus two leaf Certificates (backend internal listener, operator client)
// writing into the configured secret names.
func TestCertManagerToggle(t *testing.T) {
	if n := len(selectDocs(render(t, "minimal-values.yaml"), "Certificate")); n != 0 {
		t.Errorf("default render must not contain Certificates, got %d", n)
	}
	docs := render(t, "ingress-certmanager-values.yaml")
	certs := selectDocs(docs, "Certificate")
	if len(certs) != 3 { // internal CA + backend-internal + operator-mtls
		t.Fatalf("expected 3 Certificates with certManager.selfSigned, got %d", len(certs))
	}
	secrets := map[string]bool{}
	var operatorCN string
	for _, c := range certs {
		spec, _ := c["spec"].(map[string]any)
		if n, ok := spec["secretName"].(string); ok {
			secrets[n] = true
		}
		name, _ := meta(c)
		if name == "tcdi-operator-mtls" {
			operatorCN, _ = spec["commonName"].(string)
		}
	}
	for _, want := range []string{
		"tcdi-internal-ca",
		"tinycdi-backend-internal-tls",
		"tinycdi-operator-mtls",
	} {
		if !secrets[want] {
			t.Errorf("no Certificate producing Secret %q", want)
		}
	}
	if operatorCN != "operator" {
		t.Errorf("operator mTLS Certificate commonName = %q, must be \"operator\" (broker revoke/drain identity)", operatorCN)
	}
	if len(selectDocs(docs, "Issuer")) != 1 || len(selectDocs(docs, "ClusterIssuer")) != 1 {
		t.Errorf("selfSigned render must contain one Issuer and one selfsigned ClusterIssuer")
	}

	// issuerRef mode (no selfSigned bootstrap).
	docs = renderArgs(t,
		"-f", filepath.Join("tinycdi", "ci", "minimal-values.yaml"),
		"--set", "certManager.enabled=true",
		"--set", "certManager.issuerRef.name=corp-issuer")
	certs = selectDocs(docs, "Certificate")
	if len(certs) != 2 {
		t.Fatalf("issuerRef render: expected 2 leaf Certificates, got %d", len(certs))
	}
	if len(selectDocs(docs, "Issuer"))+len(selectDocs(docs, "ClusterIssuer")) != 0 {
		t.Error("issuerRef render must not bootstrap an Issuer/ClusterIssuer")
	}
	// enabled without issuerRef/selfSigned fails.
	out := renderErrArgs(t,
		"-f", filepath.Join("tinycdi", "ci", "minimal-values.yaml"),
		"--set", "certManager.enabled=true")
	if !strings.Contains(out, "issuerRef") {
		t.Errorf("certManager.enabled without issuer should fail mentioning issuerRef, got: %s", out)
	}
}

// TestImageRegistryOverride: global.imageRegistry prefixes every image;
// a per-image registry wins over the global override.
func TestImageRegistryOverride(t *testing.T) {
	docs := renderArgs(t,
		"-f", filepath.Join("tinycdi", "ci", "minimal-values.yaml"),
		"--set", "global.imageRegistry=registry.example.com",
		"--set", "images.frontend.registry=frontend-mirror.example.net")
	depImg := map[string]string{}
	for _, name := range []string{"backend", "operator", "frontend"} {
		depImg[name] = firstContainerImage(deployment(docs, name))
	}
	if depImg["backend"] != "registry.example.com/tinyorbitvn/tinycdi-backend:0.2.0" {
		t.Errorf("global registry override failed: backend image %q", depImg["backend"])
	}
	if depImg["frontend"] != "frontend-mirror.example.net/tinyorbitvn/tinycdi-frontend:0.2.0" {
		t.Errorf("per-image registry must win over global: frontend image %q", depImg["frontend"])
	}
}

// TestImageDigestPinning: digest beats tag; pull secrets attach to pods;
// seeded template runtime images resolve through the same helper.
func TestImageDigestPinning(t *testing.T) {
	docs := render(t, "example-values.yaml")
	want := map[string]string{
		"backend":  "registry.lab.example.net/tinycdi/backend@sha256:1111111111111111111111111111111111111111111111111111111111111111",
		"operator": "registry.lab.example.net/tinycdi/operator@sha256:2222222222222222222222222222222222222222222222222222222222222222",
		"frontend": "registry.lab.example.net/tinycdi/frontend@sha256:4444444444444444444444444444444444444444444444444444444444444444",
	}
	for name, img := range want {
		if got := firstContainerImage(deployment(docs, name)); got != img {
			t.Errorf("digest pinning: %s image = %q, want %q", name, got, img)
		}
		// imagePullSecrets from global must reach every pod.
		spec, _ := deployment(docs, name)["spec"].(map[string]any)
		tpl, _ := spec["template"].(map[string]any)
		podSpec, _ := tpl["spec"].(map[string]any)
		ips, _ := podSpec["imagePullSecrets"].([]any)
		if len(ips) == 0 {
			t.Errorf("%s: global.imagePullSecrets not rendered on pod", name)
		}
	}
	raw, _ := yaml.Marshal(selectDocs(docs, "WorkspaceTemplate"))
	s := string(raw)
	if !strings.Contains(s, "registry.lab.example.net/tinycdi/linux-desktop@sha256:5555") ||
		!strings.Contains(s, "registry.lab.example.net/tinycdi/browser@sha256:6666") {
		t.Errorf("seeded templates must resolve runtime images through .Values.images, got:\n%s", s)
	}
	// Structured profile fields must land as backend annotations.
	for _, want := range []string{
		"workspaces.cdi.tinyorbit.vn/seccomp-profile: localhost/profiles/chromium-userns.json",
		"workspaces.cdi.tinyorbit.vn/apparmor-profile: localhost/tinycdi-browser",
		"workspaces.cdi.tinyorbit.vn/node-selector",
		"workspaces.cdi.tinyorbit.vn/storage-class: longhorn",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("seeded templates missing %q", want)
		}
	}
}

func TestNoClusterRoleBindings(t *testing.T) {
	for _, vf := range lintValues {
		docs := render(t, vf)
		for _, d := range selectDocs(docs, "ClusterRoleBinding") {
			name, _ := meta(d)
			t.Errorf("%s: ClusterRoleBinding %q must not exist (RoleBinding per managed namespace only)", vf, name)
		}
	}
}

func TestRoleBindingsOnlyInAllowedNamespaces(t *testing.T) {
	for _, vf := range []string{"example-values.yaml", "minimal-values.yaml"} {
		docs := render(t, vf)
		for _, d := range selectDocs(docs, "RoleBinding") {
			name, ns := meta(d)
			if !allowedBindingNS(ns) {
				t.Errorf("%s: RoleBinding %q in unmanaged namespace %q", vf, name, ns)
			}
			rr, _ := d["roleRef"].(map[string]any)
			if rr["kind"] == "ClusterRole" {
				// binding a ClusterRole is fine ONLY via namespaced RoleBinding;
				// ensure the RoleBinding is namespaced (non-empty namespace).
				if ns == "" {
					t.Errorf("%s: RoleBinding %q binding ClusterRole %v has no namespace", vf, name, rr["name"])
				}
			}
		}
	}
}

// TestOperatorClusterRoleBoundPerManagedNamespace: SEC-09 — the manager
// ClusterRole is bound ONLY in the managed namespaces. The operator has no
// business in the release namespace (no Workspace CRs live there; leader
// election uses its own Role): binding manager-role there would let it read
// every platform Secret and create pods in the platform namespace.
func TestOperatorClusterRoleBoundPerManagedNamespace(t *testing.T) {
	docs := render(t, "example-values.yaml")
	bound := map[string]bool{}
	for _, d := range selectDocs(docs, "RoleBinding") {
		rr, _ := d["roleRef"].(map[string]any)
		rrName, _ := rr["name"].(string)
		if rrName != "manager-role" && !strings.HasSuffix(rrName, "-manager-role") {
			continue
		}
		_, ns := meta(d)
		for _, s := range toSlice(d["subjects"]) {
			sm, _ := s.(map[string]any)
			if sm["name"] == "operator" {
				bound[ns] = true
			}
		}
	}
	if bound["tcdi-system"] {
		t.Error("operator manager-role must NOT be bound in the release namespace (SEC-09)")
	}
	for _, ns := range managedNamespaces {
		if !bound[ns] {
			t.Errorf("operator manager-role not bound in %s", ns)
		}
	}
	if len(bound) != len(managedNamespaces) {
		t.Errorf("operator bound in unexpected namespaces: %v", bound)
	}
}

// TestOperatorWatchNamespacesExcludesRelease: SEC-09 — binding the manager
// role out of the release namespace only works if the informer cache stops
// watching it too, so --watch-namespaces must list exactly the managed
// namespaces (no tcdi-system entry).
func TestOperatorWatchNamespacesExcludesRelease(t *testing.T) {
	dep := deployment(render(t, "example-values.yaml"), "operator")
	for _, a := range firstContainerArgs(dep) {
		if !strings.HasPrefix(a, "--watch-namespaces=") {
			continue
		}
		got := strings.TrimPrefix(a, "--watch-namespaces=")
		for _, ns := range strings.Split(got, ",") {
			if ns == "tcdi-system" {
				t.Errorf("--watch-namespaces must not contain the release namespace, got %q", got)
			}
		}
		if got != "tinycdi-tenant-a,tinycdi-tenant-b" {
			t.Errorf("--watch-namespaces must be exactly the managed namespaces, got %q", got)
		}
		return
	}
	t.Error("operator args missing --watch-namespaces")
}

// TestOperatorPodNamespaceEnv: SEC-29 wiring — POD_NAMESPACE is injected
// via the downward API; the operator's --gateway-namespace defaults to it
// (runtime NetworkPolicies scope runtime ingress to gateway pods in the
// release namespace).
func TestOperatorPodNamespaceEnv(t *testing.T) {
	dep := deployment(render(t, "minimal-values.yaml"), "operator")
	cm := firstContainer(dep)
	found := false
	for _, e := range toSlice(cm["env"]) {
		em, _ := e.(map[string]any)
		if em["name"] != "POD_NAMESPACE" {
			continue
		}
		vf, _ := em["valueFrom"].(map[string]any)
		fr, _ := vf["fieldRef"].(map[string]any)
		if fr["fieldPath"] == "metadata.namespace" {
			found = true
		}
	}
	if !found {
		t.Error("operator pod must inject POD_NAMESPACE via fieldRef metadata.namespace (SEC-29)")
	}
}

func TestRuntimeServiceAccountAutomountFalse(t *testing.T) {
	docs := render(t, "example-values.yaml")
	found := map[string]bool{}
	for _, d := range selectDocs(docs, "ServiceAccount") {
		name, ns := meta(d)
		if name != "tinycdi-runtime" {
			continue
		}
		v, ok := d["automountServiceAccountToken"].(bool)
		if !ok || v {
			t.Errorf("runtime SA %s/%s: automountServiceAccountToken must be false, got %v", ns, name, d["automountServiceAccountToken"])
		}
		found[ns] = true
	}
	for _, ns := range managedNamespaces {
		if !found[ns] {
			t.Errorf("no tinycdi-runtime ServiceAccount in managed namespace %s", ns)
		}
	}
}

func TestDefaultDenyNetworkPolicyEverywhere(t *testing.T) {
	docs := render(t, "example-values.yaml")
	found := map[string]bool{}
	for _, d := range selectDocs(docs, "NetworkPolicy") {
		name, ns := meta(d)
		spec, _ := d["spec"].(map[string]any)
		sel, _ := spec["podSelector"].(map[string]any)
		if name == "default-deny" && len(sel) == 0 {
			types := toSlice(spec["policyTypes"])
			hasIn, hasEg := false, false
			for _, pt := range types {
				if pt == "Ingress" {
					hasIn = true
				}
				if pt == "Egress" {
					hasEg = true
				}
			}
			if hasIn && hasEg {
				found[ns] = true
			}
		}
	}
	for _, ns := range append([]string{"tcdi-system"}, managedNamespaces...) {
		if !found[ns] {
			t.Errorf("no Ingress+Egress default-deny NetworkPolicy in %s", ns)
		}
	}
}

func TestBackendEgressToRuntimeNamespaces(t *testing.T) {
	docs := render(t, "example-values.yaml")
	for _, d := range selectDocs(docs, "NetworkPolicy") {
		name, ns := meta(d)
		if name != "backend" || ns != "tcdi-system" {
			continue
		}
		spec, _ := d["spec"].(map[string]any)
		raw, _ := yaml.Marshal(spec["egress"])
		if !strings.Contains(string(raw), "workspaces.cdi.tinyorbit.vn/tenant") {
			t.Errorf("backend NetworkPolicy lacks egress to tenant namespaces (namespaceSelector on workspaces.cdi.tinyorbit.vn/tenant)")
		}
		return
	}
	t.Error("no backend NetworkPolicy in tcdi-system")
}

// TestBackendBrokerReachableFromOperator: the teardown finalizer's broker
// client (revoke/drain, ADR 0003) dials the backend internal mTLS listener
// on :9443. On an enforcing CNI both directions need a rule — backend
// ingress must accept operator pods and an operator egress policy must
// select backend on 9443 only (least privilege). Defect I-9: without these
// the finalizer stalls at CleanupRetry "broker unreachable".
func TestBackendBrokerReachableFromOperator(t *testing.T) {
	for _, vf := range lintValues {
		docs := render(t, vf)

		// backend ingress: operator pods allowed on the internal port only.
		apiFound := false
		for _, d := range selectDocs(docs, "NetworkPolicy") {
			name, ns := meta(d)
			if name != "backend" || ns != "tcdi-system" {
				continue
			}
			spec, _ := d["spec"].(map[string]any)
			for _, rule := range toSlice(spec["ingress"]) {
				rm, _ := rule.(map[string]any)
				var hasOp, has9443 bool
				for _, peer := range toSlice(rm["from"]) {
					pm, _ := peer.(map[string]any)
					sel, _ := pm["podSelector"].(map[string]any)
					lbls, _ := sel["matchLabels"].(map[string]any)
					if lbls["app.kubernetes.io/name"] == "operator" {
						hasOp = true
					}
				}
				for _, p := range toSlice(rm["ports"]) {
					pp, _ := p.(map[string]any)
					if pp["port"] == 9443 {
						has9443 = true
					}
				}
				if hasOp && has9443 {
					apiFound = true
				}
			}
		}
		if !apiFound {
			t.Errorf("%s: backend NetworkPolicy must allow ingress from operator pods on :9443 (broker revoke/drain)", vf)
		}

		// operator egress: a dedicated policy selecting backend pods on 9443
		// and nothing else.
		opFound := false
		for _, d := range selectDocs(docs, "NetworkPolicy") {
			name, ns := meta(d)
			if name != "operator" || ns != "tcdi-system" {
				continue
			}
			spec, _ := d["spec"].(map[string]any)
			sel, _ := spec["podSelector"].(map[string]any)
			lbls, _ := sel["matchLabels"].(map[string]any)
			if lbls["app.kubernetes.io/name"] != "operator" {
				t.Errorf("%s: operator NetworkPolicy must select operator pods, got %v", vf, lbls)
				continue
			}
			for _, rule := range toSlice(spec["egress"]) {
				rm, _ := rule.(map[string]any)
				var hasAPI, has9443 bool
				for _, peer := range toSlice(rm["to"]) {
					pm, _ := peer.(map[string]any)
					psel, _ := pm["podSelector"].(map[string]any)
					plbls, _ := psel["matchLabels"].(map[string]any)
					if plbls["app.kubernetes.io/name"] == "backend" {
						hasAPI = true
					}
				}
				for _, p := range toSlice(rm["ports"]) {
					pp, _ := p.(map[string]any)
					if pp["port"] == 9443 {
						has9443 = true
					}
				}
				if hasAPI && has9443 {
					opFound = true
				}
			}
		}
		if !opFound {
			t.Errorf("%s: no operator NetworkPolicy allowing egress to backend pods on :9443", vf)
		}
	}
}

func TestEveryContainerHardened(t *testing.T) {
	for _, vf := range lintValues {
		docs := render(t, vf)
		deps := selectDocs(docs, "Deployment")
		if len(deps) < 3 {
			t.Fatalf("%s: expected >=3 Deployments (backend/operator/frontend), got %d", vf, len(deps))
		}
		for _, d := range deps {
			name, _ := meta(d)
			spec, _ := d["spec"].(map[string]any)
			tpl, _ := spec["template"].(map[string]any)
			podSpec, _ := tpl["spec"].(map[string]any)

			podSC, _ := podSpec["securityContext"].(map[string]any)
			if podSC["runAsNonRoot"] != true {
				t.Errorf("%s: deployment %s pod securityContext.runAsNonRoot != true", vf, name)
			}
			for _, c := range toSlice(podSpec["containers"]) {
				cm, _ := c.(map[string]any)
				cn, _ := cm["name"].(string)

				res, _ := cm["resources"].(map[string]any)
				req, _ := res["requests"].(map[string]any)
				lim, _ := res["limits"].(map[string]any)
				if len(req) == 0 || len(lim) == 0 {
					t.Errorf("%s: %s/%s missing resource requests or limits", vf, name, cn)
				}
				if cm["readinessProbe"] == nil {
					t.Errorf("%s: %s/%s missing readinessProbe", vf, name, cn)
				}
				sc, _ := cm["securityContext"].(map[string]any)
				if sc["runAsNonRoot"] != true && podSC["runAsNonRoot"] != true {
					t.Errorf("%s: %s/%s not non-root", vf, name, cn)
				}
				if sc["allowPrivilegeEscalation"] != false {
					t.Errorf("%s: %s/%s allowPrivilegeEscalation != false", vf, name, cn)
				}
				caps, _ := sc["capabilities"].(map[string]any)
				drops := toSlice(caps["drop"])
				dropAll := false
				for _, dr := range drops {
					if dr == "ALL" {
						dropAll = true
					}
				}
				if !dropAll {
					t.Errorf("%s: %s/%s does not drop ALL capabilities", vf, name, cn)
				}
			}
		}
	}
}

func TestNoPlaintextSecrets(t *testing.T) {
	for _, vf := range lintValues {
		docs := render(t, vf)
		for _, d := range docs {
			name, ns := meta(d)
			if d["kind"] == "Secret" {
				t.Errorf("%s: chart rendered Secret %s/%s — secrets must be referenced by name only", vf, ns, name)
			}
			walk(d, []string{"stringData"}, func(m map[string]any) {
				t.Errorf("%s: %v/%s contains stringData", vf, d["kind"], name)
			})
		}
	}
}

func TestNoClusterWideDependencies(t *testing.T) {
	for _, vf := range lintValues {
		docs := render(t, vf)
		for _, d := range docs {
			gv, _ := d["apiVersion"].(string)
			kind, _ := d["kind"].(string)
			banned := []string{"kubevirt.io", "cdi.kubevirt.io", "VirtualMachine", "DataVolume"}
			for _, b := range banned {
				if strings.Contains(gv, b) || strings.Contains(kind, b) {
					t.Errorf("%s: rendered %s %q — cluster-wide dependency must never ship", vf, kind, gv)
				}
			}
		}
	}
}

func TestCRDsShippedButNotDeletedByUninstall(t *testing.T) {
	// CRDs live under crds/: helm template renders them, helm uninstall never
	// touches them. Assert they render so the chart actually ships them.
	docs := render(t, "example-values.yaml")
	crds := selectDocs(docs, "CustomResourceDefinition")
	if len(crds) < 2 {
		t.Fatalf("expected >=2 CRDs (workspaces, workspacetemplates), got %d", len(crds))
	}
}

func TestPortalAndSessionHostsDiffer(t *testing.T) {
	out := renderBad(t, "equal-hosts-values.yaml")
	if !strings.Contains(out, "portalHost") && !strings.Contains(out, "session") {
		t.Errorf("render failure should mention the host collision, got: %s", out)
	}
}

func TestExampleValuesDifferingHostsRender(t *testing.T) {
	docs := render(t, "example-values.yaml")
	raw, _ := yaml.Marshal(docs)
	s := string(raw)
	if !strings.Contains(s, "portal.lab.example.net") || !strings.Contains(s, "session.lab.example.net") {
		t.Errorf("example render must contain the portal and session hosts")
	}
}

func TestSecretsReferencedByName(t *testing.T) {
	docs := render(t, "example-values.yaml")
	raw, _ := yaml.Marshal(docs)
	s := string(raw)
	for _, want := range []string{"tinycdi-backend-db", "tinycdi-oidc-client", "tinycdi-backend-app-tls", "tinycdi-backend-session-tls", "tinycdi-backend-internal-tls", "tinycdi-frontend-tls", "tinycdi-backend-login-keys"} {
		if !strings.Contains(s, want) {
			t.Errorf("rendered chart must reference existing secret %q", want)
		}
	}
}

// TestServiceMonitor: serviceMonitor.enabled renders a monitor only for
// the backend metrics endpoint (SEC-33: the operator has no metrics
// endpoint — its secure mode needs cluster-scoped TokenReview/SAR RBAC the
// chart never grants, so operator metrics are OFF) on the dedicated
// non-public backend-metrics Service.
func TestServiceMonitor(t *testing.T) {
	docs := render(t, "example-values.yaml")
	mons := selectDocs(docs, "ServiceMonitor")
	if len(mons) != 1 {
		t.Fatalf("expected 1 ServiceMonitor (backend metrics only), got %d", len(mons))
	}
	name, _ := meta(mons[0])
	if name != "backend" {
		t.Errorf("expected the backend ServiceMonitor, got %q", name)
	}
	svcFound := false
	for _, d := range selectDocs(docs, "Service") {
		name, _ := meta(d)
		if name == "operator-metrics" {
			t.Error("operator-metrics Service must not exist — operator metrics are disabled")
		}
		if name == "backend-metrics" {
			svcFound = true
			spec, _ := d["spec"].(map[string]any)
			if spec["type"] != "ClusterIP" {
				t.Errorf("backend-metrics Service must be ClusterIP (never the public port), got %v", spec["type"])
			}
		}
	}
	if !svcFound {
		t.Error("backend-metrics Service missing although backend.metrics.enabled is set")
	}
	// No monitors when metrics are off even if serviceMonitor.enabled.
	docs = renderArgs(t,
		"-f", filepath.Join("tinycdi", "ci", "minimal-values.yaml"),
		"--set", "serviceMonitor.enabled=true")
	if n := len(selectDocs(docs, "ServiceMonitor")); n != 0 {
		t.Errorf("serviceMonitor.enabled with no metrics endpoints must render no monitors, got %d", n)
	}
}

// TestMetricsNotOnPublicBackendService: SEC-33 — the public backend
// Service must expose only the app/session/internal listeners; the metrics
// port is served by the separate ClusterIP backend-metrics Service,
// scraped only by the declared prometheusPeers through a
// backend-pod-scoped NetworkPolicy.
func TestMetricsNotOnPublicBackendService(t *testing.T) {
	docs := render(t, "example-values.yaml")
	for _, d := range selectDocs(docs, "Service") {
		name, _ := meta(d)
		if name != "backend" {
			continue
		}
		spec, _ := d["spec"].(map[string]any)
		for _, p := range toSlice(spec["ports"]) {
			pm, _ := p.(map[string]any)
			if pm["name"] == "metrics" {
				t.Errorf("public backend Service must not expose the metrics port, got %v", pm)
			}
		}
	}
	// the scrape NetworkPolicy must select backend pods — never {} (which
	// would also cover frontend/operator pods).
	found := false
	for _, d := range selectDocs(docs, "NetworkPolicy") {
		name, ns := meta(d)
		if name != "allow-metrics-scrape" || ns != "tcdi-system" {
			continue
		}
		found = true
		spec, _ := d["spec"].(map[string]any)
		sel, _ := spec["podSelector"].(map[string]any)
		if len(sel) == 0 {
			t.Error("allow-metrics-scrape podSelector must not be empty (SEC-33)")
		}
		ml, _ := sel["matchLabels"].(map[string]any)
		if ml["app.kubernetes.io/name"] != "backend" {
			t.Errorf("allow-metrics-scrape must select backend pods, got %v", sel)
		}
		raw, _ := yaml.Marshal(spec)
		if !strings.Contains(string(raw), "kubernetes.io/metadata.name: monitoring") {
			t.Errorf("scrape policy must use networkPolicy.prometheusPeers, spec:\n%s", raw)
		}
	}
	if !found {
		t.Error("no allow-metrics-scrape NetworkPolicy although backend.metrics.enabled is set")
	}
}

// TestMetricsEmptyPeersFailClosed: SEC-32 — enabling backend metrics with
// an empty prometheusPeers list would open scraping to every namespace;
// the render must fail instead.
func TestMetricsEmptyPeersFailClosed(t *testing.T) {
	out := renderErrArgs(t,
		"-f", filepath.Join("tinycdi", "ci", "minimal-values.yaml"),
		"--set", "backend.metrics.enabled=true",
		"--set", "networkPolicy.prometheusPeers=null")
	if !strings.Contains(out, "prometheusPeers") {
		t.Errorf("empty prometheusPeers with metrics enabled should fail mentioning prometheusPeers, got: %s", out)
	}
}

// TestNetworkPolicyEmptyPeersFailClosed: SEC-32 — empty peer lists in the
// api egress rules render `to: []` which matches EVERYWHERE; the render
// must fail closed instead.
func TestNetworkPolicyEmptyPeersFailClosed(t *testing.T) {
	for _, tc := range []struct {
		set, want string
	}{
		{"database.allowedPeers=null", "database.allowedPeers"},
		{"oidc.egressCIDRs=null", "oidc.egressCIDRs"},
	} {
		out := renderErrArgs(t,
			"-f", filepath.Join("tinycdi", "ci", "minimal-values.yaml"),
			"--set", tc.set)
		if !strings.Contains(out, tc.want) {
			t.Errorf("--set %s should fail the render mentioning %s, got: %s", tc.set, tc.want, out)
		}
	}
}

// TestPDBToggle: backend.pdb is ON by default (D22 — two replicas need
// minAvailable during voluntary disruption); frontend.pdb renders when
// enabled, operator keeps podDisruptionBudget.
func TestPDBToggle(t *testing.T) {
	docs := render(t, "minimal-values.yaml")
	pdbs := selectDocs(docs, "PodDisruptionBudget")
	byName := map[string]doc{}
	for _, p := range pdbs {
		name, _ := meta(p)
		byName[name] = p
	}
	bp, ok := byName["backend"]
	if !ok {
		t.Fatal("no backend PDB rendered by default (D22)")
	}
	spec, _ := bp["spec"].(map[string]any)
	sel, _ := spec["selector"].(map[string]any)
	ml, _ := sel["matchLabels"].(map[string]any)
	if ml["app.kubernetes.io/name"] != "backend" {
		t.Errorf("backend PDB must select backend pods, got %v", ml)
	}
	if spec["minAvailable"] != 1 {
		t.Errorf("backend PDB minAvailable = %v, want 1", spec["minAvailable"])
	}
	// maxUnavailable wins when set.
	docs = renderArgs(t,
		"-f", filepath.Join("tinycdi", "ci", "minimal-values.yaml"),
		"--set", "frontend.pdb.enabled=true", "--set", "frontend.pdb.maxUnavailable=1")
	var fp doc
	for _, p := range selectDocs(docs, "PodDisruptionBudget") {
		if n, _ := meta(p); n == "frontend" {
			fp = p
		}
	}
	if fp == nil {
		t.Fatal("frontend.pdb.enabled did not render a frontend PDB")
	}
	fspec, _ := fp["spec"].(map[string]any)
	if fspec["maxUnavailable"] != 1 {
		t.Errorf("frontend PDB maxUnavailable = %v, want 1", fspec["maxUnavailable"])
	}
}

// deployment returns the rendered Deployment doc named name, or nil.
func deployment(docs []doc, name string) doc {
	for _, d := range selectDocs(docs, "Deployment") {
		n, _ := meta(d)
		if n == name {
			return d
		}
	}
	return nil
}

func firstContainer(d doc) map[string]any {
	spec, _ := d["spec"].(map[string]any)
	tpl, _ := spec["template"].(map[string]any)
	podSpec, _ := tpl["spec"].(map[string]any)
	containers := toSlice(podSpec["containers"])
	if len(containers) == 0 {
		return nil
	}
	cm, _ := containers[0].(map[string]any)
	return cm
}

func firstContainerImage(d doc) string {
	if d == nil {
		return ""
	}
	img, _ := firstContainer(d)["image"].(string)
	return img
}

// firstContainerArgs returns the rendered args of the first container of
// a Deployment doc.
func firstContainerArgs(d doc) []string {
	cm := firstContainer(d)
	var out []string
	for _, a := range toSlice(cm["args"]) {
		if s, ok := a.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// secretNames returns every secret.secretName referenced by the pod
// volumes of a Deployment doc.
func secretNames(d doc) map[string]bool {
	out := map[string]bool{}
	spec, _ := d["spec"].(map[string]any)
	tpl, _ := spec["template"].(map[string]any)
	podSpec, _ := tpl["spec"].(map[string]any)
	for _, v := range toSlice(podSpec["volumes"]) {
		vm, _ := v.(map[string]any)
		sec, _ := vm["secret"].(map[string]any)
		if n, ok := sec["secretName"].(string); ok {
			out[n] = true
		}
	}
	return out
}

// TestOperatorBrokerClientWired: the operator deployment carries the
// internal-broker flags (ADR 0003) and mounts the configured CA bundle and
// operator client-cert secrets — secrets referenced by name only.
func TestOperatorBrokerClientWired(t *testing.T) {
	for _, vf := range []string{"example-values.yaml", "minimal-values.yaml"} {
		dep := deployment(render(t, vf), "operator")
		if dep == nil {
			t.Fatalf("%s: no operator Deployment rendered", vf)
		}
		args := strings.Join(firstContainerArgs(dep), "\n")
		for _, want := range []string{
			"--broker-internal-url=https://backend.tcdi-system.svc:9443",
			"--broker-ca-file=/etc/broker-ca/",
			"--broker-client-cert-file=/mtls/tls.crt",
			"--broker-client-key-file=/mtls/tls.key",
			"--watch-namespaces=tinycdi-tenant-a,tinycdi-tenant-b",
		} {
			if !strings.Contains(args, want) {
				t.Errorf("%s: operator args missing %q\nargs:\n%s", vf, want, args)
			}
		}
		if strings.Contains(args, "--dev-allow-no-broker") {
			t.Errorf("%s: operator must not render --dev-allow-no-broker by default", vf)
		}
	}
	// example values point the CA at tinycdi-extra-ca and the client cert
	// at tinycdi-operator-mtls; both must be mounted as named secrets.
	vols := secretNames(deployment(render(t, "example-values.yaml"), "operator"))
	for _, want := range []string{"tinycdi-extra-ca", "tinycdi-operator-mtls"} {
		if !vols[want] {
			t.Errorf("operator pod missing volume for secret %q", want)
		}
	}
}

// TestBackendControlToken: the structured backend.controlToken value mounts
// the token secret and passes -control-token-file; without it (minimal
// values) no control flag is rendered and the routes stay closed.
func TestBackendControlToken(t *testing.T) {
	dep := deployment(render(t, "example-values.yaml"), "backend")
	if dep == nil {
		t.Fatal("no backend Deployment rendered")
	}
	args := strings.Join(firstContainerArgs(dep), "\n")
	if !strings.Contains(args, "-control-token-file=/control/token") {
		t.Errorf("backend args missing -control-token-file=/control/token\nargs:\n%s", args)
	}
	if !secretNames(dep)["tinycdi-backend-control-token"] {
		t.Error("backend pod missing volume for secret tinycdi-backend-control-token")
	}

	dep = deployment(render(t, "minimal-values.yaml"), "backend")
	args = strings.Join(firstContainerArgs(dep), "\n")
	if strings.Contains(args, "control-token") {
		t.Errorf("minimal render must not configure a control token (routes stay closed)\nargs:\n%s", args)
	}
}

// TestBackendPortalOrigin: the session listener must receive the portal
// origin derived from portalHost (ADR 0004) — without it every browser
// launch POST is rejected as cross-origin.
func TestBackendPortalOrigin(t *testing.T) {
	for _, vf := range []string{"example-values.yaml", "minimal-values.yaml"} {
		dep := deployment(render(t, vf), "backend")
		if dep == nil {
			t.Fatalf("%s: no backend Deployment rendered", vf)
		}
		args := strings.Join(firstContainerArgs(dep), "\n")
		if !strings.Contains(args, "-portal-origin=https://portal.lab.example.net") {
			t.Errorf("%s: backend args missing -portal-origin=https://portal.lab.example.net\nargs:\n%s", vf, args)
		}
	}
}

// TestFrontendSessionOrigin: the frontend must receive the session origin
// derived from sessionHost — the SPA opens sessions by POSTing a
// cross-origin launch form to https://<sessionHost>, and the portal CSP
// form-action blocks every launch unless that origin is configured
// (launch regression).
func TestFrontendSessionOrigin(t *testing.T) {
	for _, vf := range []string{"example-values.yaml", "minimal-values.yaml"} {
		dep := deployment(render(t, vf), "frontend")
		if dep == nil {
			t.Fatalf("%s: no frontend Deployment rendered", vf)
		}
		args := strings.Join(firstContainerArgs(dep), "\n")
		if !strings.Contains(args, "-session-origin=https://session.lab.example.net") {
			t.Errorf("%s: frontend args missing -session-origin=https://session.lab.example.net\nargs:\n%s", vf, args)
		}
	}
}

// firstContainerEnv returns the rendered env entries of the first container
// of a Deployment doc as name -> value.
func firstContainerEnv(d doc) map[string]string {
	spec, _ := d["spec"].(map[string]any)
	tpl, _ := spec["template"].(map[string]any)
	podSpec, _ := tpl["spec"].(map[string]any)
	containers := toSlice(podSpec["containers"])
	if len(containers) == 0 {
		return nil
	}
	cm, _ := containers[0].(map[string]any)
	out := map[string]string{}
	for _, e := range toSlice(cm["env"]) {
		em, _ := e.(map[string]any)
		n, _ := em["name"].(string)
		v, _ := em["value"].(string)
		if n != "" {
			out[n] = v
		}
	}
	return out
}

// TestBackendPortalOriginAllowlist: the backend must get its portal Origin
// allowlist (-portal-origin, repeatable) derived from portalHost — the
// edge routes /v1 to the app listener, so every legitimate browser Origin
// is https://<portalHost>. example-values adds one extraPortalOrigins
// entry.
func TestBackendPortalOriginAllowlist(t *testing.T) {
	for _, vf := range []string{"example-values.yaml", "minimal-values.yaml"} {
		dep := deployment(render(t, vf), "backend")
		if dep == nil {
			t.Fatalf("%s: no backend Deployment rendered", vf)
		}
		found := false
		for _, a := range firstContainerArgs(dep) {
			if a == "-portal-origin=https://portal.lab.example.net" {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: backend args missing -portal-origin=https://portal.lab.example.net\nargs:\n%s", vf, strings.Join(firstContainerArgs(dep), "\n"))
		}
	}
	args := strings.Join(firstContainerArgs(deployment(render(t, "example-values.yaml"), "backend")), "\n")
	if !strings.Contains(args, "-portal-origin=https://alt.portal.example.net") {
		t.Errorf("example render: extraPortalOrigins entry missing\nargs:\n%s", args)
	}
}

func toSlice(v any) []any {
	s, _ := v.([]any)
	return s
}

// templateRevisions maps catalog-name -> rendered object name for every
// WorkspaceTemplate doc.
func templateRevisions(docs []doc) map[string]string {
	out := map[string]string{}
	for _, d := range selectDocs(docs, "WorkspaceTemplate") {
		m, _ := d["metadata"].(map[string]any)
		lbls, _ := m["labels"].(map[string]any)
		cn, _ := lbls["workspaces.cdi.tinyorbit.vn/catalog-name"].(string)
		name, _ := m["name"].(string)
		if cn == "" {
			cn = name
		}
		out[cn] = name
	}
	return out
}

// TestSeededTemplateImmutableRevisions covers defect F1: seeded
// WorkspaceTemplates are CEL-immutable, so the chart must publish a NEW
// revision object per rendered content — name "<name>-<hash8>" — rather
// than mutate in place (which made helm upgrade/rollback fail with
// "spec is immutable"). A values change must render a new name; an
// unchanged template must keep the same name (idempotent render).
func TestSeededTemplateImmutableRevisions(t *testing.T) {
	docs := render(t, "example-values.yaml")
	tpls := selectDocs(docs, "WorkspaceTemplate")
	if len(tpls) == 0 {
		t.Fatal("no seeded WorkspaceTemplate rendered")
	}
	suffix := regexp.MustCompile(`^([a-z0-9-]+)-[0-9a-f]{8}$`)
	for _, d := range tpls {
		name, ns := meta(d)
		m, _ := d["metadata"].(map[string]any)
		lbls, _ := m["labels"].(map[string]any)
		cn, _ := lbls["workspaces.cdi.tinyorbit.vn/catalog-name"].(string)
		if cn == "" {
			t.Errorf("template %s/%s missing workspaces.cdi.tinyorbit.vn/catalog-name label", ns, name)
		}
		mm := suffix.FindStringSubmatch(name)
		if mm == nil || mm[1] != cn {
			t.Errorf("template name %q must be <catalog-name>-<hash8>, got catalog-name %q", name, cn)
		}
		// standard chart labels ride along so release-owned cleanup keeps working
		if lbls["app.kubernetes.io/instance"] != "tcdi" {
			t.Errorf("template %s missing release instance label", name)
		}
	}

	// template-revision-values bumps linuxdesk1's spec.revision only:
	// its object name must change; browser01's must not.
	v1 := templateRevisions(render(t, "example-values.yaml"))
	v2 := templateRevisions(render(t, "template-revision-values.yaml"))
	if v1["linuxdesk1"] == "" || v2["linuxdesk1"] == "" {
		t.Fatalf("expected linuxdesk1 seeded template in both renders, got %v / %v", v1, v2)
	}
	if v1["linuxdesk1"] == v2["linuxdesk1"] {
		t.Errorf("spec change must render a new revision object name, got %q both times", v1["linuxdesk1"])
	}
	if v1["browser01"] != v2["browser01"] {
		t.Errorf("unchanged template must keep the same name: %q -> %q", v1["browser01"], v2["browser01"])
	}
}

// ---------------------------------------------------------------------------
// nodeProfiles.install — the optional per-node browser-sandbox profile
// installer (seccomp + AppArmor Localhost profiles).
// ---------------------------------------------------------------------------

// TestNodeProfilesFilesInSync: the copies under
// tinycdi/files/node-profiles/ must be BYTE-IDENTICAL to the canonical
// profiles in deploy/node-profiles/ — the repo copies stay the single
// source of truth and drift here ships a different profile than reviewed.
func TestNodeProfilesFilesInSync(t *testing.T) {
	pairs := [][2]string{
		{"seccomp/chromium-userns.json", "seccomp/chromium-userns.json"},
		{"apparmor/tinycdi-browser", "apparmor/tinycdi-browser"},
	}
	for _, p := range pairs {
		canon := filepath.Join(repoRoot(t), "deploy", "node-profiles", p[0])
		shipped := filepath.Join(repoRoot(t), "deploy", "helm", "tinycdi", "files", "node-profiles", p[1])
		a, err := os.ReadFile(canon)
		if err != nil {
			t.Fatalf("read %s: %v", canon, err)
		}
		b, err := os.ReadFile(shipped)
		if err != nil {
			t.Fatalf("read %s: %v", shipped, err)
		}
		if !bytes.Equal(a, b) {
			t.Errorf("%s diverged from deploy/node-profiles/%s — copy it over, never edit the chart copy", p[1], p[0])
		}
	}
}

func daemonSet(docs []doc, name string) doc {
	for _, d := range selectDocs(docs, "DaemonSet") {
		if n, _ := meta(d); n == name {
			return d
		}
	}
	return nil
}

func dsPodSpec(d doc) map[string]any {
	spec, _ := d["spec"].(map[string]any)
	tpl, _ := spec["template"].(map[string]any)
	ps, _ := tpl["spec"].(map[string]any)
	return ps
}

// TestNodeProfilesOffByDefault: no DaemonSet/ServiceAccount/ConfigMap named
// node-profiles renders unless nodeProfiles.install.enabled, and no other
// values set may produce a privileged or hostPID pod.
// (Stock defaults alone no longer render — CHTR-7 requires a real
// database.allowedPeers; ci/minimal-values.yaml supplies the required
// plumbing while keeping nodeProfiles.install off.)
func TestNodeProfilesOffByDefault(t *testing.T) {
	for _, vf := range []string{"minimal-values.yaml", "example-values.yaml", "ingress-certmanager-values.yaml", "gateway-api-values.yaml", "template-revision-values.yaml"} {
		docs := render(t, vf)
		for _, d := range docs {
			name, _ := meta(d)
			if name == "node-profiles" {
				t.Errorf("%s: %s/node-profiles rendered without nodeProfiles.install.enabled", vf, d["kind"])
			}
		}
		for _, d := range selectDocs(docs, "DaemonSet") {
			name, _ := meta(d)
			t.Errorf("%s: unexpected DaemonSet %q without nodeProfiles.install.enabled", vf, name)
		}
		// no privileged/hostPID pod anywhere in the off-by-default renders
		for _, d := range docs {
			walk(d, []string{"privileged"}, func(m map[string]any) {
				if m["privileged"] == true {
					name, _ := meta(d)
					t.Errorf("%s: %s/%s contains a privileged container without the installer enabled", vf, d["kind"], name)
				}
			})
			walk(d, []string{"hostPID"}, func(m map[string]any) {
				if m["hostPID"] == true {
					name, _ := meta(d)
					t.Errorf("%s: %s/%s sets hostPID without the installer enabled", vf, d["kind"], name)
				}
			})
		}
	}
}

// TestNodeProfilesDaemonSet: SEC-06/SEC-37 — with install.enabled the
// render must carry a dedicated installer Namespace (PSS privileged, chart
// managed, NOT the release namespace), the SA (automount off), an
// immutable content-hashed ConfigMap and one DaemonSet whose PRIVILEGED
// work is confined to an initContainer while the long-running container is
// a caps-dropped uid-0 verifier over read-only mounts. No RBAC anywhere.
func TestNodeProfilesDaemonSet(t *testing.T) {
	docs := render(t, "node-profiles-values.yaml")
	const insNS = "tinycdi-node-profiles"

	// dedicated namespace, chart-created, PSS privileged — the release
	// namespace stays baseline/restricted.
	nsFound := false
	for _, d := range selectDocs(docs, "Namespace") {
		if n, _ := meta(d); n == insNS {
			nsFound = true
			m, _ := d["metadata"].(map[string]any)
			lbls, _ := m["labels"].(map[string]any)
			if lbls["pod-security.kubernetes.io/enforce"] != "privileged" {
				t.Errorf("installer namespace must enforce privileged PSS, got %v", lbls)
			}
		}
	}
	if !nsFound {
		t.Errorf("no %s Namespace rendered with nodeProfiles.install.createNamespace", insNS)
	}

	ds := daemonSet(docs, "node-profiles")
	if ds == nil {
		t.Fatal("no node-profiles DaemonSet rendered with nodeProfiles.install.enabled")
	}
	if _, ns := meta(ds); ns != insNS {
		t.Errorf("DaemonSet must live in the dedicated installer namespace %q, got %q", insNS, ns)
	}
	podSpec := dsPodSpec(ds)
	if podSpec["hostPID"] != true {
		t.Error("installer pod must set hostPID (nsenter into PID 1 mount ns + containerd label discovery)")
	}
	if v, ok := podSpec["automountServiceAccountToken"].(bool); !ok || v {
		t.Errorf("installer pod automountServiceAccountToken must be false, got %v", podSpec["automountServiceAccountToken"])
	}
	if podSpec["serviceAccountName"] != "node-profiles" {
		t.Errorf("installer pod serviceAccountName = %v, want node-profiles", podSpec["serviceAccountName"])
	}

	// dedicated SA, token automount off at BOTH levels, zero RBAC subjects.
	saFound := false
	for _, d := range selectDocs(docs, "ServiceAccount") {
		n, ns := meta(d)
		if n == "node-profiles" {
			saFound = true
			if ns != insNS {
				t.Errorf("node-profiles SA must be in %q, got %q", insNS, ns)
			}
			if v, ok := d["automountServiceAccountToken"].(bool); !ok || v {
				t.Errorf("node-profiles SA automountServiceAccountToken must be false, got %v", d["automountServiceAccountToken"])
			}
		}
	}
	if !saFound {
		t.Error("no node-profiles ServiceAccount rendered")
	}

	// SEC-06: NOTHING in the installer namespace may grant configmaps or
	// pods write — there must be no RBAC objects there at all, and no
	// binding anywhere may carry the installer namespace or SA as subject.
	for _, d := range docs {
		name, ns := meta(d)
		switch d["kind"] {
		case "Role", "RoleBinding":
			if ns == insNS {
				t.Errorf("%s %s/%s must not exist — the installer namespace carries no RBAC", d["kind"], ns, name)
			}
		}
		for _, s := range toSlice(d["subjects"]) {
			sm, _ := s.(map[string]any)
			if sm["kind"] == "ServiceAccount" && sm["name"] == "node-profiles" {
				t.Errorf("%s/%s binds the node-profiles SA — the installer needs NO RBAC", d["kind"], name)
			}
			if sns, _ := sm["namespace"].(string); sns == insNS {
				t.Errorf("%s/%s binds a subject in the installer namespace %s", d["kind"], name, insNS)
			}
		}
	}

	// initContainer: privileged + RO rootfs, one-shot install, digest image,
	// default resources.
	initCs := toSlice(podSpec["initContainers"])
	if len(initCs) != 1 {
		t.Fatalf("expected exactly 1 initContainer (the privileged installer), got %d", len(initCs))
	}
	initC, _ := initCs[0].(map[string]any)
	sc, _ := initC["securityContext"].(map[string]any)
	if sc["privileged"] != true {
		t.Error("installer initContainer must be privileged (host writes via hostPath + nsenter)")
	}
	if sc["readOnlyRootFilesystem"] != true {
		t.Error("installer initContainer must keep a read-only root filesystem")
	}
	if img, _ := initC["image"].(string); img != "alpine@sha256:ce64758a109eb420d874a118f87920e625e12d3634e03b4a5573fd9f6e5d3507" {
		t.Errorf("default installer image must be the digest-pinned alpine, got %q", img)
	}
	env := map[string]string{}
	for _, e := range toSlice(initC["env"]) {
		em, _ := e.(map[string]any)
		env[em["name"].(string)], _ = em["value"].(string)
	}
	if env["NODE_PROFILES_ONESHOT"] != "1" {
		t.Error("installer must run one-shot inside the initContainer (NODE_PROFILES_ONESHOT=1)")
	}
	if env["NODE_PROFILES_MODE"] != "install" {
		t.Errorf("default NODE_PROFILES_MODE = %v, want install", env["NODE_PROFILES_MODE"])
	}
	res, _ := initC["resources"].(map[string]any)
	if len(res["requests"].(map[string]any)) == 0 || len(res["limits"].(map[string]any)) == 0 {
		t.Error("installer initContainer must carry default resources (SEC-37)")
	}

	// long-running container: UNPRIVILEGED verifier — exec readiness gate on
	// verify.sh, read-only hostPath mounts, no privilege at all.
	containers := toSlice(podSpec["containers"])
	if len(containers) != 1 {
		t.Fatalf("expected exactly 1 verifier container, got %d", len(containers))
	}
	c, _ := containers[0].(map[string]any)
	vsc, _ := c["securityContext"].(map[string]any)
	// CHTR-1: uid 0 is REQUIRED — apparmorfs serves the loaded-profiles
	// file only to root (a non-root verifier can never report Ready).
	// With ALL capabilities dropped, no privilege escalation and a
	// read-only rootfs + mounts, that uid is strictly a reader.
	if vsc["privileged"] == true || vsc["allowPrivilegeEscalation"] != false {
		t.Errorf("verifier container must be unprivileged, securityContext %v", vsc)
	}
	if vsc["runAsUser"] != 0 && vsc["runAsUser"] != float64(0) {
		t.Errorf("verifier must run as uid 0 to read the AppArmor loaded list, got %v", vsc["runAsUser"])
	}
	if vsc["runAsNonRoot"] == true {
		t.Error("verifier cannot be runAsNonRoot (CHTR-1: it needs uid 0)")
	}
	if vsc["readOnlyRootFilesystem"] != true {
		t.Errorf("verifier must keep a read-only rootfs, got %v", vsc)
	}
	caps, _ := vsc["capabilities"].(map[string]any)
	dropAll := false
	for _, dr := range toSlice(caps["drop"]) {
		if dr == "ALL" {
			dropAll = true
		}
	}
	if !dropAll {
		t.Error("verifier must drop ALL capabilities")
	}
	probe, _ := c["readinessProbe"].(map[string]any)
	ex, _ := probe["exec"].(map[string]any)
	if raw, _ := yaml.Marshal(ex["command"]); !strings.Contains(string(raw), "verify.sh") {
		t.Errorf("readinessProbe must exec verify.sh, got %v", ex["command"])
	}
	for _, vm := range toSlice(c["volumeMounts"]) {
		vmm, _ := vm.(map[string]any)
		name, _ := vmm["name"].(string)
		if strings.HasPrefix(name, "host-") && vmm["readOnly"] != true {
			t.Errorf("verifier hostPath mount %q must be readOnly", name)
		}
	}
	res, _ = c["resources"].(map[string]any)
	if len(res) == 0 {
		t.Error("verifier container must carry resources (SEC-37)")
	}

	// the ONLY privileged / hostPID pod in the whole render.
	for _, d := range docs {
		if d["kind"] == "DaemonSet" {
			continue
		}
		walk(d, []string{"privileged"}, func(m map[string]any) {
			if m["privileged"] == true {
				name, _ := meta(d)
				t.Errorf("%s/%s is privileged — only the node-profiles installer initContainer may be", d["kind"], name)
			}
		})
		walk(d, []string{"hostPID"}, func(m map[string]any) {
			if m["hostPID"] == true {
				name, _ := meta(d)
				t.Errorf("%s/%s sets hostPID — only the node-profiles DaemonSet may", d["kind"], name)
			}
		})
	}
	// no privileged LONG-RUNNING container even inside the DaemonSet.
	for _, cm := range containers {
		cc, _ := cm.(map[string]any)
		csc, _ := cc["securityContext"].(map[string]any)
		if csc["privileged"] == true {
			t.Error("the long-running verifier container must not be privileged")
		}
	}

	// hostPath volumes: ONLY <kubeletRoot>/seccomp/profiles (never the whole
	// kubelet root — it holds every pod's projected secrets), the fixed
	// /etc/apparmor.d and a read-only /sys for the AppArmor loaded-list.
	hostPaths := map[string]string{}
	for _, v := range toSlice(podSpec["volumes"]) {
		vm, _ := v.(map[string]any)
		hp, _ := vm["hostPath"].(map[string]any)
		if p, ok := hp["path"].(string); ok {
			hostPaths[p] = p
		}
	}
	if hostPaths["/var/lib/kubelet/seccomp/profiles"] == "" || hostPaths["/etc/apparmor.d"] == "" {
		t.Errorf("expected hostPath /var/lib/kubelet/seccomp/profiles and /etc/apparmor.d, got %v", hostPaths)
	}
	if hostPaths["/var/lib/kubelet"] != "" {
		t.Error("must NOT mount the whole kubelet root — it holds every pod's projected secrets")
	}

	// kubeletRoot drives the seccomp hostPath.
	custom := renderArgs(t,
		"-f", filepath.Join("tinycdi", "ci", "node-profiles-values.yaml"),
		"--set", "nodeProfiles.install.kubeletRoot=/custom/kubelet-root")
	found := false
	for _, v := range toSlice(dsPodSpec(daemonSet(custom, "node-profiles"))["volumes"]) {
		vm, _ := v.(map[string]any)
		hp, _ := vm["hostPath"].(map[string]any)
		if hp["path"] == "/custom/kubelet-root/seccomp/profiles" {
			found = true
		}
	}
	if !found {
		t.Error("kubeletRoot override did not reach the host-seccomp hostPath (<root>/seccomp/profiles)")
	}

	// ConfigMap: immutable + content-hashed name (SEC-06 — a compromised SA
	// must not be able to swap the scripts under a running pod).
	var cmName string
	for _, d := range selectDocs(docs, "ConfigMap") {
		n, ns := meta(d)
		if !strings.HasPrefix(n, "node-profiles-") {
			continue
		}
		cmName = n
		if ns != insNS {
			t.Errorf("node-profiles ConfigMap must be in %q, got %q", insNS, ns)
		}
		if d["immutable"] != true {
			t.Error("node-profiles ConfigMap must be immutable: true")
		}
		suffix := strings.TrimPrefix(n, "node-profiles-")
		if len(suffix) != 8 {
			t.Errorf("ConfigMap name must carry an 8-char content hash, got %q", n)
		}
		data, _ := d["data"].(map[string]any)
		for _, k := range []string{"chromium-userns.json", "tinycdi-browser", "install.sh", "verify.sh"} {
			if _, ok := data[k]; !ok {
				t.Errorf("node-profiles ConfigMap missing data key %q", k)
			}
		}
		// shipped AppArmor copy must still carry the placeholder — it is
		// resolved on the node, not at render time.
		aa, _ := data["tinycdi-browser"].(string)
		if !strings.Contains(aa, "peer=cri-containerd") {
			t.Error("ConfigMap apparmor profile lost the peer=cri-containerd placeholder")
		}
	}
	if cmName == "" {
		t.Fatal("no node-profiles-<hash> ConfigMap rendered")
	}
	// the DaemonSet volume must reference the hashed ConfigMap name.
	volFound := false
	for _, v := range toSlice(podSpec["volumes"]) {
		vm, _ := v.(map[string]any)
		cm, _ := vm["configMap"].(map[string]any)
		if cm["name"] == cmName {
			volFound = true
		}
	}
	if !volFound {
		t.Errorf("DaemonSet must mount the content-hashed ConfigMap %q", cmName)
	}

	// default placement: linux + not control-plane.
	sel, _ := podSpec["nodeSelector"].(map[string]any)
	if sel["kubernetes.io/os"] != "linux" {
		t.Errorf("default nodeSelector must pin kubernetes.io/os=linux, got %v", sel)
	}
	raw, _ := yaml.Marshal(podSpec["affinity"])
	if !strings.Contains(string(raw), "node-role.kubernetes.io/control-plane") {
		t.Error("nodeAffinity must exclude control-plane nodes")
	}

	// checksum annotation present.
	tpl, _ := ds["spec"].(map[string]any)["template"].(map[string]any)
	ann, _ := tpl["metadata"].(map[string]any)["annotations"].(map[string]any)
	if s, _ := ann["checksum/node-profiles"].(string); len(s) != 64 {
		t.Errorf("checksum/node-profiles annotation must be a sha256 hex, got %q", s)
	}

	// mode plumbed through to BOTH containers.
	rm := renderArgs(t,
		"-f", filepath.Join("tinycdi", "ci", "node-profiles-values.yaml"),
		"--set", "nodeProfiles.install.mode=remove")
	rpods := dsPodSpec(daemonSet(rm, "node-profiles"))
	for _, cl := range [][]any{toSlice(rpods["initContainers"]), toSlice(rpods["containers"])} {
		for _, ci := range cl {
			cc, _ := ci.(map[string]any)
			envMode := ""
			for _, e := range toSlice(cc["env"]) {
				em, _ := e.(map[string]any)
				if em["name"] == "NODE_PROFILES_MODE" {
					envMode, _ = em["value"].(string)
				}
			}
			if envMode != "remove" {
				t.Errorf("mode=remove must reach NODE_PROFILES_MODE in container %v, got %q", cc["name"], envMode)
			}
		}
	}
}

// TestNodeProfilesInstallerNamespaceGuards: SEC-06 — the installer
// namespace must never BE the release namespace (that would force PSS
// privileged on the platform namespace), and the image must always be
// pinned (no :latest fallback, SEC-37).
func TestNodeProfilesInstallerNamespaceGuards(t *testing.T) {
	out := renderErrArgs(t,
		"-f", filepath.Join("tinycdi", "ci", "node-profiles-values.yaml"),
		"--set", "nodeProfiles.install.namespace=tcdi-system")
	if !strings.Contains(out, "nodeProfiles.install.namespace") {
		t.Errorf("installer namespace == release namespace must fail, got: %s", out)
	}
	out = renderErrArgs(t,
		"-f", filepath.Join("tinycdi", "ci", "node-profiles-values.yaml"),
		"--set", "nodeProfiles.install.image.digest=null", "--set", "nodeProfiles.install.image.tag=null")
	if !strings.Contains(out, "image") {
		t.Errorf("unpinned installer image must fail the render, got: %s", out)
	}
}

// TestLeaderElectionRoleLeasesOnly: SEC-06 — controller-runtime leader
// election uses Leases (resourcelock.LeasesResourceLock default; the
// operator never sets a different lock), so the leader-election Role must
// NOT carry configmaps write: that grant is what let the operator SA patch
// the installer's scripts.
func TestLeaderElectionRoleLeasesOnly(t *testing.T) {
	for _, vf := range lintValues {
		docs := render(t, vf)
		for _, d := range selectDocs(docs, "Role") {
			name, _ := meta(d)
			if name != "tcdi-leader-election" {
				continue
			}
			for _, r := range toSlice(d["rules"]) {
				rm, _ := r.(map[string]any)
				for _, res := range toSlice(rm["resources"]) {
					if res == "configmaps" {
						t.Errorf("%s: leader-election Role still grants configmaps (leader election uses Leases only)", vf)
					}
				}
			}
		}
	}
}

// TestNodeProfilesChecksumRollsOnContent: the pod-template checksum
// annotation must change when any shipped profile file changes (the
// rolling update is what re-runs the idempotent installer on every node).
// Renders a mutated COPY of the chart in a temp dir — the repo chart is
// never touched.
func TestNodeProfilesChecksumRollsOnContent(t *testing.T) {
	src := filepath.Join(repoRoot(t), "deploy", "helm", "tinycdi")
	dst := filepath.Join(t.TempDir(), "tinycdi")
	if err := filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		to := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(to, 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(to, b, info.Mode())
	}); err != nil {
		t.Fatalf("copy chart: %v", err)
	}
	vf := filepath.Join(src, "ci", "node-profiles-values.yaml")
	checksum := func(chartPath string) string {
		out, err := exec.Command(helmBin(t), "template", "tcdi", chartPath,
			"--namespace", "tcdi-system", "-f", vf).CombinedOutput()
		if err != nil {
			t.Fatalf("helm template %s: %v\n%s", chartPath, err, out)
		}
		for _, d := range decodeDocs(t, out) {
			if d["kind"] != "DaemonSet" {
				continue
			}
			tpl, _ := d["spec"].(map[string]any)["template"].(map[string]any)
			ann, _ := tpl["metadata"].(map[string]any)["annotations"].(map[string]any)
			s, _ := ann["checksum/node-profiles"].(string)
			return s
		}
		t.Fatal("no DaemonSet in render")
		return ""
	}
	base := checksum(src)
	if base == "" {
		t.Fatal("empty checksum on unmodified chart")
	}
	mut := filepath.Join(dst, "files", "node-profiles", "apparmor", "tinycdi-browser")
	b, err := os.ReadFile(mut)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mut, append(b, "# mutation\n"...), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := checksum(dst); got == base {
		t.Error("checksum/node-profiles did not change after profile content changed — pods would not roll")
	}
}

// ---------------------------------------------------------------------------
// Security-hardening assertions (SEC-08/09/29/31-37, SEC-02 chart side,
// DB TLS).
// ---------------------------------------------------------------------------

// secretVolumes returns the pod-level `secret` volume definitions of a
// Deployment doc as name -> secret map.
func secretVolumes(d doc) map[string]map[string]any {
	out := map[string]map[string]any{}
	spec, _ := d["spec"].(map[string]any)
	tpl, _ := spec["template"].(map[string]any)
	podSpec, _ := tpl["spec"].(map[string]any)
	for _, v := range toSlice(podSpec["volumes"]) {
		vm, _ := v.(map[string]any)
		sec, _ := vm["secret"].(map[string]any)
		if sec != nil {
			name, _ := vm["name"].(string)
			out[name] = sec
		}
	}
	return out
}

// TestCAVolumesProjectOnlyCACrt: SEC-08 — a Secret that holds a CA can also
// hold tls.key (the cert-manager self-signed CA Secret does). Every CA
// volume must therefore project ONLY its configured public key via items:
// so the private key can never land in a pod — least privilege, defence in
// depth against file-read/RCE in the internet-facing gateway.
func TestCAVolumesProjectOnlyCACrt(t *testing.T) {
	for _, vf := range []string{"example-values.yaml", "ingress-certmanager-values.yaml"} {
		docs := render(t, vf)
		for _, depName := range []string{"backend", "operator", "frontend"} {
			dep := deployment(docs, depName)
			if dep == nil {
				t.Fatalf("%s: no %s Deployment", vf, depName)
			}
			for volName, sec := range secretVolumes(dep) {
				switch {
				case volName == "tls" || strings.HasPrefix(volName, "tls-") ||
					volName == "mtls" || volName == "control-token" ||
					volName == "login-keys":
					// leaf cert, token and login-key Secrets legitimately
					// carry their private material
					continue
				}
				items := toSlice(sec["items"])
				if len(items) != 1 {
					t.Errorf("%s: %s volume %q (secret %v) must project exactly one key via items:, got %v",
						vf, depName, volName, sec["secretName"], sec["items"])
					continue
				}
				im, _ := items[0].(map[string]any)
				key, _ := im["key"].(string)
				if key == "tls.key" || key == "" || im["path"] != key {
					t.Errorf("%s: %s volume %q must project only the public CA key, got %v", vf, depName, volName, im)
				}
			}
		}
	}
}

// TestFrontendServiceAccountLockedDown: SEC-34 — the frontend never calls
// the apiserver (static SPA server), so its SA token is not automounted
// (SA AND pod) and the pod must not carry the
// cdi.tinyorbit.vn/needs-apiserver egress label.
func TestFrontendServiceAccountLockedDown(t *testing.T) {
	for _, vf := range lintValues {
		docs := render(t, vf)
		for _, d := range selectDocs(docs, "ServiceAccount") {
			if n, _ := meta(d); n == "frontend" {
				if v, ok := d["automountServiceAccountToken"].(bool); !ok || v {
					t.Errorf("%s: frontend SA automountServiceAccountToken must be false, got %v", vf, d["automountServiceAccountToken"])
				}
			}
		}
		dep := deployment(docs, "frontend")
		if dep == nil {
			continue
		}
		spec, _ := dep["spec"].(map[string]any)
		tpl, _ := spec["template"].(map[string]any)
		podSpec, _ := tpl["spec"].(map[string]any)
		if v, ok := podSpec["automountServiceAccountToken"].(bool); !ok || v {
			t.Errorf("%s: frontend pod automountServiceAccountToken must be false, got %v", vf, podSpec["automountServiceAccountToken"])
		}
		lbls, _ := tpl["metadata"].(map[string]any)["labels"].(map[string]any)
		if _, ok := lbls["cdi.tinyorbit.vn/needs-apiserver"]; ok {
			t.Errorf("%s: frontend pod must not carry cdi.tinyorbit.vn/needs-apiserver — it never calls the apiserver", vf)
		}
	}
}

// TestBackendPodLabels: SEC-29 — the backend pod must keep
// workspaces.cdi.tinyorbit.vn/role=gateway (the operator's per-workspace
// NetworkPolicy selects it for runtime ingress) and
// cdi.tinyorbit.vn/needs-apiserver (the merged binary projects Workspace
// CRs and reads templates — the apiserver egress policy matches on it).
func TestBackendPodLabels(t *testing.T) {
	for _, vf := range lintValues {
		dep := deployment(render(t, vf), "backend")
		if dep == nil {
			continue
		}
		lbls, _ := dep["spec"].(map[string]any)["template"].(map[string]any)["metadata"].(map[string]any)["labels"].(map[string]any)
		if lbls["workspaces.cdi.tinyorbit.vn/role"] != "gateway" {
			t.Errorf("%s: backend pod must carry workspaces.cdi.tinyorbit.vn/role=gateway (SEC-29 runtime ingress selector), got %v", vf, lbls)
		}
		if lbls["cdi.tinyorbit.vn/needs-apiserver"] != "true" {
			t.Errorf("%s: backend pod must carry cdi.tinyorbit.vn/needs-apiserver=true (it projects CRs), got %v", vf, lbls)
		}
	}
}

// TestOperatorMetricsOff: SEC-33 — the operator exposes no metrics
// endpoint: controller-runtime's secure mode also enables the authn/authz
// filter, which needs cluster-scoped TokenReview/SAR RBAC this chart never
// grants; plain-HTTP operator metrics are banned. The bind address is
// pinned to 0.
func TestOperatorMetricsOff(t *testing.T) {
	for _, vf := range lintValues {
		dep := deployment(render(t, vf), "operator")
		if dep == nil {
			continue
		}
		args := strings.Join(firstContainerArgs(dep), "\n")
		if !strings.Contains(args, "--metrics-bind-address=0") {
			t.Errorf("%s: operator metrics must be disabled (--metrics-bind-address=0), args:\n%s", vf, args)
		}
		cm := firstContainer(dep)
		for _, p := range toSlice(cm["ports"]) {
			pm, _ := p.(map[string]any)
			if pm["name"] == "metrics" {
				t.Errorf("%s: operator pod must not expose a metrics port", vf)
			}
		}
	}
}

// TestIngressTLSMandatory: SEC-35 — ingress.enabled without a TLS Secret
// would publish plain-HTTP edges; the render must fail.
func TestIngressTLSMandatory(t *testing.T) {
	out := renderErrArgs(t,
		"-f", filepath.Join("tinycdi", "ci", "minimal-values.yaml"),
		"--set", "ingress.enabled=true")
	if !strings.Contains(out, "ingress.tls") {
		t.Errorf("ingress without TLS must fail mentioning ingress.tls, got: %s", out)
	}
	// with the Secret set the render must carry tls: blocks on BOTH edges.
	docs := renderArgs(t,
		"-f", filepath.Join("tinycdi", "ci", "minimal-values.yaml"),
		"--set", "ingress.enabled=true", "--set", "ingress.tls.existingSecret=tinycdi-ingress-tls")
	for _, d := range selectDocs(docs, "Ingress") {
		spec, _ := d["spec"].(map[string]any)
		if len(toSlice(spec["tls"])) == 0 {
			name, _ := meta(d)
			t.Errorf("Ingress %q must carry a tls: section", name)
		}
	}
}

// TestHTTPRouteSectionNameRequired: SEC-35 — an HTTPRoute parentRef
// without sectionName binds EVERY listener (incl. plain HTTP) on the
// Gateway; the render must refuse parentRefs lacking sectionName.
func TestHTTPRouteSectionNameRequired(t *testing.T) {
	out := renderErrArgs(t,
		"-f", filepath.Join("tinycdi", "ci", "minimal-values.yaml"),
		"--set", "gatewayApi.enabled=true",
		"--set", "gatewayApi.parentRefs[0].name=shared-gateway",
		"--set", "gatewayApi.parentRefs[0].namespace=gateway-system")
	if !strings.Contains(out, "sectionName") {
		t.Errorf("parentRef without sectionName must fail mentioning sectionName, got: %s", out)
	}
	// with sectionName the routes render it through.
	gw := render(t, "gateway-api-values.yaml")
	for _, r := range selectDocs(gw, "HTTPRoute") {
		spec, _ := r["spec"].(map[string]any)
		for _, pr := range toSlice(spec["parentRefs"]) {
			pm, _ := pr.(map[string]any)
			if pm["sectionName"] != "https" {
				name, _ := meta(r)
				t.Errorf("HTTPRoute %s parentRef must carry sectionName, got %v", name, pm)
			}
		}
	}
}

// TestSchemaHardening: SEC-36 — schema and render-time guards refuse
// plaintext URLs, comma-injected hosts and dev/privileged surfaces.
func TestSchemaHardening(t *testing.T) {
	// http:// issuer rejected by the schema.
	out := renderErrArgs(t,
		"-f", filepath.Join("tinycdi", "ci", "minimal-values.yaml"),
		"--set", "oidc.issuer=http://idp.lab.example.net/realms/tinycdi")
	if !strings.Contains(out, "issuer") && !strings.Contains(out, "https") {
		t.Errorf("http:// issuer must be rejected, got: %s", out)
	}
	// comma-injected portalHost (a second origin smuggled into the
	// TCDI_PORTAL_ORIGINS join) is rejected.
	out = renderErrArgs(t,
		"-f", filepath.Join("tinycdi", "ci", "minimal-values.yaml"),
		"--set", `portalHost=portal.lab.example.net\,evil.example.net`)
	if !strings.Contains(out, "portalHost") {
		t.Errorf("comma-injected portalHost must be rejected, got: %s", out)
	}
	// http:// extra portal origin rejected.
	out = renderErrArgs(t,
		"-f", filepath.Join("tinycdi", "ci", "minimal-values.yaml"),
		"--set", "backend.extraPortalOrigins[0]=http://alt.portal.example.net")
	if !strings.Contains(out, "extraPortalOrigins") && !strings.Contains(out, "https") {
		t.Errorf("http:// extraPortalOrigins entry must be rejected, got: %s", out)
	}
	// devAllowNoBroker is gated behind dev.enabled.
	out = renderErrArgs(t,
		"-f", filepath.Join("tinycdi", "ci", "minimal-values.yaml"),
		"--set", "operator.devAllowNoBroker=true")
	if !strings.Contains(out, "devAllowNoBroker") {
		t.Errorf("devAllowNoBroker without dev.enabled must fail, got: %s", out)
	}
	// privileged/securityContext overrides are gated behind dev.enabled.
	out = renderErrArgs(t,
		"-f", filepath.Join("tinycdi", "ci", "minimal-values.yaml"),
		"--set", "backend.securityContext.privileged=true")
	if !strings.Contains(out, "securityContext") {
		t.Errorf("privileged securityContext override without dev.enabled must fail, got: %s", out)
	}
	out = renderErrArgs(t,
		"-f", filepath.Join("tinycdi", "ci", "minimal-values.yaml"),
		"--set", "backend.podSecurityContext.runAsNonRoot=false")
	if !strings.Contains(out, "podSecurityContext") {
		t.Errorf("runAsNonRoot=false override without dev.enabled must fail, got: %s", out)
	}
	// dev.enabled is the explicit opt-out.
	renderArgs(t,
		"-f", filepath.Join("tinycdi", "ci", "minimal-values.yaml"),
		"--set", "dev.enabled=true", "--set", "operator.devAllowNoBroker=true")
}

// TestInternetOnlyRequiresClusterCIDRs: SEC-02 chart side — a seeded
// InternetOnly template with empty operator.clusterCIDRs would leave pod,
// service and node CIDRs reachable; the render must fail.
func TestInternetOnlyRequiresClusterCIDRs(t *testing.T) {
	docs := render(t, "security-values.yaml")
	tpls := selectDocs(docs, "WorkspaceTemplate")
	hasIO := false
	for _, tp := range tpls {
		spec, _ := tp["spec"].(map[string]any)
		if spec["networkProfile"] == "InternetOnly" {
			hasIO = true
		}
	}
	if !hasIO {
		t.Fatal("security-values.yaml must seed an InternetOnly template to exercise this")
	}
	dep := deployment(docs, "operator")
	args := strings.Join(firstContainerArgs(dep), "\n")
	if !strings.Contains(args, "--internet-except-cidrs=") ||
		!strings.Contains(args, "10.42.0.0/16") {
		t.Errorf("operator args must carry clusterCIDRs in --internet-except-cidrs, args:\n%s", args)
	}
	out := renderErrArgs(t,
		"-f", filepath.Join("tinycdi", "ci", "security-values.yaml"),
		"--set", "operator.clusterCIDRs=null")
	if !strings.Contains(out, "clusterCIDRs") {
		t.Errorf("InternetOnly template + empty clusterCIDRs must fail mentioning clusterCIDRs, got: %s", out)
	}
}

// TestDatabaseTLSWiring: database.tls.mode lands as PGSSLMODE and
// database.tls.caSecret mounts a read-only CA bundle exported as
// PGSSLROOTCERT (pgx honours both for URL DSNs that omit them — proven by
// TestPGConnHonoursSSEnv in pgx_env_test.go).
func TestDatabaseTLSWiring(t *testing.T) {
	docs := render(t, "security-values.yaml")
	dep := deployment(docs, "backend")
	env := firstContainerEnv(dep)
	if env["PGSSLMODE"] != "verify-full" {
		t.Errorf("PGSSLMODE = %q, want verify-full", env["PGSSLMODE"])
	}
	if env["PGSSLROOTCERT"] != "/etc/db-ca/ca.crt" {
		t.Errorf("PGSSLROOTCERT = %q, want /etc/db-ca/ca.crt", env["PGSSLROOTCERT"])
	}
	secs := secretVolumes(dep)
	ca, ok := secs["db-ca"]
	if !ok || ca["secretName"] != "tinycdi-db-ca" {
		t.Errorf("backend pod missing db-ca volume for tinycdi-db-ca, got %v", secs)
	}
	items := toSlice(ca["items"])
	if len(items) != 1 {
		t.Errorf("db-ca volume must project only the configured key, got %v", items)
	}
	// CHTR-8: the mode default is verify-full — always exported as
	// PGSSLMODE (the backend refuses non-verifying sslmodes at startup). No
	// db-ca volume without database.tls.caSecret.name (pgx verifies against
	// the system CA pool then).
	dep = deployment(render(t, "minimal-values.yaml"), "backend")
	env = firstContainerEnv(dep)
	if env["PGSSLMODE"] != "verify-full" {
		t.Errorf("default PGSSLMODE = %q, want verify-full", env["PGSSLMODE"])
	}
	if _, ok := secretVolumes(dep)["db-ca"]; ok {
		t.Error("db-ca volume must not render without database.tls.caSecret.name")
	}
}

// TestKustomizeRBACNoClusterRoleBinding: SEC-31 — the dev kustomize path
// (config/rbac, consumed by `make deploy`/`build-installer`) must not bind
// the manager ClusterRole cluster-wide either; the leader-election Role
// must be leases-only there too.
func TestKustomizeRBACNoClusterRoleBinding(t *testing.T) {
	rbacDir := filepath.Join(repoRoot(t), "config", "rbac")
	entries, err := os.ReadDir(rbacDir)
	if err != nil {
		t.Skipf("config/rbac not present: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(rbacDir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		dec := yaml.NewDecoder(bytes.NewReader(b))
		for {
			var d doc
			if err := dec.Decode(&d); err != nil {
				break
			}
			if d == nil || d["kind"] != "ClusterRoleBinding" {
				continue
			}
			rr, _ := d["roleRef"].(map[string]any)
			name, _ := rr["name"].(string)
			if strings.Contains(name, "manager") {
				t.Errorf("config/rbac/%s: ClusterRoleBinding binds %q — the manager role must be bound per-namespace", e.Name(), name)
			}
		}
		if e.Name() == "leader_election_role.yaml" {
			var role struct {
				Rules []struct {
					Resources []string `yaml:"resources"`
				} `yaml:"rules"`
			}
			if err := yaml.Unmarshal(b, &role); err != nil {
				t.Fatal(err)
			}
			for _, r := range role.Rules {
				for _, res := range r.Resources {
					if res == "configmaps" {
						t.Errorf("config/rbac/leader_election_role.yaml must not grant configmaps — leader election uses Leases")
					}
				}
			}
		}
	}
}

// TestNodeProfilesInstallerDockerProof is the permanent form of the
// offline proof: it runs install.sh/verify.sh end-to-end inside the pinned
// alpine image against a simulated host root (fake /proc, fake
// apparmor_parser, fake apparmorfs) via deploy/helm/proof/node-profiles/run.sh.
// Docker-only; skipped when docker is unavailable.
func TestNodeProfilesInstallerDockerProof(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("no docker — skipping the installer container proof")
	}
	root := repoRoot(t)
	filesDir := filepath.Join(root, "deploy", "helm", "tinycdi", "files", "node-profiles")
	proofDir := filepath.Join(root, "deploy", "helm", "proof", "node-profiles")

	// Stage the mounts the proof expects: installer scripts flat, and the
	// two profiles flat under profiles-src (matching the ConfigMap keys).
	stage := t.TempDir()
	for _, sub := range []string{"installer", "profiles-src"} {
		if err := os.MkdirAll(filepath.Join(stage, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	copies := [][2]string{
		{"installer/install.sh", "installer/install.sh"},
		{"installer/verify.sh", "installer/verify.sh"},
		{"seccomp/chromium-userns.json", "profiles-src/chromium-userns.json"},
		{"apparmor/tinycdi-browser", "profiles-src/tinycdi-browser"},
	}
	for _, cp := range copies {
		b, err := os.ReadFile(filepath.Join(filesDir, cp[0]))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(stage, cp[1]), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	img := "alpine@sha256:ce64758a109eb420d874a118f87920e625e12d3634e03b4a5573fd9f6e5d3507"
	out, err := exec.Command("docker", "run", "--rm", "--network", "none",
		"-v", filepath.Join(stage, "installer")+":/installer:ro",
		"-v", filepath.Join(stage, "profiles-src")+":/profiles-src:ro",
		"-v", proofDir+":/proof:ro",
		img, "sh", "/proof/run.sh").CombinedOutput()
	if err != nil {
		t.Fatalf("installer proof failed: %v\n%s", err, out)
	}
	t.Logf("installer proof output:\n%s", out)
}

// renderNotes renders templates/NOTES.txt via a client-side dry-run
// install (helm template does not emit NOTES) and returns the text after
// the "NOTES:" marker.
func renderNotes(t *testing.T, extra ...string) string {
	t.Helper()
	args := []string{"install", "tcdi", "./tinycdi",
		"--namespace", "tcdi-system", "--dry-run=client"}
	args = append(args, extra...)
	out, err := exec.Command(helmBin(t), args...).CombinedOutput()
	if err != nil {
		t.Fatalf("helm install --dry-run=client %v: %v\n%s", args, err, out)
	}
	s := string(out)
	i := strings.Index(s, "\nNOTES:")
	if i < 0 {
		t.Fatalf("no NOTES section in dry-run output\n%s", s)
	}
	return s[i:]
}

// T4.2/D25: by default the operator gets the dedicated-pool placement
// flags and the node-profile DaemonSet targets the same pool (label AND
// taint) so the profiles land where runtime pods can schedule.
func TestRuntimePlacementDefault(t *testing.T) {
	docs := renderArgs(t,
		"-f", filepath.Join("tinycdi", "ci", "minimal-values.yaml"),
		"--set", "runtime.placement.allowSharedNodes=false",
		"--set", "nodeProfiles.install.enabled=true")

	op := deployment(docs, "operator")
	if op == nil {
		t.Fatal("no operator Deployment rendered")
	}
	var selArg, tolArg string
	for _, a := range firstContainerArgs(op) {
		if strings.HasPrefix(a, "--runtime-node-selector=") {
			selArg = a
		}
		if strings.HasPrefix(a, "--runtime-tolerations=") {
			tolArg = a
		}
	}
	wantSel := `--runtime-node-selector={"cdi.tinyorbit.vn/workspace":"true"}`
	if selArg != wantSel {
		t.Errorf("operator selector arg = %q, want %q", selArg, wantSel)
	}
	for _, want := range []string{`"cdi.tinyorbit.vn/workspace"`, `"NoSchedule"`} {
		if !strings.Contains(tolArg, want) {
			t.Errorf("operator tolerations arg = %q, want it to contain %s", tolArg, want)
		}
	}

	ds := daemonSet(docs, "node-profiles")
	if ds == nil {
		t.Fatal("no node-profiles DaemonSet rendered")
	}
	podSpec := dsPodSpec(ds)
	ns, _ := podSpec["nodeSelector"].(map[string]any)
	if ns["cdi.tinyorbit.vn/workspace"] != "true" {
		t.Errorf("node-profiles nodeSelector = %v, want the pool label", ns)
	}
	foundTol := false
	for _, tv := range toSlice(podSpec["tolerations"]) {
		tm, _ := tv.(map[string]any)
		if tm["key"] == "cdi.tinyorbit.vn/workspace" && tm["effect"] == "NoSchedule" {
			foundTol = true
		}
	}
	if !foundTol {
		t.Errorf("node-profiles tolerations = %v, want the pool toleration", podSpec["tolerations"])
	}
}

// allowSharedNodes: true passes NO placement flags to the operator and
// the install NOTES carry the shared-node warning.
func TestAllowSharedNodes(t *testing.T) {
	docs := renderArgs(t,
		"-f", filepath.Join("tinycdi", "ci", "minimal-values.yaml"),
		"--set", "runtime.placement.allowSharedNodes=true",
		"--set", "nodeProfiles.install.enabled=true")

	op := deployment(docs, "operator")
	if op == nil {
		t.Fatal("no operator Deployment rendered")
	}
	for _, a := range firstContainerArgs(op) {
		// --runtime-host-users is NOT a placement flag — it renders
		// independently (default false) even on shared nodes.
		for _, p := range []string{
			"--runtime-node-selector=", "--runtime-tolerations=",
		} {
			if strings.HasPrefix(a, p) {
				t.Errorf("allowSharedNodes must pass no placement flags, got %q", a)
			}
		}
	}
	podSpec := dsPodSpec(daemonSet(docs, "node-profiles"))
	ns, _ := podSpec["nodeSelector"].(map[string]any)
	if _, ok := ns["cdi.tinyorbit.vn/workspace"]; ok {
		t.Errorf("shared nodes: node-profiles must not select the pool label, got %v", ns)
	}

	notes := renderNotes(t, "-f", filepath.Join("tinycdi", "ci", "minimal-values.yaml"),
		"--set", "runtime.placement.allowSharedNodes=true")
	if !strings.Contains(notes, "allowSharedNodes") {
		t.Errorf("NOTES must warn about shared-node scheduling, got:\n%s", notes)
	}
}

// A dedicated pool with an empty nodeSelector would leave every runtime
// pod Pending forever — the render must refuse it.
func TestDedicatedPoolRequiresSelector(t *testing.T) {
	errOut := renderErrArgs(t,
		"-f", filepath.Join("tinycdi", "ci", "minimal-values.yaml"),
		"--set", "runtime.placement.allowSharedNodes=false",
		"--set-json", `runtime.placement.nodeSelector=null`)
	if !strings.Contains(errOut, "runtime.placement.nodeSelector") {
		t.Fatalf("render error must name runtime.placement.nodeSelector, got:\n%s", errOut)
	}
}

// T4.2/D26: runtime.hostUsers defaults to false — the operator gets
// --runtime-host-users=false so runtime pods run in their own user
// namespace (verified on the reference environment, T4.3). Setting the
// value to null renders NO flag, leaving pod.spec.hostUsers unset.
func TestRuntimeHostUsersDefault(t *testing.T) {
	docs := renderArgs(t,
		"-f", filepath.Join("tinycdi", "ci", "minimal-values.yaml"))
	op := deployment(docs, "operator")
	if op == nil {
		t.Fatal("no operator Deployment rendered")
	}
	found := false
	for _, a := range firstContainerArgs(op) {
		if strings.HasPrefix(a, "--runtime-host-users=") {
			found = true
			if a != "--runtime-host-users=false" {
				t.Errorf("operator hostUsers arg = %q, want --runtime-host-users=false", a)
			}
		}
	}
	if !found {
		t.Error("operator must get --runtime-host-users=false by default")
	}

	docs = renderArgs(t,
		"-f", filepath.Join("tinycdi", "ci", "minimal-values.yaml"),
		"--set-json", `runtime.hostUsers=null`)
	op = deployment(docs, "operator")
	for _, a := range firstContainerArgs(op) {
		if strings.HasPrefix(a, "--runtime-host-users=") {
			t.Errorf("runtime.hostUsers=null must render no host-users flag, got %q", a)
		}
	}
}

// ---------------------------------------------------------------------------
// T1.7 — the 0.2.0 control plane: backend (API + session gateway + broker
// in one Deployment, D6/D7) and frontend (static SPA server) replace the
// api, gateway and portal components.
// ---------------------------------------------------------------------------

// TestControlPlaneIsThreeDeployments: D6 — the release namespace carries
// exactly three control-plane Deployments: backend, frontend, operator.
func TestControlPlaneIsThreeDeployments(t *testing.T) {
	for _, vf := range lintValues {
		docs := render(t, vf)
		got := map[string]bool{}
		for _, d := range selectDocs(docs, "Deployment") {
			name, ns := meta(d)
			if ns == "tcdi-system" {
				got[name] = true
			}
		}
		for _, want := range []string{"backend", "frontend", "operator"} {
			if !got[want] {
				t.Errorf("%s: missing control-plane Deployment %q", vf, want)
			}
			delete(got, want)
		}
		for name := range got {
			t.Errorf("%s: unexpected control-plane Deployment %q", vf, name)
		}
	}
}

// TestBackendDefaults: D22 — the backend Deployment ships restart-safe
// defaults: 2 replicas, rollingUpdate maxUnavailable 0 / maxSurge 1, a PDB
// with minAvailable 1 and preferred (P8 — never required, single-node
// clusters must still schedule) pod anti-affinity on
// kubernetes.io/hostname, plus a 30 s termination grace period for
// Gateway.Drain.
func TestBackendDefaults(t *testing.T) {
	docs := render(t, "minimal-values.yaml")
	dep := deployment(docs, "backend")
	if dep == nil {
		t.Fatal("no backend Deployment rendered")
	}
	spec, _ := dep["spec"].(map[string]any)
	if spec["replicas"] != 2 {
		t.Errorf("backend replicas = %v, want 2 (D22)", spec["replicas"])
	}
	strategy, _ := spec["strategy"].(map[string]any)
	ru, _ := strategy["rollingUpdate"].(map[string]any)
	if ru["maxUnavailable"] != 0 {
		t.Errorf("backend rollingUpdate.maxUnavailable = %v, want 0 (D22)", ru["maxUnavailable"])
	}
	if ru["maxSurge"] != 1 {
		t.Errorf("backend rollingUpdate.maxSurge = %v, want 1 (D22)", ru["maxSurge"])
	}
	tpl, _ := spec["template"].(map[string]any)
	podSpec, _ := tpl["spec"].(map[string]any)
	if podSpec["terminationGracePeriodSeconds"] != 30 {
		t.Errorf("backend terminationGracePeriodSeconds = %v, want 30 (Gateway.Drain budget)", podSpec["terminationGracePeriodSeconds"])
	}
	aff, _ := podSpec["affinity"].(map[string]any)
	paa, _ := aff["podAntiAffinity"].(map[string]any)
	preferred := toSlice(paa["preferredDuringSchedulingIgnoredDuringExecution"])
	if len(preferred) == 0 {
		t.Fatal("backend must carry preferred pod anti-affinity (D22/P8)")
	}
	pat, _ := preferred[0].(map[string]any)["podAffinityTerm"].(map[string]any)
	if pat["topologyKey"] != "kubernetes.io/hostname" {
		t.Errorf("backend anti-affinity topologyKey = %v, want kubernetes.io/hostname", pat["topologyKey"])
	}
	if len(toSlice(paa["requiredDuringSchedulingIgnoredDuringExecution"])) != 0 {
		t.Error("backend anti-affinity must be preferred, not required (P8 — required strands the second replica on single-node clusters)")
	}
	// the matching PDB.
	var pdb doc
	for _, d := range selectDocs(docs, "PodDisruptionBudget") {
		if n, _ := meta(d); n == "backend" {
			pdb = d
		}
	}
	if pdb == nil {
		t.Fatal("no backend PodDisruptionBudget rendered by default (D22)")
	}
	pspec, _ := pdb["spec"].(map[string]any)
	if pspec["minAvailable"] != 1 {
		t.Errorf("backend PDB minAvailable = %v, want 1", pspec["minAvailable"])
	}
}

// TestBackendGatewayIDIsStatic: every backend pod in the Deployment shares
// ONE gateway identity — -gateway-id is a literal from
// backend.gatewayID (default tinycdi-backend), never a downward-API pod
// name (a per-pod identity would split the lease directory across
// replicas).
func TestBackendGatewayIDIsStatic(t *testing.T) {
	dep := deployment(render(t, "minimal-values.yaml"), "backend")
	if dep == nil {
		t.Fatal("no backend Deployment rendered")
	}
	found := false
	for _, a := range firstContainerArgs(dep) {
		if !strings.HasPrefix(a, "-gateway-id=") {
			continue
		}
		found = true
		if a != "-gateway-id=tinycdi-backend" {
			t.Errorf("default -gateway-id = %q, want -gateway-id=tinycdi-backend", a)
		}
		if strings.Contains(a, "$(") {
			t.Errorf("-gateway-id must be a literal, not a downward-API reference: %q", a)
		}
	}
	if !found {
		t.Error("backend args missing -gateway-id")
	}
	// and no env indirection (e.g. GATEWAY_ID via fieldRef metadata.name).
	for _, e := range toSlice(firstContainer(dep)["env"]) {
		em, _ := e.(map[string]any)
		vf, _ := em["valueFrom"].(map[string]any)
		if fr, ok := vf["fieldRef"].(map[string]any); ok && fr["fieldPath"] == "metadata.name" {
			t.Errorf("backend pod must not feed metadata.name into env %v — the gateway identity is shared, not per-pod", em["name"])
		}
	}
	// the value is configurable.
	dep = deployment(renderArgs(t,
		"-f", filepath.Join("tinycdi", "ci", "minimal-values.yaml"),
		"--set", "backend.gatewayID=fleet-a"), "backend")
	found = false
	for _, a := range firstContainerArgs(dep) {
		if a == "-gateway-id=fleet-a" {
			found = true
		}
	}
	if !found {
		t.Error("backend.gatewayID=fleet-a did not reach -gateway-id")
	}
}

// TestLoginKeysRequired: the app listener seals OIDC login state with an
// AEAD key (D20) — the render must fail when neither
// backend.loginKeys.existingSecret nor .generate is set, naming
// backend.loginKeys. generate=true renders the chart-managed Secret
// instead (lookup-preserved across upgrades).
func TestLoginKeysRequired(t *testing.T) {
	out := renderErrArgs(t,
		"-f", filepath.Join("tinycdi", "ci", "minimal-values.yaml"),
		"--set", "backend.loginKeys.existingSecret=")
	if !strings.Contains(out, "backend.loginKeys") {
		t.Errorf("missing login keys must fail mentioning backend.loginKeys, got: %s", out)
	}
	// existingSecret mode: the secret mounts at /login-keys and the flag
	// points at the current key.
	docs := render(t, "minimal-values.yaml")
	dep := deployment(docs, "backend")
	args := strings.Join(firstContainerArgs(dep), "\n")
	if !strings.Contains(args, "-login-key-file=/login-keys/current") {
		t.Errorf("backend args missing -login-key-file=/login-keys/current\nargs:\n%s", args)
	}
	if !secretNames(dep)["tinycdi-backend-login-keys"] {
		t.Errorf("backend pod missing the login-keys volume for tinycdi-backend-login-keys (secrets %v)", secretNames(dep))
	}
	// generate mode: the chart produces the Secret once via lookup.
	docs = renderArgs(t,
		"-f", filepath.Join("tinycdi", "ci", "minimal-values.yaml"),
		"--set", "backend.loginKeys.existingSecret=",
		"--set", "backend.loginKeys.generate=true")
	genFound := false
	for _, d := range selectDocs(docs, "Secret") {
		name, _ := meta(d)
		if !strings.HasSuffix(name, "-backend-login-keys") {
			continue
		}
		genFound = true
		data, _ := d["data"].(map[string]any)
		if cur, _ := data["current"].(string); len(cur) == 0 {
			t.Errorf("generated login-keys Secret must carry a 'current' key, got %v", data)
		}
	}
	if !genFound {
		t.Error("backend.loginKeys.generate=true must render a *-backend-login-keys Secret")
	}
}

// TestRemovedValuesFail: chart 0.2.0 dropped the api, gateway and portal
// components — setting any of their values must fail the render with a
// migration hint pointing at backend. / frontend. (D6; the schema keeps
// the old keys valid so the hint can fire instead of a bare schema error.)
func TestRemovedValuesFail(t *testing.T) {
	for _, tc := range []struct{ set, want string }{
		{"api.replicas=1", "backend."},
		{"gateway.replicas=1", "backend."},
		{"portal.replicas=1", "frontend."},
		{"images.api.repository=x/y", "backend."},
		{"images.gateway.repository=x/y", "backend."},
		{"images.portal.repository=x/y", "frontend."},
	} {
		out := renderErrArgs(t,
			"-f", filepath.Join("tinycdi", "ci", "minimal-values.yaml"),
			"--set", tc.set)
		if !strings.Contains(out, tc.want) {
			t.Errorf("--set %s must fail with a migration hint containing %q, got: %s", tc.set, tc.want, out)
		}
	}
}

// TestRoutes: both exposure modes carry the same three edge routes —
// portal host /v1 -> backend:8443 (app listener), portal host / ->
// frontend:8443, session host -> backend:8444 (session listener).
func TestRoutes(t *testing.T) {
	ruleBackends := func(d doc) []struct {
		path, svc string
		port      any
	} {
		var out []struct {
			path, svc string
			port      any
		}
		spec, _ := d["spec"].(map[string]any)
		for _, r := range toSlice(spec["rules"]) {
			rm, _ := r.(map[string]any)
			// HTTPRoute shape.
			for _, br := range toSlice(rm["backendRefs"]) {
				bm, _ := br.(map[string]any)
				path := ""
				for _, m := range toSlice(rm["matches"]) {
					mm, _ := m.(map[string]any)
					p, _ := mm["path"].(map[string]any)
					path, _ = p["value"].(string)
				}
				svc, _ := bm["name"].(string)
				out = append(out, struct {
					path, svc string
					port      any
				}{path, svc, bm["port"]})
			}
		}
		// Ingress shape.
		for _, r := range toSlice(spec["rules"]) {
			rm, _ := r.(map[string]any)
			http, _ := rm["http"].(map[string]any)
			for _, p := range toSlice(http["paths"]) {
				pm, _ := p.(map[string]any)
				backend, _ := pm["backend"].(map[string]any)
				svc, _ := backend["service"].(map[string]any)
				name, _ := svc["name"].(string)
				port := svc["port"].(map[string]any)
				pv := port["number"]
				if pv == nil {
					pv = port["name"]
				}
				path, _ := pm["path"].(string)
				out = append(out, struct {
					path, svc string
					port      any
				}{path, name, pv})
			}
		}
		return out
	}
	byName := func(docs []doc, kind string) map[string]doc {
		out := map[string]doc{}
		for _, d := range selectDocs(docs, kind) {
			n, _ := meta(d)
			out[n] = d
		}
		return out
	}
	for _, tc := range []struct {
		label, vf, kind string
	}{
		{"HTTPRoute", "gateway-api-values.yaml", "HTTPRoute"},
		{"Ingress", "ingress-certmanager-values.yaml", "Ingress"},
	} {
		docs := render(t, tc.vf)
		routes := byName(docs, tc.kind)
		portal, ok := routes["portal"]
		if !ok {
			t.Fatalf("%s: no portal route rendered", tc.kind)
		}
		session, ok := routes["session"]
		if !ok {
			t.Fatalf("%s: no session route rendered", tc.kind)
		}
		var v1OK, rootOK, sessOK bool
		for _, r := range ruleBackends(portal) {
			if r.path == "/v1" && r.svc == "backend" && fmt.Sprintf("%v", r.port) == "8443" || r.path == "/v1" && r.svc == "backend" && r.port == "app" {
				v1OK = true
			}
			if r.path == "/" && r.svc == "frontend" && (fmt.Sprintf("%v", r.port) == "8443" || r.port == "https") {
				rootOK = true
			}
		}
		for _, r := range ruleBackends(session) {
			if r.svc == "backend" && (fmt.Sprintf("%v", r.port) == "8444" || r.port == "session") {
				sessOK = true
			}
		}
		if !v1OK {
			t.Errorf("%s portal route: /v1 must go to backend:8443 (app), got %+v", tc.kind, ruleBackends(portal))
		}
		if !rootOK {
			t.Errorf("%s portal route: / must go to frontend:8443, got %+v", tc.kind, ruleBackends(portal))
		}
		if !sessOK {
			t.Errorf("%s session route: must go to backend:8444 (session), got %+v", tc.kind, ruleBackends(session))
		}
	}
}

// TestNetworkPolicyBackendPorts: the edge may reach backend :8443 (app)
// and :8444 (session) only; the internal mTLS listener :9443 is reachable
// from operator pods alone; metrics (when enabled) are scraped on the
// backend-metrics port through prometheusPeers.
func TestNetworkPolicyBackendPorts(t *testing.T) {
	docs := render(t, "example-values.yaml")
	var np doc
	for _, d := range selectDocs(docs, "NetworkPolicy") {
		if n, ns := meta(d); n == "backend" && ns == "tcdi-system" {
			np = d
		}
	}
	if np == nil {
		t.Fatal("no backend NetworkPolicy rendered")
	}
	spec, _ := np["spec"].(map[string]any)
	ingress := toSlice(spec["ingress"])

	// Collect: which (peerKind, ports) rules exist.
	edgePorts := map[int]bool{}
	operatorPorts := map[int]bool{}
	for _, r := range ingress {
		rm, _ := r.(map[string]any)
		var ports []int
		for _, p := range toSlice(rm["ports"]) {
			pm, _ := p.(map[string]any)
			if n, ok := pm["port"].(int); ok {
				ports = append(ports, n)
			}
			if f, ok := pm["port"].(float64); ok {
				ports = append(ports, int(f))
			}
		}
		from := toSlice(rm["from"])
		if len(from) == 0 {
			// edgeIngress=any: no peers — every source, these ports only.
			for _, p := range ports {
				edgePorts[p] = true
			}
			continue
		}
		for _, peer := range from {
			pm, _ := peer.(map[string]any)
			if _, ok := pm["ipBlock"]; ok {
				for _, p := range ports {
					edgePorts[p] = true
				}
			}
			sel, _ := pm["podSelector"].(map[string]any)
			lbls, _ := sel["matchLabels"].(map[string]any)
			if lbls["app.kubernetes.io/name"] == "operator" {
				for _, p := range ports {
					operatorPorts[p] = true
				}
			}
		}
	}
	if !edgePorts[8443] || !edgePorts[8444] {
		t.Errorf("edge must reach backend :8443 and :8444, edge-visible ports were %v", edgePorts)
	}
	if edgePorts[9443] {
		t.Error("internal listener :9443 must NOT be reachable from the edge")
	}
	if !operatorPorts[9443] || len(operatorPorts) != 1 {
		t.Errorf("only the operator may reach :9443, operator-reachable ports were %v", operatorPorts)
	}

	// metrics: example-values enables backend.metrics — the scrape policy
	// must target the metrics port on backend pods only.
	var mp doc
	for _, d := range selectDocs(docs, "NetworkPolicy") {
		if n, _ := meta(d); n == "allow-metrics-scrape" {
			mp = d
		}
	}
	if mp == nil {
		t.Fatal("no allow-metrics-scrape NetworkPolicy with backend.metrics.enabled")
	}
	mspec, _ := mp["spec"].(map[string]any)
	raw, _ := yaml.Marshal(mspec)
	if !strings.Contains(string(raw), "9090") {
		t.Errorf("metrics scrape policy must open the backend.metrics port 9090, got:\n%s", raw)
	}
}

// TestNetworkPolicyEdgeIngressModes: networkPolicy.edgeIngress selects how
// edge traffic reaches the public listeners — "any" (no peers, the public
// default), "ipBlock" (requires edgeIngressCIDRs) and "cilium"
// (CiliumNetworkPolicy entities; Cilium's ingress envoy runs host-side).
func TestNetworkPolicyEdgeIngressModes(t *testing.T) {
	// ipBlock without CIDRs fails closed (an empty peer list admits
	// everything).
	out := renderErrArgs(t,
		"-f", filepath.Join("tinycdi", "ci", "minimal-values.yaml"),
		"--set", "networkPolicy.edgeIngress=ipBlock",
		"--set", "networkPolicy.edgeIngressCIDRs=null")
	if !strings.Contains(out, "edgeIngressCIDRs") {
		t.Errorf("edgeIngress=ipBlock with no CIDRs must fail mentioning edgeIngressCIDRs, got: %s", out)
	}
	// an unknown mode fails.
	out = renderErrArgs(t,
		"-f", filepath.Join("tinycdi", "ci", "minimal-values.yaml"),
		"--set", "networkPolicy.edgeIngress=bogus")
	if !strings.Contains(out, "edgeIngress") {
		t.Errorf("unknown edgeIngress mode must fail, got: %s", out)
	}
	// ipBlock renders the CIDR peers on both public listeners.
	docs := renderArgs(t,
		"-f", filepath.Join("tinycdi", "ci", "minimal-values.yaml"),
		"--set", "networkPolicy.edgeIngress=ipBlock",
		"--set", "networkPolicy.edgeIngressCIDRs[0]=10.0.0.0/8")
	raw, _ := yaml.Marshal(selectDocs(docs, "NetworkPolicy"))
	if !strings.Contains(string(raw), "10.0.0.0/8") {
		t.Errorf("edgeIngress=ipBlock must render edgeIngressCIDRs peers, got:\n%s", raw)
	}
	// cilium renders the CiliumNetworkPolicy pair instead of CIDR peers.
	docs = renderArgs(t,
		"-f", filepath.Join("tinycdi", "ci", "minimal-values.yaml"),
		"--set", "networkPolicy.edgeIngress=cilium")
	cnp := selectDocs(docs, "CiliumNetworkPolicy")
	if len(cnp) != 2 {
		t.Errorf("edgeIngress=cilium must render 2 CiliumNetworkPolicies (backend+frontend), got %d", len(cnp))
	}
}
