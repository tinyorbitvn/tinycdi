package broker_test

import (
	"errors"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/broker"
	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
	"github.com/tinyorbitvn/tinycdi/internal/store"
)

// ticketConsumed reports whether the ticket's consumed_at is set.
func ticketConsumed(t *testing.T, db *store.DB, opaque string) bool {
	t.Helper()
	var consumed bool
	if err := db.Pool().QueryRow(ctx,
		`SELECT consumed_at IS NOT NULL FROM launch_ticket WHERE ticket_hash = sha256(convert_to($1, 'UTF8'))`,
		opaque).Scan(&consumed); err != nil {
		t.Fatalf("ticket row: %v", err)
	}
	return consumed
}

// TestRedeemTicket_EveryRefusalConsumes (R1-25): once a redemption attempt
// passes the audience check, the ticket is consumed whatever the outcome —
// the refusal commits the consume. A second attempt fails even after the
// blocking condition is cleared.
func TestRedeemTicket_EveryRefusalConsumes(t *testing.T) {
	cases := []struct {
		name  string
		block func(t *testing.T, db *store.DB, clock *fakeClock) (want error, clear func())
	}{
		{"expired", func(t *testing.T, _ *store.DB, clock *fakeClock) (error, func()) {
			clock.Advance(broker.TicketTTL + time.Second)
			return broker.ErrTicketExpired, func() { clock.Advance(-(broker.TicketTTL + time.Second)) }
		}},
		{"generation revoked", func(t *testing.T, db *store.DB, clock *fakeClock) (error, func()) {
			if _, err := db.Pool().Exec(ctx,
				`INSERT INTO workspace_revocation (workspace_id, runtime_generation, revoked_at) VALUES ('ws-1', 1, $1)`,
				clock.Now()); err != nil {
				t.Fatalf("seed revocation: %v", err)
			}
			return broker.ErrRevoked, func() {
				if _, err := db.Pool().Exec(ctx, `DELETE FROM workspace_revocation WHERE workspace_id = 'ws-1'`); err != nil {
					t.Fatalf("clear revocation: %v", err)
				}
			}
		}},
		{"portal session deleted", func(t *testing.T, db *store.DB, _ *fakeClock) (error, func()) {
			deletePortalSession(t, db, testPortalSession)
			return broker.ErrRevoked, func() { seedPortalSession(t, db, testPortalSession) }
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, b, clock, src := setup(t)
			seedWorkspace(t, db, "tenant-a", alice.Owner(), "ws-1")
			src.set(readyBinding("ws-1", "tenant-a", alice.Owner(), 1, "rt-1", clock.Now()))
			tk, err := b.IssueTicket(ctx, alice, "ws-1", false, provisioning.SessionPolicy{}, testPortalSession)
			if err != nil {
				t.Fatalf("IssueTicket: %v", err)
			}
			want, clear := tc.block(t, db, clock)
			if _, err := b.RedeemTicket(ctx, gwA, tk.Token); !errors.Is(err, want) {
				t.Fatalf("refused redeem = %v, want %v", err, want)
			}
			if !ticketConsumed(t, db, tk.Token) {
				t.Fatal("refused redeem left consumed_at NULL: the consume rolled back with the refusal")
			}
			clear()
			src.set(readyBinding("ws-1", "tenant-a", alice.Owner(), 1, "rt-1", clock.Now()))
			if _, err := b.RedeemTicket(ctx, gwA, tk.Token); !errors.Is(err, broker.ErrTicketInvalid) {
				t.Fatalf("second attempt = %v, want ErrTicketInvalid (already consumed)", err)
			}
		})
	}

	t.Run("live lease without takeover", func(t *testing.T) {
		db, b, clock, src := setup(t)
		seedWorkspace(t, db, "tenant-a", alice.Owner(), "ws-1")
		src.set(readyBinding("ws-1", "tenant-a", alice.Owner(), 1, "rt-1", clock.Now()))
		t1, err := b.IssueTicket(ctx, alice, "ws-1", false, provisioning.SessionPolicy{}, testPortalSession)
		if err != nil {
			t.Fatalf("IssueTicket t1: %v", err)
		}
		t2, err := b.IssueTicket(ctx, alice, "ws-1", false, provisioning.SessionPolicy{}, testPortalSession)
		if err != nil {
			t.Fatalf("IssueTicket t2: %v", err)
		}
		l1, err := b.RedeemTicket(ctx, gwA, t1.Token)
		if err != nil {
			t.Fatalf("redeem t1: %v", err)
		}
		if _, err := b.RedeemTicket(ctx, gwA, t2.Token); !errors.Is(err, broker.ErrConnectionInUse) {
			t.Fatalf("redeem t2 while t1's lease lives = %v, want ErrConnectionInUse", err)
		}
		if !ticketConsumed(t, db, t2.Token) {
			t.Fatal("refused redeem left t2 unconsumed")
		}
		if err := b.RevokeLease(ctx, l1.ID); err != nil {
			t.Fatalf("RevokeLease: %v", err)
		}
		if l2, err := b.RedeemTicket(ctx, gwA, t2.Token); !errors.Is(err, broker.ErrTicketInvalid) {
			t.Fatalf("t2 second attempt = lease %q, err %v; want ErrTicketInvalid", l2.ID, err)
		}
	})
}
