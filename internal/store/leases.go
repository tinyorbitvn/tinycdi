// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// LiveLease is one active, unexpired connection lease — a live desktop
// session — as the admin sessions table shows it.
type LiveLease struct {
	ID            string
	WorkspaceID   string
	WorkspaceName string
	OwnerRef      string
	CreatedAt     time.Time
	LastRenewedAt *time.Time
	ExpiresAt     time.Time
}

// LeaseCursor is the keyset position (created_at, workspace id) of the last
// row of the previous page. ID is the WORKSPACE id: only one lease per
// workspace is active (connection_lease_one_active), so the pair is unique
// among live leases and the lease id itself never leaves the store.
type LeaseCursor struct {
	CreatedAt time.Time
	ID        string
}

// LeaseFilter narrows ListLiveLeases.
type LeaseFilter struct {
	OwnerRef string
	Limit    int
	After    *LeaseCursor
}

// liveLeasePredicate is the single definition of "live": state active and
// not past expires_at. HasLiveLease and ListLiveLeases share it.
const liveLeasePredicate = `l.state = 'active' AND l.expires_at > $2`

// ListLiveLeases returns up to f.Limit+1 live leases of tenantID, newest first.
func (d *DB) ListLiveLeases(ctx context.Context, tenantID string, now time.Time, f LeaseFilter) ([]LiveLease, error) {
	args := []any{tenantID, now}
	where := `l.tenant_id = $1 AND ` + liveLeasePredicate
	if f.OwnerRef != "" {
		args = append(args, f.OwnerRef)
		where += fmt.Sprintf(" AND l.principal_subject = $%d", len(args))
	}
	if f.After != nil {
		args = append(args, f.After.CreatedAt, f.After.ID)
		where += fmt.Sprintf(" AND (l.created_at, l.workspace_id) < ($%d, $%d)", len(args)-1, len(args))
	}
	args = append(args, f.Limit+1)
	rows, err := d.pool.Query(ctx, `
		SELECT l.id, l.workspace_id, w.name, l.principal_subject, l.created_at, l.last_renewed_at, l.expires_at
		FROM connection_lease l JOIN workspaces w ON w.id = l.workspace_id AND w.tenant_id = l.tenant_id
		WHERE `+where+fmt.Sprintf(` ORDER BY l.created_at DESC, l.workspace_id DESC LIMIT $%d`, len(args)), args...)
	if err != nil {
		return nil, fmt.Errorf("store: list live leases %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (LiveLease, error) {
		var l LiveLease
		err := row.Scan(&l.ID, &l.WorkspaceID, &l.WorkspaceName, &l.OwnerRef, &l.CreatedAt, &l.LastRenewedAt, &l.ExpiresAt)
		return l, err
	})
}

// HasLiveLease reports whether workspaceID has a live lease at now.
func (d *DB) HasLiveLease(ctx context.Context, workspaceID string, now time.Time) (bool, error) {
	var live bool
	if err := d.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM connection_lease l WHERE l.workspace_id = $1 AND `+liveLeasePredicate+`)`,
		workspaceID, now).Scan(&live); err != nil {
		return false, fmt.Errorf("store: has live lease %w", err)
	}
	return live, nil
}
