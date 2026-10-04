package broker_test

// Implementation-side tests: the sweep -> stop_intent -> outbox
// path, the operator workspace-revocation seam (blocks tickets and redeems,
// revokes leases) and DrainStatus. The 13 contract tests live in
// activity_test.go; these exercise the production plumbing around them.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/broker"
	"github.com/tinyorbitvn/tinycdi/internal/store"
)

// staticRunning is a RunningSource returning a fixed list.
type staticRunning []broker.RunningWorkspace

func (s staticRunning) RunningWorkspaces(context.Context) ([]broker.RunningWorkspace, error) {
	return s, nil
}

// markRunning flips a seeded workspace row into its "serving generation N"
// state — the row shape emitStop acts on.
func markRunning(t *testing.T, db *store.DB, wsUID string, gen uint64) {
	t.Helper()
	_, err := db.Pool().Exec(context.Background(),
		`UPDATE workspaces SET desired_state='Running', runtime_generation=$2, phase='Ready' WHERE id=$1`,
		wsUID, int64(gen))
	if err != nil {
		t.Fatalf("mark running: %v", err)
	}
}

// TestSweep_ExpiryEmitsOutboxStop: a crossed idle deadline lands as a
// provisioning stop intent — never a direct CR mutation — with the row
// flipped to Stopped in the same transaction.
func TestSweep_ExpiryEmitsOutboxStop(t *testing.T) {
	db, b, clock, src := setup(t)
	p := broker.NewExpiryPlanner(b)
	pol := broker.DefaultTimeoutPolicy

	seedWorkspace(t, db, "tenant-a", alice.Owner(), "ws-1")
	markRunning(t, db, "ws-1", 1)
	src.set(readyBinding("ws-1", "tenant-a", alice.Owner(), 1, "rt-1", clock.Now()))
	seedLease(t, db, "lease-ws-1", "ws-1", "tenant-a", alice.Owner(),
		1, "rt-1", 1, gwA.ID, clock.Now().Add(broker.LeaseTTL))
	fence := broker.Fence{WorkspaceUID: "ws-1", RuntimeGeneration: 1, RuntimeUID: "rt-1", FencingVersion: 1}
	if err := b.ReportActivity(ctx, gwA, "lease-ws-1", fence, broker.ActivityEvent{Type: broker.ActivityInput}); err != nil {
		t.Fatalf("ReportActivity: %v", err)
	}
	started := clock.Now()
	clock.Advance(pol.IdleTimeout + time.Second)
	src.set(readyBinding("ws-1", "tenant-a", alice.Owner(), 1, "rt-1", clock.Now())) // fresh observation

	n, err := p.Sweep(ctx, staticRunning{running("ws-1", 1, started, pol)})
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if n != 1 {
		t.Fatalf("emitted %d intents, want 1", n)
	}
	var kind, desired, phase, genStr string
	err = db.Pool().QueryRow(ctx, `
		SELECT o.kind, o.payload->>'runtimeGeneration', w.desired_state, w.phase
		FROM outbox_intent o JOIN workspaces w ON w.id = o.workspace_id
		WHERE o.workspace_id = 'ws-1'`).Scan(&kind, &genStr, &desired, &phase)
	if err != nil {
		t.Fatalf("outbox lookup: %v", err)
	}
	if kind != "stop" || genStr != "1" || desired != "Stopped" || phase != "Stopping" {
		t.Fatalf("stop emission = kind:%q gen:%s desired:%q phase:%q", kind, genStr, desired, phase)
	}

	// A second sweep must not re-emit: intents drain once, and the
	// workspace is already past Running.
	n, err = p.Sweep(ctx, staticRunning{running("ws-1", 1, started, pol)})
	if err != nil {
		t.Fatalf("Sweep(2): %v", err)
	}
	if n != 0 {
		t.Fatalf("second sweep emitted %d intents, want 0", n)
	}
}

// TestSweep_StaleGenerationNeverEmits: an intent recorded for generation 1
// must not stop the workspace after it restarted to generation 2 — the
// emission is fenced on the workspaces row's generation. (Generation 2 is
// young — still inside every deadline — so only the stale intent drains.)
func TestSweep_StaleGenerationNeverEmits(t *testing.T) {
	db, b, clock, src := setup(t)
	p := broker.NewExpiryPlanner(b)

	seedWorkspace(t, db, "tenant-a", alice.Owner(), "ws-1")
	markRunning(t, db, "ws-1", 1)
	src.set(readyBinding("ws-1", "tenant-a", alice.Owner(), 1, "rt-1", clock.Now()))
	if err := b.RequestStop(ctx, "ws-1", 1, broker.StopReasonIdleTimeout); err != nil {
		t.Fatalf("RequestStop: %v", err)
	}

	// The workspace restarts to generation 2 before the sweep drains.
	markRunning(t, db, "ws-1", 2)
	src.set(readyBinding("ws-1", "tenant-a", alice.Owner(), 2, "rt-2", clock.Now()))

	n, err := p.Sweep(ctx, staticRunning{running("ws-1", 2, clock.Now(), broker.DefaultTimeoutPolicy)})
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if n != 0 {
		t.Fatalf("stale intent emitted %d times", n)
	}
	var desired string
	if err := db.Pool().QueryRow(ctx,
		`SELECT desired_state FROM workspaces WHERE id='ws-1'`).Scan(&desired); err != nil {
		t.Fatalf("workspace lookup: %v", err)
	}
	if desired != "Running" {
		t.Fatalf("stale intent stopped the new generation: desired=%q", desired)
	}
	var pending int
	if err := db.Pool().QueryRow(ctx,
		`SELECT COUNT(*) FROM outbox_intent WHERE workspace_id='ws-1' AND kind='stop'`).Scan(&pending); err != nil {
		t.Fatalf("outbox count: %v", err)
	}
	if pending != 0 {
		t.Fatalf("stale intent reached the outbox")
	}
}

// TestSweep_CancelBeforeEmitKeepsStop: a leader loss between listing the
// pending stop intents and emitting them must not consume the rows — the
// old drain-first order marked them drained before emit, so a crash in
// that window lost the stop for good ((workspace, generation, reason) is
// unique, so it could never be re-recorded). The intent now drains inside
// the emit transaction, so the row stays pending and the next pass
// delivers it (backlog 11).
func TestSweep_CancelBeforeEmitKeepsStop(t *testing.T) {
	db, b, clock, src := setup(t)
	seedWorkspace(t, db, "tenant-a", alice.Owner(), "ws-1")
	markRunning(t, db, "ws-1", 1)
	src.set(readyBinding("ws-1", "tenant-a", alice.Owner(), 1, "rt-1", clock.Now()))
	if err := b.RequestStop(ctx, "ws-1", 1, broker.StopReasonIdleTimeout); err != nil {
		t.Fatalf("RequestStop: %v", err)
	}

	// Cancel the sweep ctx after the pending stops are listed, before
	// they are emitted — the crash window.
	sweepCtx, cancel := context.WithCancel(ctx)
	p := broker.NewExpiryPlanner(b, broker.WithAfterPendingHook(cancel))
	if _, err := p.Sweep(sweepCtx,
		staticRunning{running("ws-1", 1, clock.Now(), broker.DefaultTimeoutPolicy)}); err == nil {
		t.Fatal("cancelled sweep should fail at emit")
	}

	// The recorded stop is still pending and the workspace still runs.
	pending, err := p.PendingStops(ctx)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending after cancelled sweep = %+v err %v, want the intent retained", pending, err)
	}
	var desired string
	if err := db.Pool().QueryRow(ctx,
		`SELECT desired_state FROM workspaces WHERE id='ws-1'`).Scan(&desired); err != nil {
		t.Fatalf("workspace lookup: %v", err)
	}
	if desired != "Running" {
		t.Fatalf("cancelled sweep still stopped the workspace: desired=%q", desired)
	}
	var emitted int
	if err := db.Pool().QueryRow(ctx,
		`SELECT COUNT(*) FROM outbox_intent WHERE workspace_id='ws-1'`).Scan(&emitted); err != nil {
		t.Fatalf("outbox count: %v", err)
	}
	if emitted != 0 {
		t.Fatalf("cancelled sweep appended %d outbox intents, want 0", emitted)
	}

	// The next pass delivers it.
	n, err := p.Sweep(ctx,
		staticRunning{running("ws-1", 1, clock.Now(), broker.DefaultTimeoutPolicy)})
	if err != nil {
		t.Fatalf("second Sweep: %v", err)
	}
	if n != 1 {
		t.Fatalf("second sweep emitted %d intents, want 1", n)
	}
	if err := db.Pool().QueryRow(ctx,
		`SELECT desired_state FROM workspaces WHERE id='ws-1'`).Scan(&desired); err != nil {
		t.Fatalf("workspace lookup: %v", err)
	}
	if desired != "Stopped" {
		t.Fatalf("retried sweep did not stop the workspace: desired=%q", desired)
	}
	if pending, err = p.PendingStops(ctx); err != nil || len(pending) != 0 {
		t.Fatalf("pending after delivery = %+v err %v, want drained", pending, err)
	}
}

// TestRevokeWorkspaceLeases_BlocksTicketsAndRedeems: the operator revoke
// kills live leases, blocks new issues for covered generations and refuses
// outstanding ticket redemption — while a NEWER generation reconnects.
func TestRevokeWorkspaceLeases_BlocksTicketsAndRedeems(t *testing.T) {
	db, b, clock, src := setup(t)
	seedWorkspace(t, db, "tenant-a", alice.Owner(), "ws-1")
	markRunning(t, db, "ws-1", 5)
	src.set(readyBinding("ws-1", "tenant-a", alice.Owner(), 5, "rt-5", clock.Now()))

	tk, err := b.IssueTicket(ctx, alice, "ws-1", false, "", "")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	n, err := b.RevokeWorkspaceLeases(ctx, "ws-1", 5)
	if err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if n != 0 {
		t.Fatalf("revoked %d leases, want 0 (none redeemed yet)", n)
	}

	// Outstanding ticket: consumed but no lease minted.
	if _, err := b.RedeemTicket(ctx, gwA, tk.Token); !errors.Is(err, broker.ErrRevoked) {
		t.Fatalf("redeem under revocation = %v, want ErrRevoked", err)
	}
	// New issues for the revoked generation are denied.
	if _, err := b.IssueTicket(ctx, alice, "ws-1", false, "", ""); !errors.Is(err, broker.ErrDenied) {
		t.Fatalf("issue under revocation = %v, want ErrDenied", err)
	}

	// A restarted generation is NOT covered: it reconnects normally.
	markRunning(t, db, "ws-1", 6)
	src.set(readyBinding("ws-1", "tenant-a", alice.Owner(), 6, "rt-6", clock.Now()))
	if _, err := b.IssueTicket(ctx, alice, "ws-1", false, "", ""); err != nil {
		t.Fatalf("issue for newer generation = %v, want nil", err)
	}
}

// TestRevokeWorkspaceLeases_RevokesLiveLeases: leases pinned to covered
// generations die; a newer-generation lease survives.
func TestRevokeWorkspaceLeases_RevokesLiveLeases(t *testing.T) {
	db, b, clock, src := setup(t)
	seedWorkspace(t, db, "tenant-a", alice.Owner(), "ws-1")
	markRunning(t, db, "ws-1", 3)
	src.set(readyBinding("ws-1", "tenant-a", alice.Owner(), 3, "rt-3", clock.Now()))
	seedLease(t, db, "lease-g3", "ws-1", "tenant-a", alice.Owner(),
		3, "rt-3", 1, gwA.ID, clock.Now().Add(broker.LeaseTTL))

	n, err := b.RevokeWorkspaceLeases(ctx, "ws-1", 3)
	if err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if n != 1 {
		t.Fatalf("revoked %d leases, want 1", n)
	}
	var state string
	if err := db.Pool().QueryRow(ctx,
		`SELECT state FROM connection_lease WHERE id='lease-g3'`).Scan(&state); err != nil {
		t.Fatalf("lease lookup: %v", err)
	}
	if state != "revoked" {
		t.Fatalf("lease state = %q, want revoked", state)
	}
}

// TestDrainStatus_TracksStreamEvents: connected/disconnect events feed the
// operator drain view.
func TestDrainStatus_TracksStreamEvents(t *testing.T) {
	db, b, clock, src := setup(t)
	seedWorkspace(t, db, "tenant-a", alice.Owner(), "ws-1")
	src.set(readyBinding("ws-1", "tenant-a", alice.Owner(), 1, "rt-1", clock.Now()))
	seedLease(t, db, "lease-ws-1", "ws-1", "tenant-a", alice.Owner(),
		1, "rt-1", 1, gwA.ID, clock.Now().Add(broker.LeaseTTL))
	fence := broker.Fence{WorkspaceUID: "ws-1", RuntimeGeneration: 1, RuntimeUID: "rt-1", FencingVersion: 1}

	open, drained, err := b.DrainStatus(ctx, "ws-1")
	if err != nil || open != 0 || !drained {
		t.Fatalf("initial drain = %d,%v,%v", open, drained, err)
	}
	if err := b.ReportActivity(ctx, gwA, "lease-ws-1", fence,
		broker.ActivityEvent{Type: broker.ActivityConnected}); err != nil {
		t.Fatalf("connected: %v", err)
	}
	open, drained, err = b.DrainStatus(ctx, "ws-1")
	if err != nil || open != 1 || drained {
		t.Fatalf("after connect drain = %d,%v,%v", open, drained, err)
	}
	if err := b.ReportActivity(ctx, gwA, "lease-ws-1", fence,
		broker.ActivityEvent{Type: broker.ActivityDisconnect}); err != nil {
		t.Fatalf("disconnect: %v", err)
	}
	open, drained, err = b.DrainStatus(ctx, "ws-1")
	if err != nil || open != 0 || !drained {
		t.Fatalf("after disconnect drain = %d,%v,%v", open, drained, err)
	}
}

// --- Drain accounting ---------------------------------------------------------
//
// Defect: delete-while-streaming always burned the full 45 s drain budget
// because the gateway's disconnect report can never land once its lease is
// revoked (ReportActivity -> ErrLeaseInvalid), leaving open_streams = 1
// forever. Fix: revocation is authoritative — revoking a lease closes its
// streams for drain purposes in the same transaction, and DrainStatus never
// counts generations covered by a workspace revocation.

// TestDrainStatus_WorkspaceRevokeClosesStreams: RevokeWorkspaceLeases marks
// the revoked generation's open streams closed atomically — DrainStatus is
// drained immediately, with no gateway disconnect report needed. A late
// disconnect against the dead lease is refused but stays harmless.
func TestDrainStatus_WorkspaceRevokeClosesStreams(t *testing.T) {
	db, b, clock, src := setup(t)
	leaseID, fence := seedActivityWorkspace(t, db, src, clock, "ws-1", 3, "rt-3", 1)

	if err := b.ReportActivity(ctx, gwA, leaseID, fence,
		broker.ActivityEvent{Type: broker.ActivityConnected}); err != nil {
		t.Fatalf("connected: %v", err)
	}
	open, drained, err := b.DrainStatus(ctx, "ws-1")
	if err != nil || open != 1 || drained {
		t.Fatalf("pre-revoke drain = %d,%v,%v want 1,false", open, drained, err)
	}

	n, err := b.RevokeWorkspaceLeases(ctx, "ws-1", 3)
	if err != nil || n != 1 {
		t.Fatalf("revoke: n=%d err=%v want 1,nil", n, err)
	}
	// The killSession close-drop race means no disconnect ever arrives:
	// revocation alone must close the accounting.
	open, drained, err = b.DrainStatus(ctx, "ws-1")
	if err != nil || open != 0 || !drained {
		t.Fatalf("post-revoke drain = %d,%v,%v want 0,true", open, drained, err)
	}

	// A disconnect report on the dead lease is refused (ErrLeaseInvalid)
	// and must not disturb the closed accounting.
	if err := b.ReportActivity(ctx, gwA, leaseID, fence,
		broker.ActivityEvent{Type: broker.ActivityDisconnect}); !errors.Is(err, broker.ErrLeaseInvalid) {
		t.Fatalf("disconnect on revoked lease = %v, want ErrLeaseInvalid", err)
	}
	open, drained, err = b.DrainStatus(ctx, "ws-1")
	if err != nil || open != 0 || !drained {
		t.Fatalf("after refused disconnect drain = %d,%v,%v want 0,true", open, drained, err)
	}
}

// TestDrainStatus_RevokeKeepsNewerGenerationStreams: revocation covers only
// generations <= N — an open stream on a newer (surviving) generation must
// keep the workspace NOT drained: revocation is authoritative, not a
// blanket zero.
func TestDrainStatus_RevokeKeepsNewerGenerationStreams(t *testing.T) {
	db, b, clock, src := setup(t)
	leaseID, fence := seedActivityWorkspace(t, db, src, clock, "ws-1", 3, "rt-3", 1)
	if err := b.ReportActivity(ctx, gwA, leaseID, fence,
		broker.ActivityEvent{Type: broker.ActivityConnected}); err != nil {
		t.Fatalf("gen3 connected: %v", err)
	}

	if _, err := b.RevokeWorkspaceLeases(ctx, "ws-1", 3); err != nil {
		t.Fatalf("revoke gen<=3: %v", err)
	}
	if _, drained, err := b.DrainStatus(ctx, "ws-1"); err != nil || !drained {
		t.Fatalf("post-revoke drain: %v (want drained)", err)
	}

	// The runtime restarts to generation 4 and a fresh lease reports a
	// stream: revocation must not mask it.
	markRunning(t, db, "ws-1", 4)
	src.set(readyBinding("ws-1", "tenant-a", alice.Owner(), 4, "rt-4", clock.Now()))
	seedLease(t, db, "lease-g4", "ws-1", "tenant-a", alice.Owner(),
		4, "rt-4", 1, gwA.ID, clock.Now().Add(broker.LeaseTTL))
	fence4 := broker.Fence{WorkspaceUID: "ws-1", RuntimeGeneration: 4, RuntimeUID: "rt-4", FencingVersion: 1}
	if err := b.ReportActivity(ctx, gwA, "lease-g4", fence4,
		broker.ActivityEvent{Type: broker.ActivityConnected}); err != nil {
		t.Fatalf("gen4 connected: %v", err)
	}
	open, drained, err := b.DrainStatus(ctx, "ws-1")
	if err != nil || open != 1 || drained {
		t.Fatalf("gen4 drain = %d,%v,%v want 1,false (newer generation survives)", open, drained, err)
	}
}

// TestDrainStatus_SingleRevokeClosesStreams: the single-lease path
// (control/admin revoke) is authoritative too — its bound generation's
// streams close for drain purposes in the same transaction.
func TestDrainStatus_SingleRevokeClosesStreams(t *testing.T) {
	db, b, clock, src := setup(t)
	leaseID, fence := seedActivityWorkspace(t, db, src, clock, "ws-1", 1, "rt-1", 1)
	if err := b.ReportActivity(ctx, gwA, leaseID, fence,
		broker.ActivityEvent{Type: broker.ActivityConnected}); err != nil {
		t.Fatalf("connected: %v", err)
	}
	if err := b.RevokeLease(ctx, leaseID); err != nil {
		t.Fatalf("RevokeLease: %v", err)
	}
	open, drained, err := b.DrainStatus(ctx, "ws-1")
	if err != nil || open != 0 || !drained {
		t.Fatalf("post-revoke drain = %d,%v,%v want 0,true", open, drained, err)
	}
}

// TestDrainStatus_SupersededRevokeKeepsSuccessorStreams: revoking an
// already-dead lease must not touch a live successor's accounting on the
// same generation — the revoke is a no-op for streams it does not own.
func TestDrainStatus_SupersededRevokeKeepsSuccessorStreams(t *testing.T) {
	db, b, clock, src := setup(t)
	seedWorkspace(t, db, "tenant-a", alice.Owner(), "ws-1")
	src.set(readyBinding("ws-1", "tenant-a", alice.Owner(), 1, "rt-1", clock.Now()))
	// The old lease was superseded by the takeover; the successor is live.
	seedLease(t, db, "lease-old", "ws-1", "tenant-a", alice.Owner(),
		1, "rt-1", 1, gwA.ID, clock.Now().Add(broker.LeaseTTL))
	if _, err := db.Pool().Exec(ctx,
		`UPDATE connection_lease SET state='superseded' WHERE id='lease-old'`); err != nil {
		t.Fatalf("supersede old lease: %v", err)
	}
	seedLease(t, db, "lease-new", "ws-1", "tenant-a", alice.Owner(),
		1, "rt-1", 2, gwA.ID, clock.Now().Add(broker.LeaseTTL))
	fence := broker.Fence{WorkspaceUID: "ws-1", RuntimeGeneration: 1, RuntimeUID: "rt-1", FencingVersion: 2}
	if err := b.ReportActivity(ctx, gwA, "lease-new", fence,
		broker.ActivityEvent{Type: broker.ActivityConnected}); err != nil {
		t.Fatalf("connected: %v", err)
	}

	if err := b.RevokeLease(ctx, "lease-old"); err != nil {
		t.Fatalf("RevokeLease(superseded): %v", err)
	}
	open, drained, err := b.DrainStatus(ctx, "ws-1")
	if err != nil || open != 1 || drained {
		t.Fatalf("drain after superseded revoke = %d,%v,%v want 1,false", open, drained, err)
	}
}
