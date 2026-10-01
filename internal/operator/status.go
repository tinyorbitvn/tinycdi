package operator

import (
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	workspacesv1alpha1 "github.com/tinyorbitvn/tinycdi/api/v1alpha1"
)

// Condition reasons for the lifecycle/finalizer surfaces .
// These flow into status.conditions[Degraded].reason and events so an
// operator can tell WHY teardown is blocked without reading logs.
const (
	// ReasonStopRequested — an explicit/expiry stop is being applied.
	ReasonStopRequested = "StopRequested"
	// ReasonIdleExpired — stopped because the input-idle deadline passed.
	ReasonIdleExpired = "IdleTimeout"
	// ReasonDisconnectExpired — stopped after the disconnect grace window.
	ReasonDisconnectExpired = "DisconnectTimeout"
	// ReasonMaxDurationExpired — stopped at the absolute duration cap.
	ReasonMaxDurationExpired = "MaxDuration"
	// ReasonStreamDraining — finalizer is inside the <=45 s drain window.
	ReasonStreamDraining = "StreamDraining"
	// ReasonDrainTimedOut — drain budget exhausted; teardown continues.
	ReasonDrainTimedOut = "DrainTimedOut"
	// ReasonRetentionPending — retention step has not completed; the
	// finalizer must not be removed while this reason is set.
	ReasonRetentionPending = "RetentionPending"
	// ReasonCleanupRetry — a teardown step failed and will be retried.
	ReasonCleanupRetry = "CleanupRetry"
	// ReasonFailedCleanup — Failed incarnation is being torn down.
	ReasonFailedCleanup = "FailedCleanup"
	// ReasonMissingWorkspaceID — the CR carries no platform workspace id
	// (the workspaces.cdi.tinyorbit.vn/workspace-uid label the API applier
	// stamps), so broker seams cannot be keyed safely. Teardown blocks
	// until a human restores the label; the CR's k8s UID is never used.
	ReasonMissingWorkspaceID = "MissingWorkspaceID"
)

// SetWorkspaceCondition upserts a status condition on ws with
// LastTransitionTime pinned to now (the reconciler's injectable clock).
// It is the single write path lifecycle and finalizer code use so reason
// strings stay consistent and ObservedGeneration tracking stays uniform.
// meta.SetStatusCondition preserves the previous LastTransitionTime when
// the status itself is unchanged, so anchors such as the Failed cleanup
// deadline and the drain window stay stable across reconciles.
func SetWorkspaceCondition(ws *workspacesv1alpha1.Workspace, condType string, status metav1.ConditionStatus, reason, message string, now time.Time) {
	meta.SetStatusCondition(&ws.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: ws.Generation,
		LastTransitionTime: metav1.NewTime(now),
	})
}

// MarkFinalizerStepBlocked records the failure of step on the Degraded
// condition (status=True, reason from the step) so a stuck teardown is
// visible on the object itself. Called by Finalizer.Run before returning a
// step error; never called once the workspace's finalizer may be removed.
func MarkFinalizerStepBlocked(ws *workspacesv1alpha1.Workspace, step FinalizerStep, err error, now time.Time) {
	reason := ReasonCleanupRetry
	switch step {
	case StepRetention:
		// The finalizer is never dropped while retention is unhandled —
		// the reason must name the dependency explicitly.
		reason = ReasonRetentionPending
	case StepDrainStreams:
		reason = ReasonStreamDraining
	}
	// The message never carries raw err text (SEC-I6): conditions are
	// tenant-visible API surface, so internals (addresses, queries, object
	// names, upstream errors) stay in the operator logs. The Reason and
	// the step name carry the machine-readable cause.
	msg := fmt.Sprintf("teardown step %s blocked; retrying — detail in the operator logs", step)
	SetWorkspaceCondition(ws, workspacesv1alpha1.ConditionDegraded,
		metav1.ConditionTrue, reason, msg, now)
}
