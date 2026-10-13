// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// TenantUser is one principal that signed in to a tenant (principal_directory),
// with activity and ownership folded in. Display data only.
type TenantUser struct {
	OwnerRef    string
	Subject     string
	DisplayName string
	Email       string
	LastSeenAt  time.Time
	Workspaces  int64
}

// UserCursor is the keyset position of the previous page's last row.
type UserCursor struct {
	Key      string // the sort column's value (RFC3339Nano for lastSeen)
	OwnerRef string
}

// UserPage selects one page of ListTenantUsers.
type UserPage struct {
	Limit int
	Sort  string // ownerRef | displayName | lastSeen
	Desc  bool
	After *UserCursor
}

// tenantUsersCTE folds session activity (last_seen_at) and active-workspace
// counts into each directory row. The sessions lookup splits owner_ref at the
// first '|' (validOwnerRef guarantees the issuer holds none) so it rides the
// sessions_issuer_subject index. A principal without a display name shows
// (and sorts under) its subject, as the owner views do.
const tenantUsersCTE = `
WITH u AS (
	SELECT d.owner_ref, d.subject, COALESCE(NULLIF(d.display_name, ''), d.subject) AS display_name, d.email,
		GREATEST(d.last_login_at, COALESCE((
			SELECT max(s.last_seen_at) FROM sessions s
			WHERE s.tenant_id = d.tenant_id
			  AND s.issuer = split_part(d.owner_ref, '|', 1)
			  AND s.subject = substr(d.owner_ref, length(split_part(d.owner_ref, '|', 1)) + 2)), d.last_login_at)) AS last_seen,
		(SELECT count(*) FROM workspaces w
		 WHERE w.tenant_id = d.tenant_id AND w.owner_subject = d.owner_ref AND w.state = 'active') AS workspaces
	FROM principal_directory d WHERE d.tenant_id = $1)
SELECT owner_ref, subject, display_name, email, last_seen, workspaces FROM u`

var userSortColumns = map[string]string{"ownerRef": "owner_ref", "displayName": "display_name", "lastSeen": "last_seen"}

// ListTenantUsers pages the tenant's directory; it returns up to p.Limit+1 rows.
func (d *DB) ListTenantUsers(ctx context.Context, tenantID string, p UserPage) ([]TenantUser, error) {
	col, ok := userSortColumns[p.Sort]
	if !ok {
		return nil, fmt.Errorf("store: unknown user sort %q", p.Sort)
	}
	dir, cmp := "ASC", ">"
	if p.Desc {
		dir, cmp = "DESC", "<"
	}
	args := []any{tenantID}
	where := ""
	if p.After != nil {
		var key any = p.After.Key
		if p.Sort == "lastSeen" {
			ts, err := time.Parse(time.RFC3339Nano, p.After.Key)
			if err != nil {
				return nil, fmt.Errorf("store: user cursor %w", err)
			}
			key = ts
		}
		args = append(args, key, p.After.OwnerRef)
		where = fmt.Sprintf(" WHERE (%s, owner_ref) %s ($2, $3)", col, cmp)
	}
	args = append(args, p.Limit+1)
	rows, err := d.pool.Query(ctx, tenantUsersCTE+where+
		fmt.Sprintf(" ORDER BY %s %s, owner_ref %s LIMIT $%d", col, dir, dir, len(args)), args...)
	if err != nil {
		return nil, fmt.Errorf("store: list tenant users %w", err)
	}
	return pgx.CollectRows(rows, scanTenantUser)
}

// GetTenantUser returns one directory row; ok=false when the principal
// never signed in to tenantID.
func (d *DB) GetTenantUser(ctx context.Context, tenantID, ownerRef string) (TenantUser, bool, error) {
	rows, err := d.pool.Query(ctx, tenantUsersCTE+" WHERE owner_ref = $2", tenantID, ownerRef)
	if err != nil {
		return TenantUser{}, false, fmt.Errorf("store: get tenant user %w", err)
	}
	u, err := pgx.CollectExactlyOneRow(rows, scanTenantUser)
	if errors.Is(err, pgx.ErrNoRows) {
		return TenantUser{}, false, nil
	}
	if err != nil {
		return TenantUser{}, false, fmt.Errorf("store: get tenant user %w", err)
	}
	return u, true, nil
}

func scanTenantUser(row pgx.CollectableRow) (TenantUser, error) {
	var u TenantUser
	err := row.Scan(&u.OwnerRef, &u.Subject, &u.DisplayName, &u.Email, &u.LastSeenAt, &u.Workspaces)
	return u, err
}
