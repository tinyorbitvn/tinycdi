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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	cdiv1 "github.com/tinyorbitvn/tinycdi/api/v1"
)

func backendScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := cdiv1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func testWorkspace() *cdiv1.Workspace {
	return &cdiv1.Workspace{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ws-test",
			Namespace: "tenant-a",
			UID:       types.UID("11111111-2222-3333-4444-555555555555"),
		},
		Spec: cdiv1.WorkspaceSpec{
			DataPolicy:        cdiv1.DataPolicyEphemeral,
			RuntimeGeneration: 1,
		},
	}
}

func testTemplate(annotations map[string]string) *cdiv1.WorkspaceTemplate {
	return &cdiv1.WorkspaceTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "tpl-test",
			Namespace:   "tenant-a",
			Annotations: annotations,
		},
		Spec: cdiv1.WorkspaceTemplateSpec{
			Linux: &cdiv1.LinuxRuntimeSpec{
				Image: "registry.example.com/tinycdi/linux-desktop@sha256:" +
					"0000000000000000000000000000000000000000000000000000000000000000",
			},
			Resources: cdiv1.ResourceProfile{
				CPU:     resource.MustParse("500m"),
				Memory:  resource.MustParse("1Gi"),
				Storage: resource.MustParse("1Gi"),
			},
			NetworkProfile: cdiv1.NetworkProfileIsolated,
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
func ensurePodSpecFor(t *testing.T, tpl *cdiv1.WorkspaceTemplate, opts Options) *corev1.Pod {
	t.Helper()
	return ensurePodSpecWS(t, testWorkspace(), tpl, opts)
}

// ensurePodSpecWS is ensurePodSpecFor with an explicit workspace — the
// session-policy tests vary what the API stamped on spec.sessionPolicy.
func ensurePodSpecWS(t *testing.T, ws *cdiv1.Workspace, tpl *cdiv1.WorkspaceTemplate, opts Options) *corev1.Pod {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(backendScheme(t)).Build()
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
// operator's Options defaults. A field the template sets REPLACES the
// default; it is never merged. The v0.1 node-selector template annotation is
// gone (E14) — TestBuildPod_AnnotationNoLongerHonoured pins its removal.

var poolToleration = corev1.Toleration{
	Key:      "cdi.tinyorbit.vn/workspace",
	Operator: corev1.TolerationOpEqual,
	Value:    "true",
	Effect:   corev1.TaintEffectNoSchedule,
}

func defaultPlacementOptions() Options {
	return Options{DefaultPlacement: cdiv1.PlacementSpec{
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
	tpl.Spec.Placement = &cdiv1.PlacementSpec{
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
	tpl.Spec.Placement = &cdiv1.PlacementSpec{
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

func TestBuildPod_AnnotationNoLongerHonoured(t *testing.T) {
	// The v0.1 workspaces.cdi.tinyorbit.vn/node-selector annotation is gone
	// (E14): a template carrying it gets the same placement as one without —
	// operator default below, spec.placement field when set.
	const legacyAnnotation = "workspaces.cdi.tinyorbit.vn/node-selector"
	t.Run("annotation loses to operator default", func(t *testing.T) {
		tpl := testTemplate(map[string]string{
			legacyAnnotation: `{"workload":"annotated"}`,
		})
		pod := ensurePodSpecFor(t, tpl, defaultPlacementOptions())
		if !reflect.DeepEqual(pod.Spec.NodeSelector, map[string]string{"pool": "default"}) {
			t.Fatalf("nodeSelector = %v, want operator default — annotation must be ignored", pod.Spec.NodeSelector)
		}
	})
	t.Run("annotation loses to empty options", func(t *testing.T) {
		tpl := testTemplate(map[string]string{
			legacyAnnotation: `{"workload":"annotated"}`,
		})
		pod := ensurePodSpecFor(t, tpl, Options{})
		if len(pod.Spec.NodeSelector) != 0 {
			t.Fatalf("nodeSelector = %v, want empty — annotation must be ignored", pod.Spec.NodeSelector)
		}
	})
	t.Run("annotation does not leak into other fields", func(t *testing.T) {
		// The annotation never carried tolerations/runtimeClass, but a
		// template that sets only runtimeClassName must not resurrect it.
		tpl := testTemplate(map[string]string{
			legacyAnnotation: `{"workload":"annotated"}`,
		})
		tpl.Spec.Placement = &cdiv1.PlacementSpec{
			RuntimeClassName: ptr("gvisor"),
		}
		pod := ensurePodSpecFor(t, tpl, Options{})
		if len(pod.Spec.NodeSelector) != 0 {
			t.Fatalf("nodeSelector = %v, want empty — annotation must be ignored", pod.Spec.NodeSelector)
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
			ws.Spec.DataPolicy = cdiv1.DataPolicyRetain
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

// ClusterOnly egress defaults to RESTRICTED in v1.0: with no operator
// narrowing configured the rule confines egress to pods in namespaces
// carrying the managed-tenant label — never the pre-1.0 any-pod-anywhere
// posture. PodSelector stays unset and no ports are pinned; DNS is
// unaffected (it leads the egress list).
func TestClusterOnlyEgress_DefaultRestricted(t *testing.T) {
	rule := clusterOnlyEgressRule(Options{})
	if len(rule.To) != 1 || rule.To[0].NamespaceSelector == nil {
		t.Fatalf("%+v", rule)
	}
	me := rule.To[0].NamespaceSelector.MatchExpressions
	if len(me) != 1 || me[0].Key != "workspaces.cdi.tinyorbit.vn/tenant" || me[0].Operator != metav1.LabelSelectorOpExists {
		t.Fatalf("%+v", me)
	}
	if rule.To[0].PodSelector != nil || len(rule.Ports) != 0 {
		t.Fatalf("%+v", rule)
	}
}

// Unrestricted is the explicit opt-out of the restricted default: the
// rule keeps pod peers only — any pod in any namespace — and can never
// admit external IPs.
func TestClusterOnlyEgress_UnrestrictedOptIn(t *testing.T) {
	rule := clusterOnlyEgressRule(Options{ClusterOnlyEgress: &ClusterOnlyEgress{Unrestricted: true}})
	if len(rule.To) != 2 {
		t.Fatalf("%+v", rule)
	} // any pod, any namespace (pod peers only)
}

func TestObserve_ConnectionReadyNeedsReadyEndpointOfThisPod(t *testing.T) {
	cases := []struct {
		name  string
		slice func(pod *corev1.Pod, svc string) *discoveryv1.EndpointSlice
		want  bool
	}{
		{"no slice yet", func(*corev1.Pod, string) *discoveryv1.EndpointSlice { return nil }, false},
		{"slice for this pod, not ready", func(p *corev1.Pod, svc string) *discoveryv1.EndpointSlice {
			s := ReadyEndpointSlice(p, svc)
			s.Endpoints[0].Conditions.Ready = ptr(false)
			return s
		}, false},
		{"slice for an old pod uid", func(p *corev1.Pod, svc string) *discoveryv1.EndpointSlice {
			s := ReadyEndpointSlice(p, svc)
			s.Endpoints[0].TargetRef.UID = "old-pod-uid"
			return s
		}, false},
		{"ready slice for this pod", ReadyEndpointSlice, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			ws := testWorkspace()
			kc := fake.NewClientBuilder().WithScheme(backendScheme(t)).Build()
			b := New(kc, Options{})
			if _, err := b.Ensure(ctx, ws, testTemplate(nil)); err != nil {
				t.Fatalf("Ensure: %v", err)
			}
			pod := &corev1.Pod{}
			if err := kc.Get(ctx, client.ObjectKey{Name: PodName(ws.UID), Namespace: ws.Namespace}, pod); err != nil {
				t.Fatalf("pod: %v", err)
			}
			pod.UID = types.UID("pod-uid-1")
			if err := kc.Update(ctx, pod); err != nil {
				t.Fatalf("stamp pod uid: %v", err)
			}
			pod.Status.Phase = corev1.PodRunning
			pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
			if err := kc.Status().Update(ctx, pod); err != nil {
				t.Fatalf("mark pod ready: %v", err)
			}
			if s := c.slice(pod, ServiceName(ws.UID)); s != nil {
				if err := kc.Create(ctx, s); err != nil {
					t.Fatalf("create slice: %v", err)
				}
			}
			obs, err := b.Observe(ctx, ws)
			if err != nil {
				t.Fatal(err)
			}
			if !obs.RuntimeReady {
				t.Fatalf("RuntimeReady = false, want true (the pod is Ready)")
			}
			if obs.ConnectionReady != c.want {
				t.Errorf("ConnectionReady = %v, want %v (reason %q)", obs.ConnectionReady, c.want, obs.Reason)
			}
			if !c.want && obs.Reason != "WaitingForEndpoint" {
				t.Errorf("reason %q, want WaitingForEndpoint", obs.Reason)
			}
		})
	}
}

func TestEnsureSecret_CarriesXferTokenAndNeverRotatesIt(t *testing.T) {
	ws := testWorkspace()
	c := fake.NewClientBuilder().WithScheme(backendScheme(t)).WithObjects(ws).Build()
	b := New(c, defaultPlacementOptions())
	sec, err := b.ensureSecret(context.Background(), ws)
	if err != nil {
		t.Fatal(err)
	}
	tok := strings.TrimSpace(string(sec.Data["xfer-token"]))
	if len(tok) != 43 || tok == strings.TrimSpace(string(sec.Data["password"])) {
		t.Fatalf("xfer-token = %q; want 32 random bytes, base64url, distinct from the password", tok)
	}
	again, err := b.ensureSecret(context.Background(), ws)
	if err != nil || string(again.Data["xfer-token"]) != string(sec.Data["xfer-token"]) {
		t.Fatalf("xfer-token rotated on the second reconcile (%v)", err)
	}
}
