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

// QuotaAmounts is one resource vector in storage units.
type QuotaAmounts struct {
	Workspaces   int64 // active (not deleted) workspaces
	RunningSlots int64
	CPUMillis    int64
	MemoryBytes  int64
	DiskBytes    int64
}

// OwnerUsage is one owner's share of a tenant's usage.
type OwnerUsage struct {
	OwnerRef string // issuer|sub, as workspaces.owner_subject stores it
	Usage    QuotaAmounts
}

// QuotaReport is a read-only snapshot of a tenant's quota: configured
// limits (absent when the tenant has no quota row — admission then fails
// closed) and current usage, tenant-wide and per owner. Usage counts the
// same rows admission counts: 'held' reservations, plus active workspaces.
type QuotaReport struct {
	HasLimits bool
	Limits    QuotaAmounts // Workspaces is always 0: there is no count limit
	// Version is the limits row's change token (FormatVersion of
	// updated_at): it changes on every update and is empty when no row
	// exists. The admin API derives the quota's ETag from it.
	Version string
	Usage   QuotaAmounts
	Owners  []OwnerUsage // ordered by owner reference
	// DefaultUserLimit is the tenant-wide fallback for a principal's
	// running-workspace limit; nil means no default (unlimited unless an
	// override applies).
	DefaultUserLimit *int64
	// DefaultUserLimitVersion is the default row's change token; empty
	// when no default row exists.
	DefaultUserLimitVersion string
	// UserLimits maps owner reference to the stored per-principal
	// running-workspace override; an owner's effective limit is
	// UserLimits[owner] when present, else DefaultUserLimit, else
	// unlimited.
	UserLimits map[string]int64
	// UserLimitVersions maps owner reference to its override row's change
	// token; it has exactly the keys of UserLimits.
	UserLimitVersions map[string]string
}

// QuotaReader reads quota reports (GET /v1/quota).
type QuotaReader struct{ db *DB }

// NewQuotaReader wraps db.
func NewQuotaReader(db *DB) *QuotaReader { return &QuotaReader{db: db} }

// Report returns tenantID's quota report. It reads only; admission still
// serializes on the tenant_quota row lock, so the snapshot may trail a
// concurrent create by one transaction.
func (q *QuotaReader) Report(ctx context.Context, tenantID string) (QuotaReport, error) {
	var rep QuotaReport
	var updatedAt time.Time
	err := q.db.Pool().QueryRow(ctx, `
		SELECT max_running_slots, max_cpu_millis, max_memory_bytes, max_disk_bytes,
		       updated_at
		FROM tenant_quota WHERE tenant_id = $1`, tenantID).
		Scan(&rep.Limits.RunningSlots, &rep.Limits.CPUMillis, &rep.Limits.MemoryBytes, &rep.Limits.DiskBytes,
			&updatedAt)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return rep, fmt.Errorf("quota limits: %w", err)
	default:
		rep.HasLimits = true
		rep.Version = FormatVersion(updatedAt)
	}

	// A deleted workspace whose reservation is still held (release awaits
	// proof of runtime absence) keeps counting, exactly as Reserve does.
	rows, err := q.db.Pool().Query(ctx, `
		SELECT w.owner_subject,
		       count(*) FILTER (WHERE w.state = 'active'),
		       COALESCE(sum(r.running_slots), 0)::bigint,
		       COALESCE(sum(r.cpu_millis), 0)::bigint,
		       COALESCE(sum(r.memory_bytes), 0)::bigint,
		       COALESCE(sum(r.disk_bytes), 0)::bigint
		FROM workspaces w
		LEFT JOIN quota_reservation r ON r.workspace_id = w.id AND r.state = 'held'
		WHERE w.tenant_id = $1 AND (w.state = 'active' OR r.workspace_id IS NOT NULL)
		GROUP BY w.owner_subject
		ORDER BY w.owner_subject`, tenantID)
	if err != nil {
		return rep, fmt.Errorf("quota usage: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var o OwnerUsage
		if err := rows.Scan(&o.OwnerRef, &o.Usage.Workspaces, &o.Usage.RunningSlots,
			&o.Usage.CPUMillis, &o.Usage.MemoryBytes, &o.Usage.DiskBytes); err != nil {
			return rep, fmt.Errorf("quota usage: %w", err)
		}
		rep.Owners = append(rep.Owners, o)
		rep.Usage.Workspaces += o.Usage.Workspaces
		rep.Usage.RunningSlots += o.Usage.RunningSlots
		rep.Usage.CPUMillis += o.Usage.CPUMillis
		rep.Usage.MemoryBytes += o.Usage.MemoryBytes
		rep.Usage.DiskBytes += o.Usage.DiskBytes
	}
	if err := rows.Err(); err != nil {
		return rep, err
	}

	// Per-principal running limits live outside the limits row (the
	// tenant stays tenant-scoped): the default row plus every override.
	var def int64
	var defAt time.Time
	switch err := q.db.Pool().QueryRow(ctx, `
		SELECT max_running, updated_at FROM tenant_user_limit_default WHERE tenant_id = $1`,
		tenantID).Scan(&def, &defAt); {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return rep, fmt.Errorf("user limit default: %w", err)
	default:
		rep.DefaultUserLimit = &def
		rep.DefaultUserLimitVersion = FormatVersion(defAt)
	}
	lrows, err := q.db.Pool().Query(ctx, `
		SELECT owner_subject, max_running, updated_at FROM user_session_limit
		WHERE tenant_id = $1 ORDER BY owner_subject`, tenantID)
	if err != nil {
		return rep, fmt.Errorf("user limits: %w", err)
	}
	defer lrows.Close()
	rep.UserLimits = map[string]int64{}
	rep.UserLimitVersions = map[string]string{}
	for lrows.Next() {
		var owner string
		var max int64
		var at time.Time
		if err := lrows.Scan(&owner, &max, &at); err != nil {
			return rep, fmt.Errorf("user limits: %w", err)
		}
		rep.UserLimits[owner] = max
		rep.UserLimitVersions[owner] = FormatVersion(at)
	}
	return rep, lrows.Err()
}
