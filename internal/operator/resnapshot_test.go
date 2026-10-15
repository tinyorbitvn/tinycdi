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
	schedv1 "k8s.io/api/scheduling/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	cdiv1 "github.com/tinyorbitvn/tinycdi/api/v1"
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
	newTemplate(t, c, ns, name, func(tpl *cdiv1.WorkspaceTemplate) {
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
func snapshotOf(t *testing.T, ws *cdiv1.Workspace) templateSnapshot {
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
	mutate func(*cdiv1.Workspace)) {
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
	writeSpec(t, c, key, func(ws *cdiv1.Workspace) {
		ws.Spec.DesiredState = cdiv1.DesiredStateStopped
		ws.Spec.IntentRevision = 2
	})
	reconcile(t, r, key)
	writeSpec(t, c, key, func(ws *cdiv1.Workspace) {
		ws.Spec.TemplateRef.Name = "fam32-bbbb2222"
		ws.Spec.DesiredState = cdiv1.DesiredStateRunning
		ws.Spec.RuntimeGeneration = 2
		ws.Spec.IntentRevision = 3
	})

	podB := reconcileUntilPod(t, r, c, key, ns, ws.UID, "2")
	if got := podB.Spec.Containers[0].Image; got != resnapImageB {
		t.Fatalf("generation-2 pod image = %s, want revision B %s", got, resnapImageB)
	}
	got := getWorkspace(t, c, key)
	snap := snapshotOf(t, got)
	tplB := &cdiv1.WorkspaceTemplate{}
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

// TestSnapshot_KeptWhenStartKeepsRef: a Stopped -> Running start that
// carries no re-point (the reference is unchanged — a pinned or
// guard-skipped start) must NOT drift to the family's newer member:
// the snapshot stays on the recorded revision and the pod keeps its image.
func TestSnapshot_KeptWhenStartKeepsRef(t *testing.T) {
	env, c := startEnv(t)
	defer func() {
		if err := env.Stop(); err != nil {
			t.Logf("envtest stop: %v", err)
		}
	}()
	ns := newNamespace(t, c)
	familyRevision(t, c, ns, "fam32-aaaa1111", "2026-10-a", resnapImageA)

	// The create path stamps the resolved revision object name — the
	// workspace was admitted on fam32-aaaa1111.
	ws := newWorkspace(t, c, ns, "ws-familyref", "fam32-aaaa1111", nil)
	key := types.NamespacedName{Name: ws.Name, Namespace: ns}
	r := newReconciler(c)

	podA := reconcileUntilPod(t, r, c, key, ns, ws.UID, "1")
	if got := podA.Spec.Containers[0].Image; got != resnapImageA {
		t.Fatalf("generation-1 pod image = %s, want revision A %s", got, resnapImageA)
	}
	if snap := snapshotOf(t, getWorkspace(t, c, key)); snap.Name != "fam32-aaaa1111" ||
		snap.SourceRef != "fam32-aaaa1111" {
		t.Fatalf("recorded snapshot = %+v, want fam32-aaaa1111", snap)
	}

	// Revision B publishes, then the workspace stops and starts WITHOUT a
	// re-point (e.g. recorded revision pinned, or the guard refused).
	familyRevision(t, c, ns, "fam32-bbbb2222", "2026-10-b", resnapImageB)
	writeSpec(t, c, key, func(ws *cdiv1.Workspace) {
		ws.Spec.DesiredState = cdiv1.DesiredStateStopped
		ws.Spec.IntentRevision = 2
	})
	reconcile(t, r, key)
	writeSpec(t, c, key, func(ws *cdiv1.Workspace) {
		ws.Spec.DesiredState = cdiv1.DesiredStateRunning
		ws.Spec.RuntimeGeneration = 2
		ws.Spec.IntentRevision = 3
	})
	podB := reconcileUntilPod(t, r, c, key, ns, ws.UID, "2")
	if got := podB.Spec.Containers[0].Image; got != resnapImageA {
		t.Fatalf("generation-2 pod image = %s, want kept revision A %s — the guard's stay was overridden",
			got, resnapImageA)
	}
	snap := snapshotOf(t, getWorkspace(t, c, key))
	if snap.Name != "fam32-aaaa1111" || snap.SourceRef != "fam32-aaaa1111" || snap.RuntimeGeneration != 1 {
		t.Fatalf("snapshot = %+v, want the recorded revision A untouched", snap)
	}
}

// TestSnapshot_StatusLossAdoption: status-loss recovery end to end on a
// real apiserver. A running workspace without status.templateSnapshot
// re-establishes its record from the incarnation pod's stamped template
// identity + the LIVE revision object — the annotation is never consulted.
// Pods without the stamps, or whose stamped revision is gone, hold
// Degraded/TemplateRevisionGone with the pod left running; stop/start
// re-snapshots.
func TestSnapshot_StatusLossAdoption(t *testing.T) {
	env, c := startEnv(t)
	defer func() {
		if err := env.Stop(); err != nil {
			t.Logf("envtest stop: %v", err)
		}
	}()

	// seedRunning builds the workspace's pod from tpl through the real
	// backend — stamped with the template's identity + hash the way a
	// post-change operator builds it — and optionally marks it scheduled.
	seedRunning := func(t *testing.T, ns string, ws *cdiv1.Workspace,
		tpl *cdiv1.WorkspaceTemplate, nodeName string) *corev1.Pod {
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
	seedAnnotation := func(t *testing.T, ws *cdiv1.Workspace, ann string) {
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
		cond := condition(got, cdiv1.ConditionDegraded)
		if cond == nil || cond.Status != metav1.ConditionTrue ||
			cond.Reason != ReasonTemplateRevisionGone {
			t.Fatalf("want Degraded/TemplateRevisionGone, got %+v", got.Status.Conditions)
		}
		// The pod is the running incarnation: the hold phase stays Ready
		// with the Degraded condition carrying the hold — the broker's
		// running projection keeps the workspace under lifecycle
		// enforcement and ticket issuance keeps working.
		if got.Status.Phase != cdiv1.WorkspacePhaseReady {
			t.Fatalf("held workspace with a live pod: phase=%q, want Ready", got.Status.Phase)
		}
		if got.Status.RuntimeUID != string(pod.UID) {
			t.Fatalf("runtimeUID=%q, want the live pod %q", got.Status.RuntimeUID, pod.UID)
		}
		if got.Status.ServiceRef == nil || got.Status.StartedAt == nil {
			t.Fatalf("serviceRef/startedAt lost on hold: %+v", got.Status)
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
		live := &cdiv1.WorkspaceTemplate{}
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
		live := &cdiv1.WorkspaceTemplate{}
		if err := c.Get(context.Background(),
			client.ObjectKey{Namespace: ns, Name: "fam32-aaaa1111"}, live); err != nil {
			t.Fatal(err)
		}
		pod := seedRunning(t, ns, ws, live, "")
		// Forge: valid hash over an attacker spec (control-plane
		// placement + attacker image) claiming the honest revision.
		forged := forgedSpec("attacker.example/miner@sha256:" + fmt.Sprintf("%064x", 3))
		forged.Revision = "2026-10-a"
		forged.Placement = &cdiv1.PlacementSpec{
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
		live := &cdiv1.WorkspaceTemplate{}
		if err := c.Get(context.Background(),
			client.ObjectKey{Namespace: ns, Name: "fam32-aaaa1111"}, live); err != nil {
			t.Fatal(err)
		}
		pod := seedRunning(t, ns, ws, live, "")
		// The revision object was pruned since the pod was built; the pod
		// itself is the serving incarnation.
		markPodReady(t, c, pod)
		if err := c.Delete(context.Background(), live); err != nil {
			t.Fatalf("delete revision: %v", err)
		}
		r := newReconciler(c)
		reconcile(t, r, key)
		assertPodHeldGone(t, key, pod)
	})

	t.Run("unstamped pod holds degraded even when it matches, pod untouched", func(t *testing.T) {
		ns := newNamespace(t, c)
		familyRevision(t, c, ns, "fam32-aaaa1111", "2026-10-a", resnapImageA)
		ws := newWorkspace(t, c, ns, "ws-adoptu", "fam32-aaaa1111", nil)
		key := types.NamespacedName{Name: ws.Name, Namespace: ns}
		live := &cdiv1.WorkspaceTemplate{}
		if err := c.Get(context.Background(),
			client.ObjectKey{Namespace: ns, Name: "fam32-aaaa1111"}, live); err != nil {
			t.Fatal(err)
		}
		// An owned pod without identity stamps cannot prove its revision,
		// even when templateRef names the exact object it was built from;
		// its annotation is never a source.
		pod := seedRunning(t, ns, ws, live, "node-7") // scheduled pod
		delete(pod.Annotations, linux.AnnotationTemplateName)
		delete(pod.Annotations, linux.AnnotationTemplateRevision)
		delete(pod.Annotations, linux.AnnotationTemplateHash)
		if err := c.Update(context.Background(), pod); err != nil {
			t.Fatalf("unstamp pod: %v", err)
		}
		seedAnnotation(t, ws, snapshotAnnotation(t, live, "", 0))
		markPodReady(t, c, pod)
		r := newReconciler(c)
		reconcile(t, r, key)
		assertPodHeldGone(t, key, pod)
	})

	t.Run("unstamped pod under a re-pointed ref holds degraded", func(t *testing.T) {
		ns := newNamespace(t, c)
		familyRevision(t, c, ns, "fam32-aaaa1111", "2026-10-a", resnapImageA)
		ws := newWorkspace(t, c, ns, "ws-adoptr", "fam32-bbbb2222", nil)
		key := types.NamespacedName{Name: ws.Name, Namespace: ns}
		live := &cdiv1.WorkspaceTemplate{}
		if err := c.Get(context.Background(),
			client.ObjectKey{Namespace: ns, Name: "fam32-aaaa1111"}, live); err != nil {
			t.Fatal(err)
		}
		pod := seedRunning(t, ns, ws, live, "")
		markPodReady(t, c, pod)
		delete(pod.Annotations, linux.AnnotationTemplateName)
		delete(pod.Annotations, linux.AnnotationTemplateRevision)
		delete(pod.Annotations, linux.AnnotationTemplateHash)
		if err := c.Update(context.Background(), pod); err != nil {
			t.Fatalf("unstamp pod: %v", err)
		}
		// The pod was built from fam32-aaaa1111 but templateRef was
		// re-pointed at fam32-bbbb2222 — different pod content, so the pod
		// disproves the resolved candidate.
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
		live := &cdiv1.WorkspaceTemplate{
			ObjectMeta: metav1.ObjectMeta{
				Name: "tpl-live", Namespace: "tinycdi-tenant-a",
				UID: types.UID("dddddddd-0000-0000-0000-0000000000aa"),
			},
			Spec: cdiv1.WorkspaceTemplateSpec{
				Revision: "2026-10-b", Runtime: cdiv1.RuntimeLinuxContainer,
				Experience: cdiv1.ExperienceDesktop,
				Linux:      &cdiv1.LinuxRuntimeSpec{Image: resnapImageB},
				Resources: cdiv1.ResourceProfile{
					CPU: resource.MustParse("1"), Memory: resource.MustParse("2Gi"),
					Storage: resource.MustParse("5Gi"),
				},
				BootDeadline:   metav1.Duration{Duration: 5 * time.Minute},
				NetworkProfile: cdiv1.NetworkProfileIsolated,
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
		got := &cdiv1.Workspace{}
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
