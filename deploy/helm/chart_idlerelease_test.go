// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package chart_test

// backend.idleSessionRelease: renders -idle-session-release on the backend
// Deployment at the shipped default and under overrides (including the
// "0" disable), and the schema refuses a value that is not a Go duration.

import (
	"path/filepath"
	"strings"
	"testing"
)

// idleReleaseArg returns the rendered -idle-session-release value on the
// backend Deployment, or false when absent.
func idleReleaseArg(t *testing.T, docs []doc) (string, bool) {
	t.Helper()
	be := deployment(docs, "backend")
	if be == nil {
		t.Fatal("no backend Deployment rendered")
	}
	for _, a := range firstContainerArgs(be) {
		if strings.HasPrefix(a, "-idle-session-release=") {
			return strings.TrimPrefix(a, "-idle-session-release="), true
		}
	}
	return "", false
}

func TestIdleSessionReleaseDefaultRenders(t *testing.T) {
	v, ok := idleReleaseArg(t, renderArgs(t, "-f", filepath.Join("tinycdi", "ci", "minimal-values.yaml")))
	if !ok || v != "60s" {
		t.Fatalf("default render -idle-session-release = %q (present=%v), want 60s", v, ok)
	}
}

func TestIdleSessionReleaseOverrideRenders(t *testing.T) {
	for _, want := range []string{"90s", "0"} {
		v, ok := idleReleaseArg(t, renderArgs(t, "-f", filepath.Join("tinycdi", "ci", "minimal-values.yaml"),
			"--set-string", "backend.idleSessionRelease="+want))
		if !ok || v != want {
			t.Fatalf("override -idle-session-release = %q (present=%v), want %s", v, ok, want)
		}
	}
}

func TestIdleSessionReleaseRejectsNonDuration(t *testing.T) {
	for _, bad := range []string{"60", "soon", "-5s"} {
		out, err := helmTemplate(t, "-f", filepath.Join("tinycdi", "ci", "minimal-values.yaml"),
			"--set-string", "backend.idleSessionRelease="+bad)
		if err == nil {
			t.Fatalf("idleSessionRelease=%q rendered — the schema must require a Go duration (%s)", bad, out)
		}
	}
}
