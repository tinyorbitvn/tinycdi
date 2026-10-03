// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

//go:build integration

package integration

// V3.2 (E1/E2): a Stopped -> Running start that re-points the workspace at
// a newer family revision keeps the retained home volume — same PVC object
// (UID unchanged), same mount — while the new incarnation runs the new
// revision's image.
//
// envtest has no kubelet, so the "file written under revision A" is a
// marker annotation set on the home claim object: the claim standing in for
// the volume's content. What is asserted end-to-end is the contract — the
// claim object survives the re-point untouched and the rebuilt pod mounts
// exactly it.

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	workspacev1alpha1 "github.com/tinyorbitvn/tinycdi/api/v1alpha1"
	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
	linux "github.com/tinyorbitvn/tinycdi/internal/runtime/linux"
)

const (
	v32Tenant = "tenant-v32"
	v32Owner  = "issuer|sub-v32"
)

// v32Revision declares one published revision of the "upd32home" template
// family in envtest: catalog-name labeled, OnStart update policy, Retain
// data-policy default.
func v32Revision(t *testing.T, ns, name, revision, image string) {
	t.Helper()
	tpl := &workspacev1alpha1.WorkspaceTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels:    map[string]string{provisioning.LabelCatalogName: "upd32home"},
		},
		Spec: workspacev1alpha1.WorkspaceTemplateSpec{
			Revision:   revision,
			Runtime:    workspacev1alpha1.RuntimeLinuxContainer,
			Experience: workspacev1alpha1.ExperienceDesktop,
			Linux: &workspacev1alpha1.LinuxRuntimeSpec{
				Image: image,
			},
			Resources: workspacev1alpha1.ResourceProfile{
				CPU:     resource.MustParse("500m"),
				Memory:  resource.MustParse("512Mi"),
				Storage: resource.MustParse("5Gi"),
			},
			BootDeadline:    metav1.Duration{Duration: 5 * time.Minute},
			NetworkProfile:  workspacev1alpha1.NetworkProfileIsolated,
			ClipboardPolicy: workspacev1alpha1.ClipboardDisabled,
			Lifecycle: workspacev1alpha1.LifecycleDefaults{
				IdleTimeout:       metav1.Duration{Duration: 30 * time.Minute},
				DisconnectTimeout: metav1.Duration{Duration: 10 * time.Minute},
				MaxDuration:       metav1.Duration{Duration: 8 * time.Hour},
				DataPolicy:        workspacev1alpha1.DataPolicyRetain,
				ImageUpdate:       workspacev1alpha1.ImageUpdateOnStart,
			},
		},
	}
	if err := k8sClient.Create(context.Background(), tpl); err != nil {
		t.Fatalf("template %s: %v", name, err)
	}
}

func TestUpdate_RetainedHomeSurvives(t *testing.T) {
	ctx := context.Background()
	db := newDB(t)
	ns := newRetainedNamespace(t)
	tmap := provisioning.TenantNamespaces{v32Tenant: ns}
	setQuota(t, db, v32Tenant, provisioning.ResourceVector{
		RunningSlots: 4, CPUMillis: 1 << 30, MemoryBytes: 1 << 40, DiskBytes: 1 << 40,
	})

	imageA := "tcdi/linux-desktop@sha256:" + fmt.Sprintf("%064x", 0xa)
	imageB := "tcdi/linux-desktop@sha256:" + fmt.Sprintf("%064x", 0xb)
	// Only revision A is published at create time — the workspace's family
	// reference must resolve to it at first admit; B lands while the
	// workspace is stopped (the publish-while-stopped scenario).
	v32Revision(t, ns, "upd32home-aaaa1111", "2026-10-a", imageA)

	svc := provisioning.NewService(db).WithTemplateLookup(
		provisioning.NewK8sTemplateCatalog(k8sClient, tmap))
	rec := provisioning.NewRecovery(db, provisioning.NewK8sRuntimeObserver(k8sClient, tmap))
	applier := provisioning.NewK8sApplier(k8sClient, tmap)
	recoverPass := func() {
		t.Helper()
		if _, err := rec.Recover(ctx, applier); err != nil {
			t.Fatalf("recover: %v", err)
		}
	}
	startOperator(t, ns)

	res, err := svc.CreateWorkspace(ctx, v32Tenant, "v32-create", provisioning.CreateRequest{
		OwnerIssuer: "issuer", OwnerSubject: "sub-v32", Name: "v32-home",
		Template: provisioning.TemplateInfo{
			ID: "tpl_upd32home-aaaa1111", Name: "upd32home", Revision: 1,
			RevisionLabel: "2026-10-a", Runtime: "LinuxContainer", Experience: "Desktop",
		},
		Vector:       provisioning.ResourceVector{RunningSlots: 1, CPUMillis: 500, MemoryBytes: 1 << 30, DiskBytes: 5 << 30},
		DesiredState: "Running", DataPolicy: "Retain",
	}, provisioning.RequestHash("v32-create"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	recoverPass()

	cr := workspaceCRByUID(t, res.ID)
	if cr == nil {
		t.Fatal("workspace CR not created")
	}
	pvcKey := client.ObjectKey{Namespace: ns, Name: linux.PVCName(cr.UID)}
	var homeUID string
	eventually(t, "revision-A pod and home claim", 30*time.Second, func() bool {
		pod := &corev1.Pod{}
		if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: linux.PodName(cr.UID)}, pod); err != nil {
			return false
		}
		pvc := &corev1.PersistentVolumeClaim{}
		if err := k8sClient.Get(ctx, pvcKey, pvc); err != nil {
			return false
		}
		homeUID = string(pvc.UID)
		return pod.Spec.Containers[0].Image == imageA
	})

	// The file written under revision A (envtest has no kubelet to write
	// into the volume — a marker on the claim stands in for its content).
	home := &corev1.PersistentVolumeClaim{}
	if err := k8sClient.Get(ctx, pvcKey, home); err != nil {
		t.Fatalf("home claim: %v", err)
	}
	if home.Annotations == nil {
		home.Annotations = map[string]string{}
	}
	home.Annotations["it.v32/home-file"] = "written-under-rev-a"
	if err := k8sClient.Update(ctx, home); err != nil {
		t.Fatalf("mark home claim: %v", err)
	}

	// Stop; the pod goes away, the claim stays.
	if _, err := svc.SignalWorkspace(ctx, v32Tenant, v32Owner, "", res.ID, "v32-stop",
		provisioning.IntentStop, []byte("{}")); err != nil {
		t.Fatalf("stop: %v", err)
	}
	recoverPass()
	podKey := client.ObjectKey{Namespace: ns, Name: linux.PodName(cr.UID)}
	eventually(t, "pod teardown", 30*time.Second, func() bool {
		pod := &corev1.Pod{}
		err := k8sClient.Get(ctx, podKey, pod)
		return err != nil || !pod.DeletionTimestamp.IsZero()
	})

	// Revision B is published while the workspace is stopped.
	v32Revision(t, ns, "upd32home-bbbb2222", "2026-10-b", imageB)

	// Start: the row adopts revision B, the intent carries the re-point,
	// and the operator re-snapshots under the new generation.
	st, err := svc.SignalWorkspace(ctx, v32Tenant, v32Owner, "", res.ID, "v32-start",
		provisioning.IntentStart, startHash)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if st.Template.ID != "tpl_upd32home-bbbb2222" {
		t.Fatalf("record template = %+v, want revision B", st.Template)
	}
	recoverPass()
	eventually(t, "generation-2 pod on revision B", 60*time.Second, func() bool {
		pod := &corev1.Pod{}
		if err := k8sClient.Get(ctx, podKey, pod); err != nil || !pod.DeletionTimestamp.IsZero() {
			return false
		}
		return pod.Labels[linux.LabelRuntimeGeneration] == "2" &&
			pod.Spec.Containers[0].Image == imageB
	})

	// The recorded snapshot now provably is revision B's.
	fresh := workspaceCRByUID(t, res.ID)
	var snap struct {
		Name     string `json:"name"`
		SpecHash string `json:"specHash"`
	}
	if err := json.Unmarshal([]byte(fresh.Annotations["workspaces.cdi.tinyorbit.vn/template-snapshot"]), &snap); err != nil {
		t.Fatalf("snapshot annotation: %v", err)
	}
	if snap.Name != "upd32home-bbbb2222" {
		t.Fatalf("snapshot source = %q, want upd32home-bbbb2222", snap.Name)
	}
	if fresh.Spec.TemplateRef.Name != "upd32home-bbbb2222" {
		t.Fatalf("CR templateRef = %q, want upd32home-bbbb2222", fresh.Spec.TemplateRef.Name)
	}

	// The pod mounts the same claim; the claim itself is the same object —
	// UID unchanged — and the revision-A file marker is still on it.
	pod := &corev1.Pod{}
	if err := k8sClient.Get(ctx, podKey, pod); err != nil {
		t.Fatalf("generation-2 pod: %v", err)
	}
	var claims []string
	for _, v := range pod.Spec.Volumes {
		if v.PersistentVolumeClaim != nil {
			claims = append(claims, v.PersistentVolumeClaim.ClaimName)
		}
	}
	if len(claims) != 1 || claims[0] != pvcKey.Name {
		t.Fatalf("pod mounts claims %v, want exactly %q", claims, pvcKey.Name)
	}
	got := &corev1.PersistentVolumeClaim{}
	if err := k8sClient.Get(ctx, pvcKey, got); err != nil {
		t.Fatalf("home claim after restart: %v", err)
	}
	if string(got.UID) != homeUID {
		t.Fatalf("home claim UID %s -> %s; the retained volume was rebuilt", homeUID, got.UID)
	}
	if got.Annotations["it.v32/home-file"] != "written-under-rev-a" {
		t.Fatalf("revision-A file marker lost: annotations=%v", got.Annotations)
	}
}
