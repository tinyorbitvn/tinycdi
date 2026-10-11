// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package chart_test

// ADR 0009 (SR-3-F5) chart legs: backend.tls.internal.revokedSerials is
// additive — unset renders byte-identical (the default-render golden
// already proves it), set adds exactly the Secret volume projected to a
// fixed filename, the read-only mount and the -internal-revoked-list arg.

import (
	"path/filepath"
	"strings"
	"testing"
)

// revokedRender renders the backend Deployment with the blocklist Secret
// set and returns its pod spec pieces.
func revokedRender(t *testing.T, extra ...string) doc {
	t.Helper()
	args := []string{
		"-f", filepath.Join("tinycdi", "ci", "minimal-values.yaml"),
		"--set", "backend.tls.internal.revokedSerials.existingSecret=tinycdi-revoked-serials",
	}
	args = append(args, extra...)
	docs := renderArgs(t, args...)
	d := deployment(docs, "backend")
	if d == nil {
		t.Fatal("no backend Deployment rendered")
	}
	return d
}

// TestRevokedSerialsRenderOff: without revokedSerials the backend
// Deployment carries neither the arg, the mount nor the volume.
func TestRevokedSerialsRenderOff(t *testing.T) {
	d := deployment(render(t, "minimal-values.yaml"), "backend")
	if d == nil {
		t.Fatal("no backend Deployment rendered")
	}
	for _, a := range firstContainerArgs(d) {
		if strings.HasPrefix(a, "-internal-revoked-list") {
			t.Fatalf("revocation arg rendered with the feature unset: %s", a)
		}
	}
	spec, _ := d["spec"].(map[string]any)
	tpl, _ := spec["template"].(map[string]any)
	podSpec, _ := tpl["spec"].(map[string]any)
	for _, v := range toSlice(podSpec["volumes"]) {
		if m, _ := v.(map[string]any); m["name"] == "internal-revoked" {
			t.Fatal("internal-revoked volume rendered with the feature unset")
		}
	}
}

// TestRevokedSerialsRenderOn: setting the Secret renders the arg pointing
// at the fixed filename, the read-only mount and the projected volume —
// the same pattern client-ca uses.
func TestRevokedSerialsRenderOn(t *testing.T) {
	d := revokedRender(t)

	args := firstContainerArgs(d)
	found := false
	for _, a := range args {
		if a == "-internal-revoked-list=/etc/internal-revoked/revoked-serials.txt" {
			found = true
		}
	}
	if !found {
		t.Fatalf("-internal-revoked-list arg missing, args=%v", args)
	}

	cm := firstContainer(d)
	foundMount := false
	for _, v := range toSlice(cm["volumeMounts"]) {
		m, _ := v.(map[string]any)
		if m["name"] == "internal-revoked" {
			foundMount = true
			if m["mountPath"] != "/etc/internal-revoked" || m["readOnly"] != true {
				t.Fatalf("internal-revoked mount = %v", m)
			}
		}
	}
	if !foundMount {
		t.Fatal("internal-revoked volumeMount missing")
	}

	spec, _ := d["spec"].(map[string]any)
	tpl, _ := spec["template"].(map[string]any)
	podSpec, _ := tpl["spec"].(map[string]any)
	foundVol := false
	for _, v := range toSlice(podSpec["volumes"]) {
		m, _ := v.(map[string]any)
		if m["name"] != "internal-revoked" {
			continue
		}
		foundVol = true
		sec, _ := m["secret"].(map[string]any)
		if sec["secretName"] != "tinycdi-revoked-serials" {
			t.Fatalf("internal-revoked secretName = %v", sec["secretName"])
		}
		items := toSlice(sec["items"])
		if len(items) != 1 {
			t.Fatalf("internal-revoked items = %v — must project exactly one key", items)
		}
		it, _ := items[0].(map[string]any)
		if it["key"] != "revoked-serials.txt" || it["path"] != "revoked-serials.txt" {
			t.Fatalf("internal-revoked projection = %v — key must map to the fixed filename", it)
		}
	}
	if !foundVol {
		t.Fatal("internal-revoked volume missing")
	}
}

// TestRevokedSerialsCustomKey: the Secret's blocklist key is configurable
// and still projects onto the fixed in-pod filename.
func TestRevokedSerialsCustomKey(t *testing.T) {
	d := revokedRender(t, "--set", "backend.tls.internal.revokedSerials.key=blocklist.pem")
	spec, _ := d["spec"].(map[string]any)
	tpl, _ := spec["template"].(map[string]any)
	podSpec, _ := tpl["spec"].(map[string]any)
	for _, v := range toSlice(podSpec["volumes"]) {
		m, _ := v.(map[string]any)
		if m["name"] != "internal-revoked" {
			continue
		}
		sec, _ := m["secret"].(map[string]any)
		it, _ := toSlice(sec["items"])[0].(map[string]any)
		if it["key"] != "blocklist.pem" || it["path"] != "revoked-serials.txt" {
			t.Fatalf("custom key projection = %v", it)
		}
		return
	}
	t.Fatal("internal-revoked volume missing")
}
