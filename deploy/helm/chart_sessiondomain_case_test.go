// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package chart_test

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestSessionDomainLowerCaseOnly: only lower case round-trips through DNS and
// the backend's -session-domain parser rejects upper case, so the chart schema
// must refuse it at install time instead of letting the backend crash-loop.
func TestSessionDomainLowerCaseOnly(t *testing.T) {
	out := renderErrArgs(t,
		"-f", filepath.Join("tinycdi", "ci", "minimal-values.yaml"),
		"--set", "sessionDomain=Session.Example.com")
	if !strings.Contains(out, "sessionDomain") {
		t.Errorf("upper-case sessionDomain must be rejected by the schema naming sessionDomain, got: %s", out)
	}
}
