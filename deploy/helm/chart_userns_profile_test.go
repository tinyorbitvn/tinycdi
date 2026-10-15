// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package chart_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const usernsSeccompAnnotation = "localhost/profiles/chromium-userns.json"

// usernsTemplateValues writes a values overlay whose only seeded template
// uses the userns browser seccomp profile with the given spec.linux block.
func usernsTemplateValues(t *testing.T, linux string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "userns-values.yaml")
	body := fmt.Sprintf(`templates:
  - name: userns
    namespace: tinycdi-tenant-a
    image: browser
    seccompProfile: %s
    spec:
      revision: "2026-10-a"
      runtime: LinuxContainer
      experience: Browser
      linux: %s
      resources: {cpu: "1", memory: 2Gi, storage: 5Gi}
      bootDeadline: 5m
      networkProfile: Isolated
`, usernsSeccompAnnotation, linux)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// The userns browser seccomp profile lets the workspace user create user
// and network namespaces, so a seeded template that selects it renders
// only when its pods run with hostUsers: false — from spec.linux.hostUsers
// or, when the template leaves it unset, from runtime.hostUsers.
func TestSeededUsernsProfileRequiresHostUsersFalse(t *testing.T) {
	example := filepath.Join("tinycdi", "ci", "example-values.yaml")

	// The shipped example seeds two userns templates on the chart default
	// runtime.hostUsers=false: it renders.
	n := 0
	for _, d := range selectDocs(renderArgs(t, "-f", example), "WorkspaceTemplate") {
		ann, _ := d["metadata"].(map[string]any)["annotations"].(map[string]any)
		if ann["workspaces.cdi.tinyorbit.vn/seccomp-profile"] == usernsSeccompAnnotation {
			n++
		}
	}
	if n < 2 {
		t.Fatalf("example-values renders %d userns templates, want the browser and kasm seeds", n)
	}

	for name, extra := range map[string][]string{
		"operator default unset": {"-f", example, "--set-json", "runtime.hostUsers=null"},
		"operator default true":  {"-f", example, "--set", "runtime.hostUsers=true"},
		"template true":          {"-f", example, "-f", usernsTemplateValues(t, "{hostUsers: true}")},
		"template null, operator default unset": {"-f", example,
			"-f", usernsTemplateValues(t, "{hostUsers: null}"), "--set-json", "runtime.hostUsers=null"},
	} {
		t.Run(name, func(t *testing.T) {
			out := renderErrArgs(t, extra...)
			if !strings.Contains(out, "requires hostUsers: false") {
				t.Errorf("render failed without naming the hostUsers requirement:\n%s", out)
			}
		})
	}

	// A template's own hostUsers: false wins over an unset operator default.
	renderArgs(t, "-f", example, "-f", usernsTemplateValues(t, "{hostUsers: false}"),
		"--set-json", "runtime.hostUsers=null")
}
