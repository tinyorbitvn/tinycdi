// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

// Upgrade-adoption coverage for status.templateSnapshot: a workspace
// admitted before the status field existed carries the snapshot only in
// the (writer-controlled) annotation. The operator may adopt that record
// ONCE — and only while a live operator-owned pod proves it was built
// from exactly that record: a matching template-hash stamp, or, for pods
// built before the stamp existed, a spec that equals a fresh build
// field-for-field. Everything else re-snapshots the live template; a
// workspace with neither adopts nothing and never trusts the annotation.
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

// adoptWorkspace seeds a Running workspace carrying the recorded snapshot
// annotation (a v0.5.0-row shape) but no status.templateSnapshot.
func adoptWorkspace(ref, ann string, uid types.UID) *workspacesv1alpha1.Workspace {
	return &workspacesv1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ws-pre", Namespace: "tinycdi-tenant-a", UID: uid,
			Labels:      map[string]string{"workspaces.cdi.tinyorbit.vn/workspace-uid": "ws_pre"},
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
	}
}

// snapshotAnnotation renders the annotation the v0.5.0 operator recorded —
// the same bytes snapshotTemplate produces, sourceRef/runtimeGeneration
// filled as the re-snapshot machinery writes them.
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
// (as a v0.5.0 operator would have) and strips the template-hash stamp
// when unstamped is set, simulating a pod built before the stamp existed.
// The stored pod is returned.
func seedBuiltPod(t *testing.T, c client.Client, ws *workspacesv1alpha1.Workspace,
	tpl *workspacesv1alpha1.WorkspaceTemplate, unstamped bool) *corev1.Pod {
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
	if unstamped {
		delete(pod.Annotations, linux.AnnotationTemplateHash)
		if err := c.Update(context.Background(), pod); err != nil {
			t.Fatalf("strip stamp: %v", err)
		}
	}
	return pod
}

// A workspace whose pre-stamp pod provably was built from the recorded
// annotation adopts the record into status unchanged — the upgrade keeps
// the running revision without resolving the template again (the object
// may be gone entirely).
func TestAdoptSnapshot_PreStampPodMatches(t *testing.T) {
	s := snapScheme(t)
	tpl := adoptTemplate("tinycdi-tenant-a", "fam32-aaaa1111", types.UID("uid-a"))
	ws := adoptWorkspace("fam32", snapshotAnnotation(t, tpl, "fam32", 1), types.UID("ws-pre-1"))
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(ws).WithStatusSubresource(ws).Build()
	pod := seedBuiltPod(t, c, ws, tpl, true)

	r := &WorkspaceReconciler{Client: c, Scheme: s, Backend: linux.New(c, linux.Options{})}
	reconcile(t, r, client.ObjectKeyFromObject(ws))

	got := getWorkspace(t, c, client.ObjectKeyFromObject(ws))
	snap := got.Status.TemplateSnapshot
	if snap == nil {
		t.Fatal("pod-proven annotation was not adopted into status")
	}
	want, _ := snapshotTemplate(tpl)
	if snap.Name != want.Name || snap.UID != want.UID || snap.SpecHash != want.SpecHash ||
		snap.RuntimeGeneration != 1 || snap.SourceRef != "fam32" {
		t.Fatalf("adopted snapshot = %+v, want record of fam32-aaaa1111 gen 1", snap)
	}
	// The running incarnation is undisturbed.
	cur := &corev1.Pod{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(pod), cur); err != nil {
		t.Fatalf("pod vanished after adoption: %v", err)
	}
	if cur.UID != pod.UID {
		t.Fatal("adoption recreated the pod")
	}
}

// The stamped form of the same proof: a pod carrying the template-hash
// stamp adopts only a record whose (spec, annotations) hash matches.
func TestAdoptSnapshot_StampedPodMatches(t *testing.T) {
	s := snapScheme(t)
	tpl := adoptTemplate("tinycdi-tenant-a", "fam32-aaaa1111", types.UID("uid-a"))
	ws := adoptWorkspace("fam32", snapshotAnnotation(t, tpl, "fam32", 1), types.UID("ws-pre-2"))
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(ws).WithStatusSubresource(ws).Build()
	seedBuiltPod(t, c, ws, tpl, false)

	r := &WorkspaceReconciler{Client: c, Scheme: s, Backend: linux.New(c, linux.Options{})}
	reconcile(t, r, client.ObjectKeyFromObject(ws))

	if got := getWorkspace(t, c, client.ObjectKeyFromObject(ws)); got.Status.TemplateSnapshot == nil {
		t.Fatal("stamp-matched annotation was not adopted")
	}
}

// A stamped pod rejects a record that keeps the honest spec but swaps
// annotations (the stamp covers spec+annotations): a forged
// seccomp-profile never reaches status — the live template is
// re-snapshotted instead.
func TestAdoptSnapshot_StampedPodRejectsAnnotationSwap(t *testing.T) {
	s := snapScheme(t)
	tpl := adoptTemplate("tinycdi-tenant-a", "fam32-aaaa1111", types.UID("uid-a"))
	var parsed templateSnapshot
	ann := snapshotAnnotation(t, tpl, "fam32", 1)
	if err := json.Unmarshal([]byte(ann), &parsed); err != nil {
		t.Fatal(err)
	}
	// The forge keeps spec+specHash honest and swaps only annotations.
	parsed.Annotations = map[string]string{linux.AnnotationSeccompProfile: "localhost/attacker-loaded"}
	forgedRaw, _ := json.Marshal(&parsed)

	ws := adoptWorkspace("fam32", string(forgedRaw), types.UID("ws-pre-3"))
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(ws, tpl).WithStatusSubresource(ws).Build()
	seedBuiltPod(t, c, ws, tpl, false)

	r := &WorkspaceReconciler{Client: c, Scheme: s, Backend: linux.New(c, linux.Options{})}
	reconcile(t, r, client.ObjectKeyFromObject(ws))
	reconcile(t, r, client.ObjectKeyFromObject(ws))

	got := getWorkspace(t, c, client.ObjectKeyFromObject(ws))
	snap := got.Status.TemplateSnapshot
	if snap == nil {
		t.Fatal("live-template re-snapshot did not record status")
	}
	if snap.Annotations[linux.AnnotationSeccompProfile] != "localhost/browser-sandbox" {
		t.Fatalf("forged annotations adopted: %v", snap.Annotations)
	}
}

// An annotation whose spec the live pod does NOT match is never adopted:
// the workspace re-snapshots the live template and records THAT. The
// forged spec never converges.
func TestAdoptSnapshot_ForgedSpecRejectsAdoption(t *testing.T) {
	s := snapScheme(t)
	honestTpl := adoptTemplate("tinycdi-tenant-a", "fam32-aaaa1111", types.UID("uid-a"))
	// Forged record: valid hash over an attacker spec (control-plane
	// placement, attacker image), claiming the honest object's identity.
	forged := forgedSpec("attacker.example/miner@sha256:" + fmt.Sprintf("%064x", 3))
	forged.Revision = "2026-10-a"
	forged.Placement = &workspacesv1alpha1.PlacementSpec{
		NodeSelector: map[string]string{"node-role.kubernetes.io/control-plane": ""},
	}
	fraw, _ := json.Marshal(forged)
	fsum := sha256.Sum256(fraw)
	ann := forgedSnapshot("fam32-aaaa1111", "uid-a", "2026-10-a",
		"sha256:"+hex.EncodeToString(fsum[:]), forged,
		map[string]string{linux.AnnotationSeccompProfile: "localhost/browser-sandbox"})
	var parsed templateSnapshot
	if err := json.Unmarshal([]byte(ann), &parsed); err != nil {
		t.Fatal(err)
	}
	parsed.SourceRef = "fam32"
	parsed.RuntimeGeneration = 1
	forgedRaw, _ := json.Marshal(&parsed)

	ws := adoptWorkspace("fam32", string(forgedRaw), types.UID("ws-pre-4"))
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(ws, honestTpl).WithStatusSubresource(ws).Build()
	// The pod was honestly built — the forge does not describe it.
	pod := seedBuiltPod(t, c, ws, honestTpl, true)

	r := &WorkspaceReconciler{Client: c, Scheme: s, Backend: linux.New(c, linux.Options{})}
	reconcile(t, r, client.ObjectKeyFromObject(ws))

	got := getWorkspace(t, c, client.ObjectKeyFromObject(ws))
	snap := got.Status.TemplateSnapshot
	if snap == nil {
		t.Fatal("live-template re-snapshot did not record status")
	}
	want, _ := snapshotTemplate(honestTpl)
	if snap.SpecHash != want.SpecHash {
		t.Fatalf("forged spec adopted: %q", snap.SpecHash)
	}
	cur := &corev1.Pod{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(pod), cur); err != nil {
		t.Fatalf("pod vanished: %v", err)
	}
	if cur.Spec.Containers[0].Image != honestTpl.Spec.Linux.Image {
		t.Fatalf("pod rebuilt from forged spec: %q", cur.Spec.Containers[0].Image)
	}
}

// A pre-upgrade workspace with an annotation but NO pod cannot prove its
// record — it re-snapshots the live template like a fresh admit, and if
// no template resolves it holds Degraded with no pod.
func TestAdoptSnapshot_RequiresPod(t *testing.T) {
	t.Run("live template re-snapshots", func(t *testing.T) {
		s := snapScheme(t)
		tpl := adoptTemplate("tinycdi-tenant-a", "fam32-aaaa1111", types.UID("uid-a"))
		ws := adoptWorkspace("fam32-aaaa1111",
			snapshotAnnotation(t, tpl, "fam32-aaaa1111", 1), types.UID("ws-pre-5"))
		c := fake.NewClientBuilder().WithScheme(s).
			WithObjects(ws, tpl).WithStatusSubresource(ws).Build()
		r := &WorkspaceReconciler{Client: c, Scheme: s, Backend: linux.New(c, linux.Options{})}
		reconcile(t, r, client.ObjectKeyFromObject(ws))
		got := getWorkspace(t, c, client.ObjectKeyFromObject(ws))
		if got.Status.TemplateSnapshot == nil {
			t.Fatal("no status snapshot after re-snapshot of live template")
		}
	})

	t.Run("unresolvable template holds degraded", func(t *testing.T) {
		s := snapScheme(t)
		tpl := adoptTemplate("tinycdi-tenant-a", "fam32-aaaa1111", types.UID("uid-a"))
		ws := adoptWorkspace("fam32-aaaa1111",
			snapshotAnnotation(t, tpl, "fam32-aaaa1111", 1), types.UID("ws-pre-6"))
		// The recorded object is gone — no pod, no live template: nothing
		// trustworthy remains, so the workspace holds Degraded.
		c := fake.NewClientBuilder().WithScheme(s).
			WithObjects(ws).WithStatusSubresource(ws).Build()
		r := &WorkspaceReconciler{Client: c, Scheme: s, Backend: linux.New(c, linux.Options{})}
		reconcile(t, r, client.ObjectKeyFromObject(ws))
		got := getWorkspace(t, c, client.ObjectKeyFromObject(ws))
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

// PodMatchesTemplate ownership: a pod that was not created/owned by the
// operator for THIS workspace — right name, foreign or missing
// provenance — must never prove a record, even when its spec matches the
// forged annotation exactly. Every such pod rejects adoption: the
// workspace holds Degraded rather than trusting the annotation.
func TestAdoptSnapshot_ForeignPodNoAdopt(t *testing.T) {
	s := snapScheme(t)
	tpl := adoptTemplate("tinycdi-tenant-a", "fam32-aaaa1111", types.UID("uid-a"))
	// The annotation claims the honest record; the attack is in the POD,
	// which the workspace writer cannot create but a bug/compromise
	// scenario must still reject.
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
			r := &WorkspaceReconciler{Client: c, Scheme: s, Backend: linux.New(c, linux.Options{})}
			reconcile(t, r, client.ObjectKeyFromObject(ws))
			got := getWorkspace(t, c, client.ObjectKeyFromObject(ws))
			if got.Status.TemplateSnapshot != nil {
				t.Fatal("annotation adopted behind a foreign pod")
			}
			cond := condition(got, workspacesv1alpha1.ConditionDegraded)
			if cond == nil || cond.Status != metav1.ConditionTrue ||
				cond.Reason != ReasonTemplateInvalid {
				t.Fatalf("want Degraded TemplateInvalid, got %+v", got.Status.Conditions)
			}
		})
	}
}
