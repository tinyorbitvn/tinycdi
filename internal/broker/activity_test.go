package broker_test

// Contract tests for activity reporting and the expiry planner
// (design §8). These exercise ReportActivity, the ExpiryPlanner and
// RequestStop on the fake clock.
//
// Covered:
//   - 30 min no-input idle -> stop intent (idle_timeout);
//   - 10 min disconnect grace -> stop intent (disconnect_timeout);
//   - 8 h max duration -> stop intent (max_duration), input cannot extend it;
//   - WebSocket pings / video frames / open sockets never reset idle;
//   - reconnect inside grace cancels the disconnect timer;
//   - stale activity (old generation/runtimeUID/fencing version) can neither
//     keep alive nor stop a newer runtime;
//   - RequestStop re-checks generation: a stale generation never stops.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/broker"
	"github.com/tinyorbitvn/tinycdi/internal/store"
)

// seedLease inserts an active connection lease directly — ticket issuance is
// a different stubbed surface, so activity tests do not depend on it.
func seedLease(t *testing.T, db *store.DB, leaseID, wsUID, tenantID, subj string,
	gen uint64, rtUID string, fencing uint64, gwID string, expiresAt time.Time) {
	t.Helper()
	_, err := db.Pool().Exec(context.Background(), `
		INSERT INTO connection_lease
			(id, workspace_id, tenant_id, principal_subject, runtime_generation,
			 runtime_uid, fencing_version, gateway_id, state, expires_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'active',$9)`,
		leaseID, wsUID, tenantID, subj, gen, rtUID, fencing, gwID, expiresAt)
	if err != nil {
		t.Fatalf("seed lease: %v", err)
	}
}

// seedActivityWorkspace plants a workspace row plus an active lease and a
// Ready binding, then returns the fence ReportActivity must present.
func seedActivityWorkspace(t *testing.T, db *store.DB, src *fakeBindings, clock *fakeClock,
	wsUID string, gen uint64, rtUID string, fencing uint64) (leaseID string, fence broker.Fence) {
	t.Helper()
	seedWorkspace(t, db, "tenant-a", alice.Owner(), wsUID)
	src.set(readyBinding(broker.PlatformID(wsUID), "tenant-a", alice.Owner(), gen, rtUID, clock.Now()))
	leaseID = "lease-" + wsUID
	seedLease(t, db, leaseID, wsUID, "tenant-a", alice.Owner(),
		gen, rtUID, fencing, gwA.ID, clock.Now().Add(broker.LeaseTTL))
	return leaseID, broker.Fence{
		WorkspaceUID:      wsUID,
		RuntimeGeneration: gen,
		RuntimeUID:        rtUID,
		FencingVersion:    fencing,
	}
}

func input() broker.ActivityEvent { return broker.ActivityEvent{Type: broker.ActivityInput} }

func running(wsUID string, gen uint64, startedAt time.Time, pol broker.TimeoutPolicy) broker.RunningWorkspace {
	return broker.RunningWorkspace{WorkspaceUID: broker.PlatformID(wsUID), RuntimeGeneration: gen, StartedAt: startedAt, Policy: pol}
}

func wantIntents(t *testing.T, got []broker.StopIntent, want []broker.StopIntent) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("intents = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i].WorkspaceUID != want[i].WorkspaceUID ||
			got[i].RuntimeGeneration != want[i].RuntimeGeneration ||
			got[i].Reason != want[i].Reason {
			t.Fatalf("intent[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestActivity_InputIdleStop: 30 min without input produces an
// idle_timeout stop intent pinned to the generation.
func TestActivity_InputIdleStop(t *testing.T) {
	db, b, clock, src := setup(t)
	p := broker.NewExpiryPlanner(b)
	pol := broker.DefaultTimeoutPolicy

	leaseID, fence := seedActivityWorkspace(t, db, src, clock, "ws-1", 1, "rt-1", 1)
	if err := b.ReportActivity(ctx, gwA, leaseID, fence, input()); err != nil {
		t.Fatalf("ReportActivity: %v", err)
	}

	run := []broker.RunningWorkspace{running("ws-1", 1, clock.Now(), pol)}

	clock.Advance(pol.IdleTimeout - time.Second)
	got, err := p.Scan(ctx, run)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("stop emitted %v before the idle deadline", got)
	}

	clock.Advance(time.Second)
	got, err = p.Scan(ctx, run)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	wantIntents(t, got, []broker.StopIntent{{
		WorkspaceUID: "ws-1", RuntimeGeneration: 1, Reason: broker.StopReasonIdleTimeout,
	}})
}

// TestActivity_RecentInputKeepsAlive: input inside the window holds the
// idle deadline off — each input re-anchors it.
func TestActivity_RecentInputKeepsAlive(t *testing.T) {
	db, b, clock, src := setup(t)
	p := broker.NewExpiryPlanner(b)
	pol := broker.DefaultTimeoutPolicy

	leaseID, fence := seedActivityWorkspace(t, db, src, clock, "ws-1", 1, "rt-1", 1)
	if err := b.ReportActivity(ctx, gwA, leaseID, fence, input()); err != nil {
		t.Fatalf("ReportActivity: %v", err)
	}
	started := clock.Now()

	// Input every 10 minutes: idle never fires while under MaxDuration.
	for i := 0; i < 2; i++ {
		clock.Advance(10 * time.Minute)
		if err := b.ReportActivity(ctx, gwA, leaseID, fence, input()); err != nil {
			t.Fatalf("ReportActivity: %v", err)
		}
	}
	clock.Advance(pol.IdleTimeout - time.Second)
	got, err := p.Scan(ctx, []broker.RunningWorkspace{running("ws-1", 1, started, pol)})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("idle stop fired despite input 1s ago: %v", got)
	}
}

// TestActivity_DisconnectStop: after the last stream closes, the workspace
// gets DisconnectTimeout of grace then a disconnect_timeout intent.
func TestActivity_DisconnectStop(t *testing.T) {
	db, b, clock, src := setup(t)
	p := broker.NewExpiryPlanner(b)
	pol := broker.DefaultTimeoutPolicy

	leaseID, fence := seedActivityWorkspace(t, db, src, clock, "ws-1", 1, "rt-1", 1)
	if err := b.ReportActivity(ctx, gwA, leaseID, fence, input()); err != nil {
		t.Fatalf("ReportActivity(input): %v", err)
	}
	if err := b.ReportActivity(ctx, gwA, leaseID, fence,
		broker.ActivityEvent{Type: broker.ActivityDisconnect}); err != nil {
		t.Fatalf("ReportActivity(disconnect): %v", err)
	}
	run := []broker.RunningWorkspace{running("ws-1", 1, clock.Now(), pol)}

	clock.Advance(pol.DisconnectTimeout - time.Second)
	got, err := p.Scan(ctx, run)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("disconnect stop fired %v inside the grace window", got)
	}

	clock.Advance(time.Second)
	got, err = p.Scan(ctx, run)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	wantIntents(t, got, []broker.StopIntent{{
		WorkspaceUID: "ws-1", RuntimeGeneration: 1, Reason: broker.StopReasonDisconnectTimeout,
	}})
}

// TestActivity_ReconnectCancelsDisconnect: a connected event inside the
// grace window cancels the pending disconnect stop; the window does not
// keep ticking underneath.
func TestActivity_ReconnectCancelsDisconnect(t *testing.T) {
	db, b, clock, src := setup(t)
	p := broker.NewExpiryPlanner(b)
	pol := broker.DefaultTimeoutPolicy

	leaseID, fence := seedActivityWorkspace(t, db, src, clock, "ws-1", 1, "rt-1", 1)
	if err := b.ReportActivity(ctx, gwA, leaseID, fence, input()); err != nil {
		t.Fatalf("ReportActivity(input): %v", err)
	}
	if err := b.ReportActivity(ctx, gwA, leaseID, fence,
		broker.ActivityEvent{Type: broker.ActivityDisconnect}); err != nil {
		t.Fatalf("ReportActivity(disconnect): %v", err)
	}
	clock.Advance(pol.DisconnectTimeout / 2) // still inside grace
	if err := b.ReportActivity(ctx, gwA, leaseID, fence,
		broker.ActivityEvent{Type: broker.ActivityConnected}); err != nil {
		t.Fatalf("ReportActivity(connected): %v", err)
	}
	if err := b.ReportActivity(ctx, gwA, leaseID, fence, input()); err != nil {
		t.Fatalf("ReportActivity(input2): %v", err)
	}

	// Cross the ORIGINAL disconnect deadline — the reconnect cancelled it.
	clock.Advance(pol.DisconnectTimeout)
	run := []broker.RunningWorkspace{running("ws-1", 1, clock.Now(), pol)}
	got, err := p.Scan(ctx, run)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	for _, in := range got {
		if in.Reason == broker.StopReasonDisconnectTimeout {
			t.Fatalf("disconnect stop fired after in-grace reconnect: %+v", in)
		}
	}
}

// TestActivity_PingsFramesDoNotResetIdle: WebSocket pings, video frames and
// an open socket are not user activity — they are rejected outright and
// never move the input-idle deadline (design §8).
func TestActivity_PingsFramesDoNotResetIdle(t *testing.T) {
	db, b, clock, src := setup(t)
	p := broker.NewExpiryPlanner(b)
	pol := broker.DefaultTimeoutPolicy

	leaseID, fence := seedActivityWorkspace(t, db, src, clock, "ws-1", 1, "rt-1", 1)
	if err := b.ReportActivity(ctx, gwA, leaseID, fence, input()); err != nil {
		t.Fatalf("ReportActivity(input): %v", err)
	}

	clock.Advance(pol.IdleTimeout / 2)
	for _, evType := range []broker.ActivityEventType{"ws_ping", "video_frame", "frame", "keepalive"} {
		err := b.ReportActivity(ctx, gwA, leaseID, fence, broker.ActivityEvent{Type: evType})
		if !errors.Is(err, broker.ErrActivityType) {
			t.Fatalf("ReportActivity(%q) = %v, want ErrActivityType", evType, err)
		}
	}
	// A connected event is valid but is also not input — it must not reset
	// the input-idle clock either.
	if err := b.ReportActivity(ctx, gwA, leaseID, fence,
		broker.ActivityEvent{Type: broker.ActivityConnected}); err != nil {
		t.Fatalf("ReportActivity(connected): %v", err)
	}

	clock.Advance(pol.IdleTimeout/2 + time.Second)
	got, err := p.Scan(ctx, []broker.RunningWorkspace{running("ws-1", 1, clock.Now(), pol)})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	wantIntents(t, got, []broker.StopIntent{{
		WorkspaceUID: "ws-1", RuntimeGeneration: 1, Reason: broker.StopReasonIdleTimeout,
	}})
}

// TestActivity_MaxDurationCapsSession: continuous input holds off idle but
// the 8 h absolute cap still fires — input never extends MaxDuration.
func TestActivity_MaxDurationCapsSession(t *testing.T) {
	db, b, clock, src := setup(t)
	p := broker.NewExpiryPlanner(b)
	pol := broker.DefaultTimeoutPolicy

	leaseID, fence := seedActivityWorkspace(t, db, src, clock, "ws-1", 1, "rt-1", 1)
	started := clock.Now()

	// Keep typing for the whole cap window.
	for clock.Now().Before(started.Add(pol.MaxDuration - time.Second)) {
		if err := b.ReportActivity(ctx, gwA, leaseID, fence, input()); err != nil {
			t.Fatalf("ReportActivity: %v", err)
		}
		clock.Advance(5 * time.Minute)
	}
	if err := b.ReportActivity(ctx, gwA, leaseID, fence, input()); err != nil {
		t.Fatalf("ReportActivity: %v", err)
	}
	clock.Advance(time.Second) // now > started + 8h

	got, err := p.Scan(ctx, []broker.RunningWorkspace{running("ws-1", 1, started, pol)})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	wantIntents(t, got, []broker.StopIntent{{
		WorkspaceUID: "ws-1", RuntimeGeneration: 1, Reason: broker.StopReasonMaxDuration,
	}})
}

// TestActivity_ClientTimestampNeverTrusted: an event carrying a forged
// client-side ReceivedAt is re-anchored to the server clock — a client can
// never backdate or postdate the idle window.
func TestActivity_ClientTimestampNeverTrusted(t *testing.T) {
	db, b, clock, src := setup(t)
	p := broker.NewExpiryPlanner(b)
	pol := broker.DefaultTimeoutPolicy

	leaseID, fence := seedActivityWorkspace(t, db, src, clock, "ws-1", 1, "rt-1", 1)
	// Client claims this input happened 45 min ago; the server must anchor
	// it to receipt time, so idle fires a full 30 min from NOW — not -15m.
	forged := broker.ActivityEvent{Type: broker.ActivityInput, ReceivedAt: clock.Now().Add(-45 * time.Minute)}
	if err := b.ReportActivity(ctx, gwA, leaseID, fence, forged); err != nil {
		t.Fatalf("ReportActivity: %v", err)
	}
	run := []broker.RunningWorkspace{running("ws-1", 1, clock.Now(), pol)}

	got, err := p.Scan(ctx, run)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("forged client timestamp backdated idle: %v", got)
	}
	clock.Advance(pol.IdleTimeout - time.Second)
	got, err = p.Scan(ctx, run)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("still early for server-anchored idle: %v", got)
	}
	clock.Advance(time.Second)
	got, err = p.Scan(ctx, run)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	wantIntents(t, got, []broker.StopIntent{{
		WorkspaceUID: "ws-1", RuntimeGeneration: 1, Reason: broker.StopReasonIdleTimeout,
	}})
}

// TestActivity_StaleFenceRejected: activity presented with an old
// generation, dead runtimeUID or superseded fencing version is refused —
// a stale incarnation can neither keep alive nor stop a newer runtime.
func TestActivity_StaleFenceRejected(t *testing.T) {
	db, b, clock, src := setup(t)
	p := broker.NewExpiryPlanner(b)
	pol := broker.DefaultTimeoutPolicy

	// Workspace is now on generation 2 / rt-2 after a restart.
	seedWorkspace(t, db, "tenant-a", alice.Owner(), "ws-1")
	src.set(readyBinding("ws-1", "tenant-a", alice.Owner(), 2, "rt-2", clock.Now()))

	// The old lease (gen 1 / rt-1 / fencing 1) is what the stale gateway
	// still holds.
	seedLease(t, db, "lease-old", "ws-1", "tenant-a", alice.Owner(),
		1, "rt-1", 1, gwA.ID, clock.Now().Add(broker.LeaseTTL))
	stale := broker.Fence{WorkspaceUID: "ws-1", RuntimeGeneration: 1, RuntimeUID: "rt-1", FencingVersion: 1}

	if err := b.ReportActivity(ctx, gwA, "lease-old", stale, input()); !errors.Is(err, broker.ErrStaleBinding) {
		t.Fatalf("stale-generation activity = %v, want ErrStaleBinding", err)
	}

	// Same-generation-but-dead-incarnation activity is also stale.
	src.set(readyBinding("ws-1", "tenant-a", alice.Owner(), 2, "rt-2b", clock.Now()))
	deadRT := broker.Fence{WorkspaceUID: "ws-1", RuntimeGeneration: 2, RuntimeUID: "rt-2", FencingVersion: 2}
	if err := b.ReportActivity(ctx, gwA, "lease-old", deadRT, input()); !errors.Is(err, broker.ErrStaleBinding) {
		t.Fatalf("stale-runtimeUID activity = %v, want ErrStaleBinding", err)
	}

	// And stale input must not have moved the new generation's idle clock:
	// with no valid activity at all, the planner fires at first deadline.
	clock.Advance(pol.IdleTimeout + time.Second)
	got, err := p.Scan(ctx, []broker.RunningWorkspace{
		running("ws-1", 2, clock.Now(), pol),
	})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	wantIntents(t, got, []broker.StopIntent{{
		WorkspaceUID: "ws-1", RuntimeGeneration: 2, Reason: broker.StopReasonIdleTimeout,
	}})
}

// TestActivity_WrongGatewayDenied: a lease's activity is only accepted from
// the gateway holding it — replicas cannot write for each other.
func TestActivity_WrongGatewayDenied(t *testing.T) {
	db, b, clock, src := setup(t)
	leaseID, fence := seedActivityWorkspace(t, db, src, clock, "ws-1", 1, "rt-1", 1)
	if err := b.ReportActivity(ctx, gwB, leaseID, fence, input()); !errors.Is(err, broker.ErrDenied) {
		t.Fatalf("activity from foreign gateway = %v, want ErrDenied", err)
	}
}

// TestActivity_DeadLeaseRejected: reporting on an expired/revoked lease
// fails — a closed session cannot produce activity.
func TestActivity_DeadLeaseRejected(t *testing.T) {
	db, b, clock, src := setup(t)
	leaseID, fence := seedActivityWorkspace(t, db, src, clock, "ws-1", 1, "rt-1", 1)
	_, err := db.Pool().Exec(context.Background(),
		`UPDATE connection_lease SET state='revoked', closed_at=now() WHERE id=$1`, leaseID)
	if err != nil {
		t.Fatalf("revoke lease: %v", err)
	}
	if err := b.ReportActivity(ctx, gwA, leaseID, fence, input()); !errors.Is(err, broker.ErrLeaseInvalid) {
		t.Fatalf("activity on revoked lease = %v, want ErrLeaseInvalid", err)
	}
}

// TestRequestStop_RejectsStaleGeneration: a delayed expiry for generation 2
// must not stop the runtime after the workspace restarted to generation 3.
func TestRequestStop_RejectsStaleGeneration(t *testing.T) {
	db, b, clock, src := setup(t)
	p := broker.NewExpiryPlanner(b)
	seedWorkspace(t, db, "tenant-a", alice.Owner(), "ws-1")
	src.set(readyBinding("ws-1", "tenant-a", alice.Owner(), 3, "rt-3", clock.Now()))

	err := b.RequestStop(ctx, "ws-1", 2, broker.StopReasonIdleTimeout)
	if !errors.Is(err, broker.ErrStaleBinding) {
		t.Fatalf("RequestStop(gen 2 while gen 3 runs) = %v, want ErrStaleBinding", err)
	}
	pending, err := p.PendingStops(ctx)
	if err != nil {
		t.Fatalf("PendingStops: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("stale RequestStop still recorded an intent: %+v", pending)
	}
}

// TestRequestStop_CurrentGeneration: a stop pinned to the live generation
// is recorded durably for the lifecycle pipeline to drain.
func TestRequestStop_CurrentGeneration(t *testing.T) {
	db, b, clock, src := setup(t)
	p := broker.NewExpiryPlanner(b)
	seedWorkspace(t, db, "tenant-a", alice.Owner(), "ws-1")
	src.set(readyBinding("ws-1", "tenant-a", alice.Owner(), 3, "rt-3", clock.Now()))

	if err := b.RequestStop(ctx, "ws-1", 3, broker.StopReasonMaxDuration); err != nil {
		t.Fatalf("RequestStop: %v", err)
	}
	got, err := p.PendingStops(ctx)
	if err != nil {
		t.Fatalf("PendingStops: %v", err)
	}
	wantIntents(t, got, []broker.StopIntent{{
		WorkspaceUID: "ws-1", RuntimeGeneration: 3, Reason: broker.StopReasonMaxDuration,
	}})
	// The sweep consumes the recorded intent exactly once: emitting it
	// drains the row, so the next listing is empty.
	markRunning(t, db, "ws-1", 3)
	if _, err := p.Sweep(ctx, staticRunning{running("ws-1", 3, clock.Now(), broker.DefaultTimeoutPolicy)}); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	got, err = p.PendingStops(ctx)
	if err != nil {
		t.Fatalf("PendingStops(2): %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("drained intents replayed: %+v", got)
	}
}

// TestScan_NoRecordedActivity: a running workspace that never reported any
// activity is still bounded by its deadlines — silence is not keep-alive.
func TestScan_NoRecordedActivity(t *testing.T) {
	db, b, clock, src := setup(t)
	p := broker.NewExpiryPlanner(b)
	pol := broker.DefaultTimeoutPolicy
	seedWorkspace(t, db, "tenant-a", alice.Owner(), "ws-1")
	src.set(readyBinding("ws-1", "tenant-a", alice.Owner(), 1, "rt-1", clock.Now()))
	started := clock.Now()

	// No activity at all: the idle clock anchors at generation start, so
	// the workspace still stops after IdleTimeout of silence.
	clock.Advance(pol.IdleTimeout + time.Second)
	got, err := p.Scan(ctx, []broker.RunningWorkspace{running("ws-1", 1, started, pol)})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	wantIntents(t, got, []broker.StopIntent{{
		WorkspaceUID: "ws-1", RuntimeGeneration: 1, Reason: broker.StopReasonIdleTimeout,
	}})
}
