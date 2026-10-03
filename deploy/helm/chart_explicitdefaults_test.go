// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package chart_test

// V3.15a — GitOps no-drift contract (backlog 7). GitOps diffing compares
// the rendered manifest with the object the API server stores; on
// unstructured custom resources every schema-defaulted field the manifest
// omits is stored anyway and then shows up as permanent false drift
// (Argo CD OutOfSync / Flux diff). The chart therefore renders the
// API-server defaults explicitly:
//
//   - HTTPRoute (Gateway API CRD defaults): parentRefs[].group =
//     "gateway.networking.k8s.io", parentRefs[].kind = "Gateway",
//     rules[].matches = [{path: {type: PathPrefix, value: /}}],
//     matches[].path.type/value, backendRefs[].group = "",
//     backendRefs[].kind = "Service", backendRefs[].weight = 1.
//   - WorkspaceTemplate (workspaces.cdi.tinyorbit.vn CRD default):
//     spec.linux.adapter = "".
//
// Core-API defaults (Deployment spec.revisionHistoryLimit, pod
// dnsPolicy/restartPolicy/terminationMessagePath, Service
// sessionAffinity/protocol/ClusterIP family fields, ...) are deliberately
// NOT rendered: structured types are merged field-aware by the GitOps
// diff engines and provably do not drift.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// httpRouteSpec extracts the spec map of a rendered HTTPRoute.
func httpRouteSpec(t *testing.T, docs []doc, name string) map[string]any {
	t.Helper()
	for _, d := range selectDocs(docs, "HTTPRoute") {
		if n, _ := meta(d); n == name {
			spec, _ := d["spec"].(map[string]any)
			return spec
		}
	}
	t.Fatalf("no HTTPRoute %q rendered", name)
	return nil
}

// TestHTTPRouteAPIServerDefaultsRendered: every field the HTTPRoute CRD
// defaults server-side must be present in the manifest so a GitOps diff
// of stored-vs-rendered is empty.
func TestHTTPRouteAPIServerDefaultsRendered(t *testing.T) {
	docs := render(t, "gateway-api-values.yaml")
	routes := selectDocs(docs, "HTTPRoute")
	if len(routes) != 2 {
		t.Fatalf("expected portal + session HTTPRoutes, got %d", len(routes))
	}
	for _, d := range routes {
		name, _ := meta(d)
		spec, _ := d["spec"].(map[string]any)
		parents := toSlice(spec["parentRefs"])
		if len(parents) == 0 {
			t.Fatalf("HTTPRoute %s has no parentRefs", name)
		}
		for _, p := range parents {
			pm, _ := p.(map[string]any)
			if pm["group"] != "gateway.networking.k8s.io" {
				t.Errorf("HTTPRoute %s parentRef group = %v, want gateway.networking.k8s.io", name, pm["group"])
			}
			if pm["kind"] != "Gateway" {
				t.Errorf("HTTPRoute %s parentRef kind = %v, want Gateway", name, pm["kind"])
			}
		}
		for i, r := range toSlice(spec["rules"]) {
			rm, _ := r.(map[string]any)
			matches := toSlice(rm["matches"])
			if len(matches) == 0 {
				t.Errorf("HTTPRoute %s rule %d: matches must be explicit (server defaults to PathPrefix /)", name, i)
			}
			for _, m := range matches {
				mm, _ := m.(map[string]any)
				path, _ := mm["path"].(map[string]any)
				if path["type"] != "PathPrefix" {
					t.Errorf("HTTPRoute %s rule %d match path.type = %v, want PathPrefix", name, i, path["type"])
				}
				if _, has := path["value"]; !has {
					t.Errorf("HTTPRoute %s rule %d match missing path.value", name, i)
				}
			}
			for _, b := range toSlice(rm["backendRefs"]) {
				bm, _ := b.(map[string]any)
				g, hasGroup := bm["group"]
				if !hasGroup || g != "" {
					t.Errorf("HTTPRoute %s rule %d backendRef group = %v, want explicit \"\"", name, i, g)
				}
				if bm["kind"] != "Service" {
					t.Errorf("HTTPRoute %s rule %d backendRef kind = %v, want Service", name, i, bm["kind"])
				}
				if w, ok := bm["weight"]; !ok || w != 1 {
					t.Errorf("HTTPRoute %s rule %d backendRef weight = %v, want 1", name, i, w)
				}
			}
		}
	}

	// The session route carries no path distinction of its own — the whole
	// wildcard hostname goes to the session listener — so the server would
	// store the default match on it; the manifest must render it.
	sess := httpRouteSpec(t, docs, "session")
	rules := toSlice(sess["rules"])
	if len(rules) != 1 {
		t.Fatalf("session route must have exactly one rule, got %d", len(rules))
	}
	rm, _ := rules[0].(map[string]any)
	matches := toSlice(rm["matches"])
	if len(matches) != 1 {
		t.Fatalf("session route rule must have exactly one match, got %d", len(matches))
	}
	mm, _ := matches[0].(map[string]any)
	path, _ := mm["path"].(map[string]any)
	if path["type"] != "PathPrefix" || path["value"] != "/" {
		t.Errorf("session route match = %v, want {type: PathPrefix, value: /}", path)
	}
}

// TestHTTPRouteParentRefExplicitValuesWin: the group/kind defaults only
// fill in when the values omit them — an explicit non-Gateway parentRef
// (or a different API group) must pass through untouched.
func TestHTTPRouteParentRefExplicitValuesWin(t *testing.T) {
	docs := renderArgs(t, "-f", filepath.Join("tinycdi", "ci", "gateway-api-values.yaml"),
		"--set", "gatewayApi.parentRefs[0].group=example.io",
		"--set", "gatewayApi.parentRefs[0].kind=MultiClusterService")
	for _, d := range selectDocs(docs, "HTTPRoute") {
		name, _ := meta(d)
		spec, _ := d["spec"].(map[string]any)
		pm, _ := toSlice(spec["parentRefs"])[0].(map[string]any)
		if pm["group"] != "example.io" || pm["kind"] != "MultiClusterService" {
			t.Errorf("HTTPRoute %s: explicit parentRef group/kind overridden: %v/%v", name, pm["group"], pm["kind"])
		}
	}
}

// TestWorkspaceTemplateAdapterRendered: spec.linux.adapter is the one
// field the WorkspaceTemplate CRD defaults (to ""). Every seeded template
// must carry it explicitly; an explicit value (kasm) passes through.
func TestWorkspaceTemplateAdapterRendered(t *testing.T) {
	docs := render(t, "example-values.yaml")
	tpls := selectDocs(docs, "WorkspaceTemplate")
	if len(tpls) == 0 {
		t.Fatal("no seeded WorkspaceTemplate rendered")
	}
	var sawKasm bool
	for _, d := range tpls {
		name, _ := meta(d)
		spec, _ := d["spec"].(map[string]any)
		linux, hasLinux := spec["linux"].(map[string]any)
		if !hasLinux {
			continue
		}
		adapter, has := linux["adapter"]
		if !has {
			t.Errorf("WorkspaceTemplate %s: spec.linux.adapter must be rendered (server default \"\")", name)
			continue
		}
		if adapter == "kasm" {
			sawKasm = true
			continue
		}
		if adapter != "" {
			t.Errorf("WorkspaceTemplate %s: spec.linux.adapter = %v, want \"\" or an explicit adapter", name, adapter)
		}
	}
	if !sawKasm {
		t.Error("expected the seeded kasm template to keep adapter=kasm (explicit value wins)")
	}
}

// TestHTTPRouteRenderedGolden: byte-exact render of the two HTTPRoutes.
// Any change to the defaulted set — a dropped field, a new key — fails
// this golden and forces a deliberate review of the GitOps diff contract.
func TestHTTPRouteRenderedGolden(t *testing.T) {
	want, err := os.ReadFile(filepath.Join("testdata", "httproute-defaults.golden.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := helmTemplate(t, "-f", filepath.Join("tinycdi", "ci", "gateway-api-values.yaml"))
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	const source = "# Source: tinycdi/templates/httproute.yaml"
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
	if got := b.String(); got != string(want) {
		t.Errorf("HTTPRoute render drifted from the explicit-defaults golden "+
			"(deploy/helm/testdata/httproute-defaults.golden.yaml).\n--- got ---\n%s", got)
	}
}
