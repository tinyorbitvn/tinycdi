package broker_test

import (
	"errors"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/broker"
)

// leaseFor issues and redeems a ticket, returning the live lease.
func leaseFor(t *testing.T, b *broker.Broker, gw broker.GatewayIdentity, wsUID broker.PlatformID, takeover bool) broker.Lease {
	t.Helper()
	tk, err := b.IssueTicket(ctx, alice, wsUID, takeover, "")
	if err != nil {
		t.Fatalf("IssueTicket: %v", err)
	}
	lease, err := b.RedeemTicket(ctx, gw, tk.Token)
	if err != nil {
		t.Fatalf("RedeemTicket: %v", err)
	}
	return lease
}

func fenceOf(l broker.Lease) broker.Fence {
	return broker.Fence{
		WorkspaceUID:      l.WorkspaceUID,
		RuntimeGeneration: l.RuntimeGeneration,
		RuntimeUID:        l.RuntimeUID,
		FencingVersion:    l.FencingVersion,
	}
}

// TestRenewLease_SlidesExpiry: each ~10 s renew moves expiry forward; the
// fence presented must be the one pinned at redemption.
func TestRenewLease_SlidesExpiry(t *testing.T) {
	db, b, clock, src := setup(t)
	seedWorkspace(t, db, "tenant-a", alice.Owner(), "ws-1")
	src.set(readyBinding("ws-1", "tenant-a", alice.Owner(), 1, "rt-1", clock.Now()))

	lease := leaseFor(t, b, gwA, "ws-1", false)
	if got := lease.ExpiresAt.Sub(clock.Now()); got != broker.LeaseTTL {
		t.Fatalf("lease TTL = %v, want %v", got, broker.LeaseTTL)
	}

	clock.Advance(broker.LeaseRenewInterval)
	src.set(readyBinding("ws-1", "tenant-a", alice.Owner(), 1, "rt-1", clock.Now())) // fresh observation
	renewed, err := b.RenewLease(ctx, gwA, lease.ID, fenceOf(lease))
	if err != nil {
		t.Fatalf("RenewLease: %v", err)
	}
	if !renewed.ExpiresAt.After(lease.ExpiresAt) {
		t.Fatalf("renewed expiry %v not after %v — renewal must slide", renewed.ExpiresAt, lease.ExpiresAt)
	}
}

// TestRenewLease_WrongGateway: a lease held by one gateway identity cannot be
// renewed by another — two gateway replicas never share a writer lease.
func TestRenewLease_WrongGateway(t *testing.T) {
	db, b, clock, src := setup(t)
	seedWorkspace(t, db, "tenant-a", alice.Owner(), "ws-1")
	src.set(readyBinding("ws-1", "tenant-a", alice.Owner(), 1, "rt-1", clock.Now()))

	lease := leaseFor(t, b, gwA, "ws-1", false)
	if _, err := b.RenewLease(ctx, gwB, lease.ID, fenceOf(lease)); !errors.Is(err, broker.ErrDenied) {
		t.Fatalf("renew by foreign gateway = %v, want ErrDenied", err)
	}
}

// TestLease_StaleRuntimeGeneration: after a new generation (start/stop cycle)
// the old lease's fence no longer matches — renew and resolve must fail.
func TestLease_StaleRuntimeGeneration(t *testing.T) {
	db, b, clock, src := setup(t)
	seedWorkspace(t, db, "tenant-a", alice.Owner(), "ws-1")
	src.set(readyBinding("ws-1", "tenant-a", alice.Owner(), 2, "rt-2", clock.Now()))

	lease := leaseFor(t, b, gwA, "ws-1", false)
	src.set(readyBinding("ws-1", "tenant-a", alice.Owner(), 3, "rt-3", clock.Now())) // new generation

	if _, err := b.RenewLease(ctx, gwA, lease.ID, fenceOf(lease)); !errors.Is(err, broker.ErrStaleBinding) {
		t.Fatalf("renew with stale generation = %v, want ErrStaleBinding", err)
	}
	if _, err := b.ResolveTarget(ctx, gwA, lease.ID); err == nil {
		t.Fatal("ResolveTarget succeeded for stale-generation lease")
	}
}

// TestLease_StaleRuntimeUID: a recreated Pod/VMI keeps the generation but
// changes runtimeUID — old-incarnation access dies; reconnect needs a new
// ticket.
func TestLease_StaleRuntimeUID(t *testing.T) {
	db, b, clock, src := setup(t)
	seedWorkspace(t, db, "tenant-a", alice.Owner(), "ws-1")
	src.set(readyBinding("ws-1", "tenant-a", alice.Owner(), 4, "rt-4a", clock.Now()))

	lease := leaseFor(t, b, gwA, "ws-1", false)
	src.set(readyBinding("ws-1", "tenant-a", alice.Owner(), 4, "rt-4b", clock.Now())) // pod recreated

	if _, err := b.RenewLease(ctx, gwA, lease.ID, fenceOf(lease)); !errors.Is(err, broker.ErrStaleBinding) {
		t.Fatalf("renew with stale runtimeUID = %v, want ErrStaleBinding", err)
	}
	// A new ticket binds the NEW incarnation and reconnects cleanly.
	tk, err := b.IssueTicket(ctx, alice, "ws-1", true, "")
	if err != nil {
		t.Fatalf("IssueTicket for reconnect: %v", err)
	}
	lease2, err := b.RedeemTicket(ctx, gwA, tk.Token)
	if err != nil {
		t.Fatalf("RedeemTicket for reconnect: %v", err)
	}
	if lease2.RuntimeUID != "rt-4b" {
		t.Fatalf("new lease bound to %q, want rt-4b", lease2.RuntimeUID)
	}
}

// TestRevokeLease_FailsClosed: a revoked lease can never renew or resolve —
// the gateway must drop the session, within the 30 s budget, on the next
// check at the latest.
func TestRevokeLease_FailsClosed(t *testing.T) {
	db, b, clock, src := setup(t)
	seedWorkspace(t, db, "tenant-a", alice.Owner(), "ws-1")
	src.set(readyBinding("ws-1", "tenant-a", alice.Owner(), 1, "rt-1", clock.Now()))

	lease := leaseFor(t, b, gwA, "ws-1", false)
	if err := b.RevokeLease(ctx, lease.ID); err != nil {
		t.Fatalf("RevokeLease: %v", err)
	}
	if _, err := b.RenewLease(ctx, gwA, lease.ID, fenceOf(lease)); !errors.Is(err, broker.ErrRevoked) && !errors.Is(err, broker.ErrLeaseInvalid) {
		t.Fatalf("renew after revoke = %v, want ErrRevoked/ErrLeaseInvalid", err)
	}
	if _, err := b.ResolveTarget(ctx, gwA, lease.ID); err == nil {
		t.Fatal("ResolveTarget succeeded after revoke — must fail closed")
	}
}

// TestRevokeLeaseChanged (R9c): the variant reports whether a live lease was
// actually revoked — true once, then false for the already-dead lease and for
// a lease ID that never existed. The revoke itself still fails the lease
// closed.
func TestRevokeLeaseChanged(t *testing.T) {
	db, b, clock, src := setup(t)
	seedWorkspace(t, db, "tenant-a", alice.Owner(), "ws-1")
	src.set(readyBinding("ws-1", "tenant-a", alice.Owner(), 1, "rt-1", clock.Now()))
	lease := leaseFor(t, b, gwA, "ws-1", false)

	if changed, err := b.RevokeLeaseChanged(ctx, "no-such-lease"); err != nil || changed {
		t.Fatalf("unknown lease: changed=%v err=%v, want false, nil", changed, err)
	}
	if changed, err := b.RevokeLeaseChanged(ctx, lease.ID); err != nil || !changed {
		t.Fatalf("live lease: changed=%v err=%v, want true, nil", changed, err)
	}
	if _, err := b.RenewLease(ctx, gwA, lease.ID, fenceOf(lease)); err == nil {
		t.Fatal("renew succeeded after RevokeLeaseChanged")
	}
	if changed, err := b.RevokeLeaseChanged(ctx, lease.ID); err != nil || changed {
		t.Fatalf("already-revoked lease: changed=%v err=%v, want false, nil", changed, err)
	}
}

// TestRenewLease_StaleObservationFailsClosed: when the observed state ages
// past 15 s the broker stops renewing — access cannot outlive fresh truth.
func TestRenewLease_StaleObservationFailsClosed(t *testing.T) {
	db, b, clock, src := setup(t)
	seedWorkspace(t, db, "tenant-a", alice.Owner(), "ws-1")
	src.set(readyBinding("ws-1", "tenant-a", alice.Owner(), 1, "rt-1", clock.Now()))

	lease := leaseFor(t, b, gwA, "ws-1", false)
	clock.Advance(broker.MaxBindingAge + time.Second) // last observation now stale

	if _, err := b.RenewLease(ctx, gwA, lease.ID, fenceOf(lease)); !errors.Is(err, broker.ErrFreshness) {
		t.Fatalf("renew with stale observation = %v, want ErrFreshness", err)
	}
}

// TestSingleActiveLeaseAcrossGateways: the unique-active-lease invariant —
// even with two takeover tickets redeemed by two gateway identities, exactly
// one lease is ever active at a time.
func TestSingleActiveLeaseAcrossGateways(t *testing.T) {
	db, b, clock, src := setup(t)
	seedWorkspace(t, db, "tenant-a", alice.Owner(), "ws-1")
	src.set(readyBinding("ws-1", "tenant-a", alice.Owner(), 1, "rt-1", clock.Now()))

	leaseFor(t, b, gwA, "ws-1", false)
	leaseFor(t, b, gwB, "ws-1", true) // takeover via second gateway

	var active int
	if err := db.Pool().QueryRow(ctx,
		`SELECT count(*) FROM connection_lease WHERE workspace_id = 'ws-1' AND state = 'active'`).Scan(&active); err != nil {
		t.Fatalf("count active leases: %v", err)
	}
	if active != 1 {
		t.Fatalf("active leases = %d, want exactly 1", active)
	}
}

// TestTakeover_FencesOldLeaseFirst: an explicit takeover supersedes the live
// lease before the new lease exists — the old socket is fenced first, never
// concurrently authorized.
func TestTakeover_FencesOldLeaseFirst(t *testing.T) {
	db, b, clock, src := setup(t)
	seedWorkspace(t, db, "tenant-a", alice.Owner(), "ws-1")
	src.set(readyBinding("ws-1", "tenant-a", alice.Owner(), 1, "rt-1", clock.Now()))

	old := leaseFor(t, b, gwA, "ws-1", false)
	newLease := leaseFor(t, b, gwA, "ws-1", true)

	if newLease.FencingVersion <= old.FencingVersion {
		t.Fatalf("fencing version did not increase: old=%d new=%d", old.FencingVersion, newLease.FencingVersion)
	}
	// Old lease must be dead (superseded) — it can never renew again.
	if _, err := b.RenewLease(ctx, gwA, old.ID, fenceOf(old)); err == nil {
		t.Fatal("superseded lease renewed — takeover did not fence the old session")
	}
	// Ordering proof at the row level: old lease closed no later than the
	// new lease's creation.
	var closedAt, createdAt time.Time
	if err := db.Pool().QueryRow(ctx,
		`SELECT l1.closed_at, l2.created_at FROM connection_lease l1, connection_lease l2
		 WHERE l1.id = $1 AND l2.id = $2`, old.ID, newLease.ID).Scan(&closedAt, &createdAt); err != nil {
		t.Fatalf("lease rows: %v", err)
	}
	if closedAt.After(createdAt) {
		t.Fatalf("old lease closed at %v AFTER new lease created at %v — fence must precede grant", closedAt, createdAt)
	}
}

// TestResolveTarget_RequiresLeaseAndGateway: target resolution is bound to
// the live lease and its holding gateway; internal target never leaves the
// server side.
func TestResolveTarget_RequiresLeaseAndGateway(t *testing.T) {
	db, b, clock, src := setup(t)
	seedWorkspace(t, db, "tenant-a", alice.Owner(), "ws-1")
	src.set(readyBinding("ws-1", "tenant-a", alice.Owner(), 1, "rt-1", clock.Now()))

	lease := leaseFor(t, b, gwA, "ws-1", false)
	target, err := b.ResolveTarget(ctx, gwA, lease.ID)
	if err != nil {
		t.Fatalf("ResolveTarget: %v", err)
	}
	if target.Protocol == "" || target.ServiceDNS == "" || len(target.TLSCA) == 0 {
		t.Fatalf("target incomplete: %+v", target)
	}
	if _, err := b.ResolveTarget(ctx, gwB, lease.ID); !errors.Is(err, broker.ErrDenied) {
		t.Fatalf("resolve by foreign gateway = %v, want ErrDenied", err)
	}
}
