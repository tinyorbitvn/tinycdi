package operator

import (
	"context"
	"errors"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	workspacesv1alpha1 "github.com/tinyorbitvn/tinycdi/api/v1alpha1"
	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
	linux "github.com/tinyorbitvn/tinycdi/internal/runtime/linux"
)

// Retained-disk metadata contract (design §5).
//
// PVC metadata is the SOURCE OF TRUTH for the retained inventory: the
// retention step stamps these keys on the persistent volume, the API DB
// retained_data rows are a transactional index, and the inventory can be
// fully reconstructed from PVC metadata after an API DB loss. Persistent
// PVCs never carry an ownerReference to the Workspace/VM — nothing is
// garbage-collected when the workspace object disappears.
const (
	// LabelDataRetained marks a PVC as platform-retained. Aliased to the
	// runtime-backend constant so the marker is identical whoever stamps it.
	LabelDataRetained = linux.LabelDataRetained

	// AnnotationRetainedTenant records the owning tenant id.
	AnnotationRetainedTenant = "workspaces.cdi.tinyorbit.vn/retained-tenant"
	// AnnotationRetainedOwner records the retaining owner's iss|sub.
	AnnotationRetainedOwner = "workspaces.cdi.tinyorbit.vn/retained-owner"
	// AnnotationRetainedSourceWorkspace records the display name of the
	// workspace the disk was retained from. The workspace UID itself stays
	// in the workspaces.cdi.tinyorbit.vn/workspace-uid label.
	AnnotationRetainedSourceWorkspace = "workspaces.cdi.tinyorbit.vn/retained-source-workspace"
	// AnnotationRetainedRuntime records the runtime family (LinuxContainer
	// | WindowsVM) so attach can reject cross-runtime templates.
	AnnotationRetainedRuntime = "workspaces.cdi.tinyorbit.vn/retained-runtime"
	// AnnotationRetainedAt records the RFC3339 retain time.
	AnnotationRetainedAt = "workspaces.cdi.tinyorbit.vn/retained-at"
)

var (
	// ErrForeignPVC refuses adoption of a PVC whose labels do not pin it to
	// the workspace being processed — names are reusable; UID is identity.
	ErrForeignPVC = errors.New("retention: PVC is not owned by this workspace")
	// ErrDecommissionBlocked means a namespace still holds retained
	// datasets, so the tenant decommission workflow must not proceed.
	ErrDecommissionBlocked = errors.New("retention: namespace still holds retained datasets")
	// ErrVolumeAttached means a purge was attempted on a volume a live
	// consumer still mounts — the delete is refused until the consumer
	// detaches.
	ErrVolumeAttached = errors.New("retention: volume is still attached to a consumer")
)

// RetainedDisk is the operator-side view of one retained dataset. Identity
// is (Namespace, PVCUID) + WorkspaceUID — never a bare PVC name.
type RetainedDisk struct {
	Namespace           string
	PVCName             string
	PVCUID              types.UID
	WorkspaceUID        types.UID // source workspace's CR metadata.uid (child label value)
	SourceWorkspaceName string
	TenantID            string
	OwnerSubject        string // iss|sub
	Runtime             string
	SizeBytes           int64
	RetainedAt          time.Time
}

// RetentionInventory is the controller-owned inventory of retained
// datasets, derived exclusively from PVC metadata so it survives API DB
// loss and operator restarts.
type RetentionInventory struct {
	Client client.Client
}

// NewRetentionInventory builds an inventory over the cluster client.
func NewRetentionInventory(c client.Client) *RetentionInventory {
	return &RetentionInventory{Client: c}
}

// MarkRetained stamps the inventory contract (LabelDataRetained + retained
// annotations carrying tenant/owner/source-workspace/runtime/retained-at)
// on every persistent PVC of ws. It is idempotent and refuses a PVC that
// squats on the workspace's deterministic home-volume name while carrying
// a different workspace UID (ErrForeignPVC): dataset identity is PVC UID +
// workspace UID, never an arbitrary name.
func (i *RetentionInventory) MarkRetained(ctx context.Context, ws *workspacesv1alpha1.Workspace) error {
	uid := ws.UID

	// Foreign-PVC guard: a same-named volume owned by a different
	// workspace UID must never be stamped into this workspace's inventory.
	named := &corev1.PersistentVolumeClaim{}
	err := i.Client.Get(ctx, client.ObjectKey{Name: linux.PVCName(uid), Namespace: ws.Namespace}, named)
	switch {
	case apierrors.IsNotFound(err):
	case err != nil:
		return err
	case named.Labels[linux.LabelWorkspaceUID] != string(uid):
		return ErrForeignPVC
	}

	var pvcs corev1.PersistentVolumeClaimList
	if err := i.Client.List(ctx, &pvcs, client.InNamespace(ws.Namespace),
		client.MatchingLabels{linux.LabelWorkspaceUID: string(uid)}); err != nil {
		return err
	}

	runtime := i.runtimeFor(ctx, ws)
	tenant := ws.Labels[provisioning.LabelTenant]
	owner := ws.Spec.OwnerSubject.Issuer + "|" + ws.Spec.OwnerSubject.Subject
	now := time.Now().UTC().Format(time.RFC3339)

	for k := range pvcs.Items {
		pvc := &pvcs.Items[k]
		orig := pvc.DeepCopy()
		if pvc.Labels == nil {
			pvc.Labels = map[string]string{}
		}
		pvc.Labels[LabelDataRetained] = "true"
		if pvc.Annotations == nil {
			pvc.Annotations = map[string]string{}
		}
		pvc.Annotations[AnnotationRetainedTenant] = tenant
		pvc.Annotations[AnnotationRetainedOwner] = owner
		pvc.Annotations[AnnotationRetainedSourceWorkspace] = ws.Name
		rt := runtime
		if rt == "" {
			// Keep a previously resolved runtime; last resort is the only
			// MVP runtime family.
			rt = pvc.Annotations[AnnotationRetainedRuntime]
		}
		if rt == "" {
			rt = string(workspacesv1alpha1.RuntimeLinuxContainer)
		}
		pvc.Annotations[AnnotationRetainedRuntime] = rt
		if pvc.Annotations[AnnotationRetainedAt] == "" {
			pvc.Annotations[AnnotationRetainedAt] = now
		}
		if err := i.Client.Patch(ctx, pvc, client.MergeFrom(orig)); err != nil {
			return err
		}
	}
	return nil
}

// runtimeFor resolves the runtime family to record on the disk: the
// immutable template snapshot first (authoritative for a workspace that
// ran), then the live WorkspaceTemplate CR. "" when neither resolves — the
// caller falls back to the PVC's existing annotation, then LinuxContainer.
func (i *RetentionInventory) runtimeFor(ctx context.Context, ws *workspacesv1alpha1.Workspace) string {
	if snap := recordedSnapshot(ws); snap != nil && snap.Spec.Runtime != "" {
		return string(snap.Spec.Runtime)
	}
	tpl := &workspacesv1alpha1.WorkspaceTemplate{}
	if err := i.Client.Get(ctx, client.ObjectKey{
		Namespace: ws.Namespace, Name: ws.Spec.TemplateRef.Name,
	}, tpl); err == nil && tpl.Spec.Runtime != "" {
		return string(tpl.Spec.Runtime)
	}
	return ""
}

// List returns every retained dataset in namespace — the PVCs carrying
// LabelDataRetained — decoded into RetainedDisk. Namespace "" lists all
// namespaces the controller can see.
func (i *RetentionInventory) List(ctx context.Context, namespace string) ([]RetainedDisk, error) {
	var pvcs corev1.PersistentVolumeClaimList
	opts := []client.ListOption{client.MatchingLabels{LabelDataRetained: "true"}}
	if namespace != "" {
		opts = append(opts, client.InNamespace(namespace))
	}
	if err := i.Client.List(ctx, &pvcs, opts...); err != nil {
		return nil, err
	}
	out := make([]RetainedDisk, 0, len(pvcs.Items))
	for k := range pvcs.Items {
		out = append(out, decodeRetainedDisk(&pvcs.Items[k]))
	}
	return out, nil
}

// decodeRetainedDisk projects the stamped PVC metadata into the inventory
// record. Identity comes from the object itself (UID) and the
// workspace-uid label — never the reusable PVC name.
func decodeRetainedDisk(pvc *corev1.PersistentVolumeClaim) RetainedDisk {
	d := RetainedDisk{
		Namespace:           pvc.Namespace,
		PVCName:             pvc.Name,
		PVCUID:              pvc.UID,
		WorkspaceUID:        types.UID(pvc.Labels[linux.LabelWorkspaceUID]),
		SourceWorkspaceName: pvc.Annotations[AnnotationRetainedSourceWorkspace],
		TenantID:            pvc.Annotations[AnnotationRetainedTenant],
		OwnerSubject:        pvc.Annotations[AnnotationRetainedOwner],
		Runtime:             pvc.Annotations[AnnotationRetainedRuntime],
	}
	if q := pvc.Spec.Resources.Requests.Storage(); q != nil {
		d.SizeBytes = q.Value()
	}
	if ts := pvc.Annotations[AnnotationRetainedAt]; ts != "" {
		if t, err := time.Parse(time.RFC3339, ts); err == nil {
			d.RetainedAt = t
		}
	}
	return d
}

// Unregistered returns retained-labelled PVCs that isKnown does not
// recognise — orphan candidates created by API DB loss or a missed import.
// isKnown answers whether the API DB has a live record for the PVC UID.
func (i *RetentionInventory) Unregistered(ctx context.Context, namespace string, isKnown func(ctx context.Context, pvcUID types.UID) (bool, error)) ([]RetainedDisk, error) {
	disks, err := i.List(ctx, namespace)
	if err != nil {
		return nil, err
	}
	var out []RetainedDisk
	for _, d := range disks {
		known, err := isKnown(ctx, d.PVCUID)
		if err != nil {
			return nil, err
		}
		if !known {
			out = append(out, d)
		}
	}
	return out, nil
}

// MissingVolumes returns the subset of want (PVC UIDs of live API-DB
// records) with no PVC in namespace — records pointing at volumes that
// vanished underneath the inventory.
func (i *RetentionInventory) MissingVolumes(ctx context.Context, namespace string, want []types.UID) ([]types.UID, error) {
	var pvcs corev1.PersistentVolumeClaimList
	opts := []client.ListOption{}
	if namespace != "" {
		opts = append(opts, client.InNamespace(namespace))
	}
	if err := i.Client.List(ctx, &pvcs, opts...); err != nil {
		return nil, err
	}
	present := make(map[types.UID]bool, len(pvcs.Items))
	for k := range pvcs.Items {
		present[pvcs.Items[k].UID] = true
	}
	var out []types.UID
	for _, uid := range want {
		if !present[uid] {
			out = append(out, uid)
		}
	}
	return out, nil
}

// DecommissionCheck lists retained datasets still present in namespace. A
// non-empty result blocks tenant-namespace decommission: deleting a
// namespace must never silently take retained user data with it.
func (i *RetentionInventory) DecommissionCheck(ctx context.Context, namespace string) ([]RetainedDisk, error) {
	return i.List(ctx, namespace)
}

// Purge deletes the retained volume behind disk — the cluster-side half of
// Retained -> Purging -> Purged. It refuses to delete a volume that is
// still attached to a live consumer (ErrVolumeAttached) or one that does
// not carry the retained marker, and it is retry-safe: an absent volume or
// a volume whose UID differs from the record (the recorded dataset is
// already gone) is treated as already-purged.
func (i *RetentionInventory) Purge(ctx context.Context, disk RetainedDisk) error {
	pvc := &corev1.PersistentVolumeClaim{}
	err := i.Client.Get(ctx, client.ObjectKey{Name: disk.PVCName, Namespace: disk.Namespace}, pvc)
	switch {
	case apierrors.IsNotFound(err):
		return nil // recorded volume already gone — purge's goal state
	case err != nil:
		return err
	case pvc.UID != disk.PVCUID:
		// A different dataset reuses the name: the recorded volume is
		// gone, and the foreign one is never ours to delete.
		return nil
	case pvc.Labels[LabelDataRetained] != "true":
		return ErrForeignPVC // unmarked volume: never delete silently
	}
	if !pvc.DeletionTimestamp.IsZero() {
		return nil
	}
	attached, err := i.volumeAttached(ctx, disk.Namespace, disk.PVCName)
	if err != nil {
		return err
	}
	if attached {
		return ErrVolumeAttached
	}
	if err := i.Client.Delete(ctx, pvc); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return finishPVCDelete(ctx, i.Client, pvc)
}

// finishPVCDelete removes the admission-added pvc-protection finalizer
// once deletion has been requested, so the object actually disappears on
// clusters where the PV protection controller is absent (envtest) or the
// volume is already unused (ordered teardown / verified-detached purge).
// The strip is a merge patch — identical to the backend purge sweeper,
// whose Role grants no update verb — and only ever runs on a PVC that is
// already terminating.
func finishPVCDelete(ctx context.Context, c client.Client, pvc *corev1.PersistentVolumeClaim) error {
	cur := &corev1.PersistentVolumeClaim{}
	err := c.Get(ctx, client.ObjectKeyFromObject(pvc), cur)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if cur.DeletionTimestamp.IsZero() {
		// Not terminating: pvc-protection is doing its job — leave it.
		return nil
	}
	orig := cur.DeepCopy()
	keep := cur.Finalizers[:0]
	for _, f := range cur.Finalizers {
		if f != pvcProtectionFinalizer {
			keep = append(keep, f)
		}
	}
	if len(keep) == len(cur.Finalizers) {
		return nil
	}
	cur.Finalizers = keep
	return c.Patch(ctx, cur, client.MergeFrom(orig))
}

// volumeAttached reports whether any live consumer mounts the claim: a
// running pod with a PVC volume source, or a non-deleting Workspace CR
// whose retained-pvc annotation names it.
func (i *RetentionInventory) volumeAttached(ctx context.Context, namespace, claimName string) (bool, error) {
	var pods corev1.PodList
	if err := i.Client.List(ctx, &pods, client.InNamespace(namespace)); err != nil {
		return false, err
	}
	for k := range pods.Items {
		for _, v := range pods.Items[k].Spec.Volumes {
			if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == claimName {
				return true, nil
			}
		}
	}
	var wss workspacesv1alpha1.WorkspaceList
	if err := i.Client.List(ctx, &wss, client.InNamespace(namespace)); err != nil {
		return false, err
	}
	for k := range wss.Items {
		ws := &wss.Items[k]
		if ws.DeletionTimestamp.IsZero() && ws.Annotations[linux.AnnotationRetainedPVC] == claimName {
			return true, nil
		}
	}
	return false, nil
}

// RetentionApplier implements the finalizer's RetentionHandler seam
// (StepRetention): dataPolicy Retain stamps the workspace's persistent
// PVCs into the inventory; Ephemeral deletes them. It is idempotent and
// must be safe to re-run after a crash mid-step.
type RetentionApplier struct {
	Client    client.Client
	Inventory *RetentionInventory
}

// ApplyRetention applies ws's dataPolicy to its persistent volumes.
func (a *RetentionApplier) ApplyRetention(ctx context.Context, ws *workspacesv1alpha1.Workspace) error {
	inv := a.Inventory
	if inv == nil {
		inv = NewRetentionInventory(a.Client)
	}
	if ws.Spec.DataPolicy == workspacesv1alpha1.DataPolicyRetain {
		return inv.MarkRetained(ctx, ws)
	}
	// Ephemeral: destroy the workspace's data volumes. A volume already
	// marked retained is NEVER deleted here — retained data is destroyed
	// only through the explicit purge path.
	var pvcs corev1.PersistentVolumeClaimList
	if err := a.Client.List(ctx, &pvcs, client.InNamespace(ws.Namespace),
		client.MatchingLabels{linux.LabelWorkspaceUID: string(ws.UID)}); err != nil {
		return err
	}
	for k := range pvcs.Items {
		pvc := &pvcs.Items[k]
		if pvc.Labels[LabelDataRetained] == "true" {
			continue
		}
		if err := a.destroyEphemeral(ctx, pvc); err != nil {
			return err
		}
	}
	return nil
}

// pvcProtectionFinalizer is the admission-added finalizer that delays PVC
// deletion until the volume is unused. The retention step only deletes
// Ephemeral volumes after the runtime is already down (ordered teardown),
// so a PVC left hanging on that finalizer — e.g. on a cluster without the
// PV protection controller, or in envtest — is force-finished here.
const pvcProtectionFinalizer = "kubernetes.io/pvc-protection"

// destroyEphemeral deletes a disposable volume and finishes the delete
// when nothing but the pvc-protection finalizer holds it back.
func (a *RetentionApplier) destroyEphemeral(ctx context.Context, pvc *corev1.PersistentVolumeClaim) error {
	if pvc.DeletionTimestamp.IsZero() {
		if err := a.Client.Delete(ctx, pvc); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return err
		}
	}
	return finishPVCDelete(ctx, a.Client, pvc)
}
