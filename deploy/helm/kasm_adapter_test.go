// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

// Chart coverage for the kasm workspace adapter (docs/kasm-images.md):
//
//   - --kasm-adapter-image renders on the operator ONLY when
//     kasmAdapter.enabled=true (default false), whether or not release
//     stamping filled kasmAdapter.image.digest; enabled=true requires a
//     digest-pinned image (a tag is refused),
//   - a seeded template with spec.linux.adapter=kasm REQUIRES the adapter
//     to be enabled and pinned or the render fails,
//   - the seeded kasm template carries the digest-pinned kasmweb image,
//     the adapter marker and the sessionCmd verbatim.
package chart_test

import (
	"strings"
	"testing"
)

// TestKasmAdapterFlagRendered: with kasmAdapter.enabled=true and the
// digest set the operator gets
// --kasm-adapter-image=<registry>/<repo>@<digest>.
func TestKasmAdapterFlagRendered(t *testing.T) {
	dep := deployment(render(t, "example-values.yaml"), "operator")
	args := strings.Join(firstContainerArgs(dep), "\n")
	want := "--kasm-adapter-image=registry.lab.example.net/tinycdi/kasm-adapter@" +
		"sha256:7777777777777777777777777777777777777777777777777777777777777777"
	if !strings.Contains(args, want) {
		t.Errorf("operator args missing %q\nargs:\n%s", want, args)
	}
}

// operatorArgs renders the chart with the extra helm args and returns the
// operator's first-container args joined by newlines.
func operatorArgs(t *testing.T, extra ...string) string {
	t.Helper()
	return strings.Join(firstContainerArgs(deployment(renderArgs(t, extra...), "operator")), "\n")
}

const stampedDigest = "sha256:5555555555555555555555555555555555555555555555555555555555555555"

// TestKasmAdapterFlagAbsentWhenDisabled: a release-stamped digest alone
// (what stamp-image-digests.sh writes into the packaged values.yaml) must
// NOT turn the adapter on — kasmAdapter.enabled gates the flag (KASM-2
// condition 1: adapter off by default).
func TestKasmAdapterFlagAbsentWhenDisabled(t *testing.T) {
	for name, extra := range map[string][]string{
		"stamped digest, enabled unset": {"--set", "kasmAdapter.image.digest=" + stampedDigest},
		"stamped digest, enabled=false": {"--set", "kasmAdapter.image.digest=" + stampedDigest,
			"--set", "kasmAdapter.enabled=false"},
	} {
		args := operatorArgs(t, append([]string{"-f", "tinycdi/ci/minimal-values.yaml"}, extra...)...)
		if strings.Contains(args, "--kasm-adapter-image") {
			t.Errorf("%s: --kasm-adapter-image rendered while kasmAdapter.enabled is false\nargs:\n%s", name, args)
		}
	}
}

// TestKasmAdapterFlagRenderedWhenEnabled: enabled=true + digest renders
// the flag with the digest-pinned ref.
func TestKasmAdapterFlagRenderedWhenEnabled(t *testing.T) {
	args := operatorArgs(t, "-f", "tinycdi/ci/minimal-values.yaml",
		"--set", "kasmAdapter.enabled=true",
		"--set", "kasmAdapter.image.digest="+stampedDigest)
	want := "--kasm-adapter-image=ghcr.io/tinyorbitvn/tinycdi-kasm-adapter@" + stampedDigest
	if !strings.Contains(args, want) {
		t.Errorf("operator args missing %q\nargs:\n%s", want, args)
	}
}

// TestKasmAdapterEnabledRequiresDigest: enabling the adapter without a
// digest-pinned image fails the render with a clear message.
func TestKasmAdapterEnabledRequiresDigest(t *testing.T) {
	out := renderErrArgs(t, "-f", "tinycdi/ci/minimal-values.yaml",
		"--set", "kasmAdapter.enabled=true")
	if !strings.Contains(out, "kasmAdapter.enabled=true requires kasmAdapter.image.digest") {
		t.Fatalf("expected render to fail on enabled adapter without digest, got: %s", out)
	}
}

// TestKasmAdapterSeedRequiresEnabled: a seeded adapter=kasm template with
// the adapter disabled is inconsistent (the backend would reject the
// workspaces) and fails the render, even with a digest stamped.
func TestKasmAdapterSeedRequiresEnabled(t *testing.T) {
	out := renderErrArgs(t, "-f", "tinycdi/ci/example-values.yaml",
		"--set", "kasmAdapter.enabled=false")
	if !strings.Contains(out, "kasmAdapter.enabled=true") {
		t.Fatalf("expected render to fail naming kasmAdapter.enabled=true, got: %s", out)
	}
}

func TestKasmAdapterFlagAbsentByDefault(t *testing.T) {
	for _, vf := range []string{"minimal-values.yaml", "security-values.yaml",
		"node-profiles-values.yaml", "template-revision-values.yaml"} {
		dep := deployment(render(t, vf), "operator")
		args := strings.Join(firstContainerArgs(dep), "\n")
		if strings.Contains(args, "--kasm-adapter-image") {
			t.Errorf("%s: --kasm-adapter-image must not render while kasmAdapter.enabled is false\nargs:\n%s", vf, args)
		}
	}
}

// TestKasmAdapterOffByDefault: under every non-kasm ci values file the
// kasm path is entirely off — no --kasm-adapter-image flag on the
// operator and no seeded WorkspaceTemplate with spec.linux.adapter=kasm.
// Kasm support is strictly opt-in (KASM-2 risk acceptance: the operator
// must set kasmAdapter.enabled, the digest pin and a seeded template). Bare values.yaml
// is not covered here on purpose — it fails closed on the
// database.allowedPeers placeholder by design (chart_hardening_test.go).
func TestKasmAdapterOffByDefault(t *testing.T) {
	for _, vf := range []string{"minimal-values.yaml", "security-values.yaml",
		"node-profiles-values.yaml", "template-revision-values.yaml"} {
		docs := render(t, vf)
		args := strings.Join(firstContainerArgs(deployment(docs, "operator")), "\n")
		if strings.Contains(args, "--kasm-adapter-image") {
			t.Errorf("%s: --kasm-adapter-image must not render while kasmAdapter.enabled is false\nargs:\n%s", vf, args)
		}
		for _, d := range selectDocs(docs, "WorkspaceTemplate") {
			spec, _ := d["spec"].(map[string]any)
			linux, _ := spec["linux"].(map[string]any)
			if linux["adapter"] == "kasm" {
				name, _ := meta(d)
				t.Errorf("%s: seeds kasm template %q — kasm must be opt-in", vf, name)
			}
		}
	}
}

// TestKasmAdapterFlagGlobalRegistry: the adapter image resolves through
// the same registry machinery as every other image — global.imageRegistry
// prefixes the repository.
func TestKasmAdapterFlagGlobalRegistry(t *testing.T) {
	digest := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	dep := deployment(renderArgs(t,
		"-f", "tinycdi/ci/minimal-values.yaml",
		"--set", "global.imageRegistry=mirror.example.net",
		"--set", "kasmAdapter.enabled=true",
		"--set", "kasmAdapter.image.digest="+digest), "operator")
	args := strings.Join(firstContainerArgs(dep), "\n")
	want := "--kasm-adapter-image=mirror.example.net/tinyorbitvn/tinycdi-kasm-adapter@" + digest
	if !strings.Contains(args, want) {
		t.Errorf("operator args missing %q\nargs:\n%s", want, args)
	}
}

// TestKasmAdapterSeedRequiresDigest: a seeded adapter=kasm template with
// no pinned adapter image must fail the render — the backend would
// reject the workspaces anyway (no --kasm-adapter-image), so the values
// are simply inconsistent.
func TestKasmAdapterSeedRequiresDigest(t *testing.T) {
	out := renderErrArgs(t,
		"-f", "tinycdi/ci/example-values.yaml",
		"--set", "kasmAdapter.image.digest=null")
	if !strings.Contains(out, "kasmAdapter.image.digest") {
		t.Fatalf("expected render to fail naming kasmAdapter.image.digest, got: %s", out)
	}
}

// TestKasmAdapterTagWithoutDigestRefused: the flag must only ever carry a
// digest ref — a tag-only adapter image fails the render outright.
func TestKasmAdapterTagWithoutDigestRefused(t *testing.T) {
	out := renderErrArgs(t,
		"-f", "tinycdi/ci/minimal-values.yaml",
		"--set", "kasmAdapter.image.tag=1.2.3")
	if !strings.Contains(out, "digest-pinned") {
		t.Fatalf("expected render to fail on tag-only adapter image, got: %s", out)
	}
}

// TestKasmTemplateRendersVerbatim: the seeded adapter=kasm template
// carries the digest-pinned kasmweb image, the adapter enum and the
// sessionCmd verbatim through spec pass-through, plus the Localhost
// profile annotations.
func TestKasmTemplateRendersVerbatim(t *testing.T) {
	docs := render(t, "example-values.yaml")
	var kasm doc
	for _, d := range selectDocs(docs, "WorkspaceTemplate") {
		name, _ := meta(d)
		if strings.HasPrefix(name, "kasm-chromium-") {
			kasm = d
		}
	}
	if kasm == nil {
		t.Fatal("no kasm-chromium-* WorkspaceTemplate rendered")
	}
	spec, _ := kasm["spec"].(map[string]any)
	linux, _ := spec["linux"].(map[string]any)
	if linux["adapter"] != "kasm" {
		t.Errorf("spec.linux.adapter = %v, want kasm", linux["adapter"])
	}
	if img, _ := linux["image"].(string); img !=
		"kasmweb/chromium@sha256:c50132c99d265b78e0cbc091a8fade0e8e814f5928d634db36bd8c1649bb41f0" {
		t.Errorf("spec.linux.image = %q, want the cataloged digest-pinned kasmweb/chromium ref", img)
	}
	if cmd, _ := linux["sessionCmd"].(string); cmd !=
		"/usr/bin/chromium-orig --start-maximized https://start.lab.example.net" {
		t.Errorf("spec.linux.sessionCmd = %q", cmd)
	}
	ann, _ := kasm["metadata"].(map[string]any)["annotations"].(map[string]any)
	for k, want := range map[string]string{
		"workspaces.cdi.tinyorbit.vn/seccomp-profile":  "localhost/profiles/chromium-userns.json",
		"workspaces.cdi.tinyorbit.vn/apparmor-profile": "localhost/tinycdi-browser",
	} {
		if ann[k] != want {
			t.Errorf("annotation %s = %v, want %q", k, ann[k], want)
		}
	}
}
