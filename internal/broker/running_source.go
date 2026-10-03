package broker

import (
	"context"
	"encoding/json"
	"fmt"

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

// snapshotPolicy is the lifecycle slice of the operator's recorded
// template-snapshot annotation (operator.AnnotationTemplateSnapshot).
// Only the spec's lifecycle matters here — the admitted revision's caps.
type snapshotPolicy struct {
	Spec struct {
		Lifecycle workspacesv1alpha1.LifecycleDefaults `json:"lifecycle"`
	} `json:"spec"`
}

// policyFor resolves the lifecycle budget the expiry planner enforces on
// one workspace. The broker owns expiry decisions and plans them from the
// workspace's OWN recorded contract: the template snapshot the operator
// recorded at first admit. A template re-published after admit must not
// change a running generation's caps — that is also the input the
// operator's max-duration backstop converges on, so the two owners of the
// stop decision can never disagree. A workspace without a recorded
// snapshot (never admitted, or the annotation was lost) resolves its
// templateRef through the shared by-name resolver — a reference to a
// catalog base name resolves to the newest revision — and a missing or
// unresolvable template falls back to DefaultTimeoutPolicy, so a broken
// reference never disables the caps.
func (s *K8sRunningSource) policyFor(ctx context.Context, ws *workspacesv1alpha1.Workspace) TimeoutPolicy {
	lc := s.lifecycleFor(ctx, ws)
	pol := DefaultTimeoutPolicy
	if lc == nil {
		return pol
	}
	if d := lc.IdleTimeout.Duration; d > 0 {
		pol.IdleTimeout = d
	}
	if d := lc.DisconnectTimeout.Duration; d > 0 {
		pol.DisconnectTimeout = d
	}
	if d := lc.MaxDuration.Duration; d > 0 {
		pol.MaxDuration = d
	}
	return pol
}

// lifecycleFor returns the recorded snapshot lifecycle, else the resolved
// template's, else nil. A corrupt snapshot annotation resolves the live
// template rather than failing closed — an unplannable workspace must
// never silently drop its caps.
func (s *K8sRunningSource) lifecycleFor(ctx context.Context, ws *workspacesv1alpha1.Workspace) *workspacesv1alpha1.LifecycleDefaults {
	if raw := ws.Annotations[operator.AnnotationTemplateSnapshot]; raw != "" {
		var snap snapshotPolicy
		// An all-zero lifecycle — e.g. a snapshot written without the key —
		// carries no recorded contract; resolve the live template instead.
		if err := json.Unmarshal([]byte(raw), &snap); err == nil &&
			snap.Spec.Lifecycle != (workspacesv1alpha1.LifecycleDefaults{}) {
			lc := snap.Spec.Lifecycle
			return &lc
		}
	}
	if tpl := s.template(ctx, ws); tpl != nil {
		return &tpl.Spec.Lifecycle
	}
	return nil
}

// template finds the WorkspaceTemplate a workspace references through the
// shared by-name resolver (exact object, else newest catalog-name
// revision); nil when neither exists or the lookup errors — a broken
// reference falls back to DefaultTimeoutPolicy rather than disabling caps.
func (s *K8sRunningSource) template(ctx context.Context, ws *workspacesv1alpha1.Workspace) *workspacesv1alpha1.WorkspaceTemplate {
	tpl, err := provisioning.ResolveTemplateByName(ctx, s.cache, ws.Namespace, ws.Spec.TemplateRef.Name)
	if err != nil {
		return nil
	}
	return tpl
}
