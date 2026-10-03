// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package provisioning_test

import (
	"context"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	workspacev1alpha1 "github.com/tinyorbitvn/tinycdi/api/v1alpha1"
	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
)

// TestResolveTemplateByName covers the shared resolver all three consumers
// (API catalog, broker running source, operator admit) now use: exact
// object names win; a base name resolves to the newest catalog-name
// revision through revision churn; an unresolvable reference reports the
// original NotFound.
func TestResolveTemplateByName(t *testing.T) {
	t0 := time.Now().Add(-time.Hour)
	old := catalogTmpl("linuxdesk1-aaaa0000", "linuxdesk1", "w6a-1", t0)
	cur := catalogTmpl("linuxdesk1-bbbb1111", "linuxdesk1", "w6a-2", t0.Add(30*time.Minute))
	solo := catalogTmpl("admintpl", "", "1", t0)

	scheme := runtime.NewScheme()
	if err := workspacev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(old, cur, solo).Build()
	ctx := context.Background()

	// Exact object name resolves to itself even when a newer catalog
	// revision exists.
	tpl, err := provisioning.ResolveTemplateByName(ctx, c, "ns-a", "linuxdesk1-aaaa0000")
	if err != nil || tpl == nil || tpl.Name != "linuxdesk1-aaaa0000" {
		t.Fatalf("exact: %v/%v", tpl, err)
	}
	// Catalog base name resolves to the newest revision.
	tpl, err = provisioning.ResolveTemplateByName(ctx, c, "ns-a", "linuxdesk1")
	if err != nil || tpl == nil || tpl.Name != "linuxdesk1-bbbb1111" {
		t.Fatalf("catalog base: %v/%v, want linuxdesk1-bbbb1111", tpl, err)
	}
	// Unlabeled singleton resolves by name.
	tpl, err = provisioning.ResolveTemplateByName(ctx, c, "ns-a", "admintpl")
	if err != nil || tpl == nil || tpl.Name != "admintpl" {
		t.Fatalf("singleton: %v/%v", tpl, err)
	}
	// Unknown name: NotFound, so the operator keeps its TemplateNotFound
	// path and the catalog keeps its nil-entry convention.
	if tpl, err = provisioning.ResolveTemplateByName(ctx, c, "ns-a", "nope"); !apierrors.IsNotFound(err) {
		t.Fatalf("unknown: %v/%v, want NotFound", tpl, err)
	}
}
