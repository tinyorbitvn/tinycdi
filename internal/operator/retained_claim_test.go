// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package operator

// FX-R20: a Workspace that consumes retained data never gets a default home.
//
//   - TestRetainedClaimMissingRefused: retained-data-ref without a claim
//     reference surfaces Degraded=RetainedClaimMissing and creates no
//     runtime children (not a silent new disk).
//   - TestAttachedWorkspaceRecoversByStopStart: a workspace already in the
//     broken state (pod built on a default claim before the retained
//     annotations arrived) mounts the retained volume after a stop/start,
//     and neither volume is deleted or rebound by the operator.

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	workspacesv1alpha1 "github.com/tinyorbitvn/tinycdi/api/v1alpha1"
	"github.com/tinyorbitvn/tinycdi/internal/runtime/linux"
)

func homeClaim(t *testing.T, pod *corev1.Pod) string {
	t.Helper()
	var claims []string
	for _, v := range pod.Spec.Volumes {
		if v.PersistentVolumeClaim != nil {
			claims = append(claims, v.PersistentVolumeClaim.ClaimName)
		}
	}
	if len(claims) != 1 {
		t.Fatalf("pod mounts claims %v, want exactly one", claims)
	}
	return claims[0]
}

func TestRetainedClaimMissingRefused(t *testing.T) {
	env, c := startEnv(t)
	defer func() { _ = env.Stop() }()

	ns := newNamespace(t, c)
	newTemplate(t, c, ns, "tpl-rcm", nil)
	ws := newWorkspace(t, c, ns, "ws-rcm", "tpl-rcm", func(w *workspacesv1alpha1.Workspace) {
		w.Spec.DataPolicy = workspacesv1alpha1.DataPolicyRetain
		w.Annotations = map[string]string{linux.AnnotationRetainedDataRef: "rd_missingclaim"}
	})
	key := types.NamespacedName{Name: ws.Name, Namespace: ns}
	r := newReconciler(c)

	reconcile(t, r, key)
	reconcile(t, r, key)

	got := getWorkspace(t, c, key)
	cond := condition(got, workspacesv1alpha1.ConditionDegraded)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != ReasonRetainedClaimMissing {
		t.Fatalf("want Degraded=True reason=%q, got %+v", ReasonRetainedClaimMissing, cond)
	}
	if pods, svcs, secrets, netpols := countChildren(t, c, ns, ws.UID); pods+svcs+secrets+netpols != 0 {
		t.Fatalf("refused workspace has children: pods=%d svcs=%d secrets=%d netpols=%d", pods, svcs, secrets, netpols)
	}
	pvcs := &corev1.PersistentVolumeClaimList{}
	if err := c.List(context.Background(), pvcs, client.InNamespace(ns)); err != nil {
		t.Fatal(err)
	}
	if len(pvcs.Items) != 0 {
		t.Fatalf("a default home claim was created: %d PVCs", len(pvcs.Items))
	}
}

func TestAttachedWorkspaceRecoversByStopStart(t *testing.T) {
	env, c := startEnv(t)
	defer func() { _ = env.Stop() }()
	ctx := context.Background()

	ns := newNamespace(t, c)
	newTemplate(t, c, ns, "tpl-rec", nil)
	ws := newWorkspace(t, c, ns, "ws-rec", "tpl-rec", func(w *workspacesv1alpha1.Workspace) {
		w.Spec.DataPolicy = workspacesv1alpha1.DataPolicyRetain
	})
	key := types.NamespacedName{Name: ws.Name, Namespace: ns}
	r := newReconciler(c)

	// The broken state: the operator reconciled before the retained
	// annotations existed, so it built the default home and its pod.
	reconcile(t, r, key)
	reconcile(t, r, key)
	stray := linux.PVCName(ws.UID)
	if got := homeClaim(t, thePod(t, c, ns, ws.UID)); got != stray {
		t.Fatalf("setup: pod claim %q, want the default %q", got, stray)
	}
	strayPVC := &corev1.PersistentVolumeClaim{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: stray}, strayPVC); err != nil {
		t.Fatalf("setup: stray pvc: %v", err)
	}

	// The retained volume, relabelled to this workspace by the attach, and
	// the annotations that arrived late.
	retained := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ws-old-home", Namespace: ns,
			Labels: map[string]string{
				linux.LabelWorkspaceUID: string(ws.UID),
				linux.LabelDataRetained: "true",
				linux.LabelDataRole:     "Home",
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
			},
		},
	}
	if err := c.Create(ctx, retained); err != nil {
		t.Fatal(err)
	}
	cur := getWorkspace(t, c, key)
	cur.Annotations[linux.AnnotationRetainedPVC] = retained.Name
	cur.Annotations[linux.AnnotationRetainedPVCUID] = string(retained.UID)
	cur.Annotations[linux.AnnotationRetainedDataRef] = "rd_recover"
	if err := c.Update(ctx, cur); err != nil {
		t.Fatal(err)
	}

	// Stop, then start (a new runtime generation): the pod is rebuilt from
	// the CR as it is now.
	cur = getWorkspace(t, c, key)
	cur.Spec.DesiredState = workspacesv1alpha1.DesiredStateStopped
	cur.Spec.IntentRevision = 2
	if err := c.Update(ctx, cur); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		reconcile(t, r, key)
	}
	cur = getWorkspace(t, c, key)
	cur.Spec.DesiredState = workspacesv1alpha1.DesiredStateRunning
	cur.Spec.RuntimeGeneration = 2
	cur.Spec.IntentRevision = 3
	if err := c.Update(ctx, cur); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		reconcile(t, r, key)
	}

	pod := thePod(t, c, ns, ws.UID)
	if got := homeClaim(t, pod); got != retained.Name {
		t.Fatalf("after stop/start the pod mounts %q, want the retained %q", got, retained.Name)
	}

	// Data safety: both volumes still exist with their original identity,
	// neither deleting, and the retained one was not rebound.
	for _, want := range []*corev1.PersistentVolumeClaim{strayPVC, retained} {
		got := &corev1.PersistentVolumeClaim{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(want), got); err != nil {
			t.Fatalf("pvc %s: %v", want.Name, err)
		}
		if got.UID != want.UID || !got.DeletionTimestamp.IsZero() {
			t.Fatalf("pvc %s changed: uid %s->%s deleting=%v", want.Name, want.UID, got.UID, !got.DeletionTimestamp.IsZero())
		}
	}
}
