package provisioning_test

// Contract tests for RetainedSync — the API-side reconciler that projects
// the operator's retained-inventory PVC metadata (source of truth,
// design §5) into retained_data records. Uses a fake controller-runtime
// client for PVCs and the shared pg harness for the store.

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
	"github.com/tinyorbitvn/tinycdi/internal/store"
)

var syncScheme = func() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	return s
}()

// retainedPVC builds a PVC carrying the operator's retained-inventory
// contract (retention.go MarkRetained): data-retained label + workspace
// labels (CR uid/name) + retained-* annotations.
func retainedPVC(ns, name, crUID, crName string, ann map[string]string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns, Name: name, UID: types.UID("uid-" + name),
			Labels: map[string]string{
				"workspaces.cdi.tinyorbit.vn/data-retained":  "true",
				"workspaces.cdi.tinyorbit.vn/workspace-uid":  crUID,
				"workspaces.cdi.tinyorbit.vn/workspace-name": crName,
			},
			Annotations: ann,
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse("2Gi"),
				},
			},
		},
	}
}

func fullRetainedAnnotations() map[string]string {
	return map[string]string{
		"workspaces.cdi.tinyorbit.vn/retained-tenant":           "tenant-it",
		"workspaces.cdi.tinyorbit.vn/retained-owner":            "iss|sub-1",
		"workspaces.cdi.tinyorbit.vn/retained-source-workspace": "ws-src1abc",
		"workspaces.cdi.tinyorbit.vn/retained-runtime":          "LinuxContainer",
		"workspaces.cdi.tinyorbit.vn/retained-at":               "2026-09-30T12:00:00Z",
	}
}

func seedWorkspaceRow(t *testing.T, db *store.DB, id, tenant, owner, name string) {
	t.Helper()
	if _, err := db.Pool().Exec(context.Background(), `
		INSERT INTO workspaces (id, tenant_id, owner_subject, request_id, name)
		VALUES ($1, $2, $3, $4, $5)`, id, tenant, owner, "req-"+id, name); err != nil {
		t.Fatalf("seed workspace %s: %v", id, err)
	}
}

// TestRetainedSync_ImportsUnregisteredPVC: a retained-labelled PVC with no
// record is imported into retained_data — owner, tenant, source workspace
// and runtime all decoded from the PVC contract.
func TestRetainedSync_ImportsUnregisteredPVC(t *testing.T) {
	db := recoveryDB(t)
	ctx := context.Background()

	pvc := retainedPVC("ns-it", "pvc-1", "cruid-1", "ws-src1abc", fullRetainedAnnotations())
	kc := fake.NewClientBuilder().WithScheme(syncScheme).WithObjects(pvc).Build()

	sync := provisioning.NewRetainedSync(kc,
		provisioning.TenantNamespaces{"tenant-it": "ns-it"},
		provisioning.NewRetainedStore(db), nil, nil)
	if _, err := sync.SyncOnce(ctx); err != nil {
		t.Fatalf("SyncOnce: %v", err)
	}

	recs, _, err := provisioning.NewRetainedStore(db).ListRetained(ctx, "tenant-it", "iss|sub-1", "", "", 10)
	if err != nil {
		t.Fatalf("list retained: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("expected 1 retained record, got %d", len(recs))
	}
	rec := recs[0]
	if rec.PVCUID != "uid-pvc-1" || rec.PVCNamespace != "ns-it" || rec.PVCName != "pvc-1" {
		t.Fatalf("record identity wrong: %+v", rec)
	}
	if rec.Owner != "iss|sub-1" || rec.TenantID != "tenant-it" {
		t.Fatalf("record principal wrong: %+v", rec)
	}
	if rec.SourceWorkspaceID != "ws_src1abc" {
		t.Fatalf("source workspace id = %q, want ws_src1abc", rec.SourceWorkspaceID)
	}
	if rec.SourceWorkspaceName != "ws-src1abc" {
		// no workspaces row seeded: falls back to the PVC annotation
		t.Fatalf("source name = %q, want annotation fallback ws-src1abc", rec.SourceWorkspaceName)
	}
	if rec.Runtime != "LinuxContainer" {
		t.Fatalf("runtime = %q", rec.Runtime)
	}

	// second sweep is a no-op: idempotent on (pvc_namespace, pvc_uid)
	if _, err := sync.SyncOnce(ctx); err != nil {
		t.Fatalf("second SyncOnce: %v", err)
	}
	recs, _, _ = provisioning.NewRetainedStore(db).ListRetained(ctx, "tenant-it", "iss|sub-1", "", "", 10)
	if len(recs) != 1 {
		t.Fatalf("second sweep duplicated the record: %d rows", len(recs))
	}
}

// TestRetainedSync_OrphanCounts: the sweep reports drift in both
// directions — a retained PVC with no record (imported, counted) and a
// record whose PVC is gone (counted, never deleted).
func TestRetainedSync_OrphanCounts(t *testing.T) {
	db := recoveryDB(t)
	ctx := context.Background()
	rstore := provisioning.NewRetainedStore(db)

	// A live record pointing at a PVC that does not exist in the cluster.
	if _, err := db.Pool().Exec(ctx, `
		INSERT INTO workspaces (id, tenant_id, owner_subject, request_id)
		VALUES ('ws_ghost1', 'tenant-it', 'iss|sub-1', 'req-g')`); err != nil {
		t.Fatalf("seed workspace: %v", err)
	}
	if _, err := rstore.ImportRetained(ctx, provisioning.RetainedDiskInfo{
		PVCNamespace: "ns-it", PVCName: "pvc-ghost", PVCUID: "uid-ghost",
		TenantID: "tenant-it", Owner: "iss|sub-1",
		SourceWorkspaceID: "ws_ghost1", SourceWorkspaceName: "ghost",
		Runtime: "LinuxContainer", SizeBytes: 1 << 30,
	}); err != nil {
		t.Fatalf("import ghost: %v", err)
	}

	pvc := retainedPVC("ns-it", "pvc-new", "cruid-2", "ws-new2def", fullRetainedAnnotations())
	kc := fake.NewClientBuilder().WithScheme(syncScheme).WithObjects(pvc).Build()
	sync := provisioning.NewRetainedSync(kc,
		provisioning.TenantNamespaces{"tenant-it": "ns-it"}, rstore, nil, nil)

	res, err := sync.SyncOnce(ctx)
	if err != nil {
		t.Fatalf("SyncOnce: %v", err)
	}
	if res.UnregisteredPVCs != 1 {
		t.Fatalf("unregistered PVCs = %d, want 1", res.UnregisteredPVCs)
	}
	if res.MissingVolumes != 1 {
		t.Fatalf("missing volumes = %d, want 1", res.MissingVolumes)
	}

	// after the sweep the new PVC is registered; the ghost stays a leak
	res, err = sync.SyncOnce(ctx)
	if err != nil {
		t.Fatalf("second SyncOnce: %v", err)
	}
	if res.UnregisteredPVCs != 0 || res.MissingVolumes != 1 {
		t.Fatalf("second sweep: %+v", res)
	}
}

// TestRetainedSync_DisplayName: when the API DB still has the source
// workspaces row, the imported record carries the user-facing workspace
// name — not the CR-name annotation.
func TestRetainedSync_DisplayName(t *testing.T) {
	db := recoveryDB(t)
	ctx := context.Background()
	seedWorkspaceRow(t, db, "ws_disp9jkl", "tenant-it", "iss|sub-1", "my-desktop")
	pvc := retainedPVC("ns-it", "pvc-disp", "cruid-4", "ws-disp9jkl", fullRetainedAnnotations())
	kc := fake.NewClientBuilder().WithScheme(syncScheme).WithObjects(pvc).Build()
	sync := provisioning.NewRetainedSync(kc,
		provisioning.TenantNamespaces{"tenant-it": "ns-it"},
		provisioning.NewRetainedStore(db), nil, nil)
	if _, err := sync.SyncOnce(ctx); err != nil {
		t.Fatalf("SyncOnce: %v", err)
	}
	recs, _, _ := provisioning.NewRetainedStore(db).ListRetained(ctx, "tenant-it", "iss|sub-1", "", "", 10)
	if len(recs) != 1 || recs[0].SourceWorkspaceName != "my-desktop" {
		t.Fatalf("source name = %+v, want my-desktop", recs)
	}
}

// TestRetainedSync_TenantFallback: a PVC missing the retained-tenant
// annotation still lands in the right tenant via the namespace mapping.
func TestRetainedSync_TenantFallback(t *testing.T) {
	db := recoveryDB(t)
	ctx := context.Background()
	ann := fullRetainedAnnotations()
	delete(ann, "workspaces.cdi.tinyorbit.vn/retained-tenant")
	pvc := retainedPVC("ns-fb", "pvc-fb", "cruid-3", "ws-fb3ghi", ann)
	kc := fake.NewClientBuilder().WithScheme(syncScheme).WithObjects(pvc).Build()
	sync := provisioning.NewRetainedSync(kc,
		provisioning.TenantNamespaces{"tenant-fb": "ns-fb"},
		provisioning.NewRetainedStore(db), nil, nil)
	if _, err := sync.SyncOnce(ctx); err != nil {
		t.Fatalf("SyncOnce: %v", err)
	}
	recs, _, _ := provisioning.NewRetainedStore(db).ListRetained(ctx, "tenant-fb", "iss|sub-1", "", "", 10)
	if len(recs) != 1 {
		t.Fatalf("expected the record in tenant-fb, got %d", len(recs))
	}
}
