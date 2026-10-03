// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package provisioning_test

// FX-R24 characterised why the runtime-absence proof never succeeded for a
// stopped Retain workspace; FX-R25 flipped these tests with the quota model:
// only a pod is a live runtime incarnation, volumes never block the proof.

import (
	"context"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	workspacev1alpha1 "github.com/tinyorbitvn/tinycdi/api/v1alpha1"
	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
	linux "github.com/tinyorbitvn/tinycdi/internal/runtime/linux"
)

func retainTemplate(ns string) *workspacev1alpha1.WorkspaceTemplate {
	return &workspacev1alpha1.WorkspaceTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "tpl", Namespace: ns},
		Spec: workspacev1alpha1.WorkspaceTemplateSpec{
			Revision:   "r1",
			Runtime:    workspacev1alpha1.RuntimeLinuxContainer,
			Experience: workspacev1alpha1.ExperienceDesktop,
			Linux:      &workspacev1alpha1.LinuxRuntimeSpec{Image: "tcdi/linux-desktop@sha256:" + fmt.Sprintf("%064x", 1)},
			Resources: workspacev1alpha1.ResourceProfile{
				CPU: resource.MustParse("500m"), Memory: resource.MustParse("512Mi"), Storage: resource.MustParse("1Gi")},
			BootDeadline:    metav1.Duration{Duration: 5 * time.Minute},
			NetworkProfile:  workspacev1alpha1.NetworkProfileIsolated,
			ClipboardPolicy: workspacev1alpha1.ClipboardDisabled,
			Lifecycle: workspacev1alpha1.LifecycleDefaults{
				IdleTimeout:       metav1.Duration{Duration: 30 * time.Minute},
				DisconnectTimeout: metav1.Duration{Duration: 5 * time.Minute},
				MaxDuration:       metav1.Duration{Duration: 8 * time.Hour},
				DataPolicy:        workspacev1alpha1.DataPolicyRetain,
			},
		},
	}
}

// (b) A plain Retain workspace, run then stopped through the real Linux
// backend: Stop deletes the pod and leaves the home PVC, which carries no
// data-retained label until the workspace is DELETED. The PVC is disk, not
// compute, so the proof succeeds as for an Ephemeral workspace.
func TestK8sRuntimeObserver_StoppedRetainWorkspace(t *testing.T) {
	ctx := context.Background()
	const ns, platformUID = "ns-a", "ws_retain0001"
	tenants := provisioning.TenantNamespaces{"tenant-a": ns}

	for _, tc := range []struct {
		policy   workspacev1alpha1.DataPolicy
		wantGone bool
	}{
		{workspacev1alpha1.DataPolicyEphemeral, true},
		{workspacev1alpha1.DataPolicyRetain, true},
	} {
		t.Run(string(tc.policy), func(t *testing.T) {
			ws := obsCR(provisioning.WorkspaceCRName(platformUID), ns, platformUID, "cr-uid-r")
			ws.Spec.DesiredState = workspacev1alpha1.DesiredStateRunning
			ws.Spec.DataPolicy = tc.policy
			c := fake.NewClientBuilder().WithScheme(observerScheme(t)).WithObjects(ws).Build()
			be := linux.New(c, linux.Options{})
			if _, err := be.Ensure(ctx, ws, retainTemplate(ns)); err != nil {
				t.Fatalf("ensure: %v", err)
			}
			if err := be.Stop(ctx, ws); err != nil {
				t.Fatalf("stop: %v", err)
			}
			var pods corev1.PodList
			if err := c.List(ctx, &pods); err != nil || len(pods.Items) != 0 {
				t.Fatalf("pods after stop = %d err=%v, want 0", len(pods.Items), err)
			}
			gone, err := provisioning.NewK8sRuntimeObserver(c, tenants).RuntimeGone(ctx, platformUID)
			if err != nil || gone != tc.wantGone {
				t.Fatalf("%s stopped: gone=%v err=%v, want %v", tc.policy, gone, err, tc.wantGone)
			}
		})
	}
}

// (a) t54-attached's shape after the retained-attach: the workspace UID labels
// TWO home PVCs — the retained one the pod mounts and a stray default home
// created before the attach annotations landed. Neither blocks the proof.
func TestK8sRuntimeObserver_RetainWithStrayHome(t *testing.T) {
	ctx := context.Background()
	const ns, platformUID = "ns-a", "ws_attached01"
	tenants := provisioning.TenantNamespaces{"tenant-a": ns}
	cr := obsCR(provisioning.WorkspaceCRName(platformUID), ns, platformUID, "cr-uid-a")
	retained := obsPVC("ws-old-home", ns, "cr-uid-a", true)
	stray := obsPVC("ws-cr-uid-a-home", ns, "cr-uid-a", false)

	c := fake.NewClientBuilder().WithScheme(observerScheme(t)).WithObjects(cr, retained).Build()
	if gone, err := provisioning.NewK8sRuntimeObserver(c, tenants).RuntimeGone(ctx, platformUID); err != nil || !gone {
		t.Fatalf("retained claim only: gone=%v err=%v, want true", gone, err)
	}
	c = fake.NewClientBuilder().WithScheme(observerScheme(t)).WithObjects(cr, retained, stray).Build()
	if gone, err := provisioning.NewK8sRuntimeObserver(c, tenants).RuntimeGone(ctx, platformUID); err != nil || !gone {
		t.Fatalf("retained claim + stray home: gone=%v err=%v, want true (a stray volume is disk, not compute)", gone, err)
	}
}
