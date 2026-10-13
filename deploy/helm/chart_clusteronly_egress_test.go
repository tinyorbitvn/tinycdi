// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package chart_test

// ClusterOnly egress (v1.0): restricted is the default — the operator
// confines ClusterOnly egress to namespaces labelled
// workspaces.cdi.tinyorbit.vn/tenant when no flag is rendered.
// runtime.networkProfiles.clusterOnly.egress* renders
// --runtime-clusteronly-egress=<json> on the operator Deployment — and
// ONLY when at least one of the three values is non-empty — while
// clusterOnly.unrestricted renders the literal
// --runtime-clusteronly-egress=unrestricted opt-out. The operator refuses
// a malformed value at startup
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

// TestClusterOnlyEgress_DefaultRendersNoFlagAndNoWarning: restricted is
// the 1.0 default — no --runtime-clusteronly-egress flag is rendered and
// the install NOTES carry no ClusterOnly warning.
func TestClusterOnlyEgress_DefaultRendersNoFlagAndNoWarning(t *testing.T) {
	minimal := filepath.Join("tinycdi", "ci", "minimal-values.yaml")

	for _, extra := range [][]string{
		{},
		{"--set-json", `runtime.networkProfiles.clusterOnly.egressNamespaceSelector={}`},
		{"--set-json", `runtime.networkProfiles.clusterOnly.egressPodSelector={}`},
		{"--set-json", `runtime.networkProfiles.clusterOnly.egressPorts=[]`},
		{"--set", `runtime.networkProfiles.clusterOnly.unrestricted=false`},
		{"--set-json", `runtime.networkProfiles.clusterOnly=null`},
		{"--set-json", `runtime.networkProfiles=null`},
	} {
		args := append([]string{"-f", minimal}, extra...)
		for _, a := range firstContainerArgs(deployment(renderArgs(t, args...), "operator")) {
			if strings.HasPrefix(a, "--runtime-clusteronly-egress=") {
				t.Errorf("args %v: the restricted default must render no flag, got %q", extra, a)
			}
		}
	}

	if notes := renderNotes(t, "-f", minimal); strings.Contains(notes, "ClusterOnly egress: WARNING") {
		t.Errorf("default NOTES must not warn about ClusterOnly egress, got:\n%s", notes)
	}
}

// TestClusterOnlyEgress_UnrestrictedRendersFlag: unrestricted: true is
// the explicit opt-out of the restricted default — it renders the literal
// flag value (not JSON) and the NOTES state the opt-out on one line.
// Combining it with the narrowing selectors fails the render.
func TestClusterOnlyEgress_UnrestrictedRendersFlag(t *testing.T) {
	minimal := filepath.Join("tinycdi", "ci", "minimal-values.yaml")

	docs := renderArgs(t, "-f", minimal,
		"--set", "runtime.networkProfiles.clusterOnly.unrestricted=true")
	found := false
	for _, a := range firstContainerArgs(deployment(docs, "operator")) {
		if a == "--runtime-clusteronly-egress=unrestricted" {
			found = true
		}
		if strings.HasPrefix(a, "--runtime-clusteronly-egress=") && a != "--runtime-clusteronly-egress=unrestricted" {
			t.Errorf("unrestricted must render the literal value, got %q", a)
		}
	}
	if !found {
		t.Fatal("unrestricted=true: no --runtime-clusteronly-egress=unrestricted rendered")
	}

	notes := renderNotes(t, "-f", minimal,
		"--set", "runtime.networkProfiles.clusterOnly.unrestricted=true")
	lines := 0
	for _, l := range strings.Split(notes, "\n") {
		if strings.Contains(l, "ClusterOnly egress") {
			lines++
		}
	}
	if lines != 1 {
		t.Fatalf("NOTES must state the unrestricted opt-out on exactly one line, got:\n%s", notes)
	}

	// unrestricted and the narrowing selectors are mutually exclusive.
	out := renderErrArgs(t, "-f", minimal,
		"--set", "runtime.networkProfiles.clusterOnly.unrestricted=true",
		"--set-json", `runtime.networkProfiles.clusterOnly.egressPorts=[{"protocol":"TCP","port":443}]`)
	if !strings.Contains(out, "clusterOnly.unrestricted cannot be combined with egress selectors") {
		t.Fatalf("unrestricted + egress selectors must fail naming the conflict, got: %s", out)
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

