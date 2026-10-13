package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// TenantOverview is the Postgres half of the admin overview (F7); the
// Kubernetes half (phases, templates, finalizers) comes from the informer.
type TenantOverview struct {
	LiveSessions           int64
	ProvisioningP95Seconds *float64 // nil when no incarnation became Ready in 24 h
	ProvisioningSamples    int64
}

// TenantOverview runs the overview's single aggregate query. "Live" is the
// shared liveLeasePredicate (leases.go), so the count matches the sessions
// list; the clamp absorbs clock skew between the database (requested_at) and
// the Kubernetes status.startedAt (ready_at).
func (d *DB) TenantOverview(ctx context.Context, tenantID string, now time.Time) (TenantOverview, error) {
	var o TenantOverview
	err := d.pool.QueryRow(ctx, `
		SELECT
			(SELECT count(*) FROM connection_lease l WHERE l.tenant_id = $1 AND `+liveLeasePredicate+`),
			b.p95, b.samples
		FROM (
			SELECT percentile_cont(0.95) WITHIN GROUP (
			           ORDER BY GREATEST(extract(epoch FROM (ready_at - requested_at)), 0)::float8) AS p95,
			       count(*) AS samples
			FROM workspace_boot
			WHERE tenant_id = $1 AND ready_at IS NOT NULL AND ready_at >= $2::timestamptz - interval '24 hours'
		) b`, tenantID, now).Scan(&o.LiveSessions, &o.ProvisioningP95Seconds, &o.ProvisioningSamples)
	if err != nil {
		return TenantOverview{}, fmt.Errorf("store: tenant overview %w", err)
	}
	return o, nil
}

// MarkBootReady records that incarnation (workspaceID, gen) became Ready
// at readyAt and returns its boot latency. ok is false when the row is
// unknown or already marked — every backend replica sees the same informer
// event and exactly one wins.
func (d *DB) MarkBootReady(ctx context.Context, workspaceID string, gen int64, readyAt time.Time) (time.Duration, bool, error) {
	var secs float64
	err := d.pool.QueryRow(ctx, `
		UPDATE workspace_boot SET ready_at = $3
		WHERE workspace_id = $1 AND runtime_generation = $2 AND ready_at IS NULL
		RETURNING GREATEST(extract(epoch FROM ($3::timestamptz - requested_at)), 0)::float8`,
		workspaceID, gen, readyAt).Scan(&secs)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("store: mark boot ready %w", err)
	}
	return time.Duration(secs * float64(time.Second)), true, nil
}

// PruneWorkspaceBoot deletes boot rows requested before `before`.
func (d *DB) PruneWorkspaceBoot(ctx context.Context, before time.Time) (int64, error) {
	tag, err := d.pool.Exec(ctx, `DELETE FROM workspace_boot WHERE requested_at < $1`, before)
	if err != nil {
		return 0, fmt.Errorf("store: prune workspace_boot %w", err)
	}
	return tag.RowsAffected(), nil
}
