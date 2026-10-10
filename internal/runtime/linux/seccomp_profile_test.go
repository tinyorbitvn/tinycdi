// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

// v1.0 (SR-3 blast-radius follow-up): the seccomp-profile template
// annotation now goes through the same policy gate as apparmor-profile —
// "localhost/<name>" is accepted only when the name is a clean relative
// path under the kubelet's seccomp root, "runtime/default" (or unset)
// means RuntimeDefault, and everything else rejects the template with
// ErrTemplateRejected before any child object exists. Before this gate
// the name was prefix-checked only: "localhost/../<anything>" — or any
// value — reached the pod's securityContext untouched, and an invalid
// non-localhost value silently fell back to RuntimeDefault.

package linux

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestSeccompProfileAccepted(t *testing.T) {
	rd := &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}
	for _, tc := range []struct {
		name string
		ann  map[string]string
		want *corev1.SeccompProfile
	}{
		{"unset", nil, rd},
		{"explicit runtime/default", map[string]string{
			AnnotationSeccompProfile: "runtime/default"}, rd},
		// The chart's seeded browser profile: a path under the seccomp
		// root's profiles/ subtree.
		{"localhost subtree path", map[string]string{
			AnnotationSeccompProfile: "localhost/profiles/chromium-userns.json"},
			&corev1.SeccompProfile{
				Type:             corev1.SeccompProfileTypeLocalhost,
				LocalhostProfile: ptr("profiles/chromium-userns.json")}},
		{"localhost bare name", map[string]string{
			AnnotationSeccompProfile: "localhost/browser-sandbox"},
			&corev1.SeccompProfile{
				Type:             corev1.SeccompProfileTypeLocalhost,
				LocalhostProfile: ptr("browser-sandbox")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveSeccompProfile(tc.ann)
			if err != nil {
				t.Fatalf("resolveSeccompProfile: %v", err)
			}
			if got.Type != tc.want.Type {
				t.Fatalf("seccomp type = %v, want %v", got.Type, tc.want.Type)
			}
			if tc.want.LocalhostProfile == nil {
				if got.LocalhostProfile != nil {
					t.Fatalf("localhostProfile = %v, want nil", *got.LocalhostProfile)
				}
				return
			}
			if got.LocalhostProfile == nil || *got.LocalhostProfile != *tc.want.LocalhostProfile {
				t.Fatalf("localhostProfile = %v, want %v", got.LocalhostProfile, *tc.want.LocalhostProfile)
			}
		})
	}
}

func TestSeccompProfileRejected(t *testing.T) {
	for _, v := range []string{
		"unconfined",
		"docker/default",
		"localhostname",
		"localhost/",
		"localhost//",
		"localhost/../escape",
		"localhost/a/../../escape",
		"localhost/./dot",
		"localhost/a//b",
		"localhost/trailing/",
		"/localhost/absolute",
		"localhost/with space",
		"localhost/with\ttab",
		"localhost/with\nnewline",
		"localhost/with\\backslash",
		"localhost/a/./b",
		"localhost/a/../b",
	} {
		if _, err := resolveSeccompProfile(map[string]string{
			AnnotationSeccompProfile: v}); !errors.Is(err, ErrTemplateRejected) {
			t.Fatalf("seccomp-profile %q: err = %v, want ErrTemplateRejected", v, err)
		}
	}
}

// An out-of-policy seccomp annotation rejects the template in Ensure
// before ANY child object exists — no pod, and the error maps to the
// Degraded/TemplateRejected surface exactly like a bad AppArmor value.
func TestEnsureRejectsBadSeccompAnnotation(t *testing.T) {
	for _, v := range []string{
		"localhost/../escape",
		"unconfined",
		"localhost/",
	} {
		t.Run(v, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(backendScheme(t)).Build()
			ws := testWorkspace()
			tpl := testTemplate(map[string]string{AnnotationSeccompProfile: v})
			if _, err := New(c, Options{}).Ensure(context.Background(), ws, tpl); !errors.Is(err, ErrTemplateRejected) {
				t.Fatalf("Ensure err = %v, want ErrTemplateRejected", err)
			}
			pod := &corev1.Pod{}
			if err := c.Get(context.Background(), client.ObjectKey{
				Name: PodName(ws.UID), Namespace: ws.Namespace}, pod); err == nil {
				t.Fatalf("rejected template still produced pod %s", pod.Name)
			}
		})
	}
}

// A valid localhost profile still reaches the pod — the gate widens
// nothing, it only refuses what was never validated.
func TestBuildPodSeccompLocalhostHonored(t *testing.T) {
	pod := ensurePodSpec(t, map[string]string{
		AnnotationSeccompProfile: "localhost/profiles/chromium-userns.json",
	})
	sc := pod.Spec.Containers[0].SecurityContext.SeccompProfile
	if sc == nil || sc.Type != corev1.SeccompProfileTypeLocalhost ||
		sc.LocalhostProfile == nil || *sc.LocalhostProfile != "profiles/chromium-userns.json" {
		t.Fatalf("container seccompProfile = %+v, want Localhost profiles/chromium-userns.json", sc)
	}
}

// A caller that forgets the resolved profile gets the hardened default,
// never an unset field (nil seccompProfile is unconfined on runtimes
// that do not default it).
func TestBuildPodNilSeccompDefaultsRuntimeDefault(t *testing.T) {
	pod := buildPod(testWorkspace(), testTemplate(nil), nil, nil, Options{})
	sc := pod.Spec.Containers[0].SecurityContext.SeccompProfile
	if sc == nil || sc.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Fatalf("container seccompProfile = %+v, want RuntimeDefault", sc)
	}
}
