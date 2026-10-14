// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

// v1.0 (SR-3 blast-radius follow-up): the
// workspaces.cdi.tinyorbit.vn/seccomp-profile annotation is validated like
// apparmor-profile — only "localhost/<clean relative path>" or
// "runtime/default" are admissible. The check runs at template admission
// (a live template carrying an out-of-policy value is never recorded into
// status.templateSnapshot — the workspace holds Degraded/TemplateInvalid
// and builds no pod) AND at recorded-snapshot verification (a snapshot
// carrying an out-of-policy value is never trusted — it is re-taken from
// the live template, whose admission check then applies). Before this the
// localhost name was prefix-checked only and reached the pod spec
// untouched.

package operator

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	cdiv1 "github.com/tinyorbitvn/tinycdi/api/v1"
	linux "github.com/tinyorbitvn/tinycdi/internal/runtime/linux"
)

// seccompReconcile runs one reconcile over the seeded objects and returns
// the stored workspace.
func seccompReconcile(t *testing.T, objs ...client.Object) (client.Client, *cdiv1.Workspace) {
	t.Helper()
	s := snapScheme(t)
	var ws *cdiv1.Workspace
	b := fake.NewClientBuilder().WithScheme(s)
	for _, o := range objs {
		b = b.WithObjects(o)
		if w, ok := o.(*cdiv1.Workspace); ok {
			b = b.WithStatusSubresource(w)
			ws = w
		}
	}
	c := b.Build()
	r := &WorkspaceReconciler{Client: c, Scheme: s,
		Backend: linux.New(c, linux.Options{})}
	if _, err := r.Reconcile(context.Background(),
		ctrl.Request{NamespacedName: client.ObjectKeyFromObject(ws)}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := &cdiv1.Workspace{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(ws), got); err != nil {
		t.Fatal(err)
	}
	return c, got
}

// assertHeldNoPod: the workspace holds Degraded with the expected reason
// and no pod was ever built.
func assertHeldNoPod(t *testing.T, c client.Client, ws, got *cdiv1.Workspace, reason string) {
	t.Helper()
	pod := &corev1.Pod{}
	if err := c.Get(context.Background(), client.ObjectKey{
		Namespace: ws.Namespace, Name: linux.PodName(ws.UID)}, pod); err == nil {
		t.Fatalf("held workspace produced a pod: %s", pod.Name)
	}
	cond := condition(got, cdiv1.ConditionDegraded)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != reason {
		t.Fatalf("want Degraded=True reason=%s, got %+v", reason, got.Status.Conditions)
	}
}

// A live template carrying an out-of-policy seccomp annotation is refused
// at snapshot admission: the workspace holds Degraded/TemplateInvalid and
// no pod exists — the same fail-closed posture a bad apparmor-profile
// value already had.
func TestTemplateInvalidBadSeccompAnnotation(t *testing.T) {
	tpl := adoptTemplate("tinycdi-tenant-a", "fam32-aaaa1111", types.UID("uid-badsec"))
	tpl.Annotations[linux.AnnotationSeccompProfile] = "localhost/../escape"
	ws := adoptWorkspace("fam32-aaaa1111", "", types.UID("ws-bad-sec"))

	c, got := seccompReconcile(t, ws, tpl)
	assertHeldNoPod(t, c, ws, got, ReasonTemplateInvalid)
	if got.Status.TemplateSnapshot != nil {
		t.Fatalf("out-of-policy template admitted into status.templateSnapshot: %+v",
			got.Status.TemplateSnapshot)
	}
}

// A recorded status snapshot carrying an out-of-policy seccomp annotation
// — for example one edited through the status subresource —
// fails recorded-snapshot verification: the record is re-taken from the
// live template, the admission check refuses it for the same reason, and
// the workspace holds Degraded/TemplateInvalid with no pod.
func TestRecordedSnapshotBadSeccompAnnotationResnapshots(t *testing.T) {
	tpl := adoptTemplate("tinycdi-tenant-a", "fam32-aaaa1111", types.UID("uid-badsec"))
	tpl.Annotations[linux.AnnotationSeccompProfile] = "localhost/../escape"
	ws := adoptWorkspace("fam32-aaaa1111", "", types.UID("ws-bad-sec"))

	snap, err := snapshotTemplate(tpl)
	if err != nil {
		t.Fatalf("snapshotTemplate: %v", err)
	}
	snap.SourceRef = ws.Spec.TemplateRef.Name
	snap.RuntimeGeneration = ws.Spec.RuntimeGeneration
	ws.Status.TemplateSnapshot = snap

	c, got := seccompReconcile(t, ws, tpl)
	assertHeldNoPod(t, c, ws, got, ReasonTemplateInvalid)
	// The recorded record stays (it is operator-written) but is never
	// trusted: every reconcile re-fails verification and re-holds instead
	// of converging on it — the distinguishing mark of the verify path,
	// since a missing record would take the fresh-snapshot path and leave
	// status.templateSnapshot nil.
	if got.Status.TemplateSnapshot == nil {
		t.Fatal("recorded snapshot lost instead of held untrusted")
	}
}

// The accepted case stands as a control: a live template with a clean
// localhost profile is admitted and the pod carries the Localhost
// profile — the gate refuses only what was never validated.
func TestTemplateValidLocalhostSeccompAnnotation(t *testing.T) {
	tpl := adoptTemplate("tinycdi-tenant-a", "fam32-aaaa1111", types.UID("uid-goodsec"))
	tpl.Annotations[linux.AnnotationSeccompProfile] = "localhost/profiles/chromium-userns.json"
	ws := adoptWorkspace("fam32-aaaa1111", "", types.UID("ws-good-sec"))

	c, got := seccompReconcile(t, ws, tpl)
	if got.Status.TemplateSnapshot == nil {
		t.Fatal("valid template not admitted into status.templateSnapshot")
	}
	pod := &corev1.Pod{}
	if err := c.Get(context.Background(), client.ObjectKey{
		Namespace: ws.Namespace, Name: linux.PodName(ws.UID)}, pod); err != nil {
		t.Fatalf("pod not built from a valid template: %v", err)
	}
	sc := pod.Spec.Containers[0].SecurityContext.SeccompProfile
	if sc == nil || sc.Type != corev1.SeccompProfileTypeLocalhost ||
		sc.LocalhostProfile == nil || *sc.LocalhostProfile != "profiles/chromium-userns.json" {
		t.Fatalf("container seccompProfile = %+v, want Localhost profiles/chromium-userns.json", sc)
	}
}

// The adoption hold splits by failure class: a candidate template whose
// CONTENT is inadmissible (out-of-policy seccomp annotation) holds
// Degraded/TemplateInvalid, while a pod whose recorded revision cannot
// be proven holds Degraded/TemplateRevisionGone — and in BOTH cases the
// running pod is left untouched and exactly one Warning event fires,
// after the status persist.
func TestAdoptHold_ReasonPerFailureClass(t *testing.T) {
	s := snapScheme(t)

	reconcile := func(c client.Client, ws *cdiv1.Workspace,
		rec *record.FakeRecorder) *cdiv1.Workspace {
		t.Helper()
		r := &WorkspaceReconciler{Client: c, Scheme: s,
			Backend: linux.New(c, linux.Options{}), Recorder: rec}
		for i := 0; i < 3; i++ {
			if _, err := r.Reconcile(context.Background(),
				ctrl.Request{NamespacedName: client.ObjectKeyFromObject(ws)}); err != nil {
				t.Fatalf("reconcile: %v", err)
			}
		}
		got := &cdiv1.Workspace{}
		if err := c.Get(context.Background(), client.ObjectKeyFromObject(ws), got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	assertHeld := func(got *cdiv1.Workspace, reason string) {
		t.Helper()
		cond := condition(got, cdiv1.ConditionDegraded)
		if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != reason {
			t.Fatalf("want Degraded=True reason=%s, got %+v", reason, got.Status.Conditions)
		}
	}
	assertPodUntouched := func(c client.Client, ws *cdiv1.Workspace) {
		t.Helper()
		pod := &corev1.Pod{}
		if err := c.Get(context.Background(), client.ObjectKey{
			Namespace: ws.Namespace, Name: linux.PodName(ws.UID)}, pod); err != nil {
			t.Fatalf("held workspace's running pod was touched: %v", err)
		}
		if !pod.DeletionTimestamp.IsZero() {
			t.Fatal("held workspace's running pod is being deleted")
		}
	}
	assertOneEvent := func(rec *record.FakeRecorder, reason string) {
		t.Helper()
		select {
		case ev := <-rec.Events:
			if !strings.Contains(ev, reason) {
				t.Fatalf("event %q, want reason %s", ev, reason)
			}
		default:
			t.Fatalf("no %s Warning emitted", reason)
		}
		select {
		case ev := <-rec.Events:
			t.Fatalf("second event emitted on a repeated hold: %q", ev)
		default:
		}
	}

	t.Run("content-invalid holds TemplateInvalid, pod untouched", func(t *testing.T) {
		tpl := adoptTemplate("tinycdi-tenant-a", "fam32-aaaa1111", types.UID("uid-inv"))
		ws := adoptWorkspace("fam32", "", types.UID("ws-adopt-invalid"))
		c := fake.NewClientBuilder().WithScheme(s).
			WithObjects(ws, tpl).WithStatusSubresource(ws).Build()
		seedBuiltPod(t, c, ws, tpl) // stamped fam32-aaaa1111 @ 2026-10-a
		// The live object becomes inadmissible after the pod was built.
		tpl.Annotations[linux.AnnotationSeccompProfile] = "localhost/../escape"
		if err := c.Update(context.Background(), tpl); err != nil {
			t.Fatal(err)
		}
		rec := record.NewFakeRecorder(16)
		got := reconcile(c, ws, rec)
		assertHeld(got, ReasonTemplateInvalid)
		assertPodUntouched(c, ws)
		if got.Status.TemplateSnapshot != nil {
			t.Fatalf("inadmissible template adopted: %+v", got.Status.TemplateSnapshot)
		}
		assertOneEvent(rec, ReasonTemplateInvalid)
	})

	t.Run("identity-unprovable holds TemplateRevisionGone, pod untouched", func(t *testing.T) {
		tpl := adoptTemplate("tinycdi-tenant-a", "fam32-aaaa1111", types.UID("uid-gone"))
		ws := adoptWorkspace("fam32", "", types.UID("ws-adopt-gone"))
		c := fake.NewClientBuilder().WithScheme(s).
			WithObjects(ws).WithStatusSubresource(ws).Build()
		seedBuiltPod(t, c, ws, tpl) // stamped; the object is absent
		rec := record.NewFakeRecorder(16)
		got := reconcile(c, ws, rec)
		assertHeld(got, ReasonTemplateRevisionGone)
		assertPodUntouched(c, ws)
		assertOneEvent(rec, ReasonTemplateRevisionGone)
	})
}
