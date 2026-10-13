// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// UserLimitStore is the write side of per-principal running limits: the
// tenant default row and the per-owner overrides admission consults inside
// the reservation transaction. Every write is conditional on the row's
// version (QuotaReport.UserLimitVersions / DefaultUserLimitVersion): create
// expects no row, otherwise expectedVersion must name the current one, and
// a miss is ErrVersionMismatch. Each write is audited by the admin API.
type UserLimitStore struct{ db *DB }

// NewUserLimitStore wraps db.
func NewUserLimitStore(db *DB) *UserLimitStore { return &UserLimitStore{db: db} }

// SetOverride writes the per-principal running-workspace limit for owner
// ("issuer|sub") in tenantID and returns the row's new version.
func (s *UserLimitStore) SetOverride(ctx context.Context, tenantID, owner string, max int64, expectedVersion string, create bool) (string, error) {
	if create {
		return s.write(ctx, `
			INSERT INTO user_session_limit (tenant_id, owner_subject, max_running, updated_at)
			VALUES ($1, $2, $3, clock_timestamp())
			ON CONFLICT (tenant_id, owner_subject) DO NOTHING
			RETURNING updated_at`, tenantID, owner, max)
	}
	since, ok := parseVersion(expectedVersion)
	if !ok {
		return "", ErrVersionMismatch
	}
	return s.write(ctx, `
		UPDATE user_session_limit SET max_running = $3, updated_at = clock_timestamp()
		WHERE tenant_id = $1 AND owner_subject = $2 AND updated_at = $4
		RETURNING updated_at`, tenantID, owner, max, since)
}

// ClearOverride deletes the owner's override; the tenant default (or
// unlimited) applies again. Under create (the caller saw no row) it is a
// no-op that still refuses a row someone else created.
func (s *UserLimitStore) ClearOverride(ctx context.Context, tenantID, owner, expectedVersion string, create bool) (string, error) {
	if create {
		return "", s.expectAbsent(ctx, `
			SELECT EXISTS (SELECT 1 FROM user_session_limit WHERE tenant_id = $1 AND owner_subject = $2)`,
			tenantID, owner)
	}
	since, ok := parseVersion(expectedVersion)
	if !ok {
		return "", ErrVersionMismatch
	}
	return "", s.delete(ctx, `
		DELETE FROM user_session_limit
		WHERE tenant_id = $1 AND owner_subject = $2 AND updated_at = $3`,
		tenantID, owner, since)
}

// SetDefault writes the tenant-wide fallback limit and returns the row's
// new version.
func (s *UserLimitStore) SetDefault(ctx context.Context, tenantID string, max int64, expectedVersion string, create bool) (string, error) {
	if create {
		return s.write(ctx, `
			INSERT INTO tenant_user_limit_default (tenant_id, max_running, updated_at)
			VALUES ($1, $2, clock_timestamp())
			ON CONFLICT (tenant_id) DO NOTHING
			RETURNING updated_at`, tenantID, max)
	}
	since, ok := parseVersion(expectedVersion)
	if !ok {
		return "", ErrVersionMismatch
	}
	return s.write(ctx, `
		UPDATE tenant_user_limit_default SET max_running = $2, updated_at = clock_timestamp()
		WHERE tenant_id = $1 AND updated_at = $3
		RETURNING updated_at`, tenantID, max, since)
}

// ClearDefault deletes the tenant default; principals without an override
// become unlimited. Under create it is a no-op that refuses an existing
// row, like ClearOverride.
func (s *UserLimitStore) ClearDefault(ctx context.Context, tenantID, expectedVersion string, create bool) (string, error) {
	if create {
		return "", s.expectAbsent(ctx, `
			SELECT EXISTS (SELECT 1 FROM tenant_user_limit_default WHERE tenant_id = $1)`, tenantID)
	}
	since, ok := parseVersion(expectedVersion)
	if !ok {
		return "", ErrVersionMismatch
	}
	return "", s.delete(ctx, `
		DELETE FROM tenant_user_limit_default WHERE tenant_id = $1 AND updated_at = $2`,
		tenantID, since)
}

// write runs a conditional INSERT/UPDATE ... RETURNING updated_at; no row
// returned means the precondition failed.
func (s *UserLimitStore) write(ctx context.Context, sql string, args ...any) (string, error) {
	var at time.Time
	if err := s.db.Pool().QueryRow(ctx, sql, args...).Scan(&at); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrVersionMismatch
		}
		return "", err
	}
	return FormatVersion(at), nil
}

// delete runs a conditional DELETE; zero rows means the precondition
// failed.
func (s *UserLimitStore) delete(ctx context.Context, sql string, args ...any) error {
	tag, err := s.db.Pool().Exec(ctx, sql, args...)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrVersionMismatch
	}
	return nil
}

// expectAbsent answers ErrVersionMismatch when the EXISTS query finds a row.
func (s *UserLimitStore) expectAbsent(ctx context.Context, sql string, args ...any) error {
	var exists bool
	if err := s.db.Pool().QueryRow(ctx, sql, args...).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return ErrVersionMismatch
	}
	return nil
}
