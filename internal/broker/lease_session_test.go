// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package broker_test

// S17 defence-in-depth: lease renew and attach re-validate the lease's
// bound portal session row (epoch + absolute expiry, same semantics as
// RedeemTicket's re-check). A missing or invalid row revokes the lease at
// the store on the spot — the gateway renew loop's terminal-error path
// closes the bound stream within one renew cycle, and a replayed cookie
// resolves to a dead lease (401) on every replica. Leases that never
// recorded a portal_session_digest are exempt: there is no session to
// verify and revoking them would mass-kill sessions still minted by
// pre-upgrade replicas during a rolling deploy.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/tinyorbitvn/tinycdi/internal/broker"
	"github.com/tinyorbitvn/tinycdi/internal/observability"
	"github.com/tinyorbitvn/tinycdi/internal/store"
)

// portalSessionRowKey is the sessions.id form the digest points at (hex of
// the SHA-256 the ticket/lease store), matching seedPortalSession.
func portalSessionRowKey(portalSessionID string) string {
	sum := sha256.Sum256([]byte(portalSessionID))
	return hex.EncodeToString(sum[:])
}

// stalePortalSession flips the seeded sessions row's epoch off the live
// platform_meta value — the shape a restored pre-rotation dump leaves.
func stalePortalSession(t *testing.T, db *store.DB, portalSessionID string) {
	t.Helper()
	tag, err := db.Pool().Exec(ctx,
		`UPDATE sessions SET epoch = 'pre-rotation' WHERE id = $1`,
		portalSessionRowKey(portalSessionID))
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("stale portal session: %v (rows=%d)", err, tag.RowsAffected())
	}
}

// expirePortalSession sets the seeded sessions row's absolute expiry in
// the past.
func expirePortalSession(t *testing.T, db *store.DB, portalSessionID string) {
	t.Helper()
	tag, err := db.Pool().Exec(ctx,
		`UPDATE sessions SET expires_at = now() - interval '1 minute' WHERE id = $1`,
		portalSessionRowKey(portalSessionID))
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("expire portal session: %v (rows=%d)", err, tag.RowsAffected())
	}
}

// leaseState reads the lease row's lifecycle state and whether closed_at
// was stamped.
func leaseState(t *testing.T, db *store.DB, leaseID string) (string, bool) {
	t.Helper()
	var (
		state  string
		closed *time.Time
	)
	if err := db.Pool().QueryRow(ctx,
		`SELECT state, closed_at FROM connection_lease WHERE id = $1`, leaseID).
		Scan(&state, &closed); err != nil {
		t.Fatalf("lease row %s: %v", leaseID, err)
	}
	return state, closed != nil
}

// openStreams reads the bound generation's drain accounting.
func openStreams(t *testing.T, db *store.DB, wsUID string, gen uint64) (int, bool) {
	t.Helper()
	var (
		open    int
		disconn *time.Time
	)
	if err := db.Pool().QueryRow(ctx,
		`SELECT open_streams, disconnected_since FROM workspace_activity
		 WHERE workspace_id = $1 AND runtime_generation = $2`,
		wsUID, int64(gen)).Scan(&open, &disconn); err != nil {
		t.Fatalf("workspace_activity row: %v", err)
	}
	return open, disconn != nil
}

// leaseMissingSeries returns the expected tinycdi_lease_session_missing_total
// exposition for one reason count.
func leaseMissingSeries(reason string, n int) string {
	return `# HELP tinycdi_lease_session_missing_total Leases revoked because the bound portal session row was absent or failed the epoch/expiry check (S17 defence-in-depth), by bounded reason.
# TYPE tinycdi_lease_session_missing_total counter
tinycdi_lease_session_missing_total{reason="` + reason + `"} ` + strconv.Itoa(n) + "\n"
}

// TestLeaseSession_RenewRevokesWhenSessionDeleted: the bound portal
// session row is gone (a sign-out whose revoke never landed, or a
// restored dump without the row) — the next renew revokes the lease in
// the same store visit, zeroes the stream accounting like S17, and the
// digest resolves to a dead lease for attach.
func TestLeaseSession_RenewRevokesWhenSessionDeleted(t *testing.T) {
	db, _, clock, src := setup(t)
	seedWorkspace(t, db, "tenant-a", alice.Owner(), "ws-1")
	src.set(readyBinding("ws-1", "tenant-a", alice.Owner(), 1, "rt-1", clock.Now()))

	reg := prometheus.NewRegistry()
	m := observability.NewMetrics(reg, nil)
	b := broker.New(db, src, broker.WithClock(clock), broker.WithMetrics(m))

	lease := leaseForSess(t, db, b, gwA, "ws-1", false, "sess-1")
	d := digestOf("cookie-1")
	if err := b.BindSession(ctx, gwA, lease.ID, d); err != nil {
		t.Fatalf("BindSession: %v", err)
	}
	if err := b.ReportActivity(ctx, gwA, lease.ID, fenceOf(lease),
		broker.ActivityEvent{Type: broker.ActivityConnected}); err != nil {
		t.Fatalf("ReportActivity(connected): %v", err)
	}

	deletePortalSession(t, db, "sess-1")

	// The next renew (LeaseRenewInterval cadence) is terminal: the lease
	// is revoked at the store and the gateway's renew loop gets the same
	// ErrRevoked an S17 revoke produces — the bound stream closes within
	// one cycle, no extra round trip.
	if _, err := b.RenewLease(ctx, gwA, lease.ID, fenceOf(lease)); !errors.Is(err, broker.ErrRevoked) {
		t.Fatalf("renew under dead session = %v, want ErrRevoked", err)
	}
	state, closed := leaseState(t, db, lease.ID)
	if state != "revoked" || !closed {
		t.Fatalf("lease state=%q closed=%v, want revoked+closed_at", state, closed)
	}
	if open, disconn := openStreams(t, db, "ws-1", lease.RuntimeGeneration); open != 0 || !disconn {
		t.Fatalf("open_streams=%d disconnected_since set=%v, want 0+anchored", open, disconn)
	}
	if err := testutil.GatherAndCompare(reg,
		strings.NewReader(leaseMissingSeries("absent", 1)),
		"tinycdi_lease_session_missing_total"); err != nil {
		t.Fatalf("metric mismatch:\n%v", err)
	}

	// The cookie now resolves to a dead lease: attach fails closed.
	if _, err := b.LeaseBySession(ctx, gwA, d); !errors.Is(err, broker.ErrRevoked) {
		t.Fatalf("attach after session delete = %v, want ErrRevoked", err)
	}
	// Revocation is terminal: further renews keep failing.
	if _, err := b.RenewLease(ctx, gwA, lease.ID, fenceOf(lease)); !errors.Is(err, broker.ErrRevoked) {
		t.Fatalf("re-renew = %v, want ErrRevoked", err)
	}
}

// TestLeaseSession_AttachDeniedWhenSessionDeleted: with the session row
// gone, a cookie attach (LeaseBySession) and a stream claim on the bound
// lease both hit the check before any state is trusted — ErrRevoked is
// what the gateway renders as 401 session_revoked.
func TestLeaseSession_AttachDeniedWhenSessionDeleted(t *testing.T) {
	db, b, clock, src := setup(t)
	seedWorkspace(t, db, "tenant-a", alice.Owner(), "ws-1")
	src.set(readyBinding("ws-1", "tenant-a", alice.Owner(), 1, "rt-1", clock.Now()))

	lease := leaseForSess(t, db, b, gwA, "ws-1", false, "sess-1")
	d := digestOf("cookie-1")
	if err := b.BindSession(ctx, gwA, lease.ID, d); err != nil {
		t.Fatalf("BindSession: %v", err)
	}
	deletePortalSession(t, db, "sess-1")

	if _, err := b.LeaseBySession(ctx, gwA, d); !errors.Is(err, broker.ErrRevoked) {
		t.Fatalf("LeaseBySession under dead session = %v, want ErrRevoked", err)
	}
	if _, err := b.ClaimStream(ctx, gwA, lease.ID, fenceOf(lease), ""); !errors.Is(err, broker.ErrRevoked) {
		t.Fatalf("ClaimStream under dead session = %v, want ErrRevoked", err)
	}
	if state, _ := leaseState(t, db, lease.ID); state != "revoked" {
		t.Fatalf("lease state=%q, want revoked", state)
	}
}

// TestLeaseSession_RenewUnaffectedWhileSessionAlive: a live bound session
// changes nothing — the renew slides expiry exactly as before.
func TestLeaseSession_RenewUnaffectedWhileSessionAlive(t *testing.T) {
	db, b, clock, src := setup(t)
	seedWorkspace(t, db, "tenant-a", alice.Owner(), "ws-1")
	src.set(readyBinding("ws-1", "tenant-a", alice.Owner(), 1, "rt-1", clock.Now()))

	lease := leaseForSess(t, db, b, gwA, "ws-1", false, "sess-1")
	d := digestOf("cookie-1")
	if err := b.BindSession(ctx, gwA, lease.ID, d); err != nil {
		t.Fatalf("BindSession: %v", err)
	}
	clock.Advance(broker.LeaseRenewInterval)
	src.set(readyBinding("ws-1", "tenant-a", alice.Owner(), 1, "rt-1", clock.Now()))
	renewed, err := b.RenewLease(ctx, gwA, lease.ID, fenceOf(lease))
	if err != nil {
		t.Fatalf("renew under live session = %v, want nil", err)
	}
	if !renewed.ExpiresAt.After(lease.ExpiresAt) {
		t.Fatalf("renewed expiry %v not after %v", renewed.ExpiresAt, lease.ExpiresAt)
	}
	if got, err := b.LeaseBySession(ctx, gwA, d); err != nil || got.ID != lease.ID {
		t.Fatalf("attach = (%v, %v), want the same lease", got.ID, err)
	}
	if _, err := b.ClaimStream(ctx, gwA, lease.ID, fenceOf(lease), ""); err != nil {
		t.Fatalf("claim under live session = %v", err)
	}
}

// TestLeaseSession_StaleEpochRevokes: the sessions row survives but its
// epoch predates the current platform_meta value — what a restore leaves
// before the epoch rotation runs — so the row is invalid and the lease
// dies.
func TestLeaseSession_StaleEpochRevokes(t *testing.T) {
	db, _, clock, src := setup(t)
	seedWorkspace(t, db, "tenant-a", alice.Owner(), "ws-1")
	src.set(readyBinding("ws-1", "tenant-a", alice.Owner(), 1, "rt-1", clock.Now()))

	reg := prometheus.NewRegistry()
	m := observability.NewMetrics(reg, nil)
	b := broker.New(db, src, broker.WithClock(clock), broker.WithMetrics(m))

	lease := leaseForSess(t, db, b, gwA, "ws-1", false, "sess-1")
	stalePortalSession(t, db, "sess-1")

	if _, err := b.RenewLease(ctx, gwA, lease.ID, fenceOf(lease)); !errors.Is(err, broker.ErrRevoked) {
		t.Fatalf("renew under stale-epoch session = %v, want ErrRevoked", err)
	}
	if state, _ := leaseState(t, db, lease.ID); state != "revoked" {
		t.Fatalf("lease state=%q, want revoked", state)
	}
	if err := testutil.GatherAndCompare(reg,
		strings.NewReader(leaseMissingSeries("invalid", 1)),
		"tinycdi_lease_session_missing_total"); err != nil {
		t.Fatalf("metric mismatch:\n%v", err)
	}
}

// TestLeaseSession_ExpiredSessionRevokes: an absolutely-expired sessions
// row fails the same liveness rule the session store applies.
func TestLeaseSession_ExpiredSessionRevokes(t *testing.T) {
	db, b, clock, src := setup(t)
	seedWorkspace(t, db, "tenant-a", alice.Owner(), "ws-1")
	src.set(readyBinding("ws-1", "tenant-a", alice.Owner(), 1, "rt-1", clock.Now()))

	lease := leaseForSess(t, db, b, gwA, "ws-1", false, "sess-1")
	expirePortalSession(t, db, "sess-1")

	if _, err := b.RenewLease(ctx, gwA, lease.ID, fenceOf(lease)); !errors.Is(err, broker.ErrRevoked) {
		t.Fatalf("renew under expired session = %v, want ErrRevoked", err)
	}
	if state, _ := leaseState(t, db, lease.ID); state != "revoked" {
		t.Fatalf("lease state=%q, want revoked", state)
	}
}

// TestLeaseSession_NullDigestLeaseExempt: a lease that never recorded a
// portal_session_digest (pre-018 row, or a ticket issued with no session
// in context) has no session to verify — it keeps renewing on the plain
// TTL. The documented alternative — revoking on sight — would mass-kill
// every legacy session during a rolling upgrade.
func TestLeaseSession_NullDigestLeaseExempt(t *testing.T) {
	db, b, clock, src := setup(t)
	seedWorkspace(t, db, "tenant-a", alice.Owner(), "ws-1")
	src.set(readyBinding("ws-1", "tenant-a", alice.Owner(), 1, "rt-1", clock.Now()))

	lease := leaseFor(t, b, gwA, "ws-1", false) // NULL portal_session_digest
	for i := 0; i < 3; i++ {
		clock.Advance(broker.LeaseRenewInterval)
		src.set(readyBinding("ws-1", "tenant-a", alice.Owner(), 1, "rt-1", clock.Now()))
		if _, err := b.RenewLease(ctx, gwA, lease.ID, fenceOf(lease)); err != nil {
			t.Fatalf("renew %d of NULL-digest lease = %v, want nil", i, err)
		}
	}
	if state, _ := leaseState(t, db, lease.ID); state != "active" {
		t.Fatalf("NULL-digest lease state=%q, want active", state)
	}
}

// TestLeaseSession_MultiReplica: replicas share one gateway identity (the
// mTLS CN), so a session-dead lease dies whichever replica touches it
// first — the observing replica's renew revokes the row and every other
// replica's next read (renew, attach, claim) sees a dead lease.
func TestLeaseSession_MultiReplica(t *testing.T) {
	db, b, clock, src := setup(t)
	for _, ws := range []string{"ws-1", "ws-2"} {
		seedWorkspace(t, db, "tenant-a", alice.Owner(), ws)
		src.set(readyBinding(broker.PlatformID(ws), "tenant-a", alice.Owner(), 1, "rt-1", clock.Now()))
	}

	// Two leases under the same portal session reachable by "different
	// replicas" — same GatewayIdentity, as replicas share the mTLS
	// identity the gateway pins on every broker call.
	lease1 := leaseForSess(t, db, b, gwA, "ws-1", false, "sess-1")
	lease2 := leaseForSess(t, db, b, gwA, "ws-2", false, "sess-1")
	d := digestOf("cookie-2")
	if err := b.BindSession(ctx, gwA, lease2.ID, d); err != nil {
		t.Fatalf("BindSession: %v", err)
	}

	deletePortalSession(t, db, "sess-1")

	// Replica A's renew loop observes the dead session first and owns the
	// revoke of lease1.
	if _, err := b.RenewLease(ctx, gwA, lease1.ID, fenceOf(lease1)); !errors.Is(err, broker.ErrRevoked) {
		t.Fatalf("replica A renew = %v, want ErrRevoked", err)
	}
	// Replica B's cookie attach kills lease2 on first read — no renew of
	// that lease was needed for the row to die.
	if _, err := b.LeaseBySession(ctx, gwA, d); !errors.Is(err, broker.ErrRevoked) {
		t.Fatalf("replica B attach = %v, want ErrRevoked", err)
	}
	for _, id := range []string{lease1.ID, lease2.ID} {
		if state, _ := leaseState(t, db, id); state != "revoked" {
			t.Fatalf("lease %s state=%q, want revoked", id, state)
		}
	}
}

// TestLeaseSession_ForeignGatewayStillRevokes: even an ErrDenied-worthy
// caller triggers the revoke — the lease is dead regardless of who asked.
func TestLeaseSession_ForeignGatewayStillRevokes(t *testing.T) {
	db, b, clock, src := setup(t)
	seedWorkspace(t, db, "tenant-a", alice.Owner(), "ws-1")
	src.set(readyBinding("ws-1", "tenant-a", alice.Owner(), 1, "rt-1", clock.Now()))

	lease := leaseForSess(t, db, b, gwA, "ws-1", false, "sess-1")
	deletePortalSession(t, db, "sess-1")

	// gwB is a different identity: while the session lives it gets
	// ErrDenied; once the session is gone the lease dies and the answer
	// is ErrRevoked.
	if _, err := b.RenewLease(ctx, gwB, lease.ID, fenceOf(lease)); !errors.Is(err, broker.ErrRevoked) {
		t.Fatalf("foreign renew under dead session = %v, want ErrRevoked", err)
	}
	if state, _ := leaseState(t, db, lease.ID); state != "revoked" {
		t.Fatalf("lease state=%q, want revoked", state)
	}
}

// TestLeaseSession_NoDoubleCount: repeated detection of the same dead
// session records one revoke — after the transition the state check
// short-circuits before the session probe.
func TestLeaseSession_NoDoubleCount(t *testing.T) {
	db, _, clock, src := setup(t)
	seedWorkspace(t, db, "tenant-a", alice.Owner(), "ws-1")
	src.set(readyBinding("ws-1", "tenant-a", alice.Owner(), 1, "rt-1", clock.Now()))

	reg := prometheus.NewRegistry()
	m := observability.NewMetrics(reg, nil)
	b := broker.New(db, src, broker.WithClock(clock), broker.WithMetrics(m))

	lease := leaseForSess(t, db, b, gwA, "ws-1", false, "sess-1")
	deletePortalSession(t, db, "sess-1")

	for i := 0; i < 3; i++ {
		_, _ = b.RenewLease(ctx, gwA, lease.ID, fenceOf(lease))
	}
	if err := testutil.GatherAndCompare(reg,
		strings.NewReader(leaseMissingSeries("absent", 1)),
		"tinycdi_lease_session_missing_total"); err != nil {
		t.Fatalf("metric mismatch:\n%v", err)
	}
}
