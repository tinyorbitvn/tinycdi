// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package chart_test

import (
	"testing"
)

// TestFrontendBrandingValues (T3.9): setting frontend.branding.configMap
// mounts the named ConfigMap read-only at /branding and passes
// -branding-dir=/branding to the frontend; unset (the default) renders
// neither the flag, nor the mount, nor the volume.
func TestFrontendBrandingValues(t *testing.T) {
	t.Run("set", func(t *testing.T) {
		dep := deployment(
			renderArgs(t,
				"-f", "tinycdi/ci/minimal-values.yaml",
				"--set", "frontend.branding.configMap=acme-branding"),
			"frontend")
		if dep == nil {
			t.Fatal("no frontend Deployment rendered")
		}
		if !hasString(firstContainerArgs(dep), "-branding-dir=/branding") {
			t.Errorf("frontend args missing -branding-dir=/branding: %v", firstContainerArgs(dep))
		}
		mount := namedItem(firstContainer(dep)["volumeMounts"], "branding")
		if mount == nil {
			t.Fatal("no volumeMount named branding")
		}
		if mp, _ := mount["mountPath"].(string); mp != "/branding" {
			t.Errorf("branding volumeMount mountPath = %q, want /branding", mp)
		}
		if ro, _ := mount["readOnly"].(bool); !ro {
			t.Errorf("branding volumeMount readOnly = %v, want true", mount["readOnly"])
		}
		vol := podVolume(dep, "branding")
		if vol == nil {
			t.Fatal("no volume named branding")
		}
		cm, _ := vol["configMap"].(map[string]any)
		if name, _ := cm["name"].(string); name != "acme-branding" {
			t.Errorf("branding volume configMap.name = %q, want acme-branding", name)
		}
	})

	t.Run("unset", func(t *testing.T) {
		dep := deployment(render(t, "minimal-values.yaml"), "frontend")
		if dep == nil {
			t.Fatal("no frontend Deployment rendered")
		}
		for _, a := range firstContainerArgs(dep) {
			if a == "-branding-dir=/branding" || a == "--branding-dir" {
				t.Errorf("frontend args carry -branding-dir without the value: %v", firstContainerArgs(dep))
			}
		}
		if m := namedItem(firstContainer(dep)["volumeMounts"], "branding"); m != nil {
			t.Errorf("branding volumeMount rendered without the value: %v", m)
		}
		if v := podVolume(dep, "branding"); v != nil {
			t.Errorf("branding volume rendered without the value: %v", v)
		}
	})
}

func hasString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// namedItem returns the entry of a rendered YAML list whose "name" field is
// want (volumeMounts, env, containers, ...), or nil.
func namedItem(v any, want string) map[string]any {
	for _, e := range toSlice(v) {
		m, _ := e.(map[string]any)
		if n, _ := m["name"].(string); n == want {
			return m
		}
	}
	return nil
}

// podVolume returns the named pod-level volume of a Deployment doc, or nil.
func podVolume(d doc, name string) map[string]any {
	spec, _ := d["spec"].(map[string]any)
	tpl, _ := spec["template"].(map[string]any)
	podSpec, _ := tpl["spec"].(map[string]any)
	return namedItem(podSpec["volumes"], name)
}
