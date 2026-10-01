package operator

// Contract tests — the operator-side retained-disk
// inventory (design §5). The tests pin the intended behaviour — failing
// on missing behaviour, never on a compile error.
//
// Asserted contract:
//   - Delete+Retain stamps the inventory metadata on the workspace's
//     persistent PVCs; the inventory is derived from PVC metadata and is
//     reconstructable after API DB loss;
//   - dataset identity is (namespace, PVC UID) + workspace UID — a
//     same-named PVC under a different UID is a different dataset and is
//     never adopted;
//   - retained PVCs carry no ownerReference to the Workspace;
//   - orphan detection is bidirectional: retained-labelled PVCs unknown to
//     the API DB AND records whose PVC vanished are both surfaced;
//   - namespace decommission cannot proceed while retained disks exist;
//   - the finalizer's retention step (RetentionApplier) is idempotent.
//
// Run: KUBEBUILDER_ASSETS=<repo>/bin/k8s/1.37.0-linux-amd64 \
//      go test ./internal/operator/ -run TestRetention -v

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	workspacesv1alpha1 "github.com/tinyorbitvn/tinycdi/api/v1alpha1"
	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
	linux "github.com/tinyorbitvn/tinycdi/internal/runtime/linux"
)

// newHomePVC creates the workspace's persistent home volume object the way
// the linux backend names/labels it.
func newHomePVC(t *testing.T, c client.Client, ns string, ws *workspacesv1alpha1.Workspace) *corev1.PersistentVolumeClaim {
	t.Helper()
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      linux.PVCName(ws.UID),
			Namespace: ns,
			Labels: map[string]string{
				linux.LabelWorkspaceUID:  string(ws.UID),
				linux.LabelWorkspaceName: ws.Name,
				linux.LabelDataRole:      "home",
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("20Gi")},
			},
		},
	}
	if err := c.Create(context.Background(), pvc); err != nil {
		t.Fatalf("create pvc: %v", err)
	}
	return pvc
}

func getPVC(t *testing.T, c client.Client, ns, name string) *corev1.PersistentVolumeClaim {
	t.Helper()
	pvc := &corev1.PersistentVolumeClaim{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: ns}, pvc); err != nil {
		t.Fatalf("get pvc %s/%s: %v", ns, name, err)
	}
	return pvc
}

func retainWorkspace(t *testing.T, c client.Client, ns, name, tpl string) *workspacesv1alpha1.Workspace {
	t.Helper()
	return newWorkspace(t, c, ns, name, tpl, func(ws *workspacesv1alpha1.Workspace) {
		ws.Spec.DataPolicy = workspacesv1alpha1.DataPolicyRetain
		ws.Labels = map[string]string{provisioning.LabelTenant: "tenant-a"}
	})
}

// TestRetentionMarkAndList: MarkRetained stamps the inventory contract on
// the workspace PVC; List decodes it back with the full identity.
func TestRetentionMarkAndList(t *testing.T) {
	env, c := startEnv(t)
	defer func() { _ = env.Stop() }()

	ns := newNamespace(t, c)
	newTemplate(t, c, ns, "tpl-ret", nil)
	ws := retainWorkspace(t, c, ns, "ws-retain", "tpl-ret")
	pvc := newHomePVC(t, c, ns, ws)

	inv := NewRetentionInventory(c)
	if err := inv.MarkRetained(context.Background(), ws); err != nil {
		t.Fatalf("MarkRetained: %v", err)
	}

	got := getPVC(t, c, ns, pvc.Name)
	if got.Labels[LabelDataRetained] != "true" {
		t.Fatalf("PVC missing %s label: %v", LabelDataRetained, got.Labels)
	}
	if got.Annotations[AnnotationRetainedOwner] != "https://issuer.test|sub-1" {
		t.Fatalf("retained-owner = %q", got.Annotations[AnnotationRetainedOwner])
	}
	if got.Annotations[AnnotationRetainedTenant] != "tenant-a" {
		t.Fatalf("retained-tenant = %q", got.Annotations[AnnotationRetainedTenant])
	}
	if got.Annotations[AnnotationRetainedRuntime] != "LinuxContainer" {
		t.Fatalf("retained-runtime = %q", got.Annotations[AnnotationRetainedRuntime])
	}
	if got.Annotations[AnnotationRetainedAt] == "" {
		t.Fatal("retained-at annotation missing")
	}
	if len(got.OwnerReferences) != 0 {
		t.Fatalf("retained PVC must never gain ownerReferences, got %v", got.OwnerReferences)
	}

	disks, err := inv.List(context.Background(), ns)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(disks) != 1 {
		t.Fatalf("List returned %d disks, want 1", len(disks))
	}
	d := disks[0]
	if d.PVCUID != got.UID || d.WorkspaceUID != ws.UID {
		t.Fatalf("disk identity = (%s,%s), want pvcUID=%s wsUID=%s",
			d.PVCUID, d.WorkspaceUID, got.UID, ws.UID)
	}
	if d.OwnerSubject != "https://issuer.test|sub-1" || d.TenantID != "tenant-a" {
		t.Fatalf("disk owner/tenant = %s/%s", d.OwnerSubject, d.TenantID)
	}
	if d.Runtime != "LinuxContainer" || d.SizeBytes != 20<<30 {
		t.Fatalf("disk runtime/size = %s/%d", d.Runtime, d.SizeBytes)
	}
}

// TestRetentionIdentityIsUID: dataset identity is PVC UID + workspace UID.
// A PVC with the same name in another namespace, or a PVC stamped for a
// different workspace UID, is a different dataset entirely.
func TestRetentionIdentityIsUID(t *testing.T) {
	env, c := startEnv(t)
	defer func() { _ = env.Stop() }()

	ns := newNamespace(t, c)
	otherNS := newNamespace(t, c)
	newTemplate(t, c, ns, "tpl-ret", nil)
	ws := retainWorkspace(t, c, ns, "ws-uid", "tpl-ret")
	pvc := newHomePVC(t, c, ns, ws)

	// Same PVC name in a different namespace: never part of this inventory.
	foreign := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pvc.Name,
			Namespace: otherNS,
			Labels:    map[string]string{linux.LabelWorkspaceUID: "ffffffff-ffff-ffff-ffff-ffffffffffff"},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("20Gi")},
			},
		},
	}
	if err := c.Create(context.Background(), foreign); err != nil {
		t.Fatalf("create foreign pvc: %v", err)
	}

	inv := NewRetentionInventory(c)
	if err := inv.MarkRetained(context.Background(), ws); err != nil {
		t.Fatalf("MarkRetained: %v", err)
	}
	got := getPVC(t, c, otherNS, pvc.Name)
	if got.Labels[LabelDataRetained] == "true" {
		t.Fatal("foreign same-name PVC was stamped retained")
	}
	disks, err := inv.List(context.Background(), ns)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(disks) != 1 || disks[0].PVCUID != pvc.UID {
		t.Fatalf("inventory = %+v, want exactly the workspace PVC by UID", disks)
	}
}

// TestRetentionForeignPVCRefused: a PVC whose workspace-uid label does not
// match the deleting workspace is never stamped into the inventory.
func TestRetentionForeignPVCRefused(t *testing.T) {
	env, c := startEnv(t)
	defer func() { _ = env.Stop() }()

	ns := newNamespace(t, c)
	newTemplate(t, c, ns, "tpl-ret", nil)
	ws := retainWorkspace(t, c, ns, "ws-foreign", "tpl-ret")

	// A PVC at the workspace's home-volume name but owned by a DIFFERENT
	// workspace UID (name reuse after a recreate).
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      linux.PVCName(ws.UID),
			Namespace: ns,
			Labels:    map[string]string{linux.LabelWorkspaceUID: "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee"},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("20Gi")},
			},
		},
	}
	if err := c.Create(context.Background(), pvc); err != nil {
		t.Fatalf("create pvc: %v", err)
	}

	inv := NewRetentionInventory(c)
	err := inv.MarkRetained(context.Background(), ws)
	if !errors.Is(err, ErrForeignPVC) {
		t.Fatalf("MarkRetained on a foreign-owned PVC: err=%v, want ErrForeignPVC", err)
	}
	got := getPVC(t, c, ns, pvc.Name)
	if got.Labels[LabelDataRetained] == "true" {
		t.Fatal("foreign PVC was stamped retained")
	}
}

// TestRetentionOrphans: bidirectional orphan detection — PVCs labelled
// retained but unknown to the API DB are unregistered; record PVC UIDs with
// no volume are missing.
func TestRetentionOrphans(t *testing.T) {
	env, c := startEnv(t)
	defer func() { _ = env.Stop() }()

	ns := newNamespace(t, c)
	newTemplate(t, c, ns, "tpl-ret", nil)
	ws := retainWorkspace(t, c, ns, "ws-orphan", "tpl-ret")
	pvc := newHomePVC(t, c, ns, ws)

	inv := NewRetentionInventory(c)
	if err := inv.MarkRetained(context.Background(), ws); err != nil {
		t.Fatalf("MarkRetained: %v", err)
	}

	// DB knows nothing (simulating API DB loss): the disk is an orphan.
	orphans, err := inv.Unregistered(context.Background(), ns,
		func(context.Context, types.UID) (bool, error) { return false, nil })
	if err != nil {
		t.Fatalf("Unregistered: %v", err)
	}
	if len(orphans) != 1 || orphans[0].PVCUID != pvc.UID {
		t.Fatalf("unregistered = %+v, want the retained PVC", orphans)
	}

	// DB knows the PVC: not an orphan.
	orphans, err = inv.Unregistered(context.Background(), ns,
		func(_ context.Context, uid types.UID) (bool, error) { return uid == pvc.UID, nil })
	if err != nil {
		t.Fatalf("Unregistered: %v", err)
	}
	if len(orphans) != 0 {
		t.Fatalf("known disk reported orphaned: %+v", orphans)
	}

	// A record pointing at a PVC that no longer exists is missing.
	missing, err := inv.MissingVolumes(context.Background(), ns,
		[]types.UID{pvc.UID, "dddddddd-dddd-dddd-dddd-dddddddddddd"})
	if err != nil {
		t.Fatalf("MissingVolumes: %v", err)
	}
	if len(missing) != 1 || missing[0] != "dddddddd-dddd-dddd-dddd-dddddddddddd" {
		t.Fatalf("missing = %v, want the absent UID only", missing)
	}
}

// TestRetentionDecommissionCheck: namespace decommission is blocked while
// retained disks remain; an empty/clean namespace passes.
func TestRetentionDecommissionCheck(t *testing.T) {
	env, c := startEnv(t)
	defer func() { _ = env.Stop() }()

	ns := newNamespace(t, c)
	cleanNS := newNamespace(t, c)
	newTemplate(t, c, ns, "tpl-ret", nil)
	ws := retainWorkspace(t, c, ns, "ws-decomm", "tpl-ret")
	newHomePVC(t, c, ns, ws)

	inv := NewRetentionInventory(c)
	if err := inv.MarkRetained(context.Background(), ws); err != nil {
		t.Fatalf("MarkRetained: %v", err)
	}

	blocking, err := inv.DecommissionCheck(context.Background(), ns)
	if err != nil {
		t.Fatalf("DecommissionCheck: %v", err)
	}
	if len(blocking) != 1 {
		t.Fatalf("decommission check = %d disks, want 1 blocking", len(blocking))
	}

	blocking, err = inv.DecommissionCheck(context.Background(), cleanNS)
	if err != nil {
		t.Fatalf("DecommissionCheck clean ns: %v", err)
	}
	if len(blocking) != 0 {
		t.Fatalf("clean namespace blocked by %+v", blocking)
	}
}

// TestRetentionApplierRetain: the finalizer's retention step marks Retain
// PVCs into the inventory and never deletes them; it is idempotent.
func TestRetentionApplierRetain(t *testing.T) {
	env, c := startEnv(t)
	defer func() { _ = env.Stop() }()

	ns := newNamespace(t, c)
	newTemplate(t, c, ns, "tpl-ret", nil)
	ws := retainWorkspace(t, c, ns, "ws-apply", "tpl-ret")
	pvc := newHomePVC(t, c, ns, ws)

	a := &RetentionApplier{Client: c, Inventory: NewRetentionInventory(c)}
	for i := 0; i < 2; i++ {
		if err := a.ApplyRetention(context.Background(), ws); err != nil {
			t.Fatalf("ApplyRetention run %d: %v", i, err)
		}
	}
	got := getPVC(t, c, ns, pvc.Name)
	if got.Labels[LabelDataRetained] != "true" {
		t.Fatal("ApplyRetention did not mark the PVC retained")
	}
}

// TestRetentionApplierEphemeral: Ephemeral workspaces have their data
// volumes deleted — nothing enters the inventory.
func TestRetentionApplierEphemeral(t *testing.T) {
	env, c := startEnv(t)
	defer func() { _ = env.Stop() }()

	ns := newNamespace(t, c)
	newTemplate(t, c, ns, "tpl-eph", nil)
	ws := newWorkspace(t, c, ns, "ws-eph", "tpl-eph", nil) // default Ephemeral
	pvc := newHomePVC(t, c, ns, ws)

	a := &RetentionApplier{Client: c, Inventory: NewRetentionInventory(c)}
	if err := a.ApplyRetention(context.Background(), ws); err != nil {
		t.Fatalf("ApplyRetention: %v", err)
	}
	err := c.Get(context.Background(), types.NamespacedName{Name: pvc.Name, Namespace: ns}, &corev1.PersistentVolumeClaim{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("ephemeral PVC still present (get err=%v)", err)
	}
	disks, err := NewRetentionInventory(c).List(context.Background(), ns)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(disks) != 0 {
		t.Fatalf("ephemeral delete left inventory entries: %+v", disks)
	}
}

// TestRetentionReconstructedAfterLoss: the inventory is rebuildable from
// PVC metadata alone — a second inventory instance over the same client
// sees the same disks with the same identity (models API DB loss).
func TestRetentionReconstructedAfterLoss(t *testing.T) {
	env, c := startEnv(t)
	defer func() { _ = env.Stop() }()

	ns := newNamespace(t, c)
	newTemplate(t, c, ns, "tpl-ret", nil)
	ws := retainWorkspace(t, c, ns, "ws-recon", "tpl-ret")
	pvc := newHomePVC(t, c, ns, ws)

	if err := NewRetentionInventory(c).MarkRetained(context.Background(), ws); err != nil {
		t.Fatalf("MarkRetained: %v", err)
	}
	// "Post-loss" reader: a fresh inventory, no shared state.
	disks, err := NewRetentionInventory(c).List(context.Background(), ns)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(disks) != 1 || disks[0].PVCUID != pvc.UID || disks[0].WorkspaceUID != ws.UID {
		t.Fatalf("reconstructed inventory = %+v", disks)
	}
}
