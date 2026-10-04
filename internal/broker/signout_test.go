package broker_test

// S17: sign-out revocation. A portal session's session-layer material —
// active leases minted under its portal_session_digest plus its still
// outstanding launch tickets — dies at the store so every gateway
// replica's renew loop closes the bound streams within one renew cycle
// and a replayed workspace cookie resolves to a dead lease on any
// replica.

import (
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tinyorbitvn/tinycdi/internal/broker"
)

// TestRevokePortalSession_RevokesBoundLeases: every active lease minted
// under the session's digest dies; another session's leases and a
// NULL-digest lease (no portal-session binding recorded) survive.
func TestRevokePortalSession_RevokesBoundLeases(t *testing.T) {
	db, b, clock, src := setup(t)
	for _, ws := range []string{"ws-1", "ws-2", "ws-3", "ws-4"} {
		seedWorkspace(t, db, "tenant-a", alice.Owner(), ws)
		src.set(readyBinding(broker.PlatformID(ws), "tenant-a", alice.Owner(), 1, "rt-1", clock.Now()))
	}

	mine1 := leaseForSess(t, db, b, gwA, "ws-1", false, "sess-1")
	mine2 := leaseForSess(t, db, b, gwA, "ws-2", false, "sess-1")
	other := leaseForSess(t, db, b, gwA, "ws-3", false, "sess-2")
	legacy := leaseFor(t, b, gwA, "ws-4", false) // NULL portal_session_digest

	d := digestOf("cookie-1")
	if err := b.BindSession(ctx, gwA, mine1.ID, d); err != nil {
		t.Fatalf("BindSession: %v", err)
	}

	n, err := b.RevokePortalSession(ctx, "sess-1")
	if err != nil {
		t.Fatalf("RevokePortalSession: %v", err)
	}
	if n != 2 {
		t.Fatalf("revoked %d leases, want 2", n)
	}

	for _, id := range []string{mine1.ID, mine2.ID} {
		if _, err := b.RenewLease(ctx, gwA, id, fenceOf(mine1)); !errors.Is(err, broker.ErrRevoked) {
			t.Fatalf("renew revoked lease = %v, want ErrRevoked", err)
		}
	}
	// The workspace cookie dies with the lease: the digest resolves to a
	// dead row, not "unknown".
	if _, err := b.LeaseBySession(ctx, gwA, d); !errors.Is(err, broker.ErrRevoked) {
		t.Fatalf("LeaseBySession after revoke = %v, want ErrRevoked", err)
	}
	// Untouched: the other session's lease and the unbound (legacy) lease.
	if _, err := b.RenewLease(ctx, gwA, other.ID, fenceOf(other)); err != nil {
		t.Fatalf("other session's lease revoked: %v", err)
	}
	if _, err := b.RenewLease(ctx, gwA, legacy.ID, fenceOf(legacy)); err != nil {
		t.Fatalf("NULL-digest lease revoked: %v", err)
	}
}

// TestRevokePortalSession_RevokesOutstandingTickets: a launch ticket the
// session minted but never redeemed can never mint a lease after sign-out.
func TestRevokePortalSession_RevokesOutstandingTickets(t *testing.T) {
	db, b, clock, src := setup(t)
	seedWorkspace(t, db, "tenant-a", alice.Owner(), "ws-1")
	src.set(readyBinding("ws-1", "tenant-a", alice.Owner(), 1, "rt-1", clock.Now()))
	seedPortalSession(t, db, "sess-1")

	tk, err := b.IssueTicket(ctx, alice, "ws-1", false, "", "sess-1")
	if err != nil {
		t.Fatalf("IssueTicket: %v", err)
	}
	if _, err := b.RevokePortalSession(ctx, "sess-1"); err != nil {
		t.Fatalf("RevokePortalSession: %v", err)
	}
	if _, err := b.RedeemTicket(ctx, gwA, tk.Token); !errors.Is(err, broker.ErrRevoked) {
		t.Fatalf("redeem after sign-out = %v, want ErrRevoked", err)
	}
}

// TestRedeemTicket_DeadPortalSessionDenied: redemption re-checks the
// issuing portal session — the second barrier, covering a sign-out whose
// session delete landed but whose revoke call never did. Denial is
// stable: while the session stays dead every replay fails the same way.
func TestRedeemTicket_DeadPortalSessionDenied(t *testing.T) {
	db, b, clock, src := setup(t)
	seedWorkspace(t, db, "tenant-a", alice.Owner(), "ws-1")
	src.set(readyBinding("ws-1", "tenant-a", alice.Owner(), 1, "rt-1", clock.Now()))
	seedPortalSession(t, db, "sess-1")

	tk, err := b.IssueTicket(ctx, alice, "ws-1", false, "", "sess-1")
	if err != nil {
		t.Fatalf("IssueTicket: %v", err)
	}
	deletePortalSession(t, db, "sess-1")

	for i := 0; i < 2; i++ {
		if _, err := b.RedeemTicket(ctx, gwA, tk.Token); !errors.Is(err, broker.ErrRevoked) {
			t.Fatalf("redeem %d under dead session = %v, want ErrRevoked", i, err)
		}
	}
}

// TestRevokePortalSession_ZeroesOpenStreams: drain accounting follows the
// same rule as RevokeLease — streams on a revoked lease can never report
// their close, so the transition zeroes open_streams and anchors the
// disconnect clock.
func TestRevokePortalSession_ZeroesOpenStreams(t *testing.T) {
	db, b, clock, src := setup(t)
	seedWorkspace(t, db, "tenant-a", alice.Owner(), "ws-1")
	src.set(readyBinding("ws-1", "tenant-a", alice.Owner(), 1, "rt-1", clock.Now()))

	lease := leaseForSess(t, db, b, gwA, "ws-1", false, "sess-1")
	if err := b.ReportActivity(ctx, gwA, lease.ID, fenceOf(lease),
		broker.ActivityEvent{Type: broker.ActivityConnected}); err != nil {
		t.Fatalf("ReportActivity(connected): %v", err)
	}

	if _, err := b.RevokePortalSession(ctx, "sess-1"); err != nil {
		t.Fatalf("RevokePortalSession: %v", err)
	}
	var (
		open    int
		disconn *time.Time
	)
	if err := db.Pool().QueryRow(ctx,
		`SELECT open_streams, disconnected_since FROM workspace_activity
		 WHERE workspace_id = $1 AND runtime_generation = $2`,
		"ws-1", int64(lease.RuntimeGeneration)).Scan(&open, &disconn); err != nil {
		t.Fatalf("workspace_activity row: %v", err)
	}
	if open != 0 {
		t.Fatalf("open_streams = %d after revoke, want 0", open)
	}
	if disconn == nil {
		t.Fatal("disconnected_since NULL after revoke — disconnect timeout could never fire")
	}
}

// TestRevokePortalSession_IdempotentAndEmpty: re-running the revoke is a
// no-op, and an empty session id (a ticket that recorded no binding)
// matches nothing.
func TestRevokePortalSession_IdempotentAndEmpty(t *testing.T) {
	db, b, clock, src := setup(t)
	seedWorkspace(t, db, "tenant-a", alice.Owner(), "ws-1")
	src.set(readyBinding("ws-1", "tenant-a", alice.Owner(), 1, "rt-1", clock.Now()))

	lease := leaseForSess(t, db, b, gwA, "ws-1", false, "sess-1")
	for i, want := range []int{1, 0} {
		n, err := b.RevokePortalSession(ctx, "sess-1")
		if err != nil {
			t.Fatalf("revoke %d: %v", i, err)
		}
		if n != want {
			t.Fatalf("revoke %d = %d leases, want %d", i, n, want)
		}
	}
	if n, err := b.RevokePortalSession(ctx, ""); err != nil || n != 0 {
		t.Fatalf("empty id = (%d, %v), want (0, nil)", n, err)
	}
	if n, err := b.RevokePortalSession(ctx, "never-issued"); err != nil || n != 0 {
		t.Fatalf("unknown id = (%d, %v), want (0, nil)", n, err)
	}
	if _, err := b.RenewLease(ctx, gwA, lease.ID, fenceOf(lease)); !errors.Is(err, broker.ErrRevoked) {
		t.Fatalf("lease alive after no-op revokes: %v", err)
	}
}

// TestRevokePortalSession_LiveSessionSurvivesRevoke: a ticket that stays
// bound to a DIFFERENT live portal session is not a casualty of the
// first's sign-out — only the signing-out session's digest matches.
func TestRevokePortalSession_LiveSessionSurvivesRevoke(t *testing.T) {
	db, b, clock, src := setup(t)
	seedWorkspace(t, db, "tenant-a", alice.Owner(), "ws-1")
	src.set(readyBinding("ws-1", "tenant-a", alice.Owner(), 1, "rt-1", clock.Now()))
	seedPortalSession(t, db, "sess-2")

	tk, err := b.IssueTicket(ctx, alice, "ws-1", false, "", "sess-2")
	if err != nil {
		t.Fatalf("IssueTicket: %v", err)
	}
	if _, err := b.RevokePortalSession(ctx, "sess-1"); err != nil {
		t.Fatalf("RevokePortalSession: %v", err)
	}
	if _, err := b.RedeemTicket(ctx, gwA, tk.Token); err != nil {
		t.Fatalf("sess-2 ticket died with sess-1's sign-out: %v", err)
	}
}

// TestMigration020_Idempotent: the portal_session_digest indexes land on a
// fresh database and on re-run.
func TestMigration020_Idempotent(t *testing.T) {
	db := newDB(t)
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	for _, name := range []string{"connection_lease_portal_session", "launch_ticket_portal_session"} {
		var def string
		if err := db.Pool().QueryRow(ctx,
			`SELECT indexdef FROM pg_indexes
			 WHERE schemaname = 'public' AND indexname = $1`, name).
			Scan(&def); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("index %s missing", name)
			}
			t.Fatalf("index %s probe: %v", name, err)
		}
	}
}
