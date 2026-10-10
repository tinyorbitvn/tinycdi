// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

// SEC-10 regression coverage: the
// operator must not trust the workspaces.cdi.tinyorbit.vn/template-snapshot
// annotation at all. The annotation is ordinary Workspace metadata — a
// principal with Workspace write can pre-seed or replace it — so
// convergence builds pods only from status.templateSnapshot, the record
// the operator wrote itself through the workspaces/status subresource
// (granted to the operator service account alone). The annotation can
// reach status only through the one-time upgrade-adoption proof (a live
// operator-owned pod verifiably built from it). A forged annotation must
// produce NO runtime children from its own spec.
package operator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
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
		cond.Reason != ReasonTemplateInvalid {
		t.Fatalf("want Degraded=True reason=%s, got %+v",
			ReasonTemplateInvalid, got.Status.Conditions)
	}
}

// A forged snapshot whose specHash does not cover the embedded
// spec — tag-only image, arbitrary command, control-plane nodeSelector.
func TestForgedSnapshotBadHashRejected(t *testing.T) {
	spec := forgedSpec("attacker.example/miner:latest")
	ann := forgedSnapshot("does-not-exist", "x", "x", "sha256:00", spec,
		map[string]string{linux.AnnotationSeccompProfile: `localhost/attacker`})
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
// template is overridden by an honest re-snapshot of the live object:
// the forged spec never converges, the pod is built from the template's
// own spec.
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

	// No pod may exist yet, and never one built from the forged spec: the
	// annotation is unadoptable without an operator-owned pod, so the
	// operator re-snapshots the LIVE template and converges on it.
	pod := &corev1.Pod{}
	err := c.Get(context.Background(), client.ObjectKey{
		Namespace: ws.Namespace, Name: linux.PodName(ws.UID)}, pod)
	if err != nil {
		t.Fatalf("honest re-snapshot produced no pod: %v", err)
	}
	if img := pod.Spec.Containers[0].Image; img != live.Spec.Linux.Image {
		t.Fatalf("pod image %q — the forged spec converged", img)
	}
	if got.Status.TemplateSnapshot == nil {
		t.Fatal("re-snapshot did not record status.templateSnapshot")
	}
	want, err := snapshotTemplate(live)
	if err != nil {
		t.Fatalf("snapshotTemplate(live): %v", err)
	}
	if got.Status.TemplateSnapshot.SpecHash != want.SpecHash ||
		got.Status.TemplateSnapshot.Name != "tpl-live" {
		t.Fatalf("status snapshot = %+v, want honest record of tpl-live",
			got.Status.TemplateSnapshot)
	}
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

// The adapter/sessionCmd CEL rules on LinuxRuntimeSpec are part of the
// contract a forged snapshot must re-satisfy: a snapshot embedding
// sessionCmd without adapter=kasm, an unknown adapter value, a command
// alongside adapter=kasm, or a sessionCmd violating the printable-ASCII
// pattern must all be rejected.
func TestForgedSnapshotAdapterInvariantsRejected(t *testing.T) {
	img := "cr.example/img@sha256:" + fmt.Sprintf("%064x", 7)
	for _, tc := range []struct {
		name   string
		mutate func(spec *workspacesv1alpha1.WorkspaceTemplateSpec)
	}{
		{"sessionCmd without adapter", func(s *workspacesv1alpha1.WorkspaceTemplateSpec) {
			s.Linux.Command = nil
			s.Linux.SessionCmd = "xterm"
		}},
		{"unknown adapter", func(s *workspacesv1alpha1.WorkspaceTemplateSpec) {
			s.Linux.Command = nil
			s.Linux.Adapter = "docker"
		}},
		{"command with kasm adapter", func(s *workspacesv1alpha1.WorkspaceTemplateSpec) {
			s.Linux.Adapter = workspacesv1alpha1.AdapterKasm // Command already set by forgedSpec
		}},
		{"sessionCmd with newline", func(s *workspacesv1alpha1.WorkspaceTemplateSpec) {
			s.Linux.Command = nil
			s.Linux.Adapter = workspacesv1alpha1.AdapterKasm
			s.Linux.SessionCmd = "xterm\nid"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := forgedSpec(img)
			tc.mutate(&spec)
			raw, _ := json.Marshal(spec)
			sum := sha256.Sum256(raw)
			ann := forgedSnapshot("does-not-exist", "x", "x",
				"sha256:"+hex.EncodeToString(sum[:]), spec, nil)
			ws := forgedWorkspace(ann)
			c, got := reconcileForged(t, ws)
			assertSnapshotRejected(t, c, ws, got)
		})
	}
}

// And the same rules do not block an honest kasm snapshot: a valid
// adapter=kasm + sessionCmd spec verifies and converges (the reconciler's
// backend here is configured without a kasm adapter image, so convergence
// itself stops at ErrTemplateRejected — the snapshot verification is the
// layer under test and it must PASS the spec).
func TestValidateSnapshotSpecKasmAdapter(t *testing.T) {
	img := "cr.example/img@sha256:" + fmt.Sprintf("%064x", 7)
	ok := forgedSpec(img)
	ok.Linux.Command = nil
	ok.Linux.Adapter = workspacesv1alpha1.AdapterKasm
	ok.Linux.SessionCmd = "/usr/bin/chromium-orig --start-maximized"
	if err := validateSnapshotSpec(&ok); err != nil {
		t.Fatalf("valid kasm spec rejected: %v", err)
	}
}

// A fully VALID forged snapshot — correct specHash over a spec that
// satisfies every CRD/CEL invariant (digest-pinned image, kasm rules),
// self-consistent sourceRef/runtimeGeneration — naming a template that was
// never published. This is the structurally-valid forge the old
// provenance check could not catch (template NotFound returned early
// success): attacker-chosen image, command, control-plane placement,
// runtimeClass and unbounded resources. Under the status-authoritative
// model it is simply untrusted: no status record exists, the annotation
// is never adopted without a proving pod, and the unresolvable reference
// holds the workspace Degraded with NO pod.
func TestForgedSnapshotStructurallyValidRejected(t *testing.T) {
	spec := forgedSpec("attacker.example/miner@sha256:" + fmt.Sprintf("%064x", 3))
	// Attacker-chosen placement: land the pod on control-plane nodes.
	spec.Placement = &workspacesv1alpha1.PlacementSpec{
		NodeSelector: map[string]string{"node-role.kubernetes.io/control-plane": ""},
		Tolerations: []corev1.Toleration{{
			Key:      "node-role.kubernetes.io/control-plane",
			Operator: corev1.TolerationOpExists,
			Effect:   corev1.TaintEffectNoSchedule,
		}},
	}
	rc := "kata-containers"
	spec.Placement.RuntimeClassName = &rc
	spec.NetworkProfile = workspacesv1alpha1.NetworkProfileInternetOnly
	raw, _ := json.Marshal(spec)
	sum := sha256.Sum256(raw)
	ann := forgedSnapshot("never-published", "ffffffff-0000-0000-0000-000000000001",
		spec.Revision, "sha256:"+hex.EncodeToString(sum[:]), spec,
		map[string]string{linux.AnnotationSeccompProfile: "localhost/attacker-loaded"})

	ws := forgedWorkspace(ann)
	// The forger keeps the snapshot self-consistent with the spec it
	// plants on the CR: SourceRef must equal spec.templateRef.name and the
	// recorded generation must cover spec.runtimeGeneration.
	var parsed templateSnapshot
	if err := json.Unmarshal([]byte(ann), &parsed); err != nil {
		t.Fatal(err)
	}
	parsed.SourceRef = ws.Spec.TemplateRef.Name
	parsed.RuntimeGeneration = ws.Spec.RuntimeGeneration
	rawAnn, _ := json.Marshal(&parsed)
	ws.Annotations[AnnotationTemplateSnapshot] = string(rawAnn)

	c, got := reconcileForged(t, ws)
	assertSnapshotRejected(t, c, ws, got)
	if got.Status.TemplateSnapshot != nil {
		t.Fatalf("forged annotation was adopted into status: %+v",
			got.Status.TemplateSnapshot)
	}
}

// The held workspace emits exactly one Warning event on the transition
// into TemplateInvalid — the hold requeues and reconciles again, but the
// transition fires once, not per pass.
func TestTemplateInvalidEventEmittedOnce(t *testing.T) {
	spec := forgedSpec("attacker.example/miner@sha256:" + fmt.Sprintf("%064x", 3))
	raw, _ := json.Marshal(spec)
	sum := sha256.Sum256(raw)
	ann := forgedSnapshot("never-published", "x", "x",
		"sha256:"+hex.EncodeToString(sum[:]), spec, nil)
	ws := forgedWorkspace(ann)

	s := snapScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(ws).WithStatusSubresource(ws).Build()
	rec := record.NewFakeRecorder(16)
	r := &WorkspaceReconciler{Client: c, Scheme: s,
		Backend: linux.New(c, linux.Options{}), Recorder: rec}
	key := client.ObjectKeyFromObject(ws)
	for i := 0; i < 4; i++ {
		if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	}
	select {
	case ev := <-rec.Events:
		if !strings.Contains(ev, ReasonTemplateInvalid) {
			t.Fatalf("event %q, want reason %s", ev, ReasonTemplateInvalid)
		}
	default:
		t.Fatal("no TemplateInvalid event emitted")
	}
	select {
	case ev := <-rec.Events:
		t.Fatalf("second event emitted on a repeated hold: %q", ev)
	default:
	}
}

// The status copy is the source of truth: a workspace whose
// status.templateSnapshot the operator recorded still converges on it
// even when the annotation is replaced by a forgery — the mirror is
// repaired, never read.
func TestStatusSnapshotAuthoritativeOverForgedAnnotation(t *testing.T) {
	tpl := &workspacesv1alpha1.WorkspaceTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name: "tpl-honest", Namespace: "tinycdi-tenant-a",
			UID: types.UID("eeeeeeee-0000-0000-0000-000000000002"),
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
	honest, err := snapshotTemplate(tpl)
	if err != nil {
		t.Fatalf("snapshotTemplate: %v", err)
	}
	honest.RuntimeGeneration = 1
	honest.SourceRef = "tpl-honest"

	// Forged annotation with control-plane placement claims the same
	// source — it must lose to the status record and be overwritten.
	spec := forgedSpec("attacker.example/miner@sha256:" + fmt.Sprintf("%064x", 3))
	spec.Placement = &workspacesv1alpha1.PlacementSpec{
		NodeSelector: map[string]string{"node-role.kubernetes.io/control-plane": ""},
	}
	fraw, _ := json.Marshal(spec)
	fsum := sha256.Sum256(fraw)
	forged := &templateSnapshot{
		Name: "tpl-honest", UID: "ffffffff-0000-0000-0000-000000000009",
		Revision: "2026-09-a", SpecHash: "sha256:" + hex.EncodeToString(fsum[:]),
		Spec: spec, RuntimeGeneration: 1, SourceRef: "tpl-honest",
	}
	forgedRaw, _ := json.Marshal(forged)

	ws := forgedWorkspace(string(forgedRaw))
	ws.Spec.TemplateRef.Name = "tpl-honest"
	ws.Status.TemplateSnapshot = honest
	c, got := reconcileForged(t, ws)

	pod := &corev1.Pod{}
	if err := c.Get(context.Background(), client.ObjectKey{
		Namespace: ws.Namespace, Name: linux.PodName(ws.UID)}, pod); err != nil {
		t.Fatalf("status snapshot blocked convergence: %v", err)
	}
	if img := pod.Spec.Containers[0].Image; img != tpl.Spec.Linux.Image {
		t.Fatalf("pod image %q, want status-recorded %q — the forged annotation drove convergence",
			img, tpl.Spec.Linux.Image)
	}
	wantAnn, _ := json.Marshal(honest)
	if got.Annotations[AnnotationTemplateSnapshot] != string(wantAnn) {
		t.Fatal("forged annotation was not repaired to the status mirror")
	}
}

// First admit writes the snapshot to status (the trusted copy) and
// mirrors it to the annotation for readers — the operator builds the
// record from the live template, never from caller-supplied bytes.
func TestStatusSnapshotRecordedAtFirstAdmit(t *testing.T) {
	tpl := &workspacesv1alpha1.WorkspaceTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name: "tpl-admit", Namespace: "tinycdi-tenant-a",
			UID:         types.UID("eeeeeeee-0000-0000-0000-000000000003"),
			Annotations: map[string]string{linux.AnnotationSeccompProfile: "localhost/browser-sandbox"},
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
	ws := forgedWorkspace("")
	delete(ws.Annotations, AnnotationTemplateSnapshot)
	ws.Spec.TemplateRef.Name = "tpl-admit"
	c, got := reconcileForged(t, ws, tpl)

	snap := got.Status.TemplateSnapshot
	if snap == nil {
		t.Fatal("status.templateSnapshot not recorded at first admit")
	}
	want, err := snapshotTemplate(tpl)
	if err != nil {
		t.Fatalf("snapshotTemplate: %v", err)
	}
	if snap.Name != want.Name || snap.UID != want.UID || snap.SpecHash != want.SpecHash ||
		snap.SourceRef != "tpl-admit" || snap.RuntimeGeneration != 1 ||
		snap.Annotations[linux.AnnotationSeccompProfile] != "localhost/browser-sandbox" {
		t.Fatalf("recorded snapshot = %+v, want honest record of tpl-admit", snap)
	}
	rawAnn, _ := json.Marshal(snap)
	if got.Annotations[AnnotationTemplateSnapshot] != string(rawAnn) {
		t.Fatal("annotation does not mirror the status record")
	}
	pod := &corev1.Pod{}
	if err := c.Get(context.Background(), client.ObjectKey{
		Namespace: ws.Namespace, Name: linux.PodName(ws.UID)}, pod); err != nil {
		t.Fatalf("no pod from recorded snapshot: %v", err)
	}
	if pod.Annotations[linux.AnnotationTemplateHash] == "" {
		t.Fatal("pod missing the template-hash stamp")
	}
}

// spec.placement is hashed into the snapshot: two template revisions that
// differ only in placement must not share a specHash — placement is spec
// (immutable, admin-only), not metadata (D24).
func TestSnapshot_IncludesPlacement(t *testing.T) {
	tpl := &workspacesv1alpha1.WorkspaceTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "tpl-place", UID: types.UID("u-place")},
		Spec:       forgedSpec("tcdi/linux-desktop@sha256:" + fmt.Sprintf("%064x", 8)),
	}
	plain, err := snapshotTemplate(tpl)
	if err != nil {
		t.Fatalf("snapshotTemplate: %v", err)
	}
	placed := tpl.DeepCopy()
	placed.Spec.Placement = &workspacesv1alpha1.PlacementSpec{
		NodeSelector: map[string]string{"workload": "runtime"},
	}
	snapPlaced, err := snapshotTemplate(placed)
	if err != nil {
		t.Fatalf("snapshotTemplate: %v", err)
	}
	if plain.SpecHash == snapPlaced.SpecHash {
		t.Fatal("specHash unchanged after spec.placement was added — the snapshot no longer covers the full spec")
	}
	if err := validateSnapshotSpec(&placed.Spec); err != nil {
		t.Fatalf("a spec with placement must still satisfy snapshot invariants: %v", err)
	}
}
