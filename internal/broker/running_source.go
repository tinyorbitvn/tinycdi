package broker

import (
	"context"
	"fmt"

	crcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	workspacesv1alpha1 "github.com/tinyorbitvn/tinycdi/api/v1alpha1"
	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
)

// K8sRunningSource is the production RunningSource for the ExpiryPlanner:
// it lists Workspace CRs through the same informer cache as
// K8sBindingSource and projects each workspace whose current runtime
// generation is Ready (desired Running, status.startedAt set) into a
// RunningWorkspace. The TimeoutPolicy comes from the workspace's
// WorkspaceTemplate lifecycle defaults in the same namespace; a missing
// template or a zero field falls back to DefaultTimeoutPolicy so a broken
// reference never disables the caps.
type K8sRunningSource struct {
	cache crcache.Cache
}

// NewK8sRunningSource builds the running-set projection on the given cache.
func NewK8sRunningSource(c crcache.Cache) *K8sRunningSource {
	return &K8sRunningSource{cache: c}
}

// RunningWorkspaces implements RunningSource.
func (s *K8sRunningSource) RunningWorkspaces(ctx context.Context) ([]RunningWorkspace, error) {
	var list workspacesv1alpha1.WorkspaceList
	if err := s.cache.List(ctx, &list,
		client.HasLabels{provisioning.LabelWorkspaceUID}); err != nil {
		return nil, fmt.Errorf("broker: list running workspaces: %w", err)
	}
	var out []RunningWorkspace
	for i := range list.Items {
		ws := &list.Items[i]
		if ws.Spec.DesiredState != workspacesv1alpha1.DesiredStateRunning ||
			ws.Status.Phase != workspacesv1alpha1.WorkspacePhaseReady ||
			ws.Status.StartedAt == nil {
			continue
		}
		out = append(out, RunningWorkspace{
			WorkspaceUID:      provisioning.PlatformID(ws.Labels[provisioning.LabelWorkspaceUID]),
			RuntimeGeneration: uint64(ws.Status.ObservedRuntimeGeneration),
			StartedAt:         ws.Status.StartedAt.Time,
			Policy:            s.policyFor(ctx, ws),
		})
	}
	return out, nil
}

// policyFor resolves the template lifecycle snapshot for one workspace.
func (s *K8sRunningSource) policyFor(ctx context.Context, ws *workspacesv1alpha1.Workspace) TimeoutPolicy {
	pol := DefaultTimeoutPolicy
	var tpl workspacesv1alpha1.WorkspaceTemplate
	if err := s.cache.Get(ctx, client.ObjectKey{
		Namespace: ws.Namespace,
		Name:      ws.Spec.TemplateRef.Name,
	}, &tpl); err != nil {
		return pol
	}
	if d := tpl.Spec.Lifecycle.IdleTimeout.Duration; d > 0 {
		pol.IdleTimeout = d
	}
	if d := tpl.Spec.Lifecycle.DisconnectTimeout.Duration; d > 0 {
		pol.DisconnectTimeout = d
	}
	if d := tpl.Spec.Lifecycle.MaxDuration.Duration; d > 0 {
		pol.MaxDuration = d
	}
	return pol
}
