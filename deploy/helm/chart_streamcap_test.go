// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package chart_test

// backend.sessionStreamCap (V11): renders -session-stream-cap on the
// backend Deployment at the shipped default and under an override, and
// the schema refuses values < 1 — the per-workspace upstream bound
// cannot be disabled.

import (
	"path/filepath"
	"strings"
	"testing"
)

// streamCapArg returns the rendered -session-stream-cap arg on the
// backend Deployment, or false when absent.
func streamCapArg(t *testing.T, docs []doc) (string, bool) {
	t.Helper()
	be := deployment(docs, "backend")
	if be == nil {
		t.Fatal("no backend Deployment rendered")
	}
	for _, a := range firstContainerArgs(be) {
		if strings.HasPrefix(a, "-session-stream-cap=") {
			return strings.TrimPrefix(a, "-session-stream-cap="), true
		}
	}
	return "", false
}

func TestSessionStreamCapDefaultRenders(t *testing.T) {
	v, ok := streamCapArg(t, renderArgs(t, "-f", filepath.Join("tinycdi", "ci", "minimal-values.yaml")))
	if !ok || v != "32" {
		t.Fatalf("default render -session-stream-cap = %q (present=%v), want 32", v, ok)
	}
}

func TestSessionStreamCapOverrideRenders(t *testing.T) {
	v, ok := streamCapArg(t, renderArgs(t, "-f", filepath.Join("tinycdi", "ci", "minimal-values.yaml"),
		"--set", "backend.sessionStreamCap=64"))
	if !ok || v != "64" {
		t.Fatalf("override -session-stream-cap = %q (present=%v), want 64", v, ok)
	}
}

func TestSessionStreamCapBelowOneRejected(t *testing.T) {
	for _, bad := range []string{"0", "-1"} {
		out, err := helmTemplate(t, "-f", filepath.Join("tinycdi", "ci", "minimal-values.yaml"),
			"--set", "backend.sessionStreamCap="+bad)
		if err == nil {
			t.Fatalf("sessionStreamCap=%s rendered — the bound must not be removable (%s)", bad, out)
		}
	}
}
