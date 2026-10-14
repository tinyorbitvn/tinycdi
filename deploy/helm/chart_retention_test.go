// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package chart_test

// Terminal-row retention (SR-4-F5): backend.retention.terminalRowDays
// renders -terminal-retention-days on the backend Deployment — default
// 30, explicit values pass through, 0 disables, and the schema rejects
// negatives and unknown keys under backend.retention.

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestRetentionDefaults(t *testing.T) {
	args := backendArgsWith(t)
	if !hasArg(args, "-terminal-retention-days=30") {
		t.Errorf("default render lacks -terminal-retention-days=30: %v", args)
	}
}

func TestRetentionCustomAndDisabled(t *testing.T) {
	args := backendArgsWith(t, "backend.retention.terminalRowDays=7")
	if !hasArg(args, "-terminal-retention-days=7") {
		t.Errorf("backend.retention.terminalRowDays=7 not rendered: %v", args)
	}
	args = backendArgsWith(t, "backend.retention.terminalRowDays=0")
	if !hasArg(args, "-terminal-retention-days=0") {
		t.Errorf("backend.retention.terminalRowDays=0 (disabled) not rendered: %v", args)
	}
}

func TestRetentionSchemaRejectsBadValues(t *testing.T) {
	out := renderErrArgs(t, "-f", filepath.Join("tinycdi", "ci", "minimal-values.yaml"),
		"--set", "backend.retention.terminalRowDays=-1")
	if !strings.Contains(out, "terminalRowDays") && !strings.Contains(out, "minimum") {
		t.Errorf("a negative terminalRowDays must be rejected by the schema, got: %s", out)
	}
	out = renderErrArgs(t, "-f", filepath.Join("tinycdi", "ci", "minimal-values.yaml"),
		"--set", "backend.retention.bogus=1")
	if !strings.Contains(out, "retention") && !strings.Contains(out, "additionalProperties") {
		t.Errorf("an unknown key under backend.retention must be rejected by the schema, got: %s", out)
	}
}

func TestAuditRetentionDefaultAndCustom(t *testing.T) {
	if args := backendArgsWith(t); !hasArg(args, "-audit-retention-days=90") {
		t.Errorf("default render lacks -audit-retention-days=90: %v", args)
	}
	if args := backendArgsWith(t, "backend.retention.auditDays=30"); !hasArg(args, "-audit-retention-days=30") {
		t.Errorf("backend.retention.auditDays=30 not rendered: %v", args)
	}
	out := renderErrArgs(t, "-f", filepath.Join("tinycdi", "ci", "minimal-values.yaml"),
		"--set", "backend.retention.auditDays=0")
	if !strings.Contains(out, "auditDays") && !strings.Contains(out, "minimum") {
		t.Errorf("auditDays=0 must be rejected by the schema, got: %s", out)
	}
}
