// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

// Package operator holds the Workspace controller (design §4/§5).
//
// The reconcile is level-based and idempotent:
//   - the applied intent (desiredState, runtimeGeneration, dataPolicy) is
//     snapshotted into the workspaces.cdi.tinyorbit.vn/applied-intent annotation
//     the first time a newer spec.intentRevision is observed; a spec whose
//     intentRevision <= the recorded revision is ignored, so a replayed stale
//     intent can never flip desiredState back;
//   - the WorkspaceTemplate is resolved once at first admit into
//     status.templateSnapshot (spec JSON + sha256 hash) — writable only
//     through the workspaces/status subresource, which RBAC grants to the
//     operator service account alone — and mirrored to the
//     template-snapshot annotation for readers; it is re-taken only when
//     spec.runtimeGeneration advances and spec.templateRef moved off the
//     snapshot's source (an OnStart family re-point written by the API's
//     start path). Within one generation the template object is never
//     re-read — it may be deleted or re-published without touching a
//     running generation;
//   - all runtime convergence goes through the backend; status reports
//     observedRuntimeGeneration + runtimeUID of the CURRENT incarnation, and
//     Ready is reached only when the runtime's own readiness (healthcheck
//     probe) passes — Pod Running alone is never enough;
//   - a template's bootDeadline bounds Pending/Provisioning: past it, the
//     Workspace goes Failed with reason BootDeadlineExceeded.
package operator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	workspacesv1alpha1 "github.com/tinyorbitvn/tinycdi/api/v1alpha1"
	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
	tcdiruntime "github.com/tinyorbitvn/tinycdi/internal/runtime"
	linux "github.com/tinyorbitvn/tinycdi/internal/runtime/linux"
)

// Operator-owned metadata on the Workspace object.
const (
	// FinalizerRuntimeCleanup blocks deletion until the runtime is torn down
	// and retention policy is applied.
	FinalizerRuntimeCleanup = "workspaces.cdi.tinyorbit.vn/runtime-cleanup"

	// AnnotationAppliedIntent records the newest spec intent the operator has
	// adopted (JSON AppliedIntent). spec.intentRevision <= its revision is
	// ignored, so a replayed stale intent can never flip desiredState back.
	AnnotationAppliedIntent = "workspaces.cdi.tinyorbit.vn/applied-intent"

	// AnnotationTemplateSnapshot mirrors the operator-recorded
	// status.templateSnapshot (JSON TemplateSnapshot) for consumers that
	// predate the status field. It is plain object metadata — a Workspace
	// writer can rewrite it — so convergence NEVER reads it; the status
	// copy is the only source of truth.
	AnnotationTemplateSnapshot = "workspaces.cdi.tinyorbit.vn/template-snapshot"

	// AnnotationIncarnationStart records the first observation of the
	// current runtime incarnation (JSON incarnationRecord). The boot
	// deadline is anchored at max(appliedAt, incarnation start) so a
	// restored Workspace — whose recorded appliedAt predates the backup —
	// gets a fresh boot window for the incarnation the operator actually
	// converges, instead of latching BootDeadlineExceeded on the stale
	// appliedAt.
	AnnotationIncarnationStart = "workspaces.cdi.tinyorbit.vn/incarnation-start"
)

// Condition reasons surfaced through status.conditions.
const (
	ReasonTemplateResolved = "TemplateResolved"
	ReasonTemplateNotFound = "TemplateNotFound"
	// ReasonTemplateRejected — the backend refused the snapshot's requested
	// runtime configuration (e.g. an out-of-policy apparmor-profile
	// annotation value); no runtime children are created.
	ReasonTemplateRejected = "TemplateRejected"
	// ReasonTemplateInvalid — no trustworthy template source exists for
	// the workspace: status.templateSnapshot is absent or failed
	// verification AND no valid live template resolves under
	// spec.templateRef (the writer-controlled annotation is never
	// trusted). Convergence is held — no runtime children — and one
	// edge-triggered Warning event is emitted (SEC-10).
	ReasonTemplateInvalid = "TemplateInvalid"
	// ReasonTemplateRevisionGone — upgrade-path hold: a running workspace
	// has no status.templateSnapshot yet and its incarnation pod's
	// recorded template revision cannot be proven (the pod predates the
	// identity stamp, or the stamped revision object was pruned or
	// republished). The pod keeps running untouched; the workspace holds
	// Pending with Degraded=True and one edge-triggered Warning event
	// (emitted after the status persist). A stop/start re-snapshots from
	// the live template.
	ReasonTemplateRevisionGone = "TemplateRevisionGone"
	ReasonProvisioning         = "Provisioning"
	ReasonReady                = "Ready"
	ReasonStopped              = "Stopped"
	ReasonTerminating          = "Terminating"
	ReasonNameConflict         = "NameConflict"
	ReasonBootDeadlineExceeded = "BootDeadlineExceeded"
	ReasonBackendError         = "BackendError"
	ReasonNominal              = "Nominal"

	// ReasonRetainedClaimMissing — the Workspace says it consumes retained
	// data (retained-data-ref) but names no retained claim; the backend
	// refuses to build a default home in its place, so no runtime children
	// are created until the claim reference is present (FX-R20).
	ReasonRetainedClaimMissing = "RetainedClaimMissing"

	// ReasonWaitingForDisk — the Workspace names its retained claim but the
	// volume has not been retargeted to it yet (the attach is between
	// stamping the CR and relabelling the claim). Transient: shown as the
	// disk step on StorageReady, never as a backend error (V3.27). Distinct
	// from ReasonRetainedClaimMissing, which is the refusal when the claim
	// is not named at all.
	ReasonWaitingForDisk = "WaitingForDisk"

	// ReasonIntentBehind — the platform's intent stream trails the
	// workspace's applied intent fence: the applier dropped an intent whose
	// revision was behind the CR's (IntentBehindMark annotation), or the
	// spec itself predates the applied record. The stale intent is still
	// never applied; the condition reports the drift so an operator can
	// realign the stream (docs/runbooks/disaster-recovery.md).
	ReasonIntentBehind = "IntentBehind"
)

const (
	// requeueNotReady is the poll interval while a Running intent is
	// converging; pod watches trigger sooner, this also drives the boot
	// deadline clock.
	requeueNotReady = 3 * time.Second
	// requeueRetry is the interval for non-error soft retries (e.g. missing
	// template).
	requeueRetry = 10 * time.Second
)

// AppliedIntent is the persisted record of the newest spec intent the
// operator adopted. It is the fencing record: a spec carrying
// intentRevision <= Revision is ignored entirely.
type AppliedIntent struct {
	Revision          int64                           `json:"revision"`
	DesiredState      workspacesv1alpha1.DesiredState `json:"desiredState"`
	RuntimeGeneration int64                           `json:"runtimeGeneration"`
	DataPolicy        workspacesv1alpha1.DataPolicy   `json:"dataPolicy"`
	AppliedAt         metav1.Time                     `json:"appliedAt"`
}

// templateSnapshot aliases the status record type: the snapshot the
// operator writes to status.templateSnapshot (and mirrors to the
// annotation) is the api type, shared with every reader.
type templateSnapshot = workspacesv1alpha1.TemplateSnapshot

// WorkspaceReconciler reconciles Workspace objects against a runtime backend.
type WorkspaceReconciler struct {
	client.Client
	Scheme  *runtime.Scheme
	Backend tcdiruntime.Backend
	Now     func() time.Time

	// Teardown seams for the ordered finalizer . Connects,
	// Leases and Drainer are broker-side dependencies: while the broker's
	// internal API is unwired the reconciler installs
	// standaloneBroker, which admits teardown because no brokered leases or
	// gateway streams exist. Retention defaults to pvcRetention until the
	// retention inventory lands. Setting any of these replaces the
	// corresponding default — the Finalizer never treats a nil seam it was
	// explicitly handed as skippable.
	Connects  ConnectBlocker
	Leases    LeaseRevoker
	Drainer   StreamDrainer
	Retention RetentionHandler

	// Recorder emits Kubernetes events (the IntentBehind transition). Nil
	// skips emission — the condition still records the state.
	Recorder record.EventRecorder
}

// newFinalizer assembles the step-wise teardown executor for one run.
func (r *WorkspaceReconciler) newFinalizer() *Finalizer {
	f := &Finalizer{
		Client:    r.Client,
		Backend:   r.Backend,
		Now:       r.Now,
		Connects:  r.Connects,
		Leases:    r.Leases,
		Drainer:   r.Drainer,
		Retention: r.Retention,
	}
	if f.Connects == nil {
		f.Connects = standaloneBroker{}
	}
	if f.Leases == nil {
		f.Leases = standaloneBroker{}
	}
	if f.Drainer == nil {
		f.Drainer = standaloneBroker{}
	}
	if f.Retention == nil {
		f.Retention = pvcRetention{client: r.Client}
	}
	return f
}

// +kubebuilder:rbac:groups=workspaces.cdi.tinyorbit.vn,resources=workspaces,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=workspaces.cdi.tinyorbit.vn,resources=workspaces/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=workspaces.cdi.tinyorbit.vn,resources=workspaces/finalizers,verbs=update
// +kubebuilder:rbac:groups=workspaces.cdi.tinyorbit.vn,resources=workspacetemplates,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

func (r *WorkspaceReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// backendErrorMessage is the fixed condition text for ReasonBackendError.
// Like every tenant-visible message it never carries the raw error (SEC-I6):
// quota, admission and API-server text stays in the operator logs.
const backendErrorMessage = "the platform could not complete a runtime step; retrying - detail in the operator logs"

// recordBackendError surfaces a failed backend call on the object so the
// portal does not show a frozen workspace until the boot deadline: it sets
// RuntimeReady=False/BackendError and nothing else (no phase change, no
// Degraded - the reconcile is still retrying). The caller still returns the
// error so controller-runtime backs off. Optimistic-lock conflicts and
// cancelled contexts are routine and write nothing. keepReady leaves a Ready
// workspace alone: its runtime is up, and a transient control-plane error
// must not flicker the condition. Best effort: a failed status write is
// logged, never returned over the original error.
func (r *WorkspaceReconciler) recordBackendError(ctx context.Context, ws *workspacesv1alpha1.Workspace, err error, keepReady bool) {
	r.recordStepStatus(ctx, ws, workspacesv1alpha1.ConditionRuntimeReady, ReasonBackendError,
		backendErrorMessage, err, keepReady)
}

// waitingForDiskMessage is the fixed text for ReasonWaitingForDisk.
const waitingForDiskMessage = "waiting for the retained disk to be handed over to this workspace"

// recordStepStatus is the best-effort write behind recordBackendError and
// the WaitingForDisk step: condition typ is set False with reason/msg and
// nothing else changes. The caller returns err itself.
func (r *WorkspaceReconciler) recordStepStatus(ctx context.Context, ws *workspacesv1alpha1.Workspace, typ, reason, msg string, err error, keepReady bool) {
	if err == nil || apierrors.IsConflict(err) ||
		errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return
	}
	if keepReady && ws.Status.Phase == workspacesv1alpha1.WorkspacePhaseReady {
		return
	}
	cur := meta.FindStatusCondition(ws.Status.Conditions, typ)
	if cur != nil && cur.Status == metav1.ConditionFalse && cur.Reason == reason &&
		cur.ObservedGeneration == ws.Generation {
		return
	}
	SetWorkspaceCondition(ws, typ, metav1.ConditionFalse, reason, msg, r.now())
	if uerr := r.Status().Update(ctx, ws); uerr != nil {
		logf.FromContext(ctx).Error(uerr, "record step status", "condition", typ, "reason", reason)
	}
}

// Reconcile converges a Workspace toward its last applied intent.
func (r *WorkspaceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	ws := &workspacesv1alpha1.Workspace{}
	if err := r.Get(ctx, req.NamespacedName, ws); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// --- deletion path ------------------------------------------------------
	if !ws.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(ws, FinalizerRuntimeCleanup) {
			done, err := r.newFinalizer().Run(ctx, ws)
			if apierrors.IsNotFound(err) {
				// The object vanished under this reconcile (a previous
				// reconcile removed the finalizer and the cache still served
				// the Terminating copy). Gone is the goal of a delete.
				return ctrl.Result{}, nil
			}
			if err != nil {
				log.Error(err, "workspace teardown blocked; will retry")
				return ctrl.Result{}, err
			}
			if !done {
				// Inside the stream-drain window: requeue at the persisted
				// deadline rather than polling.
				return ctrl.Result{RequeueAfter: r.NextLifecycleRequeue(ws)}, nil
			}
			if controllerutil.RemoveFinalizer(ws, FinalizerRuntimeCleanup) {
				if err := r.Update(ctx, ws); err != nil {
					return ctrl.Result{}, client.IgnoreNotFound(err)
				}
			}
		}
		return ctrl.Result{}, nil
	}

	// --- finalizer ----------------------------------------------------------
	if controllerutil.AddFinalizer(ws, FinalizerRuntimeCleanup) {
		if err := r.Update(ctx, ws); err != nil {
			return ctrl.Result{}, err
		}
	}

	// --- intent fencing -----------------------------------------------------
	applied, err := appliedIntent(ws)
	if err != nil {
		return ctrl.Result{}, err
	}
	if ws.Spec.IntentRevision > applied.Revision {
		applied = &AppliedIntent{
			Revision:          ws.Spec.IntentRevision,
			DesiredState:      ws.Spec.DesiredState,
			RuntimeGeneration: ws.Spec.RuntimeGeneration,
			DataPolicy:        ws.Spec.DataPolicy,
			AppliedAt:         metav1.NewTime(r.now()),
		}
		raw, err := json.Marshal(applied)
		if err != nil {
			return ctrl.Result{}, err
		}
		setAnnotation(ws, AnnotationAppliedIntent, string(raw))
		// A newer spec intent can only have been written by a stream that
		// caught up to the fence: any drift marker is stale now.
		delete(ws.Annotations, provisioning.AnnotationIntentBehind)
		if err := r.Update(ctx, ws); err != nil {
			return ctrl.Result{}, err
		}
		log.Info("applied intent", "intentRevision", applied.Revision,
			"desiredState", applied.DesiredState,
			"runtimeGeneration", applied.RuntimeGeneration)
	}
	// A spec carrying intentRevision <= applied.Revision is stale: its fields
	// are ignored; convergence below targets the applied record only.

	if applied.DesiredState == workspacesv1alpha1.DesiredStateStopped {
		return r.reconcileStopped(ctx, ws, applied)
	}
	return r.reconcileRunning(ctx, ws, applied)
}

// reconcileStopped terminates the incarnation while keeping data allowed by
// dataPolicy.
func (r *WorkspaceReconciler) reconcileStopped(ctx context.Context, ws *workspacesv1alpha1.Workspace, applied *AppliedIntent) (ctrl.Result, error) {
	if err := r.Backend.Stop(ctx, ws); err != nil && !errors.Is(err, linux.ErrNameConflict) {
		r.recordBackendError(ctx, ws, err, false)
		return ctrl.Result{}, err
	}
	obs, err := r.Backend.Observe(ctx, ws)
	if err != nil {
		r.recordBackendError(ctx, ws, err, false)
		return ctrl.Result{}, err
	}

	phase := workspacesv1alpha1.WorkspacePhaseStopped
	if obs.RuntimeUID != "" {
		phase = workspacesv1alpha1.WorkspacePhaseStopping
	}
	return ctrl.Result{}, r.writeStatus(ctx, ws, applied, obs, phase, nil, nil)
}

// reconcileRunning converges toward a running incarnation under the applied
// generation.
func (r *WorkspaceReconciler) reconcileRunning(ctx context.Context, ws *workspacesv1alpha1.Workspace, applied *AppliedIntent) (ctrl.Result, error) {
	// --- terminal failure for this applied intent ---------------------------
	// Once a workspace goes Failed its incarnation is never respawned: the
	// only ways forward are the bounded cleanup at FailedCleanupDelay or an
	// explicit new intent (higher intentRevision). This is the guard against
	// an infinite create-fail-create loop.
	if ws.Status.Phase == workspacesv1alpha1.WorkspacePhaseFailed &&
		ws.Status.LastAppliedIntentRevision == applied.Revision {
		return r.reconcileFailed(ctx, ws, applied)
	}

	// --- template snapshot --------------------------------------------------
	// status.templateSnapshot is the ONLY source trusted for pod building:
	// it is writable solely through the workspaces/status subresource,
	// which RBAC grants to the operator service account alone, so a
	// Workspace spec writer cannot forge it. The annotation of the same
	// record is kept as a mirror for readers and is never trusted.
	snap := ws.Status.TemplateSnapshot
	if snap == nil {
		// Upgrade path: a RUNNING workspace's incarnation pod names its
		// template revision in operator-written stamped metadata; the
		// LIVE revision object supplies the record's content. The
		// writer-controlled annotation is never consulted — no pod (or a
		// stopped workspace) falls through to a fresh snapshot of the
		// live template, and an unprovable identity holds Degraded.
		adopted, hres, done, aerr := r.adoptSnapshotFromPod(ctx, ws, applied)
		if aerr != nil {
			return hres, aerr
		}
		if done {
			return hres, nil
		}
		if adopted != nil {
			if err := r.recordSnapshot(ctx, ws, adopted); err != nil {
				return ctrl.Result{}, err
			}
			snap = adopted
		}
	}
	// Re-record when the status record is unusable (failed integrity or
	// invariant checks — it is replaced by a fresh snapshot of the live
	// template rather than trusted), or when the applied generation
	// advanced AND spec.templateRef moved off the snapshot's source — the
	// API's start path re-points the reference only while the CR is
	// Stopped (CEL) and only when the compatibility guard allowed the move
	// (E1/E2), so a moved reference under a new generation is the
	// re-snapshot signal. Within one generation the recorded snapshot is
	// authoritative: a pod crash/recreate converges on it, never on a
	// re-read of the template.
	resnapshot := snap == nil
	if !resnapshot {
		if verr := verifySnapshotRecord(snap); verr != nil {
			logf.FromContext(ctx).Info("recorded template snapshot failed verification; re-snapshotting",
				"reason", verr.Error())
			resnapshot = true
		} else {
			stale, serr := r.snapshotStale(ctx, ws, applied, snap)
			if serr != nil {
				return ctrl.Result{}, serr
			}
			resnapshot = stale
		}
	}
	if resnapshot {
		prev := snap
		tpl, gerr := r.resolveTemplate(ctx, ws)
		switch {
		case apierrors.IsNotFound(gerr):
			// No usable operator-recorded snapshot and no valid live
			// template to take one from — fail closed rather than trust
			// the writer-controlled annotation.
			return r.holdTemplateInvalid(ctx, ws, applied,
				fmt.Sprintf("template %q does not resolve", ws.Spec.TemplateRef.Name))
		case gerr != nil:
			return ctrl.Result{}, gerr
		}
		recorded, rerr := snapshotTemplate(tpl)
		if rerr != nil {
			return ctrl.Result{}, rerr
		}
		snap = recorded
		snap.RuntimeGeneration = applied.RuntimeGeneration
		snap.SourceRef = ws.Spec.TemplateRef.Name
		if rerr := r.recordSnapshot(ctx, ws, snap); rerr != nil {
			return ctrl.Result{}, rerr
		}
		if prev != nil {
			logf.FromContext(ctx).Info("template snapshot re-recorded on new generation",
				"from", prev.Name, "to", snap.Name,
				"runtimeGeneration", applied.RuntimeGeneration)
		}
	} else {
		// Repair the annotation mirror if a Workspace writer diverged it;
		// readers see the status truth, never the drift.
		if merr := r.mirrorSnapshotAnnotation(ctx, ws, snap); merr != nil {
			return ctrl.Result{}, merr
		}
	}
	tpl := templateFromSnapshot(ws, snap)

	// --- converge runtime ----------------------------------------------------
	obs, berr := r.Backend.Ensure(ctx, ws, tpl)
	var statusErr error
	switch {
	case berr == nil:
	case errors.Is(berr, linux.ErrNameConflict):
		statusErr = errors.New(ReasonNameConflict)
		obs, _ = r.Backend.Observe(ctx, ws)
	case errors.Is(berr, linux.ErrRetainedClaimMissing):
		logf.FromContext(ctx).Info("retained claim reference missing; refusing to build a default home",
			"retainedDataRef", ws.Annotations[linux.AnnotationRetainedDataRef])
		statusErr = errors.New(ReasonRetainedClaimMissing)
		obs, _ = r.Backend.Observe(ctx, ws)
	case errors.Is(berr, linux.ErrTemplateRejected):
		logf.FromContext(ctx).Info("template rejected by backend", "error", berr)
		statusErr = errors.New(ReasonTemplateRejected)
		obs, _ = r.Backend.Observe(ctx, ws)
	case errors.Is(berr, linux.ErrRetainedNotClaimed):
		r.recordStepStatus(ctx, ws, workspacesv1alpha1.ConditionStorageReady, ReasonWaitingForDisk,
			waitingForDiskMessage, berr, true)
		return ctrl.Result{}, berr
	default:
		r.recordBackendError(ctx, ws, berr, true)
		return ctrl.Result{}, berr
	}

	// --- phase ---------------------------------------------------------------
	phase := workspacesv1alpha1.WorkspacePhaseProvisioning
	anchor, err := r.bootAnchor(ctx, ws, applied, obs)
	if err != nil {
		return ctrl.Result{}, err
	}
	deadline := anchor.Add(snap.Spec.BootDeadline.Duration)
	switch {
	case obs.StorageReady && obs.RuntimeReady && obs.ConnectionReady:
		phase = workspacesv1alpha1.WorkspacePhaseReady
	case !r.now().Before(deadline):
		phase = workspacesv1alpha1.WorkspacePhaseFailed
		statusErr = errors.New(ReasonBootDeadlineExceeded)
	}

	// --- max-duration expiry -------------------------------------------------
	// The absolute generation cap (design §8) fires regardless of activity:
	// once the running generation's age passes the template's MaxDuration the
	// workspace stops as if an expiry intent had arrived — driven through the
	// same applied-intent path RequestStop uses. The age is measured from
	// the running incarnation's start: a startedAt left by an ended
	// incarnation is not a start of this one (FX-R24).
	if started := incarnationStartedAt(ws); started != nil {
		maxDur := snap.Spec.Lifecycle.MaxDuration.Duration
		if maxDur > 0 && !r.now().Before(started.Add(maxDur)) {
			logf.FromContext(ctx).Info("max duration reached; applying stop",
				"runtimeGeneration", applied.RuntimeGeneration,
				"maxDuration", maxDur)
			return r.expireRunning(ctx, ws, applied)
		}
	}

	res := ctrl.Result{}
	if phase != workspacesv1alpha1.WorkspacePhaseReady &&
		phase != workspacesv1alpha1.WorkspacePhaseFailed {
		res.RequeueAfter = requeueNotReady
	}
	return res, r.writeStatus(ctx, ws, applied, obs, phase, statusErr, snap)
}

// incarnationEnded reports whether phase says the runtime incarnation that
// status.startedAt described is over.
func incarnationEnded(phase workspacesv1alpha1.WorkspacePhase) bool {
	switch phase {
	case workspacesv1alpha1.WorkspacePhaseStopped,
		workspacesv1alpha1.WorkspacePhaseStopping,
		workspacesv1alpha1.WorkspacePhaseFailed:
		return true
	}
	return false
}

// incarnationStartedAt returns when the running incarnation became Ready,
// or nil when status.startedAt is unset or was left by an incarnation that
// has since ended (a workspace written by an operator that never cleared it).
func incarnationStartedAt(ws *workspacesv1alpha1.Workspace) *metav1.Time {
	if ws.Status.StartedAt == nil || incarnationEnded(ws.Status.Phase) {
		return nil
	}
	return ws.Status.StartedAt
}

// incarnationRecord is the persisted first-observation timestamp of one
// runtime incarnation, keyed by its UID.
type incarnationRecord struct {
	RuntimeUID string      `json:"runtimeUID"`
	At         metav1.Time `json:"at"`
}

// incarnationStart reads the persisted incarnation-start record.
func incarnationStart(ws *workspacesv1alpha1.Workspace) (rec incarnationRecord, ok bool) {
	raw := ws.Annotations[AnnotationIncarnationStart]
	if raw == "" {
		return rec, false
	}
	if err := json.Unmarshal([]byte(raw), &rec); err != nil || rec.RuntimeUID == "" {
		return incarnationRecord{}, false
	}
	return rec, true
}

// bootAnchor returns the anchor the boot deadline is measured from: the
// applied intent's AppliedAt, raised to the first-observation time of the
// current runtime incarnation when that is later. The first time a
// runtime UID is observed its timestamp is persisted — restarts recompute
// the identical anchor. A restored workspace whose appliedAt predates the
// backup gets a fresh window from the incarnation created after restore;
// a genuinely stuck boot still fails bootDeadline after its incarnation
// appeared.
func (r *WorkspaceReconciler) bootAnchor(ctx context.Context, ws *workspacesv1alpha1.Workspace, applied *AppliedIntent, obs tcdiruntime.Observation) (time.Time, error) {
	anchor := applied.AppliedAt.Time
	if obs.RuntimeUID == "" {
		return anchor, nil
	}
	rec, ok := incarnationStart(ws)
	if !ok || rec.RuntimeUID != obs.RuntimeUID {
		rec = incarnationRecord{RuntimeUID: obs.RuntimeUID, At: metav1.NewTime(r.now())}
		raw, err := json.Marshal(rec)
		if err != nil {
			return anchor, err
		}
		setAnnotation(ws, AnnotationIncarnationStart, string(raw))
		if err := r.Update(ctx, ws); err != nil {
			return anchor, err
		}
	}
	if rec.At.Time.After(anchor) {
		anchor = rec.At.Time
	}
	return anchor, nil
}

// reconcileFailed handles a workspace that is terminal-Failed on the
// currently applied intent: no runtime is ever recreated — the failed
// incarnation is deleted once the persisted cleanup deadline passes, and
// the record stays Failed until an explicit new intent arrives.
func (r *WorkspaceReconciler) reconcileFailed(ctx context.Context, ws *workspacesv1alpha1.Workspace, applied *AppliedIntent) (ctrl.Result, error) {
	obs, err := r.Backend.Observe(ctx, ws)
	if err != nil {
		return ctrl.Result{}, err
	}
	res := ctrl.Result{}
	statusErr := errors.New(ReasonBootDeadlineExceeded)
	if remaining := r.failedCleanupRemaining(ws); remaining > 0 {
		res.RequeueAfter = remaining
	} else {
		if err := r.Backend.DeleteRuntime(ctx, ws); err != nil {
			logf.FromContext(ctx).Error(err, "failed-incarnation cleanup; will retry")
			return ctrl.Result{}, err
		}
		statusErr = errors.New(ReasonFailedCleanup)
	}
	return res, r.writeStatus(ctx, ws, applied, obs, workspacesv1alpha1.WorkspacePhaseFailed, statusErr, nil)
}

// expireRunning applies an out-of-band stop to the live generation: the
// applied intent is flipped to Stopped and the stopped path converges the
// incarnation down in the same pass.
func (r *WorkspaceReconciler) expireRunning(ctx context.Context, ws *workspacesv1alpha1.Workspace, applied *AppliedIntent) (ctrl.Result, error) {
	applied.DesiredState = workspacesv1alpha1.DesiredStateStopped
	applied.AppliedAt = metav1.NewTime(r.now())
	raw, err := json.Marshal(applied)
	if err != nil {
		return ctrl.Result{}, err
	}
	setAnnotation(ws, AnnotationAppliedIntent, string(raw))
	if err := r.Update(ctx, ws); err != nil {
		return ctrl.Result{}, err
	}
	return r.reconcileStopped(ctx, ws, applied)
}

// writeStatus projects the observation into status fields + conditions and
// persists it. statusErr, when non-nil, marks Degraded with its reason.
// snap, when non-nil, is the pass's recorded template snapshot — re-staged
// here because the drift path's main-resource update returns the stored
// (unstaged) status.
func (r *WorkspaceReconciler) writeStatus(ctx context.Context, ws *workspacesv1alpha1.Workspace, applied *AppliedIntent, obs tcdiruntime.Observation, phase workspacesv1alpha1.WorkspacePhase, statusErr error, snap *templateSnapshot) error {
	gen := ws.Generation
	// Intent-fence drift: reconcile the params annotation BEFORE any status
	// field is staged — persisting it needs a main-resource update whose
	// response carries the stored (unstaged) status.
	drift, derr := r.evalIntentDrift(ctx, ws, applied)
	if derr != nil {
		return derr
	}
	st := &ws.Status
	if snap != nil {
		st.TemplateSnapshot = snap
	}
	// A workspace that already latched Failed on this intent keeps its step
	// conditions as they were: they record which step stalled, and the
	// incarnation cleanup would otherwise overwrite them with "Provisioning"
	// once the pod is gone.
	frozen := phase == workspacesv1alpha1.WorkspacePhaseFailed &&
		st.Phase == workspacesv1alpha1.WorkspacePhaseFailed &&
		st.LastAppliedIntentRevision == applied.Revision
	// The persisted phase is read before it is overwritten: a startedAt
	// recorded under an ended phase must not carry into the next incarnation.
	priorEnded := incarnationEnded(st.Phase)
	st.ObservedGeneration = gen
	st.LastAppliedIntentRevision = applied.Revision
	st.Phase = phase

	st.RuntimeUID = obs.RuntimeUID
	if obs.RuntimeUID != "" {
		st.ObservedRuntimeGeneration = obs.RuntimeGeneration
	}
	st.ServiceRef = obs.ServiceRef

	now := metav1.NewTime(r.now())
	setCond := func(typ string, status metav1.ConditionStatus, reason, msg string) {
		meta.SetStatusCondition(&st.Conditions, metav1.Condition{
			Type:               typ,
			Status:             status,
			Reason:             reason,
			Message:            msg,
			ObservedGeneration: gen,
			LastTransitionTime: now,
		})
		// Reconcile-written conditions never carry message params: clear any
		// a finalizer-path write recorded for this type. The annotation only
		// persists through a main-resource update, not the status write
		// below — an entry the status write cannot drop stays inert anyway:
		// the API projects params only for an exact "<type>.<reason>" match.
		setConditionParams(ws, typ, reason, nil)
	}

	if applied.DesiredState == workspacesv1alpha1.DesiredStateRunning {
		setCond(workspacesv1alpha1.ConditionAdmitted, metav1.ConditionTrue,
			ReasonTemplateResolved, "template snapshot recorded at first admit")
	} else {
		setCond(workspacesv1alpha1.ConditionAdmitted, metav1.ConditionTrue,
			"IntentApplied", "stop intent applied")
	}

	// Step conditions: skipped while frozen (see above).
	setStep := func(typ string, status metav1.ConditionStatus, reason, msg string) {
		if !frozen {
			setCond(typ, status, reason, msg)
		}
	}
	switch {
	case obs.StorageReady:
		setStep(workspacesv1alpha1.ConditionStorageReady, metav1.ConditionTrue, ReasonReady, "")
	default:
		setStep(workspacesv1alpha1.ConditionStorageReady, metav1.ConditionFalse,
			ReasonProvisioning, "waiting for volumes")
	}

	switch {
	case applied.DesiredState == workspacesv1alpha1.DesiredStateStopped:
		setStep(workspacesv1alpha1.ConditionRuntimeReady, metav1.ConditionFalse,
			ReasonStopped, "runtime stopped by intent")
		setStep(workspacesv1alpha1.ConditionConnectionReady, metav1.ConditionFalse,
			ReasonStopped, "runtime stopped by intent")
	case obs.RuntimeReady:
		setStep(workspacesv1alpha1.ConditionRuntimeReady, metav1.ConditionTrue, ReasonReady, "")
	default:
		reason := obs.Reason
		if reason == "" {
			reason = ReasonProvisioning
		}
		if statusErr != nil && statusErr.Error() == ReasonBootDeadlineExceeded {
			reason = ReasonBootDeadlineExceeded
		}
		setStep(workspacesv1alpha1.ConditionRuntimeReady, metav1.ConditionFalse, reason, "")
	}
	if applied.DesiredState == workspacesv1alpha1.DesiredStateRunning {
		if obs.ConnectionReady {
			setStep(workspacesv1alpha1.ConditionConnectionReady, metav1.ConditionTrue, ReasonReady, "")
		} else {
			reason := obs.Reason
			if reason == "" || reason == "Ready" {
				reason = ReasonProvisioning
			}
			setStep(workspacesv1alpha1.ConditionConnectionReady, metav1.ConditionFalse, reason, "")
		}
	}

	switch {
	case statusErr != nil:
		setCond(workspacesv1alpha1.ConditionDegraded, metav1.ConditionTrue,
			statusErr.Error(), "")
	default:
		setCond(workspacesv1alpha1.ConditionDegraded, metav1.ConditionFalse, ReasonNominal, "")
	}

	// IntentBehind reports intent-fence drift (the applier's marker or a
	// spec trailing the applied record); it goes False once the stream is
	// aligned again. A workspace that never drifted gets no row.
	switch {
	case drift.drifted:
		SetWorkspaceConditionParams(ws, workspacesv1alpha1.ConditionIntentBehind,
			metav1.ConditionTrue, ReasonIntentBehind, intentBehindMessage,
			map[string]string{
				"crRevision":  drift.crRevision,
				"rowRevision": drift.rowRevision,
			}, now.Time)
	case drift.had:
		SetWorkspaceConditionParams(ws, workspacesv1alpha1.ConditionIntentBehind,
			metav1.ConditionFalse, ReasonNominal, intentAlignedMessage, nil, now.Time)
	}

	// timestamps. startedAt is the running incarnation's start: cleared when
	// an incarnation ended (now, or at the previous write) and set again when
	// the next one becomes Ready.
	if priorEnded || incarnationEnded(phase) {
		st.StartedAt = nil
	}
	if phase == workspacesv1alpha1.WorkspacePhaseReady && st.StartedAt == nil {
		st.StartedAt = &now
	}
	if phase == workspacesv1alpha1.WorkspacePhaseStopped && st.StoppedAt == nil {
		st.StoppedAt = &now
	}
	if phase != workspacesv1alpha1.WorkspacePhaseStopped {
		st.StoppedAt = nil
	}

	if err := r.Status().Update(ctx, ws); err != nil {
		return err
	}
	// The drift event is edge-triggered: it fires once on the transition
	// into drift, after the condition that proves it persisted.
	if drift.edge && r.Recorder != nil {
		r.Recorder.Eventf(ws, corev1.EventTypeWarning, ReasonIntentBehind,
			"intent stream revision %s is behind the applied revision %s; "+
				"new intents are dropped until the stream is realigned "+
				"(see the disaster-recovery runbook)",
			drift.rowRevision, drift.crRevision)
	}
	return nil
}

// intentBehindMessage is the fixed condition text for ReasonIntentBehind;
// tenant-visible, so it names no internals beyond the fence itself.
const intentBehindMessage = "the platform's intent stream is behind this workspace; new intents are held until it is realigned"

// intentAlignedMessage is the fixed text once the stream has caught up.
const intentAlignedMessage = "the intent stream is aligned with the workspace again"

// intentFenceDrift reports whether the intent stream trails the workspace's
// applied fence: either the applier stamped the intent-behind marker on a
// stale drop (the freshest signal — it carries the dropped intent's row
// revision), or spec.intentRevision itself predates the applied record.
// Returns the CR-side and stream-side revisions for the condition params.
func intentFenceDrift(ws *workspacesv1alpha1.Workspace, applied *AppliedIntent) (crRev, rowRev int64, drifted bool) {
	if raw := ws.Annotations[provisioning.AnnotationIntentBehind]; raw != "" {
		var m provisioning.IntentBehindMark
		if err := json.Unmarshal([]byte(raw), &m); err == nil {
			return m.CRRevision, m.RowRevision, true
		}
		// An unparseable marker still proves the applier saw a drop.
		return ws.Spec.IntentRevision, 0, true
	}
	if ws.Spec.IntentRevision < applied.Revision {
		return applied.Revision, ws.Spec.IntentRevision, true
	}
	return 0, 0, false
}

// intentDrift is the drift evaluation of one reconcile pass.
type intentDrift struct {
	drifted                 bool // the intent stream trails the applied fence
	had                     bool // an IntentBehind condition already exists (kept -> False)
	edge                    bool // this pass transitions into drift (fires the event)
	crRevision, rowRevision string
}

// evalIntentDrift evaluates the drift state and syncs the params
// annotation ahead of the status staging in writeStatus: the annotation
// persists only through a main-resource update (/status drops metadata),
// and that update must happen before status fields are staged because its
// response restores the stored status. A workspace that never drifted
// writes nothing — no update, no annotation entry.
func (r *WorkspaceReconciler) evalIntentDrift(ctx context.Context, ws *workspacesv1alpha1.Workspace, applied *AppliedIntent) (intentDrift, error) {
	var d intentDrift
	cur := meta.FindStatusCondition(ws.Status.Conditions, workspacesv1alpha1.ConditionIntentBehind)
	crRev, rowRev, drifted := intentFenceDrift(ws, applied)
	d.drifted = drifted
	d.had = cur != nil
	d.edge = drifted && (cur == nil || cur.Status != metav1.ConditionTrue)
	if !drifted && !d.had {
		return d, nil
	}
	d.crRevision = strconv.FormatInt(crRev, 10)
	d.rowRevision = strconv.FormatInt(rowRev, 10)

	annotations := maps.Clone(ws.Annotations)
	if drifted {
		setConditionParams(ws, workspacesv1alpha1.ConditionIntentBehind, ReasonIntentBehind,
			map[string]string{"crRevision": d.crRevision, "rowRevision": d.rowRevision})
	} else {
		setConditionParams(ws, workspacesv1alpha1.ConditionIntentBehind, "", nil)
	}
	if !maps.Equal(annotations, ws.Annotations) {
		if err := r.Update(ctx, ws); err != nil {
			return d, err
		}
	}
	return d, nil
}

// appliedIntent reads the persisted applied-intent record; a missing
// annotation yields a zero record (revision 0 < any real intentRevision).
func appliedIntent(ws *workspacesv1alpha1.Workspace) (*AppliedIntent, error) {
	raw := ws.Annotations[AnnotationAppliedIntent]
	if raw == "" {
		return &AppliedIntent{}, nil
	}
	a := &AppliedIntent{}
	if err := json.Unmarshal([]byte(raw), a); err != nil {
		return nil, fmt.Errorf("corrupt %s annotation: %w", AnnotationAppliedIntent, err)
	}
	return a, nil
}

// resolveTemplate fetches the WorkspaceTemplate named by
// spec.templateRef.name through the shared by-name resolver: the exact
// object first, else the newest revision carrying the catalog-name label
// equal to the reference, so a stopped workspace created before an upgrade
// that replaced the chart-seeded immutable revision still resolves.
// Nothing else is retried: a reference to a specific revision object that
// was deleted stays TemplateNotFound.
func (r *WorkspaceReconciler) resolveTemplate(ctx context.Context, ws *workspacesv1alpha1.Workspace) (*workspacesv1alpha1.WorkspaceTemplate, error) {
	return provisioning.ResolveTemplateByName(ctx, r.Client, ws.Namespace, ws.Spec.TemplateRef.Name)
}

// recordedSnapshot returns the operator-recorded template snapshot —
// status.templateSnapshot, falling back to the annotation mirror for rows
// recorded before the status field existed. The annotation side is for
// hints only (deadline requeues, retained-disk runtime labels): any caller
// needing provenance must go through status or the adoption pod proof —
// convergence never does this.
func recordedSnapshot(ws *workspacesv1alpha1.Workspace) *templateSnapshot {
	if s := ws.Status.TemplateSnapshot; s != nil {
		return s
	}
	raw := ws.Annotations[AnnotationTemplateSnapshot]
	if raw == "" {
		return nil
	}
	s := &templateSnapshot{}
	if err := json.Unmarshal([]byte(raw), s); err != nil {
		return nil
	}
	return s
}

// templateFromSnapshot reconstructs the WorkspaceTemplate the runtime
// backend converges on from a recorded snapshot — the object is never
// re-read, the record is the whole contract.
func templateFromSnapshot(ws *workspacesv1alpha1.Workspace, snap *templateSnapshot) *workspacesv1alpha1.WorkspaceTemplate {
	return &workspacesv1alpha1.WorkspaceTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:        snap.Name,
			Namespace:   ws.Namespace,
			Annotations: snap.Annotations,
		},
		Spec: snap.Spec,
	}
}

// recordSnapshot records snap for this pass: the annotation mirror is
// written first (a main-resource update, which returns the stored status),
// then the authoritative copy is staged on ws.Status for the pass's
// writeStatus to persist — the status subresource sees exactly one write
// per reconcile, and a crash can leave at most a stale mirror, never a
// stale truth.
func (r *WorkspaceReconciler) recordSnapshot(ctx context.Context, ws *workspacesv1alpha1.Workspace, snap *templateSnapshot) error {
	if err := r.mirrorSnapshotAnnotation(ctx, ws, snap); err != nil {
		return err
	}
	ws.Status.TemplateSnapshot = snap
	return nil
}

// mirrorSnapshotAnnotation keeps the template-snapshot annotation equal to
// the status record for consumers that predate the status field (the
// broker's expiry projection reads status first and falls back to the
// annotation only for pre-upgrade rows). A Workspace writer can rewrite
// the annotation at any time — harmless: nothing in convergence reads it.
func (r *WorkspaceReconciler) mirrorSnapshotAnnotation(ctx context.Context, ws *workspacesv1alpha1.Workspace, snap *templateSnapshot) error {
	raw, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	if ws.Annotations[AnnotationTemplateSnapshot] == string(raw) {
		return nil
	}
	setAnnotation(ws, AnnotationTemplateSnapshot, string(raw))
	return r.Update(ctx, ws)
}

// adoptSnapshotFromPod is the one-time upgrade path for workspaces
// admitted before status.templateSnapshot existed. The running
// incarnation pod — operator-owned, established by the backend — names
// the template revision it was built from in its stamped metadata; the
// LIVE WorkspaceTemplate object of that revision supplies the record's
// content, so the writer-controlled snapshot annotation is never
// consulted, and runtimeGeneration/sourceRef come from the workspace
// alone. Returns (nil, _, false, nil) when there is no proving pod: the
// caller falls through to a fresh snapshot of the live template. When
// the pod exists but its revision cannot be proven — it predates the
// stamps, the stamped object was pruned or republished, or the pod does
// not provably match it — done is true and the returned ctrl.Result is
// the Degraded/TemplateRevisionGone hold the caller must return.
func (r *WorkspaceReconciler) adoptSnapshotFromPod(ctx context.Context, ws *workspacesv1alpha1.Workspace, applied *AppliedIntent) (snap *templateSnapshot, res ctrl.Result, done bool, err error) {
	ident, ierr := r.Backend.PodTemplateIdentity(ctx, ws)
	if ierr != nil {
		return nil, ctrl.Result{}, false, ierr
	}
	if !ident.Owned {
		return nil, ctrl.Result{}, false, nil
	}
	gone := func(detail string) (*templateSnapshot, ctrl.Result, bool, error) {
		hres, hErr := r.holdTemplateRevisionGone(ctx, ws, applied, detail)
		return nil, hres, hErr == nil, hErr
	}
	if ident.Name == "" {
		return gone("the running pod predates the template-identity stamp")
	}
	live := &workspacesv1alpha1.WorkspaceTemplate{}
	gerr := r.Get(ctx, client.ObjectKey{Namespace: ws.Namespace, Name: ident.Name}, live)
	switch {
	case apierrors.IsNotFound(gerr):
		return gone(fmt.Sprintf("recorded template revision %q no longer exists", ident.Name))
	case gerr != nil:
		return nil, ctrl.Result{}, false, gerr
	}
	if live.Spec.Revision != ident.Revision {
		return gone(fmt.Sprintf("template %q now carries revision %q; the pod recorded %q",
			ident.Name, live.Spec.Revision, ident.Revision))
	}
	// The pod must also provably derive from that object — stamp match
	// or an exact rebuild — so a mislabeled pod cannot drag an unrelated
	// revision into status.
	ok, merr := r.Backend.PodMatchesTemplate(ctx, ws, live)
	if merr != nil {
		return nil, ctrl.Result{}, false, merr
	}
	if !ok {
		return gone(fmt.Sprintf("the running pod does not match template %q", ident.Name))
	}
	snap, rerr := snapshotTemplate(live)
	if rerr != nil {
		return nil, ctrl.Result{}, false, rerr
	}
	snap.SourceRef = ws.Spec.TemplateRef.Name
	snap.RuntimeGeneration = applied.RuntimeGeneration
	logf.FromContext(ctx).Info("template snapshot adopted into status",
		"name", snap.Name, "uid", snap.UID,
		"runtimeGeneration", snap.RuntimeGeneration)
	return snap, ctrl.Result{}, false, nil
}

// holdDegraded persists a Pending phase with Degraded=True/reason and
// emits exactly one Warning event on the transition into the hold —
// after the status write that proves the transition. Runtime children
// are left untouched: a held workspace keeps whatever incarnation it
// had.
func (r *WorkspaceReconciler) holdDegraded(ctx context.Context, ws *workspacesv1alpha1.Workspace, applied *AppliedIntent, reason, eventMsg string) (ctrl.Result, error) {
	obs := tcdiruntime.Observation{
		RuntimeGeneration: applied.RuntimeGeneration,
		Reason:            reason,
	}
	edge := func() bool {
		cur := meta.FindStatusCondition(ws.Status.Conditions, workspacesv1alpha1.ConditionDegraded)
		return cur == nil || cur.Status != metav1.ConditionTrue || cur.Reason != reason
	}()
	err := r.writeStatus(ctx, ws, applied, obs,
		workspacesv1alpha1.WorkspacePhasePending, errors.New(reason), nil)
	if err != nil {
		return ctrl.Result{}, err
	}
	if edge && r.Recorder != nil {
		r.Recorder.Event(ws, corev1.EventTypeWarning, reason, eventMsg)
	}
	return ctrl.Result{RequeueAfter: requeueRetry}, nil
}

// holdTemplateInvalid is the fail-closed exit of snapshot resolution: no
// operator-recorded snapshot is usable and nothing valid replaced it —
// the writer-controlled annotation is never trusted (SEC-10). No runtime
// children are built; the workspace holds Pending with Degraded=True
// reason=TemplateInvalid and requeues (a later-published template still
// resolves).
func (r *WorkspaceReconciler) holdTemplateInvalid(ctx context.Context, ws *workspacesv1alpha1.Workspace, applied *AppliedIntent, detail string) (ctrl.Result, error) {
	logf.FromContext(ctx).Info("no trustworthy template source; convergence held",
		"detail", detail)
	return r.holdDegraded(ctx, ws, applied, ReasonTemplateInvalid,
		"the workspace's template could not be established; runtime convergence is held")
}

// holdTemplateRevisionGone is the upgrade-path hold for a workspace whose
// running pod exists but whose recorded template revision cannot be
// proven — the pod predates identity stamps, the stamped object was
// pruned or republished, or the pod does not match it. The pod keeps
// running untouched; a stop/start re-snapshots from the live template.
func (r *WorkspaceReconciler) holdTemplateRevisionGone(ctx context.Context, ws *workspacesv1alpha1.Workspace, applied *AppliedIntent, detail string) (ctrl.Result, error) {
	logf.FromContext(ctx).Info("recorded template revision unavailable; workspace held",
		"detail", detail)
	return r.holdDegraded(ctx, ws, applied, ReasonTemplateRevisionGone,
		"the workspace's recorded template revision is no longer available; "+
			"the runtime keeps running — stop and start the workspace to re-snapshot from the current template")
}

// snapshotStale reports whether the recorded snapshot must be re-taken for
// the applied intent (E1): the applied spec.runtimeGeneration advanced past
// the generation the snapshot was recorded under AND spec.templateRef now
// names a source other than the one the snapshot was taken from.
//
// The reference comparison is against the recorded source ref, not the
// resolved object name: the create path writes the catalog (family) name
// into templateRef while the snapshot records the resolved revision object,
// so a bare name comparison would re-resolve the family to its newest
// member on every generation — silently overriding a guard/pinned stay the
// API's start path already decided. Only an actual reference move (the
// carried re-point of a start intent, or an admin edit made while Stopped)
// re-takes the snapshot.
//
// Snapshots recorded before this machinery existed carry no SourceRef:
// their recorded object name is the fallback source, extended by the
// catalog-name label while that object still exists, so a legacy workspace
// whose templateRef still holds the family name keeps its revision while a
// carried re-point to a sibling revision object re-takes it. Only a clean
// NotFound on the recorded object counts as "the revision is gone, resolve
// the reference fresh" — any other read error propagates and the reconcile
// retries, so a transient catalog miss can never look like a re-point.
func (r *WorkspaceReconciler) snapshotStale(ctx context.Context, ws *workspacesv1alpha1.Workspace, applied *AppliedIntent, snap *templateSnapshot) (bool, error) {
	if applied.RuntimeGeneration <= snap.RuntimeGeneration {
		return false, nil
	}
	ref := ws.Spec.TemplateRef.Name
	if snap.SourceRef != "" {
		return ref != snap.SourceRef, nil
	}
	if ref == snap.Name {
		return false, nil
	}
	live := &workspacesv1alpha1.WorkspaceTemplate{}
	gerr := r.Get(ctx, types.NamespacedName{Name: snap.Name, Namespace: ws.Namespace}, live)
	switch {
	case apierrors.IsNotFound(gerr):
		// The recorded revision object is gone — the moved reference stands
		// alone and re-resolves (a deleted revision adopts the newest).
		return true, nil
	case gerr != nil:
		return false, gerr
	}
	return live.Labels[provisioning.LabelCatalogName] != ref, nil
}

// snapshotDigestPattern mirrors the apiserver's Pattern validation on
// LinuxRuntimeSpec.Image — the digest-only invariant every real template
// satisfies and a forged snapshot must re-satisfy (SEC-10).
var snapshotDigestPattern = regexp.MustCompile(workspacesv1alpha1.DigestPattern)

// snapshotSessionCmdPattern mirrors the Pattern on
// LinuxRuntimeSpec.SessionCmd (printable ASCII, 1..512 chars).
var snapshotSessionCmdPattern = regexp.MustCompile(workspacesv1alpha1.SessionCmdPattern)

// verifySnapshotRecord re-checks the integrity and invariants of a recorded
// template snapshot before it may drive convergence:
//
//   - integrity: SpecHash must equal the sha256 snapshotTemplate writes
//     over the recorded spec JSON;
//   - consistency: the header fields must agree with the embedded spec;
//   - invariants: the spec must satisfy the CRD/CEL contract a real
//     template can never violate — a digest-pinned linux image and the
//     runtime-block exclusivity rules.
//
// Provenance is NOT re-checked here and needs no live object: the status
// copy is writable only by the operator service account, and an
// annotation-side record reaches the same function only through the
// upgrade-adoption pod proof — each establishes provenance at record time.
// A deleted or re-published revision therefore never invalidates a
// recorded snapshot (design §4).
func verifySnapshotRecord(snap *templateSnapshot) error {
	specJSON, err := json.Marshal(snap.Spec)
	if err != nil {
		return fmt.Errorf("spec does not marshal: %w", err)
	}
	sum := sha256.Sum256(specJSON)
	if snap.SpecHash != "sha256:"+hex.EncodeToString(sum[:]) {
		return errors.New("specHash does not cover the recorded spec")
	}
	if snap.Name == "" || snap.Spec.Revision == "" || snap.Revision != snap.Spec.Revision {
		return fmt.Errorf("header fields inconsistent: name=%q revision=%q spec.revision=%q",
			snap.Name, snap.Revision, snap.Spec.Revision)
	}
	return validateSnapshotSpec(&snap.Spec)
}

// validateSnapshotSpec re-checks the invariants the CRD enforces on a
// WorkspaceTemplate — the boundary a forged snapshot bypasses, so the
// embedded spec is checked again here (SEC-10).
func validateSnapshotSpec(spec *workspacesv1alpha1.WorkspaceTemplateSpec) error {
	switch spec.Runtime {
	case workspacesv1alpha1.RuntimeLinuxContainer:
		if spec.Linux == nil || spec.Windows != nil {
			return errors.New("runtime=LinuxContainer requires spec.linux and forbids spec.windows")
		}
		if !snapshotDigestPattern.MatchString(spec.Linux.Image) {
			return errors.New("spec.linux.image is not a digest-pinned reference")
		}
		// The adapter/sessionCmd invariants are enforced on real templates
		// by CRD enum + CEL; a forged snapshot must re-satisfy them here.
		switch spec.Linux.Adapter {
		case workspacesv1alpha1.AdapterNone, workspacesv1alpha1.AdapterKasm:
		default:
			return fmt.Errorf("unknown spec.linux.adapter %q", spec.Linux.Adapter)
		}
		if spec.Linux.SessionCmd != "" {
			if spec.Linux.Adapter != workspacesv1alpha1.AdapterKasm {
				return errors.New("spec.linux.sessionCmd is only valid with adapter=kasm")
			}
			if !snapshotSessionCmdPattern.MatchString(spec.Linux.SessionCmd) {
				return errors.New("spec.linux.sessionCmd is not printable ASCII within 512 chars")
			}
		}
		if spec.Linux.Adapter == workspacesv1alpha1.AdapterKasm && len(spec.Linux.Command) > 0 {
			return errors.New("spec.linux.command must be empty with adapter=kasm")
		}
	case workspacesv1alpha1.RuntimeWindowsVM:
		if spec.Windows == nil || spec.Linux != nil {
			return errors.New("runtime=WindowsVM requires spec.windows and forbids spec.linux")
		}
		if spec.Windows.SourcePVCRef.Namespace != "" {
			return errors.New("spec.windows.sourcePVCRef.namespace must be empty")
		}
	default:
		return fmt.Errorf("unknown runtime %q", spec.Runtime)
	}
	return nil
}

// snapshotTemplate records the template's identity and a hash of its spec so
// the workspace provably runs the revision that was admitted.
func snapshotTemplate(tpl *workspacesv1alpha1.WorkspaceTemplate) (*templateSnapshot, error) {
	specJSON, err := json.Marshal(tpl.Spec)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(specJSON)
	annotations := map[string]string{}
	for k, v := range tpl.Annotations {
		annotations[k] = v
	}
	return &templateSnapshot{
		Name:        tpl.Name,
		UID:         string(tpl.UID),
		Revision:    tpl.Spec.Revision,
		SpecHash:    "sha256:" + hex.EncodeToString(sum[:]),
		Spec:        tpl.Spec,
		Annotations: annotations,
	}, nil
}

func setAnnotation(ws *workspacesv1alpha1.Workspace, key, value string) {
	if ws.Annotations == nil {
		ws.Annotations = map[string]string{}
	}
	ws.Annotations[key] = value
}

// SetupWithManager registers the controller: watches the Workspace and every
// owned child kind, with a bounded exponential failure limiter.
func (r *WorkspaceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&workspacesv1alpha1.Workspace{}).
		Owns(&corev1.Pod{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.Secret{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Owns(&networkingv1.NetworkPolicy{}).
		WithOptions(controller.Options{
			// Bounded exponential backoff for retries.
			RateLimiter: workqueue.NewTypedItemExponentialFailureRateLimiter[ctrl.Request](
				100*time.Millisecond, 30*time.Second),
		}).
		Complete(r)
}
