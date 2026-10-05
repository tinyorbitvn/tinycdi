// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

// envtest coverage for the intent-fence drift surface (INTENT-DRIFT):
// a stale-intent drop that evidences drift — the applier's
// intent-behind marker, or a spec trailing the applied record — surfaces
// as the IntentBehind condition with crRevision/rowRevision params and
// exactly one Warning event; the drift is still never applied, and
// realigning the stream clears the condition. Normal reconciles never
// set it.
//
// Run: KUBEBUILDER_ASSETS=<repo>/bin/k8s/1.37.0-linux-amd64 \
//      go test ./internal/operator/ -run TestIntentBehind -v

package operator

import (
	"context"
	"encoding/json"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"

	workspacesv1alpha1 "github.com/tinyorbitvn/tinycdi/api/v1alpha1"
	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
)

// drainEvents collects every event the recorder emitted so far.
func drainEvents(rec *record.FakeRecorder) []string {
	var out []string
	for {
		select {
		case ev := <-rec.Events:
			out = append(out, ev)
		default:
			return out
		}
	}
}

func intentBehindCond(t *testing.T, ws *workspacesv1alpha1.Workspace) *metav1.Condition {
	t.Helper()
	return condition(ws, workspacesv1alpha1.ConditionIntentBehind)
}

func TestIntentBehindDrift(t *testing.T) {
	env, c := startEnv(t)
	defer func() {
		if err := env.Stop(); err != nil {
			t.Logf("envtest stop: %v", err)
		}
	}()

	t.Run("marker stamps condition+event once, realign clears", func(t *testing.T) {
		ns := newNamespace(t, c)
		newTemplate(t, c, ns, "tpl-drift", nil)
		ws := newWorkspace(t, c, ns, "ws-drift", "tpl-drift", func(ws *workspacesv1alpha1.Workspace) {
			ws.Spec.DesiredState = workspacesv1alpha1.DesiredStateStopped
			ws.Spec.RuntimeGeneration = 0
		})
		key := types.NamespacedName{Name: ws.Name, Namespace: ns}
		rec := record.NewFakeRecorder(16)
		r := newReconciler(c)
		r.Recorder = rec

		// Normal flow: the fence is aligned, so no condition and no event.
		reconcile(t, r, key)
		reconcile(t, r, key)
		wsNow := getWorkspace(t, c, key)
		if cond := intentBehindCond(t, wsNow); cond != nil {
			t.Fatalf("normal flow set IntentBehind: %+v", cond)
		}
		if evs := drainEvents(rec); len(evs) != 0 {
			t.Fatalf("normal flow emitted events: %v", evs)
		}

		// The applier dropped an intent whose revision trailed the CR's
		// spec.intentRevision (=5 after the bump below): it stamps the
		// marker on the CR.
		wsNow.Spec.IntentRevision = 5
		if err := c.Update(context.Background(), wsNow); err != nil {
			t.Fatalf("bump spec revision: %v", err)
		}
		reconcile(t, r, key) // adopts rev 5
		wsNow = getWorkspace(t, c, key)
		wsNow.Annotations[provisioning.AnnotationIntentBehind] =
			`{"rowRevision":3,"crRevision":5}`
		if err := c.Update(context.Background(), wsNow); err != nil {
			t.Fatalf("stamp marker: %v", err)
		}
		reconcile(t, r, key)

		wsNow = getWorkspace(t, c, key)
		cond := intentBehindCond(t, wsNow)
		if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != ReasonIntentBehind {
			t.Fatalf("IntentBehind not set on drift: %+v", cond)
		}
		var params map[string]map[string]string
		raw := wsNow.Annotations[provisioning.AnnotationConditionParams]
		if raw == "" {
			t.Fatal("condition-params annotation missing")
		}
		if err := json.Unmarshal([]byte(raw), &params); err != nil {
			t.Fatalf("condition-params not JSON: %v", err)
		}
		got := params["IntentBehind.IntentBehind"]
		if got["crRevision"] != "5" || got["rowRevision"] != "3" {
			t.Fatalf("params = %v, want crRevision=5 rowRevision=3", got)
		}
		evs := drainEvents(rec)
		if len(evs) != 1 {
			t.Fatalf("events = %v, want exactly one", evs)
		}

		// Steady drift: more reconciles refresh nothing, emit nothing.
		for i := 0; i < 3; i++ {
			reconcile(t, r, key)
		}
		if evs := drainEvents(rec); len(evs) != 0 {
			t.Fatalf("re-drift emitted more events: %v", evs)
		}
		if cond := intentBehindCond(t, getWorkspace(t, c, key)); cond.Status != metav1.ConditionTrue {
			t.Fatalf("IntentBehind lost during drift: %+v", cond)
		}

		// The stream caught up: the applier's forward intent arrives at
		// rev 6 (the marker may still be attached — adoption clears it).
		wsNow = getWorkspace(t, c, key)
		wsNow.Spec.IntentRevision = 6
		if err := c.Update(context.Background(), wsNow); err != nil {
			t.Fatalf("forward intent: %v", err)
		}
		reconcile(t, r, key)
		wsNow = getWorkspace(t, c, key)
		if wsNow.Annotations[provisioning.AnnotationIntentBehind] != "" {
			t.Fatal("drift marker survived the adopted intent")
		}
		cond = intentBehindCond(t, wsNow)
		if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != ReasonNominal {
			t.Fatalf("IntentBehind not cleared on realign: %+v", cond)
		}
	})

	t.Run("spec trailing the applied record drifts without a marker", func(t *testing.T) {
		ns := newNamespace(t, c)
		newTemplate(t, c, ns, "tpl-behind", nil)
		ws := newWorkspace(t, c, ns, "ws-behind", "tpl-behind", func(ws *workspacesv1alpha1.Workspace) {
			ws.Spec.DesiredState = workspacesv1alpha1.DesiredStateStopped
			ws.Spec.RuntimeGeneration = 0
		})
		key := types.NamespacedName{Name: ws.Name, Namespace: ns}
		rec := record.NewFakeRecorder(16)
		r := newReconciler(c)
		r.Recorder = rec

		reconcile(t, r, key)

		// As after a backup/restore that kept the annotation ahead of the
		// spec: applied revision 5 while the spec presents revision 1.
		wsNow := getWorkspace(t, c, key)
		wsNow.Annotations[AnnotationAppliedIntent] =
			`{"revision":5,"desiredState":"Stopped","runtimeGeneration":0,"dataPolicy":"Ephemeral","appliedAt":"2026-01-01T00:00:00Z"}`
		if err := c.Update(context.Background(), wsNow); err != nil {
			t.Fatalf("applied annotation: %v", err)
		}
		reconcile(t, r, key)

		wsNow = getWorkspace(t, c, key)
		cond := intentBehindCond(t, wsNow)
		if cond == nil || cond.Status != metav1.ConditionTrue {
			t.Fatalf("spec-behind-applied did not set IntentBehind: %+v", wsNow.Status.Conditions)
		}
		var params map[string]map[string]string
		if err := json.Unmarshal([]byte(wsNow.Annotations[provisioning.AnnotationConditionParams]), &params); err != nil {
			t.Fatalf("condition-params not JSON: %v", err)
		}
		if got := params["IntentBehind.IntentBehind"]; got["crRevision"] != "5" || got["rowRevision"] != "1" {
			t.Fatalf("params = %v, want crRevision=5 rowRevision=1", got)
		}
		if evs := drainEvents(rec); len(evs) != 1 {
			t.Fatalf("events = %v, want exactly one", evs)
		}

		// The stale spec is still fenced: applied revision stays 5 and
		// the phase keeps tracking the applied record, not the spec.
		if wsNow.Status.LastAppliedIntentRevision != 5 {
			t.Fatalf("lastApplied regressed to %d", wsNow.Status.LastAppliedIntentRevision)
		}

		// Realign by presenting the fence's own revision.
		wsNow.Spec.IntentRevision = 5
		if err := c.Update(context.Background(), wsNow); err != nil {
			t.Fatalf("realign spec: %v", err)
		}
		reconcile(t, r, key)
		cond = intentBehindCond(t, getWorkspace(t, c, key))
		if cond == nil || cond.Status != metav1.ConditionFalse {
			t.Fatalf("IntentBehind not cleared after realign: %+v", cond)
		}
	})
}
