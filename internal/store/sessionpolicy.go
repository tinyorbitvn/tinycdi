// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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
	// Version is the row's change token — its updated_at, formatted
	// RFC3339Nano: it changes on every write and is empty when no row
	// exists. PUT echoes it in If-Match (optimistic concurrency).
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

// ErrSessionPolicyVersionMismatch is returned by
// SessionPolicyStore.SetCeiling when the If-Match version does not name
// the current row — the ceiling changed between the client's read and
// its write — or when the precondition names no row ("*") and one exists.
var ErrSessionPolicyVersionMismatch = errors.New("session policy version mismatch")

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
	c.Version = updatedAt.UTC().Format(time.RFC3339Nano)
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

// SetCeiling writes the ceiling under optimistic concurrency — the same
// If-Match discipline as the quota store (adminQuotaStore.SetLimits):
// ifMatch must name the row's current version (Ceiling.Version) or be
// IfMatchCreate ("*") when no row exists; a stale or wrong precondition
// answers ErrSessionPolicyVersionMismatch. The check and the write are a
// single statement, so they cannot be raced. An all-defaults write
// deletes the row — "no row" and "all-defaults row" are the same state,
// and the store keeps exactly one representation of it.
func (s *SessionPolicyStore) SetCeiling(ctx context.Context, tenantID string, w SessionPolicyWrite, ifMatch string) error {
	return s.db.WithTx(ctx, func(tx Tx) error {
		if w.Defaults() {
			// Conditional delete: remove whatever row exists and compare
			// its version to If-Match inside the same transaction — a
			// mismatch rolls the delete back, so the statement is
			// race-free by construction. Nothing deleted is a match only
			// for "*" (the caller believes there is no row).
			var deletedAt *time.Time
			err := tx.QueryRow(ctx, `
				DELETE FROM tenant_session_policy WHERE tenant_id = $1
				RETURNING updated_at`, tenantID).Scan(&deletedAt)
			switch {
			case errors.Is(err, pgx.ErrNoRows):
				if ifMatch != IfMatchCreate {
					return ErrSessionPolicyVersionMismatch
				}
				return nil
			case err != nil:
				return err
			}
			if !deletedAt.UTC().Equal(mustParseVersion(ifMatch)) {
				return ErrSessionPolicyVersionMismatch
			}
			return nil
		}

		var tag pgconn.CommandTag
		var err error
		if ifMatch == IfMatchCreate {
			tag, err = tx.Exec(ctx, `
				INSERT INTO tenant_session_policy
					(tenant_id, clipboard, file_transfer, file_max_bytes)
				VALUES ($1, $2, $3, $4)
				ON CONFLICT (tenant_id) DO NOTHING`,
				tenantID, w.Clipboard, w.FileTransfer, w.FileMaxBytes)
		} else {
			since, perr := parseVersion(ifMatch)
			if perr != nil {
				return ErrSessionPolicyVersionMismatch
			}
			tag, err = tx.Exec(ctx, `
				UPDATE tenant_session_policy SET
					clipboard      = $2,
					file_transfer  = $3,
					file_max_bytes = $4,
					updated_at     = clock_timestamp()
				WHERE tenant_id = $1 AND updated_at = $5`,
				tenantID, w.Clipboard, w.FileTransfer, w.FileMaxBytes, since)
		}
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrSessionPolicyVersionMismatch
		}
		return nil
	})
}

// IfMatchCreate is the If-Match value a client sends to create a row for
// a tenant that has none. It is the only accepted precondition on a
// missing row — an existing row requires its exact current version, so a
// bare "*" can never skip the concurrency check. Mirrors api.IfMatchCreate
// (the constant lives here so the store needs no api import).
const IfMatchCreate = "*"

// parseVersion reads the If-Match change token: the row's updated_at
// formatted RFC3339Nano. Anything else can never equal a row's
// updated_at — refuse it as a precondition failure rather than letting
// Postgres's cast error out.
func parseVersion(ifMatch string) (time.Time, error) {
	return time.Parse(time.RFC3339Nano, ifMatch)
}

// mustParseVersion parses like parseVersion; an unparseable token yields
// the zero time, which can never equal a row's updated_at — so the delete
// path treats it as a precondition failure, not a crash.
func mustParseVersion(ifMatch string) time.Time {
	since, err := parseVersion(ifMatch)
	if err != nil {
		return time.Time{}
	}
	return since
}
