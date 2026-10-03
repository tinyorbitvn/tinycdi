// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

//go:build integration

package integration

// FX-R19: a deleted workspace must leave the workspace list once its runtime
// is proven gone. The delete intent flips the row to state='deleted' with
// phase 'Terminating'; nothing else ever rewrites the phase, so the only
// terminal signal the API has is the released quota reservation (Recovery
// releases it on proven runtime absence). Driven against real PostgreSQL
// and the envtest apiserver, with the production K8sRuntimeObserver and the
// production RetainedSync.

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	workspacesv1alpha1 "github.com/tinyorbitvn/tinycdi/api/v1alpha1"
	"github.com/tinyorbitvn/tinycdi/internal/operator"
	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
	linux "github.com/tinyorbitvn/tinycdi/internal/runtime/linux"
	"github.com/tinyorbitvn/tinycdi/internal/store"
)

type finalizationFixture struct {
	db      *store.DB
	svc     *provisioning.Service
	rec     *provisioning.Recovery
	tenant  string
	ns      string
	owner   string
	applier provisioning.WorkspaceApplier
}

// noopApplier accepts every intent: the CR side is simulated directly in
// envtest (the operator is not running in this test).
type noopApplier struct{}

func (noopApplier) Apply(context.Context, provisioning.Intent) error { return nil }

func newFinalizationFixture(t *testing.T) *finalizationFixture {
	t.Helper()
	db := newDB(t)
	tenant := "tenant-fx-r19"
	ns := newRetainedNamespace(t)
	setQuota(t, db, tenant, provisioning.ResourceVector{
		RunningSlots: 10, CPUMillis: 1 << 30, MemoryBytes: 1 << 40, DiskBytes: 1 << 40})
	obs := provisioning.NewK8sRuntimeObserver(k8sClient, provisioning.TenantNamespaces{tenant: ns})
	return &finalizationFixture{
		db: db, svc: provisioning.NewService(db),
		rec:    provisioning.NewRecovery(db, obs),
		tenant: tenant, ns: ns, owner: "issuer|sub-fx", applier: noopApplier{},
	}
}

func (f *finalizationFixture) create(t *testing.T, name, policy string) provisioning.WorkspaceRecord {
	t.Helper()
	req := provisioning.CreateRequest{
		OwnerIssuer: "issuer", OwnerSubject: "sub-fx", Name: name,
		Template:     provisioning.TemplateInfo{ID: "tpl_linuxdesktop", Name: "linux", Revision: 1, Runtime: "LinuxContainer", Experience: "Desktop"},
		Vector:       provisioning.ResourceVector{RunningSlots: 1, CPUMillis: 500, MemoryBytes: 1 << 30, DiskBytes: 1 << 30},
		DesiredState: "Running", DataPolicy: policy,
	}
	res, err := f.svc.CreateWorkspace(context.Background(), f.tenant, "create-"+name, req, provisioning.RequestHash(req))
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	markDispatched(t, f.db, res.ID)
	return res
}

func (f *finalizationFixture) del(t *testing.T, id string) provisioning.WorkspaceRecord {
	t.Helper()
	res, err := f.svc.SignalWorkspace(context.Background(), f.tenant, f.owner, f.owner, id,
		"delete-"+id, provisioning.IntentDelete, provisioning.RequestHash("delete"))
	if err != nil {
		t.Fatalf("delete %s: %v", id, err)
	}
	return res
}

func (f *finalizationFixture) listed(t *testing.T, id string) bool {
	t.Helper()
	recs, _, err := f.svc.ListWorkspaces(context.Background(), f.tenant, f.owner, "", "", 200)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, r := range recs {
		if r.ID == id {
			return true
		}
	}
	return false
}

// retainedPVC creates the retained-labelled home PVC the operator's
// retention step leaves behind.
func (f *finalizationFixture) retainedPVC(t *testing.T, crName string) *corev1.PersistentVolumeClaim {
	t.Helper()
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pvc-" + crName, Namespace: f.ns,
			Labels: map[string]string{
				linux.LabelWorkspaceName:   crName,
				linux.LabelDataRole:        "home",
				operator.LabelDataRetained: "true",
			},
			Annotations: map[string]string{
				operator.AnnotationRetainedTenant:          f.tenant,
				operator.AnnotationRetainedOwner:           f.owner,
				operator.AnnotationRetainedSourceWorkspace: crName,
				operator.AnnotationRetainedRuntime:         "LinuxContainer",
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
			},
		},
	}
	if err := k8sClient.Create(context.Background(), pvc); err != nil {
		t.Fatalf("pvc: %v", err)
	}
	return pvc
}

func (f *finalizationFixture) held(t *testing.T) provisioning.ResourceVector {
	t.Helper()
	used, err := provisioning.HeldUsage(context.Background(), f.db.Pool(), f.tenant)
	if err != nil {
		t.Fatalf("held usage: %v", err)
	}
	return used
}

func (f *finalizationFixture) recover(t *testing.T) {
	t.Helper()
	if _, err := f.rec.Recover(context.Background(), f.applier); err != nil {
		t.Fatalf("recover: %v", err)
	}
}

// runtimePod simulates a still-terminating workload carrying the workspace's
// labels (the CR is already gone, the pod is not).
func (f *finalizationFixture) runtimePod(t *testing.T, wsID string) *corev1.Pod {
	t.Helper()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pod-" + provisioning.WorkspaceCRName(provisioning.PlatformID(wsID)), Namespace: f.ns,
			Labels: map[string]string{linux.LabelWorkspaceName: provisioning.WorkspaceCRName(provisioning.PlatformID(wsID))},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "busybox"}}},
	}
	if err := k8sClient.Create(context.Background(), pod); err != nil {
		t.Fatalf("pod: %v", err)
	}
	return pod
}

// TestDeleteFinalization_EphemeralLeavesList: create -> delete -> the CR is
// gone -> the workspace leaves the list and GET answers not-found within one
// recovery pass. While a runtime pod lingers it stays visible as
// Terminating (honest "Deleting" progress), and DELETE stays idempotent.
func TestDeleteFinalization_EphemeralLeavesList(t *testing.T) {
	f := newFinalizationFixture(t)
	ctx := context.Background()

	before := f.held(t)
	ws := f.create(t, "fx-ephemeral", "Ephemeral")
	pod := f.runtimePod(t, ws.ID)
	if during := f.held(t); during == before {
		t.Fatalf("create did not reserve quota (held %+v)", during)
	}

	got := f.del(t, ws.ID)
	if got.Phase != "Terminating" {
		t.Fatalf("delete response phase=%q, want Terminating", got.Phase)
	}
	// A workspace being deleted must not advertise desiredState=Running.
	if got.DesiredState != "Stopped" {
		t.Fatalf("delete response desiredState=%q, want Stopped", got.DesiredState)
	}
	f.recover(t)
	if !f.listed(t, ws.ID) {
		t.Fatal("workspace left the list while its runtime pod still exists")
	}
	if _, err := f.svc.GetWorkspace(ctx, f.tenant, f.owner, ws.ID); err != nil {
		t.Fatalf("get while runtime lingers: %v, want the Terminating record", err)
	}

	// The runtime disappears.
	if err := k8sClient.Delete(ctx, pod, client.GracePeriodSeconds(0)); err != nil {
		t.Fatalf("delete pod: %v", err)
	}
	f.recover(t)

	if f.listed(t, ws.ID) {
		t.Fatal("deleted workspace is still listed after its runtime is gone (FX-R19)")
	}
	// Delete policy: nothing stays reserved, so usage is back to the value
	// before the create and repeated deletes can never exhaust the quota.
	if after := f.held(t); after != before {
		t.Fatalf("held quota after delete = %+v, want the pre-create value %+v", after, before)
	}
	if _, err := f.svc.GetWorkspace(ctx, f.tenant, f.owner, ws.ID); !errors.Is(err, provisioning.ErrWorkspaceNotFound) {
		t.Fatalf("get after finalisation: %v, want ErrWorkspaceNotFound", err)
	}
	// A repeated DELETE (client retry, new key) still answers, never 5xx.
	if _, err := f.svc.SignalWorkspace(ctx, f.tenant, f.owner, f.owner, ws.ID,
		"delete-again-1", provisioning.IntentDelete, provisioning.RequestHash("delete")); err != nil {
		t.Fatalf("repeat delete: %v", err)
	}
	// Other workspaces are untouched.
	other := f.create(t, "fx-other", "Ephemeral")
	f.recover(t)
	if !f.listed(t, other.ID) {
		t.Fatal("live workspace vanished from the list")
	}
}

// TestDeleteFinalization_RetainKeepsDataRecord: a Retain workspace leaves the
// list once its runtime is gone, its retained PVC survives, and the
// retained-data record exists (RetainedSync imports it from the PVC).
func TestDeleteFinalization_RetainKeepsDataRecord(t *testing.T) {
	for _, order := range []string{"sync-first", "recover-first"} {
		t.Run(order, func(t *testing.T) { retainKeepsDataRecord(t, order) })
	}
}

func retainKeepsDataRecord(t *testing.T, order string) {
	f := newFinalizationFixture(t)
	ctx := context.Background()

	ws := f.create(t, "fx-retain", "Retain")
	crName := provisioning.WorkspaceCRName(provisioning.PlatformID(ws.ID))
	pvc := f.retainedPVC(t, crName)

	retained := provisioning.NewRetainedStore(f.db)
	syncer := provisioning.NewRetainedSync(k8sClient, provisioning.TenantNamespaces{f.tenant: f.ns}, retained, nil, nil)
	diskOnly := provisioning.ResourceVector{DiskBytes: 1 << 30}

	f.del(t, ws.ID)
	if order == "sync-first" {
		// The retained record lands while the reservation is still held
		// (ImportRetained finds nothing to restore).
		if res, err := syncer.SyncOnce(ctx); err != nil || res.Imported != 1 {
			t.Fatalf("retained sync: imported=%d err=%v, want 1", res.Imported, err)
		}
	}
	f.recover(t) // no CR, no pods, only the retained PVC: runtime is gone
	if order == "recover-first" {
		if res, err := syncer.SyncOnce(ctx); err != nil || res.Imported != 1 {
			t.Fatalf("retained sync: imported=%d err=%v, want 1", res.Imported, err)
		}
	}
	// Further recovery passes and sweeps must be stable (no churn).
	f.recover(t)
	if _, err := syncer.SyncOnce(ctx); err != nil {
		t.Fatalf("resync: %v", err)
	}
	f.recover(t)

	if f.listed(t, ws.ID) {
		t.Fatal("deleted Retain workspace is still listed after its runtime is gone (FX-R19)")
	}
	got := &corev1.PersistentVolumeClaim{}
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(pvc), got); err != nil {
		t.Fatalf("retained PVC must survive: %v", err)
	}
	var n int
	if err := f.db.Pool().QueryRow(ctx, `
		SELECT count(*) FROM retained_data
		WHERE source_workspace_id = $1 AND state = 'Retained' AND pvc_uid = $2`,
		ws.ID, string(got.UID)).Scan(&n); err != nil || n != 1 {
		t.Fatalf("retained_data records for %s = %d (err %v), want 1 in state Retained", ws.ID, n, err)
	}
	// Retain policy: compute is released, only the retained disk stays reserved.
	if after := f.held(t); after != diskOnly {
		t.Fatalf("held quota = %+v, want only the retained disk %+v", after, diskOnly)
	}
}

// TestDeleteFinalization_StuckRowSelfHeals: rows stranded by the original
// bug (Retain workspace whose whole reservation was released before the
// retained record existed, so the retained disk is not counted) are repaired
// by a plain recovery pass: no manual database action.
func TestDeleteFinalization_StuckRowSelfHeals(t *testing.T) {
	f := newFinalizationFixture(t)
	ctx := context.Background()
	ws := f.create(t, "fx-stuck", "Retain")
	crName := provisioning.WorkspaceCRName(provisioning.PlatformID(ws.ID))
	f.retainedPVC(t, crName)
	f.del(t, ws.ID)
	syncer := provisioning.NewRetainedSync(k8sClient, provisioning.TenantNamespaces{f.tenant: f.ns},
		provisioning.NewRetainedStore(f.db), nil, nil)
	f.recover(t)
	if _, err := syncer.SyncOnce(ctx); err != nil {
		t.Fatalf("sync: %v", err)
	}
	// Recreate the stuck state seen on the cluster: whole reservation
	// released, retained record present, disk not counted, phase Terminating.
	if _, err := f.db.Pool().Exec(ctx, `
		UPDATE quota_reservation SET state = 'released', released_at = now(),
			release_proof = 'runtime_absent_observed', running_slots = 1, cpu_millis = 500,
			memory_bytes = 1073741824, disk_bytes = 1073741824
		WHERE workspace_id = $1`, ws.ID); err != nil {
		t.Fatalf("seed stuck reservation: %v", err)
	}
	if f.listed(t, ws.ID) {
		t.Fatal("stuck row is visible")
	}
	f.recover(t)
	if got, want := f.held(t), (provisioning.ResourceVector{DiskBytes: 1 << 30}); got != want {
		t.Fatalf("held after repair = %+v, want retained disk only %+v", got, want)
	}
}

// TestDeleteFinalization_CRVanishesUnderReporter: the Workspace CR is
// deleted out from under the control plane — it carries a finalizer, a pod
// still runs, and the CR disappears (finalizer stripped, as the operator
// does) in between two recovery passes. The row stays "Deleting" while any
// compute exists and is finalised on the first pass after the CR and its
// children are gone; the observer's "not found" for the vanished CR is
// proof of absence, not an error.
func TestDeleteFinalization_CRVanishesUnderReporter(t *testing.T) {
	f := newFinalizationFixture(t)
	ctx := context.Background()
	ws := f.create(t, "fx-crgone", "Ephemeral")
	crName := provisioning.WorkspaceCRName(provisioning.PlatformID(ws.ID))

	cr := newRetainedWorkspace(t, f.ns, crName, workspacesv1alpha1.DataPolicyEphemeral)
	cr.Finalizers = []string{operator.FinalizerRuntimeCleanup}
	cr.Labels = map[string]string{provisioning.LabelWorkspaceUID: ws.ID, provisioning.LabelTenant: f.tenant}
	if err := k8sClient.Update(ctx, cr); err != nil {
		t.Fatalf("finalizer: %v", err)
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pod-" + crName, Namespace: f.ns,
			Labels: map[string]string{linux.LabelWorkspaceUID: string(cr.UID), linux.LabelWorkspaceName: crName}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "busybox"}}},
	}
	if err := k8sClient.Create(ctx, pod); err != nil {
		t.Fatalf("pod: %v", err)
	}

	f.del(t, ws.ID)
	if err := k8sClient.Delete(ctx, cr); err != nil { // Terminating, finalizer holds it
		t.Fatalf("delete CR: %v", err)
	}
	f.recover(t)
	if !f.listed(t, ws.ID) {
		t.Fatal("row left the list while the CR and its pod still exist")
	}

	// Teardown finishes: children go, then the finalizer is removed and the
	// CR vanishes before any further report reaches the backend.
	if err := k8sClient.Delete(ctx, pod, client.GracePeriodSeconds(0)); err != nil {
		t.Fatalf("delete pod: %v", err)
	}
	cur := &workspacesv1alpha1.Workspace{}
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(cr), cur); err != nil {
		t.Fatalf("get terminating CR: %v", err)
	}
	cur.Finalizers = nil
	if err := k8sClient.Update(ctx, cur); err != nil {
		t.Fatalf("strip finalizer: %v", err)
	}
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(cr), cur); !apierrors.IsNotFound(err) {
		t.Fatalf("CR should be gone, got %v", err)
	}
	f.recover(t)
	if f.listed(t, ws.ID) {
		t.Fatal("row still listed after the CR vanished (FX-R19)")
	}
	if got := f.held(t); got != (provisioning.ResourceVector{}) {
		t.Fatalf("held quota after CR vanished = %+v, want zero", got)
	}
}
