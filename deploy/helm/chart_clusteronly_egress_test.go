// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package chart_test

// Opt-in ClusterOnly egress narrowing (v1.0 chart hardening):
// runtime.networkProfiles.clusterOnly.egress* renders
// --runtime-clusteronly-egress on the operator Deployment — and ONLY
// when at least one of the three values is non-empty, so the default
// render stays byte-identical (any pod in any namespace on any port).
// The operator refuses a malformed value at startup
// (cmd/operator TestParseClusterOnlyEgress) and the backend test pins
// the resulting NetworkPolicy shape.

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// clusterOnlyEgressFlag returns the decoded JSON of the rendered
// --runtime-clusteronly-egress arg, or (nil, false) when absent.
func clusterOnlyEgressFlag(t *testing.T, docs []doc) (map[string]any, bool) {
	t.Helper()
	op := deployment(docs, "operator")
	if op == nil {
		t.Fatal("no operator Deployment rendered")
	}
	for _, a := range firstContainerArgs(op) {
		if strings.HasPrefix(a, "--runtime-clusteronly-egress=") {
			var v map[string]any
			if err := json.Unmarshal([]byte(strings.TrimPrefix(a, "--runtime-clusteronly-egress=")), &v); err != nil {
				t.Fatalf("rendered flag is not JSON: %v (%q)", err, a)
			}
			return v, true
		}
	}
	return nil, false
}

func TestClusterOnlyEgressDefaultRendersNoFlag(t *testing.T) {
	minimal := filepath.Join("tinycdi", "ci", "minimal-values.yaml")

	for _, extra := range [][]string{
		{},
		{"--set-json", `runtime.networkProfiles.clusterOnly.egressNamespaceSelector={}`},
		{"--set-json", `runtime.networkProfiles.clusterOnly.egressPodSelector={}`},
		{"--set-json", `runtime.networkProfiles.clusterOnly.egressPorts=[]`},
		{"--set-json", `runtime.networkProfiles.clusterOnly=null`},
		{"--set-json", `runtime.networkProfiles=null`},
	} {
		args := append([]string{"-f", minimal}, extra...)
		if v, ok := clusterOnlyEgressFlag(t, renderArgs(t, args...)); ok {
			t.Errorf("args %v: empty selector set must render no flag, got %v", extra, v)
		}
	}
}

// A set namespaceSelector renders the flag carrying exactly that
// selector; the operator turns it into the single egress peer.
func TestClusterOnlyEgressRendersNamespaceSelector(t *testing.T) {
	minimal := filepath.Join("tinycdi", "ci", "minimal-values.yaml")
	v, ok := clusterOnlyEgressFlag(t, renderArgs(t, "-f", minimal,
		"--set-json", `runtime.networkProfiles.clusterOnly.egressNamespaceSelector=`+
			`{"matchExpressions":[{"key":"workspaces.cdi.tinyorbit.vn/tenant","operator":"Exists"}]}`))
	if !ok {
		t.Fatal("egressNamespaceSelector set: no --runtime-clusteronly-egress rendered")
	}
	ns, _ := v["namespaceSelector"].(map[string]any)
	exprs := toSlice(ns["matchExpressions"])
	if len(exprs) != 1 {
		t.Fatalf("namespaceSelector = %v, want one matchExpression", v)
	}
	expr, _ := exprs[0].(map[string]any)
	if expr["key"] != "workspaces.cdi.tinyorbit.vn/tenant" || expr["operator"] != "Exists" {
		t.Fatalf("namespaceSelector matchExpression = %v", expr)
	}
	if _, has := v["podSelector"]; has {
		t.Errorf("empty egressPodSelector must not reach the flag: %v", v)
	}
	if _, has := v["ports"]; has {
		t.Errorf("empty egressPorts must not reach the flag: %v", v)
	}
}

// podSelector and ports compose on the same flag; a podSelector alone is
// still emitted (it scopes to the workspace's own namespace).
func TestClusterOnlyEgressRendersPodSelectorAndPorts(t *testing.T) {
	minimal := filepath.Join("tinycdi", "ci", "minimal-values.yaml")
	v, ok := clusterOnlyEgressFlag(t, renderArgs(t, "-f", minimal,
		"--set-json", `runtime.networkProfiles.clusterOnly.egressPodSelector=`+
			`{"matchLabels":{"app.kubernetes.io/name":"postgres"}}`,
		"--set-json", `runtime.networkProfiles.clusterOnly.egressPorts=`+
			`[{"protocol":"TCP","port":5432}]`))
	if !ok {
		t.Fatal("podSelector+ports set: no --runtime-clusteronly-egress rendered")
	}
	ps, _ := v["podSelector"].(map[string]any)
	labels, _ := ps["matchLabels"].(map[string]any)
	if labels["app.kubernetes.io/name"] != "postgres" {
		t.Fatalf("podSelector = %v", v)
	}
	ports := toSlice(v["ports"])
	if len(ports) != 1 {
		t.Fatalf("ports = %v, want one entry", v)
	}
	p, _ := ports[0].(map[string]any)
	if p["protocol"] != "TCP" || p["port"] != float64(5432) {
		t.Fatalf("ports entry = %v", p)
	}
}

// The schema rejects unknown keys and wrongly-typed selectors so a typo
// can never reach the flag silently.
func TestClusterOnlyEgressSchema(t *testing.T) {
	minimal := filepath.Join("tinycdi", "ci", "minimal-values.yaml")
	for _, tc := range []struct {
		name string
		set  string
	}{
		{"unknown clusterOnly key", `runtime.networkProfiles.clusterOnly.bogus=true`},
		{"unknown networkProfiles key", `runtime.networkProfiles.internetOnly={}`},
		{"selector not an object", `runtime.networkProfiles.clusterOnly.egressNamespaceSelector="tenant"`},
		{"ports not an array", `runtime.networkProfiles.clusterOnly.egressPorts={}`},
		{"bad port protocol", `runtime.networkProfiles.clusterOnly.egressPorts=[{"protocol":"XYZ","port":1}]`},
		{"port zero", `runtime.networkProfiles.clusterOnly.egressPorts=[{"protocol":"TCP","port":0}]`},
		{"endPort without port", `runtime.networkProfiles.clusterOnly.egressPorts=[{"protocol":"TCP","endPort":9000}]`},
		// Selector shape is validated at render time too (the operator
		// additionally rejects bad selectors at startup).
		{"bad matchExpression operator", `runtime.networkProfiles.clusterOnly.egressNamespaceSelector={"matchExpressions":[{"key":"k","operator":"Sometimes","values":["v"]}]}`},
		{"empty matchExpression key", `runtime.networkProfiles.clusterOnly.egressNamespaceSelector={"matchExpressions":[{"key":"","operator":"Exists"}]}`},
		{"unknown selector key", `runtime.networkProfiles.clusterOnly.egressNamespaceSelector={"matchLabel":{"a":"b"}}`},
	} {
		out := renderErrArgs(t, "-f", minimal, "--set-json", tc.set)
		if !strings.Contains(out, "clusterOnly") && !strings.Contains(out, "networkProfiles") {
			t.Errorf("%s (%s) must be rejected naming the block, got: %s", tc.name, tc.set, out)
		}
	}
}
