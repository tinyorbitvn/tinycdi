package broker_test

// ADR 0007: sign-out-everywhere. Broker.RevokePrincipalSessions ends every
// portal session of a principal inside ONE tenant — in a single
// transaction it revokes the principal's outstanding launch tickets,
// deletes all their session rows for the tenant (caller's included), and
// revokes every active lease the principal holds there, including
// pre-portal_session_digest rows. Streams die within one renew cycle on
// every replica and a replayed workspace cookie resolves to a dead lease.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tinyorbitvn/tinycdi/internal/api"
	"github.com/tinyorbitvn/tinycdi/internal/broker"
	"github.com/tinyorbitvn/tinycdi/internal/store"
)

// seedSessionRowFor is seedPortalSession with an explicit principal and
// tenant: revoke-all matches session rows by (issuer, subject, tenant_id),
// so the test rows must carry the caller's real identity.
func seedSessionRowFor(t *testing.T, db *store.DB, sessionID, issuer, subject, tenantID string) {
	t.Helper()
	sum := sha256.Sum256([]byte(sessionID))
	_, err := db.Pool().Exec(ctx, `
		INSERT INTO sessions (id, issuer, subject, tenant_id, groups,
			created_at, last_seen_at, expires_at, epoch)
		VALUES ($1, $2, $3, $4, '[]', now(), now(),
			now() + interval '1 hour',
			(SELECT value FROM platform_meta WHERE key = 'session_epoch'))
		ON CONFLICT (id) DO UPDATE SET
			issuer = EXCLUDED.issuer, subject = EXCLUDED.subject,
			tenant_id = EXCLUDED.tenant_id`,
		hex.EncodeToString(sum[:]), issuer, subject, tenantID)
	if err != nil {
		t.Fatalf("seed session row: %v", err)
	}
}

// sessionAlive reports whether the session row for sessionID still exists.
func sessionAlive(t *testing.T, db *store.DB, sessionID string) bool {
	t.Helper()
	sum := sha256.Sum256([]byte(sessionID))
	var alive bool
	if err := db.Pool().QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM sessions WHERE id = $1)`,
		hex.EncodeToString(sum[:])).Scan(&alive); err != nil {
		t.Fatalf("session probe: %v", err)
	}
	return alive
}

// leaseForPrincipal is leaseFor with an explicit principal — the lease's
// tenant_id/principal_subject come from the issuing principal, which is
// exactly what the principal-scoped revoke matches on.
func leaseForPrincipal(t *testing.T, b *broker.Broker, gw broker.GatewayIdentity, p api.Principal, wsUID broker.PlatformID, takeover bool) broker.Lease {
	t.Helper()
	tk, err := b.IssueTicket(ctx, p, wsUID, takeover, "", "")
	if err != nil {
		t.Fatalf("IssueTicket: %v", err)
	}
	lease, err := b.RedeemTicket(ctx, gw, tk.Token)
	if err != nil {
		t.Fatalf("RedeemTicket: %v", err)
	}
	return lease
}

var (
	carol  = api.Principal{Issuer: "https://idp.example", Subject: "carol", TenantID: "tenant-a"}
	aliceB = api.Principal{Issuer: "https://idp.example", Subject: "alice", TenantID: "tenant-b"}
)

// TestRevokePrincipalSessions_RevokesAllInTenant: every session row of the
// principal in the tenant dies (caller's included), every active lease —
// digest-bound or pre-digest legacy — is revoked with drain accounting,
// every outstanding ticket can never redeem, and each revoked session's
// workspace cookie resolves to a dead lease. Other principals and other
// tenants are untouched.
func TestRevokePrincipalSessions_RevokesAllInTenant(t *testing.T) {
	db, b, clock, src := setup(t)
	for _, ws := range []string{"ws-1", "ws-2", "ws-3", "ws-t1", "ws-t2"} {
		seedWorkspace(t, db, "tenant-a", alice.Owner(), ws)
		src.set(readyBinding(broker.PlatformID(ws), "tenant-a", alice.Owner(), 1, "rt-1", clock.Now()))
	}
	seedWorkspace(t, db, "tenant-b", aliceB.Owner(), "ws-b")
	src.set(readyBinding("ws-b", "tenant-b", aliceB.Owner(), 1, "rt-b", clock.Now()))
	seedWorkspace(t, db, "tenant-a", carol.Owner(), "ws-c")
	src.set(readyBinding("ws-c", "tenant-a", carol.Owner(), 1, "rt-c", clock.Now()))

	// Two sessions of alice in tenant-a, each holding a live lease and an
	// outstanding ticket; one legacy lease with no digest binding; alice's
	// tenant-b session + material and carol's tenant-a lease survive.
	seedSessionRowFor(t, db, "sess-1", alice.Issuer, alice.Subject, "tenant-a")
	seedSessionRowFor(t, db, "sess-2", alice.Issuer, alice.Subject, "tenant-a")
	seedSessionRowFor(t, db, "sess-b", alice.Issuer, alice.Subject, "tenant-b")

	l1 := leaseForSess(t, db, b, gwA, "ws-1", false, "sess-1")
	l2 := leaseForSess(t, db, b, gwA, "ws-2", false, "sess-2")
	legacy := leaseFor(t, b, gwA, "ws-3", false) // NULL portal_session_digest
	lCarol := leaseForPrincipal(t, b, gwA, carol, "ws-c", false)

	d1 := digestOf("cookie-1")
	d2 := digestOf("cookie-2")
	if err := b.BindSession(ctx, gwA, l1.ID, d1); err != nil {
		t.Fatalf("BindSession 1: %v", err)
	}
	if err := b.BindSession(ctx, gwA, l2.ID, d2); err != nil {
		t.Fatalf("BindSession 2: %v", err)
	}

	tk1, err := b.IssueTicket(ctx, alice, "ws-t1", false, "", "sess-1")
	if err != nil {
		t.Fatalf("IssueTicket t1: %v", err)
	}
	tk2, err := b.IssueTicket(ctx, alice, "ws-t2", false, "", "sess-2")
	if err != nil {
		t.Fatalf("IssueTicket t2: %v", err)
	}
	tkB, err := b.IssueTicket(ctx, aliceB, "ws-b", false, "", "sess-b")
	if err != nil {
		t.Fatalf("IssueTicket tenant-b: %v", err)
	}

	res, err := b.RevokePrincipalSessions(ctx, "tenant-a", alice.Issuer, alice.Subject)
	if err != nil {
		t.Fatalf("RevokePrincipalSessions: %v", err)
	}
	if res.Sessions != 2 || res.Tickets != 2 || res.Leases != 3 {
		t.Fatalf("counts = %+v, want {Sessions:2 Tickets:2 Leases:3}", res)
	}

	// Sessions: both tenant-a rows gone, tenant-b untouched.
	for _, id := range []string{"sess-1", "sess-2"} {
		if sessionAlive(t, db, id) {
			t.Fatalf("session %s survived revoke-all", id)
		}
	}
	if !sessionAlive(t, db, "sess-b") {
		t.Fatal("tenant-b session died — revoke crossed the tenant boundary")
	}

	// Every revoked lease fails renew; each session's replayed cookie
	// resolves to a dead lease (the 401 the workspace host answers).
	for _, l := range []broker.Lease{l1, l2, legacy} {
		if _, err := b.RenewLease(ctx, gwA, l.ID, fenceOf(l)); !errors.Is(err, broker.ErrRevoked) {
			t.Fatalf("renew lease %s = %v, want ErrRevoked", l.ID, err)
		}
	}
	for _, d := range []broker.SessionDigest{d1, d2} {
		if _, err := b.LeaseBySession(ctx, gwA, d); !errors.Is(err, broker.ErrRevoked) {
			t.Fatalf("replayed cookie = %v, want ErrRevoked", err)
		}
	}

	// Outstanding tickets can never mint now.
	for _, tk := range []broker.Ticket{tk1, tk2} {
		if _, err := b.RedeemTicket(ctx, gwA, tk.Token); !errors.Is(err, broker.ErrRevoked) {
			t.Fatalf("redeem revoked-all ticket = %v, want ErrRevoked", err)
		}
	}

	// Carol's lease and alice's tenant-b ticket are still alive.
	if _, err := b.RenewLease(ctx, gwA, lCarol.ID, fenceOf(lCarol)); err != nil {
		t.Fatalf("other principal's lease revoked: %v", err)
	}
	if _, err := b.RedeemTicket(ctx, gwA, tkB.Token); err != nil {
		t.Fatalf("tenant-b ticket died with tenant-a revoke: %v", err)
	}
}

// TestRevokePrincipalSessions_TenantScoped: a principal signed in under
// two tenants revokes only the caller's tenant — sessions, leases and
// outstanding tickets in the other tenant all survive.
func TestRevokePrincipalSessions_TenantScoped(t *testing.T) {
	db, b, clock, src := setup(t)
	seedWorkspace(t, db, "tenant-a", alice.Owner(), "ws-a")
	seedWorkspace(t, db, "tenant-b", aliceB.Owner(), "ws-b")
	src.set(readyBinding("ws-a", "tenant-a", alice.Owner(), 1, "rt-a", clock.Now()))
	src.set(readyBinding("ws-b", "tenant-b", aliceB.Owner(), 1, "rt-b", clock.Now()))
	seedSessionRowFor(t, db, "sess-a", alice.Issuer, alice.Subject, "tenant-a")
	seedSessionRowFor(t, db, "sess-b", alice.Issuer, alice.Subject, "tenant-b")

	leaseA := leaseFor(t, b, gwA, "ws-a", false)
	tkB, err := b.IssueTicket(ctx, aliceB, "ws-b", false, "", "sess-b")
	if err != nil {
		t.Fatalf("IssueTicket tenant-b: %v", err)
	}

	res, err := b.RevokePrincipalSessions(ctx, "tenant-a", alice.Issuer, alice.Subject)
	if err != nil {
		t.Fatalf("RevokePrincipalSessions: %v", err)
	}
	if res.Sessions != 1 || res.Leases != 1 {
		t.Fatalf("counts = %+v, want {Sessions:1 Leases:1}", res)
	}
	if _, err := b.RenewLease(ctx, gwA, leaseA.ID, fenceOf(leaseA)); !errors.Is(err, broker.ErrRevoked) {
		t.Fatalf("tenant-a lease = %v, want ErrRevoked", err)
	}
	// Same principal string, other tenant: session row lives, ticket
	// still redeems to a live lease.
	if !sessionAlive(t, db, "sess-b") {
		t.Fatal("tenant-b session deleted by a tenant-a revoke")
	}
	leaseB, err := b.RedeemTicket(ctx, gwA, tkB.Token)
	if err != nil {
		t.Fatalf("tenant-b redeem after tenant-a revoke: %v", err)
	}
	if _, err := b.RenewLease(ctx, gwA, leaseB.ID, fenceOf(leaseB)); err != nil {
		t.Fatalf("tenant-b lease dead: %v", err)
	}
}

// TestRevokePrincipalSessions_Idempotent: a second call destroys nothing
// and still succeeds — safe to retry after a 500, safe under a double
// click.
func TestRevokePrincipalSessions_Idempotent(t *testing.T) {
	db, b, clock, src := setup(t)
	seedWorkspace(t, db, "tenant-a", alice.Owner(), "ws-1")
	src.set(readyBinding("ws-1", "tenant-a", alice.Owner(), 1, "rt-1", clock.Now()))
	seedSessionRowFor(t, db, "sess-1", alice.Issuer, alice.Subject, "tenant-a")
	leaseForSess(t, db, b, gwA, "ws-1", false, "sess-1")

	first, err := b.RevokePrincipalSessions(ctx, "tenant-a", alice.Issuer, alice.Subject)
	if err != nil {
		t.Fatalf("first revoke: %v", err)
	}
	if first.Sessions != 1 || first.Leases != 1 {
		t.Fatalf("first = %+v, want Sessions 1 Leases 1", first)
	}
	second, err := b.RevokePrincipalSessions(ctx, "tenant-a", alice.Issuer, alice.Subject)
	if err != nil {
		t.Fatalf("second revoke: %v", err)
	}
	if second != (broker.PrincipalRevocation{}) {
		t.Fatalf("second = %+v, want all zero", second)
	}
	// Unknown principal / tenant: a clean no-op, not an error.
	empty, err := b.RevokePrincipalSessions(ctx, "tenant-a", "https://idp.example", "nobody")
	if err != nil || empty != (broker.PrincipalRevocation{}) {
		t.Fatalf("unknown principal = (%+v, %v)", empty, err)
	}
}

// TestRevokePrincipalSessions_RedeemCommitThenRevoke drives the
// redeem-wins ordering of the ticket-lock serialization — the same
// interleave TestRevokePortalSession_RedeemCommitThenRevoke proves for the
// per-session revoke: an in-flight redemption holds its ticket row FOR
// UPDATE, so the principal revoke's ticket UPDATE blocks on it; the
// redemption commits, the revoke proceeds on a fresh snapshot and still
// revokes the lease the redeem minted. Never a live lease for a destroyed
// session.
func TestRevokePrincipalSessions_RedeemCommitThenRevoke(t *testing.T) {
	db, b, clock, src := setup(t)
	seedWorkspace(t, db, "tenant-a", alice.Owner(), "ws-1")
	src.set(readyBinding("ws-1", "tenant-a", alice.Owner(), 1, "rt-1", clock.Now()))
	seedSessionRowFor(t, db, "sess-1", alice.Issuer, alice.Subject, "tenant-a")

	tk, err := b.IssueTicket(ctx, alice, "ws-1", false, "", "sess-1")
	if err != nil {
		t.Fatalf("IssueTicket: %v", err)
	}
	hash := sha256.Sum256([]byte(tk.Token))
	digest := sha256.Sum256([]byte("sess-1"))

	// The in-flight redemption, paused holding the ticket row FOR UPDATE.
	conn, err := db.Pool().Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	redeemTx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("redeem tx: %v", err)
	}
	defer redeemTx.Rollback(ctx)
	if _, err := redeemTx.Exec(ctx,
		`SELECT ticket_hash FROM launch_ticket WHERE ticket_hash = $1 FOR UPDATE`,
		hash[:]); err != nil {
		t.Fatalf("redeem ticket lock: %v", err)
	}

	type revokeRes struct {
		res broker.PrincipalRevocation
		err error
	}
	revoked := make(chan revokeRes, 1)
	go func() {
		res, err := b.RevokePrincipalSessions(ctx, "tenant-a", alice.Issuer, alice.Subject)
		revoked <- revokeRes{res, err}
	}()
	waitForLockWait(t, db, `%UPDATE launch_ticket%`)

	// The redemption finishes — its own two writes — and commits,
	// releasing the lock the revoke waits on.
	now := clock.Now()
	if _, err := redeemTx.Exec(ctx,
		`UPDATE launch_ticket SET consumed_at = $2 WHERE ticket_hash = $1`,
		hash[:], now); err != nil {
		t.Fatalf("redeem consume: %v", err)
	}
	if _, err := redeemTx.Exec(ctx, `
		INSERT INTO connection_lease
			(id, workspace_id, tenant_id, principal_subject,
			 runtime_generation, runtime_uid, fencing_version, gateway_id,
			 state, created_at, expires_at, last_renewed_at,
			 portal_session_digest)
		 VALUES ('lease-race', 'ws-1', 'tenant-a', $1,
			 1, 'rt-1', 1, $2, 'active', $3, $4, $3, $5)`,
		alice.Owner(), gwA.ID, now, now.Add(time.Minute), digest[:]); err != nil {
		t.Fatalf("redeem lease insert: %v", err)
	}
	if err := redeemTx.Commit(ctx); err != nil {
		t.Fatalf("redeem commit: %v", err)
	}

	got := <-revoked
	if got.err != nil {
		t.Fatalf("RevokePrincipalSessions: %v", got.err)
	}
	if got.res.Leases != 1 {
		t.Fatalf("revoked %d leases, want 1 — the lease the racing redeem minted", got.res.Leases)
	}
	if got.res.Sessions != 1 {
		t.Fatalf("deleted %d sessions, want 1", got.res.Sessions)
	}
	// The committed redemption kept the ticket consumed, not revoked.
	var consumedAt, revokedAt *time.Time
	if err := db.Pool().QueryRow(ctx,
		`SELECT consumed_at, revoked_at FROM launch_ticket WHERE ticket_hash = $1`,
		hash[:]).Scan(&consumedAt, &revokedAt); err != nil {
		t.Fatalf("ticket row: %v", err)
	}
	if consumedAt == nil || revokedAt != nil {
		t.Fatalf("ticket consumed_at=%v revoked_at=%v — want consumed, not revoked",
			consumedAt, revokedAt)
	}
	// And the lease it minted is dead — no live lease survives a destroyed
	// session.
	minted := broker.Lease{
		ID: "lease-race", WorkspaceUID: "ws-1",
		RuntimeGeneration: 1, RuntimeUID: "rt-1", FencingVersion: 1,
	}
	if _, err := b.RenewLease(ctx, gwA, minted.ID, fenceOf(minted)); !errors.Is(err, broker.ErrRevoked) {
		t.Fatalf("renew lease minted mid-revoke-all = %v, want ErrRevoked", err)
	}
}

// TestRevokePrincipalSessions_RevokeCommitThenRedeem: the reverse
// ordering — the revoke's ticket UPDATE lands first; a redemption started
// while it holds the row wakes onto revoked_at and never mints.
func TestRevokePrincipalSessions_RevokeCommitThenRedeem(t *testing.T) {
	db, b, clock, src := setup(t)
	seedWorkspace(t, db, "tenant-a", alice.Owner(), "ws-1")
	src.set(readyBinding("ws-1", "tenant-a", alice.Owner(), 1, "rt-1", clock.Now()))
	seedSessionRowFor(t, db, "sess-1", alice.Issuer, alice.Subject, "tenant-a")

	tk, err := b.IssueTicket(ctx, alice, "ws-1", false, "", "sess-1")
	if err != nil {
		t.Fatalf("IssueTicket: %v", err)
	}
	hash := sha256.Sum256([]byte(tk.Token))

	// The in-flight revoke, paused after its ticket UPDATE — the same row
	// lock RevokePrincipalSessions's first statement takes.
	conn, err := db.Pool().Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	revokeTx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("revoke tx: %v", err)
	}
	defer revokeTx.Rollback(ctx)
	if _, err := revokeTx.Exec(ctx, `
		UPDATE launch_ticket SET revoked_at = $2
		WHERE ticket_hash = $1
		  AND consumed_at IS NULL AND revoked_at IS NULL`,
		hash[:], clock.Now()); err != nil {
		t.Fatalf("revoke ticket update: %v", err)
	}

	type redeemRes struct {
		lease broker.Lease
		err   error
	}
	redeemed := make(chan redeemRes, 1)
	go func() {
		l, err := b.RedeemTicket(ctx, gwA, tk.Token)
		redeemed <- redeemRes{l, err}
	}()
	waitForLockWait(t, db, `%launch_ticket%FOR UPDATE%`)

	if err := revokeTx.Commit(ctx); err != nil {
		t.Fatalf("revoke commit: %v", err)
	}
	res := <-redeemed
	if !errors.Is(res.err, broker.ErrRevoked) {
		t.Fatalf("redeem under committed revoke-all = %v, want ErrRevoked", res.err)
	}
	var leases int
	if err := db.Pool().QueryRow(ctx,
		`SELECT COUNT(*) FROM connection_lease WHERE workspace_id = 'ws-1'`).
		Scan(&leases); err != nil {
		t.Fatalf("lease count: %v", err)
	}
	if leases != 0 {
		t.Fatalf("%d lease(s) minted under a revoked principal, want 0", leases)
	}
}

// TestRevokePrincipalSessions_ConcurrentNoDeadlock is the -race
// interleaving the advisor asked for (ADR 0007): revoke-all racing a real
// redemption AND a per-session sign-out of the same principal — three
// mutators sharing the tickets → sessions → leases lock order, so none can
// deadlock, and whatever the redeem committed the revoke still covers.
// Afterwards: no active lease remains for the destroyed principal and no
// outstanding ticket can redeem.
func TestRevokePrincipalSessions_ConcurrentNoDeadlock(t *testing.T) {
	db, b, clock, src := setup(t)
	for _, ws := range []string{"ws-1", "ws-2", "ws-3"} {
		seedWorkspace(t, db, "tenant-a", alice.Owner(), ws)
		src.set(readyBinding(broker.PlatformID(ws), "tenant-a", alice.Owner(), 1, "rt-1", clock.Now()))
	}
	seedSessionRowFor(t, db, "sess-1", alice.Issuer, alice.Subject, "tenant-a")
	seedSessionRowFor(t, db, "sess-2", alice.Issuer, alice.Subject, "tenant-a")
	leaseForSess(t, db, b, gwA, "ws-2", false, "sess-2")

	tkRace, err := b.IssueTicket(ctx, alice, "ws-1", false, "", "sess-1")
	if err != nil {
		t.Fatalf("IssueTicket race: %v", err)
	}
	tkDead, err := b.IssueTicket(ctx, alice, "ws-3", false, "", "sess-2")
	if err != nil {
		t.Fatalf("IssueTicket dead: %v", err)
	}

	var wg sync.WaitGroup
	done := make(chan struct{})
	wg.Add(3)
	go func() { wg.Wait(); close(done) }()

	go func() {
		defer wg.Done()
		// Sign-out-everywhere for alice/tenant-a.
		if _, err := b.RevokePrincipalSessions(ctx, "tenant-a", alice.Issuer, alice.Subject); err != nil {
			t.Errorf("RevokePrincipalSessions: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		// A redemption in flight the whole time — win or lose, its lease
		// must not survive.
		_, _ = b.RedeemTicket(ctx, gwA, tkRace.Token)
	}()
	go func() {
		defer wg.Done()
		// The per-session sign-out of sess-2 racing the principal revoke
		// (same delete-then-revoke shape LogoutHandler produces).
		sum := sha256.Sum256([]byte("sess-2"))
		if _, err := db.Pool().Exec(ctx,
			`DELETE FROM sessions WHERE id = $1`, hex.EncodeToString(sum[:])); err != nil {
			t.Errorf("delete sess-2: %v", err)
		}
		if _, err := b.RevokePortalSession(ctx, "sess-2"); err != nil {
			t.Errorf("RevokePortalSession: %v", err)
		}
	}()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("concurrent revoke-all / redeem / sign-out deadlocked")
	}

	// Whatever order the three ran in, the principal's tenant-a material
	// is gone: no live lease, no outstanding ticket, no session rows.
	var live int
	if err := db.Pool().QueryRow(ctx,
		`SELECT COUNT(*) FROM connection_lease
		 WHERE tenant_id = 'tenant-a' AND principal_subject = $1 AND state = 'active'`,
		alice.Owner()).Scan(&live); err != nil {
		t.Fatalf("live lease count: %v", err)
	}
	if live != 0 {
		t.Fatalf("%d active lease(s) survived revoke-all", live)
	}
	var outstanding int
	if err := db.Pool().QueryRow(ctx,
		`SELECT COUNT(*) FROM launch_ticket
		 WHERE tenant_id = 'tenant-a' AND principal_subject = $1
		   AND consumed_at IS NULL AND revoked_at IS NULL`,
		alice.Owner()).Scan(&outstanding); err != nil {
		t.Fatalf("outstanding ticket count: %v", err)
	}
	if outstanding != 0 {
		t.Fatalf("%d outstanding ticket(s) survived revoke-all", outstanding)
	}
	for _, id := range []string{"sess-1", "sess-2"} {
		if sessionAlive(t, db, id) {
			t.Fatalf("session %s survived the interleave", id)
		}
	}
	if _, err := b.RedeemTicket(ctx, gwA, tkDead.Token); !errors.Is(err, broker.ErrRevoked) {
		t.Fatalf("late redeem = %v, want ErrRevoked", err)
	}
}

// TestMigration023_Idempotent: the principal-scoped revocation indexes
// land on a fresh database and on re-run.
func TestMigration023_Idempotent(t *testing.T) {
	db := newDB(t)
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	for _, name := range []string{"connection_lease_principal_active", "launch_ticket_principal_outstanding"} {
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
