// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package chart_test

// TOPO-1: runtime.topologySpread.enabled defaults to true and then renders
// NO flag (the operator flag --runtime-topology-spread defaults to true),
// so the operator Deployment is identical to the pre-v0.3.1 render.
// enabled=false renders --runtime-topology-spread=false — the only value
// that changes the manifest.
//
// The constraint this flag switches on is pinned by the backend unit test
// (internal/runtime/linux TestTopologySpread_Enabled): whenUnsatisfiable
// ScheduleAnyway, maxSkew 1 — the advisor's condition for defaulting ON.

import (
	"path/filepath"
	"strings"
	"testing"
)

func topologySpreadFlag(t *testing.T, docs []doc) (string, bool) {
	t.Helper()
	op := deployment(docs, "operator")
	if op == nil {
		t.Fatal("no operator Deployment rendered")
	}
	for _, a := range firstContainerArgs(op) {
		if strings.HasPrefix(a, "--runtime-topology-spread") {
			return a, true
		}
	}
	return "", false
}

func TestRuntimeTopologySpreadDefault(t *testing.T) {
	minimal := filepath.Join("tinycdi", "ci", "minimal-values.yaml")

	if a, ok := topologySpreadFlag(t, renderArgs(t, "-f", minimal)); ok {
		t.Errorf("default render must carry no topology-spread flag (enabled defaults true), got %q", a)
	}
	if a, ok := topologySpreadFlag(t, renderArgs(t, "-f", minimal,
		"--set", "runtime.topologySpread.enabled=true")); ok {
		t.Errorf("explicit true must carry no topology-spread flag, got %q", a)
	}

	a, ok := topologySpreadFlag(t, renderArgs(t, "-f", minimal,
		"--set", "runtime.topologySpread.enabled=false"))
	if !ok || a != "--runtime-topology-spread=false" {
		t.Errorf("enabled=false must render --runtime-topology-spread=false, got %q (present=%v)", a, ok)
	}
}

// The value is boolean only — a string or null is a schema error.
func TestRuntimeTopologySpreadSchema(t *testing.T) {
	minimal := filepath.Join("tinycdi", "ci", "minimal-values.yaml")
	for _, v := range []string{`"false"`, `0`, `"yes"`, `[]`} {
		out := renderErrArgs(t, "-f", minimal,
			"--set-json", `runtime.topologySpread.enabled=`+v)
		if !strings.Contains(out, "enabled") {
			t.Errorf("value %s must be rejected naming enabled, got: %s", v, out)
		}
	}
	out := renderErrArgs(t, "-f", minimal, "--set", "runtime.topologySpread.bogus=true")
	if !strings.Contains(out, "bogus") && !strings.Contains(out, "topologySpread") {
		t.Errorf("unknown runtime.topologySpread key must be rejected, got: %s", out)
	}
}
