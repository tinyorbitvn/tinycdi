//go:build integration

package integration

// Contract tests — retained data (design §5). Driven
// against real PostgreSQL (the API-side RetainedDataStore transaction
// contract) and envtest (the operator PVC inventory). Every store method is
// a compile-only stub when written, so tests fail on
// "api: retained data not implemented" / "operator: not implemented" only.
//
// Asserted contract:
//   - migration 007 applies cleanly and enforces the state enum;
//   - Delete+Retain lands the disk in the inventory (PVC metadata -> record);
//   - attach and purge exclude each other atomically — a race has exactly
//     one winner; a disk has at most one consumer;
//   - unauthorized (foreign-owner) attach/purge fail;
//   - purge/attach retries are idempotent;
//   - disk quota stays held until the volume is actually deleted
//     (CompletePurge), and attach moves it to the new workspace without
//     double counting;
//   - attach without headroom is a coded quota error;
//   - a workspace deleted mid-attach returns the disk to Retained;
//   - orphan detection is bidirectional (PVC without record, record
//     without PVC); namespace decommission cannot skip the inventory.
//
// Run: go test -tags=integration ./tests/integration -run TestRetention -v

import (
	"context"
	"errors"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	workspacesv1alpha1 "github.com/tinyorbitvn/tinycdi/api/v1alpha1"
	"github.com/tinyorbitvn/tinycdi/internal/api"
	"github.com/tinyorbitvn/tinycdi/internal/operator"
	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
	linux "github.com/tinyorbitvn/tinycdi/internal/runtime/linux"
)

const retOwner = "https://issuer.test|sub-1"

func retainedInfo(tenant, pvcNS, pvcName, pvcUID, srcWS, srcName, runtime string, size int64) api.RetainedDiskInfo {
	return api.RetainedDiskInfo{
		PVCNamespace: pvcNS, PVCName: pvcName, PVCUID: pvcUID,
		TenantID: tenant, Owner: retOwner,
		SourceWorkspaceID: srcWS, SourceWorkspaceName: srcName,
		Runtime: runtime, SizeBytes: size,
	}
}

func attachReq() api.AttachRequest {
	return api.AttachRequest{
		Name: "restored-desktop",
		Template: provisioning.TemplateInfo{
			ID: "tpl_linuxdesktop", Name: "linuxdesktop", Revision: 1,
			Runtime: "LinuxContainer", Experience: "Desktop",
		},
		Vector: provisioning.ResourceVector{
			RunningSlots: 1, CPUMillis: 2000, MemoryBytes: 4 << 30, DiskBytes: 20 << 30,
		},
		DesiredState: "Running",
	}
}

func newRetainedNamespace(t *testing.T) string {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "it-ret-"}}
	if err := k8sClient.Create(context.Background(), ns); err != nil {
		t.Fatalf("namespace: %v", err)
	}
	return ns.Name
}

func newRetainedPVC(t *testing.T, ns string, wsUID types.UID, wsName string, retained bool) *corev1.PersistentVolumeClaim {
	t.Helper()
	labels := map[string]string{
		linux.LabelWorkspaceUID:  string(wsUID),
		linux.LabelWorkspaceName: wsName,
		linux.LabelDataRole:      "home",
	}
	if retained {
		labels[operator.LabelDataRetained] = "true"
	}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      linux.PVCName(wsUID),
			Namespace: ns,
			Labels:    labels,
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("20Gi")},
			},
		},
	}
	if retained {
		pvc.Annotations = map[string]string{
			operator.AnnotationRetainedTenant:          "tenant-it",
			operator.AnnotationRetainedOwner:           retOwner,
			operator.AnnotationRetainedSourceWorkspace: wsName,
			operator.AnnotationRetainedRuntime:         "LinuxContainer",
		}
	}
	if err := k8sClient.Create(context.Background(), pvc); err != nil {
		t.Fatalf("create pvc: %v", err)
	}
	return pvc
}

func newRetainedWorkspace(t *testing.T, ns, name string, policy workspacesv1alpha1.DataPolicy) *workspacesv1alpha1.Workspace {
	t.Helper()
	ws := &workspacesv1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: workspacesv1alpha1.WorkspaceSpec{
			TemplateRef:       workspacesv1alpha1.TemplateReference{Name: "tpl"},
			OwnerSubject:      workspacesv1alpha1.OwnerSubject{Issuer: "https://issuer.test", Subject: "sub-1"},
			DesiredState:      workspacesv1alpha1.DesiredStateStopped,
			DataPolicy:        policy,
			RuntimeGeneration: 1,
			IntentRevision:    1,
		},
	}
	if err := k8sClient.Create(context.Background(), ws); err != nil {
		t.Fatalf("workspace CR: %v", err)
	}
	return ws
}

// TestRetention_MigrationSchema: 007 applies cleanly after 001–006 and the
// state enum is enforced at the schema level.
func TestRetention_MigrationSchema(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()

	var exists bool
	if err := db.Pool().QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM information_schema.tables
			WHERE table_name = 'retained_data')`).Scan(&exists); err != nil || !exists {
		t.Fatalf("retained_data table missing after migrate (exists=%v err=%v)", exists, err)
	}

	// The state machine enum rejects illegal states.
	_, err := db.Pool().Exec(ctx, `
		INSERT INTO retained_data (id, tenant_id, owner_subject, state,
			pvc_namespace, pvc_name, pvc_uid, source_workspace_id, runtime, size_bytes)
		VALUES ('rd_schema01', 'tenant-it', 'o', 'Bogus', 'ns', 'pvc', 'uid', 'ws', 'LinuxContainer', 1)`)
	if err == nil {
		t.Fatal("state CHECK accepted an illegal retained-data state")
	}
}

// TestRetention_DeleteRetainToInventory: the Delete+Retain pipeline — the
// finalizer's retention step marks the workspace PVC; the inventory reports
// it from PVC metadata; the API-side import records it for /v1/data.
func TestRetention_DeleteRetainToInventory(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	ns := newRetainedNamespace(t)

	ws := newRetainedWorkspace(t, ns, "ws-retain", workspacesv1alpha1.DataPolicyRetain)
	pvc := newRetainedPVC(t, ns, ws.UID, ws.Name, false)

	inv := operator.NewRetentionInventory(k8sClient)
	applier := &operator.RetentionApplier{Client: k8sClient, Inventory: inv}
	if err := applier.ApplyRetention(ctx, ws); err != nil {
		t.Fatalf("ApplyRetention: %v", err)
	}

	got := &corev1.PersistentVolumeClaim{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: pvc.Name, Namespace: ns}, got); err != nil {
		t.Fatalf("retained PVC must survive workspace teardown: %v", err)
	}
	if got.Labels[operator.LabelDataRetained] != "true" {
		t.Fatalf("PVC not stamped retained: %v", got.Labels)
	}

	disks, err := inv.List(ctx, ns)
	if err != nil || len(disks) != 1 {
		t.Fatalf("inventory List = %d disks, err=%v; want 1", len(disks), err)
	}
	if disks[0].PVCUID != got.UID {
		t.Fatalf("inventory disk identity %s != PVC UID %s", disks[0].PVCUID, got.UID)
	}

	rstore := api.NewRetainedStore(db)
	rec, err := rstore.ImportRetained(ctx, api.RetainedDiskInfo{
		PVCNamespace: ns, PVCName: got.Name, PVCUID: string(got.UID),
		TenantID: "tenant-it", Owner: retOwner,
		SourceWorkspaceID: string(ws.UID), SourceWorkspaceName: ws.Name,
		Runtime: disks[0].Runtime, SizeBytes: disks[0].SizeBytes,
	})
	if err != nil {
		t.Fatalf("ImportRetained: %v", err)
	}
	recs, _, err := rstore.ListRetained(ctx, "tenant-it", retOwner, retOwner, "", 10)
	if err != nil {
		t.Fatalf("ListRetained: %v", err)
	}
	if len(recs) != 1 || recs[0].ID != rec.ID || recs[0].State != api.RetainedStateRetained {
		t.Fatalf("retained list = %+v", recs)
	}
}

// TestRetention_AttachPurgeRace: attach-in-flight vs purge — exactly one
// side wins; run both directions plus a concurrent race.
func TestRetention_AttachPurgeRace(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	rstore := api.NewRetainedStore(db)
	tenant := "tenant-race"
	setQuota(t, db, tenant, provisioning.ResourceVector{
		RunningSlots: 100, CPUMillis: 1 << 30, MemoryBytes: 1 << 40, DiskBytes: 1 << 40,
	})

	purgeNonce := func(t *testing.T, id string) string {
		recs, _, err := rstore.ListRetained(ctx, tenant, retOwner, retOwner, "", 10)
		if err != nil {
			t.Fatalf("list for nonce: %v", err)
		}
		for _, r := range recs {
			if r.ID == id {
				return r.PurgeNonce
			}
		}
		t.Fatalf("record %s not listed", id)
		return ""
	}

	// Sequential, both directions.
	t.Run("attachThenPurge", func(t *testing.T) {
		rec, err := rstore.ImportRetained(ctx, retainedInfo(tenant, "ns", "pvc-atp", "uid-atp", "ws-atp", "src", "LinuxContainer", 20<<30))
		if err != nil {
			t.Fatalf("import: %v", err)
		}
		if _, err := rstore.AttachRetained(ctx, tenant, retOwner, retOwner, rec.ID, "k-atp-a", attachReq(), []byte("a")); err != nil {
			t.Fatalf("attach: %v", err)
		}
		if _, err := rstore.PurgeRetained(ctx, tenant, retOwner, retOwner, rec.ID, purgeNonce(t, rec.ID), "", nil); !errors.Is(err, api.ErrRetainedState) {
			t.Fatalf("purge during attach: err=%v, want ErrRetainedState", err)
		}
	})
	t.Run("purgeThenAttach", func(t *testing.T) {
		rec, err := rstore.ImportRetained(ctx, retainedInfo(tenant, "ns", "pvc-pta", "uid-pta", "ws-pta", "src", "LinuxContainer", 20<<30))
		if err != nil {
			t.Fatalf("import: %v", err)
		}
		if _, err := rstore.PurgeRetained(ctx, tenant, retOwner, retOwner, rec.ID, purgeNonce(t, rec.ID), "", nil); err != nil {
			t.Fatalf("purge: %v", err)
		}
		if _, err := rstore.AttachRetained(ctx, tenant, retOwner, retOwner, rec.ID, "k-pta-a", attachReq(), []byte("a")); !errors.Is(err, api.ErrRetainedState) {
			t.Fatalf("attach during purge: err=%v, want ErrRetainedState", err)
		}
	})

	// Concurrent race: exactly one winner, record lands in exactly one
	// successor state.
	t.Run("concurrent", func(t *testing.T) {
		rec, err := rstore.ImportRetained(ctx, retainedInfo(tenant, "ns", "pvc-race", "uid-race", "ws-race", "src", "LinuxContainer", 20<<30))
		if err != nil {
			t.Fatalf("import: %v", err)
		}
		nonce := purgeNonce(t, rec.ID)
		var wg sync.WaitGroup
		var attachErr, purgeErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, attachErr = rstore.AttachRetained(ctx, tenant, retOwner, retOwner, rec.ID, "k-race-a", attachReq(), []byte("a"))
		}()
		go func() {
			defer wg.Done()
			_, purgeErr = rstore.PurgeRetained(ctx, tenant, retOwner, retOwner, rec.ID, nonce, "", nil)
		}()
		wg.Wait()
		winners := 0
		if attachErr == nil {
			winners++
		}
		if purgeErr == nil {
			winners++
		}
		if winners != 1 {
			t.Fatalf("race produced %d winners (attach=%v purge=%v), want exactly 1", winners, attachErr, purgeErr)
		}
		recs, _, err := rstore.ListRetained(ctx, tenant, retOwner, "", "", 50)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		var final api.RetainedRecord
		for _, r := range recs {
			if r.ID == rec.ID {
				final = r
			}
		}
		if final.State != api.RetainedStateAttaching && final.State != api.RetainedStatePurging {
			t.Fatalf("final state = %q, want Attaching xor Purging", final.State)
		}
		if attachErr == nil && final.State != api.RetainedStateAttaching {
			t.Fatalf("attach won but state=%s", final.State)
		}
		if purgeErr == nil && final.State != api.RetainedStatePurging {
			t.Fatalf("purge won but state=%s", final.State)
		}
	})
}

// TestRetention_DoubleAttach: a disk has at most one consumer — a second
// attach (different key, same record) is rejected atomically.
func TestRetention_DoubleAttach(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	rstore := api.NewRetainedStore(db)
	tenant := "tenant-dbl"
	setQuota(t, db, tenant, provisioning.ResourceVector{
		RunningSlots: 100, CPUMillis: 1 << 30, MemoryBytes: 1 << 40, DiskBytes: 1 << 40,
	})

	rec, err := rstore.ImportRetained(ctx, retainedInfo(tenant, "ns", "pvc-dbl", "uid-dbl", "ws-dbl", "src", "LinuxContainer", 20<<30))
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if _, err := rstore.AttachRetained(ctx, tenant, retOwner, retOwner, rec.ID, "k-dbl-1", attachReq(), []byte("a")); err != nil {
		t.Fatalf("first attach: %v", err)
	}
	if _, err := rstore.AttachRetained(ctx, tenant, retOwner, retOwner, rec.ID, "k-dbl-2", attachReq(), []byte("b")); !errors.Is(err, api.ErrRetainedState) {
		t.Fatalf("second attach: err=%v, want ErrRetainedState", err)
	}

	// Concurrent variant for good measure on a fresh record. The winner's
	// new workspace needs a name free of the earlier attach's (owner,name)
	// uniqueness — a different name, same disk.
	rec2, err := rstore.ImportRetained(ctx, retainedInfo(tenant, "ns", "pvc-dbl2", "uid-dbl2", "ws-dbl2", "src", "LinuxContainer", 20<<30))
	if err != nil {
		t.Fatalf("import2: %v", err)
	}
	req2 := attachReq()
	req2.Name = "restored-desktop-2"
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = rstore.AttachRetained(ctx, tenant, retOwner, retOwner, rec2.ID, "k-dblc-"+string(rune('a'+i)), req2, []byte{byte('a' + i)})
		}(i)
	}
	wg.Wait()
	ok := 0
	for _, e := range errs {
		if e == nil {
			ok++
		}
	}
	if ok != 1 {
		t.Fatalf("concurrent double attach: %d succeeded, want 1", ok)
	}
}

// TestRetention_IdempotentRetries: replayed attach (same Idempotency-Key +
// body) returns the same workspace and never mints a second; repeated purge
// is a no-op.
func TestRetention_IdempotentRetries(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	rstore := api.NewRetainedStore(db)
	tenant := "tenant-idem"
	setQuota(t, db, tenant, provisioning.ResourceVector{
		RunningSlots: 100, CPUMillis: 1 << 30, MemoryBytes: 1 << 40, DiskBytes: 1 << 40,
	})

	rec, err := rstore.ImportRetained(ctx, retainedInfo(tenant, "ns", "pvc-idem", "uid-idem", "ws-idem", "src", "LinuxContainer", 20<<30))
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	ws1, err := rstore.AttachRetained(ctx, tenant, retOwner, retOwner, rec.ID, "k-idem-a", attachReq(), []byte("same-body"))
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	ws2, err := rstore.AttachRetained(ctx, tenant, retOwner, retOwner, rec.ID, "k-idem-a", attachReq(), []byte("same-body"))
	if err != nil {
		t.Fatalf("replay attach: %v", err)
	}
	if ws1.ID != ws2.ID {
		t.Fatalf("idempotent attach minted %s then %s", ws1.ID, ws2.ID)
	}
	var count int
	if err := db.Pool().QueryRow(ctx,
		`SELECT count(*) FROM workspaces WHERE retained_data_ref = $1`, rec.ID).Scan(&count); err != nil {
		t.Fatalf("count workspaces: %v", err)
	}
	if count != 1 {
		t.Fatalf("attach replay created %d workspaces, want 1", count)
	}

	// Purge retries.
	rec2, err := rstore.ImportRetained(ctx, retainedInfo(tenant, "ns", "pvc-idem2", "uid-idem2", "ws-idem2", "src", "LinuxContainer", 20<<30))
	if err != nil {
		t.Fatalf("import2: %v", err)
	}
	recs, _, _ := rstore.ListRetained(ctx, tenant, retOwner, retOwner, "", 10)
	var nonce string
	for _, r := range recs {
		if r.ID == rec2.ID {
			nonce = r.PurgeNonce
		}
	}
	p1, err := rstore.PurgeRetained(ctx, tenant, retOwner, retOwner, rec2.ID, nonce, "k-idem-p", nil)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	// Fresh nonce, retry: naturally idempotent.
	recs, _, _ = rstore.ListRetained(ctx, tenant, retOwner, retOwner, "", 10)
	for _, r := range recs {
		if r.ID == rec2.ID {
			nonce = r.PurgeNonce
		}
	}
	p2, err := rstore.PurgeRetained(ctx, tenant, retOwner, retOwner, rec2.ID, nonce, "k-idem-p2", nil)
	if err != nil {
		t.Fatalf("retry purge: %v", err)
	}
	if p1.State != api.RetainedStatePurging || p2.State != api.RetainedStatePurging {
		t.Fatalf("purge states = %s/%s, want Purging", p1.State, p2.State)
	}
}

// TestRetention_Unauthorized: attach and purge by a caller who is not the
// record owner fail — foreign records are indistinguishable from missing.
func TestRetention_Unauthorized(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	rstore := api.NewRetainedStore(db)
	tenant := "tenant-unauth"
	setQuota(t, db, tenant, provisioning.ResourceVector{
		RunningSlots: 100, CPUMillis: 1 << 30, MemoryBytes: 1 << 40, DiskBytes: 1 << 40,
	})
	const other = "https://issuer.test|mallory"

	rec, err := rstore.ImportRetained(ctx, retainedInfo(tenant, "ns", "pvc-un", "uid-un", "ws-un", "src", "LinuxContainer", 20<<30))
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if _, err := rstore.AttachRetained(ctx, tenant, other, other, rec.ID, "k-un-a", attachReq(), nil); !errors.Is(err, api.ErrRetainedNotFound) {
		t.Fatalf("foreign attach: err=%v, want ErrRetainedNotFound", err)
	}
	if _, err := rstore.PurgeRetained(ctx, tenant, other, other, rec.ID, "whatever-nonce", "", nil); !errors.Is(err, api.ErrRetainedNotFound) {
		t.Fatalf("foreign purge: err=%v, want ErrRetainedNotFound", err)
	}
	// The owner's own record is untouched.
	recs, _, err := rstore.ListRetained(ctx, tenant, retOwner, retOwner, "", 10)
	if err != nil || len(recs) != 1 || recs[0].State != api.RetainedStateRetained {
		t.Fatalf("record after foreign attempts = %+v err=%v", recs, err)
	}
}

// TestRetention_QuotaHeldUntilDeletion: the retained disk's storage stays
// counted while Purging and is released only when the volume is actually
// gone (CompletePurge).
func TestRetention_QuotaHeldUntilDeletion(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	rstore := api.NewRetainedStore(db)
	tenant := "tenant-quota-hold"
	setQuota(t, db, tenant, provisioning.ResourceVector{
		RunningSlots: 10, CPUMillis: 1 << 30, MemoryBytes: 1 << 40, DiskBytes: 100 << 30,
	})

	// Source workspace held 20Gi of disk; delete+retain keeps it held.
	svc := provisioning.NewService(db)
	src, err := svc.CreateWorkspace(ctx, tenant, "key-src", provisioning.CreateRequest{
		OwnerIssuer: "https://issuer.test", OwnerSubject: "sub-1",
		Name:       "src-ws",
		Template:   provisioning.TemplateInfo{ID: "tpl_linuxdesktop", Name: "linuxdesktop", Revision: 1, Runtime: "LinuxContainer"},
		Vector:     provisioning.ResourceVector{RunningSlots: 1, CPUMillis: 2000, MemoryBytes: 4 << 30, DiskBytes: 20 << 30},
		DataPolicy: "Retain",
	}, []byte("create-src"))
	if err != nil {
		t.Fatalf("create source ws: %v", err)
	}

	rec, err := rstore.ImportRetained(ctx, retainedInfo(tenant, "ns", "pvc-q", "uid-q", src.ID, "src-ws", "LinuxContainer", 20<<30))
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	used, err := provisioning.HeldUsage(ctx, db.Pool(), tenant)
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	if used.DiskBytes < 20<<30 {
		t.Fatalf("retained disk not counted: held disk=%d", used.DiskBytes)
	}

	recs, _, _ := rstore.ListRetained(ctx, tenant, retOwner, retOwner, "", 10)
	var nonce string
	for _, r := range recs {
		if r.ID == rec.ID {
			nonce = r.PurgeNonce
		}
	}
	if _, err := rstore.PurgeRetained(ctx, tenant, retOwner, retOwner, rec.ID, nonce, "", nil); err != nil {
		t.Fatalf("purge: %v", err)
	}
	// Purging: still counted — the volume is not gone yet.
	used, _ = provisioning.HeldUsage(ctx, db.Pool(), tenant)
	if used.DiskBytes < 20<<30 {
		t.Fatalf("disk quota released while Purging: held=%d", used.DiskBytes)
	}
	// Actual deletion completes -> released.
	if err := rstore.CompletePurge(ctx, rec.ID); err != nil {
		t.Fatalf("CompletePurge: %v", err)
	}
	used, _ = provisioning.HeldUsage(ctx, db.Pool(), tenant)
	if used.DiskBytes != 0 {
		t.Fatalf("disk quota still held after purge completion: %d", used.DiskBytes)
	}
}

// TestRetention_AttachMovesQuota: attaching a retained disk moves its held
// disk reservation to the new workspace — counted once, not twice.
func TestRetention_AttachMovesQuota(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	rstore := api.NewRetainedStore(db)
	tenant := "tenant-quota-move"
	setQuota(t, db, tenant, provisioning.ResourceVector{
		RunningSlots: 10, CPUMillis: 1 << 30, MemoryBytes: 1 << 40, DiskBytes: 100 << 30,
	})

	svc := provisioning.NewService(db)
	src, err := svc.CreateWorkspace(ctx, tenant, "key-src", provisioning.CreateRequest{
		OwnerIssuer: "https://issuer.test", OwnerSubject: "sub-1",
		Name:       "src-ws",
		Template:   provisioning.TemplateInfo{ID: "tpl_linuxdesktop", Name: "linuxdesktop", Revision: 1, Runtime: "LinuxContainer"},
		Vector:     provisioning.ResourceVector{RunningSlots: 1, CPUMillis: 2000, MemoryBytes: 4 << 30, DiskBytes: 20 << 30},
		DataPolicy: "Retain",
	}, []byte("create-src"))
	if err != nil {
		t.Fatalf("create source ws: %v", err)
	}
	rec, err := rstore.ImportRetained(ctx, retainedInfo(tenant, "ns", "pvc-qm", "uid-qm", src.ID, "src-ws", "LinuxContainer", 20<<30))
	if err != nil {
		t.Fatalf("import: %v", err)
	}

	ws, err := rstore.AttachRetained(ctx, tenant, retOwner, retOwner, rec.ID, "k-qm", attachReq(), []byte("a"))
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	used, err := provisioning.HeldUsage(ctx, db.Pool(), tenant)
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	// Disk counted exactly once (20Gi moved, not duplicated); compute is the
	// consuming workspace's.
	if used.DiskBytes != 20<<30 {
		t.Fatalf("held disk after attach = %d, want exactly 20Gi (no double count)", used.DiskBytes)
	}
	var rows int
	if err := db.Pool().QueryRow(ctx, `
		SELECT count(*) FROM quota_reservation
		WHERE tenant_id = $1 AND state = 'held' AND disk_bytes > 0`, tenant).Scan(&rows); err != nil {
		t.Fatalf("reservation query: %v", err)
	}
	if rows != 1 {
		t.Fatalf("%d held reservations carry disk bytes, want 1", rows)
	}
	_ = ws
}

// TestRetention_AttachNoHeadroom: attach against a tenant without compute
// headroom (or without a quota row at all) fails with a coded quota error.
func TestRetention_AttachNoHeadroom(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	rstore := api.NewRetainedStore(db)
	tenant := "tenant-full"
	setQuota(t, db, tenant, provisioning.ResourceVector{
		RunningSlots: 0, CPUMillis: 0, MemoryBytes: 0, DiskBytes: 100 << 30,
	})

	rec, err := rstore.ImportRetained(ctx, retainedInfo(tenant, "ns", "pvc-full", "uid-full", "ws-full", "src", "LinuxContainer", 20<<30))
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	_, err = rstore.AttachRetained(ctx, tenant, retOwner, retOwner, rec.ID, "k-full", attachReq(), nil)
	if !provisioning.IsQuotaExceeded(err) {
		t.Fatalf("attach over quota: err=%v, want QuotaExceededError", err)
	}

	// No quota row at all -> fail closed.
	_, err = rstore.AttachRetained(ctx, "tenant-noquota", retOwner, retOwner, rec.ID, "k-nq", attachReq(), nil)
	if err == nil {
		t.Fatal("attach without quota row succeeded")
	}
}

// TestRetention_AttachDeletedMidway: when the consuming workspace is
// deleted before the attach completes, the disk returns to Retained and is
// attachable/purgeable again — never orphaned.
func TestRetention_AttachDeletedMidway(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	rstore := api.NewRetainedStore(db)
	tenant := "tenant-midway"
	setQuota(t, db, tenant, provisioning.ResourceVector{
		RunningSlots: 100, CPUMillis: 1 << 30, MemoryBytes: 1 << 40, DiskBytes: 1 << 40,
	})

	rec, err := rstore.ImportRetained(ctx, retainedInfo(tenant, "ns", "pvc-mid", "uid-mid", "ws-mid", "src", "LinuxContainer", 20<<30))
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	ws, err := rstore.AttachRetained(ctx, tenant, retOwner, retOwner, rec.ID, "k-mid", attachReq(), []byte("a"))
	if err != nil {
		t.Fatalf("attach: %v", err)
	}

	// The consuming workspace is deleted before the attach settles — the
	// delete path hands the disk back through ReturnToRetained.
	if _, err := provisioning.NewService(db).SignalWorkspace(ctx, tenant, retOwner, retOwner, ws.ID, "", provisioning.IntentDelete, nil); err != nil {
		t.Fatalf("delete consuming ws: %v", err)
	}
	if err := rstore.ReturnToRetained(ctx, rec.ID, ws.ID); err != nil {
		t.Fatalf("ReturnToRetained: %v", err)
	}
	recs, _, err := rstore.ListRetained(ctx, tenant, retOwner, retOwner, "", 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var final *api.RetainedRecord
	for i := range recs {
		if recs[i].ID == rec.ID {
			final = &recs[i]
		}
	}
	if final == nil || final.State != api.RetainedStateRetained {
		t.Fatalf("record after mid-attach delete = %+v, want Retained", final)
	}
	// The disk can be claimed again.
	if _, err := rstore.AttachRetained(ctx, tenant, retOwner, retOwner, rec.ID, "k-mid-2", attachReq(), []byte("b")); err != nil {
		t.Fatalf("re-attach after return: %v", err)
	}
}

// TestRetention_OrphanInventory: orphan detection is bidirectional — a
// retained-labelled PVC with no API-DB record is unregistered, and a record
// pointing at a missing volume is flagged.
func TestRetention_OrphanInventory(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	ns := newRetainedNamespace(t)
	rstore := api.NewRetainedStore(db)
	inv := operator.NewRetentionInventory(k8sClient)

	// PVC labelled retained but unknown to the DB.
	orphanPVC := newRetainedPVC(t, ns, "ws-orphan-uid-0001", "ws-orphan", true)

	// A live record whose PVC was never created in the namespace.
	rec, err := rstore.ImportRetained(ctx, retainedInfo("tenant-it", ns, "pvc-ghost", "uid-ghost", "ws-ghost", "ghost", "LinuxContainer", 20<<30))
	if err != nil {
		t.Fatalf("import ghost record: %v", err)
	}
	_ = rec

	known, err := rstore.RetainedPVCUIDs(ctx, "tenant-it")
	if err != nil {
		t.Fatalf("RetainedPVCUIDs: %v", err)
	}
	orphans, err := inv.Unregistered(ctx, ns, func(_ context.Context, uid types.UID) (bool, error) {
		_, ok := known[string(uid)]
		return ok, nil
	})
	if err != nil {
		t.Fatalf("Unregistered: %v", err)
	}
	found := false
	for _, d := range orphans {
		if d.PVCUID == orphanPVC.UID {
			found = true
		}
	}
	if !found {
		t.Fatalf("orphan PVC %s not detected: %+v", orphanPVC.UID, orphans)
	}

	var want []types.UID
	for uid := range known {
		want = append(want, types.UID(uid))
	}
	missing, err := inv.MissingVolumes(ctx, ns, want)
	if err != nil {
		t.Fatalf("MissingVolumes: %v", err)
	}
	found = false
	for _, u := range missing {
		if string(u) == "uid-ghost" {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing volume uid-ghost not detected: %v", missing)
	}
}

// TestRetention_DecommissionGuard: a namespace with retained datasets
// cannot be decommissioned — inventory check is mandatory, not optional.
func TestRetention_DecommissionGuard(t *testing.T) {
	ctx := context.Background()
	ns := newRetainedNamespace(t)
	clean := newRetainedNamespace(t)
	newRetainedPVC(t, ns, "ws-decomm-uid-0001", "ws-decomm", true)

	inv := operator.NewRetentionInventory(k8sClient)
	blocking, err := inv.DecommissionCheck(ctx, ns)
	if err != nil {
		t.Fatalf("DecommissionCheck: %v", err)
	}
	if len(blocking) == 0 {
		t.Fatal("decommission not blocked despite retained PVC")
	}
	blocking, err = inv.DecommissionCheck(ctx, clean)
	if err != nil {
		t.Fatalf("DecommissionCheck clean: %v", err)
	}
	if len(blocking) != 0 {
		t.Fatalf("clean namespace blocked: %+v", blocking)
	}
}
