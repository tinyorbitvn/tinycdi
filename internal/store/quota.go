// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"errors"
	"fmt"

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
	Usage     QuotaAmounts
	Owners    []OwnerUsage // ordered by owner reference
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
	err := q.db.Pool().QueryRow(ctx, `
		SELECT max_running_slots, max_cpu_millis, max_memory_bytes, max_disk_bytes
		FROM tenant_quota WHERE tenant_id = $1`, tenantID).
		Scan(&rep.Limits.RunningSlots, &rep.Limits.CPUMillis, &rep.Limits.MemoryBytes, &rep.Limits.DiskBytes)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return rep, fmt.Errorf("quota limits: %w", err)
	default:
		rep.HasLimits = true
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
	return rep, rows.Err()
}
