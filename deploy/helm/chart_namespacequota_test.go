// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package chart_test

// Opt-in per-managed-namespace ResourceQuota + LimitRange (v1.0 chart
// hardening): managedNamespaces[].namespaceQuota renders the two objects
// ONLY into entries that declare the block — the default render carries
// neither kind anywhere.

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestNamespaceQuotaDefaultRendersNothing(t *testing.T) {
	for _, docs := range [][]doc{
		render(t, "minimal-values.yaml"),
		render(t, "example-values.yaml"),
	} {
		if n := len(selectDocs(docs, "ResourceQuota")); n != 0 {
			t.Errorf("default render must contain no ResourceQuota, got %d", n)
		}
		if n := len(selectDocs(docs, "LimitRange")); n != 0 {
			t.Errorf("default render must contain no LimitRange, got %d", n)
		}
	}
}

// The block renders one ResourceQuota and one LimitRange into ONLY the
// entry's namespace, carrying the declared hard limits / limits and the
// tenant label — never into sibling managed namespaces.
func TestNamespaceQuotaRendersPerEntry(t *testing.T) {
	minimal := filepath.Join("tinycdi", "ci", "minimal-values.yaml")
	docs := renderArgs(t, "-f", minimal,
		"--set-json", `managedNamespaces[0].namespaceQuota=`+
			`{"resourceQuota":{"hard":{"requests.cpu":"24","pods":"30"}},`+
			`"limitRange":{"limits":[{"type":"Container",`+
			`"defaultRequest":{"cpu":"1","memory":"2Gi"},`+
			`"default":{"cpu":"4","memory":"8Gi"}}]}}`)

	rqs := selectDocs(docs, "ResourceQuota")
	if len(rqs) != 1 {
		t.Fatalf("ResourceQuota docs = %d, want 1", len(rqs))
	}
	rq := rqs[0]
	name, ns := meta(rq)
	if name != "tinycdi-workspace-quota" || ns != "tinycdi-tenant-a" {
		t.Fatalf("ResourceQuota %s/%s, want tinycdi-tenant-a/tinycdi-workspace-quota", ns, name)
	}
	lbls, _ := rq["metadata"].(map[string]any)["labels"].(map[string]any)
	if lbls["workspaces.cdi.tinyorbit.vn/tenant"] != "tenant-a" {
		t.Errorf("ResourceQuota tenant label = %v", lbls)
	}
	hard, _ := rq["spec"].(map[string]any)["hard"].(map[string]any)
	if hard["requests.cpu"] != "24" || hard["pods"] != "30" {
		t.Errorf("ResourceQuota hard = %v", hard)
	}

	lrs := selectDocs(docs, "LimitRange")
	if len(lrs) != 1 {
		t.Fatalf("LimitRange docs = %d, want 1", len(lrs))
	}
	name, ns = meta(lrs[0])
	if name != "tinycdi-runtime-defaults" || ns != "tinycdi-tenant-a" {
		t.Fatalf("LimitRange %s/%s, want tinycdi-tenant-a/tinycdi-runtime-defaults", ns, name)
	}
	limits := toSlice(lrs[0]["spec"].(map[string]any)["limits"])
	if len(limits) != 1 {
		t.Fatalf("LimitRange limits = %v", limits)
	}
	entry, _ := limits[0].(map[string]any)
	if entry["type"] != "Container" {
		t.Errorf("LimitRange entry type = %v", entry["type"])
	}
}

// Each sub-object renders alone too — a LimitRange-only or
// ResourceQuota-only block is valid.
func TestNamespaceQuotaRendersSubObjectsIndependently(t *testing.T) {
	minimal := filepath.Join("tinycdi", "ci", "minimal-values.yaml")

	docs := renderArgs(t, "-f", minimal,
		"--set-json", `managedNamespaces[0].namespaceQuota=`+
			`{"resourceQuota":{"hard":{"pods":"10"}}}`)
	if len(selectDocs(docs, "ResourceQuota")) != 1 || len(selectDocs(docs, "LimitRange")) != 0 {
		t.Fatal("resourceQuota-only block must render only the ResourceQuota")
	}

	docs = renderArgs(t, "-f", minimal,
		"--set-json", `managedNamespaces[1].namespaceQuota=`+
			`{"limitRange":{"limits":[{"type":"Container","default":{"cpu":"2"}}]}}`)
	lrs := selectDocs(docs, "LimitRange")
	if len(lrs) != 1 {
		t.Fatalf("limitRange-only block: LimitRange docs = %d", len(lrs))
	}
	if _, ns := meta(lrs[0]); ns != "tinycdi-tenant-b" {
		t.Fatalf("LimitRange namespace = %s, want tinycdi-tenant-b", ns)
	}
	if len(selectDocs(docs, "ResourceQuota")) != 0 {
		t.Fatal("limitRange-only block must render no ResourceQuota")
	}
}

// An empty or unknown-keyed block is a schema error — never a silent
// no-op (a quota the operator believes is set must exist).
func TestNamespaceQuotaSchema(t *testing.T) {
	minimal := filepath.Join("tinycdi", "ci", "minimal-values.yaml")
	for _, tc := range []struct {
		name string
		set  string
	}{
		{"empty block", `managedNamespaces[0].namespaceQuota={}`},
		{"unknown key", `managedNamespaces[0].namespaceQuota={"bogus":{}}`},
		{"resourceQuota without hard", `managedNamespaces[0].namespaceQuota={"resourceQuota":{}}`},
		{"empty hard", `managedNamespaces[0].namespaceQuota={"resourceQuota":{"hard":{}}}`},
		{"limitRange without limits", `managedNamespaces[0].namespaceQuota={"limitRange":{}}`},
		{"empty limits", `managedNamespaces[0].namespaceQuota={"limitRange":{"limits":[]}}`},
	} {
		out := renderErrArgs(t, "-f", minimal, "--set-json", tc.set)
		if !strings.Contains(out, "namespaceQuota") && !strings.Contains(out, "managedNamespaces") {
			t.Errorf("%s (%s) must be rejected naming the block, got: %s", tc.name, tc.set, out)
		}
	}
}
