package operator

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	workspacesv1alpha1 "github.com/tinyorbitvn/tinycdi/api/v1alpha1"
	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
)

// ErrNotImplemented is returned by the compile-only retention stubs still
// in internal/operator/retention.go. Lifecycle and finalizer
// code no longer use it.
var ErrNotImplemented = errors.New("operator: not implemented")

// ErrStaleGeneration refuses a generation-pinned request that names an
// older runtime generation than the workspace's current one. A delayed
// expiry intent or replayed stop must never stop a newer runtime
// (design §8: the controller re-checks generation before Stop).
var ErrStaleGeneration = errors.New("operator: stale runtime generation")

// FailedCleanupDelay bounds how long a Failed incarnation may linger before
// the operator tears it down. The deadline is recomputed from persisted
// status (the Degraded condition's lastTransitionTime) so an operator
// restart never loses it — nothing is kept in memory.
const FailedCleanupDelay = 5 * time.Minute

// requeueASAP is the near-immediate requeue NextLifecycleRequeue returns
// for a deadline that already passed: the work is due, not absent.
const requeueASAP = time.Millisecond

// StopRequest is a generation-pinned stop request arriving outside the
// ordered intent stream — expiry planner output (idle / disconnect /
// max-duration) or admin action. Ordered outbox intents do not go through
// this path.
type StopRequest struct {
	// WorkspaceUID is the platform workspace id (matches the
	// workspaces.cdi.tinyorbit.vn/workspace-uid label the applier stamps) —
	// never the CR's metadata.uid.
	WorkspaceUID provisioning.PlatformID
	// RuntimeGeneration pins the request to one generation of runtime.
	RuntimeGeneration int64
	// Reason is the machine-readable stop reason surfaced into conditions
	// (e.g. "idle_timeout", "disconnect_timeout", "max_duration").
	Reason string
}

// RequestStop is the operator-side generation re-check for out-of-band
// stops. It returns (true, nil) once the stop has been driven into the
// workspace's applied-intent path — subsequent reconciles converge the
// runtime down. When the workspace's current spec.runtimeGeneration is
// newer than the request's, it returns ErrStaleGeneration with no side
// effects: a delayed expiry intent can never kill a fresh runtime. A
// request that already matches an applied stop is idempotent.
func (r *WorkspaceReconciler) RequestStop(ctx context.Context, ws *workspacesv1alpha1.Workspace, req StopRequest) (bool, error) {
	applied, err := appliedIntent(ws)
	if err != nil {
		return false, err
	}
	// Fence on the newest generation the workspace knows: the spec's and
	// the already-applied record's, whichever is higher.
	current := ws.Spec.RuntimeGeneration
	if applied.RuntimeGeneration > current {
		current = applied.RuntimeGeneration
	}
	if req.RuntimeGeneration != current {
		return false, ErrStaleGeneration
	}
	if applied.DesiredState == workspacesv1alpha1.DesiredStateStopped {
		return true, nil // stop already applied for this generation
	}
	applied.DesiredState = workspacesv1alpha1.DesiredStateStopped
	applied.AppliedAt = metav1.NewTime(r.now())
	raw, err := json.Marshal(applied)
	if err != nil {
		return false, err
	}
	setAnnotation(ws, AnnotationAppliedIntent, string(raw))
	if err := r.Update(ctx, ws); err != nil {
		return false, err
	}
	return true, nil
}

// NextLifecycleRequeue computes — from persisted object state only — the
// delay until the next operator-owned lifecycle deadline for ws: the
// template boot deadline while Provisioning, FailedCleanupDelay once a
// workspace is Failed, the stream-drain deadline while deleting, and the
// max-duration cap while a generation runs. It returns 0 when no deadline
// is pending. The operator uses this for reconcile requeues instead of
// fixed polls, so a restart recomputes identical deadlines and a crash can
// never strand a pending cleanup.
func (r *WorkspaceReconciler) NextLifecycleRequeue(ws *workspacesv1alpha1.Workspace) time.Duration {
	now := r.now()

	// Deleting: the only deadline left is the stream-drain window anchored
	// in the persisted finalizer progress.
	if !ws.DeletionTimestamp.IsZero() {
		prog, err := finalizerProgress(ws)
		if err != nil || prog.DrainStartedAt == nil {
			return 0
		}
		if prog.doneSet()[StepDrainStreams] {
			return 0
		}
		return positive(prog.DrainStartedAt.Add(MaxStreamDrain).Sub(now))
	}

	applied, err := appliedIntent(ws)
	if err != nil || applied.Revision == 0 {
		return 0
	}

	// A workspace Failed on the currently-applied intent is terminal until
	// an explicit retry; its only deadline is the incarnation teardown at
	// Degraded-transition + FailedCleanupDelay.
	if ws.Status.Phase == workspacesv1alpha1.WorkspacePhaseFailed &&
		ws.Status.LastAppliedIntentRevision == applied.Revision {
		anchor := degradedTransitionTime(ws)
		if anchor.IsZero() {
			return FailedCleanupDelay
		}
		return positive(anchor.Add(FailedCleanupDelay).Sub(now))
	}

	if applied.DesiredState != workspacesv1alpha1.DesiredStateRunning {
		return 0
	}
	snap, err := templateSnapshotFor(ws)
	if err != nil || snap == nil {
		return 0
	}

	// Earliest pending deadline wins.
	var earliest time.Time
	consider := func(t time.Time) {
		if earliest.IsZero() || t.Before(earliest) {
			earliest = t
		}
	}
	switch ws.Status.Phase {
	case workspacesv1alpha1.WorkspacePhasePending, workspacesv1alpha1.WorkspacePhaseProvisioning:
		// Boot deadline bounds the admit->Ready window, anchored the same
		// way reconcile computes it: max(appliedAt, first observation of
		// the current runtime incarnation) — the incarnation-start
		// annotation only counts when it still names the persisted
		// runtimeUID (a newer incarnation's record belongs to it alone).
		anchor := applied.AppliedAt.Time
		if rec, ok := incarnationStart(ws); ok && rec.RuntimeUID == ws.Status.RuntimeUID &&
			rec.At.Time.After(anchor) {
			anchor = rec.At.Time
		}
		consider(anchor.Add(snap.Spec.BootDeadline.Duration))
	}
	if started := incarnationStartedAt(ws); started != nil && snap.Spec.Lifecycle.MaxDuration.Duration > 0 {
		// Absolute generation cap — input never extends it. Measured from
		// the running incarnation's start, never an ended one's.
		consider(started.Add(snap.Spec.Lifecycle.MaxDuration.Duration))
	}
	if earliest.IsZero() {
		return 0
	}
	return positive(earliest.Sub(now))
}

// positive clamps a computed delay: an already-passed deadline requeues
// almost immediately (the work is due), never reports "no deadline".
func positive(d time.Duration) time.Duration {
	if d <= 0 {
		return requeueASAP
	}
	return d
}

// degradedTransitionTime reads the persisted anchor of the current
// impairment: the Degraded condition's lastTransitionTime.
func degradedTransitionTime(ws *workspacesv1alpha1.Workspace) time.Time {
	for _, c := range ws.Status.Conditions {
		if c.Type == workspacesv1alpha1.ConditionDegraded && c.Status == metav1.ConditionTrue {
			return c.LastTransitionTime.Time
		}
	}
	return time.Time{}
}

// failedCleanupRemaining is the live counterpart of the Failed branch in
// NextLifecycleRequeue: how long until the failed incarnation is torn
// down. <=0 means the deadline already passed.
func (r *WorkspaceReconciler) failedCleanupRemaining(ws *workspacesv1alpha1.Workspace) time.Duration {
	anchor := degradedTransitionTime(ws)
	if anchor.IsZero() {
		return FailedCleanupDelay
	}
	return anchor.Add(FailedCleanupDelay).Sub(r.now())
}
