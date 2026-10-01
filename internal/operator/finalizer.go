package operator

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	workspacesv1alpha1 "github.com/tinyorbitvn/tinycdi/api/v1alpha1"
	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
	tcdiruntime "github.com/tinyorbitvn/tinycdi/internal/runtime"
)

// AnnotationFinalizerProgress persists the teardown position on the
// Workspace (JSON FinalizerProgress) so a crash or operator restart resumes
// at the first incomplete step — completed steps are never re-executed.
const AnnotationFinalizerProgress = "workspaces.cdi.tinyorbit.vn/finalizer-progress"

// MaxStreamDrain is the design §5 budget for the gateway to close open
// streams after lease revocation. When it expires teardown proceeds —
// gateways are fail-closed and dead streams die with the runtime anyway.
const MaxStreamDrain = 45 * time.Second

// FinalizerStep is one stage of the mandated teardown order
// (design §5: block new connects -> revoke leases -> wait <=45 s for the
// gateway to close streams -> stop runtime -> retention -> cleanup
// service/secret -> done).
type FinalizerStep string

const (
	// StepBlockConnects denies new launch tickets/leases for the workspace.
	StepBlockConnects FinalizerStep = "block-connects"
	// StepRevokeLeases revokes every active connection lease.
	StepRevokeLeases FinalizerStep = "revoke-leases"
	// StepDrainStreams waits, at most MaxStreamDrain, for open streams to close.
	StepDrainStreams FinalizerStep = "drain-streams"
	// StepStopRuntime terminates the incarnation (data volumes untouched).
	StepStopRuntime FinalizerStep = "stop-runtime"
	// StepRetention applies dataPolicy: Retain moves persistent volumes to
	// the retained inventory; Ephemeral schedules their destruction.
	StepRetention FinalizerStep = "retention"
	// StepCleanup removes ephemeral children (service, secrets, scratch).
	StepCleanup FinalizerStep = "cleanup"
)

// FinalizerOrder is the mandated step sequence; Run executes steps in this
// order and never reorders them.
var FinalizerOrder = []FinalizerStep{
	StepBlockConnects,
	StepRevokeLeases,
	StepDrainStreams,
	StepStopRuntime,
	StepRetention,
	StepCleanup,
}

// FinalizerProgress is the persisted checkpoint. Completed lists finished
// steps in order; DrainStartedAt anchors the 45 s stream-drain budget so it
// survives restarts (a restart does NOT restart the drain window).
type FinalizerProgress struct {
	Completed      []FinalizerStep `json:"completed"`
	DrainStartedAt *time.Time      `json:"drainStartedAt,omitempty"`
}

func (p *FinalizerProgress) doneSet() map[FinalizerStep]bool {
	m := make(map[FinalizerStep]bool, len(p.Completed))
	for _, s := range p.Completed {
		m[s] = true
	}
	return m
}

// ConnectBlocker stops issuance of new tickets/leases for a workspace.
//
// Expected production call (wired once the broker internal API lands,
// broker contract): broker BlockConnects(ctx, workspaceUID) — deny new launch
// tickets and lease redemptions for the workspace. Until then the
// controller wires standaloneBroker.
type ConnectBlocker interface {
	BlockConnects(ctx context.Context, workspaceUID provisioning.PlatformID) error
}

// LeaseRevoker revokes every live connection lease of a workspace.
//
// Contract implemented by the operator-side broker HTTP client :
// RevokeAllForWorkspace revokes all active leases pinned to
// runtimeGeneration of the workspace in one call and returns how many were
// revoked; an error means revocation is unconfirmed and the step retries.
type LeaseRevoker interface {
	RevokeAllForWorkspace(ctx context.Context, workspaceUID provisioning.PlatformID, runtimeGeneration int64) (int, error)
}

// StreamDrainer reports the gateway-reported count of open interactive
// streams bound to the workspace (broker DrainStatus): drained is true
// only when zero streams remain.
type StreamDrainer interface {
	DrainStatus(ctx context.Context, workspaceUID provisioning.PlatformID) (openStreams int, drained bool, err error)
}

// RetentionHandler applies the workspace's dataPolicy to its persistent
// volumes. It must be idempotent: partial progress (e.g. one of two PVCs
// marked retained) is retried on the next run.
type RetentionHandler interface {
	ApplyRetention(ctx context.Context, ws *workspacesv1alpha1.Workspace) error
}

// stepHook is the package-internal seam used to observe and fault-inject
// the backend-owned steps (stop-runtime, cleanup), which have no external
// dependency and therefore no wired interface. The method is unexported so
// only fakes inside this package can implement it — production wiring is
// unaffected.
type stepHook interface {
	hit(step FinalizerStep) error
}

// Finalizer executes the ordered teardown of a deleting Workspace, one
// persisted step at a time. Every step must be safe to retry; progress is
// written to AnnotationFinalizerProgress between steps.
type Finalizer struct {
	Client  client.Client
	Backend tcdiruntime.Backend

	// Connects, Leases, Drainer and Retention are the external seams. A nil
	// seam means the dependency is unavailable — the run must fail its step
	// rather than silently skip it: the finalizer is never dropped while
	// retention is unhandled or Broker/K8s/CSI is unreachable.
	Connects  ConnectBlocker
	Leases    LeaseRevoker
	Drainer   StreamDrainer
	Retention RetentionHandler

	// Now is injectable for fake-clock tests; nil uses time.Now.
	Now func() time.Time
	// DrainBudget overrides MaxStreamDrain; zero uses the default.
	DrainBudget time.Duration

	// AllowMissingWorkspaceID is a TEST-ONLY seam: when set, a CR without
	// the platform workspace-id label falls back to its k8s UID for the
	// broker seams. Production wiring must never set it — a label-less CR
	// in production is operator-data-plane corruption (Degraded,
	// ReasonMissingWorkspaceID), and the CR UID must never reach the
	// broker because it matches no workspace/lease row.
	AllowMissingWorkspaceID bool
}

func (f *Finalizer) now() time.Time {
	if f.Now != nil {
		return f.Now()
	}
	return time.Now()
}

func (f *Finalizer) drainBudget() time.Duration {
	if f.DrainBudget > 0 {
		return f.DrainBudget
	}
	return MaxStreamDrain
}

// hook returns the first configured seam that also implements the internal
// stepHook probe; nil in production.
func (f *Finalizer) hook() stepHook {
	for _, seam := range []any{f.Connects, f.Leases, f.Drainer, f.Retention} {
		if h, ok := seam.(stepHook); ok {
			return h
		}
	}
	return nil
}

// Run advances the teardown until it completes or a step blocks/fails.
// done=true only after StepCleanup finished — the caller may then remove
// the finalizer. A step failure returns done=false and a non-nil error;
// the step's reason lands on the Degraded condition via the status
// helpers, and the next Run resumes at that step. Run on a workspace whose
// finalizer-progress annotation shows earlier steps completed does not
// re-execute them.
func (f *Finalizer) Run(ctx context.Context, ws *workspacesv1alpha1.Workspace) (done bool, err error) {
	now := f.now()
	prog, err := finalizerProgress(ws)
	if err != nil {
		return false, err
	}
	completed := prog.doneSet()
	// The broker seams (Connects/Leases/Drainer) key the PLATFORM workspace
	// id — the workspaces.cdi.tinyorbit.vn/workspace-uid label the API applier
	// stamps on the CR ("ws_…"), which is what connection_lease and
	// workspace_revocation rows reference. The CR's k8s UID (used by the
	// runtime backend for child objects) is a different identifier: a
	// label-less CR is data-plane corruption, so degrade and hold the
	// finalizer rather than revoke "nothing" under the wrong id.
	uid := provisioning.PlatformID(ws.Labels[provisioning.LabelWorkspaceUID])
	if uid == "" {
		if f.AllowMissingWorkspaceID {
			uid = provisioning.PlatformID(ws.UID)
		} else {
			SetWorkspaceCondition(ws, workspacesv1alpha1.ConditionDegraded,
				metav1.ConditionTrue, ReasonMissingWorkspaceID,
				"CR lacks the platform workspace id label; teardown held until it is restored", now)
			if uerr := f.Client.Status().Update(ctx, ws); uerr != nil {
				return false, fmt.Errorf("mark missing workspace id: %w", uerr)
			}
			return false, nil
		}
	}

	for _, step := range FinalizerOrder {
		if completed[step] {
			continue
		}
		var serr error
		switch step {
		case StepBlockConnects:
			serr = f.blockConnects(ctx, uid)
		case StepRevokeLeases:
			serr = f.revokeLeases(ctx, ws, uid)
		case StepDrainStreams:
			drained, derr := f.runDrain(ctx, ws, prog, uid, now)
			if derr != nil {
				serr = derr
			} else if !drained {
				// Inside the drain window: surface the wait and requeue;
				// not an error, but teardown is not done.
				return false, nil
			}
		case StepStopRuntime:
			if serr = f.probe(step); serr == nil {
				serr = f.Backend.Stop(ctx, ws)
			}
		case StepRetention:
			serr = f.applyRetention(ctx, ws)
		case StepCleanup:
			if serr = f.probe(step); serr == nil {
				serr = f.Backend.DeleteRuntime(ctx, ws)
			}
		}
		if serr != nil {
			MarkFinalizerStepBlocked(ws, step, serr, now)
			if uerr := f.Client.Status().Update(ctx, ws); uerr != nil {
				return false, fmt.Errorf("record blocked step %s: %w", step, uerr)
			}
			return false, serr
		}
		prog.Completed = append(prog.Completed, step)
		completed[step] = true
		if err := f.persistProgress(ctx, ws, prog); err != nil {
			return false, err
		}
	}
	return true, nil
}

// probe routes backend-owned steps through the internal test hook when one
// is wired; production finalizers always return nil here.
func (f *Finalizer) probe(step FinalizerStep) error {
	if h := f.hook(); h != nil {
		return h.hit(step)
	}
	return nil
}

func (f *Finalizer) blockConnects(ctx context.Context, uid provisioning.PlatformID) error {
	if f.Connects == nil {
		return fmt.Errorf("finalizer: connect blocker unavailable (broker seam)")
	}
	return f.Connects.BlockConnects(ctx, uid)
}

func (f *Finalizer) revokeLeases(ctx context.Context, ws *workspacesv1alpha1.Workspace, uid provisioning.PlatformID) error {
	if f.Leases == nil {
		return fmt.Errorf("finalizer: lease revoker unavailable (broker seam)")
	}
	// Leases bind the incarnation the operator converged; the applied
	// record is authoritative, spec is the fallback.
	gen := ws.Spec.RuntimeGeneration
	if applied, err := appliedIntent(ws); err == nil && applied.RuntimeGeneration > 0 {
		gen = applied.RuntimeGeneration
	}
	_, err := f.Leases.RevokeAllForWorkspace(ctx, uid, gen)
	return err
}

func (f *Finalizer) applyRetention(ctx context.Context, ws *workspacesv1alpha1.Workspace) error {
	if f.Retention == nil {
		return fmt.Errorf("finalizer: retention handler unavailable (CSI seam)")
	}
	return f.Retention.ApplyRetention(ctx, ws)
}

// runDrain waits out the bounded stream-drain window. It returns
// (true, nil) when the step is complete — either the gateway reported all
// streams closed or the persisted drain deadline expired — and (false, nil)
// while still inside the window. The drain anchor is persisted before the
// first gateway call so a restart never extends the budget.
func (f *Finalizer) runDrain(ctx context.Context, ws *workspacesv1alpha1.Workspace, prog *FinalizerProgress, uid provisioning.PlatformID, now time.Time) (bool, error) {
	if f.Drainer == nil {
		return false, fmt.Errorf("finalizer: stream drainer unavailable (broker seam)")
	}
	if prog.DrainStartedAt == nil {
		started := now
		prog.DrainStartedAt = &started
		if err := f.persistProgress(ctx, ws, prog); err != nil {
			return false, err
		}
	}
	_, drained, err := f.Drainer.DrainStatus(ctx, uid)
	if err != nil {
		return false, err
	}
	if drained {
		return true, nil
	}
	if !now.Before(prog.DrainStartedAt.Add(f.drainBudget())) {
		// Budget exhausted: proceed without the gateway; fail-closed
		// streams die with the runtime anyway.
		SetWorkspaceCondition(ws, workspacesv1alpha1.ConditionDegraded,
			metav1.ConditionTrue, ReasonDrainTimedOut,
			"gateway streams did not close inside the drain budget; continuing teardown", now)
		if err := f.Client.Status().Update(ctx, ws); err != nil {
			return false, err
		}
		return true, nil
	}
	SetWorkspaceCondition(ws, workspacesv1alpha1.ConditionDegraded,
		metav1.ConditionTrue, ReasonStreamDraining,
		"waiting for gateway streams to close", now)
	if err := f.Client.Status().Update(ctx, ws); err != nil {
		return false, err
	}
	return false, nil
}

// persistProgress writes the checkpoint annotation and updates the object.
// Steps are idempotent, so a failed write simply re-runs the last step.
func (f *Finalizer) persistProgress(ctx context.Context, ws *workspacesv1alpha1.Workspace, prog *FinalizerProgress) error {
	raw, err := json.Marshal(prog)
	if err != nil {
		return err
	}
	setAnnotation(ws, AnnotationFinalizerProgress, string(raw))
	return f.Client.Update(ctx, ws)
}

func finalizerProgress(ws *workspacesv1alpha1.Workspace) (*FinalizerProgress, error) {
	raw := ws.Annotations[AnnotationFinalizerProgress]
	if raw == "" {
		return &FinalizerProgress{}, nil
	}
	p := &FinalizerProgress{}
	if err := json.Unmarshal([]byte(raw), p); err != nil {
		return nil, fmt.Errorf("corrupt %s annotation: %w", AnnotationFinalizerProgress, err)
	}
	return p, nil
}

// --- controller default seams --------------------------------------------
//
// Until the broker's internal API is wired , the operator runs
// standalone: there are no brokered launch tickets or connection leases to
// revoke and no gateway streams to drain, so these defaults admit
// teardown. They are explicit wiring choices made by the reconciler — the
// Finalizer itself still refuses a nil seam.

// standaloneBroker is the interim ConnectBlocker/LeaseRevoker/
// StreamDrainer for the unwired control plane. Replace with the
// broker-backed HTTP client once its internal API is served.
type standaloneBroker struct{}

func (standaloneBroker) BlockConnects(context.Context, provisioning.PlatformID) error { return nil }
func (standaloneBroker) RevokeAllForWorkspace(context.Context, provisioning.PlatformID, int64) (int, error) {
	return 0, nil
}
func (standaloneBroker) DrainStatus(context.Context, provisioning.PlatformID) (int, bool, error) {
	return 0, true, nil
}

// pvcRetention wires the production default RetentionHandler to the
// controller-owned retained-disk inventory implemented in retention.go:
// Retain stamps the full metadata contract (label + tenant/owner/
// source-workspace/runtime/retained-at annotations, reconstructable after
// API DB loss); Ephemeral destroys the workspace's data volumes. The
// concrete behaviour and its idempotency contract live on
// RetentionApplier — this type only binds it to the reconciler's seam.
type pvcRetention struct {
	client client.Client
}

func (h pvcRetention) ApplyRetention(ctx context.Context, ws *workspacesv1alpha1.Workspace) error {
	return (&RetentionApplier{Client: h.client, Inventory: NewRetentionInventory(h.client)}).ApplyRetention(ctx, ws)
}
