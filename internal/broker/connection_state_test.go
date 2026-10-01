package broker_test

// Contract tests for Broker.ConnectionState — the passive per-workspace
// connection status the portal polls (P4) — and the input hook that lets the
// API slide the caller's portal idle timer on real desktop activity (D18).

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/broker"
)

// TestConnection_States maps (lease, last_renewed_at, open_streams) to the
// documented state machine: no live lease -> none; last renew > 20 s ago ->
// stale; open streams -> connected; otherwise disconnected.
func TestConnection_States(t *testing.T) {
	const ws = "ws_0000000000000001"
	db, b, clock, src := setup(t)
	seedWorkspace(t, db, "tenant-a", alice.Owner(), ws)
	src.set(readyBinding(ws, "tenant-a", alice.Owner(), 1, "rt-1", clock.Now()))

	// No lease at all -> none.
	st, err := b.ConnectionState(ctx, broker.PlatformID(ws))
	if err != nil || st.State != "none" || st.LeaseActive || st.LastRenewedAt != nil {
		t.Fatalf("no lease: %+v err=%v, want none", st, err)
	}

	lease := leaseFor(t, b, gwA, broker.PlatformID(ws), false)

	// Fresh lease, no streams -> disconnected, lease active.
	st, err = b.ConnectionState(ctx, broker.PlatformID(ws))
	if err != nil || st.State != "disconnected" || !st.LeaseActive || st.LastRenewedAt == nil {
		t.Fatalf("fresh lease no streams: %+v err=%v, want disconnected", st, err)
	}

	// An open stream -> connected.
	if err := b.ReportActivity(ctx, gwA, lease.ID, fenceOf(lease),
		broker.ActivityEvent{Type: broker.ActivityConnected}); err != nil {
		t.Fatalf("report connected: %v", err)
	}
	st, err = b.ConnectionState(ctx, broker.PlatformID(ws))
	if err != nil || st.State != "connected" || !st.LeaseActive {
		t.Fatalf("open stream: %+v err=%v, want connected", st, err)
	}

	// Renewals stopped > 20 s ago but inside the 30 s TTL -> stale.
	clock.Advance(21 * time.Second)
	st, err = b.ConnectionState(ctx, broker.PlatformID(ws))
	if err != nil || st.State != "stale" {
		t.Fatalf("renewal older than 20 s: %+v err=%v, want stale", st, err)
	}

	// Past the lease TTL the lease is dead -> none.
	clock.Advance(10 * time.Second)
	st, err = b.ConnectionState(ctx, broker.PlatformID(ws))
	if err != nil || st.State != "none" || st.LeaseActive {
		t.Fatalf("expired lease: %+v err=%v, want none", st, err)
	}
}

// TestConnection_StaleTakesPrecedenceOverStreams: a stale lease reports
// "stale" even while a stream count lingers — the lease heartbeat, not the
// stream count, is the freshness signal.
func TestConnection_StaleTakesPrecedenceOverStreams(t *testing.T) {
	const ws = "ws_0000000000000002"
	db, b, clock, src := setup(t)
	seedWorkspace(t, db, "tenant-a", alice.Owner(), ws)
	src.set(readyBinding(ws, "tenant-a", alice.Owner(), 1, "rt-1", clock.Now()))
	lease := leaseFor(t, b, gwA, broker.PlatformID(ws), false)
	if err := b.ReportActivity(ctx, gwA, lease.ID, fenceOf(lease),
		broker.ActivityEvent{Type: broker.ActivityConnected}); err != nil {
		t.Fatalf("report connected: %v", err)
	}
	clock.Advance(21 * time.Second)
	st, err := b.ConnectionState(ctx, broker.PlatformID(ws))
	if err != nil || st.State != "stale" {
		t.Fatalf("open stream on stale lease: %+v err=%v, want stale", st, err)
	}

	// A renewal refreshes last_renewed_at: back to connected.
	src.set(readyBinding(ws, "tenant-a", alice.Owner(), 1, "rt-1", clock.Now()))
	if _, err := b.RenewLease(ctx, gwA, lease.ID, fenceOf(lease)); err != nil {
		t.Fatalf("renew: %v", err)
	}
	st, err = b.ConnectionState(ctx, broker.PlatformID(ws))
	if err != nil || st.State != "connected" {
		t.Fatalf("after renew: %+v err=%v, want connected", st, err)
	}
}

// TestInputHook_CalledWithPrincipal: each recorded "input" event invokes the
// hook with the lease's principal (iss|sub); connected/disconnect and
// rejected events do not.
func TestInputHook_CalledWithPrincipal(t *testing.T) {
	const ws = "ws_0000000000000003"
	db, _, clock, src := setup(t)
	var (
		mu  sync.Mutex
		got []string
	)
	b := broker.New(db, src, broker.WithClock(clock),
		broker.WithInputHook(func(_ context.Context, principal string) {
			mu.Lock()
			got = append(got, principal)
			mu.Unlock()
		}))
	seedWorkspace(t, db, "tenant-a", alice.Owner(), ws)
	src.set(readyBinding(ws, "tenant-a", alice.Owner(), 1, "rt-1", clock.Now()))
	lease := leaseFor(t, b, gwA, broker.PlatformID(ws), false)

	if err := b.ReportActivity(ctx, gwA, lease.ID, fenceOf(lease), input()); err != nil {
		t.Fatalf("report input: %v", err)
	}
	if err := b.ReportActivity(ctx, gwA, lease.ID, fenceOf(lease),
		broker.ActivityEvent{Type: broker.ActivityConnected}); err != nil {
		t.Fatalf("report connected: %v", err)
	}
	if err := b.ReportActivity(ctx, gwA, lease.ID, fenceOf(lease),
		broker.ActivityEvent{Type: broker.ActivityDisconnect}); err != nil {
		t.Fatalf("report disconnect: %v", err)
	}
	if err := b.ReportActivity(ctx, gwA, lease.ID, fenceOf(lease), input()); err != nil {
		t.Fatalf("report second input: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 || got[0] != alice.Owner() || got[1] != alice.Owner() {
		t.Fatalf("hook calls = %v, want two calls with %q", got, alice.Owner())
	}
}
