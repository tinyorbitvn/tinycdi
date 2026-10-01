//go:build integration

package integration

// Permanent regression tests for the 2026-10-01 security review:
//
//   - SEC-01 (High): POST /v1/workspaces carrying retainedDataRef bypassed
//     the retained-disk owner/state machine — a same-tenant user could mount
//     another user's retained home disk. Fix: create-with-ref routes through
//     the claimed
//     AttachRetained path; plain CreateWorkspace rejects the ref;
//     RetainedApplier.applyAttach fails closed unless the record was
//     claimed (Attaching/Attached) for THIS workspace by THIS owner before
//     any CR/PVC mutation.
//   - SEC-21 (Low): idempotency keys were scoped to (tenant, key) only —
//     the op was not compared and a replay returned before the owner
//     check. Fix: keys are bound to the authenticated principal
//     (scopedIdemKey), the stored op is compared, and SignalWorkspace
//     performs the owner-scoped lookup before consulting idempotency.
//   - SEC-I7: a malformed pageToken is a 400, not a 500.
//
// Run: go test -tags=integration ./tests/integration -run 'TestSEC' -v

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/tinyorbitvn/tinycdi/internal/api"
	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
	linux "github.com/tinyorbitvn/tinycdi/internal/runtime/linux"
)

const secTenant = "tenant-sec01"

// secApplier wires the production apply chain (CR projection + retained
// side effects) over envtest and the real store.
func secApplier(tenant, ns string, rstore *provisioning.RetainedStore) *provisioning.RetainedApplier {
	tmap := provisioning.TenantNamespaces{tenant: ns}
	return provisioning.NewRetainedApplier(
		provisioning.NewK8sApplier(k8sClient, tmap), k8sClient, tmap, rstore)
}

// TestSEC01_CreateRejectsBareRef: the plain create path fails closed on a
// retainedDataRef — no workspace row, no intent, record untouched — even
// when the caller knows a foreign rd_ id.
func TestSEC01_CreateRejectsBareRef(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	setQuota(t, db, secTenant, provisioning.ResourceVector{
		RunningSlots: 100, CPUMillis: 1 << 30, MemoryBytes: 1 << 40, DiskBytes: 1 << 40,
	})
	svc := provisioning.NewService(db)
	rstore := provisioning.NewRetainedStore(db)

	rec, err := rstore.ImportRetained(ctx, api.RetainedDiskInfo{
		PVCNamespace: "ns", PVCName: "pvc-vic", PVCUID: "uid-vic", TenantID: secTenant,
		Owner: "https://issuer.test|victim", SourceWorkspaceID: "ws_vicsrc0001",
		SourceWorkspaceName: "victim-desktop", Runtime: "LinuxContainer", SizeBytes: 20 << 30,
	})
	if err != nil {
		t.Fatalf("import: %v", err)
	}

	_, err = svc.CreateWorkspace(ctx, secTenant, "key-evil-0001", provisioning.CreateRequest{
		OwnerIssuer:     "https://issuer.test",
		OwnerSubject:    "mallory",
		Name:            "loot",
		Template:        provisioning.TemplateInfo{ID: "tpl_linuxdesktop", Name: "linuxdesktop", Revision: 1, Runtime: "LinuxContainer"},
		Vector:          provisioning.ResourceVector{RunningSlots: 1, CPUMillis: 2000, MemoryBytes: 4 << 30, DiskBytes: 20 << 30},
		RetainedDataRef: rec.ID,
	}, []byte("evil"))
	if err == nil {
		t.Fatal("create-with-ref accepted: plain create must refuse a retainedDataRef")
	}

	var n int
	if err := db.Pool().QueryRow(ctx,
		`SELECT count(*) FROM workspaces WHERE retained_data_ref = $1`, rec.ID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("workspaces referencing record = %d (err %v), want 0", n, err)
	}
	if err := db.Pool().QueryRow(ctx,
		`SELECT count(*) FROM outbox_intent`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("outbox intents = %d (err %v), want 0", n, err)
	}
	after, err := rstore.GetRetained(ctx, secTenant, rec.ID)
	if err != nil || after.State != api.RetainedStateRetained || after.ConsumingWorkspaceID != "" {
		t.Fatalf("record after rejected create = %+v (err %v), want untouched Retained", after, err)
	}
}

// TestSEC01_UnclaimedRecordNeverMutated: an intent that names a record
// which was never claimed for this workspace (state Retained, consumer
// mismatch, or owner mismatch) must fail BEFORE any cluster mutation —
// no Workspace CR stamp, no PVC relabel.
func TestSEC01_UnclaimedRecordNeverMutated(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	ns := newRetainedNamespace(t)
	setQuota(t, db, secTenant, provisioning.ResourceVector{
		RunningSlots: 100, CPUMillis: 1 << 30, MemoryBytes: 1 << 40, DiskBytes: 1 << 40,
	})
	rstore := provisioning.NewRetainedStore(db)
	victimOwner := "https://issuer.test|victim"
	attacker := provisioning.IntentSpec{
		WorkspaceName: "loot", TemplateName: "linuxdesktop",
		OwnerIssuer: "https://issuer.test", OwnerSubject: "mallory",
		DataPolicy: "Retain",
	}

	// Victim's retained home volume, labelled for its original workspace.
	pvc := newRetainedPVC(t, ns, types.UID("victim-old-cr-uid"), "victim-desktop", true)
	wantUIDLabel := pvc.Labels[linux.LabelWorkspaceUID]

	rec, err := rstore.ImportRetained(ctx, api.RetainedDiskInfo{
		PVCNamespace: ns, PVCName: pvc.Name, PVCUID: string(pvc.UID), TenantID: secTenant,
		Owner: victimOwner, SourceWorkspaceID: "ws_vicsrc0002",
		SourceWorkspaceName: "victim-desktop", Runtime: "LinuxContainer", SizeBytes: 20 << 30,
	})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	applier := secApplier(secTenant, ns, rstore)

	// wsID must look like a platform id: ws_<hex> -> CR name ws-<hex>.
	evilUID := provisioning.PlatformID("ws_evil0000001")
	forged := provisioning.Intent{
		WorkspaceUID:      evilUID,
		TenantID:          secTenant,
		Revision:          1,
		Kind:              provisioning.IntentCreate,
		RequestID:         "req-evil-1",
		DesiredState:      "Running",
		RuntimeGeneration: 1,
		Spec:              attacker,
	}
	forged.Spec.RetainedDataRef = rec.ID

	err = applier.Apply(ctx, forged)
	if !errors.Is(err, api.ErrRetainedState) {
		t.Fatalf("applyAttach on unclaimed record: err=%v, want ErrRetainedState", err)
	}

	// The record is untouched.
	after, err := rstore.GetRetained(ctx, secTenant, rec.ID)
	if err != nil || after.State != api.RetainedStateRetained || after.ConsumingWorkspaceID != "" {
		t.Fatalf("record after forged intent = %+v, want Retained unclaimed", after)
	}
	// The victim PVC keeps its original labels — no retarget to the
	// attacker's workspace.
	gotPVC := &corev1.PersistentVolumeClaim{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: pvc.Name}, gotPVC); err != nil {
		t.Fatalf("get pvc: %v", err)
	}
	if gotPVC.Labels[linux.LabelWorkspaceUID] != wantUIDLabel ||
		gotPVC.Labels[linux.LabelWorkspaceName] != "victim-desktop" {
		t.Fatalf("victim PVC relabelled despite failed claim: %v", gotPVC.Labels)
	}
	// The attacker's CR (created by the inner applier) carries no
	// retained-pvc stamp.
	got := workspaceCRByUID(t, string(evilUID))
	if got != nil {
		if got.Annotations[linux.AnnotationRetainedPVC] != "" ||
			got.Annotations[linux.AnnotationRetainedPVCUID] != "" ||
			got.Annotations[provisioning.AnnotationRetainedDataRef] != "" {
			t.Fatalf("attacker CR stamped with retained annotations: %v", got.Annotations)
		}
	}
}

// TestSEC01_ClaimedRecordApplies: the legitimate claim path still works —
// AttachRetained moves Retained->Attaching, the applier stamps the CR and
// retargets the PVC, CompleteAttach settles Attached, and a replayed
// delivery is a no-op success.
func TestSEC01_ClaimedRecordApplies(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	ns := newRetainedNamespace(t)
	setQuota(t, db, secTenant, provisioning.ResourceVector{
		RunningSlots: 100, CPUMillis: 1 << 30, MemoryBytes: 1 << 40, DiskBytes: 1 << 40,
	})
	svc := provisioning.NewService(db)
	rstore := provisioning.NewRetainedStore(db)

	pvc := newRetainedPVC(t, ns, types.UID("old-cr-uid"), "old-desktop", true)
	rec, err := rstore.ImportRetained(ctx, api.RetainedDiskInfo{
		PVCNamespace: ns, PVCName: pvc.Name, PVCUID: string(pvc.UID), TenantID: secTenant,
		Owner: retOwner, SourceWorkspaceID: "ws_src0000001",
		SourceWorkspaceName: "old-desktop", Runtime: "LinuxContainer", SizeBytes: 20 << 30,
	})
	if err != nil {
		t.Fatalf("import: %v", err)
	}

	ws, err := svc.AttachRetained(ctx, secTenant, retOwner, retOwner, rec.ID, "k-sec01-attach", attachReq(), []byte("a"))
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	if ws.Owner != retOwner || ws.DataPolicy != "Retain" || ws.RetainedDataRef != rec.ID {
		t.Fatalf("attach workspace = %+v", ws)
	}
	mid, err := rstore.GetRetained(ctx, secTenant, rec.ID)
	if err != nil || mid.State != api.RetainedStateAttaching || mid.ConsumingWorkspaceID != ws.ID {
		t.Fatalf("record mid-attach = %+v, want Attaching -> %s", mid, ws.ID)
	}

	// Deliver the create intent exactly as the dispatcher would.
	pending, err := provisioning.NewOutbox(db).PendingIntents(ctx, provisioning.PlatformID(ws.ID))
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending intents = %v (err %v), want 1 create", pending, err)
	}
	applier := secApplier(secTenant, ns, rstore)
	if err := applier.Apply(ctx, pending[0]); err != nil {
		t.Fatalf("apply create: %v", err)
	}

	cr := workspaceCRByUID(t, ws.ID)
	if cr == nil {
		t.Fatal("consuming workspace CR not created")
	}
	ann := cr.Annotations
	if ann[linux.AnnotationRetainedPVC] != pvc.Name ||
		ann[linux.AnnotationRetainedPVCUID] != string(pvc.UID) ||
		ann[provisioning.AnnotationRetainedDataRef] != rec.ID {
		t.Fatalf("CR annotations = %v", ann)
	}
	gotPVC := &corev1.PersistentVolumeClaim{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: pvc.Name}, gotPVC); err != nil {
		t.Fatalf("get pvc: %v", err)
	}
	if gotPVC.Labels[linux.LabelWorkspaceUID] != string(cr.UID) {
		t.Fatalf("PVC workspace-uid = %q, want consumer CR UID %q",
			gotPVC.Labels[linux.LabelWorkspaceUID], cr.UID)
	}
	after, err := rstore.GetRetained(ctx, secTenant, rec.ID)
	if err != nil || after.State != api.RetainedStateAttached || after.ConsumingWorkspaceID != ws.ID {
		t.Fatalf("record after apply = %+v, want Attached -> %s", after, ws.ID)
	}

	// At-least-once replay of the same intent is a no-op success.
	if err := applier.Apply(ctx, pending[0]); err != nil {
		t.Fatalf("replayed apply: %v", err)
	}

	// A forged intent naming the now-Attached record for a DIFFERENT
	// workspace still fails closed — the claim belongs to ws.ID.
	forged := pending[0]
	forged.WorkspaceUID = provisioning.PlatformID("ws_evil0000002")
	if err := applier.Apply(ctx, forged); !errors.Is(err, api.ErrRetainedState) {
		t.Fatalf("forged apply on Attached record: err=%v, want ErrRetainedState", err)
	}
}

// TestSEC21_IdempotencyPrincipalScoped: keys are bound to the
// authenticated principal and the stored op is compared.
func TestSEC21_IdempotencyPrincipalScoped(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	tenant := "tenant-sec21"
	setQuota(t, db, tenant, provisioning.ResourceVector{
		RunningSlots: 100, CPUMillis: 1 << 30, MemoryBytes: 1 << 40, DiskBytes: 1 << 40,
	})
	svc := provisioning.NewService(db)
	rstore := provisioning.NewRetainedStore(db)

	mkReq := func(sub, name string) provisioning.CreateRequest {
		return provisioning.CreateRequest{
			OwnerIssuer:  "https://issuer.test",
			OwnerSubject: sub,
			Name:         name,
			Template:     provisioning.TemplateInfo{ID: "tpl_linuxdesktop", Name: "linuxdesktop", Revision: 1, Runtime: "LinuxContainer"},
			Vector:       provisioning.ResourceVector{RunningSlots: 1, CPUMillis: 2000, MemoryBytes: 4 << 30, DiskBytes: 20 << 30},
			DataPolicy:   "Ephemeral",
		}
	}
	// The wire body carries no owner — both principals send the same one.
	bodyHash := []byte("same-body")
	alice := "https://issuer.test|alice"
	bob := "https://issuer.test|bob"

	// (a) bob reusing alice's key+body mints HIS OWN workspace — he can
	// never receive alice's stored result.
	wsA, err := svc.CreateWorkspace(ctx, tenant, "shared-key-0001", mkReq("alice", "desk"), bodyHash)
	if err != nil {
		t.Fatalf("alice create: %v", err)
	}
	wsB, err := svc.CreateWorkspace(ctx, tenant, "shared-key-0001", mkReq("bob", "desk"), bodyHash)
	if err != nil {
		t.Fatalf("bob create: %v", err)
	}
	if wsB.ID == wsA.ID || wsB.Owner != bob {
		t.Fatalf("bob replayed alice's result: %+v", wsB)
	}
	// And alice's own replay still returns her stored workspace.
	replayA, err := svc.CreateWorkspace(ctx, tenant, "shared-key-0001", mkReq("alice", "desk"), bodyHash)
	if err != nil || !replayA.Replayed || replayA.ID != wsA.ID {
		t.Fatalf("alice replay = %+v err=%v, want stored %s", replayA, err, wsA.ID)
	}

	// (b) bob presenting alice's start key on alice's workspace hits the
	// owner check BEFORE any replay: not found, never alice's record.
	empty := []byte{}
	if _, err := svc.SignalWorkspace(ctx, tenant, alice, alice, wsA.ID, "start-key-0001", provisioning.IntentStart, empty); err != nil {
		t.Fatalf("alice start: %v", err)
	}
	if _, err := svc.SignalWorkspace(ctx, tenant, bob, bob, wsA.ID, "start-key-0001", provisioning.IntentStart, empty); !errors.Is(err, provisioning.ErrWorkspaceNotFound) {
		t.Fatalf("bob signal on alice ws: err=%v, want ErrWorkspaceNotFound", err)
	}
	// The same key on bob's own workspace is an independent op and runs.
	bStart, err := svc.SignalWorkspace(ctx, tenant, bob, bob, wsB.ID, "start-key-0001", provisioning.IntentStart, empty)
	if err != nil || bStart.ID != wsB.ID {
		t.Fatalf("bob start own ws: %+v err=%v", bStart, err)
	}

	// (c) same key reused for a different op is a conflict, not a silent
	// replay — alice's stop is NOT suppressed by her recorded start.
	if _, err := svc.SignalWorkspace(ctx, tenant, alice, alice, wsA.ID, "start-key-0001", provisioning.IntentStop, empty); !errors.Is(err, provisioning.ErrIdempotencyConflict) {
		t.Fatalf("alice stop reusing start key: err=%v, want ErrIdempotencyConflict", err)
	}
	st, err := svc.SignalWorkspace(ctx, tenant, alice, alice, wsA.ID, "stop-key-00001", provisioning.IntentStop, empty)
	if err != nil || st.DesiredState != "Stopped" {
		t.Fatalf("alice stop: %+v err=%v", st, err)
	}

	// (d) attach is principal-scoped too: bob reusing alice's attach key on
	// alice's record does not replay her workspace — it runs and fails
	// owner-scoped.
	rec, err := rstore.ImportRetained(ctx, api.RetainedDiskInfo{
		PVCNamespace: "ns", PVCName: "pvc-a", PVCUID: "uid-a", TenantID: tenant,
		Owner: alice, SourceWorkspaceID: "ws_srca000001",
		SourceWorkspaceName: "alice-desktop", Runtime: "LinuxContainer", SizeBytes: 1 << 30,
	})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if _, err := rstore.AttachRetained(ctx, tenant, alice, alice, rec.ID, "at-key-0001", attachReq(), bodyHash); err != nil {
		t.Fatalf("alice attach: %v", err)
	}
	if _, err := rstore.AttachRetained(ctx, tenant, bob, bob, rec.ID, "at-key-0001", attachReq(), bodyHash); !errors.Is(err, api.ErrRetainedNotFound) {
		t.Fatalf("bob attach alice's record with her key: err=%v, want ErrRetainedNotFound", err)
	}
}

// TestSECI7_BadPageToken: a malformed cursor is ErrBadCursor so handlers
// return 400 INVALID_REQUEST instead of 500.
func TestSECI7_BadPageToken(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	svc := provisioning.NewService(db)
	rstore := provisioning.NewRetainedStore(db)

	for _, tok := range []string{"!!!", "not-base64!!!", "AAAA"} {
		if _, _, err := svc.ListWorkspaces(ctx, secTenant, "", "", tok, 10); !errors.Is(err, provisioning.ErrBadCursor) {
			t.Fatalf("ListWorkspaces pageToken %q: err=%v, want ErrBadCursor", tok, err)
		}
		if _, _, err := rstore.ListRetained(ctx, secTenant, retOwner, retOwner, tok, 10); !errors.Is(err, provisioning.ErrBadCursor) {
			t.Fatalf("ListRetained pageToken %q: err=%v, want ErrBadCursor", tok, err)
		}
	}
}
