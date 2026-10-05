// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package chart_test

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Deploy-time guards (v0.5): two values-driven footguns the render must
// refuse or pin — operator replicas>1 without leader election (two active
// reconcilers double-driving the same Workspaces), and the unauthenticated
// scrape-only metrics listener leaking onto the edge path.

// TestOperatorLeaderElectionGuard: operator.leaderElect=false is legal only
// for single-replica installs. With replicas>1 the render fails — the binary
// cannot observe its Deployment's replica count, so the chart is the only
// place the two values meet (tinycdi.validate).
func TestOperatorLeaderElectionGuard(t *testing.T) {
	// The default (replicas=2, leaderElect=true) renders with election on.
	docs := render(t, "minimal-values.yaml")
	dep := deployment(docs, "operator")
	spec, _ := dep["spec"].(map[string]any)
	if spec["replicas"] != 2 {
		t.Fatalf("operator replicas = %v, want the 2-replica default", spec["replicas"])
	}
	elected := false
	for _, a := range firstContainerArgs(dep) {
		if a == "--leader-elect=true" {
			elected = true
		}
	}
	if !elected {
		t.Errorf("default render lacks --leader-elect=true: %v", firstContainerArgs(dep))
	}

	// Election off with two or more replicas fails the render, naming both
	// offending values.
	for _, replicas := range []string{"2", "3"} {
		out := renderErrArgs(t,
			"-f", minimalValues,
			"--set", "operator.replicas="+replicas,
			"--set", "operator.leaderElect=false")
		if !strings.Contains(out, "operator.leaderElect") || !strings.Contains(out, "replicas") {
			t.Errorf("replicas=%s + leaderElect=false must fail naming both values, got: %s", replicas, out)
		}
	}

	// replicas=1 without election stays legal: a single replica needs no
	// lease and the flag renders through.
	docs = renderArgs(t,
		"-f", minimalValues,
		"--set", "operator.replicas=1",
		"--set", "operator.leaderElect=false")
	dep = deployment(docs, "operator")
	off := false
	for _, a := range firstContainerArgs(dep) {
		if a == "--leader-elect=false" {
			off = true
		}
	}
	if !off {
		t.Errorf("replicas=1 + leaderElect=false must render --leader-elect=false: %v", firstContainerArgs(dep))
	}
}

// metricsPolicy returns the allow-metrics-scrape NetworkPolicy or nil.
func metricsPolicy(docs []doc) doc {
	for _, d := range selectDocs(docs, "NetworkPolicy") {
		if n, _ := meta(d); n == "allow-metrics-scrape" {
			return d
		}
	}
	return nil
}

// serviceNamed returns the Service with the given name or nil.
func serviceNamed(docs []doc, name string) doc {
	for _, d := range selectDocs(docs, "Service") {
		if n, _ := meta(d); n == name {
			return d
		}
	}
	return nil
}

// rulePorts flattens a NetworkPolicy ingress/egress rule's ports into
// "<port>/<protocol>".
func rulePorts(rule map[string]any) []string {
	var out []string
	for _, p := range toSlice(rule["ports"]) {
		pm, _ := p.(map[string]any)
		proto, _ := pm["protocol"].(string)
		out = append(out, portString(pm["port"])+"/"+proto)
	}
	return out
}

// isEdgeRule reports whether a NetworkPolicy rule admits edge traffic: an
// absent/empty `from` (every source) or any ipBlock peer — the shapes the
// networkPolicy.edgeIngress modes produce. Pod/namespace-scoped peers are
// in-cluster and not edge by construction.
func isEdgeRule(rule map[string]any) bool {
	from := toSlice(rule["from"])
	if len(from) == 0 {
		return true
	}
	for _, peer := range from {
		if pm, _ := peer.(map[string]any); pm["ipBlock"] != nil {
			return true
		}
	}
	return false
}

// TestMetricsListenerIsolation: the backend metrics listener is plain HTTP
// with no auth (SEC-33), so nothing but the declared scrape source may reach
// it. Whatever the edge mode (ipBlock, any, cilium — the modes that admit
// gateway/edge traffic): the allow-metrics-scrape policy is the ONLY rule
// opening the metrics port, it is scoped to backend pods, its `from` is
// exactly networkPolicy.prometheusPeers, and the backend-metrics Service
// stays ClusterIP with no Ingress/HTTPRoute targeting it.
func TestMetricsListenerIsolation(t *testing.T) {
	// example-values enables metrics with prometheusPeers =
	// namespaceSelector{kubernetes.io/metadata.name: monitoring}; a
	// distinctive podSelector is added so the rendered `from` is checked
	// peer-for-peer against the values, not a hardcoded echo.
	peersJSON := `[{"namespaceSelector":{"matchLabels":{"kubernetes.io/metadata.name":"monitoring"}},"podSelector":{"matchLabels":{"app":"prometheus"}}}]`
	wantPeer := map[string]any{
		"namespaceSelector": map[string]any{
			"matchLabels": map[string]any{"kubernetes.io/metadata.name": "monitoring"},
		},
		"podSelector": map[string]any{
			"matchLabels": map[string]any{"app": "prometheus"},
		},
	}

	for _, mode := range []string{"ipBlock", "any", "cilium"} {
		t.Run(mode, func(t *testing.T) {
			docs := renderArgs(t,
				"-f", filepath.Join("tinycdi", "ci", "example-values.yaml"),
				"--set", "networkPolicy.edgeIngress="+mode,
				"--set-json", "networkPolicy.prometheusPeers="+peersJSON)

			// The scrape policy: backend pods only, one ingress rule,
			// metrics port only, from = exactly the configured peers.
			pol := metricsPolicy(docs)
			if pol == nil {
				t.Fatal("metrics enabled but no allow-metrics-scrape NetworkPolicy rendered")
			}
			pspec, _ := pol["spec"].(map[string]any)
			sel, _ := pspec["podSelector"].(map[string]any)
			ml, _ := sel["matchLabels"].(map[string]any)
			if ml["app.kubernetes.io/name"] != "backend" {
				t.Errorf("metrics policy must select backend pods only, got podSelector %v", sel)
			}
			ingress := toSlice(pspec["ingress"])
			if len(ingress) != 1 {
				t.Fatalf("metrics policy must carry exactly one ingress rule, got %d", len(ingress))
			}
			rule, _ := ingress[0].(map[string]any)
			if got := rulePorts(rule); !reflect.DeepEqual(got, []string{"9090/TCP"}) {
				t.Errorf("metrics policy ports = %v, want [9090/TCP] only", got)
			}
			from := toSlice(rule["from"])
			if len(from) == 0 {
				t.Error("metrics policy must not be peerless — an empty from admits every source")
			}
			if !reflect.DeepEqual(from, []any{wantPeer}) {
				t.Errorf("metrics policy from = %v, want exactly networkPolicy.prometheusPeers %v", from, []any{wantPeer})
			}

			// No other rule — edge or in-cluster — opens the metrics port,
			// and no edge rule does in any mode. CiliumNetworkPolicies are
			// edge policies by construction.
			for _, np := range selectDocs(docs, "NetworkPolicy") {
				name, _ := meta(np)
				spec, _ := np["spec"].(map[string]any)
				for _, r := range toSlice(spec["ingress"]) {
					rm, _ := r.(map[string]any)
					for _, p := range rulePorts(rm) {
						if p == "9090/TCP" && name != "allow-metrics-scrape" {
							t.Errorf("NetworkPolicy %s opens the metrics port outside allow-metrics-scrape: %v", name, rm)
						}
						if p == "9090/TCP" && isEdgeRule(rm) {
							t.Errorf("NetworkPolicy %s admits the edge on the metrics port: %v", name, rm)
						}
					}
				}
			}
			for _, c := range selectDocs(docs, "CiliumNetworkPolicy") {
				name, _ := meta(c)
				spec, _ := c["spec"].(map[string]any)
				for _, r := range toSlice(spec["ingress"]) {
					rm, _ := r.(map[string]any)
					for _, p := range ciliumPorts(rm) {
						if strings.HasPrefix(p, "9090/") {
							t.Errorf("CiliumNetworkPolicy %s admits the edge on the metrics port: %v", name, p)
						}
					}
				}
			}

			// The metrics Service is ClusterIP and no edge route points at
			// it; the public backend Service must not carry the port.
			svc := serviceNamed(docs, "backend-metrics")
			if svc == nil {
				t.Fatal("metrics enabled but no backend-metrics Service rendered")
			}
			sspec, _ := svc["spec"].(map[string]any)
			if sspec["type"] != "ClusterIP" {
				t.Errorf("backend-metrics Service type = %v, want ClusterIP", sspec["type"])
			}
			if n := len(toSlice(sspec["ports"])); n != 1 {
				t.Errorf("backend-metrics Service ports = %d, want the single metrics port", n)
			}
			backend := serviceNamed(docs, "backend")
			for _, p := range toSlice(backend["spec"].(map[string]any)["ports"]) {
				if pm, _ := p.(map[string]any); pm["name"] == "metrics" || portString(pm["port"]) == "9090" {
					t.Errorf("public backend Service must never carry the metrics port (SEC-33): %v", pm)
				}
			}
			for _, ing := range selectDocs(docs, "Ingress") {
				name, _ := meta(ing)
				spec, _ := ing["spec"].(map[string]any)
				for _, r := range toSlice(spec["rules"]) {
					rm, _ := r.(map[string]any)
					http, _ := rm["http"].(map[string]any)
					for _, p := range toSlice(http["paths"]) {
						pm, _ := p.(map[string]any)
						be, _ := pm["backend"].(map[string]any)
						svcRef, _ := be["service"].(map[string]any)
						if svcRef["name"] == "backend-metrics" {
							t.Errorf("Ingress %s routes edge traffic to backend-metrics", name)
						}
					}
				}
			}
			for _, rt := range selectDocs(docs, "HTTPRoute") {
				name, _ := meta(rt)
				spec, _ := rt["spec"].(map[string]any)
				for _, r := range toSlice(spec["rules"]) {
					rm, _ := r.(map[string]any)
					for _, br := range toSlice(rm["backendRefs"]) {
						bm, _ := br.(map[string]any)
						if bm["name"] == "backend-metrics" {
							t.Errorf("HTTPRoute %s routes edge traffic to backend-metrics", name)
						}
					}
				}
			}
		})
	}

	// Metrics off (the default): no scrape policy, no metrics Service.
	docs := render(t, "minimal-values.yaml")
	if metricsPolicy(docs) != nil {
		t.Error("default render must not contain allow-metrics-scrape")
	}
	if serviceNamed(docs, "backend-metrics") != nil {
		t.Error("default render must not contain the backend-metrics Service")
	}

	// Metrics on without declared scrape peers fails the render (SEC-32 —
	// an empty from would admit every source).
	out := renderErrArgs(t,
		"-f", minimalValues,
		"--set", "backend.metrics.enabled=true")
	if !strings.Contains(out, "prometheusPeers") {
		t.Errorf("metrics enabled without prometheusPeers must fail naming the value, got: %s", out)
	}
}
