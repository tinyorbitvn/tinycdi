// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package provisioning

import (
	"context"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	workspacev1alpha1 "github.com/tinyorbitvn/tinycdi/api/v1alpha1"
)

// ResolveTemplateByName resolves a templateRef name to a WorkspaceTemplate:
// the exact object first, else the newest revision carrying the
// workspaces.cdi.tinyorbit.vn/catalog-name label equal to the reference —
// seeded templates are published as immutable "<name>-<hash8>" objects, so a
// base-name reference resolves through revision churn. The single
// implementation of that rule, shared by the API catalog, the broker's
// running-set projection and the operator's first-admit resolution.
//
// The original NotFound error is returned when neither an exact object nor
// a catalog revision exists so callers keep their own miss handling; every
// other error propagates.
func ResolveTemplateByName(ctx context.Context, c client.Reader, namespace, name string) (*workspacev1alpha1.WorkspaceTemplate, error) {
	tpl := &workspacev1alpha1.WorkspaceTemplate{}
	err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, tpl)
	if !apierrors.IsNotFound(err) {
		return tpl, err
	}
	var list workspacev1alpha1.WorkspaceTemplateList
	if lerr := c.List(ctx, &list, client.InNamespace(namespace),
		client.MatchingLabels{LabelCatalogName: name}); lerr != nil {
		return nil, lerr
	}
	latest := latestRevision(list.Items)
	if latest == nil {
		return nil, err
	}
	return latest, nil
}
