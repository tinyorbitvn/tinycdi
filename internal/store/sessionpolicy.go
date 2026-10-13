// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// SessionPolicyCeiling is the tenant_session_policy row: the tenant
// administrator's ceiling over every template's own session data-transfer
// policy (ADR 0010 §1). A nil channel column means FollowTemplate — the
// channel is not narrowed — and the store keeps "no row" as the single
// representation of an all-defaults ceiling (SetCeiling deletes a row
// whose every ceiling column went NULL).
type SessionPolicyCeiling struct {
	// HasRow reports whether a tenant_session_policy row exists; false
	// means the ceiling is FollowTemplate on every channel.
	HasRow       bool
	Clipboard    *string
	FileTransfer *string
	FileMaxBytes *int64
	// Version is the row's change token (FormatVersion of updated_at):
	// it changes on every write and is empty when no row exists. The API
	// derives the resource's ETag from it.
	Version string
}

// SessionPolicyWrite is the ceiling vector a PUT installs. A nil field
// stores NULL (FollowTemplate); a write whose every ceiling field is nil
// deletes the row instead — the single representation of the default.
type SessionPolicyWrite struct {
	Clipboard    *string
	FileTransfer *string
	FileMaxBytes *int64
}

// Defaults reports whether the write sets every channel back to
// FollowTemplate — the state represented by no row.
func (w SessionPolicyWrite) Defaults() bool {
	return w.Clipboard == nil && w.FileTransfer == nil && w.FileMaxBytes == nil
}

// SessionPolicyStore is the Postgres seam behind
// /v1/admin/tenants/{tenant}/session-policy and the session-policy
// resolver: one conditional write path (check and write cannot be raced)
// and the ceiling read both consumers share.
type SessionPolicyStore struct{ db *DB }

// NewSessionPolicyStore wraps db.
func NewSessionPolicyStore(db *DB) *SessionPolicyStore { return &SessionPolicyStore{db: db} }

// policyQuerier is satisfied by *pgxpool.Pool and pgx.Tx, so the ceiling
// read runs standalone (the admin GET, the ticket-issue resolver) or
// inside the intent transaction (create/start).
type policyQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// ceilingFrom reads the ceiling row through q; absent row is
// HasRow=false, never an error.
func ceilingFrom(ctx context.Context, q policyQuerier, tenantID string) (SessionPolicyCeiling, error) {
	var c SessionPolicyCeiling
	var updatedAt time.Time
	err := q.QueryRow(ctx, `
		SELECT clipboard, file_transfer, file_max_bytes, updated_at
		FROM tenant_session_policy WHERE tenant_id = $1`, tenantID).
		Scan(&c.Clipboard, &c.FileTransfer, &c.FileMaxBytes, &updatedAt)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return SessionPolicyCeiling{}, nil
	case err != nil:
		return SessionPolicyCeiling{}, err
	}
	c.HasRow = true
	c.Version = FormatVersion(updatedAt)
	return c, nil
}

// Ceiling reads the tenant's row through the pool; absent row is
// HasRow=false, never an error.
func (s *SessionPolicyStore) Ceiling(ctx context.Context, tenantID string) (SessionPolicyCeiling, error) {
	return ceilingFrom(ctx, s.db.Pool(), tenantID)
}

// CeilingTx reads the tenant's row inside tx — the resolution the create
// and start intents take must see the same snapshot the intent records.
func (s *SessionPolicyStore) CeilingTx(ctx context.Context, tx Tx, tenantID string) (SessionPolicyCeiling, error) {
	return ceilingFrom(ctx, tx, tenantID)
}

// SetCeiling writes the ceiling conditionally and returns the row's new
// version ("" when the write deleted the row). create expects no row: the
// write inserts only when none exists. Otherwise expectedVersion must name
// the row's current version; the check rides in the UPDATE/DELETE itself,
// so it cannot be raced. Every miss is ErrVersionMismatch. An all-defaults
// write deletes the row — "no row" and "all-defaults row" are the same
// state, and the store keeps exactly one representation of it; under
// create it is a no-op that still refuses a row someone else created.
func (s *SessionPolicyStore) SetCeiling(ctx context.Context, tenantID string, w SessionPolicyWrite, expectedVersion string, create bool) (string, error) {
	var version string
	err := s.db.WithTx(ctx, func(tx Tx) error {
		if w.Defaults() {
			if create {
				var exists bool
				if err := tx.QueryRow(ctx, `
					SELECT EXISTS (SELECT 1 FROM tenant_session_policy WHERE tenant_id = $1)`,
					tenantID).Scan(&exists); err != nil {
					return err
				}
				if exists {
					return ErrVersionMismatch
				}
				return nil
			}
			since, ok := parseVersion(expectedVersion)
			if !ok {
				return ErrVersionMismatch
			}
			tag, err := tx.Exec(ctx, `
				DELETE FROM tenant_session_policy WHERE tenant_id = $1 AND updated_at = $2`,
				tenantID, since)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				return ErrVersionMismatch
			}
			return nil
		}

		var row pgx.Row
		if create {
			row = tx.QueryRow(ctx, `
				INSERT INTO tenant_session_policy
					(tenant_id, clipboard, file_transfer, file_max_bytes, updated_at)
				VALUES ($1, $2, $3, $4, clock_timestamp())
				ON CONFLICT (tenant_id) DO NOTHING
				RETURNING updated_at`,
				tenantID, w.Clipboard, w.FileTransfer, w.FileMaxBytes)
		} else {
			since, ok := parseVersion(expectedVersion)
			if !ok {
				return ErrVersionMismatch
			}
			row = tx.QueryRow(ctx, `
				UPDATE tenant_session_policy SET
					clipboard      = $2,
					file_transfer  = $3,
					file_max_bytes = $4,
					updated_at     = clock_timestamp()
				WHERE tenant_id = $1 AND updated_at = $5
				RETURNING updated_at`,
				tenantID, w.Clipboard, w.FileTransfer, w.FileMaxBytes, since)
		}
		var updatedAt time.Time
		if err := row.Scan(&updatedAt); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrVersionMismatch
			}
			return err
		}
		version = FormatVersion(updatedAt)
		return nil
	})
	if err != nil {
		return "", err
	}
	return version, nil
}
