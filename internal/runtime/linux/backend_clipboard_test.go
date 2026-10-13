// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

// The resolved clipboard policy is enforced server-side by the session
// server: buildPod carries spec.sessionPolicy.clipboard verbatim to the
// desktop container as TCDI_CLIPBOARD_POLICY and the session entrypoints
// render the KasmVNC DLP block from it (direction semantics live in
// internal/api/openapi.yaml). The env must reach BOTH adapter paths —
// the linux-base entrypoint and the injected kasm-adapter one — because
// either may serve a template with a non-Disabled policy.
package linux

import (
	"testing"

	cdiv1 "github.com/tinyorbitvn/tinycdi/api/v1"
)

func TestPodCarriesClipboardPolicy(t *testing.T) {
	policies := []cdiv1.ClipboardPolicy{
		cdiv1.ClipboardDisabled,
		cdiv1.ClipboardSend,
		cdiv1.ClipboardReceive,
		cdiv1.ClipboardBidirectional,
	}
	for _, adapter := range []cdiv1.RuntimeAdapter{
		cdiv1.AdapterNone,
		cdiv1.AdapterKasm,
	} {
		for _, policy := range policies {
			tpl := testTemplate(nil)
			tpl.Spec.ClipboardPolicy = policy
			ws := testWorkspace()
			ws.Spec.SessionPolicy = cdiv1.SessionPolicySpec{
				Clipboard:    policy,
				FileTransfer: cdiv1.FileTransferDisabled,
			}
			opts := Options{}
			if adapter == cdiv1.AdapterKasm {
				tpl.Spec.Linux.Adapter = adapter
				opts.KasmAdapterImage = testAdapterImage
			}
			pod := ensurePodSpecWS(t, ws, tpl, opts)
			ctr := pod.Spec.Containers[0]
			v, ok := envOf(&ctr, "TCDI_CLIPBOARD_POLICY")
			if !ok || v != string(policy) {
				t.Errorf("adapter=%q policy=%q: env TCDI_CLIPBOARD_POLICY = %q (present=%v), want %q",
					adapter, policy, v, ok, policy)
			}
		}
	}
}

// TestPodSessionPolicyWins (ADR 0010 §2): spec.sessionPolicy — the meet
// of template policy ∧ tenant ceiling the API stamped — is the only
// source pod building reads; the snapshot's own clipboardPolicy is not
// consulted.
func TestPodSessionPolicyWins(t *testing.T) {
	env := func(ws *cdiv1.Workspace, tpl *cdiv1.WorkspaceTemplate) (string, bool) {
		return envOf(&ensurePodSpecWS(t, ws, tpl, Options{}).Spec.Containers[0], "TCDI_CLIPBOARD_POLICY")
	}

	// Ceiling narrowed the resolved policy to Disabled over a
	// Bidirectional template: the stamped value wins.
	tpl := testTemplate(nil)
	tpl.Spec.ClipboardPolicy = cdiv1.ClipboardBidirectional
	ws := testWorkspace()
	ws.Spec.SessionPolicy = cdiv1.SessionPolicySpec{
		Clipboard:    cdiv1.ClipboardDisabled,
		FileTransfer: cdiv1.FileTransferDisabled,
	}
	if v, ok := env(ws, tpl); !ok || v != "Disabled" {
		t.Fatalf("resolved env = %q (present=%v), want Disabled", v, ok)
	}

	// A resolved Send narrows the bidirectional template.
	ws2 := testWorkspace()
	ws2.Spec.SessionPolicy = cdiv1.SessionPolicySpec{
		Clipboard:    cdiv1.ClipboardSend,
		FileTransfer: cdiv1.FileTransferDisabled,
	}
	if v, ok := env(ws2, tpl); !ok || v != "Send" {
		t.Fatalf("resolved env = %q (present=%v), want Send", v, ok)
	}
}

func TestPodCarriesFileTransferEnv(t *testing.T) {
	for _, adapter := range []cdiv1.RuntimeAdapter{cdiv1.AdapterNone, cdiv1.AdapterKasm} {
		tpl := testTemplate(nil)
		opts := Options{}
		if adapter == cdiv1.AdapterKasm {
			tpl.Spec.Linux.Adapter = adapter
			opts.KasmAdapterImage = testAdapterImage
		}
		ws := testWorkspace()
		ws.Spec.SessionPolicy = cdiv1.SessionPolicySpec{
			Clipboard:    cdiv1.ClipboardDisabled,
			FileTransfer: cdiv1.FileTransferUpload,
			MaxFileBytes: 4096,
		}
		ctr := ensurePodSpecWS(t, ws, tpl, opts).Spec.Containers[0]
		for k, want := range map[string]string{"TCDI_XFER_POLICY": "Upload", "TCDI_XFER_MAX_BYTES": "4096"} {
			if v, ok := envOf(&ctr, k); !ok || v != want {
				t.Errorf("adapter=%q: env %s = %q (present=%v), want %q", adapter, k, v, ok, want)
			}
		}
	}
}
