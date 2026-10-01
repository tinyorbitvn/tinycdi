// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

// SEC-10 regression coverage: the
// operator must not trust the workspaces.cdi.tinyorbit.vn/template-snapshot
// annotation blindly. The annotation is ordinary Workspace metadata —
// a principal with Workspace write can pre-seed or replace it — so before
// it drives convergence the snapshot must prove integrity (specHash over
// the recorded spec), re-satisfy the CRD invariants the apiserver
// enforces on real templates (digest-pinned image, runtime-block
// exclusivity), and, when the recorded template object still exists,
// equal it. A forged annotation must produce NO runtime children.
package operator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	workspacesv1alpha1 "github.com/tinyorbitvn/tinycdi/api/v1alpha1"
	linux "github.com/tinyorbitvn/tinycdi/internal/runtime/linux"
)

func snapScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := workspacesv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

// forgedSnapshot builds a snapshot annotation body. specHash may be a
// real hash over spec or an attacker-supplied lie.
func forgedSnapshot(name, uid, revision, specHash string, spec workspacesv1alpha1.WorkspaceTemplateSpec, ann map[string]string) string {
	raw, _ := json.Marshal(map[string]any{
		"name": name, "uid": uid, "revision": revision, "specHash": specHash,
		"spec": spec, "annotations": ann,
	})
	return string(raw)
}

func forgedSpec(image string) workspacesv1alpha1.WorkspaceTemplateSpec {
	return workspacesv1alpha1.WorkspaceTemplateSpec{
		Revision: "x", Runtime: workspacesv1alpha1.RuntimeLinuxContainer,
		Experience: workspacesv1alpha1.ExperienceDesktop,
		Linux: &workspacesv1alpha1.LinuxRuntimeSpec{
			Image:   image,
			Command: []string{"/bin/sh", "-c", "echo arbitrary"},
		},
		Resources: workspacesv1alpha1.ResourceProfile{
			CPU: resource.MustParse("64"), Memory: resource.MustParse("256Gi"),
			Storage: resource.MustParse("1Ti"),
		},
		BootDeadline:   metav1.Duration{Duration: time.Hour},
		NetworkProfile: workspacesv1alpha1.NetworkProfileClusterOnly,
	}
}

func forgedWorkspace(ann string) *workspacesv1alpha1.Workspace {
	return &workspacesv1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ws-forged", Namespace: "tinycdi-tenant-a",
			UID:         types.UID("cccccccc-0000-0000-0000-000000000001"),
			Labels:      map[string]string{"workspaces.cdi.tinyorbit.vn/workspace-uid": "ws_forged"},
			Annotations: map[string]string{AnnotationTemplateSnapshot: ann},
		},
		Spec: workspacesv1alpha1.WorkspaceSpec{
			TemplateRef:       workspacesv1alpha1.TemplateReference{Name: "does-not-exist"},
			OwnerSubject:      workspacesv1alpha1.OwnerSubject{Issuer: "i", Subject: "s"},
			DesiredState:      workspacesv1alpha1.DesiredStateRunning,
			DataPolicy:        workspacesv1alpha1.DataPolicyEphemeral,
			RuntimeGeneration: 1,
			IntentRevision:    1,
		},
	}
}

func reconcileForged(t *testing.T, ws *workspacesv1alpha1.Workspace, extra ...client.Object) (client.Client, *workspacesv1alpha1.Workspace) {
	t.Helper()
	s := snapScheme(t)
	b := fake.NewClientBuilder().WithScheme(s).WithObjects(ws).WithStatusSubresource(ws)
	if len(extra) > 0 {
		b = b.WithObjects(extra...)
	}
	c := b.Build()
	r := &WorkspaceReconciler{Client: c, Scheme: s, Backend: linux.New(c, linux.Options{})}
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

func assertSnapshotRejected(t *testing.T, c client.Client, ws *workspacesv1alpha1.Workspace, got *workspacesv1alpha1.Workspace) {
	t.Helper()
	pod := &corev1.Pod{}
	err := c.Get(context.Background(), client.ObjectKey{
		Namespace: ws.Namespace, Name: linux.PodName(ws.UID)}, pod)
	if err == nil {
		t.Fatalf("forged snapshot produced a pod: image=%s command=%v",
			pod.Spec.Containers[0].Image, pod.Spec.Containers[0].Command)
	}
	cond := condition(got, workspacesv1alpha1.ConditionDegraded)
	if cond == nil || cond.Status != metav1.ConditionTrue ||
		cond.Reason != ReasonTemplateSnapshotInvalid {
		t.Fatalf("want Degraded=True reason=%s, got %+v",
			ReasonTemplateSnapshotInvalid, got.Status.Conditions)
	}
}

// A forged snapshot whose specHash does not cover the embedded
// spec — tag-only image, arbitrary command, control-plane nodeSelector.
func TestForgedSnapshotBadHashRejected(t *testing.T) {
	spec := forgedSpec("attacker.example/miner:latest")
	ann := forgedSnapshot("does-not-exist", "x", "x", "sha256:00", spec,
		map[string]string{linux.AnnotationNodeSelector: `{"node-role.kubernetes.io/control-plane":"true"}`})
	ws := forgedWorkspace(ann)
	c, got := reconcileForged(t, ws)
	assertSnapshotRejected(t, c, ws, got)
}

// A smarter forgery computes a VALID specHash over its own spec — but a
// tag-only image still violates the digest-only invariant the CRD
// enforces on real templates.
func TestForgedSnapshotUndigestImageRejected(t *testing.T) {
	spec := forgedSpec("attacker.example/miner:latest")
	raw, _ := json.Marshal(spec)
	sum := sha256.Sum256(raw)
	ann := forgedSnapshot("does-not-exist", "x", "x",
		"sha256:"+hex.EncodeToString(sum[:]), spec, nil)
	ws := forgedWorkspace(ann)
	c, got := reconcileForged(t, ws)
	assertSnapshotRejected(t, c, ws, got)
}

// Structural forgery: hash is valid and the image is digest-pinned, but
// the runtime-block exclusivity CEL rule is violated (linux AND windows).
func TestForgedSnapshotCELInvariantRejected(t *testing.T) {
	spec := forgedSpec("cr.example/img@sha256:" + fmt.Sprintf("%064x", 7))
	spec.Windows = &workspacesv1alpha1.WindowsRuntimeSpec{
		SourcePVCRef:   workspacesv1alpha1.WindowsSourcePVCRef{Name: "seed"},
		SourceChecksum: "sha256:" + fmt.Sprintf("%064x", 7),
	}
	raw, _ := json.Marshal(spec)
	sum := sha256.Sum256(raw)
	ann := forgedSnapshot("does-not-exist", "x", "x",
		"sha256:"+hex.EncodeToString(sum[:]), spec, nil)
	ws := forgedWorkspace(ann)
	c, got := reconcileForged(t, ws)
	assertSnapshotRejected(t, c, ws, got)
}

// The recorded template object still exists with the same UID but the
// recorded spec does not match it — a forged annotation naming a REAL
// template must be caught too.
func TestForgedSnapshotLiveTemplateMismatchRejected(t *testing.T) {
	live := &workspacesv1alpha1.WorkspaceTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name: "tpl-live", Namespace: "tinycdi-tenant-a",
			UID: types.UID("dddddddd-0000-0000-0000-000000000001"),
		},
		Spec: workspacesv1alpha1.WorkspaceTemplateSpec{
			Revision: "2026-09-a", Runtime: workspacesv1alpha1.RuntimeLinuxContainer,
			Experience: workspacesv1alpha1.ExperienceDesktop,
			Linux: &workspacesv1alpha1.LinuxRuntimeSpec{
				Image: "cr.example/img@sha256:" + fmt.Sprintf("%064x", 1),
			},
			Resources: workspacesv1alpha1.ResourceProfile{
				CPU: resource.MustParse("1"), Memory: resource.MustParse("2Gi"),
				Storage: resource.MustParse("5Gi"),
			},
			BootDeadline:   metav1.Duration{Duration: 5 * time.Minute},
			NetworkProfile: workspacesv1alpha1.NetworkProfileIsolated,
		},
	}
	// Attacker claims the snapshot records tpl-live but embeds their own spec.
	spec := forgedSpec("cr.example/img@sha256:" + fmt.Sprintf("%064x", 9))
	spec.Revision = "2026-09-a"
	raw, _ := json.Marshal(spec)
	sum := sha256.Sum256(raw)
	ann := forgedSnapshot("tpl-live", string(live.UID), "2026-09-a",
		"sha256:"+hex.EncodeToString(sum[:]), spec, nil)
	ws := forgedWorkspace(ann)
	ws.Spec.TemplateRef.Name = "tpl-live"
	c, got := reconcileForged(t, ws, live)
	assertSnapshotRejected(t, c, ws, got)
}

// The annotation recorded by the operator itself still verifies — and a
// workspace whose template is deleted after admit keeps converging on the
// recorded revision (design §4: the snapshot outlives the template).
func TestHonestSnapshotSurvivesTemplateDeletion(t *testing.T) {
	s := snapScheme(t)
	tpl := &workspacesv1alpha1.WorkspaceTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name: "tpl-honest", Namespace: "tinycdi-tenant-a",
			UID: types.UID("eeeeeeee-0000-0000-0000-000000000001"),
		},
		Spec: workspacesv1alpha1.WorkspaceTemplateSpec{
			Revision: "2026-09-a", Runtime: workspacesv1alpha1.RuntimeLinuxContainer,
			Experience: workspacesv1alpha1.ExperienceDesktop,
			Linux: &workspacesv1alpha1.LinuxRuntimeSpec{
				Image: "cr.example/img@sha256:" + fmt.Sprintf("%064x", 1),
			},
			Resources: workspacesv1alpha1.ResourceProfile{
				CPU: resource.MustParse("1"), Memory: resource.MustParse("2Gi"),
				Storage: resource.MustParse("5Gi"),
			},
			BootDeadline:   metav1.Duration{Duration: 5 * time.Minute},
			NetworkProfile: workspacesv1alpha1.NetworkProfileIsolated,
		},
	}
	ws := &workspacesv1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ws-honest", Namespace: "tinycdi-tenant-a",
			UID: types.UID("cccccccc-0000-0000-0000-000000000002"),
		},
		Spec: workspacesv1alpha1.WorkspaceSpec{
			TemplateRef:       workspacesv1alpha1.TemplateReference{Name: "tpl-honest"},
			OwnerSubject:      workspacesv1alpha1.OwnerSubject{Issuer: "i", Subject: "s"},
			DesiredState:      workspacesv1alpha1.DesiredStateRunning,
			DataPolicy:        workspacesv1alpha1.DataPolicyEphemeral,
			RuntimeGeneration: 1,
			IntentRevision:    1,
		},
	}
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(tpl, ws).WithStatusSubresource(ws).Build()
	r := &WorkspaceReconciler{Client: c, Scheme: s, Backend: linux.New(c, linux.Options{})}
	key := client.ObjectKeyFromObject(ws)
	// First reconcile records the snapshot; the template is then deleted —
	// convergence must continue (the recorded revision is what runs).
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("admit reconcile: %v", err)
	}
	if err := c.Delete(context.Background(), tpl); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile after template deletion: %v", err)
	}
	pod := &corev1.Pod{}
	if err := c.Get(context.Background(),
		client.ObjectKey{Namespace: ws.Namespace, Name: linux.PodName(ws.UID)}, pod); err != nil {
		t.Fatalf("honest snapshot blocked convergence: %v", err)
	}
}
