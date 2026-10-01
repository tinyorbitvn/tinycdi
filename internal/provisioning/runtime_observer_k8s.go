package provisioning

// Production RuntimeObserver (design §8): proof of runtime absence is read
// from the cluster itself. A workspace's runtime is gone when no compute is
// left — i.e. the Workspace CR is gone (or carries no live children) AND no
// Pods or in-use PVCs carrying the workspace's labels remain in its
// namespace. Ambiguity (API errors, unknown tenants, duplicate CRs) always
// answers "not gone" — Recovery never releases quota on a maybe.
//
// Label note: runtime children are stamped by the Linux backend with
// workspaces.cdi.tinyorbit.vn/workspace-uid = the CR's Kubernetes UID and
// workspaces.cdi.tinyorbit.vn/workspace-name = the CR name (deterministic from
// the platform id). The CR itself carries the platform id under the same
// workspace-uid label key — the two value domains are intentionally kept
// distinct here (platform id in, CR UID out).

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	workspacesv1alpha1 "github.com/tinyorbitvn/tinycdi/api/v1alpha1"
	linux "github.com/tinyorbitvn/tinycdi/internal/runtime/linux"
)

// K8sRuntimeObserver implements RuntimeObserver against the live cluster
// using the platform's client credentials (a controller-runtime client).
type K8sRuntimeObserver struct {
	client  client.Client
	tenants TenantNamespaces
}

// NewK8sRuntimeObserver binds the observer to a Kubernetes client and the
// managed tenant namespaces.
func NewK8sRuntimeObserver(c client.Client, tenants TenantNamespaces) *K8sRuntimeObserver {
	return &K8sRuntimeObserver{client: c, tenants: tenants}
}

// RuntimeGone reports whether the workspace's runtime is proven absent.
// workspaceUID is the platform id (workspaces.id / the workspace-uid label
// on the CR); runtime children are matched by the CR's Kubernetes UID or,
// when the CR is already gone, by the deterministic workspace-name label.
// Any error or ambiguity returns (false, err): callers must treat both as
// "not proven gone".
func (o *K8sRuntimeObserver) RuntimeGone(ctx context.Context, workspaceUID PlatformID) (bool, error) {
	crName := WorkspaceCRName(workspaceUID)

	// Locate the CR inside the managed namespaces — its name is
	// deterministic, and more than one hit means label/name corruption we
	// must not reason about.
	var cr *workspacesv1alpha1.Workspace
	var found int
	for _, ns := range o.tenants {
		var w workspacesv1alpha1.Workspace
		err := o.client.Get(ctx, client.ObjectKey{Namespace: ns, Name: crName}, &w)
		switch {
		case apierrors.IsNotFound(err):
		case err != nil:
			return false, fmt.Errorf("observer: get workspace %s/%s: %w", ns, crName, err)
		default:
			found++
			cr = &w
		}
	}
	if found > 1 {
		return false, fmt.Errorf("observer: %d Workspace CRs named %s", found, crName)
	}

	if cr != nil {
		// The CR is authoritative for which children belong to this
		// workspace: match on the CR UID under the workspace-uid label.
		gone, err := o.noChildren(ctx, cr.Namespace,
			client.MatchingLabels{linux.LabelWorkspaceUID: string(cr.UID)})
		if err != nil {
			return false, err
		}
		return gone, nil
	}

	// CR already gone (finalizer completed): GC may still be reaping
	// children. The workspace-name label is deterministic, so strays are
	// still detectable — scan every managed namespace.
	for _, ns := range o.tenants {
		gone, err := o.noChildren(ctx, ns,
			client.MatchingLabels{linux.LabelWorkspaceName: crName})
		if err != nil || !gone {
			return gone, err
		}
	}
	return true, nil
}

// noChildren reports whether no Pods and no in-use (non-retained) PVCs
// carry the given labels in ns. Retained volumes are data inventory, not
// compute — they never block quota release.
func (o *K8sRuntimeObserver) noChildren(ctx context.Context, ns string, sel client.MatchingLabels) (bool, error) {
	var pods corev1.PodList
	if err := o.client.List(ctx, &pods, client.InNamespace(ns), sel); err != nil {
		return false, fmt.Errorf("observer: list pods in %s: %w", ns, err)
	}
	if len(pods.Items) > 0 {
		return false, nil
	}
	var pvcs corev1.PersistentVolumeClaimList
	if err := o.client.List(ctx, &pvcs, client.InNamespace(ns), sel); err != nil {
		return false, fmt.Errorf("observer: list pvcs in %s: %w", ns, err)
	}
	for i := range pvcs.Items {
		if pvcs.Items[i].Labels[linux.LabelDataRetained] == "true" {
			continue
		}
		return false, nil
	}
	return true, nil
}

// compile-time assertion.
var _ RuntimeObserver = (*K8sRuntimeObserver)(nil)
