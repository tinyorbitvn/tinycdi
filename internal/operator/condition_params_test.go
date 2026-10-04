// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

// B3-PARAMS: the condition-params annotation carries the structured values
// a condition message interpolates (teardown step, drain budget) so the
// API can project them onto WorkspaceCondition.params. These tests pin the
// write contract: the entry is keyed "<type>.<reason>", a param-less write
// clears the type's entry, and the annotation is removed when it empties.
package operator

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	workspacesv1alpha1 "github.com/tinyorbitvn/tinycdi/api/v1alpha1"
	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
)

func conditionParamsOf(t *testing.T, ws *workspacesv1alpha1.Workspace) map[string]map[string]string {
	t.Helper()
	raw := ws.Annotations[AnnotationConditionParams]
	if raw == "" {
		return nil
	}
	var m map[string]map[string]string
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("condition-params annotation is not JSON: %v", err)
	}
	return m
}

// A param-bearing write lands the values the message interpolated under
// "<type>.<reason>"; a param-less rewrite of the same type clears them,
// and the annotation itself disappears once empty.
func TestSetWorkspaceConditionParams_Lifecycle(t *testing.T) {
	ws := &workspacesv1alpha1.Workspace{}
	now := time.Now()

	SetWorkspaceConditionParams(ws, workspacesv1alpha1.ConditionDegraded,
		metav1.ConditionTrue, ReasonCleanupRetry, "teardown step cleanup blocked; retrying",
		map[string]string{"step": string(StepCleanup)}, now)

	got := conditionParamsOf(t, ws)
	if got["Degraded.CleanupRetry"]["step"] != "cleanup" {
		t.Fatalf("params = %v, want Degraded.CleanupRetry.step=cleanup", got)
	}

	// A different type's entry survives a rewrite of Degraded.
	SetWorkspaceConditionParams(ws, workspacesv1alpha1.ConditionRuntimeReady,
		metav1.ConditionFalse, "DrainingStreams", "teardown step drain-streams in progress",
		map[string]string{"step": string(StepDrainStreams)}, now)
	SetWorkspaceCondition(ws, workspacesv1alpha1.ConditionDegraded,
		metav1.ConditionFalse, ReasonNominal, "", now)
	got = conditionParamsOf(t, ws)
	if _, ok := got["Degraded.CleanupRetry"]; ok {
		t.Fatalf("param-less Degraded write kept stale params: %v", got)
	}
	if got["RuntimeReady.DrainingStreams"]["step"] != "drain-streams" {
		t.Fatalf("RuntimeReady entry lost with Degraded clear: %v", got)
	}

	// Clearing the last entry removes the annotation entirely.
	SetWorkspaceCondition(ws, workspacesv1alpha1.ConditionRuntimeReady,
		metav1.ConditionTrue, ReasonReady, "", now)
	if raw := ws.Annotations[AnnotationConditionParams]; raw != "" {
		t.Fatalf("empty params left annotation behind: %q", raw)
	}
}

// The blocked-step marker params the step its message names.
func TestMarkFinalizerStepBlocked_Params(t *testing.T) {
	ws := &workspacesv1alpha1.Workspace{}
	MarkFinalizerStepBlocked(ws, StepRevokeLeases, nil, time.Now())

	cond := findCondition(ws, workspacesv1alpha1.ConditionDegraded)
	if cond == nil || cond.Reason != ReasonCleanupRetry {
		t.Fatalf("cond = %+v, want Degraded/CleanupRetry", cond)
	}
	got := conditionParamsOf(t, ws)
	if got["Degraded.CleanupRetry"]["step"] != string(StepRevokeLeases) {
		t.Fatalf("params = %v, want step=%s", got, StepRevokeLeases)
	}
}

// The step mark on RuntimeReady records the same step token the message
// embeds, and a reconcile-style param-less write clears it.
func TestMarkStep_Params(t *testing.T) {
	s := snapScheme(t)
	ws := &workspacesv1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-a", Name: "ws-a"},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(ws).WithStatusSubresource(ws).Build()
	f := &Finalizer{Client: c}

	f.markStep(context.Background(), ws, StepDrainStreams)

	cond := findCondition(ws, workspacesv1alpha1.ConditionRuntimeReady)
	if cond == nil || cond.Reason != "DrainingStreams" {
		t.Fatalf("cond = %+v, want RuntimeReady/DrainingStreams", cond)
	}
	got := conditionParamsOf(t, ws)
	if got["RuntimeReady.DrainingStreams"]["step"] != string(StepDrainStreams) {
		t.Fatalf("params = %v, want step=%s", got, StepDrainStreams)
	}

	SetWorkspaceCondition(ws, workspacesv1alpha1.ConditionRuntimeReady,
		metav1.ConditionFalse, ReasonStopped, "runtime stopped by intent", time.Now())
	if m := conditionParamsOf(t, ws); m != nil {
		t.Fatalf("param-less rewrite kept params: %v", m)
	}
}

// The persisted drain conditions carry the step and the configured drain
// budget — the values the English messages describe — under keys the
// API's "<type>.<reason>" lookup matches.
func TestRunDrain_Params(t *testing.T) {
	s := snapScheme(t)
	ws := &workspacesv1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "tenant-a", Name: "ws-a",
			Labels: map[string]string{provisioning.LabelWorkspaceUID: "ws_a"},
		},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(ws).WithStatusSubresource(ws).Build()

	// Inside the drain window: StreamDraining reports step + budget.
	f := &Finalizer{Client: c, Drainer: pendingDrainer{}, DrainBudget: 45 * time.Second}
	prog := &FinalizerProgress{}
	drained, err := f.runDrain(context.Background(), ws, prog, "ws_a", time.Now())
	if err != nil || drained {
		t.Fatalf("runDrain = (%v, %v), want inside-window wait", drained, err)
	}
	got := conditionParamsOf(t, ws)
	p := got["Degraded.StreamDraining"]
	if p["step"] != string(StepDrainStreams) || p["budgetSeconds"] != "45" {
		t.Fatalf("StreamDraining params = %v, want step+budgetSeconds=45", p)
	}

	// Past the budget: DrainTimedOut reports the same values.
	prog.DrainStartedAt = ptrTime(time.Now().Add(-time.Hour))
	if err := f.persistProgress(context.Background(), ws, prog); err != nil {
		t.Fatal(err)
	}
	drained, err = f.runDrain(context.Background(), ws, prog, "ws_a", time.Now())
	if err != nil || !drained {
		t.Fatalf("runDrain = (%v, %v), want timed-out drain done", drained, err)
	}
	got = conditionParamsOf(t, ws)
	p = got["Degraded.DrainTimedOut"]
	if p["step"] != string(StepDrainStreams) || p["budgetSeconds"] != "45" {
		t.Fatalf("DrainTimedOut params = %v, want step+budgetSeconds=45", p)
	}
	if _, ok := got["Degraded.StreamDraining"]; ok {
		t.Fatalf("timed-out write kept the waiting entry: %v", got)
	}
}

// pendingDrainer always reports open streams (drain never done).
type pendingDrainer struct{}

func (pendingDrainer) DrainStatus(context.Context, provisioning.PlatformID) (int, bool, error) {
	return 3, false, nil
}

func findCondition(ws *workspacesv1alpha1.Workspace, typ string) *metav1.Condition {
	for i := range ws.Status.Conditions {
		if ws.Status.Conditions[i].Type == typ {
			return &ws.Status.Conditions[i]
		}
	}
	return nil
}

func ptrTime(t time.Time) *time.Time { return &t }
