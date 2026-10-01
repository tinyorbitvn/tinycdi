// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

// Chart coverage for the kasm workspace adapter (docs/kasm-images.md):
//
//   - kasmAdapter.image.digest renders --kasm-adapter-image on the
//     operator (digest-pinned only — a tag is refused),
//   - a seeded template with spec.linux.adapter=kasm REQUIRES the pinned
//     adapter image or the render fails,
//   - the seeded kasm template carries the digest-pinned kasmweb image,
//     the adapter marker and the sessionCmd verbatim.
package chart_test

import (
	"strings"
	"testing"
)

// TestKasmAdapterFlagRendered: with kasmAdapter.image.digest set the
// operator gets --kasm-adapter-image=<registry>/<repo>@<digest>; without
// it the flag is absent entirely.
func TestKasmAdapterFlagRendered(t *testing.T) {
	dep := deployment(render(t, "example-values.yaml"), "operator")
	args := strings.Join(firstContainerArgs(dep), "\n")
	want := "--kasm-adapter-image=registry.lab.example.net/tinycdi/kasm-adapter@" +
		"sha256:7777777777777777777777777777777777777777777777777777777777777777"
	if !strings.Contains(args, want) {
		t.Errorf("operator args missing %q\nargs:\n%s", want, args)
	}
}

func TestKasmAdapterFlagAbsentByDefault(t *testing.T) {
	for _, vf := range []string{"minimal-values.yaml", "security-values.yaml",
		"node-profiles-values.yaml", "template-revision-values.yaml"} {
		dep := deployment(render(t, vf), "operator")
		args := strings.Join(firstContainerArgs(dep), "\n")
		if strings.Contains(args, "--kasm-adapter-image") {
			t.Errorf("%s: --kasm-adapter-image must not render without kasmAdapter.image.digest\nargs:\n%s", vf, args)
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
