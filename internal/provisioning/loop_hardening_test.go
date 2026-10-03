// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package provisioning_test

// V3.25 loop-hardening contract tests:
//   - bookkeeping after a state change lands on a bounded detached context
//     even when the pass ctx is cancelled mid-apply (backlog 11);
//   - a held reservation left with every dimension zero is settled
//     (releaseDiskQuota in-tx, plus the recovery sweep for stranded rows);
//   - the event-driven settle worker drains observed-absence triggers and
//     the periodic pass still settles when no event ever arrives.

import (
	"context"
	"errors"
	"testing"

	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
	"github.com/tinyorbitvn/tinycdi/internal/store"
)

// cancelApplier cancels the pass context inside Apply, simulating the
// leader losing its lock between the state change and the bookkeeping;
// failOn marks the workspace whose apply errors.
type cancelApplier struct {
	cancel context.CancelFunc
	failOn provisioning.PlatformID
}

func (a cancelApplier) Apply(_ context.Context, in provisioning.Intent) error {
	a.cancel()
	if in.WorkspaceUID == a.failOn {
		return errors.New("injected apply failure")
	}
	return nil
}

func mustAppendIntent(t *testing.T, db *store.DB, wsUID string, kind provisioning.IntentKind) uint64 {
	t.Helper()
	var rev uint64
	if err := db.WithTx(context.Background(), func(tx store.Tx) error {
		var err error
		rev, err = provisioning.AppendIntent(context.Background(), tx, provisioning.PlatformID(wsUID), kind)
		return err
	}); err != nil {
		t.Fatalf("append intent: %v", err)
	}
	return rev
}

// TestRecovery_DetachedBookkeeping: a cancelled pass ctx must not drop the
// apply bookkeeping — the intent is still marked dispatched after a
// successful apply, and a failed apply still bumps its persisted attempt
// count (backlog 11, same class as the dispatcher's detached ack).
func TestRecovery_DetachedBookkeeping(t *testing.T) {
	db := recoveryDB(t)
	ctx := context.Background()
	seedHeldWorkspace(t, db, "tenant-det", "ws_detok", quotaVec)
	seedHeldWorkspace(t, db, "tenant-det", "ws_detfail", quotaVec)
	rec := provisioning.NewRecovery(db, &fakeObserver{})

	// Pass 1: apply succeeds as the leader loses its lock — the ack still
	// lands on the detached ctx.
	mustAppendIntent(t, db, "ws_detok", provisioning.IntentStop)
	passCtx, cancel := context.WithCancel(ctx)
	_, _ = rec.Recover(passCtx, cancelApplier{cancel: cancel})
	var dispatched bool
	if err := db.Pool().QueryRow(ctx, `
		SELECT dispatched_at IS NOT NULL FROM outbox_intent
		WHERE workspace_id = 'ws_detok'`).Scan(&dispatched); err != nil {
		t.Fatal(err)
	}
	if !dispatched {
		t.Fatal("apply ack dropped on cancelled ctx")
	}

	// Pass 2: apply fails as the leader loses its lock — the attempt count
	// still lands on the detached ctx.
	mustAppendIntent(t, db, "ws_detfail", provisioning.IntentStop)
	passCtx2, cancel2 := context.WithCancel(ctx)
	_, _ = rec.Recover(passCtx2, cancelApplier{cancel: cancel2, failOn: "ws_detfail"})
	var attempts int
	if err := db.Pool().QueryRow(ctx, `
		SELECT COALESCE((payload->'recovery'->>'attempts')::int, 0) FROM outbox_intent
		WHERE workspace_id = 'ws_detfail'`).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1 recorded on cancelled ctx", attempts)
	}
}

// resState reads one reservation's (state, vector, restart-present, proof).
func resState(t *testing.T, db *store.DB, wsUID string) (state string, v provisioning.ResourceVector, restart bool, proof string) {
	t.Helper()
	var rs, rc, rm *int64
	var rp *string
	err := db.Pool().QueryRow(context.Background(), `
		SELECT state, running_slots, cpu_millis, memory_bytes, disk_bytes,
		       restart_slots, restart_cpu_millis, restart_memory_bytes, release_proof
		FROM quota_reservation WHERE workspace_id = $1`, wsUID).
		Scan(&state, &v.RunningSlots, &v.CPUMillis, &v.MemoryBytes, &v.DiskBytes, &rs, &rc, &rm, &rp)
	if err != nil {
		t.Fatalf("reservation %s: %v", wsUID, err)
	}
	if rp != nil {
		proof = *rp
	}
	return state, v, rs != nil || rc != nil || rm != nil, proof
}

// TestPurge_SettlesEmptyReservation: purging the last retained disk of a
// deleted workspace leaves a held reservation with every dimension zero —
// the subtraction settles it to released with proof quota_settled.
func TestPurge_SettlesEmptyReservation(t *testing.T) {
	db := recoveryDB(t)
	ctx := context.Background()
	st := provisioning.NewRetainedStore(db)
	const tenant = "tenant-z"

	// Deleted source workspace: compute released earlier (released row),
	// the retained disk's bytes re-held disk-only by the import.
	seedHeldWorkspace(t, db, tenant, "ws_zsrc", quotaVec)
	if _, err := db.Pool().Exec(ctx,
		`UPDATE workspaces SET state = 'deleted' WHERE id = 'ws_zsrc'`); err != nil {
		t.Fatal(err)
	}
	if err := db.WithTx(ctx, func(tx store.Tx) error {
		return provisioning.Release(ctx, tx, tenant, "ws_zsrc", provisioning.ProofRuntimeAbsent)
	}); err != nil {
		t.Fatal(err)
	}
	rec, err := st.ImportRetained(ctx, provisioning.RetainedDiskInfo{
		TenantID: tenant, Owner: "iss|sub-1", SourceWorkspaceID: "ws_zsrc",
		Runtime: "LinuxContainer", SizeBytes: 2 << 30,
		PVCNamespace: "ns-it", PVCName: "pvc-z", PVCUID: "uid-pvc-z",
	})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if state, v, _, _ := resState(t, db, "ws_zsrc"); state != "held" ||
		v != (provisioning.ResourceVector{DiskBytes: 2 << 30}) {
		t.Fatalf("post-import reservation = %s %+v, want held disk-only", state, v)
	}

	// Retained -> Purging under the owner nonce, then the sweeper's
	// completion proof.
	got, err := st.ReadRetained(ctx, tenant, "iss|sub-1", "iss|sub-1", rec.ID)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if _, err := st.PurgeRetained(ctx, tenant, "iss|sub-1", "iss|sub-1", rec.ID,
		got.PurgeNonce, "", nil); err != nil {
		t.Fatalf("purge: %v", err)
	}
	if err := st.CompletePurge(ctx, rec.ID); err != nil {
		t.Fatalf("complete purge: %v", err)
	}

	state, v, restart, proof := resState(t, db, "ws_zsrc")
	if state != "released" || proof != string(provisioning.ProofQuotaSettled) || restart {
		t.Fatalf("reservation = %s proof %q restart %v, want released quota_settled", state, proof, restart)
	}
	if v != (provisioning.ResourceVector{}) {
		t.Fatalf("released row vector = %+v, want zero", v)
	}
}

// TestRecovery_SettlesStrandedEmptyRows: held all-zero rows stranded by
// disk releases that predate the in-transaction settle are swept by the
// recovery pass — but only on deleted workspaces.
func TestRecovery_SettlesStrandedEmptyRows(t *testing.T) {
	db := recoveryDB(t)
	ctx := context.Background()
	const tenant = "tenant-strand"
	if err := db.WithTx(ctx, func(tx store.Tx) error {
		return provisioning.SetQuota(ctx, tx, tenant, provisioning.ResourceVector{
			RunningSlots: 5, CPUMillis: 8000, MemoryBytes: 1 << 34, DiskBytes: 1 << 40})
	}); err != nil {
		t.Fatal(err)
	}
	seedWorkspaceRow(t, db, "ws_strand", tenant, "iss|sub-1", "strand")
	seedWorkspaceRow(t, db, "ws_live", tenant, "iss|sub-1", "live")
	if _, err := db.Pool().Exec(ctx,
		`UPDATE workspaces SET state = 'deleted' WHERE id = 'ws_strand'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool().Exec(ctx, `
		INSERT INTO quota_reservation (workspace_id, tenant_id,
			running_slots, cpu_millis, memory_bytes, disk_bytes)
		VALUES ('ws_strand', $1, 0, 0, 0, 0),
		       ('ws_live', $1, 0, 0, 0, 0)`, tenant); err != nil {
		t.Fatal(err)
	}

	actions, err := provisioning.NewRecovery(db, &fakeObserver{}).Recover(ctx, &recordingApplier{})
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	var settled bool
	for _, a := range actions {
		if a.WorkspaceUID == "ws_strand" && a.Kind == provisioning.ActionSettleReservation {
			settled = true
		}
	}
	if !settled {
		t.Fatalf("no settle_reservation action for ws_strand in %+v", actions)
	}
	if state, _, _, proof := resState(t, db, "ws_strand"); state != "released" ||
		proof != string(provisioning.ProofQuotaSettled) {
		t.Fatalf("stranded row = %s proof %q, want released quota_settled", state, proof)
	}
	if state, _, _, _ := resState(t, db, "ws_live"); state != "held" {
		t.Fatalf("live workspace row = %s, want untouched held", state)
	}
}

// TestSettleWorker_EventAndFallback: the drain loop settles a triggered
// candidate on the absence proof; a non-candidate (still Running) stays
// held; and a candidate that never produced an event is still settled by
// the periodic recovery pass.
func TestSettleWorker_EventAndFallback(t *testing.T) {
	db := recoveryDB(t)
	ctx := context.Background()
	const tenant = "tenant-ev"
	if err := db.WithTx(ctx, func(tx store.Tx) error {
		return provisioning.SetQuota(ctx, tx, tenant, provisioning.ResourceVector{
			RunningSlots: 5, CPUMillis: 8000, MemoryBytes: 1 << 34, DiskBytes: 1 << 40})
	}); err != nil {
		t.Fatal(err)
	}
	seed := func(wsUID, state, desired string) {
		t.Helper()
		seedHeldWorkspace(t, db, tenant, wsUID, quotaVec)
		if _, err := db.Pool().Exec(ctx,
			`UPDATE workspaces SET state = $2, desired_state = $3 WHERE id = $1`,
			wsUID, state, desired); err != nil {
			t.Fatal(err)
		}
	}
	seed("ws_ev1", "deleted", "Stopped")
	seed("ws_running", "active", "Running")
	seed("ws_noevent", "deleted", "Stopped")

	trig := provisioning.NewSettleTrigger()
	w := &provisioning.SettleWorker{
		DB:      db,
		Trigger: trig,
		Rec:     provisioning.NewRecovery(db, &fakeObserver{gone: true}),
	}
	trig.Enqueue("ws_ev1")
	trig.Enqueue("ws_running")
	w.DrainOnce(ctx)

	if state, _, _, proof := resState(t, db, "ws_ev1"); state != "released" ||
		proof != string(provisioning.ProofRuntimeAbsent) {
		t.Fatalf("event-settled row = %s proof %q, want released runtime_absent_observed", state, proof)
	}
	if state, _, _, _ := resState(t, db, "ws_running"); state != "held" {
		t.Fatalf("running workspace = %s, want held (non-candidate skipped)", state)
	}
	if n := trig.Pending(); n != 0 {
		t.Fatalf("trigger still holds %d ids, want drained", n)
	}

	// Fallback: no event for ws_noevent — the periodic pass settles it.
	actions, err := provisioning.NewRecovery(db, &fakeObserver{gone: true}).Recover(ctx, &recordingApplier{})
	if err != nil {
		t.Fatalf("recovery fallback: %v", err)
	}
	var released bool
	for _, a := range actions {
		if a.WorkspaceUID == "ws_noevent" && a.Kind == provisioning.ActionReleaseQuota {
			released = true
		}
	}
	if !released {
		t.Fatalf("tick did not settle the event-less workspace: %+v", actions)
	}
	if state, _, _, _ := resState(t, db, "ws_noevent"); state != "released" {
		t.Fatalf("ws_noevent = %s, want released by the tick", state)
	}
}

// TestSettleWorker_UnprovenStaysHeld: an observed event with an observer
// that cannot prove absence leaves the reservation held.
func TestSettleWorker_UnprovenStaysHeld(t *testing.T) {
	db := recoveryDB(t)
	ctx := context.Background()
	seedHeldWorkspace(t, db, "tenant-ev2", "ws_up", quotaVec)
	if _, err := db.Pool().Exec(ctx,
		`UPDATE workspaces SET state = 'deleted' WHERE id = 'ws_up'`); err != nil {
		t.Fatal(err)
	}
	trig := provisioning.NewSettleTrigger()
	w := &provisioning.SettleWorker{
		DB:      db,
		Trigger: trig,
		Rec:     provisioning.NewRecovery(db, &fakeObserver{err: errors.New("api down")}),
	}
	trig.Enqueue("ws_up")
	w.DrainOnce(ctx)
	if state, _, _, _ := resState(t, db, "ws_up"); state != "held" {
		t.Fatalf("unproven absence = %s, want held", state)
	}
}
