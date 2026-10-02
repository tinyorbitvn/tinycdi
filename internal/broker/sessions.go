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

// ClaimStream increments the lease's stream epoch and returns the new value.
// It applies the same liveness and fence checks as RenewLease: a stale or
// foreign incarnation can never claim a stream.
func (b *Broker) ClaimStream(ctx context.Context, gw GatewayIdentity, leaseID string, fence Fence) (uint64, error) {
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
		if err := tx.QueryRow(ctx,
			`UPDATE connection_lease SET stream_epoch = stream_epoch + 1
			 WHERE id = $1 AND state = 'active' RETURNING stream_epoch`, l.ID).
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
