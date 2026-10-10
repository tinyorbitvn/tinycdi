// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

// V3.11a: AppArmor RuntimeDefault is a cluster-wide setting
// (--runtime-apparmor-require-default). Default (required) renders pods
// exactly as before; when not required, buildPod leaves appArmorProfile nil
// where it would have set RuntimeDefault, while a Localhost profile from the
// template annotation is always kept and every other hardening field is
// untouched.
package linux

import (
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"

	workspacesv1alpha1 "github.com/tinyorbitvn/tinycdi/api/v1alpha1"
)

func aaBuild(t *testing.T, annotations map[string]string, kasm bool, opts Options) *corev1.Pod {
	t.Helper()
	tpl := testTemplate(annotations)
	if kasm {
		tpl.Spec.Linux.Adapter = workspacesv1alpha1.AdapterKasm
		opts.KasmAdapterImage = testAdapterImage
	}
	aa, err := resolveAppArmorProfile(tpl.Annotations)
	if err != nil {
		t.Fatalf("resolveAppArmorProfile: %v", err)
	}
	sc, err := resolveSeccompProfile(tpl.Annotations)
	if err != nil {
		t.Fatalf("resolveSeccompProfile: %v", err)
	}
	return buildPod(testWorkspace(), tpl, aa, sc, opts)
}

// wantSecurityContext is the golden container security context of the
// desktop container; aa is the only field the setting may vary.
func wantSecurityContext(aa *corev1.AppArmorProfile) *corev1.SecurityContext {
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: ptr(false),
		RunAsNonRoot:             ptr(true),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		AppArmorProfile:          aa,
	}
}

func TestBuildPod_AppArmorDefaultUnchanged(t *testing.T) {
	rd := &corev1.AppArmorProfile{Type: corev1.AppArmorProfileTypeRuntimeDefault}
	for name, opts := range map[string]Options{
		"zero options":     {},
		"explicit require": {AppArmorNotRequired: false},
	} {
		t.Run(name, func(t *testing.T) {
			pod := aaBuild(t, nil, false, opts)
			if got := pod.Spec.Containers[0].SecurityContext; !reflect.DeepEqual(got, wantSecurityContext(rd)) {
				t.Fatalf("container securityContext =\n%+v\nwant\n%+v", got, wantSecurityContext(rd))
			}
			wantPod := &corev1.PodSecurityContext{
				RunAsNonRoot:   ptr(true),
				RunAsUser:      ptr(int64(1000)),
				RunAsGroup:     ptr(int64(1000)),
				FSGroup:        ptr(int64(1000)),
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			}
			if !reflect.DeepEqual(pod.Spec.SecurityContext, wantPod) {
				t.Fatalf("pod securityContext = %+v, want %+v", pod.Spec.SecurityContext, wantPod)
			}
			// The adapter init container keeps its explicit RuntimeDefault.
			kasm := aaBuild(t, nil, true, opts)
			if got := kasm.Spec.InitContainers[0].SecurityContext.AppArmorProfile; !reflect.DeepEqual(got, rd) {
				t.Fatalf("adapter init appArmorProfile = %+v, want RuntimeDefault", got)
			}
		})
	}
}

func TestBuildPod_AppArmorNotRequired_RuntimeDefaultOmitted(t *testing.T) {
	opts := Options{AppArmorNotRequired: true}
	for name, ann := range map[string]map[string]string{
		"annotation absent":      nil,
		"annotation runtime/def": {AnnotationAppArmorProfile: "runtime/default"},
	} {
		t.Run(name, func(t *testing.T) {
			pod := aaBuild(t, ann, false, opts)
			if aa := pod.Spec.Containers[0].SecurityContext.AppArmorProfile; aa != nil {
				t.Fatalf("container appArmorProfile = %+v, want nil", aa)
			}
		})
	}
	// The kasm adapter init container sets RuntimeDefault explicitly and
	// would be refused on the same hosts, so it follows the setting.
	kasm := aaBuild(t, nil, true, opts)
	if aa := kasm.Spec.InitContainers[0].SecurityContext.AppArmorProfile; aa != nil {
		t.Fatalf("adapter init appArmorProfile = %+v, want nil", aa)
	}
}

func TestBuildPod_AppArmorNotRequired_LocalhostKept(t *testing.T) {
	pod := aaBuild(t, map[string]string{AnnotationAppArmorProfile: "localhost/tinycdi-browser"},
		false, Options{AppArmorNotRequired: true})
	aa := pod.Spec.Containers[0].SecurityContext.AppArmorProfile
	if aa == nil || aa.Type != corev1.AppArmorProfileTypeLocalhost ||
		aa.LocalhostProfile == nil || *aa.LocalhostProfile != "tinycdi-browser" {
		t.Fatalf("container appArmorProfile = %+v, want Localhost tinycdi-browser", aa)
	}
}

func TestBuildPod_AppArmorNotRequired_OtherHardeningUnchanged(t *testing.T) {
	for name, ann := range map[string]map[string]string{
		"runtime default": nil,
		"seccomp localhost": {
			AnnotationSeccompProfile: "localhost/profiles/chromium-userns.json",
		},
		"apparmor localhost": {
			AnnotationAppArmorProfile: "localhost/tinycdi-browser",
		},
	} {
		for _, kasm := range []bool{false, true} {
			req := aaBuild(t, ann, kasm, Options{})
			not := aaBuild(t, ann, kasm, Options{AppArmorNotRequired: true})

			// Everything except the RuntimeDefault AppArmor field is
			// byte-identical: normalise it on the required pod and compare
			// the whole pod.
			want := req.DeepCopy()
			clearRuntimeDefaultAppArmor(want)
			if !reflect.DeepEqual(not, want) {
				t.Errorf("%s kasm=%v: pod differs beyond appArmorProfile:\n got %+v\nwant %+v", name, kasm, not.Spec, want.Spec)
			}
			if not.Spec.Containers[0].SecurityContext.SeccompProfile == nil ||
				not.Spec.Containers[0].SecurityContext.Capabilities == nil ||
				*not.Spec.Containers[0].SecurityContext.AllowPrivilegeEscalation ||
				!*not.Spec.Containers[0].SecurityContext.RunAsNonRoot {
				t.Errorf("%s kasm=%v: hardening lost: %+v", name, kasm, not.Spec.Containers[0].SecurityContext)
			}
		}
	}
	// hostUsers is the operator default, independent of the AppArmor setting.
	hu := false
	not := aaBuild(t, nil, false, Options{AppArmorNotRequired: true, DefaultHostUsers: &hu})
	if not.Spec.HostUsers == nil || *not.Spec.HostUsers {
		t.Fatalf("hostUsers = %v, want false", not.Spec.HostUsers)
	}
}

func clearRuntimeDefaultAppArmor(p *corev1.Pod) {
	strip := func(cs []corev1.Container) {
		for i := range cs {
			if sc := cs[i].SecurityContext; sc != nil && sc.AppArmorProfile != nil &&
				sc.AppArmorProfile.Type == corev1.AppArmorProfileTypeRuntimeDefault {
				sc.AppArmorProfile = nil
			}
		}
	}
	strip(p.Spec.Containers)
	strip(p.Spec.InitContainers)
}
