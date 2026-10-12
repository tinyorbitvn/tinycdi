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
	"testing"

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
