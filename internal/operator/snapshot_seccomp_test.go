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
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	workspacesv1alpha1 "github.com/tinyorbitvn/tinycdi/api/v1alpha1"
	linux "github.com/tinyorbitvn/tinycdi/internal/runtime/linux"
)

// seccompReconcile runs one reconcile over the seeded objects and returns
// the stored workspace.
func seccompReconcile(t *testing.T, objs ...client.Object) (client.Client, *workspacesv1alpha1.Workspace) {
	t.Helper()
	s := snapScheme(t)
	var ws *workspacesv1alpha1.Workspace
	b := fake.NewClientBuilder().WithScheme(s)
	for _, o := range objs {
		b = b.WithObjects(o)
		if w, ok := o.(*workspacesv1alpha1.Workspace); ok {
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
	got := &workspacesv1alpha1.Workspace{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(ws), got); err != nil {
		t.Fatal(err)
	}
	return c, got
}

// assertHeldNoPod: the workspace holds Degraded with the expected reason
// and no pod was ever built.
func assertHeldNoPod(t *testing.T, c client.Client, ws, got *workspacesv1alpha1.Workspace, reason string) {
	t.Helper()
	pod := &corev1.Pod{}
	if err := c.Get(context.Background(), client.ObjectKey{
		Namespace: ws.Namespace, Name: linux.PodName(ws.UID)}, pod); err == nil {
		t.Fatalf("held workspace produced a pod: %s", pod.Name)
	}
	cond := condition(got, workspacesv1alpha1.ConditionDegraded)
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
// — the shape an operator could have written before the gate existed —
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
