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
//   - the WorkspaceTemplate is resolved once at first admit into the
//     template-snapshot annotation (spec JSON + sha256 hash); it is re-taken
//     only when spec.runtimeGeneration advances and spec.templateRef moved
//     off the snapshot's source (an OnStart family re-point written by the
//     API's start path). Within one generation the template object is never
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
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
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

	// AnnotationTemplateSnapshot records the immutable template snapshot taken
	// at first admit (JSON templateSnapshot).
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
	// ReasonTemplateSnapshotInvalid — the recorded template-snapshot
	// annotation failed verification (tampered hash, a spec the CRD would
	// never admit, or content that disagrees with the live template
	// object); convergence is held until a human restores a valid record
	// (SEC-10).
	ReasonTemplateSnapshotInvalid = "TemplateSnapshotInvalid"
	ReasonProvisioning            = "Provisioning"
	ReasonReady                   = "Ready"
	ReasonStopped                 = "Stopped"
	ReasonTerminating             = "Terminating"
	ReasonNameConflict            = "NameConflict"
	ReasonBootDeadlineExceeded    = "BootDeadlineExceeded"
	ReasonBackendError            = "BackendError"
	ReasonNominal                 = "Nominal"

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

// templateSnapshot is the template copy recorded at admit, re-recorded when
// a start's family re-point moves spec.templateRef onto a newer revision.
type templateSnapshot struct {
	Name     string `json:"name"`
	UID      string `json:"uid"`
	Revision string `json:"revision"`
	// SpecHash is sha256 of the canonical spec JSON — provenance for the
	// recorded revision.
	SpecHash string                                   `json:"specHash"`
	Spec     workspacesv1alpha1.WorkspaceTemplateSpec `json:"spec"`
	// Annotations carries the admin-controlled template annotations the
	// backend honors (node-selector, seccomp-profile, apparmor-profile,
	// storage-class) — the
	// snapshot must capture them since the template object is never re-read.
	Annotations map[string]string `json:"annotations,omitempty"`
	// RuntimeGeneration is the applied spec.runtimeGeneration the snapshot
	// was recorded under; SourceRef is the spec.templateRef.name it was
	// taken from (the catalog base name the create path writes, or the
	// revision object name a carried re-point writes). Both are empty on
	// snapshots recorded before the re-snapshot machinery existed — the
	// recorded object name is the fallback source for those.
	RuntimeGeneration int64  `json:"runtimeGeneration,omitempty"`
	SourceRef         string `json:"sourceRef,omitempty"`
}

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
	return ctrl.Result{}, r.writeStatus(ctx, ws, applied, obs, phase, nil)
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
	// Recorded at first admit, and re-recorded when the applied generation
	// advanced AND spec.templateRef moved off the snapshot's source — the
	// API's start path re-points the reference only while the CR is Stopped
	// (CEL) and only when the compatibility guard allowed the move (E1/E2),
	// so a moved reference under a new generation is the re-snapshot signal.
	// Within one generation the recorded snapshot is authoritative: a pod
	// crash/recreate converges on it, never on a re-read of the template.
	snap, err := templateSnapshotFor(ws)
	if err != nil {
		return ctrl.Result{}, err
	}
	resnapshot := snap == nil
	if !resnapshot {
		stale, serr := r.snapshotStale(ctx, ws, applied, snap)
		if serr != nil {
			return ctrl.Result{}, serr
		}
		resnapshot = stale
	}
	if resnapshot {
		tpl, gerr := r.resolveTemplate(ctx, ws)
		switch {
		case apierrors.IsNotFound(gerr):
			obs := tcdiruntime.Observation{RuntimeGeneration: applied.RuntimeGeneration, Reason: ReasonTemplateNotFound}
			return ctrl.Result{RequeueAfter: requeueRetry}, r.writeStatus(ctx, ws, applied, obs,
				workspacesv1alpha1.WorkspacePhasePending, errors.New(ReasonTemplateNotFound))
		case gerr != nil:
			return ctrl.Result{}, gerr
		}
		prev := snap
		snap, err = snapshotTemplate(tpl)
		if err != nil {
			return ctrl.Result{}, err
		}
		snap.RuntimeGeneration = applied.RuntimeGeneration
		snap.SourceRef = ws.Spec.TemplateRef.Name
		raw, _ := json.Marshal(snap)
		setAnnotation(ws, AnnotationTemplateSnapshot, string(raw))
		if err := r.Update(ctx, ws); err != nil {
			return ctrl.Result{}, err
		}
		if prev != nil {
			logf.FromContext(ctx).Info("template snapshot re-recorded on new generation",
				"from", prev.Name, "to", snap.Name,
				"runtimeGeneration", applied.RuntimeGeneration)
		}
	} else if verr := r.verifySnapshot(ctx, ws, snap); verr != nil {
		// The annotation is plain object metadata — a principal able to
		// write Workspace objects can pre-seed or rewrite it, so a
		// snapshot that fails verification must never reach the backend
		// (SEC-10). Fail closed: no runtime children, the workspace marks
		// Degraded until a human restores a valid record. The freshly
		// recorded snapshot (the branch above) is trusted by construction.
		logf.FromContext(ctx).Info("template snapshot failed verification; held",
			"reason", verr.Error())
		obs := tcdiruntime.Observation{
			RuntimeGeneration: applied.RuntimeGeneration,
			Reason:            ReasonTemplateSnapshotInvalid,
		}
		return ctrl.Result{RequeueAfter: requeueRetry}, r.writeStatus(ctx, ws, applied, obs,
			workspacesv1alpha1.WorkspacePhasePending,
			errors.New(ReasonTemplateSnapshotInvalid))
	}
	tpl := &workspacesv1alpha1.WorkspaceTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:        snap.Name,
			Namespace:   ws.Namespace,
			Annotations: snap.Annotations,
		},
		Spec: snap.Spec,
	}

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
	return res, r.writeStatus(ctx, ws, applied, obs, phase, statusErr)
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
	return res, r.writeStatus(ctx, ws, applied, obs, workspacesv1alpha1.WorkspacePhaseFailed, statusErr)
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
func (r *WorkspaceReconciler) writeStatus(ctx context.Context, ws *workspacesv1alpha1.Workspace, applied *AppliedIntent, obs tcdiruntime.Observation, phase workspacesv1alpha1.WorkspacePhase, statusErr error) error {
	gen := ws.Generation
	st := &ws.Status
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

	return r.Status().Update(ctx, ws)
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

func templateSnapshotFor(ws *workspacesv1alpha1.Workspace) (*templateSnapshot, error) {
	raw := ws.Annotations[AnnotationTemplateSnapshot]
	if raw == "" {
		return nil, nil
	}
	s := &templateSnapshot{}
	if err := json.Unmarshal([]byte(raw), s); err != nil {
		return nil, fmt.Errorf("corrupt %s annotation: %w", AnnotationTemplateSnapshot, err)
	}
	return s, nil
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

// verifySnapshot re-establishes trust in a recorded template snapshot
// before it may drive convergence. The annotation is ordinary metadata —
// a principal able to write Workspace objects can pre-seed or rewrite it,
// bypassing every CRD/CEL guard real templates enforce — so the recorded
// content must re-earn trust on every pass:
//
//   - integrity: SpecHash must equal the sha256 snapshotTemplate writes
//     over the recorded spec JSON;
//   - consistency: the header fields must agree with the embedded spec;
//   - invariants: the spec must satisfy the CRD/CEL contract a real
//     template can never violate — a digest-pinned linux image and the
//     runtime-block exclusivity rules;
//   - provenance: while the recorded template object still exists under
//     the recorded UID, it must still belong to the catalog the
//     workspace's templateRef names AND its spec/annotations must equal
//     the recorded ones. A deleted or re-published revision (NotFound, or
//     a different UID) does NOT invalidate the snapshot — design §4 gives
//     the recorded revision lifetime validity; integrity + invariants are
//     the residual gate.
func (r *WorkspaceReconciler) verifySnapshot(ctx context.Context, ws *workspacesv1alpha1.Workspace, snap *templateSnapshot) error {
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
	if err := validateSnapshotSpec(&snap.Spec); err != nil {
		return err
	}

	live := &workspacesv1alpha1.WorkspaceTemplate{}
	gerr := r.Get(ctx, types.NamespacedName{Name: snap.Name, Namespace: ws.Namespace}, live)
	switch {
	case apierrors.IsNotFound(gerr):
		// The recorded revision is gone — the snapshot keeps its lifetime
		// validity on the strength of the checks above.
		return nil
	case gerr != nil:
		return gerr
	case string(live.UID) != snap.UID:
		// Same name, new object: a re-published revision does not
		// invalidate a snapshot recorded under the old one.
		return nil
	}
	if live.Name != ws.Spec.TemplateRef.Name &&
		live.Labels[provisioning.LabelCatalogName] != ws.Spec.TemplateRef.Name {
		return fmt.Errorf("recorded template %q belongs to a different catalog than templateRef %q",
			snap.Name, ws.Spec.TemplateRef.Name)
	}
	want, err := snapshotTemplate(live)
	if err != nil {
		return err
	}
	if want.SpecHash != snap.SpecHash || !maps.Equal(want.Annotations, snap.Annotations) {
		return errors.New("recorded spec/annotations differ from the live template object")
	}
	return nil
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
