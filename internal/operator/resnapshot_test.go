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
	"errors"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	schedv1 "k8s.io/api/scheduling/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

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

// TestSnapshot_UpgradeAdoption: the upgrade path end to end on a real
// apiserver. A running workspace admitted before status.templateSnapshot
// existed establishes its record from the incarnation pod's stamped
// template identity + the LIVE revision object — the annotation is never
// consulted. Pods that predate the stamps, or whose stamped revision is
// gone, hold Degraded/TemplateRevisionGone with the pod left running;
// stop/start re-snapshots.
func TestSnapshot_UpgradeAdoption(t *testing.T) {
	env, c := startEnv(t)
	defer func() {
		if err := env.Stop(); err != nil {
			t.Logf("envtest stop: %v", err)
		}
	}()

	// seedRunning builds the workspace's pod from tpl through the real
	// backend — stamped with the template's identity + hash the way a
	// post-change operator builds it — and optionally marks it scheduled.
	seedRunning := func(t *testing.T, ns string, ws *workspacesv1alpha1.Workspace,
		tpl *workspacesv1alpha1.WorkspaceTemplate, nodeName string) *corev1.Pod {
		t.Helper()
		b := linux.New(c, linux.Options{})
		if _, err := b.Ensure(context.Background(), ws, tpl); err != nil {
			t.Fatalf("seed pod: %v", err)
		}
		pod := &corev1.Pod{}
		if err := c.Get(context.Background(), client.ObjectKey{
			Namespace: ns, Name: linux.PodName(ws.UID)}, pod); err != nil {
			t.Fatalf("seeded pod: %v", err)
		}
		if nodeName != "" {
			// Pod spec is immutable after create — rebuild the pod as a
			// scheduled incarnation would look: nodeName set at bind
			// time and a real PriorityClass stamped by priority
			// admission.
			pc := &schedv1.PriorityClass{
				ObjectMeta: metav1.ObjectMeta{Name: "tcdi-session-priority-" + ns},
				Value:      1234,
			}
			if err := c.Create(context.Background(), pc); err != nil {
				t.Fatalf("seed priority class: %v", err)
			}
			scheduled := &corev1.Pod{
				ObjectMeta: *pod.ObjectMeta.DeepCopy(),
				Spec:       *pod.Spec.DeepCopy(),
			}
			prio := int32(1234)
			scheduled.Spec.NodeName = nodeName
			scheduled.Spec.PriorityClassName = pc.Name
			scheduled.Spec.Priority = &prio
			scheduled.Spec.DeprecatedServiceAccount = scheduled.Spec.ServiceAccountName
			scheduled.ResourceVersion = ""
			scheduled.UID = ""
			scheduled.CreationTimestamp = metav1.Time{}
			if err := c.Delete(context.Background(), pod); err != nil {
				t.Fatalf("replace pod: %v", err)
			}
			if err := c.Create(context.Background(), scheduled); err != nil {
				t.Fatalf("mark pod scheduled: %v", err)
			}
			pod = scheduled
		}
		return pod
	}
	seedAnnotation := func(t *testing.T, ws *workspacesv1alpha1.Workspace, ann string) {
		t.Helper()
		cur := getWorkspace(t, c, client.ObjectKeyFromObject(ws))
		if cur.Annotations == nil {
			cur.Annotations = map[string]string{}
		}
		cur.Annotations[AnnotationTemplateSnapshot] = ann
		if err := c.Update(context.Background(), cur); err != nil {
			t.Fatalf("seed annotation: %v", err)
		}
	}
	assertPodHeldGone := func(t *testing.T, key types.NamespacedName, pod *corev1.Pod) {
		t.Helper()
		got := getWorkspace(t, c, key)
		cond := condition(got, workspacesv1alpha1.ConditionDegraded)
		if cond == nil || cond.Status != metav1.ConditionTrue ||
			cond.Reason != ReasonTemplateRevisionGone {
			t.Fatalf("want Degraded/TemplateRevisionGone, got %+v", got.Status.Conditions)
		}
		cur := &corev1.Pod{}
		if err := c.Get(context.Background(), client.ObjectKeyFromObject(pod), cur); err != nil {
			t.Fatalf("pod vanished on revision-gone hold: %v", err)
		}
		if cur.UID != pod.UID {
			t.Fatal("revision-gone hold recreated the pod")
		}
	}

	t.Run("stamped pod adopts its live revision, pod untouched", func(t *testing.T) {
		ns := newNamespace(t, c)
		familyRevision(t, c, ns, "fam32-aaaa1111", "2026-10-a", resnapImageA)
		ws := newWorkspace(t, c, ns, "ws-adopt", "fam32", nil)
		key := types.NamespacedName{Name: ws.Name, Namespace: ns}
		live := &workspacesv1alpha1.WorkspaceTemplate{}
		if err := c.Get(context.Background(),
			client.ObjectKey{Namespace: ns, Name: "fam32-aaaa1111"}, live); err != nil {
			t.Fatal(err)
		}
		pod := seedRunning(t, ns, ws, live, "node-7") // scheduled pod

		r := newReconciler(c)
		reconcile(t, r, key)
		got := getWorkspace(t, c, key)
		snap := got.Status.TemplateSnapshot
		if snap == nil {
			t.Fatal("stamped pod's revision was not adopted into status")
		}
		want, err := snapshotTemplate(live)
		if err != nil {
			t.Fatalf("snapshotTemplate: %v", err)
		}
		if snap.SpecHash != want.SpecHash || snap.UID != want.UID ||
			snap.RuntimeGeneration != 1 || snap.SourceRef != "fam32" {
			t.Fatalf("adopted snapshot = %+v, want record of fam32-aaaa1111", snap)
		}
		cur := &corev1.Pod{}
		if err := c.Get(context.Background(), client.ObjectKeyFromObject(pod), cur); err != nil {
			t.Fatalf("pod vanished after adoption: %v", err)
		}
		if cur.UID != pod.UID {
			t.Fatal("adoption recreated the pod")
		}
	})

	t.Run("forged annotation never consulted", func(t *testing.T) {
		ns := newNamespace(t, c)
		familyRevision(t, c, ns, "fam32-aaaa1111", "2026-10-a", resnapImageA)
		ws := newWorkspace(t, c, ns, "ws-adoptf", "fam32", nil)
		key := types.NamespacedName{Name: ws.Name, Namespace: ns}
		live := &workspacesv1alpha1.WorkspaceTemplate{}
		if err := c.Get(context.Background(),
			client.ObjectKey{Namespace: ns, Name: "fam32-aaaa1111"}, live); err != nil {
			t.Fatal(err)
		}
		pod := seedRunning(t, ns, ws, live, "")
		// Forge: valid hash over an attacker spec (control-plane
		// placement + attacker image) claiming the honest revision.
		forged := forgedSpec("attacker.example/miner@sha256:" + fmt.Sprintf("%064x", 3))
		forged.Revision = "2026-10-a"
		forged.Placement = &workspacesv1alpha1.PlacementSpec{
			NodeSelector: map[string]string{"node-role.kubernetes.io/control-plane": ""},
		}
		fraw, _ := json.Marshal(forged)
		fsum := sha256.Sum256(fraw)
		var snap templateSnapshot
		if err := json.Unmarshal([]byte(forgedSnapshot("fam32-aaaa1111",
			string(live.UID), "2026-10-a", "sha256:"+hex.EncodeToString(fsum[:]),
			forged, nil)), &snap); err != nil {
			t.Fatal(err)
		}
		snap.SourceRef = "fam32"
		snap.RuntimeGeneration = 1
		annRaw, _ := json.Marshal(&snap)
		seedAnnotation(t, ws, string(annRaw))

		r := newReconciler(c)
		reconcile(t, r, key)
		got := getWorkspace(t, c, key)
		recorded := got.Status.TemplateSnapshot
		if recorded == nil {
			t.Fatal("stamped pod's revision was not adopted")
		}
		want, err := snapshotTemplate(live)
		if err != nil {
			t.Fatalf("snapshotTemplate: %v", err)
		}
		if recorded.SpecHash != want.SpecHash {
			t.Fatalf("forged annotation content reached status: %q", recorded.SpecHash)
		}
		cur := &corev1.Pod{}
		if err := c.Get(context.Background(), client.ObjectKeyFromObject(pod), cur); err != nil {
			t.Fatalf("pod vanished: %v", err)
		}
		if img := cur.Spec.Containers[0].Image; img != resnapImageA {
			t.Fatalf("pod rebuilt from forged spec: %q", img)
		}
	})

	t.Run("stamped revision gone holds degraded", func(t *testing.T) {
		ns := newNamespace(t, c)
		familyRevision(t, c, ns, "fam32-aaaa1111", "2026-10-a", resnapImageA)
		ws := newWorkspace(t, c, ns, "ws-adoptg", "fam32", nil)
		key := types.NamespacedName{Name: ws.Name, Namespace: ns}
		live := &workspacesv1alpha1.WorkspaceTemplate{}
		if err := c.Get(context.Background(),
			client.ObjectKey{Namespace: ns, Name: "fam32-aaaa1111"}, live); err != nil {
			t.Fatal(err)
		}
		pod := seedRunning(t, ns, ws, live, "")
		// The revision object was pruned since the pod was built.
		if err := c.Delete(context.Background(), live); err != nil {
			t.Fatalf("delete revision: %v", err)
		}
		r := newReconciler(c)
		reconcile(t, r, key)
		assertPodHeldGone(t, key, pod)
	})

	t.Run("pre-stamp pod adopts resolved revision, pod untouched", func(t *testing.T) {
		ns := newNamespace(t, c)
		familyRevision(t, c, ns, "fam32-aaaa1111", "2026-10-a", resnapImageA)
		ws := newWorkspace(t, c, ns, "ws-adoptu", "fam32", nil)
		key := types.NamespacedName{Name: ws.Name, Namespace: ns}
		live := &workspacesv1alpha1.WorkspaceTemplate{}
		if err := c.Get(context.Background(),
			client.ObjectKey{Namespace: ns, Name: "fam32-aaaa1111"}, live); err != nil {
			t.Fatal(err)
		}
		// A v0.5.0 pod carries no identity stamps; its annotation — even
		// an honest legacy-shaped one — is never a source. The resolved
		// candidate is the live revision; the pod provably matches it.
		pod := seedRunning(t, ns, ws, live, "node-7") // scheduled pod
		delete(pod.Annotations, linux.AnnotationTemplateName)
		delete(pod.Annotations, linux.AnnotationTemplateRevision)
		delete(pod.Annotations, linux.AnnotationTemplateHash)
		if err := c.Update(context.Background(), pod); err != nil {
			t.Fatalf("unstamp pod: %v", err)
		}
		seedAnnotation(t, ws, snapshotAnnotation(t, live, "", 0))
		r := newReconciler(c)
		reconcile(t, r, key)
		got := getWorkspace(t, c, key)
		snap := got.Status.TemplateSnapshot
		if snap == nil || snap.Name != "fam32-aaaa1111" {
			t.Fatalf("pre-stamp pod's resolved revision was not adopted: %+v", snap)
		}
		cur := &corev1.Pod{}
		if err := c.Get(context.Background(), client.ObjectKeyFromObject(pod), cur); err != nil {
			t.Fatalf("pod vanished: %v", err)
		}
		if cur.UID != pod.UID {
			t.Fatal("adoption recreated the pod")
		}
		// The identity stamps were backfilled onto the running pod.
		if cur.Annotations[linux.AnnotationTemplateName] != "fam32-aaaa1111" {
			t.Fatalf("identity stamps not backfilled: %+v", cur.Annotations)
		}
	})

	t.Run("pre-stamp pod rotated revision holds degraded", func(t *testing.T) {
		ns := newNamespace(t, c)
		familyRevision(t, c, ns, "fam32-aaaa1111", "2026-10-a", resnapImageA)
		ws := newWorkspace(t, c, ns, "ws-adoptr", "fam32", nil)
		key := types.NamespacedName{Name: ws.Name, Namespace: ns}
		live := &workspacesv1alpha1.WorkspaceTemplate{}
		if err := c.Get(context.Background(),
			client.ObjectKey{Namespace: ns, Name: "fam32-aaaa1111"}, live); err != nil {
			t.Fatal(err)
		}
		pod := seedRunning(t, ns, ws, live, "")
		delete(pod.Annotations, linux.AnnotationTemplateName)
		delete(pod.Annotations, linux.AnnotationTemplateRevision)
		delete(pod.Annotations, linux.AnnotationTemplateHash)
		if err := c.Update(context.Background(), pod); err != nil {
			t.Fatalf("unstamp pod: %v", err)
		}
		// The pod was built from fam32-aaaa1111; a fresh revision object
		// with different pod content now resolves the family — the pod
		// disproves the candidate.
		familyRevision(t, c, ns, "fam32-bbbb2222", "2026-10-b", resnapImageB)
		r := newReconciler(c)
		reconcile(t, r, key)
		assertPodHeldGone(t, key, pod)
	})
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

// legacySnapshotAnnotation renders the annotation exactly as a pre-V3.2
// operator recorded it: sourceRef/runtimeGeneration omitted.
func legacySnapshotAnnotation(t *testing.T, tpl *workspacesv1alpha1.WorkspaceTemplate) string {
	t.Helper()
	s, err := snapshotTemplate(tpl)
	if err != nil {
		t.Fatalf("snapshotTemplate: %v", err)
	}
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	return string(raw)
}

// legacyWorkspace seeds a Running workspace carrying a pre-V3.2 snapshot
// (no sourceRef) of the recorded revision object — in status, as the
// upgrade-adoption path records it, and in the mirrored annotation.
func legacyWorkspace(ref, ann string, uid types.UID) *workspacesv1alpha1.Workspace {
	var snap templateSnapshot
	if err := json.Unmarshal([]byte(ann), &snap); err != nil {
		panic(err)
	}
	return &workspacesv1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ws-legacy", Namespace: "tinycdi-tenant-a", UID: uid,
			Labels:      map[string]string{"workspaces.cdi.tinyorbit.vn/workspace-uid": "ws_legacy"},
			Annotations: map[string]string{AnnotationTemplateSnapshot: ann},
		},
		Spec: workspacesv1alpha1.WorkspaceSpec{
			TemplateRef:       workspacesv1alpha1.TemplateReference{Name: ref},
			OwnerSubject:      workspacesv1alpha1.OwnerSubject{Issuer: "i", Subject: "s"},
			DesiredState:      workspacesv1alpha1.DesiredStateRunning,
			DataPolicy:        workspacesv1alpha1.DataPolicyEphemeral,
			RuntimeGeneration: 1,
			IntentRevision:    1,
		},
		Status: workspacesv1alpha1.WorkspaceStatus{TemplateSnapshot: &snap},
	}
}

// recordedRevision builds the revision-A object a legacy snapshot would have
// been taken from — catalog-name labeled fam32.
func recordedRevision(uid types.UID) *workspacesv1alpha1.WorkspaceTemplate {
	return &workspacesv1alpha1.WorkspaceTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name: "fam32-aaaa1111", Namespace: "tinycdi-tenant-a", UID: uid,
			Labels: map[string]string{provisioning.LabelCatalogName: "fam32"},
		},
		Spec: workspacesv1alpha1.WorkspaceTemplateSpec{
			Revision: "2026-10-a", Runtime: workspacesv1alpha1.RuntimeLinuxContainer,
			Experience: workspacesv1alpha1.ExperienceDesktop,
			Linux:      &workspacesv1alpha1.LinuxRuntimeSpec{Image: resnapImageA},
			Resources: workspacesv1alpha1.ResourceProfile{
				CPU: resource.MustParse("500m"), Memory: resource.MustParse("512Mi"),
				Storage: resource.MustParse("5Gi"),
			},
			BootDeadline:   metav1.Duration{Duration: 5 * time.Minute},
			NetworkProfile: workspacesv1alpha1.NetworkProfileIsolated,
		},
	}
}

// TestSnapshot_LegacySourceRefFallback (review PR #56): the SourceRef==""
// branch of snapshotStale — a pre-V3.2 snapshot has no recorded source, so
// the reference is compared against the recorded object name extended by
// its catalog-name label.
func TestSnapshot_LegacySourceRefFallback(t *testing.T) {
	newest := func() *workspacesv1alpha1.WorkspaceTemplate {
		b := recordedRevision(types.UID("uid-b"))
		b.Name = "fam32-bbbb2222"
		b.Spec.Revision = "2026-10-b"
		b.Spec.Linux.Image = resnapImageB
		return b
	}
	reconciled := func(t *testing.T, c client.Client, ws *workspacesv1alpha1.Workspace) *workspacesv1alpha1.Workspace {
		t.Helper()
		r := &WorkspaceReconciler{Client: c, Scheme: snapScheme(t), Backend: linux.New(c, linux.Options{})}
		key := client.ObjectKeyFromObject(ws)
		reconcile(t, r, key)
		reconcile(t, r, key)
		got := &workspacesv1alpha1.Workspace{}
		if err := c.Get(context.Background(), key, got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	podImage := func(t *testing.T, c client.Client, ws *workspacesv1alpha1.Workspace) string {
		t.Helper()
		pod := &corev1.Pod{}
		if err := c.Get(context.Background(), client.ObjectKey{
			Namespace: ws.Namespace, Name: linux.PodName(ws.UID)}, pod); err != nil {
			t.Fatalf("pod: %v", err)
		}
		return pod.Spec.Containers[0].Image
	}

	// The reference names the recorded revision object itself: not stale.
	t.Run("ref is the recorded object name", func(t *testing.T) {
		s := snapScheme(t)
		rec := recordedRevision(types.UID("uid-a"))
		ann := legacySnapshotAnnotation(t, rec)
		ws := legacyWorkspace("fam32-aaaa1111", ann, types.UID("ws-legacy-a"))
		c := fake.NewClientBuilder().WithScheme(s).
			WithObjects(ws, rec).WithStatusSubresource(ws).Build()
		got := reconciled(t, c, ws)
		if got.Annotations[AnnotationTemplateSnapshot] != ann {
			t.Fatal("snapshot rewritten although the reference still names the recorded object")
		}
		if img := podImage(t, c, ws); img != resnapImageA {
			t.Fatalf("pod image = %s, want recorded revision A %s", img, resnapImageA)
		}
	})

	// The reference holds the family (catalog) name and the recorded object
	// still exists to prove membership: keep the snapshot — a pinned or
	// guard-skipped start must not re-resolve the family.
	t.Run("family ref + live recorded object keeps", func(t *testing.T) {
		s := snapScheme(t)
		rec := recordedRevision(types.UID("uid-a"))
		ann := legacySnapshotAnnotation(t, rec)
		ws := legacyWorkspace("fam32", ann, types.UID("ws-legacy-b"))
		c := fake.NewClientBuilder().WithScheme(s).
			WithObjects(ws, rec, newest()).WithStatusSubresource(ws).Build()
		got := reconciled(t, c, ws)
		if got.Annotations[AnnotationTemplateSnapshot] != ann {
			t.Fatal("snapshot rewritten although the family reference is unchanged")
		}
		if img := podImage(t, c, ws); img != resnapImageA {
			t.Fatalf("pod image = %s, want recorded revision A %s", img, resnapImageA)
		}
	})

	// The recorded object is gone (chart upgrade deleted it): the family
	// reference re-resolves to the newest published revision — the V3.1
	// adopt-newest contract.
	t.Run("family ref + deleted recorded object adopts newest", func(t *testing.T) {
		s := snapScheme(t)
		ann := legacySnapshotAnnotation(t, recordedRevision(types.UID("uid-a")))
		ws := legacyWorkspace("fam32", ann, types.UID("ws-legacy-c"))
		c := fake.NewClientBuilder().WithScheme(s).
			WithObjects(ws, newest()).WithStatusSubresource(ws).Build()
		got := reconciled(t, c, ws)
		snap := snapshotOf(t, got)
		if snap.Name != "fam32-bbbb2222" || snap.SourceRef != "fam32" || snap.RuntimeGeneration != 1 {
			t.Fatalf("snapshot = %+v, want re-recorded fam32-bbbb2222", snap)
		}
		if img := podImage(t, c, ws); img != resnapImageB {
			t.Fatalf("pod image = %s, want adopted newest revision B %s", img, resnapImageB)
		}
	})

	// A transient read error on the recorded object is not a NotFound: the
	// reconcile must retry with the recorded snapshot untouched — never a
	// silent adopt-newest.
	t.Run("recorded object read error requeues, snapshot kept", func(t *testing.T) {
		s := snapScheme(t)
		rec := recordedRevision(types.UID("uid-a"))
		ann := legacySnapshotAnnotation(t, rec)
		ws := legacyWorkspace("fam32", ann, types.UID("ws-legacy-d"))
		boom := errors.New("apiserver unavailable")
		c := fake.NewClientBuilder().WithScheme(s).
			WithObjects(ws, rec, newest()).WithStatusSubresource(ws).
			WithInterceptorFuncs(interceptor.Funcs{
				Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if _, ok := obj.(*workspacesv1alpha1.WorkspaceTemplate); ok {
						return boom
					}
					return cl.Get(ctx, key, obj, opts...)
				},
			}).Build()
		r := &WorkspaceReconciler{Client: c, Scheme: s, Backend: linux.New(c, linux.Options{})}
		_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(ws)})
		if !errors.Is(err, boom) {
			t.Fatalf("reconcile err = %v, want the catalog read error propagated (retry, no silent re-point)", err)
		}
		got := &workspacesv1alpha1.Workspace{}
		if err := c.Get(context.Background(), client.ObjectKeyFromObject(ws), got); err != nil {
			t.Fatal(err)
		}
		if got.Annotations[AnnotationTemplateSnapshot] != ann {
			t.Fatal("snapshot rewritten on a transient catalog error")
		}
		pod := &corev1.Pod{}
		if err := c.Get(context.Background(), client.ObjectKey{
			Namespace: ws.Namespace, Name: linux.PodName(ws.UID)}, pod); err == nil {
			t.Fatalf("pod created while the catalog read was failing: %s", pod.Spec.Containers[0].Image)
		}
	})
}
