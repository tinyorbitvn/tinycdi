// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package chart_test

// Default-render gate (v1.0): the WHOLE chart rendered with the lightest
// ci values file must stay byte-identical to the checked-in golden, so an
// opt-in hardening knob (runtime.networkProfiles.clusterOnly.*,
// managedNamespaces[].namespaceQuota, ...) can never alter the default
// manifests unnoticed. An intentional default-render change regenerates
// the golden, reviewed like any other behavioural diff:
//
//	helm template tcdi ./tinycdi --namespace tcdi-system --include-crds \
//	  -f tinycdi/ci/minimal-values.yaml > testdata/default-render.golden.yaml

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDefaultRenderUnchangedGolden: byte-exact full render (every object,
// CRDs included) vs the pre-change golden.
func TestDefaultRenderUnchangedGolden(t *testing.T) {
	want, err := os.ReadFile(filepath.Join("testdata", "default-render.golden.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := helmTemplate(t, "-f", filepath.Join("tinycdi", "ci", "minimal-values.yaml"))
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	if got := string(out); got != string(want) {
		t.Errorf("default render drifted from the golden "+
			"(deploy/helm/testdata/default-render.golden.yaml).\n--- got ---\n%s", got)
	}
}

// The opt-in hardening values at their shipped defaults must not change
// the render either — an explicit empty block is the same as absent.
func TestDefaultRenderUnchangedWithExplicitEmptyHardening(t *testing.T) {
	out, err := helmTemplate(t, "-f", filepath.Join("tinycdi", "ci", "minimal-values.yaml"),
		"--set-json", `runtime.networkProfiles.clusterOnly={`+
			`"egressNamespaceSelector":{},"egressPodSelector":{},"egressPorts":[]}`)
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	want, err := os.ReadFile(filepath.Join("testdata", "default-render.golden.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(out); got != string(want) {
		t.Errorf("explicit-empty clusterOnly values changed the default render.\n--- got ---\n%s", got)
	}

	// And a managedNamespaces entry WITHOUT namespaceQuota must produce
	// no ResourceQuota/LimitRange — pinned separately in
	// chart_namespacequota_test.go; here we assert the rendered docs
	// carry neither kind at the default.
	docs := decodeDocs(t, out)
	var stray []string
	for _, d := range docs {
		k, _ := d["kind"].(string)
		if k == "ResourceQuota" || k == "LimitRange" {
			n, ns := meta(d)
			stray = append(stray, ns+"/"+n)
		}
	}
	if len(stray) != 0 {
		t.Errorf("default render must contain no ResourceQuota/LimitRange, got %v", stray)
	}
	if !strings.Contains(string(out), "kind: Namespace") {
		t.Error("default render must still contain the managed Namespaces")
	}
}
