package broker_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/api"
	"github.com/tinyorbitvn/tinycdi/internal/broker"
)

var (
	ctx       = context.Background()
	alice     = api.Principal{Issuer: "https://idp.example", Subject: "alice", TenantID: "tenant-a"}
	bobTenant = api.Principal{Issuer: "https://idp.example", Subject: "bob", TenantID: "tenant-b"}
	gwA       = broker.GatewayIdentity{ID: "gw-1", Audience: "session.example.dev"}
	gwB       = broker.GatewayIdentity{ID: "gw-2", Audience: "session.example.dev"}
	gwForeign = broker.GatewayIdentity{ID: "gw-9", Audience: "other.example.dev"}
)

// TestIssueTicket_BindsCurrentIncarnation: a ticket binds the exact
// (workspaceUID, runtimeGeneration, runtimeUID) the operator currently
// reports, lives 60 s, is opaque and is persisted only as a hash.
func TestIssueTicket_BindsCurrentIncarnation(t *testing.T) {
	db, b, clock, src := setup(t)
	seedWorkspace(t, db, "tenant-a", alice.Owner(), "ws-1")
	src.set(readyBinding("ws-1", "tenant-a", alice.Owner(), 3, "rt-uid-3", clock.Now()))

	tk, err := b.IssueTicket(ctx, alice, "ws-1", false)
	if err != nil {
		t.Fatalf("IssueTicket: %v", err)
	}
	if tk.Token == "" || len(tk.Token) < 32 {
		t.Fatalf("ticket token %q too short/empty — needs ~256 bits opaque", tk.Token)
	}
	if tk.WorkspaceID != "ws-1" {
		t.Fatalf("ticket workspace = %q, want ws-1", tk.WorkspaceID)
	}
	if got := tk.ExpiresAt.Sub(clock.Now()); got != broker.TicketTTL {
		t.Fatalf("ticket TTL = %v, want %v", got, broker.TicketTTL)
	}

	// Only the hash may be persisted: the row must exist, carry a 32-byte
	// hash and no stored column may equal the plaintext token.
	var hashLen int
	var plain bool
	err = db.Pool().QueryRow(ctx,
		`SELECT octet_length(ticket_hash), ticket_hash = $1::bytea FROM launch_ticket WHERE workspace_id = 'ws-1'`,
		[]byte(tk.Token)).Scan(&hashLen, &plain)
	if err != nil {
		t.Fatalf("ticket row: %v", err)
	}
	if plain || hashLen != 32 {
		t.Fatalf("ticket persisted insecurely: plaintext=%v hashLen=%d", plain, hashLen)
	}
}

// TestIssueTicket_RequiresReady: tickets only issue for a Ready workspace.
func TestIssueTicket_RequiresReady(t *testing.T) {
	db, b, clock, src := setup(t)
	seedWorkspace(t, db, "tenant-a", alice.Owner(), "ws-1")
	binding := readyBinding("ws-1", "tenant-a", alice.Owner(), 1, "rt-1", clock.Now())
	binding.Phase = "Provisioning"
	src.set(binding)

	_, err := b.IssueTicket(ctx, alice, "ws-1", false)
	if !errors.Is(err, broker.ErrNotReady) {
		t.Fatalf("IssueTicket on Provisioning = %v, want ErrNotReady", err)
	}
}

// TestIssueTicket_StaleObservedState: freshness budget is 15 s — past it the
// broker must stop issuing (fail closed until fresh state returns).
func TestIssueTicket_StaleObservedState(t *testing.T) {
	db, b, clock, src := setup(t)
	seedWorkspace(t, db, "tenant-a", alice.Owner(), "ws-1")
	binding := readyBinding("ws-1", "tenant-a", alice.Owner(), 1, "rt-1", clock.Now())
	src.set(binding)

	clock.Advance(broker.MaxBindingAge + time.Second) // observation now stale
	_, err := b.IssueTicket(ctx, alice, "ws-1", false)
	if !errors.Is(err, broker.ErrFreshness) {
		t.Fatalf("IssueTicket with stale binding = %v, want ErrFreshness", err)
	}
}

// TestIssueTicket_TenantAndOwnership: a principal may not mint tickets for a
// workspace outside their tenant, nor for one owned by someone else.
func TestIssueTicket_TenantAndOwnership(t *testing.T) {
	db, b, clock, src := setup(t)
	seedWorkspace(t, db, "tenant-b", bobTenant.Owner(), "ws-other")
	src.set(readyBinding("ws-other", "tenant-b", bobTenant.Owner(), 1, "rt-1", clock.Now()))

	_, err := b.IssueTicket(ctx, alice, "ws-other", false)
	if !errors.Is(err, broker.ErrNotFound) && !errors.Is(err, broker.ErrDenied) {
		t.Fatalf("IssueTicket cross-tenant = %v, want ErrNotFound or ErrDenied", err)
	}
}

// TestRedeemTicket_ExactlyOnce: a valid ticket redeems once into a lease bound
// to the recorded incarnation, fencing version and expiry; replay fails.
func TestRedeemTicket_ExactlyOnce(t *testing.T) {
	db, b, clock, src := setup(t)
	seedWorkspace(t, db, "tenant-a", alice.Owner(), "ws-1")
	src.set(readyBinding("ws-1", "tenant-a", alice.Owner(), 5, "rt-5", clock.Now()))

	tk, err := b.IssueTicket(ctx, alice, "ws-1", false)
	if err != nil {
		t.Fatalf("IssueTicket: %v", err)
	}
	lease, err := b.RedeemTicket(ctx, gwA, tk.Token)
	if err != nil {
		t.Fatalf("RedeemTicket: %v", err)
	}
	if lease.WorkspaceUID != "ws-1" || lease.RuntimeGeneration != 5 || lease.RuntimeUID != "rt-5" {
		t.Fatalf("lease bound to %+v, want ws-1/gen5/rt-5", lease)
	}
	if lease.FencingVersion < 1 {
		t.Fatalf("lease fencing_version = %d, want >=1", lease.FencingVersion)
	}
	if lease.GatewayID != gwA.ID {
		t.Fatalf("lease held by %q, want %q", lease.GatewayID, gwA.ID)
	}

	if _, err := b.RedeemTicket(ctx, gwA, tk.Token); err == nil {
		t.Fatal("replay of consumed ticket succeeded — tickets are single-use")
	}
}

// TestRedeemTicket_ConcurrentExactlyOneWins: concurrent redemption of the
// same ticket yields exactly one lease — the atomic claim is DB-enforced.
func TestRedeemTicket_ConcurrentExactlyOneWins(t *testing.T) {
	db, b, clock, src := setup(t)
	seedWorkspace(t, db, "tenant-a", alice.Owner(), "ws-1")
	src.set(readyBinding("ws-1", "tenant-a", alice.Owner(), 1, "rt-1", clock.Now()))

	tk, err := b.IssueTicket(ctx, alice, "ws-1", false)
	if err != nil {
		t.Fatalf("IssueTicket: %v", err)
	}

	const n = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	var wins int
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := b.RedeemTicket(ctx, gwA, tk.Token); err == nil {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("concurrent redemptions succeeded %d times, want exactly 1", wins)
	}
}

// TestRedeemTicket_ExpiredStillConsumed: an expired ticket is consumed by the
// attempt — strictly one redemption try, even after TTL passes.
func TestRedeemTicket_ExpiredStillConsumed(t *testing.T) {
	db, b, clock, src := setup(t)
	seedWorkspace(t, db, "tenant-a", alice.Owner(), "ws-1")
	src.set(readyBinding("ws-1", "tenant-a", alice.Owner(), 1, "rt-1", clock.Now()))

	tk, err := b.IssueTicket(ctx, alice, "ws-1", false)
	if err != nil {
		t.Fatalf("IssueTicket: %v", err)
	}
	clock.Advance(broker.TicketTTL + time.Second)
	if _, err := b.RedeemTicket(ctx, gwA, tk.Token); !errors.Is(err, broker.ErrTicketExpired) && !errors.Is(err, broker.ErrTicketInvalid) {
		t.Fatalf("redeem expired = %v, want ErrTicketExpired/ErrTicketInvalid", err)
	}
	// A second attempt must also fail — the ticket was consumed, not merely expired.
	if _, err := b.RedeemTicket(ctx, gwA, tk.Token); err == nil {
		t.Fatal("expired ticket redeemed on second attempt — consume-on-attempt violated")
	}
}

// TestRedeemTicket_RevokedNeverRedeems: a revoked ticket is deny-listed — it
// can never redeem, before or after issuance-time expiry.
func TestRedeemTicket_RevokedNeverRedeems(t *testing.T) {
	db, b, clock, src := setup(t)
	seedWorkspace(t, db, "tenant-a", alice.Owner(), "ws-1")
	src.set(readyBinding("ws-1", "tenant-a", alice.Owner(), 1, "rt-1", clock.Now()))

	tk, err := b.IssueTicket(ctx, alice, "ws-1", false)
	if err != nil {
		t.Fatalf("IssueTicket: %v", err)
	}
	if err := b.RevokeTicket(ctx, tk.Token); err != nil {
		t.Fatalf("RevokeTicket: %v", err)
	}
	if _, err := b.RedeemTicket(ctx, gwA, tk.Token); !errors.Is(err, broker.ErrRevoked) && !errors.Is(err, broker.ErrTicketInvalid) {
		t.Fatalf("redeem revoked = %v, want ErrRevoked/ErrTicketInvalid", err)
	}
}

// TestRedeemTicket_WrongGatewayAudience: a ticket bound to the session
// audience must not redeem through a differently-audienced gateway.
func TestRedeemTicket_WrongGatewayAudience(t *testing.T) {
	db, b, clock, src := setup(t)
	seedWorkspace(t, db, "tenant-a", alice.Owner(), "ws-1")
	src.set(readyBinding("ws-1", "tenant-a", alice.Owner(), 1, "rt-1", clock.Now()))

	tk, err := b.IssueTicket(ctx, alice, "ws-1", false)
	if err != nil {
		t.Fatalf("IssueTicket: %v", err)
	}
	if _, err := b.RedeemTicket(ctx, gwForeign, tk.Token); !errors.Is(err, broker.ErrDenied) && !errors.Is(err, broker.ErrTicketInvalid) {
		t.Fatalf("redeem via foreign audience = %v, want ErrDenied/ErrTicketInvalid", err)
	}
}

// TestRedeemTicket_ExpiredLeaseClosesStreams: when the first touch of a
// dead lease is a ticket redemption — not a directory lookup or a renew —
// the fence must still close the dead lease's drain accounting: the replica
// that owned it is gone and its streams can never report their close
// (Review Focus #1; same accounting as liveLease lazy expiry and
// RevokeLease).
func TestRedeemTicket_ExpiredLeaseClosesStreams(t *testing.T) {
	db, b, clock, src := setup(t)
	seedWorkspace(t, db, "tenant-a", alice.Owner(), "ws-1")
	src.set(readyBinding("ws-1", "tenant-a", alice.Owner(), 1, "rt-1", clock.Now()))

	old := leaseFor(t, b, gwA, "ws-1", false)
	if err := b.ReportActivity(ctx, gwA, old.ID, fenceOf(old),
		broker.ActivityEvent{Type: broker.ActivityConnected}); err != nil {
		t.Fatalf("ReportActivity(connected): %v", err)
	}

	// L1 dies un-renewed; IssueTicket's advisory gate sees the expired row
	// and a plain (non-takeover) ticket issues. Refreshing the observation
	// keeps the binding inside the freshness budget.
	clock.Advance(broker.LeaseTTL + time.Second)
	src.set(readyBinding("ws-1", "tenant-a", alice.Owner(), 1, "rt-1", clock.Now()))
	tk, err := b.IssueTicket(ctx, alice, "ws-1", false)
	if err != nil {
		t.Fatalf("IssueTicket over expired lease: %v", err)
	}
	newLease, err := b.RedeemTicket(ctx, gwA, tk.Token)
	if err != nil {
		t.Fatalf("RedeemTicket over expired lease: %v", err)
	}
	if newLease.ID == old.ID {
		t.Fatal("redemption returned the expired lease")
	}

	var (
		open    int
		disconn *time.Time
		state   string
	)
	if err := db.Pool().QueryRow(ctx,
		`SELECT state FROM connection_lease WHERE id = $1`, old.ID).Scan(&state); err != nil {
		t.Fatalf("old lease row: %v", err)
	}
	if state != "expired" {
		t.Fatalf("old lease state = %q, want expired", state)
	}
	if err := db.Pool().QueryRow(ctx,
		`SELECT open_streams, disconnected_since FROM workspace_activity
		 WHERE workspace_id = $1 AND runtime_generation = $2`,
		"ws-1", int64(old.RuntimeGeneration)).Scan(&open, &disconn); err != nil {
		t.Fatalf("workspace_activity row: %v", err)
	}
	if open != 0 {
		t.Fatalf("open_streams = %d after redemption fenced the expired lease, want 0", open)
	}
	if disconn == nil {
		t.Fatal("disconnected_since NULL after redemption fenced the expired lease — disconnect timeout could never fire")
	}
}

// TestIssueTicket_ConnectionInUse: issuing while a live lease exists requires
// explicit takeover — this is what maps to 409 CONNECTION_IN_USE on the API.
func TestIssueTicket_ConnectionInUse(t *testing.T) {
	db, b, clock, src := setup(t)
	seedWorkspace(t, db, "tenant-a", alice.Owner(), "ws-1")
	src.set(readyBinding("ws-1", "tenant-a", alice.Owner(), 1, "rt-1", clock.Now()))

	tk1, err := b.IssueTicket(ctx, alice, "ws-1", false)
	if err != nil {
		t.Fatalf("IssueTicket #1: %v", err)
	}
	if _, err := b.RedeemTicket(ctx, gwA, tk1.Token); err != nil {
		t.Fatalf("RedeemTicket #1: %v", err)
	}
	if _, err := b.IssueTicket(ctx, alice, "ws-1", false); !errors.Is(err, broker.ErrConnectionInUse) {
		t.Fatalf("IssueTicket with live lease = %v, want ErrConnectionInUse", err)
	}
}
