// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

// The browser seccomp profile (localhost/profiles/chromium-userns.json)
// lets the workspace user create user and network namespaces — the
// Chromium sandbox needs them. The profile is admitted only when the pod
// runs in its own user namespace (effective hostUsers: false), so what
// the user reaches through those namespaces never runs as a node UID.
// Any other profile is unaffected.

package linux

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestEnsureUsernsProfileRequiresHostUsersFalse(t *testing.T) {
	userns := map[string]string{
		AnnotationSeccompProfile: "localhost/profiles/chromium-userns.json",
	}
	for _, tc := range []struct {
		name     string
		ann      map[string]string
		tplHU    *bool
		opts     Options
		rejected bool
	}{
		{"no operator default, template unset", userns, nil, Options{}, true},
		{"operator default true", userns, nil, Options{DefaultHostUsers: ptr(true)}, true},
		{"template true over default false", userns, ptr(true), Options{DefaultHostUsers: ptr(false)}, true},
		{"operator default false", userns, nil, Options{DefaultHostUsers: ptr(false)}, false},
		{"template false, no default", userns, ptr(false), Options{}, false},
		{"other localhost profile, host users", map[string]string{
			AnnotationSeccompProfile: "localhost/browser-sandbox"}, nil, Options{}, false},
		{"runtime default, host users", nil, nil, Options{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(backendScheme(t)).Build()
			ws := testWorkspace()
			tpl := testTemplate(tc.ann)
			tpl.Spec.Linux.HostUsers = tc.tplHU
			_, err := New(c, tc.opts).Ensure(context.Background(), ws, tpl)
			pod := &corev1.Pod{}
			getErr := c.Get(context.Background(), client.ObjectKey{
				Name: PodName(ws.UID), Namespace: ws.Namespace}, pod)
			if !tc.rejected {
				if err != nil {
					t.Fatalf("Ensure: %v", err)
				}
				if getErr != nil {
					t.Fatalf("pod not created: %v", getErr)
				}
				return
			}
			if !errors.Is(err, ErrTemplateRejected) {
				t.Fatalf("Ensure err = %v, want ErrTemplateRejected", err)
			}
			if !strings.Contains(err.Error(), "hostUsers: false") {
				t.Errorf("rejection %q does not name the hostUsers: false requirement", err)
			}
			if getErr == nil {
				t.Fatalf("rejected template still produced pod %s", pod.Name)
			}
		})
	}
}
