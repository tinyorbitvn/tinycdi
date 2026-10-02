// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

// Unit coverage for the template-annotation security contract of the Linux
// backend (I-10): workspaces.cdi.tinyorbit.vn/apparmor-profile selects the
// container AppArmor profile (K8s 1.30+ securityContext.appArmorProfile).
// Only "localhost/<name>" (a profile pre-loaded on the node) and
// "runtime/default" are accepted; anything else — including "unconfined" —
// rejects the template (ErrTemplateRejected) before ANY runtime child is
// created, so the workspace is marked Degraded instead of silently
// launching under the containerd default profile (the verified finding: the
// default profile denies the user namespaces Chromium's sandbox needs).
package linux

import (
	"context"
	"errors"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	workspacesv1alpha1 "github.com/tinyorbitvn/tinycdi/api/v1alpha1"
)

func backendScheme(t *testing.T) *runtime.Scheme {
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

func testWorkspace() *workspacesv1alpha1.Workspace {
	return &workspacesv1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ws-test",
			Namespace: "tenant-a",
			UID:       types.UID("11111111-2222-3333-4444-555555555555"),
		},
		Spec: workspacesv1alpha1.WorkspaceSpec{
			DataPolicy:        workspacesv1alpha1.DataPolicyEphemeral,
			RuntimeGeneration: 1,
		},
	}
}

func testTemplate(annotations map[string]string) *workspacesv1alpha1.WorkspaceTemplate {
	return &workspacesv1alpha1.WorkspaceTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "tpl-test",
			Namespace:   "tenant-a",
			Annotations: annotations,
		},
		Spec: workspacesv1alpha1.WorkspaceTemplateSpec{
			Linux: &workspacesv1alpha1.LinuxRuntimeSpec{
				Image: "registry.example.com/tinycdi/linux-desktop@sha256:" +
					"0000000000000000000000000000000000000000000000000000000000000000",
			},
			Resources: workspacesv1alpha1.ResourceProfile{
				CPU:     resource.MustParse("500m"),
				Memory:  resource.MustParse("1Gi"),
				Storage: resource.MustParse("1Gi"),
			},
			NetworkProfile: workspacesv1alpha1.NetworkProfileIsolated,
		},
	}
}

// ensurePodSpec runs Ensure against a fake client and returns the converged
// Pod spec (the workspace object itself need not exist — children are
// keyed/labeled by its UID).
func ensurePodSpec(t *testing.T, annotations map[string]string) *corev1.Pod {
	t.Helper()
	return ensurePodSpecFor(t, testTemplate(annotations), Options{})
}

// ensurePodSpecFor is ensurePodSpec with an explicit template and backend
// options — the placement tests vary both axes.
func ensurePodSpecFor(t *testing.T, tpl *workspacesv1alpha1.WorkspaceTemplate, opts Options) *corev1.Pod {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(backendScheme(t)).Build()
	ws := testWorkspace()
	if _, err := New(c, opts).Ensure(context.Background(), ws, tpl); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	pod := &corev1.Pod{}
	if err := c.Get(context.Background(),
		client.ObjectKey{Name: PodName(ws.UID), Namespace: ws.Namespace}, pod); err != nil {
		t.Fatalf("pod not created: %v", err)
	}
	return pod
}

func TestAppArmorProfileLocalhost(t *testing.T) {
	pod := ensurePodSpec(t, map[string]string{
		AnnotationAppArmorProfile: "localhost/tinycdi-browser-check",
	})
	aa := pod.Spec.Containers[0].SecurityContext.AppArmorProfile
	if aa == nil || aa.Type != corev1.AppArmorProfileTypeLocalhost {
		t.Fatalf("container appArmorProfile = %+v, want Localhost", aa)
	}
	if aa.LocalhostProfile == nil || *aa.LocalhostProfile != "tinycdi-browser-check" {
		t.Fatalf("localhostProfile = %v, want tinycdi-browser-check", aa.LocalhostProfile)
	}
}

func TestAppArmorProfileRuntimeDefaultExplicit(t *testing.T) {
	pod := ensurePodSpec(t, map[string]string{
		AnnotationAppArmorProfile: "runtime/default",
	})
	aa := pod.Spec.Containers[0].SecurityContext.AppArmorProfile
	if aa == nil || aa.Type != corev1.AppArmorProfileTypeRuntimeDefault {
		t.Fatalf("container appArmorProfile = %+v, want RuntimeDefault", aa)
	}
	if aa.LocalhostProfile != nil {
		t.Fatalf("runtime/default must not carry a localhostProfile, got %q", *aa.LocalhostProfile)
	}
}

func TestAppArmorProfileAbsentDefaultsRuntimeDefault(t *testing.T) {
	pod := ensurePodSpec(t, nil)
	aa := pod.Spec.Containers[0].SecurityContext.AppArmorProfile
	if aa == nil || aa.Type != corev1.AppArmorProfileTypeRuntimeDefault {
		t.Fatalf("absent annotation must yield explicit RuntimeDefault, got %+v", aa)
	}
}

// A rejected template must create NO runtime children at all: the finalizer
// would otherwise have to tear down a partially-converged workspace that can
// never become valid.
func TestAppArmorProfileRejected(t *testing.T) {
	for _, v := range []string{
		"unconfined",
		"Unconfined",
		"localhost/",     // empty profile name
		"localhost",      // missing name separator
		"docker/default", // docker is not a selectable value
		"runtime/unconfined",
		" localhost/x",  // leading space
		"localhost/a b", // whitespace in the profile name
	} {
		c := fake.NewClientBuilder().WithScheme(backendScheme(t)).Build()
		ws := testWorkspace()
		tpl := testTemplate(map[string]string{AnnotationAppArmorProfile: v})
		_, err := New(c, Options{}).Ensure(context.Background(), ws, tpl)
		if err == nil {
			t.Fatalf("annotation %q: Ensure must fail, pod built anyway", v)
		}
		if !errors.Is(err, ErrTemplateRejected) {
			t.Fatalf("annotation %q: error %v must wrap ErrTemplateRejected", v, err)
		}
		// No children: no Secret, no PVC, no Pod, no Service, no NetworkPolicy.
		for _, obj := range []client.Object{
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: SecretName(ws.UID)}},
			&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: PodName(ws.UID)}},
			&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: ServiceName(ws.UID)}},
		} {
			err := c.Get(context.Background(),
				client.ObjectKey{Name: obj.GetName(), Namespace: ws.Namespace}, obj)
			if !apierrors.IsNotFound(err) {
				t.Fatalf("annotation %q: child %T %s must not exist (err=%v)", v, obj, obj.GetName(), err)
			}
		}
	}
}

// --- spec.placement (D24) ---------------------------------------------------
//
// Placement precedence is per field: the typed template field wins, then the
// legacy node-selector annotation (nodeSelector only — it predates the typed
// field and stays for one release), then the operator's Options defaults.
// A field the template sets REPLACES the default; it is never merged.

var poolToleration = corev1.Toleration{
	Key:      "cdi.tinyorbit.vn/workspace",
	Operator: corev1.TolerationOpEqual,
	Value:    "true",
	Effect:   corev1.TaintEffectNoSchedule,
}

func defaultPlacementOptions() Options {
	return Options{DefaultPlacement: workspacesv1alpha1.PlacementSpec{
		NodeSelector: map[string]string{"pool": "default"},
		Tolerations: []corev1.Toleration{{
			Key:      "default-taint",
			Operator: corev1.TolerationOpExists,
			Effect:   corev1.TaintEffectNoExecute,
		}},
		RuntimeClassName: ptr("default-rc"),
	}}
}

func TestBuildPod_PlacementFromTemplate(t *testing.T) {
	tpl := testTemplate(nil)
	tpl.Spec.Placement = &workspacesv1alpha1.PlacementSpec{
		NodeSelector:     map[string]string{"workload": "runtime"},
		Tolerations:      []corev1.Toleration{poolToleration},
		RuntimeClassName: ptr("gvisor"),
	}
	pod := ensurePodSpecFor(t, tpl, defaultPlacementOptions())
	if !reflect.DeepEqual(pod.Spec.NodeSelector, map[string]string{"workload": "runtime"}) {
		t.Fatalf("nodeSelector = %v, want template value", pod.Spec.NodeSelector)
	}
	if !reflect.DeepEqual(pod.Spec.Tolerations, []corev1.Toleration{poolToleration}) {
		t.Fatalf("tolerations = %v, want template value", pod.Spec.Tolerations)
	}
	if pod.Spec.RuntimeClassName == nil || *pod.Spec.RuntimeClassName != "gvisor" {
		t.Fatalf("runtimeClassName = %v, want gvisor", pod.Spec.RuntimeClassName)
	}
}

func TestBuildPod_PlacementDefaults(t *testing.T) {
	pod := ensurePodSpecFor(t, testTemplate(nil), defaultPlacementOptions())
	if !reflect.DeepEqual(pod.Spec.NodeSelector, map[string]string{"pool": "default"}) {
		t.Fatalf("nodeSelector = %v, want operator default", pod.Spec.NodeSelector)
	}
	if len(pod.Spec.Tolerations) != 1 || pod.Spec.Tolerations[0].Key != "default-taint" {
		t.Fatalf("tolerations = %v, want operator default", pod.Spec.Tolerations)
	}
	if pod.Spec.RuntimeClassName == nil || *pod.Spec.RuntimeClassName != "default-rc" {
		t.Fatalf("runtimeClassName = %v, want default-rc", pod.Spec.RuntimeClassName)
	}
}

func TestBuildPod_PerFieldFallback(t *testing.T) {
	tpl := testTemplate(nil)
	tpl.Spec.Placement = &workspacesv1alpha1.PlacementSpec{
		RuntimeClassName: ptr("gvisor"),
	}
	pod := ensurePodSpecFor(t, tpl, defaultPlacementOptions())
	if pod.Spec.RuntimeClassName == nil || *pod.Spec.RuntimeClassName != "gvisor" {
		t.Fatalf("runtimeClassName = %v, want gvisor", pod.Spec.RuntimeClassName)
	}
	if !reflect.DeepEqual(pod.Spec.NodeSelector, map[string]string{"pool": "default"}) {
		t.Fatalf("nodeSelector = %v, want operator default kept", pod.Spec.NodeSelector)
	}
	if len(pod.Spec.Tolerations) != 1 || pod.Spec.Tolerations[0].Key != "default-taint" {
		t.Fatalf("tolerations = %v, want operator default kept", pod.Spec.Tolerations)
	}
}

func TestBuildPod_LegacyAnnotationStillWorks(t *testing.T) {
	// Annotation alone — it must beat the operator default but lose to the
	// typed spec field (D24: one release of overlap).
	t.Run("annotation wins over default", func(t *testing.T) {
		tpl := testTemplate(map[string]string{
			AnnotationNodeSelector: `{"workload":"annotated"}`,
		})
		pod := ensurePodSpecFor(t, tpl, defaultPlacementOptions())
		if !reflect.DeepEqual(pod.Spec.NodeSelector, map[string]string{"workload": "annotated"}) {
			t.Fatalf("nodeSelector = %v, want annotation value", pod.Spec.NodeSelector)
		}
	})
	t.Run("spec field wins over annotation", func(t *testing.T) {
		tpl := testTemplate(map[string]string{
			AnnotationNodeSelector: `{"workload":"annotated"}`,
		})
		tpl.Spec.Placement = &workspacesv1alpha1.PlacementSpec{
			NodeSelector: map[string]string{"workload": "typed"},
		}
		pod := ensurePodSpecFor(t, tpl, Options{})
		if !reflect.DeepEqual(pod.Spec.NodeSelector, map[string]string{"workload": "typed"}) {
			t.Fatalf("nodeSelector = %v, want spec.placement value", pod.Spec.NodeSelector)
		}
	})
	t.Run("spec placement without selector still honors annotation", func(t *testing.T) {
		// The template sets only runtimeClassName — nodeSelector falls
		// through to the legacy annotation.
		tpl := testTemplate(map[string]string{
			AnnotationNodeSelector: `{"workload":"annotated"}`,
		})
		tpl.Spec.Placement = &workspacesv1alpha1.PlacementSpec{
			RuntimeClassName: ptr("gvisor"),
		}
		pod := ensurePodSpecFor(t, tpl, Options{})
		if !reflect.DeepEqual(pod.Spec.NodeSelector, map[string]string{"workload": "annotated"}) {
			t.Fatalf("nodeSelector = %v, want annotation value", pod.Spec.NodeSelector)
		}
	})
}

func TestBuildPod_HostUsers(t *testing.T) {
	t.Run("template hostUsers=false lands on the pod", func(t *testing.T) {
		tpl := testTemplate(nil)
		tpl.Spec.Linux.HostUsers = ptr(false)
		pod := ensurePodSpecFor(t, tpl, Options{})
		if pod.Spec.HostUsers == nil || *pod.Spec.HostUsers != false {
			t.Fatalf("hostUsers = %v, want false", pod.Spec.HostUsers)
		}
	})
	t.Run("unset stays nil without a default", func(t *testing.T) {
		pod := ensurePodSpecFor(t, testTemplate(nil), Options{})
		if pod.Spec.HostUsers != nil {
			t.Fatalf("hostUsers = %v, want nil", *pod.Spec.HostUsers)
		}
	})
	t.Run("operator default applies when template is silent", func(t *testing.T) {
		pod := ensurePodSpecFor(t, testTemplate(nil), Options{DefaultHostUsers: ptr(false)})
		if pod.Spec.HostUsers == nil || *pod.Spec.HostUsers != false {
			t.Fatalf("hostUsers = %v, want false", pod.Spec.HostUsers)
		}
	})
	t.Run("template wins over operator default", func(t *testing.T) {
		tpl := testTemplate(nil)
		tpl.Spec.Linux.HostUsers = ptr(true)
		pod := ensurePodSpecFor(t, tpl, Options{DefaultHostUsers: ptr(false)})
		if pod.Spec.HostUsers == nil || *pod.Spec.HostUsers != true {
			t.Fatalf("hostUsers = %v, want true", pod.Spec.HostUsers)
		}
	})
}

// TestEnsureRefusesRetainedDataWithoutClaim (FX-R20): a workspace that says
// it consumes retained data but names no retained claim must never get a
// default home — Ensure fails with ErrRetainedClaimMissing before any child
// object (not even the Secret) exists.
func TestEnsureRefusesRetainedDataWithoutClaim(t *testing.T) {
	cases := map[string]map[string]string{
		"no claim at all":   {AnnotationRetainedDataRef: "rd_x"},
		"name without UID":  {AnnotationRetainedDataRef: "rd_x", AnnotationRetainedPVC: "ws-old-home"},
		"UID without name":  {AnnotationRetainedDataRef: "rd_x", AnnotationRetainedPVCUID: "uid"},
		"empty claim value": {AnnotationRetainedDataRef: "rd_x", AnnotationRetainedPVC: "", AnnotationRetainedPVCUID: ""},
	}
	for name, ann := range cases {
		t.Run(name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(backendScheme(t)).Build()
			ws := testWorkspace()
			ws.Spec.DataPolicy = workspacesv1alpha1.DataPolicyRetain
			ws.Annotations = ann
			_, err := New(c, Options{}).Ensure(context.Background(), ws, testTemplate(nil))
			if !errors.Is(err, ErrRetainedClaimMissing) {
				t.Fatalf("Ensure err = %v, want ErrRetainedClaimMissing", err)
			}
			pvcs := &corev1.PersistentVolumeClaimList{}
			secrets := &corev1.SecretList{}
			pods := &corev1.PodList{}
			for _, l := range []client.ObjectList{pvcs, secrets, pods} {
				if err := c.List(context.Background(), l); err != nil {
					t.Fatal(err)
				}
			}
			if len(pvcs.Items)+len(secrets.Items)+len(pods.Items) != 0 {
				t.Fatalf("children created: pvcs=%d secrets=%d pods=%d",
					len(pvcs.Items), len(secrets.Items), len(pods.Items))
			}
		})
	}
}
