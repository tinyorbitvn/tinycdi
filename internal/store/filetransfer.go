// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"fmt"
	"time"
)

// File-transfer row lifetimes (ADR 0009 §5).
const (
	// FileTransferTTL is how long a minted row may wait for its claim.
	FileTransferTTL = 2 * time.Minute
	// FileTransferClaimDeadAfter is how long a claimed row counts as a live
	// stream; past it the sweep fails the row and frees the workspace.
	FileTransferClaimDeadAfter = 30 * time.Minute
	// FileTransferKeep is how long finished rows stay for audit joins.
	FileTransferKeep = 7 * 24 * time.Hour
)

// FileTransfer is one minted, single-use transfer grant.
type FileTransfer struct {
	ID                string
	WorkspaceID       string
	Tenant            string
	PrincipalIssuer   string
	PrincipalSubject  string
	RuntimeGeneration int64
	RuntimeUID        string
	Direction         string // upload | download
	Path              string // a single file name inside the transfer root
	DeclaredBytes     int64
	ResolvedPolicy    string // the file-transfer mode in force at mint
	ExpiresAt         time.Time
}

// InsertFileTransfer records a pending grant.
func InsertFileTransfer(ctx context.Context, db *DB, t FileTransfer) error {
	// In a transaction so the request-scoped audit hook (Task 1) commits the
	// audit row with the grant, or rolls both back.
	if err := db.WithTx(ctx, func(tx Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO session_file_transfer (id, workspace_id, tenant, principal_issuer, principal_subject,
				runtime_generation, runtime_uid, direction, path, declared_bytes, resolved_policy, expires_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
			t.ID, t.WorkspaceID, t.Tenant, t.PrincipalIssuer, t.PrincipalSubject,
			t.RuntimeGeneration, t.RuntimeUID, t.Direction, t.Path, t.DeclaredBytes, t.ResolvedPolicy, t.ExpiresAt)
		return err
	}); err != nil {
		return fmt.Errorf("store: insert file transfer %w", err)
	}
	return nil
}

// SweepFileTransfers expires unclaimed rows past their TTL, fails claims
// older than FileTransferClaimDeadAfter, and prunes rows created before
// now - FileTransferKeep that are no longer pending or claimed.
func (d *DB) SweepFileTransfers(ctx context.Context, now time.Time) (expired, failed, pruned int64, err error) {
	tag, err := d.pool.Exec(ctx,
		`UPDATE session_file_transfer SET status = 'expired' WHERE status = 'pending' AND expires_at <= $1`, now)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("store: expire transfers %w", err)
	}
	expired = tag.RowsAffected()
	tag, err = d.pool.Exec(ctx,
		`UPDATE session_file_transfer SET status = 'failed', completed_at = $1
		 WHERE status = 'claimed' AND claimed_at <= $2`, now, now.Add(-FileTransferClaimDeadAfter))
	if err != nil {
		return expired, 0, 0, fmt.Errorf("store: fail dead transfers %w", err)
	}
	failed = tag.RowsAffected()
	tag, err = d.pool.Exec(ctx,
		`DELETE FROM session_file_transfer WHERE created_at < $1 AND status NOT IN ('pending', 'claimed')`,
		now.Add(-FileTransferKeep))
	if err != nil {
		return expired, failed, 0, fmt.Errorf("store: prune transfers %w", err)
	}
	return expired, failed, tag.RowsAffected(), nil
}
