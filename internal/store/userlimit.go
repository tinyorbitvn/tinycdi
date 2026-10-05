// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package store

import (
	"context"
)

// UserLimitStore is the write side of per-principal running limits: the
// tenant default row and the per-owner overrides admission consults inside
// the reservation transaction. All writes are plain upserts/deletes —
// last write wins; each is audited by the admin API layer.
type UserLimitStore struct{ db *DB }

// NewUserLimitStore wraps db.
func NewUserLimitStore(db *DB) *UserLimitStore { return &UserLimitStore{db: db} }

// SetOverride upserts the per-principal running-workspace limit for owner
// ("issuer|sub") in tenantID.
func (s *UserLimitStore) SetOverride(ctx context.Context, tenantID, owner string, max int64) error {
	_, err := s.db.Pool().Exec(ctx, `
		INSERT INTO user_session_limit (tenant_id, owner_subject, max_running)
		VALUES ($1, $2, $3)
		ON CONFLICT (tenant_id, owner_subject) DO UPDATE SET
			max_running = EXCLUDED.max_running,
			updated_at  = clock_timestamp()`,
		tenantID, owner, max)
	return err
}

// ClearOverride deletes the owner's override; the tenant default (or
// unlimited) applies again. Deleting a missing row is a no-op.
func (s *UserLimitStore) ClearOverride(ctx context.Context, tenantID, owner string) error {
	_, err := s.db.Pool().Exec(ctx, `
		DELETE FROM user_session_limit WHERE tenant_id = $1 AND owner_subject = $2`,
		tenantID, owner)
	return err
}

// SetDefault upserts the tenant-wide fallback limit.
func (s *UserLimitStore) SetDefault(ctx context.Context, tenantID string, max int64) error {
	_, err := s.db.Pool().Exec(ctx, `
		INSERT INTO tenant_user_limit_default (tenant_id, max_running)
		VALUES ($1, $2)
		ON CONFLICT (tenant_id) DO UPDATE SET
			max_running = EXCLUDED.max_running,
			updated_at  = clock_timestamp()`,
		tenantID, max)
	return err
}

// ClearDefault deletes the tenant default; principals without an override
// become unlimited. Deleting a missing row is a no-op.
func (s *UserLimitStore) ClearDefault(ctx context.Context, tenantID string) error {
	_, err := s.db.Pool().Exec(ctx, `
		DELETE FROM tenant_user_limit_default WHERE tenant_id = $1`, tenantID)
	return err
}
