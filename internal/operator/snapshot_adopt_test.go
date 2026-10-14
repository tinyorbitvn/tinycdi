// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

// Status-loss recovery for status.templateSnapshot. A running workspace
// without the status record re-establishes it from the RUNNING
// incarnation, never from the annotation: the operator-owned pod names its
// template revision in stamped metadata
// (workspaces.cdi.tinyorbit.vn/template-name|template-revision), the LIVE
// revision object supplies the content, and the pod's template-hash stamp
// must match it. A pod that cannot prove its revision — no identity or
// hash stamp, a pruned or republished object, a mismatched hash — holds
// the workspace Degraded/TemplateRevisionGone with the pod left running; a
// workspace with no pod re-snapshots the live template. The annotation is
// never consulted.
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

	cdiv1 "github.com/tinyorbitvn/tinycdi/api/v1"
	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
	linux "github.com/tinyorbitvn/tinycdi/internal/runtime/linux"
)

// adoptTemplate builds the revision object a workspace was admitted
// under — catalog-name labeled fam32, the way published revisions carry
// their family.
func adoptTemplate(ns, name string, uid types.UID) *cdiv1.WorkspaceTemplate {
	return &cdiv1.WorkspaceTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: ns, UID: uid,
			Labels:      map[string]string{provisioning.LabelCatalogName: "fam32"},
			Annotations: map[string]string{linux.AnnotationSeccompProfile: "localhost/browser-sandbox"},
		},
		Spec: cdiv1.WorkspaceTemplateSpec{
			Revision: "2026-10-a", Runtime: cdiv1.RuntimeLinuxContainer,
			Experience: cdiv1.ExperienceDesktop,
			Linux: &cdiv1.LinuxRuntimeSpec{
				Image: "cr.example/img@sha256:" + fmt.Sprintf("%064x", 0xa1),
			},
			Resources: cdiv1.ResourceProfile{
				CPU: resource.MustParse("1"), Memory: resource.MustParse("2Gi"),
				Storage: resource.MustParse("5Gi"),
			},
			BootDeadline:   metav1.Duration{Duration: 5 * time.Minute},
			NetworkProfile: cdiv1.NetworkProfileIsolated,
		},
	}
}

// adoptWorkspace seeds a Running workspace whose status was lost — no
// status.templateSnapshot. ann, when non-empty, seeds the
// (never-trusted) snapshot annotation.
func adoptWorkspace(ref, ann string, uid types.UID) *cdiv1.Workspace {
	annotations := map[string]string{}
	if ann != "" {
		annotations[AnnotationTemplateSnapshot] = ann
	}
	return &cdiv1.Workspace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ws-pre", Namespace: "tinycdi-tenant-a", UID: uid,
			Labels:      map[string]string{"workspaces.cdi.tinyorbit.vn/workspace-uid": "ws_pre"},
			Annotations: annotations,
		},
		Spec: cdiv1.WorkspaceSpec{
			TemplateRef:       cdiv1.TemplateReference{Name: ref},
			OwnerSubject:      cdiv1.OwnerSubject{Issuer: "i", Subject: "s"},
			DesiredState:      cdiv1.DesiredStateRunning,
			DataPolicy:        cdiv1.DataPolicyEphemeral,
			RuntimeGeneration: 1,
			IntentRevision:    1,
			SessionPolicy: cdiv1.SessionPolicySpec{
				Clipboard:    cdiv1.ClipboardDisabled,
				FileTransfer: cdiv1.FileTransferDisabled,
			},
		},
	}
}

// snapshotAnnotation renders the annotation bytes the operator mirrors —
// used to prove the annotation is never read.
func snapshotAnnotation(t *testing.T, tpl *cdiv1.WorkspaceTemplate, ref string, gen int64) string {
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
func seedBuiltPod(t *testing.T, c client.Client, ws *cdiv1.Workspace,
	tpl *cdiv1.WorkspaceTemplate) *corev1.Pod {
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
	ws *cdiv1.Workspace) *cdiv1.Workspace {
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

// An annotation — honest or forged — is never consulted: the stamped
// pod's identity decides everything.
func TestAdoptSnapshot_AnnotationNeverConsulted(t *testing.T) {
	s := snapScheme(t)
	tpl := adoptTemplate("tinycdi-tenant-a", "fam32-aaaa1111", types.UID("uid-a"))

	forged := forgedSpec("attacker.example/miner@sha256:" + fmt.Sprintf("%064x", 3))
	forged.Revision = "2026-10-a"
	forged.Placement = &cdiv1.PlacementSpec{
		NodeSelector: map[string]string{"node-role.kubernetes.io/control-plane": ""},
	}
	fraw, _ := json.Marshal(forged)
	fsum := sha256.Sum256(fraw)
	forgedRaw, _ := json.Marshal(&templateSnapshot{
		Name: "fam32-aaaa1111", UID: "uid-a", Revision: "2026-10-a",
		SpecHash: "sha256:" + hex.EncodeToString(fsum[:]), Spec: forged,
		RuntimeGeneration: 1, SourceRef: "fam32",
	})

	for name, ann := range map[string]string{
		"forged spec":           string(forgedRaw),
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
	forged.NetworkProfile = cdiv1.NetworkProfileInternetOnly
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
		got.Status.TemplateSnapshot.Spec.NetworkProfile != cdiv1.NetworkProfileIsolated {
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

// unstamp turns pod into an owned pod without the identity stamps (name,
// revision and hash).
func unstamp(t *testing.T, c client.Client, pod *corev1.Pod) {
	t.Helper()
	delete(pod.Annotations, linux.AnnotationTemplateName)
	delete(pod.Annotations, linux.AnnotationTemplateRevision)
	delete(pod.Annotations, linux.AnnotationTemplateHash)
	if err := c.Update(context.Background(), pod); err != nil {
		t.Fatalf("unstamp pod: %v", err)
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
		cond := condition(got, cdiv1.ConditionDegraded)
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
				APIVersion: "workspaces.cdi.tinyorbit.vn/v1",
				Kind:       "Workspace",
				Name:       "ws-legacy",
				UID:        "ws-pre-x",
			}}
		}},
		{name: "matching labels, controller ownerRef to another uid", munge: func(pod *corev1.Pod) {
			tr := true
			pod.OwnerReferences = []metav1.OwnerReference{{
				APIVersion: "workspaces.cdi.tinyorbit.vn/v1",
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
			cond := condition(got, cdiv1.ConditionDegraded)
			if cond == nil || cond.Status != metav1.ConditionTrue ||
				cond.Reason != ReasonTemplateInvalid {
				t.Fatalf("want Degraded TemplateInvalid, got %+v", got.Status.Conditions)
			}
		})
	}
}

// An owned pod without the identity and hash stamps cannot prove its
// revision: the workspace holds Degraded/TemplateRevisionGone and the pod
// is left exactly as it was. Every pod this operator builds carries the
// stamps.
//
// templateRef names the exact revision object the pod was built from, so
// the removed spec-rebuild proof would have matched and adopted it: this is
// the case that proves the proof is gone.
func TestAdoptSnapshot_UnstampedPodHolds(t *testing.T) {
	s := snapScheme(t)
	tpl := adoptTemplate("tinycdi-tenant-a", "fam32-aaaa1111", types.UID("uid-a"))
	ws := adoptWorkspace("fam32-aaaa1111", "", types.UID("ws-unstamped"))
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(ws, tpl).WithStatusSubresource(ws).Build()
	pod := seedBuiltPod(t, c, ws, tpl)
	unstamp(t, c, pod) // existing helper: drops name, revision and hash stamps and updates
	before := pod.ResourceVersion

	got := adoptReconcile(t, s, c, ws)
	assertRevisionGone(t, got) // also fails if a snapshot was recorded
	after := &corev1.Pod{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(pod), after); err != nil {
		t.Fatalf("pod gone: %v", err)
	}
	if after.ResourceVersion != before {
		t.Fatal("the held workspace's pod was modified")
	}
}

// A pod with the name/revision stamps but no template-hash stamp cannot
// prove its content either: PodMatchesTemplate is the hash comparison
// alone, so the workspace holds and the pod is left as it was.
func TestAdoptSnapshot_StampedPodWithoutHashHolds(t *testing.T) {
	s := snapScheme(t)
	tpl := adoptTemplate("tinycdi-tenant-a", "fam32-aaaa1111", types.UID("uid-a"))
	ws := adoptWorkspace("fam32", "", types.UID("ws-nohash"))
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(ws, tpl).WithStatusSubresource(ws).Build()
	pod := seedBuiltPod(t, c, ws, tpl)
	delete(pod.Annotations, linux.AnnotationTemplateHash)
	if err := c.Update(context.Background(), pod); err != nil {
		t.Fatalf("drop hash stamp: %v", err)
	}
	before := pod.ResourceVersion

	got := adoptReconcile(t, s, c, ws)
	assertRevisionGone(t, got)
	after := &corev1.Pod{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(pod), after); err != nil {
		t.Fatalf("pod gone: %v", err)
	}
	if after.ResourceVersion != before {
		t.Fatal("the held workspace's pod was modified")
	}
}

// recordedSnapshot reads status only: the template-snapshot annotation is a
// mirror for human readers and is never a source.
func TestRecordedSnapshot_IgnoresAnnotation(t *testing.T) {
	tpl := adoptTemplate("tinycdi-tenant-a", "fam32-aaaa1111", types.UID("uid-a"))
	ws := adoptWorkspace("fam32", "", types.UID("ws-anno"))
	ws.Annotations = map[string]string{AnnotationTemplateSnapshot: snapshotAnnotation(t, tpl, "fam32-aaaa1111", 1)}
	if got := recordedSnapshot(ws); got != nil {
		t.Fatalf("recordedSnapshot = %+v, want nil without status", got)
	}
}

// assertRevisionGone asserts the workspace holds Degraded with reason
// TemplateRevisionGone.
func assertRevisionGone(t *testing.T, got *cdiv1.Workspace) {
	t.Helper()
	cond := condition(got, cdiv1.ConditionDegraded)
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
