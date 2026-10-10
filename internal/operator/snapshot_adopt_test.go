// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

// Upgrade-adoption coverage for status.templateSnapshot. A workspace
// admitted before the status field existed establishes its record from
// the RUNNING incarnation, never from the annotation: the operator-owned
// pod names its template revision in stamped metadata
// (workspaces.cdi.tinyorbit.vn/template-name|template-revision), the
// LIVE revision object supplies the content, and the pod must provably
// derive from it (template-hash stamp, or an exact spec rebuild for
// stamp-less pods). A pod that cannot name its revision — pre-stamp
// pods, pruned or republished objects, mismatched builds — holds the
// workspace Degraded/TemplateRevisionGone with the pod left running;
// a workspace with no pod re-snapshots the live template at next start.
// The annotation is never consulted.
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
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	workspacesv1alpha1 "github.com/tinyorbitvn/tinycdi/api/v1alpha1"
	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
	linux "github.com/tinyorbitvn/tinycdi/internal/runtime/linux"
)

// adoptTemplate builds the revision object a v0.5.0 workspace would have
// been admitted under — catalog-name labeled fam32, the way published
// revisions carry their family.
func adoptTemplate(ns, name string, uid types.UID) *workspacesv1alpha1.WorkspaceTemplate {
	return &workspacesv1alpha1.WorkspaceTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: ns, UID: uid,
			Labels:      map[string]string{provisioning.LabelCatalogName: "fam32"},
			Annotations: map[string]string{linux.AnnotationSeccompProfile: "localhost/browser-sandbox"},
		},
		Spec: workspacesv1alpha1.WorkspaceTemplateSpec{
			Revision: "2026-10-a", Runtime: workspacesv1alpha1.RuntimeLinuxContainer,
			Experience: workspacesv1alpha1.ExperienceDesktop,
			Linux: &workspacesv1alpha1.LinuxRuntimeSpec{
				Image: "cr.example/img@sha256:" + fmt.Sprintf("%064x", 0xa1),
			},
			Resources: workspacesv1alpha1.ResourceProfile{
				CPU: resource.MustParse("1"), Memory: resource.MustParse("2Gi"),
				Storage: resource.MustParse("5Gi"),
			},
			BootDeadline:   metav1.Duration{Duration: 5 * time.Minute},
			NetworkProfile: workspacesv1alpha1.NetworkProfileIsolated,
		},
	}
}

// adoptWorkspace seeds a Running workspace in the upgrade-time shape —
// no status.templateSnapshot. ann, when non-empty, seeds the
// (never-trusted) snapshot annotation.
func adoptWorkspace(ref, ann string, uid types.UID) *workspacesv1alpha1.Workspace {
	annotations := map[string]string{}
	if ann != "" {
		annotations[AnnotationTemplateSnapshot] = ann
	}
	return &workspacesv1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ws-pre", Namespace: "tinycdi-tenant-a", UID: uid,
			Labels:      map[string]string{"workspaces.cdi.tinyorbit.vn/workspace-uid": "ws_pre"},
			Annotations: annotations,
		},
		Spec: workspacesv1alpha1.WorkspaceSpec{
			TemplateRef:       workspacesv1alpha1.TemplateReference{Name: ref},
			OwnerSubject:      workspacesv1alpha1.OwnerSubject{Issuer: "i", Subject: "s"},
			DesiredState:      workspacesv1alpha1.DesiredStateRunning,
			DataPolicy:        workspacesv1alpha1.DataPolicyEphemeral,
			RuntimeGeneration: 1,
			IntentRevision:    1,
		},
	}
}

// snapshotAnnotation renders the annotation bytes a v0.5.0 operator
// recorded — used to prove the annotation is never read, and to seed the
// legacy (pre-V3.2, no sourceRef/runtimeGeneration) shape.
func snapshotAnnotation(t *testing.T, tpl *workspacesv1alpha1.WorkspaceTemplate, ref string, gen int64) string {
	t.Helper()
	s, err := snapshotTemplate(tpl)
	if err != nil {
		t.Fatalf("snapshotTemplate: %v", err)
	}
	s.SourceRef = ref
	s.RuntimeGeneration = gen
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	return string(raw)
}

// seedBuiltPod lets the real backend build the workspace's pod from tpl
// (as the recording operator would have — stamped with the template's
// identity + hash). The stored pod is returned.
func seedBuiltPod(t *testing.T, c client.Client, ws *workspacesv1alpha1.Workspace,
	tpl *workspacesv1alpha1.WorkspaceTemplate) *corev1.Pod {
	t.Helper()
	b := linux.New(c, linux.Options{})
	if _, err := b.Ensure(context.Background(), ws, tpl); err != nil {
		t.Fatalf("seed pod via Ensure: %v", err)
	}
	pod := &corev1.Pod{}
	if err := c.Get(context.Background(), client.ObjectKey{
		Namespace: ws.Namespace, Name: linux.PodName(ws.UID)}, pod); err != nil {
		t.Fatalf("seeded pod missing: %v", err)
	}
	return pod
}

// adoptReconcile runs the seeded workspace through one reconcile and
// returns the stored object.
func adoptReconcile(t *testing.T, s *runtime.Scheme, c client.Client,
	ws *workspacesv1alpha1.Workspace) *workspacesv1alpha1.Workspace {
	t.Helper()
	r := &WorkspaceReconciler{Client: c, Scheme: s,
		Backend: linux.New(c, linux.Options{})}
	reconcile(t, r, client.ObjectKeyFromObject(ws))
	return getWorkspace(t, c, client.ObjectKeyFromObject(ws))
}

// A running workspace whose stamped pod names a live revision object
// adopts THAT object's content into status — the pod is undisturbed,
// scheduled or not.
func TestAdoptSnapshot_StampedPodAdoptsLiveRevision(t *testing.T) {
	for _, node := range []string{"", "node-7"} {
		t.Run(fmt.Sprintf("nodeName=%q", node), func(t *testing.T) {
			s := snapScheme(t)
			tpl := adoptTemplate("tinycdi-tenant-a", "fam32-aaaa1111", types.UID("uid-a"))
			ws := adoptWorkspace("fam32", "", types.UID("ws-pre-1"))
			c := fake.NewClientBuilder().WithScheme(s).
				WithObjects(ws, tpl).WithStatusSubresource(ws).Build()
			pod := seedBuiltPod(t, c, ws, tpl)
			if node != "" {
				// The pod is scheduled: apiserver/cluster bookkeeping the
				// builder never wrote must not break the proof.
				pod.Spec.NodeName = node
				pod.Spec.PriorityClassName = "system-cluster-critical"
				pod.Spec.DeprecatedServiceAccount = "tcdi-workspace"
				if err := c.Update(context.Background(), pod); err != nil {
					t.Fatalf("mark pod scheduled: %v", err)
				}
			}

			got := adoptReconcile(t, s, c, ws)
			snap := got.Status.TemplateSnapshot
			if snap == nil {
				t.Fatal("stamped pod's revision was not adopted into status")
			}
			want, err := snapshotTemplate(tpl)
			if err != nil {
				t.Fatalf("snapshotTemplate: %v", err)
			}
			if snap.Name != want.Name || snap.UID != want.UID || snap.SpecHash != want.SpecHash ||
				snap.RuntimeGeneration != 1 || snap.SourceRef != "fam32" {
				t.Fatalf("adopted snapshot = %+v, want record of fam32-aaaa1111 gen 1", snap)
			}
			cur := &corev1.Pod{}
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(pod), cur); err != nil {
				t.Fatalf("pod vanished after adoption: %v", err)
			}
			if cur.UID != pod.UID {
				t.Fatal("adoption recreated the pod")
			}
		})
	}
}

// A stamped-name pod missing the hash stamp falls back to the exact
// spec-rebuild proof — the F-A fix: scheduler- and apiserver-injected
// fields (nodeName, priorityClassName, deprecatedServiceAccount) must not
// break it.
func TestAdoptSnapshot_ScheduledUnstampedHashAdopts(t *testing.T) {
	s := snapScheme(t)
	tpl := adoptTemplate("tinycdi-tenant-a", "fam32-aaaa1111", types.UID("uid-a"))
	ws := adoptWorkspace("fam32", "", types.UID("ws-pre-9"))
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(ws, tpl).WithStatusSubresource(ws).Build()
	pod := seedBuiltPod(t, c, ws, tpl)
	delete(pod.Annotations, linux.AnnotationTemplateHash)
	pod.Spec.NodeName = "ip-10-0-1-7.compute.internal"
	pod.Spec.PriorityClassName = "system-cluster-critical"
	pod.Spec.DeprecatedServiceAccount = "tcdi-workspace"
	if err := c.Update(context.Background(), pod); err != nil {
		t.Fatalf("mark pod scheduled: %v", err)
	}
	got := adoptReconcile(t, s, c, ws)
	if got.Status.TemplateSnapshot == nil {
		t.Fatal("scheduled hash-less stamped pod was not adopted")
	}
	want, _ := snapshotTemplate(tpl)
	if got.Status.TemplateSnapshot.SpecHash != want.SpecHash {
		t.Fatalf("adopted %q, want live revision %q",
			got.Status.TemplateSnapshot.SpecHash, want.SpecHash)
	}
}

// An annotation — honest, forged, or legacy-shaped — is never consulted:
// the stamped pod's identity decides everything.
func TestAdoptSnapshot_AnnotationNeverConsulted(t *testing.T) {
	s := snapScheme(t)
	tpl := adoptTemplate("tinycdi-tenant-a", "fam32-aaaa1111", types.UID("uid-a"))

	forged := forgedSpec("attacker.example/miner@sha256:" + fmt.Sprintf("%064x", 3))
	forged.Revision = "2026-10-a"
	forged.Placement = &workspacesv1alpha1.PlacementSpec{
		NodeSelector: map[string]string{"node-role.kubernetes.io/control-plane": ""},
	}
	fraw, _ := json.Marshal(forged)
	fsum := sha256.Sum256(fraw)
	forgedRaw, _ := json.Marshal(&templateSnapshot{
		Name: "fam32-aaaa1111", UID: "uid-a", Revision: "2026-10-a",
		SpecHash: "sha256:" + hex.EncodeToString(fsum[:]), Spec: forged,
		RuntimeGeneration: 1, SourceRef: "fam32",
	})

	// Pre-V3.2 legacy shape — no sourceRef/runtimeGeneration at all.
	var legacy templateSnapshot
	if err := json.Unmarshal([]byte(snapshotAnnotation(t, tpl, "", 0)), &legacy); err != nil {
		t.Fatal(err)
	}
	legacyRaw, _ := json.Marshal(&legacy)

	for name, ann := range map[string]string{
		"forged spec":           string(forgedRaw),
		"legacy shape":          string(legacyRaw),
		"honest current record": snapshotAnnotation(t, tpl, "fam32", 1),
	} {
		t.Run(name, func(t *testing.T) {
			ws := adoptWorkspace("fam32", ann, types.UID("ws-pre-2-"+name[:3]))
			c := fake.NewClientBuilder().WithScheme(s).
				WithObjects(ws, tpl).WithStatusSubresource(ws).Build()
			seedBuiltPod(t, c, ws, tpl)
			got := adoptReconcile(t, s, c, ws)
			snap := got.Status.TemplateSnapshot
			if snap == nil {
				t.Fatal("stamped pod's revision was not adopted")
			}
			want, _ := snapshotTemplate(tpl)
			if snap.SpecHash != want.SpecHash {
				t.Fatalf("adopted spec %q, want live revision %q — annotation bytes leaked",
					snap.SpecHash, want.SpecHash)
			}
		})
	}
}

// A forged annotation flipping networkProfile to InternetOnly must not
// ride adoption: the recorded revision keeps its own profile and the
// boundary NetworkPolicy is left byte-for-byte unchanged.
func TestAdoptSnapshot_ForgedNetworkProfileLeavesNetPol(t *testing.T) {
	s := snapScheme(t)
	tpl := adoptTemplate("tinycdi-tenant-a", "fam32-aaaa1111", types.UID("uid-a"))
	forged := forgedSpec("attacker.example/miner@sha256:" + fmt.Sprintf("%064x", 3))
	forged.Revision = "2026-10-a"
	forged.NetworkProfile = workspacesv1alpha1.NetworkProfileInternetOnly
	fraw, _ := json.Marshal(forged)
	fsum := sha256.Sum256(fraw)
	forgedRaw, _ := json.Marshal(&templateSnapshot{
		Name: "fam32-aaaa1111", UID: "uid-a", Revision: "2026-10-a",
		SpecHash: "sha256:" + hex.EncodeToString(fsum[:]), Spec: forged,
		RuntimeGeneration: 1, SourceRef: "fam32",
	})
	ws := adoptWorkspace("fam32", string(forgedRaw), types.UID("ws-pre-3"))
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(ws, tpl).WithStatusSubresource(ws).Build()
	seedBuiltPod(t, c, ws, tpl)

	before := &networkingv1.NetworkPolicy{}
	if err := c.Get(context.Background(), client.ObjectKey{
		Namespace: ws.Namespace, Name: linux.NetPolName(linux.CRUID(ws.UID))}, before); err != nil {
		t.Fatalf("seeded netpol: %v", err)
	}
	beforeJSON, _ := json.Marshal(before.Spec)

	got := adoptReconcile(t, s, c, ws)
	if got.Status.TemplateSnapshot == nil ||
		got.Status.TemplateSnapshot.Spec.NetworkProfile != workspacesv1alpha1.NetworkProfileIsolated {
		t.Fatalf("forged profile adopted: %+v", got.Status.TemplateSnapshot)
	}
	after := &networkingv1.NetworkPolicy{}
	if err := c.Get(context.Background(), client.ObjectKey{
		Namespace: ws.Namespace, Name: linux.NetPolName(linux.CRUID(ws.UID))}, after); err != nil {
		t.Fatalf("netpol vanished: %v", err)
	}
	afterJSON, _ := json.Marshal(after.Spec)
	if string(beforeJSON) != string(afterJSON) {
		t.Fatal("boundary NetworkPolicy changed — forged spec converged")
	}
}

// The stamped identity names an object that is gone — the revision was
// pruned — or was republished under a different revision string: the pod
// keeps running, the workspace holds Degraded/TemplateRevisionGone.
func TestAdoptSnapshot_RevisionGone(t *testing.T) {
	s := snapScheme(t)
	tpl := adoptTemplate("tinycdi-tenant-a", "fam32-aaaa1111", types.UID("uid-a"))

	t.Run("object pruned", func(t *testing.T) {
		ws := adoptWorkspace("fam32", "", types.UID("ws-pre-4"))
		c := fake.NewClientBuilder().WithScheme(s).WithObjects(ws).WithStatusSubresource(ws).Build()
		pod := seedBuiltPod(t, c, ws, tpl) // stamped fam32-aaaa1111, but the object is absent
		got := adoptReconcile(t, s, c, ws)
		assertRevisionGone(t, got)
		cur := &corev1.Pod{}
		if err := c.Get(context.Background(), client.ObjectKeyFromObject(pod), cur); err != nil {
			t.Fatalf("pod vanished on revision-gone hold: %v", err)
		}
	})

	t.Run("object republished", func(t *testing.T) {
		other := adoptTemplate("tinycdi-tenant-a", "fam32-aaaa1111", types.UID("uid-b"))
		other.Spec.Revision = "2026-11-z"
		ws := adoptWorkspace("fam32", "", types.UID("ws-pre-5"))
		c := fake.NewClientBuilder().WithScheme(s).WithObjects(ws, other).WithStatusSubresource(ws).Build()
		seedBuiltPod(t, c, ws, tpl) // pod stamps revision 2026-10-a
		got := adoptReconcile(t, s, c, ws)
		assertRevisionGone(t, got)
	})
}

// unstamp removes the identity stamps — the v0.5 pod shape.
func unstamp(t *testing.T, c client.Client, pod *corev1.Pod) {
	t.Helper()
	delete(pod.Annotations, linux.AnnotationTemplateName)
	delete(pod.Annotations, linux.AnnotationTemplateRevision)
	delete(pod.Annotations, linux.AnnotationTemplateHash)
	if err := c.Update(context.Background(), pod); err != nil {
		t.Fatalf("unstamp pod: %v", err)
	}
}

// The pre-stamp candidate match: a pod built before the identity stamps
// names nothing, so the candidate is the live revision spec.templateRef
// resolves to. A pod that provably was built from it adopts the live
// object into status — and gets its identity stamps backfilled — with no
// Degraded and no restart. Scheduled or not.
func TestAdoptSnapshot_UnstampedPodCandidateMatch(t *testing.T) {
	for _, node := range []string{"", "node-9"} {
		t.Run(fmt.Sprintf("nodeName=%q", node), func(t *testing.T) {
			s := snapScheme(t)
			tpl := adoptTemplate("tinycdi-tenant-a", "fam32-aaaa1111", types.UID("uid-a"))
			ws := adoptWorkspace("fam32", "", types.UID("ws-pre-6"+node))
			c := fake.NewClientBuilder().WithScheme(s).
				WithObjects(ws, tpl).WithStatusSubresource(ws).Build()
			pod := seedBuiltPod(t, c, ws, tpl)
			unstamp(t, c, pod)
			if node != "" {
				pod.Spec.NodeName = node
				pod.Spec.PriorityClassName = "system-cluster-critical"
				pod.Spec.DeprecatedServiceAccount = "tcdi-workspace"
				if err := c.Update(context.Background(), pod); err != nil {
					t.Fatalf("mark pod scheduled: %v", err)
				}
			}

			got := adoptReconcile(t, s, c, ws)
			snap := got.Status.TemplateSnapshot
			if snap == nil {
				t.Fatal("pre-stamp pod's resolved revision was not adopted")
			}
			want, err := snapshotTemplate(tpl)
			if err != nil {
				t.Fatalf("snapshotTemplate: %v", err)
			}
			if snap.SpecHash != want.SpecHash || snap.Name != "fam32-aaaa1111" {
				t.Fatalf("adopted %q/%q, want live revision fam32-aaaa1111", snap.Name, snap.SpecHash)
			}
			cond := condition(got, workspacesv1alpha1.ConditionDegraded)
			if cond != nil && cond.Status == metav1.ConditionTrue {
				t.Fatalf("Degraded set on a successful candidate match: %+v", cond)
			}
			// Identity stamps were backfilled onto the still-running pod.
			cur := &corev1.Pod{}
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(pod), cur); err != nil {
				t.Fatalf("pod vanished: %v", err)
			}
			if cur.UID != pod.UID {
				t.Fatal("adoption recreated the pod")
			}
			if cur.Annotations[linux.AnnotationTemplateName] != "fam32-aaaa1111" ||
				cur.Annotations[linux.AnnotationTemplateRevision] != "2026-10-a" {
				t.Fatalf("identity stamps not backfilled: %+v", cur.Annotations)
			}
		})
	}
}

// A pre-stamp pod that does NOT equal the resolved candidate's rebuild
// holds Degraded/TemplateRevisionGone — the pod stays running, never
// disproved-by-silence. Covers rotated-out revisions, pods built from a
// forged spec, and webhook-altered pods.
func TestAdoptSnapshot_UnstampedPodCandidateMismatch(t *testing.T) {
	s := snapScheme(t)
	tpl := adoptTemplate("tinycdi-tenant-a", "fam32-aaaa1111", types.UID("uid-a"))

	forgedBuilt := adoptTemplate("tinycdi-tenant-a", "fam32-aaaa1111", types.UID("uid-a"))
	forgedBuilt.Spec.Linux.Image = "attacker.example/miner@sha256:" + fmt.Sprintf("%064x", 4)
	forgedBuilt.Spec.Placement = &workspacesv1alpha1.PlacementSpec{
		NodeSelector: map[string]string{"node-role.kubernetes.io/control-plane": ""},
	}

	cases := []struct {
		name     string
		podTpl   *workspacesv1alpha1.WorkspaceTemplate // what the pod was built from
		liveTpl  *workspacesv1alpha1.WorkspaceTemplate // nil: family pruned
		mungePod func(pod *corev1.Pod)
	}{
		{name: "forged-spec pod mismatch", podTpl: forgedBuilt, liveTpl: tpl},
		{name: "webhook-altered pod mismatch", podTpl: tpl, liveTpl: tpl,
			mungePod: func(pod *corev1.Pod) {
				pod.Spec.Containers[0].Env = append(pod.Spec.Containers[0].Env,
					corev1.EnvVar{Name: "INJECTED_BY_WEBHOOK", Value: "1"})
			}},
		{name: "template pruned entirely", podTpl: tpl},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ws := adoptWorkspace("fam32", "", types.UID(fmt.Sprintf("ws-mm-%d", i)))
			objs := []client.Object{ws}
			if tc.liveTpl != nil {
				objs = append(objs, tc.liveTpl)
			}
			c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).WithStatusSubresource(ws).Build()
			pod := seedBuiltPod(t, c, ws, tc.podTpl)
			unstamp(t, c, pod)
			if tc.mungePod != nil {
				tc.mungePod(pod)
				if err := c.Update(context.Background(), pod); err != nil {
					t.Fatalf("munge pod: %v", err)
				}
			}
			got := adoptReconcile(t, s, c, ws)
			assertRevisionGone(t, got)
			cur := &corev1.Pod{}
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(pod), cur); err != nil {
				t.Fatalf("pod vanished on hold: %v", err)
			}
		})
	}
}

// Rotated case (1): the family now resolves a revision whose built pod
// differs — the pod disproves the candidate and holds.
func TestAdoptSnapshot_UnstampedPodRotatedMismatch(t *testing.T) {
	s := snapScheme(t)
	tpl := adoptTemplate("tinycdi-tenant-a", "fam32-aaaa1111", types.UID("uid-a"))
	ws := adoptWorkspace("fam32", "", types.UID("ws-mm-rot"))
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(ws, tpl).WithStatusSubresource(ws).Build()
	pod := seedBuiltPod(t, c, ws, tpl)
	unstamp(t, c, pod)
	// Rotate: the resolved revision now differs from what the pod was
	// built from (spec is immutable — a republished object under the
	// resolved name carries the new content).
	rotated := tpl.DeepCopy()
	rotated.Spec.Linux.Image = "cr.example/img@sha256:" + fmt.Sprintf("%064x", 0xb2)
	if err := c.Update(context.Background(), rotated); err != nil {
		t.Fatalf("rotate revision: %v", err)
	}
	got := adoptReconcile(t, s, c, ws)
	assertRevisionGone(t, got)
	cur := &corev1.Pod{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(pod), cur); err != nil {
		t.Fatalf("pod vanished on hold: %v", err)
	}
	if cur.UID != pod.UID {
		t.Fatal("hold recreated the pod")
	}
}

// The rotated-but-pod-identical corner: spec.templateRef resolves a
// NEWER revision object whose built pod is field-identical — the record
// silently re-points to the live object (the pod cannot distinguish
// revisions that build the same pod, and the live object is trusted).
func TestAdoptSnapshot_UnstampedPodRotatedIdentical(t *testing.T) {
	s := snapScheme(t)
	old := adoptTemplate("tinycdi-tenant-a", "fam32-aaaa1111", types.UID("uid-a"))
	// The resolved object: same pod-shaping content, newer revision name.
	newer := adoptTemplate("tinycdi-tenant-a", "fam32-bbbb2222", types.UID("uid-b"))
	newer.Spec.Revision = "2026-10-b"
	ws := adoptWorkspace("fam32-bbbb2222", "", types.UID("ws-rot"))
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(ws, newer).WithStatusSubresource(ws).Build()
	pod := seedBuiltPod(t, c, ws, old)
	unstamp(t, c, pod)

	got := adoptReconcile(t, s, c, ws)
	snap := got.Status.TemplateSnapshot
	if snap == nil || snap.Name != "fam32-bbbb2222" {
		t.Fatalf("pod-identical newer revision was not adopted: %+v", snap)
	}
}

// No pod: the workspace re-snapshots the live template like a fresh
// admit — or holds Degraded when nothing resolves.
func TestAdoptSnapshot_RequiresPod(t *testing.T) {
	t.Run("live template re-snapshots", func(t *testing.T) {
		s := snapScheme(t)
		tpl := adoptTemplate("tinycdi-tenant-a", "fam32-aaaa1111", types.UID("uid-a"))
		ws := adoptWorkspace("fam32-aaaa1111",
			snapshotAnnotation(t, tpl, "fam32-aaaa1111", 1), types.UID("ws-pre-7"))
		c := fake.NewClientBuilder().WithScheme(s).
			WithObjects(ws, tpl).WithStatusSubresource(ws).Build()
		got := adoptReconcile(t, s, c, ws)
		if got.Status.TemplateSnapshot == nil {
			t.Fatal("no status snapshot after re-snapshot of live template")
		}
	})

	t.Run("unresolvable template holds degraded", func(t *testing.T) {
		s := snapScheme(t)
		tpl := adoptTemplate("tinycdi-tenant-a", "fam32-aaaa1111", types.UID("uid-a"))
		ws := adoptWorkspace("fam32-aaaa1111",
			snapshotAnnotation(t, tpl, "fam32-aaaa1111", 1), types.UID("ws-pre-8"))
		c := fake.NewClientBuilder().WithScheme(s).
			WithObjects(ws).WithStatusSubresource(ws).Build()
		got := adoptReconcile(t, s, c, ws)
		cond := condition(got, workspacesv1alpha1.ConditionDegraded)
		if cond == nil || cond.Status != metav1.ConditionTrue ||
			cond.Reason != ReasonTemplateInvalid {
			t.Fatalf("want Degraded=True reason=%s, got %+v",
				ReasonTemplateInvalid, got.Status.Conditions)
		}
		pod := &corev1.Pod{}
		if err := c.Get(context.Background(), client.ObjectKey{
			Namespace: ws.Namespace, Name: linux.PodName(ws.UID)}, pod); err == nil {
			t.Fatalf("pod built from an unproven annotation: %s", pod.Spec.Containers[0].Image)
		}
	})
}

// A pod that was not created/owned by the operator for THIS workspace —
// right name, foreign or missing provenance — must never ground an
// adoption. Every such pod falls through to re-snapshot; with no live
// template the workspace holds Degraded.
func TestAdoptSnapshot_ForeignPodNoAdopt(t *testing.T) {
	s := snapScheme(t)
	tpl := adoptTemplate("tinycdi-tenant-a", "fam32-aaaa1111", types.UID("uid-a"))
	ann := snapshotAnnotation(t, tpl, "fam32", 1)

	cases := []struct {
		name  string
		munge func(pod *corev1.Pod)
	}{
		{name: "foreign label, no ownerRef", munge: func(pod *corev1.Pod) {
			pod.Labels[linux.LabelWorkspaceUID] = "someone-else"
			pod.OwnerReferences = nil
		}},
		{name: "matching labels, no ownerRef", munge: func(pod *corev1.Pod) {
			pod.OwnerReferences = nil
		}},
		{name: "matching labels, non-controller ownerRef", munge: func(pod *corev1.Pod) {
			pod.OwnerReferences = []metav1.OwnerReference{{
				APIVersion: "workspaces.cdi.tinyorbit.vn/v1alpha1",
				Kind:       "Workspace",
				Name:       "ws-legacy",
				UID:        "ws-pre-x",
			}}
		}},
		{name: "matching labels, controller ownerRef to another uid", munge: func(pod *corev1.Pod) {
			tr := true
			pod.OwnerReferences = []metav1.OwnerReference{{
				APIVersion: "workspaces.cdi.tinyorbit.vn/v1alpha1",
				Kind:       "Workspace",
				Name:       "other",
				UID:        "not-this-workspace",
				Controller: &tr,
			}}
		}},
		{name: "operator labels squatted generation", munge: func(pod *corev1.Pod) {
			pod.Labels[linux.LabelRuntimeGeneration] = "9"
		}},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ws := adoptWorkspace("fam32", ann, types.UID(fmt.Sprintf("ws-pre-%d", 70+i)))
			c := fake.NewClientBuilder().WithScheme(s).WithObjects(ws).WithStatusSubresource(ws).Build()
			b := linux.New(c, linux.Options{})
			if _, err := b.Ensure(context.Background(), ws, tpl); err != nil {
				t.Fatalf("seed pod: %v", err)
			}
			pod := &corev1.Pod{}
			if err := c.Get(context.Background(), client.ObjectKey{
				Namespace: ws.Namespace, Name: linux.PodName(ws.UID)}, pod); err != nil {
				t.Fatal(err)
			}
			tc.munge(pod)
			if err := c.Update(context.Background(), pod); err != nil {
				t.Fatal(err)
			}
			got := adoptReconcile(t, s, c, ws)
			if got.Status.TemplateSnapshot != nil {
				t.Fatal("snapshot recorded behind a foreign pod")
			}
			cond := condition(got, workspacesv1alpha1.ConditionDegraded)
			if cond == nil || cond.Status != metav1.ConditionTrue ||
				cond.Reason != ReasonTemplateInvalid {
				t.Fatalf("want Degraded TemplateInvalid, got %+v", got.Status.Conditions)
			}
		})
	}
}

// assertRevisionGone asserts the workspace holds Degraded with reason
// TemplateRevisionGone.
func assertRevisionGone(t *testing.T, got *workspacesv1alpha1.Workspace) {
	t.Helper()
	cond := condition(got, workspacesv1alpha1.ConditionDegraded)
	if cond == nil || cond.Status != metav1.ConditionTrue ||
		cond.Reason != ReasonTemplateRevisionGone {
		t.Fatalf("want Degraded=True reason=%s, got %+v",
			ReasonTemplateRevisionGone, got.Status.Conditions)
	}
	if got.Status.TemplateSnapshot != nil {
		t.Fatalf("a snapshot was recorded for an unprovable revision: %+v",
			got.Status.TemplateSnapshot)
	}
}
