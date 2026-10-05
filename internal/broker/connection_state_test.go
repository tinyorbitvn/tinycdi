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
	st, err := b.ConnectionState(ctx, broker.PlatformID(ws), alice.Owner())
	if err != nil || st.State != "none" || st.LeaseActive || st.LastRenewedAt != nil {
		t.Fatalf("no lease: %+v err=%v, want none", st, err)
	}

	lease := leaseFor(t, b, gwA, broker.PlatformID(ws), false)

	// Fresh lease, no streams -> disconnected, lease active.
	st, err = b.ConnectionState(ctx, broker.PlatformID(ws), alice.Owner())
	if err != nil || st.State != "disconnected" || !st.LeaseActive || st.LastRenewedAt == nil {
		t.Fatalf("fresh lease no streams: %+v err=%v, want disconnected", st, err)
	}

	// An open stream -> connected.
	if err := b.ReportActivity(ctx, gwA, lease.ID, fenceOf(lease),
		broker.ActivityEvent{Type: broker.ActivityConnected}); err != nil {
		t.Fatalf("report connected: %v", err)
	}
	st, err = b.ConnectionState(ctx, broker.PlatformID(ws), alice.Owner())
	if err != nil || st.State != "connected" || !st.LeaseActive {
		t.Fatalf("open stream: %+v err=%v, want connected", st, err)
	}

	// Renewals stopped > 20 s ago but inside the 30 s TTL -> stale.
	clock.Advance(21 * time.Second)
	st, err = b.ConnectionState(ctx, broker.PlatformID(ws), alice.Owner())
	if err != nil || st.State != "stale" {
		t.Fatalf("renewal older than 20 s: %+v err=%v, want stale", st, err)
	}

	// Past the lease TTL the lease is dead -> none.
	clock.Advance(10 * time.Second)
	st, err = b.ConnectionState(ctx, broker.PlatformID(ws), alice.Owner())
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
	st, err := b.ConnectionState(ctx, broker.PlatformID(ws), alice.Owner())
	if err != nil || st.State != "stale" {
		t.Fatalf("open stream on stale lease: %+v err=%v, want stale", st, err)
	}

	// A renewal refreshes last_renewed_at: back to connected.
	src.set(readyBinding(ws, "tenant-a", alice.Owner(), 1, "rt-1", clock.Now()))
	if _, err := b.RenewLease(ctx, gwA, lease.ID, fenceOf(lease)); err != nil {
		t.Fatalf("renew: %v", err)
	}
	st, err = b.ConnectionState(ctx, broker.PlatformID(ws), alice.Owner())
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

	st, err := b.ConnectionState(ctx, broker.PlatformID(ws), alice.Owner())
	if err != nil || st.LeaseRef != "" {
		t.Fatalf("no lease: %+v err=%v, want empty LeaseRef", st, err)
	}

	first := leaseFor(t, b, gwA, broker.PlatformID(ws), false)
	st1, err := b.ConnectionState(ctx, broker.PlatformID(ws), alice.Owner())
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
	if again, _ := b.ConnectionState(ctx, broker.PlatformID(ws), alice.Owner()); again.LeaseRef != st1.LeaseRef {
		t.Fatalf("LeaseRef not stable across reads: %q vs %q", again.LeaseRef, st1.LeaseRef)
	}

	second := leaseFor(t, b, gwA, broker.PlatformID(ws), true)
	if second.ID == first.ID {
		t.Fatal("takeover reused the lease ID")
	}
	st2, err := b.ConnectionState(ctx, broker.PlatformID(ws), alice.Owner())
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

	st, err := b.ConnectionState(ctx, broker.PlatformID(ws), alice.Owner())
	if err != nil || st.StreamEpoch != 0 {
		t.Fatalf("fresh lease: %+v err=%v, want StreamEpoch 0", st, err)
	}
	ref := st.LeaseRef
	for want := uint64(1); want <= 2; want++ {
		got, err := b.ClaimStream(ctx, gwA, lease.ID, fenceOf(lease), "")
		if err != nil || got != want {
			t.Fatalf("ClaimStream = %d, %v; want %d", got, err, want)
		}
		st, err = b.ConnectionState(ctx, broker.PlatformID(ws), alice.Owner())
		if err != nil || st.StreamEpoch != want {
			t.Fatalf("after claim %d: %+v err=%v, want StreamEpoch %d", want, st, err, want)
		}
		if st.LeaseRef != ref {
			t.Fatalf("a new stream on the same lease changed LeaseRef: %q -> %q", ref, st.LeaseRef)
		}
	}
}

// TestConnection_StreamOwnerTab (FX-R31): the claim's tab id is stored in
// the same row update as the epoch and reported back ONLY to the lease's
// own principal. Two claims of the same tab both record that id (never a
// false "elsewhere"), a different tab's claim records the other id, a
// claim without an id stores NULL — and a foreign principal never learns
// the value at all.
func TestConnection_StreamOwnerTab(t *testing.T) {
	const ws = "ws_0000000000000006"
	const tabA = "0123456789abcdef0123456789abcdef"
	const tabB = "fedcba9876543210fedcba9876543210"
	const portalSess = "portal-session-1"
	db, b, clock, src := setup(t)
	seedWorkspace(t, db, "tenant-a", alice.Owner(), ws)
	src.set(readyBinding(ws, "tenant-a", alice.Owner(), 1, "rt-1", clock.Now()))
	lease := leaseForSess(t, db, b, gwA, broker.PlatformID(ws), false, portalSess)

	// Two claims of the same tab inside one poll interval: the owner is
	// that tab on both — a restart re-claim stays "ours".
	for want := uint64(1); want <= 2; want++ {
		if _, err := b.ClaimStream(ctx, gwA, lease.ID, fenceOf(lease), tabA); err != nil {
			t.Fatalf("claim %d: %v", want, err)
		}
	}
	st, err := b.ConnectionState(ctx, broker.PlatformID(ws), portalSess)
	if err != nil || st.StreamOwnerTab != tabA || st.StreamEpoch != 2 {
		t.Fatalf("same-tab claims: %+v err=%v, want StreamOwnerTab %q", st, err, tabA)
	}

	// A different portal session of the same user never learns the id —
	// the gate is the lease's portal_session_digest, not the principal.
	st, err = b.ConnectionState(ctx, broker.PlatformID(ws), "portal-session-2")
	if err != nil || st.State == "" || st.StreamOwnerTab != "" {
		t.Fatalf("other session: %+v err=%v, want StreamOwnerTab empty", st, err)
	}

	// A different tab's claim replaces the owner in the same update as the
	// epoch — they can never diverge.
	if _, err := b.ClaimStream(ctx, gwA, lease.ID, fenceOf(lease), tabB); err != nil {
		t.Fatalf("foreign claim: %v", err)
	}
	st, err = b.ConnectionState(ctx, broker.PlatformID(ws), portalSess)
	if err != nil || st.StreamOwnerTab != tabB || st.StreamEpoch != 3 {
		t.Fatalf("foreign claim: %+v err=%v, want StreamOwnerTab %q", st, err, tabB)
	}

	// A legacy claim (no id — or a malformed one) stores NULL in the same
	// update: the row reflects the CURRENT stream, never the last valid id.
	if _, err := b.ClaimStream(ctx, gwA, lease.ID, fenceOf(lease), ""); err != nil {
		t.Fatalf("legacy claim: %v", err)
	}
	st, err = b.ConnectionState(ctx, broker.PlatformID(ws), portalSess)
	if err != nil || st.StreamOwnerTab != "" || st.StreamEpoch != 4 {
		t.Fatalf("legacy claim: %+v err=%v, want StreamOwnerTab empty", st, err)
	}
	if _, err := b.ClaimStream(ctx, gwA, lease.ID, fenceOf(lease), "not-hex!"); err != nil {
		t.Fatalf("malformed claim: %v", err)
	}
	st, err = b.ConnectionState(ctx, broker.PlatformID(ws), portalSess)
	if err != nil || st.StreamOwnerTab != "" || st.StreamEpoch != 5 {
		t.Fatalf("malformed claim: %+v err=%v, want StreamOwnerTab empty", st, err)
	}
}

// TestConnection_StreamOwnerEpochStale (R-V3c): the stored id only names the
// current stream while stream_owner_epoch = stream_epoch. A replica
// predating the columns bumps the epoch without naming them — its claim
// leaves stale evidence that must read as absent, never as a match.
func TestConnection_StreamOwnerEpochStale(t *testing.T) {
	const ws = "ws_0000000000000007"
	const portalSess = "portal-session-1"
	const tabA = "0123456789abcdef0123456789abcdef"
	db, b, clock, src := setup(t)
	seedWorkspace(t, db, "tenant-a", alice.Owner(), ws)
	src.set(readyBinding(ws, "tenant-a", alice.Owner(), 1, "rt-1", clock.Now()))
	lease := leaseForSess(t, db, b, gwA, broker.PlatformID(ws), false, portalSess)

	if _, err := b.ClaimStream(ctx, gwA, lease.ID, fenceOf(lease), tabA); err != nil {
		t.Fatalf("claim: %v", err)
	}
	st, err := b.ConnectionState(ctx, broker.PlatformID(ws), portalSess)
	if err != nil || st.StreamOwnerTab != tabA {
		t.Fatalf("claim: %+v err=%v, want StreamOwnerTab %q", st, err, tabA)
	}

	// An rc.2-era claim: epoch bumps, owner columns untouched.
	if _, err := db.Pool().Exec(ctx, `
		UPDATE connection_lease SET stream_epoch = stream_epoch + 1
		WHERE id = $1`, lease.ID); err != nil {
		t.Fatalf("rc.2-shaped claim: %v", err)
	}
	st, err = b.ConnectionState(ctx, broker.PlatformID(ws), portalSess)
	if err != nil || st.StreamOwnerTab != "" || st.StreamEpoch != 2 {
		t.Fatalf("stale owner: %+v err=%v, want StreamOwnerTab empty", st, err)
	}
}

// TestValidStreamOwnerTab: the stored format is fixed — exactly 32
// lowercase hex chars (128 bits). Anything else is stored as NULL and can
// never match a real tab.
func TestValidStreamOwnerTab(t *testing.T) {
	for _, ok := range []struct {
		in   string
		want bool
	}{
		{"0123456789abcdef0123456789abcdef", true},
		{"", false},
		{"0123456789abcdef0123456789abcde", false},   // 31
		{"0123456789abcdef0123456789abcdef0", false}, // 33
		{"0123456789ABCDEF0123456789abcdef", false},  // uppercase
		{"0123456789abcdef0123456789abcdeg", false},  // non-hex
		{"0123456789abcdef0123456789abcde ", false},  // trailing space
	} {
		if got := broker.ValidStreamOwnerTab(ok.in); got != ok.want {
			t.Fatalf("ValidStreamOwnerTab(%q) = %v, want %v", ok.in, got, ok.want)
		}
	}
}
