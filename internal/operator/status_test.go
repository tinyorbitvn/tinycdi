// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

// SEC-I6: condition messages on the Workspace CR are public API surface —
// they flow to tenant-facing status views — so a raw internal error
// string (SQL detail, broker URL, secret name) must never be stamped
// into status.conditions[].message. The machine-readable Reason carries
// the cause; detail stays in operator logs.
package operator

import (
	"errors"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	workspacesv1alpha1 "github.com/tinyorbitvn/tinycdi/api/v1alpha1"
)

func TestMarkFinalizerStepBlockedSanitizesError(t *testing.T) {
	ws := &workspacesv1alpha1.Workspace{}
	err := errors.New("dial tcp 10.0.0.5:5432: pq: password authentication failed for user \"tcdi\"")
	MarkFinalizerStepBlocked(ws, StepRevokeLeases, err, time.Now())

	var cond *metav1.Condition
	for i := range ws.Status.Conditions {
		if ws.Status.Conditions[i].Type == workspacesv1alpha1.ConditionDegraded {
			cond = &ws.Status.Conditions[i]
		}
	}
	if cond == nil {
		t.Fatalf("Degraded condition not set")
	}
	if cond.Reason != ReasonCleanupRetry {
		t.Fatalf("reason = %q, want %s", cond.Reason, ReasonCleanupRetry)
	}
	if strings.Contains(cond.Message, "10.0.0.5") || strings.Contains(cond.Message, "pq:") ||
		strings.Contains(cond.Message, "password") {
		t.Fatalf("raw error text leaked into public condition message: %q", cond.Message)
	}
	if !strings.Contains(cond.Message, string(StepRevokeLeases)) {
		t.Fatalf("message %q should name the blocked step", cond.Message)
	}
}

// A nil error still marks the step blocked (reason-only, no message body).
func TestMarkFinalizerStepBlockedNilError(t *testing.T) {
	ws := &workspacesv1alpha1.Workspace{}
	MarkFinalizerStepBlocked(ws, StepRetention, nil, time.Now())
	var cond *metav1.Condition
	for i := range ws.Status.Conditions {
		if ws.Status.Conditions[i].Type == workspacesv1alpha1.ConditionDegraded {
			cond = &ws.Status.Conditions[i]
		}
	}
	if cond == nil || cond.Reason != ReasonRetentionPending {
		t.Fatalf("cond = %+v, want reason %s", cond, ReasonRetentionPending)
	}
}
