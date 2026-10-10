// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

// A template hold follows the incarnation, not the hold itself: a
// workspace whose operator-owned pod is still running holds phase Ready —
// the Degraded condition carries the hold — so the broker's running
// projection keeps it under lifecycle enforcement (the stricter held
// caps) and ticket issuance for reconnect keeps working. A hold with no
// operator-owned pod is Pending. Before the fix every hold wrote Pending,
// which dropped still-running held workspaces out of the broker's
// running set entirely — no idle/disconnect/max-duration enforcement,
// no reconnect.
package operator

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	workspacesv1alpha1 "github.com/tinyorbitvn/tinycdi/api/v1alpha1"
	linux "github.com/tinyorbitvn/tinycdi/internal/runtime/linux"
)

// podUID stamps the UID a real apiserver assigns at create — the fake
// client leaves it empty, and status.runtimeUID rides on it.
func podUID(t *testing.T, c client.Client, pod *corev1.Pod, uid types.UID) {
	t.Helper()
	pod.UID = uid
	if err := c.Update(context.Background(), pod); err != nil {
		t.Fatalf("stamp pod uid: %v", err)
	}
}

// assertHeldReady asserts the with-pod hold shape: phase Ready — the live
// incarnation — with Degraded carrying the hold reason and every status
// field the broker's serving paths read still populated.
func assertHeldReady(t *testing.T, c client.Client, got *workspacesv1alpha1.Workspace, pod *corev1.Pod, reason string) {
	t.Helper()
	if got.Status.Phase != workspacesv1alpha1.WorkspacePhaseReady {
		t.Fatalf("held workspace with a live pod: phase=%q, want Ready", got.Status.Phase)
	}
	cond := condition(got, workspacesv1alpha1.ConditionDegraded)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != reason {
		t.Fatalf("want Degraded=True reason=%s, got %+v", reason, got.Status.Conditions)
	}
	if got.Status.TemplateSnapshot != nil {
		t.Fatalf("a snapshot was recorded for a held workspace: %+v", got.Status.TemplateSnapshot)
	}
	if got.Status.RuntimeUID != string(pod.UID) {
		t.Fatalf("runtimeUID=%q, want the live pod %q", got.Status.RuntimeUID, pod.UID)
	}
	if got.Status.ObservedRuntimeGeneration != 1 {
		t.Fatalf("observedRuntimeGeneration=%d, want 1", got.Status.ObservedRuntimeGeneration)
	}
	if got.Status.ServiceRef == nil {
		t.Fatal("serviceRef empty on a held workspace — reconnect cannot find the endpoint")
	}
	if got.Status.StartedAt == nil {
		t.Fatal("startedAt empty on a held workspace — the broker cannot project it")
	}
	cur := &corev1.Pod{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(pod), cur); err != nil {
		t.Fatalf("pod vanished on hold: %v", err)
	}
	if cur.UID != pod.UID {
		t.Fatal("hold recreated the pod")
	}
}

// The with-pod holds — identity-unprovable (TemplateRevisionGone) and
// content-invalid (TemplateInvalid) — keep phase Ready while their pod is
// live, and keep it across reconciles.
func TestHoldWithPodStaysReady(t *testing.T) {
	s := snapScheme(t)
	tpl := adoptTemplate("tinycdi-tenant-a", "fam32-aaaa1111", types.UID("uid-a"))

	t.Run("revision gone", func(t *testing.T) {
		ws := adoptWorkspace("fam32", "", types.UID("ws-hold-1"))
		c := fake.NewClientBuilder().WithScheme(s).
			WithObjects(ws).WithStatusSubresource(ws).Build()
		// Stamped pod names a revision object that has since been pruned.
		pod := seedBuiltPod(t, c, ws, tpl)
		podUID(t, c, pod, "pod-uid-1")

		got := adoptReconcile(t, s, c, ws)
		assertHeldReady(t, c, got, pod, ReasonTemplateRevisionGone)
		// The hold is stable: the next reconcile reports the same shape.
		got = adoptReconcile(t, s, c, ws)
		assertHeldReady(t, c, got, pod, ReasonTemplateRevisionGone)
	})

	t.Run("content invalid", func(t *testing.T) {
		live := adoptTemplate("tinycdi-tenant-a", "fam32-aaaa1111", types.UID("uid-a"))
		live.Annotations[linux.AnnotationSeccompProfile] = "unconfined"
		ws := adoptWorkspace("fam32", "", types.UID("ws-hold-2"))
		c := fake.NewClientBuilder().WithScheme(s).
			WithObjects(ws, live).WithStatusSubresource(ws).Build()
		// The pod's revision proves fine; the live template's content
		// fails admission while the pod keeps running.
		pod := seedBuiltPod(t, c, ws, tpl)
		podUID(t, c, pod, "pod-uid-2")

		got := adoptReconcile(t, s, c, ws)
		assertHeldReady(t, c, got, pod, ReasonTemplateInvalid)
	})

	t.Run("ready pod keeps its ready surface", func(t *testing.T) {
		ws := adoptWorkspace("fam32", "", types.UID("ws-hold-3"))
		c := fake.NewClientBuilder().WithScheme(s).
			WithObjects(ws).WithStatusSubresource(ws).Build()
		pod := seedBuiltPod(t, c, ws, tpl)
		podUID(t, c, pod, "pod-uid-3")
		pod.Status.Phase = corev1.PodRunning
		pod.Status.Conditions = []corev1.PodCondition{
			{Type: corev1.PodReady, Status: corev1.ConditionTrue, Reason: "Ready"},
		}
		if err := c.Status().Update(context.Background(), pod); err != nil {
			t.Fatalf("mark pod ready: %v", err)
		}

		got := adoptReconcile(t, s, c, ws)
		assertHeldReady(t, c, got, pod, ReasonTemplateRevisionGone)
		// The observed step truth survives the hold: a ready pod under a
		// held workspace still reports RuntimeReady/ConnectionReady — the
		// reconnect surface ticket issuance rides on.
		for _, typ := range []string{
			workspacesv1alpha1.ConditionRuntimeReady,
			workspacesv1alpha1.ConditionConnectionReady,
		} {
			cc := condition(got, typ)
			if cc == nil || cc.Status != metav1.ConditionTrue {
				t.Fatalf("%s = %+v, want True behind the held-but-running pod", typ, cc)
			}
		}
	})
}

// A hold without an operator-owned pod is Pending — both the
// fresh-template-invalid case and the foreign-pod case, where a pod
// squatting on the workspace-pod name without provenance must not pull
// the workspace into the serving set.
func TestHoldWithoutPodStaysPending(t *testing.T) {
	s := snapScheme(t)
	tpl := adoptTemplate("tinycdi-tenant-a", "fam32-aaaa1111", types.UID("uid-a"))

	t.Run("no pod, template unresolvable", func(t *testing.T) {
		ws := adoptWorkspace("fam32-aaaa1111",
			snapshotAnnotation(t, tpl, "fam32-aaaa1111", 1), types.UID("ws-hold-4"))
		c := fake.NewClientBuilder().WithScheme(s).
			WithObjects(ws).WithStatusSubresource(ws).Build()
		got := adoptReconcile(t, s, c, ws)
		if got.Status.Phase != workspacesv1alpha1.WorkspacePhasePending {
			t.Fatalf("held workspace with no pod: phase=%q, want Pending", got.Status.Phase)
		}
		cond := condition(got, workspacesv1alpha1.ConditionDegraded)
		if cond == nil || cond.Status != metav1.ConditionTrue ||
			cond.Reason != ReasonTemplateInvalid {
			t.Fatalf("want Degraded=True reason=%s, got %+v",
				ReasonTemplateInvalid, got.Status.Conditions)
		}
	})

	t.Run("foreign pod does not ground the hold", func(t *testing.T) {
		ws := adoptWorkspace("fam32", "", types.UID("ws-hold-5"))
		c := fake.NewClientBuilder().WithScheme(s).
			WithObjects(ws).WithStatusSubresource(ws).Build()
		b := linux.New(c, linux.Options{})
		if _, err := b.Ensure(context.Background(), ws, tpl); err != nil {
			t.Fatalf("seed pod: %v", err)
		}
		pod := &corev1.Pod{}
		if err := c.Get(context.Background(), client.ObjectKey{
			Namespace: ws.Namespace, Name: linux.PodName(ws.UID)}, pod); err != nil {
			t.Fatal(err)
		}
		// The squatter carries the managed labels but no provenance.
		pod.OwnerReferences = nil
		if err := c.Update(context.Background(), pod); err != nil {
			t.Fatal(err)
		}
		got := adoptReconcile(t, s, c, ws)
		if got.Status.Phase != workspacesv1alpha1.WorkspacePhasePending {
			t.Fatalf("held workspace behind a foreign pod: phase=%q, want Pending", got.Status.Phase)
		}
		cond := condition(got, workspacesv1alpha1.ConditionDegraded)
		if cond == nil || cond.Status != metav1.ConditionTrue ||
			cond.Reason != ReasonTemplateInvalid {
			t.Fatalf("want Degraded=True reason=%s, got %+v",
				ReasonTemplateInvalid, got.Status.Conditions)
		}
	})
}
