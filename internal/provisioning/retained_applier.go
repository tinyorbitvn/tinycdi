package provisioning

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	workspacev1alpha1 "github.com/tinyorbitvn/tinycdi/api/v1alpha1"
	linux "github.com/tinyorbitvn/tinycdi/internal/runtime/linux"
)

// AnnotationRetainedDataRef records the rd_ record the consuming workspace
// was attached from — bookkeeping for operators; the mount contract itself
// is linux.AnnotationRetainedPVC(+UID).
const AnnotationRetainedDataRef = "workspaces.cdi.tinyorbit.vn/retained-data-ref"

// RetainedApplier wraps a WorkspaceApplier with the retained-data side
// effects the pure CR projection cannot express (design §5):
//
//   - a create intent carrying Spec.RetainedDataRef additionally stamps
//     the consuming Workspace CR's retained-pvc annotations, retargets the
//     retained PVC's workspace-uid/workspace-name labels to the consumer
//     (verified by PVC UID — never by name) and settles the record
//     Attaching -> Attached;
//   - a delete intent returns the consumed disk to the Retained state when
//     the workspace that claimed it is deleted, so the disk is never
//     orphaned by a mid-attach delete.
//
// All operations are idempotent: delivery is at-least-once, so a replayed
// intent must converge to the same end state.
type RetainedApplier struct {
	Next    WorkspaceApplier
	Client  client.Client
	Tenants TenantNamespaces
	Records *RetainedStore
}

// NewRetainedApplier wraps applier with the retained-data side effects.
func NewRetainedApplier(applier WorkspaceApplier, c client.Client, tenants TenantNamespaces, records *RetainedStore) *RetainedApplier {
	return &RetainedApplier{Next: applier, Client: c, Tenants: tenants, Records: records}
}

// Apply implements WorkspaceApplier.
func (a *RetainedApplier) Apply(ctx context.Context, in Intent) error {
	if err := a.Next.Apply(ctx, in); err != nil {
		return err
	}
	switch {
	case in.Kind == IntentCreate && in.Spec.RetainedDataRef != "":
		return a.applyAttach(ctx, in)
	case in.Kind == IntentDelete:
		return a.applyReturn(ctx, in)
	}
	return nil
}

// applyAttach retargets the retained PVC to the consuming workspace and
// settles the record. The PVC UID must match the record — a UID mismatch
// means the recorded volume is gone (or was never this object) and the
// claim must not proceed.
//
// It fails closed BEFORE any cluster mutation: the record must already be
// claimed for this workspace by its owner (Retained->Attaching through
// RetainedStore.AttachRetained, which performs the owner/tenant-admin,
// state and runtime checks and the quota transfer). A create intent
// carrying a bare rd_ reference is never sufficient — without the claim
// there is no CR stamp and no PVC relabel (SEC-01).
func (a *RetainedApplier) applyAttach(ctx context.Context, in Intent) error {
	rec, err := a.Records.GetRetained(ctx, in.TenantID, in.Spec.RetainedDataRef)
	if err != nil {
		return err
	}
	owner := in.Spec.OwnerIssuer + "|" + in.Spec.OwnerSubject
	settled := rec.State == RetainedStateAttached
	if rec.ConsumingWorkspaceID != string(in.WorkspaceUID) || rec.Owner != owner ||
		(rec.State != RetainedStateAttaching && !settled) {
		return fmt.Errorf("attach retained: record %s not claimed for workspace %s by %s: %w",
			rec.ID, in.WorkspaceUID, owner, ErrRetainedState)
	}
	ns, ok := a.Tenants.Namespace(in.TenantID)
	if !ok {
		return fmt.Errorf("attach retained: no namespace for tenant %q", in.TenantID)
	}

	// Stamp the mount reference on the consuming Workspace CR so the
	// backend mounts the retained volume by name.
	ws := &workspacev1alpha1.Workspace{}
	if err := a.Client.Get(ctx, client.ObjectKey{
		Namespace: ns, Name: WorkspaceCRName(in.WorkspaceUID),
	}, ws); err != nil {
		return fmt.Errorf("attach retained: get workspace CR: %w", err)
	}
	orig := ws.DeepCopy()
	if ws.Annotations == nil {
		ws.Annotations = map[string]string{}
	}
	ws.Annotations[linux.AnnotationRetainedPVC] = rec.PVCName
	ws.Annotations[linux.AnnotationRetainedPVCUID] = rec.PVCUID
	ws.Annotations[AnnotationRetainedDataRef] = rec.ID
	if ws.Annotations[linux.AnnotationRetainedPVC] != orig.Annotations[linux.AnnotationRetainedPVC] ||
		ws.Annotations[linux.AnnotationRetainedPVCUID] != orig.Annotations[linux.AnnotationRetainedPVCUID] ||
		ws.Annotations[AnnotationRetainedDataRef] != orig.Annotations[AnnotationRetainedDataRef] {
		if err := a.Client.Patch(ctx, ws, client.MergeFrom(orig)); err != nil {
			return fmt.Errorf("attach retained: patch workspace CR: %w", err)
		}
	}

	// Retarget the PVC's consumer labels — only after the UID check. The
	// workspace-uid label now names the consuming workspace, so the
	// consumer's own lifecycle (mount, re-retain, eventual purge) treats
	// the volume as its home disk. Dataset identity stays the PVC UID.
	pvc := &corev1.PersistentVolumeClaim{}
	if err := a.Client.Get(ctx, client.ObjectKey{
		Namespace: rec.PVCNamespace, Name: rec.PVCName,
	}, pvc); err != nil {
		return fmt.Errorf("attach retained: get pvc %s/%s: %w", rec.PVCNamespace, rec.PVCName, err)
	}
	if string(pvc.UID) != rec.PVCUID {
		return fmt.Errorf("attach retained: pvc %s/%s UID %s != recorded %s",
			rec.PVCNamespace, rec.PVCName, pvc.UID, rec.PVCUID)
	}
	porig := pvc.DeepCopy()
	if pvc.Labels == nil {
		pvc.Labels = map[string]string{}
	}
	// The workspace-uid label on a runtime child carries the CR's
	// metadata.uid — never the platform id — matching what the linux
	// backend stamps and what its claim checks enforce.
	pvc.Labels[linux.LabelWorkspaceUID] = string(ws.UID)
	pvc.Labels[linux.LabelWorkspaceName] = in.Spec.WorkspaceName
	if err := a.Client.Patch(ctx, pvc, client.MergeFrom(porig)); err != nil {
		return fmt.Errorf("attach retained: retarget pvc labels: %w", err)
	}

	// The claim is verified and the volume retargeted: settle the record.
	// A record already Attached is a redelivery of a completed attach —
	// the side effects above are idempotent, so this is a no-op success.
	// (The runtime mount itself converges through the backend's reconcile;
	// MVP proof of attach = verified claim transfer.)
	if settled {
		return nil
	}
	return a.Records.CompleteAttach(ctx, rec.ID, string(in.WorkspaceUID))
}

// applyReturn hands the disk back to the inventory when a consuming
// workspace is deleted — covering both a mid-attach delete and a later
// delete of an attached workspace.
func (a *RetainedApplier) applyReturn(ctx context.Context, in Intent) error {
	ref, err := a.Records.RetainedRefOf(ctx, string(in.WorkspaceUID))
	if err != nil {
		return err
	}
	if ref == "" {
		return nil
	}
	if err := a.Records.ReturnToRetained(ctx, ref, string(in.WorkspaceUID)); err != nil &&
		!errors.Is(err, ErrRetainedNotFound) && !errors.Is(err, ErrRetainedState) {
		return err
	}
	return nil
}

// ---------------------------------------------------------------------------
// Purge execution
// ---------------------------------------------------------------------------

// PurgeSweeper executes the destroy intents recorded by PurgeRetained
// (state Purging): for each record it verifies the volume is not attached
// to a live consumer, deletes the PVC and completes the record — the
// completion proof advances Purging -> Purged and releases the held disk
// quota. The backend never purges persistent data on its own; this is the
// only platform component allowed to delete a retained volume.
type PurgeSweeper struct {
	Records *RetainedStore
	Client  client.Client
	Poll    time.Duration
	Log     *slog.Logger
}

// NewPurgeSweeper builds a sweeper over the retained store and a cluster
// client. Poll defaults to 30 s.
func NewPurgeSweeper(records *RetainedStore, c client.Client, log *slog.Logger) *PurgeSweeper {
	if log == nil {
		log = slog.Default()
	}
	return &PurgeSweeper{Records: records, Client: c, Poll: 30 * time.Second, Log: log}
}

// Run sweeps until ctx is cancelled.
func (s *PurgeSweeper) Run(ctx context.Context) error {
	poll := s.Poll
	if poll <= 0 {
		poll = 30 * time.Second
	}
	tick := time.NewTicker(poll)
	defer tick.Stop()
	for {
		s.SweepOnce(ctx)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

// SweepOnce processes every recorded purge once; it is exported so it can
// also be driven manually (runbook drill, tests).
func (s *PurgeSweeper) SweepOnce(ctx context.Context) {
	recs, err := s.Records.PendingPurges(ctx, "")
	if err != nil {
		s.Log.Warn("retained purge sweep: list", "err", err)
		return
	}
	for i := range recs {
		if err := s.purgeOne(ctx, &recs[i]); err != nil {
			s.Log.Warn("retained purge sweep",
				"record", recs[i].ID, "pvc", recs[i].PVCName, "err", err)
		}
	}
}

// purgeOne deletes the recorded volume when it is verifiably not attached,
// then completes the record. Deletion and completion are both
// retry-safe: the next sweep finishes whatever a crash left.
func (s *PurgeSweeper) purgeOne(ctx context.Context, rec *RetainedRecord) error {
	pvc := &corev1.PersistentVolumeClaim{}
	err := s.Client.Get(ctx, client.ObjectKey{Namespace: rec.PVCNamespace, Name: rec.PVCName}, pvc)
	switch {
	case apierrors.IsNotFound(err):
		// Volume already gone — completion proof.
		return s.Records.CompletePurge(ctx, rec.ID)
	case err != nil:
		return err
	case string(pvc.UID) != rec.PVCUID:
		// A different dataset sits at the name: the recorded volume is
		// gone, and the foreign object is never ours to delete.
		return s.Records.CompletePurge(ctx, rec.ID)
	case pvc.Labels[linux.LabelDataRetained] != "true":
		return fmt.Errorf("pvc %s/%s lost its retained marker; refusing to delete", rec.PVCNamespace, rec.PVCName)
	}
	if pvc.DeletionTimestamp.IsZero() {
		attached, err := s.attached(ctx, rec)
		if err != nil {
			return err
		}
		if attached {
			return fmt.Errorf("pvc %s/%s still attached to a consumer", rec.PVCNamespace, rec.PVCName)
		}
		if err := s.Client.Delete(ctx, pvc); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	// Finish the delete: the admission-added pvc-protection finalizer can
	// hold the object on clusters without the PV protection controller
	// (envtest) — the purge already proved the volume is detached.
	cur := &corev1.PersistentVolumeClaim{}
	err = s.Client.Get(ctx, client.ObjectKey{Namespace: rec.PVCNamespace, Name: rec.PVCName}, cur)
	switch {
	case apierrors.IsNotFound(err):
	case err != nil:
		return err
	default:
		var keep []string
		for _, f := range cur.Finalizers {
			if f != "kubernetes.io/pvc-protection" {
				keep = append(keep, f)
			}
		}
		if len(keep) != len(cur.Finalizers) {
			cur.Finalizers = keep
			if err := s.Client.Update(ctx, cur); err != nil {
				return err
			}
		}
	}
	return s.Records.CompletePurge(ctx, rec.ID)
}

// attached reports whether any live consumer mounts the recorded claim: a
// pod with a PVC volume source, or a non-deleting Workspace CR whose
// retained-pvc annotation names it.
func (s *PurgeSweeper) attached(ctx context.Context, rec *RetainedRecord) (bool, error) {
	var pods corev1.PodList
	if err := s.Client.List(ctx, &pods, client.InNamespace(rec.PVCNamespace)); err != nil {
		return false, err
	}
	for k := range pods.Items {
		for _, v := range pods.Items[k].Spec.Volumes {
			if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == rec.PVCName {
				return true, nil
			}
		}
	}
	var wss workspacev1alpha1.WorkspaceList
	if err := s.Client.List(ctx, &wss, client.InNamespace(rec.PVCNamespace)); err != nil {
		return false, err
	}
	for k := range wss.Items {
		ws := &wss.Items[k]
		if ws.DeletionTimestamp.IsZero() && ws.Annotations[linux.AnnotationRetainedPVC] == rec.PVCName {
			return true, nil
		}
	}
	return false, nil
}
