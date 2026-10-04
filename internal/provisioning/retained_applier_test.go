// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package provisioning_test

// RetainedApplier delivery is at-least-once like every WorkspaceApplier;
// these tests pin backlog 10 for its cluster + record side effects: a
// second delivery of the same intent revision is a successful no-op.

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	workspacev1alpha1 "github.com/tinyorbitvn/tinycdi/api/v1alpha1"
	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
	"github.com/tinyorbitvn/tinycdi/internal/store"
)

func retainedApplyScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := workspacev1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

// TestRetainedApplierSameRevisionTwice: create-with-retainedDataRef applied
// twice converges to exactly one consuming CR, the retargeted claim labels
// and a single Attaching -> Attached settle; a delete applied twice returns
// the disk to the inventory exactly once.
func TestRetainedApplierSameRevisionTwice(t *testing.T) {
	db := recoveryDB(t)
	ctx := context.Background()
	rstore := provisioning.NewRetainedStore(db)
	svc := provisioning.NewService(db)
	const tenant = "tenant-b10r"
	if err := db.WithTx(ctx, func(tx store.Tx) error {
		return provisioning.SetQuota(ctx, tx, tenant, provisioning.ResourceVector{
			RunningSlots: 10, CPUMillis: 1 << 20, MemoryBytes: 1 << 40, DiskBytes: 1 << 40})
	}); err != nil {
		t.Fatalf("set quota: %v", err)
	}
	seedWorkspaceRow(t, db, "ws_b10rsrc01", tenant, "iss|sub-1", "src-b10r")
	rec, err := rstore.ImportRetained(ctx, provisioning.RetainedDiskInfo{
		TenantID: tenant, Owner: "iss|sub-1", SourceWorkspaceID: "ws_b10rsrc01",
		Runtime: "LinuxContainer", SizeBytes: 2 << 30,
		PVCNamespace: "ns-it", PVCName: "pvc-b10", PVCUID: "uid-pvc-b10",
	})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	ws, err := svc.AttachRetained(ctx, tenant, "iss|sub-1", "iss|sub-1", rec.ID, "k-b10r",
		provisioning.AttachRequest{
			Name:     "consumer-b10r",
			Template: provisioning.TemplateInfo{ID: "tpl_t", Name: "tmpl", Runtime: "LinuxContainer"},
			Vector:   quotaVec, DesiredState: "Stopped",
		}, []byte("a"))
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	pending, err := provisioning.NewOutbox(db).PendingIntents(ctx, provisioning.PlatformID(ws.ID))
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending intents = %v (err %v), want 1 create", pending, err)
	}

	// The retained volume exists in the cluster under the recorded UID.
	pvc := retainedPVC("ns-it", "pvc-b10", "cruid-src", "ws-b10rsrc01", nil)
	kc := fake.NewClientBuilder().WithScheme(retainedApplyScheme(t)).WithObjects(pvc).Build()
	tmap := provisioning.TenantNamespaces{tenant: "ns-it"}
	applier := provisioning.NewRetainedApplier(provisioning.NewK8sApplier(kc, tmap), kc, tmap, rstore)

	create := pending[0]
	if err := applier.Apply(ctx, create); err != nil {
		t.Fatalf("apply create: %v", err)
	}
	// The pipeline acks a delivered intent; mark it so the delete is the
	// only pending row below.
	if err := provisioning.NewOutbox(db).MarkDispatched(ctx, create.WorkspaceUID, create.Revision); err != nil {
		t.Fatalf("ack create: %v", err)
	}
	cr := &workspacev1alpha1.Workspace{}
	if err := kc.Get(ctx, client.ObjectKey{Namespace: "ns-it", Name: provisioning.WorkspaceCRName(create.WorkspaceUID)}, cr); err != nil {
		t.Fatalf("consuming CR: %v", err)
	}
	// Snapshot the side-effect surface the replay must not double: held
	// quota, the retained rows and the events projection (outbox rows).
	usedBefore, err := provisioning.HeldUsage(ctx, db.Pool(), tenant)
	if err != nil {
		t.Fatalf("held usage: %v", err)
	}
	if err := applier.Apply(ctx, create); err != nil {
		t.Fatalf("re-apply create: %v", err)
	}
	usedAfter, err := provisioning.HeldUsage(ctx, db.Pool(), tenant)
	if err != nil {
		t.Fatalf("held usage after replay: %v", err)
	}
	if usedBefore != usedAfter {
		t.Fatalf("replayed apply moved quota %+v -> %+v, want a no-op", usedBefore, usedAfter)
	}
	var nRows, nIntents int
	if err := db.Pool().QueryRow(ctx,
		`SELECT COUNT(*) FROM retained_data WHERE tenant_id = $1`, tenant).Scan(&nRows); err != nil {
		t.Fatalf("retained row count: %v", err)
	}
	if nRows != 1 {
		t.Fatalf("retained_data rows = %d after replayed apply, want 1", nRows)
	}
	if err := db.Pool().QueryRow(ctx,
		`SELECT COUNT(*) FROM outbox_intent WHERE workspace_id = $1`, string(create.WorkspaceUID)).Scan(&nIntents); err != nil {
		t.Fatalf("intent count: %v", err)
	}
	if nIntents != 1 {
		t.Fatalf("outbox_intent rows = %d after replayed apply, want 1", nIntents)
	}

	got, err := rstore.GetRetained(ctx, tenant, rec.ID)
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if got.State != provisioning.RetainedStateAttached || got.TransitionSeq != rec.TransitionSeq+2 {
		t.Fatalf("record after replayed attach = %+v, want Attached seq %d", got, rec.TransitionSeq+2)
	}
	gotPVC := &corev1.PersistentVolumeClaim{}
	if err := kc.Get(ctx, client.ObjectKey{Namespace: "ns-it", Name: "pvc-b10"}, gotPVC); err != nil {
		t.Fatalf("pvc: %v", err)
	}
	if gotPVC.Labels["workspaces.cdi.tinyorbit.vn/workspace-uid"] != string(cr.UID) ||
		gotPVC.Labels["workspaces.cdi.tinyorbit.vn/workspace-name"] != "consumer-b10r" {
		t.Fatalf("pvc labels after replay = %v, want consumer binding", gotPVC.Labels)
	}

	// Delete twice: the first returns the disk to the inventory, the second
	// finds nothing bound to the workspace and is a no-op.
	if _, err := svc.SignalWorkspace(ctx, tenant, "iss|sub-1", "", ws.ID, "",
		provisioning.IntentDelete, nil); err != nil {
		t.Fatalf("signal delete: %v", err)
	}
	pending, err = provisioning.NewOutbox(db).PendingIntents(ctx, provisioning.PlatformID(ws.ID))
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending after delete = %v (err %v), want 1 delete", pending, err)
	}
	del := pending[0]
	if del.Kind != provisioning.IntentDelete {
		t.Fatalf("pending kind = %q, want delete", del.Kind)
	}
	if err := applier.Apply(ctx, del); err != nil {
		t.Fatalf("apply delete: %v", err)
	}
	got, err = rstore.GetRetained(ctx, tenant, rec.ID)
	if err != nil {
		t.Fatalf("record after delete: %v", err)
	}
	if got.State != provisioning.RetainedStateRetained || got.ConsumingWorkspaceID != "" {
		t.Fatalf("record after delete = %+v, want Retained unbound", got)
	}
	// The returned disk's held bytes moved exactly once: the replay must
	// not subtract from the consumer or restore to the source again.
	usedBefore, err = provisioning.HeldUsage(ctx, db.Pool(), tenant)
	if err != nil {
		t.Fatalf("held usage post-delete: %v", err)
	}
	if err := applier.Apply(ctx, del); err != nil {
		t.Fatalf("re-apply delete: %v", err)
	}
	usedAfter, err = provisioning.HeldUsage(ctx, db.Pool(), tenant)
	if err != nil {
		t.Fatalf("held usage after delete replay: %v", err)
	}
	if usedBefore != usedAfter {
		t.Fatalf("replayed delete moved quota %+v -> %+v, want a no-op", usedBefore, usedAfter)
	}
	if err := db.Pool().QueryRow(ctx,
		`SELECT COUNT(*) FROM retained_data WHERE tenant_id = $1`, tenant).Scan(&nRows); err != nil {
		t.Fatalf("retained row count: %v", err)
	}
	if nRows != 1 {
		t.Fatalf("retained_data rows = %d after replayed delete, want 1", nRows)
	}
	got, err = rstore.GetRetained(ctx, tenant, rec.ID)
	if err != nil {
		t.Fatalf("record after delete replay: %v", err)
	}
	if got.State != provisioning.RetainedStateRetained || got.ConsumingWorkspaceID != "" {
		t.Fatalf("record after delete replay = %+v, want Retained unbound", got)
	}
}
