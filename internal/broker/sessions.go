// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package broker

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/tinyorbitvn/tinycdi/internal/store"
)

// SessionDigest is SHA-256 of the gateway session cookie value. The cookie
// value itself is never stored.
type SessionDigest [sha256.Size]byte

// BindSession records d on the active lease leaseID owned by gw.
// Same digest again: nil. Different digest already bound: ErrDenied.
// Dead lease: ErrRevoked. Unknown lease: ErrLeaseInvalid. Foreign gateway:
// ErrDenied. A digest already bound to a DIFFERENT active lease is denied
// too — one cookie must never resolve to two sessions.
func (b *Broker) BindSession(ctx context.Context, gw GatewayIdentity, leaseID string, d SessionDigest) error {
	now := b.now()
	l, err := b.liveLease(ctx, gw, leaseID, now)
	if err != nil {
		return err
	}
	tag, err := b.db.Pool().Exec(ctx, `
		UPDATE connection_lease SET session_digest = $2
		WHERE id = $1 AND state = 'active' AND expires_at > $3
		  AND (session_digest IS NULL OR session_digest = $2)`,
		l.ID, d[:], now)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return ErrDenied
		}
		return fmt.Errorf("broker: bind session: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// The lease died or expired between the liveness check and this
		// write, or it already carries a different digest — re-run the
		// liveness check to tell the two apart.
		if _, lerr := b.liveLease(ctx, gw, leaseID, now); lerr != nil {
			return lerr
		}
		return ErrDenied
	}
	return nil
}

// LeaseBySession returns the live lease bound to d for gw.
// Unknown digest: ErrLeaseInvalid. Dead lease: ErrRevoked. Foreign gateway:
// ErrDenied. The live lookup goes through the partial unique index
// (session_digest + state = 'active'); the dead-row check runs only on the
// miss path, through the plain session_digest index, so a replayed cookie
// still fails closed as revoked without scanning the unpruned lease table.
func (b *Broker) LeaseBySession(ctx context.Context, gw GatewayIdentity, d SessionDigest) (Lease, error) {
	now := b.now()
	var leaseID string
	err := b.db.Pool().QueryRow(ctx,
		`SELECT id FROM connection_lease
		 WHERE session_digest = $1 AND state = 'active'`, d[:]).
		Scan(&leaseID)
	switch {
	case err == nil:
		return b.liveLease(ctx, gw, leaseID, now)
	case errors.Is(err, pgx.ErrNoRows):
		var bound bool
		if qerr := b.db.Pool().QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM connection_lease WHERE session_digest = $1)`,
			d[:]).Scan(&bound); qerr != nil {
			return Lease{}, fmt.Errorf("broker: session lookup: %w", qerr)
		}
		if !bound {
			return Lease{}, ErrLeaseInvalid
		}
		return Lease{}, ErrRevoked
	default:
		return Lease{}, fmt.Errorf("broker: session lookup: %w", err)
	}
}

// RevokePortalSession ends a portal session's authority over the session
// layer (threat-model S17): inside one transaction it revokes every active
// connection lease minted under the session's digest — so each gateway
// replica's renew loop sees the lease die at its next renew and closes the
// bound stream within one renew cycle, and a replayed workspace cookie
// resolves to a dead lease on any replica — plus every still-outstanding
// launch ticket the session issued, so a ticket in flight at sign-out can
// never mint a replacement lease. Returns the number of leases revoked
// (0 when nothing was live — re-running the revoke is a no-op).
//
// The ticket UPDATE is also the serialization point: RedeemTicket holds a
// FOR UPDATE lock on its ticket row for the whole transaction, so this
// UPDATE waits out any in-flight redemption of this session's tickets.
// Whether the redeem then commits or aborts, the lease UPDATE below runs
// on a fresh READ-COMMITTED snapshot that sees whatever it committed. A
// redeem that starts after this transaction finds revoked_at — or a dead
// session row, which RedeemTicket re-checks — and never mints. The portal
// session row itself is deleted by the caller before this runs: deleting
// it first makes "the session is gone" visible to redemption as early as
// possible, and the store still owns the ordering either way.
func (b *Broker) RevokePortalSession(ctx context.Context, portalSessionID string) (int, error) {
	d := portalSessionDigest(portalSessionID)
	if d == nil {
		return 0, nil
	}
	now := b.now()
	n := 0
	err := b.db.WithTx(ctx, func(tx store.Tx) error {
		if _, err := tx.Exec(ctx, `
			UPDATE launch_ticket SET revoked_at = $2
			WHERE portal_session_digest = $1
			  AND consumed_at IS NULL AND revoked_at IS NULL`, d, now); err != nil {
			return fmt.Errorf("broker: revoke portal tickets: %w", err)
		}
		rows, err := tx.Query(ctx, `
			UPDATE connection_lease SET state = 'revoked', closed_at = $2
			WHERE portal_session_digest = $1 AND state = 'active'
			RETURNING workspace_id, runtime_generation`, d, now)
		if err != nil {
			return fmt.Errorf("broker: revoke portal leases: %w", err)
		}
		var (
			wsUIDs []string
			gens   []int64
		)
		for rows.Next() {
			var wsUID string
			var gen int64
			if err := rows.Scan(&wsUID, &gen); err != nil {
				rows.Close()
				return fmt.Errorf("broker: revoke portal leases: %w", err)
			}
			wsUIDs = append(wsUIDs, wsUID)
			gens = append(gens, gen)
			n++
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("broker: revoke portal leases: %w", err)
		}
		rows.Close()
		// Same rule as RevokeLease: a revoked lease's streams can never
		// report their close, so the transition owns the bound
		// generations' drain accounting in the same transaction.
		for i := range wsUIDs {
			if err := closeStreamsTx(ctx, tx, wsUIDs[i], uint64(gens[i]), now); err != nil {
				return fmt.Errorf("broker: close revoked streams: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}

// PrincipalRevocation counts what RevokePrincipalSessions destroyed — for
// the session.revoke_all audit record, never for control flow.
type PrincipalRevocation struct {
	// Sessions is the number of portal session rows deleted (the caller's
	// included).
	Sessions int
	// Tickets is the number of outstanding launch tickets revoked.
	Tickets int
	// Leases is the number of active connection leases revoked.
	Leases int
}

// RevokePrincipalSessions ends every portal session of a principal inside
// one tenant — sign-out-everywhere (ADR 0007). In a single transaction it
// revokes the principal's outstanding launch tickets, deletes all their
// session rows for the tenant (the caller's own session included — revoke
// everywhere means everywhere), and revokes every active connection lease
// the principal holds in the tenant, including leases minted before
// portal_session_digest existed: the predicate is the principal, not the
// digest. Each gateway replica's renew loop sees the leases die within one
// renew cycle, and a replayed workspace cookie from any revoked session
// resolves to a dead lease on any replica.
//
// Lock order is identical to the other session-layer mutators — tickets,
// then sessions, then leases. The ticket UPDATE is the serialization
// point, exactly as in RevokePortalSession: RedeemTicket holds a
// FOR UPDATE lock on its ticket row for the whole transaction, so this
// UPDATE waits out any in-flight redemption of the principal's tickets.
// Whether a redeem then commits or aborts, the lease UPDATE below runs on
// a fresh READ-COMMITTED snapshot that sees whatever it committed; a
// redeem that starts after this transaction finds revoked_at — or the
// deleted session row, which RedeemTicket re-checks — and never mints.
// Re-running revokes zero rows: the operation is idempotent.
func (b *Broker) RevokePrincipalSessions(ctx context.Context, tenantID, issuer, subject string) (PrincipalRevocation, error) {
	principal := issuer + "|" + subject
	now := b.now()
	var res PrincipalRevocation
	err := b.db.WithTx(ctx, func(tx store.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE launch_ticket SET revoked_at = $3
			WHERE tenant_id = $1 AND principal_subject = $2
			  AND consumed_at IS NULL AND revoked_at IS NULL`,
			tenantID, principal, now)
		if err != nil {
			return fmt.Errorf("broker: revoke principal tickets: %w", err)
		}
		res.Tickets = int(tag.RowsAffected())
		tag, err = tx.Exec(ctx, `
			DELETE FROM sessions
			WHERE issuer = $2 AND subject = $3 AND tenant_id = $1`,
			tenantID, issuer, subject)
		if err != nil {
			return fmt.Errorf("broker: delete principal sessions: %w", err)
		}
		res.Sessions = int(tag.RowsAffected())
		rows, err := tx.Query(ctx, `
			UPDATE connection_lease SET state = 'revoked', closed_at = $3
			WHERE tenant_id = $1 AND principal_subject = $2 AND state = 'active'
			RETURNING workspace_id, runtime_generation`,
			tenantID, principal, now)
		if err != nil {
			return fmt.Errorf("broker: revoke principal leases: %w", err)
		}
		var (
			wsUIDs []string
			gens   []int64
		)
		for rows.Next() {
			var wsUID string
			var gen int64
			if err := rows.Scan(&wsUID, &gen); err != nil {
				rows.Close()
				return fmt.Errorf("broker: revoke principal leases: %w", err)
			}
			wsUIDs = append(wsUIDs, wsUID)
			gens = append(gens, gen)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("broker: revoke principal leases: %w", err)
		}
		rows.Close()
		res.Leases = len(wsUIDs)
		// Same rule as RevokeLease: a revoked lease's streams can never
		// report their close, so the transition owns the bound
		// generations' drain accounting in the same transaction.
		for i := range wsUIDs {
			if err := closeStreamsTx(ctx, tx, wsUIDs[i], uint64(gens[i]), now); err != nil {
				return fmt.Errorf("broker: close revoked streams: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return PrincipalRevocation{}, err
	}
	return res, nil
}

// StreamOwnerTabLen is the fixed length of a stream-owner tab id: 128 bits
// as lowercase hex (32 chars) — what the portal mints per browser tab and
// sends with its stream claim (see migration 018).
const StreamOwnerTabLen = 32

// ValidStreamOwnerTab reports whether s is a well-formed stream-owner tab
// id: exactly 32 lowercase hex characters. Anything else — a missing,
// truncated or non-hex value — is not a usable owner id: it must be stored
// as NULL and never trusted as a match.
func ValidStreamOwnerTab(s string) bool {
	if len(s) != StreamOwnerTabLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// ownerTabOrNull normalizes a claimed tab id for storage: the id itself
// when valid, nil (SQL NULL) otherwise. A legacy claim without an id must
// overwrite a previous id too — the row always reflects the CURRENT
// stream's owner, not the last valid one.
func ownerTabOrNull(s string) *string {
	if !ValidStreamOwnerTab(s) {
		return nil
	}
	return &s
}

// ClaimStream increments the lease's stream epoch and returns the new value.
// It applies the same liveness and fence checks as RenewLease: a stale or
// foreign incarnation can never claim a stream.
//
// ownerTab is the claiming tab's id; it lands on the lease in the same row
// update that bumps stream_epoch, so the epoch and its owner can never
// diverge. An empty or malformed id is stored as NULL (a legacy claim) —
// never as a matchable owner.
func (b *Broker) ClaimStream(ctx context.Context, gw GatewayIdentity, leaseID string, fence Fence, ownerTab string) (uint64, error) {
	now := b.now()
	l, err := b.liveLease(ctx, gw, leaseID, now)
	if err != nil {
		return 0, err
	}
	if fence.WorkspaceUID != l.WorkspaceUID ||
		fence.RuntimeGeneration != l.RuntimeGeneration ||
		fence.RuntimeUID != l.RuntimeUID ||
		fence.FencingVersion != l.FencingVersion {
		return 0, ErrStaleBinding
	}
	if _, err := b.boundCurrent(ctx, l, now); err != nil {
		return 0, err
	}
	// Claim and release the previous stream's slot atomically: the claim
	// fences every earlier stream of this lease, so its open_streams share
	// is dead whether or not its replica ever reports the disconnect (a
	// hard-killed replica never does). The new stream's "connected" report
	// sets the count back to 1 and clears the grace window.
	var epoch uint64
	err = b.db.WithTx(ctx, func(tx store.Tx) error {
		// stream_owner_epoch repeats the new epoch: a replica predating the
		// column bumps stream_epoch without naming the owner columns, and a
		// stored id whose epoch no longer matches is stale evidence — read
		// as absent, never as a match (R-V3c).
		if err := tx.QueryRow(ctx,
			`UPDATE connection_lease SET stream_epoch = stream_epoch + 1,
				stream_owner_tab = $2::text,
				stream_owner_epoch = CASE WHEN $2::text IS NULL THEN NULL ELSE stream_epoch + 1 END
			 WHERE id = $1 AND state = 'active' RETURNING stream_epoch`, l.ID, ownerTabOrNull(ownerTab)).
			Scan(&epoch); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrRevoked
			}
			return fmt.Errorf("broker: claim stream: %w", err)
		}
		if err := closeStreamsTx(ctx, tx, l.WorkspaceUID, l.RuntimeGeneration, now); err != nil {
			return fmt.Errorf("broker: claim stream accounting: %w", err)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return epoch, nil
}
