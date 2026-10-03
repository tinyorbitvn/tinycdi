// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

// V3.2 (E1/E2): the template-snapshot annotation is re-recorded when the
// applied spec.runtimeGeneration advances AND spec.templateRef moved off
// the snapshot's source — the shape of the API's OnStart family re-point,
// which the applier writes while the CR is Stopped. Within one generation
// the recorded snapshot stays authoritative: a pod crash/recreate
// converges on it, never on a re-read of the template object.
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
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	workspacesv1alpha1 "github.com/tinyorbitvn/tinycdi/api/v1alpha1"
	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
	linux "github.com/tinyorbitvn/tinycdi/internal/runtime/linux"
)

var (
	resnapImageA = "tcdi/linux-desktop@sha256:" + fmt.Sprintf("%064x", 0xa1)
	resnapImageB = "tcdi/linux-desktop@sha256:" + fmt.Sprintf("%064x", 0xb2)
)

// familyRevision declares one published revision of the fam32 template
// family: catalog-name labeled, digest-pinned image, the caller's revision
// label.
func familyRevision(t *testing.T, c client.Client, ns, name, revision, image string) {
	t.Helper()
	newTemplate(t, c, ns, name, func(tpl *workspacesv1alpha1.WorkspaceTemplate) {
		tpl.Labels = map[string]string{provisioning.LabelCatalogName: "fam32"}
		tpl.Spec.Revision = revision
		tpl.Spec.Linux.Image = image
	})
}

// podAtGeneration returns the workspace's live pod carrying the given
// runtime-generation label, or nil while none exists.
func podAtGeneration(t *testing.T, c client.Client, ns string, uid types.UID, gen string) *corev1.Pod {
	t.Helper()
	pods := &corev1.PodList{}
	if err := c.List(context.Background(), pods, client.InNamespace(ns),
		client.MatchingLabels{linux.LabelWorkspaceUID: string(uid)}); err != nil {
		t.Fatalf("list pods: %v", err)
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Labels[linux.LabelRuntimeGeneration] == gen && p.DeletionTimestamp.IsZero() {
			return p
		}
	}
	return nil
}

// reconcileUntilPod drives the reconciler until a pod of the given
// generation exists (bounded — the caller fails when it never shows up).
func reconcileUntilPod(t *testing.T, r *WorkspaceReconciler, c client.Client,
	key types.NamespacedName, ns string, uid types.UID, gen string) *corev1.Pod {
	t.Helper()
	for i := 0; i < 20; i++ {
		reconcile(t, r, key)
		if p := podAtGeneration(t, c, ns, uid, gen); p != nil {
			return p
		}
	}
	t.Fatalf("no pod of generation %s after 20 reconciles", gen)
	return nil
}

// snapshotOf parses the recorded template-snapshot annotation.
func snapshotOf(t *testing.T, ws *workspacesv1alpha1.Workspace) templateSnapshot {
	t.Helper()
	var s templateSnapshot
	raw := ws.Annotations[AnnotationTemplateSnapshot]
	if raw == "" {
		t.Fatal("no template-snapshot annotation")
	}
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		t.Fatalf("snapshot annotation: %v", err)
	}
	return s
}

// writeSpec applies the applier's spec write for one intent: stop bumps the
// intent revision only; a re-pointing start moves templateRef, desiredState,
// runtimeGeneration and the revision in a single Update — the only shape the
// templateRef CEL rule admits (the old spec must have been Stopped).
func writeSpec(t *testing.T, c client.Client, key types.NamespacedName,
	mutate func(*workspacesv1alpha1.Workspace)) {
	t.Helper()
	ws := getWorkspace(t, c, key)
	mutate(ws)
	if err := c.Update(context.Background(), ws); err != nil {
		t.Fatalf("spec write: %v", err)
	}
}

// TestSnapshot_RewrittenOnNewGeneration: a start that carries the family
// re-point (Stopped -> Running write moving templateRef to revision B under
// a new runtimeGeneration) re-records the snapshot — the annotation's hash
// equals revision B's, and the pod the backend converges runs B's image.
func TestSnapshot_RewrittenOnNewGeneration(t *testing.T) {
	env, c := startEnv(t)
	defer func() {
		if err := env.Stop(); err != nil {
			t.Logf("envtest stop: %v", err)
		}
	}()
	ns := newNamespace(t, c)
	familyRevision(t, c, ns, "fam32-aaaa1111", "2026-10-a", resnapImageA)
	familyRevision(t, c, ns, "fam32-bbbb2222", "2026-10-b", resnapImageB)

	ws := newWorkspace(t, c, ns, "ws-resnap", "fam32-aaaa1111", nil)
	key := types.NamespacedName{Name: ws.Name, Namespace: ns}
	r := newReconciler(c)

	podA := reconcileUntilPod(t, r, c, key, ns, ws.UID, "1")
	if got := podA.Spec.Containers[0].Image; got != resnapImageA {
		t.Fatalf("generation-1 pod image = %s, want revision A %s", got, resnapImageA)
	}
	before := snapshotOf(t, getWorkspace(t, c, key))
	if before.Name != "fam32-aaaa1111" || before.SourceRef != "fam32-aaaa1111" ||
		before.RuntimeGeneration != 1 {
		t.Fatalf("recorded snapshot = %+v, want source fam32-aaaa1111 gen 1", before)
	}

	// Stop (revision forward only), then the re-pointing start the applier
	// writes: templateRef -> B, Running, generation 2, revision 3.
	writeSpec(t, c, key, func(ws *workspacesv1alpha1.Workspace) {
		ws.Spec.DesiredState = workspacesv1alpha1.DesiredStateStopped
		ws.Spec.IntentRevision = 2
	})
	reconcile(t, r, key)
	writeSpec(t, c, key, func(ws *workspacesv1alpha1.Workspace) {
		ws.Spec.TemplateRef.Name = "fam32-bbbb2222"
		ws.Spec.DesiredState = workspacesv1alpha1.DesiredStateRunning
		ws.Spec.RuntimeGeneration = 2
		ws.Spec.IntentRevision = 3
	})

	podB := reconcileUntilPod(t, r, c, key, ns, ws.UID, "2")
	if got := podB.Spec.Containers[0].Image; got != resnapImageB {
		t.Fatalf("generation-2 pod image = %s, want revision B %s", got, resnapImageB)
	}
	got := getWorkspace(t, c, key)
	snap := snapshotOf(t, got)
	tplB := &workspacesv1alpha1.WorkspaceTemplate{}
	if err := c.Get(context.Background(),
		client.ObjectKey{Namespace: ns, Name: "fam32-bbbb2222"}, tplB); err != nil {
		t.Fatalf("get revision B: %v", err)
	}
	want, err := snapshotTemplate(tplB)
	if err != nil {
		t.Fatalf("snapshotTemplate(B): %v", err)
	}
	if snap.Name != "fam32-bbbb2222" || snap.SourceRef != "fam32-bbbb2222" ||
		snap.RuntimeGeneration != 2 || snap.SpecHash != want.SpecHash {
		t.Fatalf("re-recorded snapshot = %+v, want fam32-bbbb2222 gen 2 hash %q",
			snap, want.SpecHash)
	}
}

// TestSnapshot_UnchangedWithinGeneration: a pod crash and recreate inside
// one runtime generation keeps the recorded snapshot — the template is not
// re-read and the replacement pod runs the same recorded image.
func TestSnapshot_UnchangedWithinGeneration(t *testing.T) {
	env, c := startEnv(t)
	defer func() {
		if err := env.Stop(); err != nil {
			t.Logf("envtest stop: %v", err)
		}
	}()
	ns := newNamespace(t, c)
	familyRevision(t, c, ns, "fam32-aaaa1111", "2026-10-a", resnapImageA)

	ws := newWorkspace(t, c, ns, "ws-crash", "fam32-aaaa1111", nil)
	key := types.NamespacedName{Name: ws.Name, Namespace: ns}
	r := newReconciler(c)

	podA := reconcileUntilPod(t, r, c, key, ns, ws.UID, "1")
	annBefore := getWorkspace(t, c, key).Annotations[AnnotationTemplateSnapshot]

	// The pod dies inside the generation; the controller must rebuild from
	// the recorded snapshot, not the template object — even when the
	// template was re-published under a different image meanwhile (the new
	// revision object exists but the reference never moved).
	familyRevision(t, c, ns, "fam32-bbbb2222", "2026-10-b", resnapImageB)
	if err := c.Delete(context.Background(), podA); err != nil {
		t.Fatalf("delete pod: %v", err)
	}
	podB := reconcileUntilPod(t, r, c, key, ns, ws.UID, "1")
	if podB.UID == podA.UID {
		t.Fatal("pod was not recreated")
	}
	if got := podB.Spec.Containers[0].Image; got != resnapImageA {
		t.Fatalf("recreated pod image = %s, want recorded revision A %s", got, resnapImageA)
	}
	annAfter := getWorkspace(t, c, key).Annotations[AnnotationTemplateSnapshot]
	if annAfter != annBefore {
		t.Fatalf("snapshot annotation changed within the generation:\nbefore %s\nafter  %s",
			annBefore, annAfter)
	}
}

// TestSnapshot_KeptWhenStartKeepsFamilyRef: a Stopped -> Running start that
// carries no re-point (the family reference is unchanged — a pinned or
// guard-skipped start) must NOT re-resolve the family to its newer member:
// the snapshot stays on the recorded revision and the pod keeps its image.
func TestSnapshot_KeptWhenStartKeepsFamilyRef(t *testing.T) {
	env, c := startEnv(t)
	defer func() {
		if err := env.Stop(); err != nil {
			t.Logf("envtest stop: %v", err)
		}
	}()
	ns := newNamespace(t, c)
	familyRevision(t, c, ns, "fam32-aaaa1111", "2026-10-a", resnapImageA)

	// The create path writes the catalog (family) name, not the resolved
	// revision object — the resolver records revision A under it.
	ws := newWorkspace(t, c, ns, "ws-familyref", "fam32", nil)
	key := types.NamespacedName{Name: ws.Name, Namespace: ns}
	r := newReconciler(c)

	podA := reconcileUntilPod(t, r, c, key, ns, ws.UID, "1")
	if got := podA.Spec.Containers[0].Image; got != resnapImageA {
		t.Fatalf("generation-1 pod image = %s, want revision A %s", got, resnapImageA)
	}
	if snap := snapshotOf(t, getWorkspace(t, c, key)); snap.Name != "fam32-aaaa1111" ||
		snap.SourceRef != "fam32" {
		t.Fatalf("recorded snapshot = %+v, want fam32-aaaa1111 from ref fam32", snap)
	}

	// Revision B publishes, then the workspace stops and starts WITHOUT a
	// re-point (e.g. recorded revision pinned, or the guard refused).
	familyRevision(t, c, ns, "fam32-bbbb2222", "2026-10-b", resnapImageB)
	writeSpec(t, c, key, func(ws *workspacesv1alpha1.Workspace) {
		ws.Spec.DesiredState = workspacesv1alpha1.DesiredStateStopped
		ws.Spec.IntentRevision = 2
	})
	reconcile(t, r, key)
	writeSpec(t, c, key, func(ws *workspacesv1alpha1.Workspace) {
		ws.Spec.DesiredState = workspacesv1alpha1.DesiredStateRunning
		ws.Spec.RuntimeGeneration = 2
		ws.Spec.IntentRevision = 3
	})
	podB := reconcileUntilPod(t, r, c, key, ns, ws.UID, "2")
	if got := podB.Spec.Containers[0].Image; got != resnapImageA {
		t.Fatalf("generation-2 pod image = %s, want kept revision A %s — the guard's stay was overridden",
			got, resnapImageA)
	}
	snap := snapshotOf(t, getWorkspace(t, c, key))
	if snap.Name != "fam32-aaaa1111" || snap.SourceRef != "fam32" || snap.RuntimeGeneration != 1 {
		t.Fatalf("snapshot = %+v, want the recorded revision A untouched", snap)
	}
}

// TestForgedSnapshotStillRejected (SEC-10, V3.2 regression): a forged
// template-snapshot annotation still never reaches the backend — a bad
// hash or an invariant-breaking spec is held, and a forged record under a
// moved reference is overwritten by an honest re-snapshot of the resolved
// template instead of being converged.
func TestForgedSnapshotStillRejected(t *testing.T) {
	t.Run("forgery held", func(t *testing.T) {
		spec := forgedSpec("attacker.example/miner:latest")
		ann := forgedSnapshot("does-not-exist", "x", "x", "sha256:00", spec, nil)
		ws := forgedWorkspace(ann)
		c, got := reconcileForged(t, ws)
		assertSnapshotRejected(t, c, ws, got)
	})

	t.Run("moved ref re-snapshots over the forgery", func(t *testing.T) {
		s := snapScheme(t)
		live := &workspacesv1alpha1.WorkspaceTemplate{
			ObjectMeta: metav1.ObjectMeta{
				Name: "tpl-live", Namespace: "tinycdi-tenant-a",
				UID: types.UID("dddddddd-0000-0000-0000-0000000000aa"),
			},
			Spec: workspacesv1alpha1.WorkspaceTemplateSpec{
				Revision: "2026-10-b", Runtime: workspacesv1alpha1.RuntimeLinuxContainer,
				Experience: workspacesv1alpha1.ExperienceDesktop,
				Linux:      &workspacesv1alpha1.LinuxRuntimeSpec{Image: resnapImageB},
				Resources: workspacesv1alpha1.ResourceProfile{
					CPU: resource.MustParse("1"), Memory: resource.MustParse("2Gi"),
					Storage: resource.MustParse("5Gi"),
				},
				BootDeadline:   metav1.Duration{Duration: 5 * time.Minute},
				NetworkProfile: workspacesv1alpha1.NetworkProfileIsolated,
			},
		}
		// Forged record: valid hash over an attacker spec, but it claims a
		// source the reference no longer names — the re-snapshot path must
		// replace it with the resolved object, never converge it.
		spec := forgedSpec("attacker.example/miner@sha256:" + fmt.Sprintf("%064x", 9))
		raw, _ := json.Marshal(spec)
		sum := sha256.Sum256(raw)
		ann := map[string]any{
			"name": "does-not-exist", "uid": "x", "revision": "x",
			"specHash": "sha256:" + hex.EncodeToString(sum[:]),
			"spec":     spec, "sourceRef": "old-ref", "runtimeGeneration": 0,
		}
		annRaw, _ := json.Marshal(ann)
		ws := forgedWorkspace(string(annRaw))
		ws.Spec.TemplateRef.Name = "tpl-live"

		c := fake.NewClientBuilder().WithScheme(s).
			WithObjects(ws, live).WithStatusSubresource(ws).Build()
		r := &WorkspaceReconciler{Client: c, Scheme: s, Backend: linux.New(c, linux.Options{})}
		key := client.ObjectKeyFromObject(ws)
		reconcile(t, r, key)
		reconcile(t, r, key)

		pod := &corev1.Pod{}
		if err := c.Get(context.Background(), client.ObjectKey{
			Namespace: ws.Namespace, Name: linux.PodName(ws.UID)}, pod); err != nil {
			t.Fatalf("honest re-snapshot produced no pod: %v", err)
		}
		if got := pod.Spec.Containers[0].Image; got != resnapImageB {
			t.Fatalf("pod image = %s, want the resolved template's %s (forged spec never converged)",
				got, resnapImageB)
		}
		got := &workspacesv1alpha1.Workspace{}
		if err := c.Get(context.Background(), key, got); err != nil {
			t.Fatal(err)
		}
		snap := snapshotOf(t, got)
		want, err := snapshotTemplate(live)
		if err != nil {
			t.Fatalf("snapshotTemplate(live): %v", err)
		}
		if snap.SpecHash != want.SpecHash || snap.Name != "tpl-live" {
			t.Fatalf("snapshot after re-snapshot = %+v, want honest record of tpl-live", snap)
		}
	})
}
