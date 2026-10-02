package broker_test

// Contract tests for Broker.ConnectionState — the passive per-workspace
// connection status the portal polls (P4) — and the input hook that lets the
// API slide the caller's portal idle timer on real desktop activity (D18).

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

// TestConnection_LeaseRefChangesOnTakeover (R9a): a tab tells WHICH lease it
// is looking at by leaseRef — the first 16 hex chars of SHA-256 of the active
// lease ID. It is stable while the lease lives, changes when another
// principal-session takes the lease over, is absent without a lease, and is
// never the lease ID itself.
func TestConnection_LeaseRefChangesOnTakeover(t *testing.T) {
	const ws = "ws_0000000000000004"
	db, b, clock, src := setup(t)
	seedWorkspace(t, db, "tenant-a", alice.Owner(), ws)
	src.set(readyBinding(ws, "tenant-a", alice.Owner(), 1, "rt-1", clock.Now()))

	st, err := b.ConnectionState(ctx, broker.PlatformID(ws))
	if err != nil || st.LeaseRef != "" {
		t.Fatalf("no lease: %+v err=%v, want empty LeaseRef", st, err)
	}

	first := leaseFor(t, b, gwA, broker.PlatformID(ws), false)
	st1, err := b.ConnectionState(ctx, broker.PlatformID(ws))
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	sum := sha256.Sum256([]byte(first.ID))
	if want := hex.EncodeToString(sum[:])[:16]; st1.LeaseRef != want {
		t.Fatalf("LeaseRef = %q, want %q (first 16 hex of sha256(lease id))", st1.LeaseRef, want)
	}
	if st1.LeaseRef == first.ID || len(st1.LeaseRef) != 16 {
		t.Fatalf("LeaseRef %q must not be the lease ID %q", st1.LeaseRef, first.ID)
	}
	if again, _ := b.ConnectionState(ctx, broker.PlatformID(ws)); again.LeaseRef != st1.LeaseRef {
		t.Fatalf("LeaseRef not stable across reads: %q vs %q", again.LeaseRef, st1.LeaseRef)
	}

	second := leaseFor(t, b, gwA, broker.PlatformID(ws), true)
	if second.ID == first.ID {
		t.Fatal("takeover reused the lease ID")
	}
	st2, err := b.ConnectionState(ctx, broker.PlatformID(ws))
	if err != nil || st2.LeaseRef == "" || st2.LeaseRef == st1.LeaseRef {
		t.Fatalf("after takeover LeaseRef = %q (err=%v), want a new ref != %q", st2.LeaseRef, err, st1.LeaseRef)
	}
}

// TestConnection_StreamEpochAdvancesOnNewStream (R9a): streamEpoch is the
// lease's stream_epoch — it advances each time a stream is claimed on the
// same lease, so a duplicated tab with the same cookie is distinguishable.
func TestConnection_StreamEpochAdvancesOnNewStream(t *testing.T) {
	const ws = "ws_0000000000000005"
	db, b, clock, src := setup(t)
	seedWorkspace(t, db, "tenant-a", alice.Owner(), ws)
	src.set(readyBinding(ws, "tenant-a", alice.Owner(), 1, "rt-1", clock.Now()))
	lease := leaseFor(t, b, gwA, broker.PlatformID(ws), false)

	st, err := b.ConnectionState(ctx, broker.PlatformID(ws))
	if err != nil || st.StreamEpoch != 0 {
		t.Fatalf("fresh lease: %+v err=%v, want StreamEpoch 0", st, err)
	}
	ref := st.LeaseRef
	for want := uint64(1); want <= 2; want++ {
		got, err := b.ClaimStream(ctx, gwA, lease.ID, fenceOf(lease))
		if err != nil || got != want {
			t.Fatalf("ClaimStream = %d, %v; want %d", got, err, want)
		}
		st, err = b.ConnectionState(ctx, broker.PlatformID(ws))
		if err != nil || st.StreamEpoch != want {
			t.Fatalf("after claim %d: %+v err=%v, want StreamEpoch %d", want, st, err, want)
		}
		if st.LeaseRef != ref {
			t.Fatalf("a new stream on the same lease changed LeaseRef: %q -> %q", ref, st.LeaseRef)
		}
	}
}
