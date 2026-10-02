// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package chart_test

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// Edge ingress policy (GitHub issue #13). v0.2 already ships
// networkPolicy.edgeIngress=cilium, which renders CiliumNetworkPolicies that
// admit the ingress/host/remote-node entities — the identities a host-network
// Gateway API / Ingress envoy carries, and which an ipBlock peer never
// matches under Cilium's default policy-cidr-match-mode. These tests pin that
// contract; there is deliberately no second switch for it.

// minimalValues is the lightest ci values file; edgeIngress defaults to
// ipBlock there.
var minimalValues = filepath.Join("tinycdi", "ci", "minimal-values.yaml")

// renderNetworkPolicySource returns the raw, byte-exact output of the
// networkpolicy.yaml template: every "# Source: tinycdi/templates/
// networkpolicy.yaml" document, concatenated in render order.
func renderNetworkPolicySource(t *testing.T, extra ...string) string {
	t.Helper()
	out, err := helmTemplate(t, append([]string{"-f", minimalValues}, extra...)...)
	if err != nil {
		t.Fatalf("helm template %v: %v\n%s", extra, err, out)
	}
	const source = "# Source: tinycdi/templates/networkpolicy.yaml"
	var b strings.Builder
	keep := false
	for _, line := range strings.SplitAfter(string(out), "\n") {
		switch {
		case strings.TrimRight(line, "\n") == source:
			keep = true
		case strings.HasPrefix(line, "# Source:"):
			keep = false
		case keep:
			b.WriteString(line)
		}
	}
	return b.String()
}

// TestEdgePolicyDefaultUnchanged: the default (edgeIngress=ipBlock) render of
// the network policies is byte-equal to what v0.2 shipped, so adopting a
// newer chart never silently changes who may reach the public listeners.
func TestEdgePolicyDefaultUnchanged(t *testing.T) {
	want, err := os.ReadFile(filepath.Join("testdata", "edge-policy-default.golden.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	got := renderNetworkPolicySource(t)
	if got != string(want) {
		t.Errorf("default network policy render drifted from the v0.2 golden "+
			"(deploy/helm/testdata/edge-policy-default.golden.yaml).\n--- got ---\n%s", got)
	}
	if strings.Contains(got, "CiliumNetworkPolicy") {
		t.Error("default render must not contain CiliumNetworkPolicy objects")
	}
	// Setting the default explicitly changes nothing either.
	if explicit := renderNetworkPolicySource(t, "--set", "networkPolicy.edgeIngress=ipBlock"); explicit != got {
		t.Error("networkPolicy.edgeIngress=ipBlock must render identically to the default")
	}
}

// ciliumPorts flattens an ingress rule's toPorts into "<port>/<protocol>".
func ciliumPorts(rule map[string]any) []string {
	var out []string
	for _, tp := range toSlice(rule["toPorts"]) {
		tpm, _ := tp.(map[string]any)
		for _, p := range toSlice(tpm["ports"]) {
			pm, _ := p.(map[string]any)
			port, _ := pm["port"].(string)
			proto, _ := pm["protocol"].(string)
			out = append(out, port+"/"+proto)
		}
	}
	sort.Strings(out)
	return out
}

func stringSlice(v any) []string {
	var out []string
	for _, e := range toSlice(v) {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func appName(d doc, selectorKey string) string {
	spec, _ := d["spec"].(map[string]any)
	sel, _ := spec[selectorKey].(map[string]any)
	lbls, _ := sel["matchLabels"].(map[string]any)
	n, _ := lbls["app.kubernetes.io/name"].(string)
	return n
}

func namedNetworkPolicy(t *testing.T, docs []doc, name string) doc {
	t.Helper()
	for _, d := range selectDocs(docs, "NetworkPolicy") {
		if n, ns := meta(d); n == name && ns == "tcdi-system" {
			return d
		}
	}
	t.Fatalf("no NetworkPolicy %q rendered", name)
	return nil
}

// TestEdgePolicyCiliumHostNetwork: edgeIngress=cilium is the host-network
// gateway fix. It renders exactly one CiliumNetworkPolicy for the backend
// (8443 app + 8444 session only) and one for the frontend (its TLS port),
// admitting the host and remote-node entities (plus ingress), and leaves no
// plain NetworkPolicy edge peers behind — a stale ipBlock/any rule would be a
// second, ineffective door that hides the real one.
func TestEdgePolicyCiliumHostNetwork(t *testing.T) {
	docs := renderArgs(t, "-f", minimalValues, "--set", "networkPolicy.edgeIngress=cilium")

	cnps := selectDocs(docs, "CiliumNetworkPolicy")
	if len(cnps) != 2 {
		t.Fatalf("want exactly 2 CiliumNetworkPolicies (backend + frontend), got %d", len(cnps))
	}
	wantPorts := map[string][]string{
		"backend":  {"8443/TCP", "8444/TCP"},
		"frontend": {"8443/TCP"},
	}
	seen := map[string]bool{}
	for _, c := range cnps {
		name, ns := meta(c)
		if ns != "tcdi-system" {
			t.Errorf("CiliumNetworkPolicy %s in namespace %q, want the release namespace", name, ns)
		}
		app := appName(c, "endpointSelector")
		want, ok := wantPorts[app]
		if !ok {
			t.Errorf("CiliumNetworkPolicy %s selects app %q, want backend or frontend", name, app)
			continue
		}
		seen[app] = true
		spec, _ := c["spec"].(map[string]any)
		ingress := toSlice(spec["ingress"])
		if len(ingress) != 1 {
			t.Errorf("%s: want one ingress rule, got %d", name, len(ingress))
			continue
		}
		rule, _ := ingress[0].(map[string]any)
		entities := stringSlice(rule["fromEntities"])
		for _, must := range []string{"host", "remote-node"} {
			found := false
			for _, e := range entities {
				found = found || e == must
			}
			if !found {
				t.Errorf("%s: fromEntities %v must include %q", name, entities, must)
			}
		}
		for _, e := range entities {
			if e == "world" || e == "all" {
				t.Errorf("%s: fromEntities %v must not include %q", name, entities, e)
			}
		}
		if got := ciliumPorts(rule); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s: ports = %v, want %v", name, got, want)
		}
		if _, has := spec["egress"]; has {
			t.Errorf("%s: an edge policy must not carry egress rules", name)
		}
	}
	for app := range wantPorts {
		if !seen[app] {
			t.Errorf("no CiliumNetworkPolicy for %s", app)
		}
	}

	// No plain-NP edge peers: the backend keeps only the in-cluster control
	// surface (release namespace :8444) and the operator's :9443; the
	// frontend admits nothing through the plain NetworkPolicy.
	backend, _ := namedNetworkPolicy(t, docs, "backend")["spec"].(map[string]any)
	for _, r := range toSlice(backend["ingress"]) {
		rm, _ := r.(map[string]any)
		from := toSlice(rm["from"])
		if len(from) == 0 {
			t.Errorf("backend NetworkPolicy has a peerless (every-source) ingress rule: %v", rm)
		}
		for _, peer := range from {
			pm, _ := peer.(map[string]any)
			if _, ip := pm["ipBlock"]; ip {
				t.Errorf("backend NetworkPolicy still carries an ipBlock edge peer: %v", pm)
			}
		}
	}
	frontend, _ := namedNetworkPolicy(t, docs, "frontend")["spec"].(map[string]any)
	if n := len(toSlice(frontend["ingress"])); n != 0 {
		t.Errorf("frontend NetworkPolicy must have no ingress rules in cilium mode, got %d", n)
	}
}

// TestEdgePolicyNeverOpensInternalPort: whatever the edge mode, nothing
// admitted from the edge reaches the internal mTLS listener (9443) or the
// metrics listener (9090); the edge only ever sees the TLS app/session ports.
func TestEdgePolicyNeverOpensInternalPort(t *testing.T) {
	forbidden := map[string]bool{"9443": true, "9090": true}
	cases := []struct {
		name   string
		values string
		extra  []string
	}{
		{"ipBlock default", "minimal-values.yaml", nil},
		{"ipBlock narrowed", "minimal-values.yaml", []string{
			"--set", "networkPolicy.edgeIngress=ipBlock",
			"--set", "networkPolicy.edgeIngressCIDRs[0]=10.0.0.0/8"}},
		{"any", "minimal-values.yaml", []string{"--set", "networkPolicy.edgeIngress=any"}},
		{"cilium", "minimal-values.yaml", []string{"--set", "networkPolicy.edgeIngress=cilium"}},
		// example-values turns the metrics listener on — the case most
		// likely to leak 9090 to the edge.
		{"ipBlock with metrics", "example-values.yaml", nil},
		{"any with metrics", "example-values.yaml", []string{"--set", "networkPolicy.edgeIngress=any"}},
		{"cilium with metrics", "example-values.yaml", []string{"--set", "networkPolicy.edgeIngress=cilium"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"-f", filepath.Join("tinycdi", "ci", tc.values)}, tc.extra...)
			docs := renderArgs(t, args...)

			// Plain NetworkPolicies: an edge rule is one whose peers are
			// ipBlocks or absent (every source). Pod/namespace-scoped peers
			// are in-cluster (operator, release namespace, Prometheus).
			for _, np := range selectDocs(docs, "NetworkPolicy") {
				name, _ := meta(np)
				spec, _ := np["spec"].(map[string]any)
				for _, r := range toSlice(spec["ingress"]) {
					rm, _ := r.(map[string]any)
					edge := len(toSlice(rm["from"])) == 0
					for _, peer := range toSlice(rm["from"]) {
						pm, _ := peer.(map[string]any)
						if _, ip := pm["ipBlock"]; ip {
							edge = true
						}
					}
					if !edge {
						continue
					}
					ports := toSlice(rm["ports"])
					if len(ports) == 0 {
						t.Errorf("NetworkPolicy %s: edge rule without ports opens every port: %v", name, rm)
					}
					for _, p := range ports {
						pm, _ := p.(map[string]any)
						if port := portString(pm["port"]); forbidden[port] {
							t.Errorf("NetworkPolicy %s admits the edge on internal port %s", name, port)
						}
					}
				}
			}

			// CiliumNetworkPolicies are edge policies by construction.
			for _, c := range selectDocs(docs, "CiliumNetworkPolicy") {
				name, _ := meta(c)
				spec, _ := c["spec"].(map[string]any)
				for _, r := range toSlice(spec["ingress"]) {
					rm, _ := r.(map[string]any)
					ports := ciliumPorts(rm)
					if len(ports) == 0 {
						t.Errorf("CiliumNetworkPolicy %s: rule without toPorts opens every port", name)
					}
					for _, p := range ports {
						if forbidden[strings.SplitN(p, "/", 2)[0]] {
							t.Errorf("CiliumNetworkPolicy %s admits the edge on internal port %s", name, p)
						}
					}
				}
			}
		})
	}
}

func portString(v any) string {
	switch p := v.(type) {
	case int:
		return strconv.Itoa(p)
	case int64:
		return strconv.FormatInt(p, 10)
	case float64:
		return strconv.Itoa(int(p))
	case string:
		return p
	}
	return ""
}

// TestEdgeIngressSchemaKeys: the schema pins the edge keys — edgeIngress is
// an enum that rejects unknown modes at the schema layer (not only in the
// template's fail), edgeIngressCIDRs is an array, and the networkPolicy block
// stays closed so a typo or a competing switch (for example
// networkPolicy.edge.fromHostNetwork) is an error rather than a silent no-op.
func TestEdgeIngressSchemaKeys(t *testing.T) {
	out := renderErrArgs(t, "-f", minimalValues, "--set", "networkPolicy.edgeIngress=bogus")
	if !strings.Contains(out, "edgeIngress") || !strings.Contains(out, "ipBlock") {
		t.Errorf("unknown edgeIngress must be rejected by the schema enum naming the key and listing the modes, got: %s", out)
	}
	out = renderErrArgs(t, "-f", minimalValues, "--set", "networkPolicy.edgeIngress=true")
	if !strings.Contains(out, "edgeIngress") {
		t.Errorf("edgeIngress=true (boolean-looking) must be rejected, got: %s", out)
	}
	out = renderErrArgs(t, "-f", minimalValues, "--set", "networkPolicy.edgeIngressCIDRs=10.0.0.0/8")
	if !strings.Contains(out, "edgeIngressCIDRs") {
		t.Errorf("edgeIngressCIDRs must be an array, got: %s", out)
	}
	out = renderErrArgs(t, "-f", minimalValues, "--set", "networkPolicy.edge.fromHostNetwork=true")
	if !strings.Contains(out, "edge") {
		t.Errorf("unknown networkPolicy keys must be rejected (additionalProperties=false), got: %s", out)
	}
	for _, mode := range []string{"ipBlock", "any", "cilium"} {
		renderArgs(t, "-f", minimalValues, "--set", "networkPolicy.edgeIngress="+mode)
	}
}
