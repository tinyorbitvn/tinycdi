// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package operator

import (
	"context"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	workspacesv1alpha1 "github.com/tinyorbitvn/tinycdi/api/v1alpha1"
)

// staleReadClient serves Workspace reads from a frozen copy, like an
// informer cache that has not yet seen the object disappear; writes go to
// the real apiserver.
type staleReadClient struct {
	client.Client
	stale *workspacesv1alpha1.Workspace
}

func (c *staleReadClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if ws, ok := obj.(*workspacesv1alpha1.Workspace); ok && key.Name == c.stale.Name && key.Namespace == c.stale.Namespace {
		c.stale.DeepCopyInto(ws)
		return nil
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

// TestReconcile_DeletedWorkspaceVanishesUnderReconcile (FX-R19): the CR can
// disappear between a reconcile's read and its write (the previous reconcile
// removed the finalizer; the cache still serves the Terminating object). The
// object being gone is the goal of a delete, so the reconcile must succeed
// instead of reporting "workspaces ... not found" as a reconciler error.
func TestReconcile_DeletedWorkspaceVanishesUnderReconcile(t *testing.T) {
	env, c := startEnv(t)
	defer func() { _ = env.Stop() }()

	ns := newNamespace(t, c)
	newTemplate(t, c, ns, "tpl-vanish", nil)
	ws, key := deletingWorkspace(t, c, ns, "ws-vanish", "tpl-vanish", nil)
	stale := ws.DeepCopy()

	rec := newStepRecorder()
	r := newReconciler(c)
	r.Backend = &fakeBackend{}
	r.Connects, r.Leases, r.Drainer, r.Retention = rec, rec, rec, rec

	// First reconcile completes teardown and removes the finalizer.
	reconcile(t, r, key)
	if err := c.Get(context.Background(), key, &workspacesv1alpha1.Workspace{}); !apierrors.IsNotFound(err) {
		t.Fatalf("workspace still present after teardown: %v", err)
	}

	// Second reconcile reads the stale Terminating copy; every write now
	// hits a vanished object.
	r.Client = &staleReadClient{Client: c, stale: stale}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile of an already-gone workspace = %v, want nil", err)
	}
}
