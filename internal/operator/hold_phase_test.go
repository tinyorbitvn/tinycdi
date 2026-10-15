// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

// A template hold follows the incarnation, not the hold itself: a
// workspace whose operator-owned pod is still serving holds phase Ready —
// the Degraded condition carries the hold — so the broker's running
// projection keeps it under lifecycle enforcement (the stricter held
// caps) and ticket issuance for reconnect keeps working. An owned pod
// that is not serving, a foreign pod, and no pod at all are Pending.
// Before the fix every hold wrote Pending, which dropped still-running
// held workspaces out of the broker's running set entirely — no
// idle/disconnect/max-duration enforcement, no reconnect.
package operator

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	cdiv1 "github.com/tinyorbitvn/tinycdi/api/v1"
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

// servingPod marks the incarnation what a live desktop behind a hold
// looks like: Running phase + PodReady, plus the apiserver-assigned UID.
func servingPod(t *testing.T, c client.Client, pod *corev1.Pod, uid types.UID) {
	t.Helper()
	pod.UID = uid
	if err := c.Update(context.Background(), pod); err != nil {
		t.Fatalf("stamp pod uid: %v", err)
	}
	pod.Status.Phase = corev1.PodRunning
	pod.Status.Conditions = []corev1.PodCondition{
		{Type: corev1.PodReady, Status: corev1.ConditionTrue, Reason: "Ready"},
	}
	if err := c.Status().Update(context.Background(), pod); err != nil {
		t.Fatalf("mark pod serving: %v", err)
	}
	publishReadyEndpoint(t, c, pod)
}

// assertHeldReady asserts the with-pod hold shape: phase Ready — the
// serving incarnation — with Degraded carrying the hold reason and every
// status field the broker's serving paths read still populated.
func assertHeldReady(t *testing.T, c client.Client, got *cdiv1.Workspace, pod *corev1.Pod, reason string) {
	t.Helper()
	if got.Status.Phase != cdiv1.WorkspacePhaseReady {
		t.Fatalf("held workspace with a serving pod: phase=%q, want Ready", got.Status.Phase)
	}
	cond := condition(got, cdiv1.ConditionDegraded)
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
// serving, and keep it across reconciles.
func TestHoldWithPodStaysReady(t *testing.T) {
	s := snapScheme(t)
	tpl := adoptTemplate("tinycdi-tenant-a", "fam32-aaaa1111", types.UID("uid-a"))

	t.Run("revision gone", func(t *testing.T) {
		ws := adoptWorkspace("fam32", "", types.UID("ws-hold-1"))
		c := fake.NewClientBuilder().WithScheme(s).
			WithObjects(ws).WithStatusSubresource(ws).Build()
		// Stamped pod names a revision object that has since been pruned.
		pod := seedBuiltPod(t, c, ws, tpl)
		servingPod(t, c, pod, "pod-uid-1")

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
		servingPod(t, c, pod, "pod-uid-2")

		got := adoptReconcile(t, s, c, ws)
		assertHeldReady(t, c, got, pod, ReasonTemplateInvalid)
	})

	t.Run("ready pod keeps its ready surface", func(t *testing.T) {
		ws := adoptWorkspace("fam32", "", types.UID("ws-hold-3"))
		c := fake.NewClientBuilder().WithScheme(s).
			WithObjects(ws).WithStatusSubresource(ws).Build()
		pod := seedBuiltPod(t, c, ws, tpl)
		servingPod(t, c, pod, "pod-uid-3")

		got := adoptReconcile(t, s, c, ws)
		assertHeldReady(t, c, got, pod, ReasonTemplateRevisionGone)
		// The observed step truth survives the hold: a serving pod under a
		// held workspace still reports RuntimeReady/ConnectionReady — the
		// reconnect surface ticket issuance rides on.
		for _, typ := range []string{
			cdiv1.ConditionRuntimeReady,
			cdiv1.ConditionConnectionReady,
		} {
			cc := condition(got, typ)
			if cc == nil || cc.Status != metav1.ConditionTrue {
				t.Fatalf("%s = %+v, want True behind the held-but-serving pod", typ, cc)
			}
		}
	})
}

// A hold without a serving pod is Pending — the owned pod that exists
// but is not Ready (still booting or crash-looping: no tickets may be
// minted against it), the foreign squatter, and the no-pod case.
func TestHoldWithoutServingPodStaysPending(t *testing.T) {
	s := snapScheme(t)
	tpl := adoptTemplate("tinycdi-tenant-a", "fam32-aaaa1111", types.UID("uid-a"))

	t.Run("owned pod not serving", func(t *testing.T) {
		ws := adoptWorkspace("fam32", "", types.UID("ws-hold-6"))
		c := fake.NewClientBuilder().WithScheme(s).
			WithObjects(ws).WithStatusSubresource(ws).Build()
		// The pod is live and owned but never reported Ready.
		pod := seedBuiltPod(t, c, ws, tpl)
		podUID(t, c, pod, "pod-uid-6")

		got := adoptReconcile(t, s, c, ws)
		if got.Status.Phase != cdiv1.WorkspacePhasePending {
			t.Fatalf("held workspace with a not-serving pod: phase=%q, want Pending",
				got.Status.Phase)
		}
		cond := condition(got, cdiv1.ConditionDegraded)
		if cond == nil || cond.Status != metav1.ConditionTrue ||
			cond.Reason != ReasonTemplateRevisionGone {
			t.Fatalf("want Degraded=True reason=%s, got %+v",
				ReasonTemplateRevisionGone, got.Status.Conditions)
		}
		// The observed step truth is still written: the incarnation's
		// identity and endpoint are recorded, the steps report it is not
		// ready yet.
		if got.Status.RuntimeUID != string(pod.UID) {
			t.Fatalf("runtimeUID=%q, want the observed pod %q",
				got.Status.RuntimeUID, pod.UID)
		}
		if got.Status.ServiceRef == nil {
			t.Fatal("serviceRef empty although the pod's Service exists")
		}
		if rr := condition(got, cdiv1.ConditionRuntimeReady); rr == nil ||
			rr.Status != metav1.ConditionFalse {
			t.Fatalf("RuntimeReady = %+v, want False for a not-serving pod", rr)
		}
	})

	t.Run("no pod, template unresolvable", func(t *testing.T) {
		ws := adoptWorkspace("fam32-aaaa1111",
			snapshotAnnotation(t, tpl, "fam32-aaaa1111", 1), types.UID("ws-hold-4"))
		c := fake.NewClientBuilder().WithScheme(s).
			WithObjects(ws).WithStatusSubresource(ws).Build()
		got := adoptReconcile(t, s, c, ws)
		if got.Status.Phase != cdiv1.WorkspacePhasePending {
			t.Fatalf("held workspace with no pod: phase=%q, want Pending", got.Status.Phase)
		}
		cond := condition(got, cdiv1.ConditionDegraded)
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
		if got.Status.Phase != cdiv1.WorkspacePhasePending {
			t.Fatalf("held workspace behind a foreign pod: phase=%q, want Pending", got.Status.Phase)
		}
		cond := condition(got, cdiv1.ConditionDegraded)
		if cond == nil || cond.Status != metav1.ConditionTrue ||
			cond.Reason != ReasonTemplateInvalid {
			t.Fatalf("want Degraded=True reason=%s, got %+v",
				ReasonTemplateInvalid, got.Status.Conditions)
		}
	})
}
