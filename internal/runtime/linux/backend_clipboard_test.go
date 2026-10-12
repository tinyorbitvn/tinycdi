// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

// The template clipboard policy is enforced server-side by the session
// server: buildPod carries spec.clipboardPolicy verbatim to the desktop
// container as TCDI_CLIPBOARD_POLICY and the session entrypoints render
// the KasmVNC DLP block from it (direction semantics live in
// internal/api/openapi.yaml). The env must reach BOTH adapter paths —
// the linux-base entrypoint and the injected kasm-adapter one — because
// either may serve a template with a non-Disabled policy.
package linux

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	workspacesv1alpha1 "github.com/tinyorbitvn/tinycdi/api/v1alpha1"
)

func TestPodCarriesClipboardPolicy(t *testing.T) {
	policies := []workspacesv1alpha1.ClipboardPolicy{
		workspacesv1alpha1.ClipboardDisabled,
		workspacesv1alpha1.ClipboardSend,
		workspacesv1alpha1.ClipboardReceive,
		workspacesv1alpha1.ClipboardBidirectional,
	}
	for _, adapter := range []workspacesv1alpha1.RuntimeAdapter{
		workspacesv1alpha1.AdapterNone,
		workspacesv1alpha1.AdapterKasm,
	} {
		for _, policy := range policies {
			tpl := testTemplate(nil)
			tpl.Spec.ClipboardPolicy = policy
			opts := Options{}
			if adapter == workspacesv1alpha1.AdapterKasm {
				tpl.Spec.Linux.Adapter = adapter
				opts.KasmAdapterImage = testAdapterImage
			}
			pod := ensurePodSpecFor(t, tpl, opts)
			ctr := pod.Spec.Containers[0]
			v, ok := envOf(&ctr, "TCDI_CLIPBOARD_POLICY")
			if !ok || v != string(policy) {
				t.Errorf("adapter=%q policy=%q: env TCDI_CLIPBOARD_POLICY = %q (present=%v), want %q",
					adapter, policy, v, ok, policy)
			}
		}
	}
}

// TestPodSessionPolicyWins (ADR 0010 §2): the resolved policy the API
// stamped as spec.sessionPolicy — template policy ∧ tenant ceiling —
// wins over the snapshot's own clipboardPolicy; a CR written before the
// field existed falls back to the snapshot.
func TestPodSessionPolicyWins(t *testing.T) {
	mkPod := func(ws *workspacesv1alpha1.Workspace, tpl *workspacesv1alpha1.WorkspaceTemplate) *corev1.Pod {
		c := fake.NewClientBuilder().WithScheme(backendScheme(t)).Build()
		if _, err := New(c, Options{}).Ensure(context.Background(), ws, tpl); err != nil {
			t.Fatalf("Ensure: %v", err)
		}
		pod := &corev1.Pod{}
		if err := c.Get(context.Background(),
			client.ObjectKey{Name: PodName(ws.UID), Namespace: ws.Namespace}, pod); err != nil {
			t.Fatalf("pod not created: %v", err)
		}
		return pod
	}
	env := func(ws *workspacesv1alpha1.Workspace, tpl *workspacesv1alpha1.WorkspaceTemplate) (string, bool) {
		return envOf(&mkPod(ws, tpl).Spec.Containers[0], "TCDI_CLIPBOARD_POLICY")
	}

	// Ceiling narrowed the resolved policy to Disabled over a
	// Bidirectional template: the stamped value wins.
	tpl := testTemplate(nil)
	tpl.Spec.ClipboardPolicy = workspacesv1alpha1.ClipboardBidirectional
	ws := testWorkspace()
	ws.Spec.SessionPolicy = &workspacesv1alpha1.SessionPolicySpec{
		Clipboard: workspacesv1alpha1.ClipboardDisabled,
	}
	if v, ok := env(ws, tpl); !ok || v != "Disabled" {
		t.Fatalf("resolved env = %q (present=%v), want Disabled", v, ok)
	}

	// A resolved Send narrows the bidirectional template.
	ws2 := testWorkspace()
	ws2.Spec.SessionPolicy = &workspacesv1alpha1.SessionPolicySpec{
		Clipboard: workspacesv1alpha1.ClipboardSend,
	}
	if v, ok := env(ws2, tpl); !ok || v != "Send" {
		t.Fatalf("resolved env = %q (present=%v), want Send", v, ok)
	}

	// A pre-field CR carries no sessionPolicy: the snapshot's own policy
	// applies, exactly as before.
	ws3 := testWorkspace()
	if v, ok := env(ws3, tpl); !ok || v != "Bidirectional" {
		t.Fatalf("pre-field fallback env = %q (present=%v), want Bidirectional", v, ok)
	}
}
