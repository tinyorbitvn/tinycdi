// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package chart_test

// CHART-3 (v0.4): per-route HTTPRoute annotations and route names.
//
//   - gatewayApi.portalAnnotations / .sessionAnnotations merge OVER the
//     shared gatewayApi.annotations — on a key collision the per-route
//     value wins (e.g. a CDN proxy flag ON for the portal route, OFF
//     for the long-lived-websocket session route, or a link annotation
//     on the portal route only). Same rule as ingress.portalAnnotations.
//   - gatewayApi.portalRouteName / .sessionRouteName rename the two
//     HTTPRoutes; the defaults render the same names every earlier
//     chart release produced, so a GitOps wrapper that pairs
//     hand-written routes by convention (e.g. a <name>-redirect route)
//     keeps working unchanged.
//
// Byte-identity of the shared-only render is pinned by
// TestHTTPRouteRenderedGolden (chart_explicitdefaults_test.go): the
// checked-in golden predates this change, so a passing golden proves
// the pre-v0.4 render is unchanged.

import (
	"path/filepath"
	"strings"
	"testing"
)

// httpRouteAnnotations returns the metadata.annotations map of a
// rendered HTTPRoute (nil when the route carries none).
func httpRouteAnnotations(t *testing.T, docs []doc, name string) map[string]any {
	t.Helper()
	for _, d := range selectDocs(docs, "HTTPRoute") {
		if n, _ := meta(d); n == name {
			m, _ := d["metadata"].(map[string]any)
			ann, _ := m["annotations"].(map[string]any)
			return ann
		}
	}
	t.Fatalf("no HTTPRoute %q rendered", name)
	return nil
}

// TestHTTPRouteSharedAnnotationsUnchanged: with only the shared
// gatewayApi.annotations set, BOTH routes keep the default names and
// carry the shared map verbatim; with no annotations at all the routes
// render no annotations key.
func TestHTTPRouteSharedAnnotationsUnchanged(t *testing.T) {
	docs := render(t, "gateway-api-values.yaml")
	for _, route := range []string{"portal", "session"} {
		ann := httpRouteAnnotations(t, docs, route)
		if len(ann) != 1 || ann["example.com/route-owner"] != "tinycdi" {
			t.Errorf("HTTPRoute %s annotations = %v, want the shared map verbatim", route, ann)
		}
	}
	docs = renderArgs(t,
		"-f", filepath.Join("tinycdi", "ci", "minimal-values.yaml"),
		"--set", "gatewayApi.enabled=true",
		"--set", "gatewayApi.parentRefs[0].name=shared-gateway",
		"--set", "gatewayApi.parentRefs[0].sectionName=https")
	for _, route := range []string{"portal", "session"} {
		if ann := httpRouteAnnotations(t, docs, route); ann != nil {
			t.Errorf("HTTPRoute %s annotations = %v, want none rendered", route, ann)
		}
	}
}

// TestHTTPRoutePerRouteAnnotationsMerge: per-route annotations merge
// over the shared map — the per-route value wins on a key collision,
// shared keys pass through, and neither route sees the other's extras.
func TestHTTPRoutePerRouteAnnotationsMerge(t *testing.T) {
	docs := renderArgs(t,
		"-f", filepath.Join("tinycdi", "ci", "minimal-values.yaml"),
		"--set", "gatewayApi.enabled=true",
		"--set", "gatewayApi.parentRefs[0].name=edge-gw",
		"--set", "gatewayApi.parentRefs[0].sectionName=https",
		"--set", "gatewayApi.annotations.example\\.com/shared=present",
		"--set", "gatewayApi.annotations.example\\.com/cdn-proxy=off",
		"--set", "gatewayApi.portalAnnotations.example\\.com/cdn-proxy=on",
		"--set", "gatewayApi.portalAnnotations.example\\.com/route-link=docs",
		"--set", "gatewayApi.sessionAnnotations.example\\.com/streaming=websocket")
	portal := httpRouteAnnotations(t, docs, "portal")
	if portal["example.com/shared"] != "present" {
		t.Errorf("portal shared annotation = %v, want present", portal["example.com/shared"])
	}
	if portal["example.com/cdn-proxy"] != "on" {
		t.Errorf("portal cdn-proxy = %v, want on (per-route wins the collision)", portal["example.com/cdn-proxy"])
	}
	if portal["example.com/route-link"] != "docs" {
		t.Errorf("portal route-link = %v, want docs", portal["example.com/route-link"])
	}
	if _, has := portal["example.com/streaming"]; has {
		t.Errorf("portal must not carry the session-only annotation: %v", portal)
	}
	session := httpRouteAnnotations(t, docs, "session")
	if session["example.com/shared"] != "present" {
		t.Errorf("session shared annotation = %v, want present", session["example.com/shared"])
	}
	if session["example.com/cdn-proxy"] != "off" {
		t.Errorf("session cdn-proxy = %v, want off (shared value kept)", session["example.com/cdn-proxy"])
	}
	if session["example.com/streaming"] != "websocket" {
		t.Errorf("session streaming = %v, want websocket", session["example.com/streaming"])
	}
	if _, has := session["example.com/route-link"]; has {
		t.Errorf("session must not carry the portal-only annotation: %v", session)
	}
}

// TestHTTPRouteCustomNames: both routes take gatewayApi.*RouteName and
// nothing else about them changes — parentRefs still resolve and the
// portal/wildcard hostnames stay on their own routes.
func TestHTTPRouteCustomNames(t *testing.T) {
	docs := renderArgs(t,
		"-f", filepath.Join("tinycdi", "ci", "minimal-values.yaml"),
		"--set", "gatewayApi.enabled=true",
		"--set", "gatewayApi.parentRefs[0].name=edge-gw",
		"--set", "gatewayApi.parentRefs[0].sectionName=https",
		"--set", "gatewayApi.portalRouteName=portal-edge",
		"--set", "gatewayApi.sessionRouteName=session-edge")
	routes := selectDocs(docs, "HTTPRoute")
	if len(routes) != 2 {
		t.Fatalf("expected 2 HTTPRoutes, got %d", len(routes))
	}
	names := map[string]bool{}
	for _, d := range routes {
		n, _ := meta(d)
		names[n] = true
	}
	for _, want := range []string{"portal-edge", "session-edge"} {
		if !names[want] {
			t.Errorf("no HTTPRoute named %q rendered (got %v)", want, names)
		}
	}
	for _, old := range []string{"portal", "session"} {
		if names[old] {
			t.Errorf("HTTPRoute %q must be renamed, got %v", old, names)
		}
	}
	for _, route := range []string{"portal-edge", "session-edge"} {
		if got := routeParentRefNames(t, docs, route); len(got) != 1 || got[0] != "edge-gw/https" {
			t.Errorf("HTTPRoute %s parentRefs = %v, want [edge-gw/https]", route, got)
		}
	}
	if hosts := toSlice(httpRouteSpec(t, docs, "portal-edge")["hostnames"]); len(hosts) != 1 || hosts[0] != "portal.lab.example.net" {
		t.Errorf("portal-edge hostnames = %v, want [portal.lab.example.net]", hosts)
	}
	if hosts := toSlice(httpRouteSpec(t, docs, "session-edge")["hostnames"]); len(hosts) != 1 || hosts[0] != "*.session.lab.example.net" {
		t.Errorf("session-edge hostnames = %v, want [*.session.lab.example.net]", hosts)
	}
}

// TestHTTPRouteNameSchema: the route names land on HTTPRoute
// metadata.name, so the schema enforces DNS-1123 subdomain on both.
func TestHTTPRouteNameSchema(t *testing.T) {
	base := []string{
		"-f", filepath.Join("tinycdi", "ci", "minimal-values.yaml"),
		"--set", "gatewayApi.enabled=true",
		"--set", "gatewayApi.parentRefs[0].name=edge-gw",
		"--set", "gatewayApi.parentRefs[0].sectionName=https",
	}
	for _, bad := range []string{
		"Portal",                       // uppercase
		"-portal",                      // leading dash
		"portal-",                      // trailing dash
		"portal_edge",                  // underscore
		"a..b",                         // empty label
		strings.Repeat("a", 64) + ".b", // label > 63 chars
	} {
		out := renderErrArgs(t, append(base, "--set", "gatewayApi.portalRouteName="+bad)...)
		if !strings.Contains(out, "portalRouteName") {
			t.Errorf("bad portalRouteName %q must fail naming portalRouteName, got: %s", bad, out)
		}
	}
	out := renderErrArgs(t, append(base, "--set", "gatewayApi.sessionRouteName=Not_DNS")...)
	if !strings.Contains(out, "sessionRouteName") {
		t.Errorf("bad sessionRouteName must fail naming sessionRouteName, got: %s", out)
	}
}

// TestHTTPRouteNamesMustDiffer: two HTTPRoutes sharing one metadata.name
// collide on apply — the render refuses it.
func TestHTTPRouteNamesMustDiffer(t *testing.T) {
	out := renderErrArgs(t,
		"-f", filepath.Join("tinycdi", "ci", "minimal-values.yaml"),
		"--set", "gatewayApi.enabled=true",
		"--set", "gatewayApi.parentRefs[0].name=edge-gw",
		"--set", "gatewayApi.parentRefs[0].sectionName=https",
		"--set", "gatewayApi.portalRouteName=edge",
		"--set", "gatewayApi.sessionRouteName=edge")
	if !strings.Contains(out, "must differ") {
		t.Errorf("equal route names must fail mentioning the difference rule, got: %s", out)
	}
}
