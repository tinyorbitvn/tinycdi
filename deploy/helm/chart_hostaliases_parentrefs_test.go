// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package chart_test

// CHART-2 (v0.3.2): two additive chart values.
//
//   - backend.hostAliases renders as spec.template.spec.hostAliases on
//     the backend Deployment (e.g. resolving an in-cluster OIDC issuer
//     host to a Service ClusterIP when cluster DNS cannot). Empty by
//     default — the pre-v0.3.2 render is unchanged.
//   - gatewayApi.portalParentRefs / .sessionParentRefs are per-route
//     parentRef overrides: a non-empty list REPLACES the shared
//     gatewayApi.parentRefs for that HTTPRoute (e.g. a wildcard-https
//     listener for *.<sessionDomain> and a cdi-https listener for the
//     portal host). Empty falls back to the shared list, so existing
//     values files render identically. With per-route lists on BOTH
//     routes the shared list may be left empty.

import (
	"path/filepath"
	"strings"
	"testing"
)

// backendPodSpec returns the pod spec of a rendered Deployment doc.
func backendPodSpec(d doc) map[string]any {
	spec, _ := d["spec"].(map[string]any)
	tpl, _ := spec["template"].(map[string]any)
	ps, _ := tpl["spec"].(map[string]any)
	return ps
}

// routeParentRefNames returns the parentRef {name, sectionName} pairs of
// a rendered HTTPRoute.
func routeParentRefNames(t *testing.T, docs []doc, route string) []string {
	t.Helper()
	spec := httpRouteSpec(t, docs, route)
	var out []string
	for _, pr := range toSlice(spec["parentRefs"]) {
		pm, _ := pr.(map[string]any)
		name, _ := pm["name"].(string)
		section, _ := pm["sectionName"].(string)
		out = append(out, name+"/"+section)
	}
	return out
}

// TestBackendHostAliasesDefault: the default render carries NO
// hostAliases on the backend pod — the value is additive-only.
func TestBackendHostAliasesDefault(t *testing.T) {
	for _, vf := range []string{"minimal-values.yaml", "gateway-api-values.yaml"} {
		docs := render(t, vf)
		dep := deployment(docs, "backend")
		if dep == nil {
			t.Fatalf("%s: no backend Deployment rendered", vf)
		}
		if _, has := backendPodSpec(dep)["hostAliases"]; has {
			t.Errorf("%s: default render must carry no backend hostAliases", vf)
		}
	}
}

// TestBackendHostAliasesRendered: backend.hostAliases lands verbatim on
// the backend pod spec, and only there.
func TestBackendHostAliasesRendered(t *testing.T) {
	docs := renderArgs(t,
		"-f", filepath.Join("tinycdi", "ci", "minimal-values.yaml"),
		"--set", "backend.hostAliases[0].ip=10.20.30.40",
		"--set", "backend.hostAliases[0].hostnames[0]=idp.cluster.example.net",
		"--set", "backend.hostAliases[0].hostnames[1]=keycloak")
	dep := deployment(docs, "backend")
	if dep == nil {
		t.Fatal("no backend Deployment rendered")
	}
	aliases := toSlice(backendPodSpec(dep)["hostAliases"])
	if len(aliases) != 1 {
		t.Fatalf("backend hostAliases = %v, want 1 entry", aliases)
	}
	a, _ := aliases[0].(map[string]any)
	if a["ip"] != "10.20.30.40" {
		t.Errorf("hostAlias ip = %v, want 10.20.30.40", a["ip"])
	}
	hosts := toSlice(a["hostnames"])
	if len(hosts) != 2 || hosts[0] != "idp.cluster.example.net" || hosts[1] != "keycloak" {
		t.Errorf("hostAlias hostnames = %v, want [idp.cluster.example.net keycloak]", hosts)
	}
	for _, name := range []string{"frontend", "operator"} {
		if d := deployment(docs, name); d != nil {
			if _, has := backendPodSpec(d)["hostAliases"]; has {
				t.Errorf("%s Deployment must not carry backend.hostAliases", name)
			}
		}
	}
}

// TestBackendHostAliasesSchema: a non-array hostAliases is a schema error.
func TestBackendHostAliasesSchema(t *testing.T) {
	out := renderErrArgs(t,
		"-f", filepath.Join("tinycdi", "ci", "minimal-values.yaml"),
		"--set", "backend.hostAliases=not-a-list")
	if !strings.Contains(out, "hostAliases") {
		t.Errorf("non-array backend.hostAliases must be rejected naming hostAliases, got: %s", out)
	}
}

// TestHTTPRouteSharedParentRefsUnchanged: the single shared
// gatewayApi.parentRefs list still parents BOTH routes (pre-v0.3.2
// shape, exercised here with --set to prove the file-format path too).
func TestHTTPRouteSharedParentRefsUnchanged(t *testing.T) {
	docs := render(t, "gateway-api-values.yaml")
	for _, route := range []string{"portal", "session"} {
		got := routeParentRefNames(t, docs, route)
		if len(got) != 1 || got[0] != "shared-gateway/https" {
			t.Errorf("HTTPRoute %s parentRefs = %v, want [shared-gateway/https]", route, got)
		}
	}
}

// TestHTTPRoutePerRouteParentRefsOverride: a non-empty per-route list
// replaces the shared parentRefs for that route only; the other route
// keeps the shared list.
func TestHTTPRoutePerRouteParentRefsOverride(t *testing.T) {
	docs := renderArgs(t,
		"-f", filepath.Join("tinycdi", "ci", "minimal-values.yaml"),
		"--set", "gatewayApi.enabled=true",
		"--set", "gatewayApi.parentRefs[0].name=shared-gateway",
		"--set", "gatewayApi.parentRefs[0].sectionName=https",
		"--set", "gatewayApi.sessionParentRefs[0].name=shared-gateway",
		"--set", "gatewayApi.sessionParentRefs[0].sectionName=wildcard-https")
	if got := routeParentRefNames(t, docs, "portal"); len(got) != 1 || got[0] != "shared-gateway/https" {
		t.Errorf("portal parentRefs = %v, want [shared-gateway/https] (shared fallback)", got)
	}
	if got := routeParentRefNames(t, docs, "session"); len(got) != 1 || got[0] != "shared-gateway/wildcard-https" {
		t.Errorf("session parentRefs = %v, want [shared-gateway/wildcard-https] (override)", got)
	}
}

// TestHTTPRoutePerRouteParentRefsOnly: with per-route lists on BOTH
// routes the shared parentRefs may stay empty — GitOps wrappers that
// need distinct listeners per route no longer render their own
// HTTPRoutes.
func TestHTTPRoutePerRouteParentRefsOnly(t *testing.T) {
	docs := renderArgs(t,
		"-f", filepath.Join("tinycdi", "ci", "minimal-values.yaml"),
		"--set", "gatewayApi.enabled=true",
		"--set", "gatewayApi.portalParentRefs[0].name=edge-gw",
		"--set", "gatewayApi.portalParentRefs[0].sectionName=cdi-https",
		"--set", "gatewayApi.sessionParentRefs[0].name=edge-gw",
		"--set", "gatewayApi.sessionParentRefs[0].sectionName=wildcard-https")
	if got := routeParentRefNames(t, docs, "portal"); len(got) != 1 || got[0] != "edge-gw/cdi-https" {
		t.Errorf("portal parentRefs = %v, want [edge-gw/cdi-https]", got)
	}
	if got := routeParentRefNames(t, docs, "session"); len(got) != 1 || got[0] != "edge-gw/wildcard-https" {
		t.Errorf("session parentRefs = %v, want [edge-gw/wildcard-https]", got)
	}
}

// TestHTTPRoutePerRouteParentRefsRequired: gatewayApi.enabled with a
// route that resolves to ZERO parentRefs still fails the render — a
// route-less HTTPRoute would silently never attach.
func TestHTTPRoutePerRouteParentRefsRequired(t *testing.T) {
	// Only the portal override set: the session route has no refs.
	out := renderErrArgs(t,
		"-f", filepath.Join("tinycdi", "ci", "minimal-values.yaml"),
		"--set", "gatewayApi.enabled=true",
		"--set", "gatewayApi.portalParentRefs[0].name=edge-gw",
		"--set", "gatewayApi.portalParentRefs[0].sectionName=cdi-https")
	if !strings.Contains(out, "parentRef") {
		t.Errorf("unresolved route must fail mentioning parentRef, got: %s", out)
	}
	// Nothing at all: the original failure mode.
	out = renderErrArgs(t,
		"-f", filepath.Join("tinycdi", "ci", "minimal-values.yaml"),
		"--set", "gatewayApi.enabled=true")
	if !strings.Contains(out, "parentRef") {
		t.Errorf("gatewayApi.enabled without any parentRefs must fail mentioning parentRef, got: %s", out)
	}
}

// TestHTTPRoutePerRouteParentRefsDefaults: the API-server defaults the
// template renders (group gateway.networking.k8s.io, kind Gateway) apply
// to per-route entries too, and an explicit group/kind still wins.
func TestHTTPRoutePerRouteParentRefsDefaults(t *testing.T) {
	docs := renderArgs(t,
		"-f", filepath.Join("tinycdi", "ci", "minimal-values.yaml"),
		"--set", "gatewayApi.enabled=true",
		"--set", "gatewayApi.portalParentRefs[0].name=edge-gw",
		"--set", "gatewayApi.portalParentRefs[0].sectionName=cdi-https",
		"--set", "gatewayApi.sessionParentRefs[0].name=edge-gw",
		"--set", "gatewayApi.sessionParentRefs[0].sectionName=wildcard-https",
		"--set", "gatewayApi.sessionParentRefs[0].group=example.io",
		"--set", "gatewayApi.sessionParentRefs[0].kind=MultiClusterService")
	spec := httpRouteSpec(t, docs, "portal")
	pm, _ := toSlice(spec["parentRefs"])[0].(map[string]any)
	if pm["group"] != "gateway.networking.k8s.io" || pm["kind"] != "Gateway" {
		t.Errorf("portal parentRef group/kind = %v/%v, want defaulted gateway.networking.k8s.io/Gateway", pm["group"], pm["kind"])
	}
	spec = httpRouteSpec(t, docs, "session")
	pm, _ = toSlice(spec["parentRefs"])[0].(map[string]any)
	if pm["group"] != "example.io" || pm["kind"] != "MultiClusterService" {
		t.Errorf("session parentRef explicit group/kind overridden: %v/%v", pm["group"], pm["kind"])
	}
}

// TestHTTPRoutePerRouteSectionNameRequired: SEC-35 applies to per-route
// entries — a parentRef without sectionName binds every listener on the
// Gateway including plain HTTP, so the schema/render must refuse it.
func TestHTTPRoutePerRouteSectionNameRequired(t *testing.T) {
	out := renderErrArgs(t,
		"-f", filepath.Join("tinycdi", "ci", "minimal-values.yaml"),
		"--set", "gatewayApi.enabled=true",
		"--set", "gatewayApi.parentRefs[0].name=shared-gateway",
		"--set", "gatewayApi.parentRefs[0].sectionName=https",
		"--set", "gatewayApi.sessionParentRefs[0].name=shared-gateway")
	if !strings.Contains(out, "sectionName") {
		t.Errorf("per-route parentRef without sectionName must fail mentioning sectionName, got: %s", out)
	}
}
