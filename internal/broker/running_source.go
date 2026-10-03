package broker

import (
	"context"
	"encoding/json"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	crcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	workspacesv1alpha1 "github.com/tinyorbitvn/tinycdi/api/v1alpha1"
	"github.com/tinyorbitvn/tinycdi/internal/operator"
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

// OperatorStoppedWorkspaces implements OperatorStoppedSource: workspaces whose
// CR still holds the Running intent the row pinned but whose applied intent
// the operator flipped to Stopped (its max-duration backstop).
func (s *K8sRunningSource) OperatorStoppedWorkspaces(ctx context.Context) ([]OperatorStopped, error) {
	var list workspacesv1alpha1.WorkspaceList
	if err := s.cache.List(ctx, &list,
		client.HasLabels{provisioning.LabelWorkspaceUID}); err != nil {
		return nil, fmt.Errorf("broker: list operator-stopped workspaces: %w", err)
	}
	var out []OperatorStopped
	for i := range list.Items {
		ws := &list.Items[i]
		if ws.Spec.DesiredState != workspacesv1alpha1.DesiredStateRunning ||
			ws.Status.Phase != workspacesv1alpha1.WorkspacePhaseStopped {
			continue
		}
		raw := ws.Annotations[operator.AnnotationAppliedIntent]
		if raw == "" {
			continue
		}
		var applied operator.AppliedIntent
		if err := json.Unmarshal([]byte(raw), &applied); err != nil {
			continue
		}
		// The exact signature of the operator's backstop: the applied record
		// is the spec's own intent (same revision and runtime generation)
		// flipped to Stopped. Anything looser is somebody else's stop.
		if applied.DesiredState != workspacesv1alpha1.DesiredStateStopped ||
			applied.Revision != ws.Spec.IntentRevision ||
			applied.RuntimeGeneration != ws.Spec.RuntimeGeneration ||
			ws.Status.LastAppliedIntentRevision != applied.Revision {
			continue
		}
		out = append(out, OperatorStopped{
			WorkspaceUID:      provisioning.PlatformID(ws.Labels[provisioning.LabelWorkspaceUID]),
			RuntimeGeneration: uint64(ws.Spec.RuntimeGeneration),
			IntentRevision:    uint64(ws.Spec.IntentRevision),
		})
	}
	return out, nil
}

// policyFor resolves the template lifecycle snapshot for one workspace. A
// reference to a catalog base name resolves, like the operator's
// resolveTemplate, to the newest revision carrying the catalog-name label:
// the seeded revision objects are named "<name>-<hash8>", so a plain Get by
// the reference alone misses them and silently falls back to the platform
// defaults (a 5 m disconnectTimeout would run as 10 m).
func (s *K8sRunningSource) policyFor(ctx context.Context, ws *workspacesv1alpha1.Workspace) TimeoutPolicy {
	pol := DefaultTimeoutPolicy
	tpl := s.template(ctx, ws)
	if tpl == nil {
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

// template finds the WorkspaceTemplate a workspace references: the exact
// object first, else the newest revision of that catalog name; nil when
// neither exists.
func (s *K8sRunningSource) template(ctx context.Context, ws *workspacesv1alpha1.Workspace) *workspacesv1alpha1.WorkspaceTemplate {
	var tpl workspacesv1alpha1.WorkspaceTemplate
	err := s.cache.Get(ctx, client.ObjectKey{Namespace: ws.Namespace, Name: ws.Spec.TemplateRef.Name}, &tpl)
	if err == nil {
		return &tpl
	}
	if !apierrors.IsNotFound(err) {
		return nil
	}
	var list workspacesv1alpha1.WorkspaceTemplateList
	if err := s.cache.List(ctx, &list, client.InNamespace(ws.Namespace),
		client.MatchingLabels{provisioning.LabelCatalogName: ws.Spec.TemplateRef.Name}); err != nil {
		return nil
	}
	var latest *workspacesv1alpha1.WorkspaceTemplate
	for i := range list.Items {
		t := &list.Items[i]
		if latest == nil ||
			t.CreationTimestamp.After(latest.CreationTimestamp.Time) ||
			(t.CreationTimestamp.Equal(&latest.CreationTimestamp) && t.Name > latest.Name) {
			latest = t
		}
	}
	return latest
}
