// Package provisioning implements the control-plane write path that does
// not depend on Kubernetes: quota reservations, the transactional outbox
// with per-workspace intent ordering, and idempotent request results.
//
// The Kubernetes side is hidden behind WorkspaceApplier; wiring to real
// Workspace CRs happens in the operator-facing layer.
package provisioning

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/tinyorbitvn/tinycdi/internal/store"
)

// ResourceVector is a multi-dimensional quota amount. Reservations hold
// one vector per workspace; tenant_quota holds the configured limits.
type ResourceVector struct {
	RunningSlots int64 `json:"runningSlots"`
	CPUMillis    int64 `json:"cpuMillis"`
	MemoryBytes  int64 `json:"memoryBytes"`
	DiskBytes    int64 `json:"diskBytes"`
}

// Exceeds reports the first dimension where used+req would pass limit,
// or "" when the request fits. A limit of 0 means "none allowed".
func (v ResourceVector) Exceeds(used, limit ResourceVector) string {
	switch {
	case used.RunningSlots+v.RunningSlots > limit.RunningSlots:
		return "runningSlots"
	case used.CPUMillis+v.CPUMillis > limit.CPUMillis:
		return "cpuMillis"
	case used.MemoryBytes+v.MemoryBytes > limit.MemoryBytes:
		return "memoryBytes"
	case used.DiskBytes+v.DiskBytes > limit.DiskBytes:
		return "diskBytes"
	}
	return ""
}

// QuotaExceededError is returned by Reserve when the request would push a
// tenant over a configured limit.
type QuotaExceededError struct {
	TenantID  string
	Dimension string
	Limit     int64
	Used      int64
	Requested int64
}

func (e *QuotaExceededError) Error() string {
	return fmt.Sprintf("quota exceeded for tenant %q on %s: used %d + requested %d > limit %d",
		e.TenantID, e.Dimension, e.Used, e.Requested, e.Limit)
}

// IsQuotaExceeded reports whether err is a quota rejection.
func IsQuotaExceeded(err error) bool {
	var q *QuotaExceededError
	return errors.As(err, &q)
}

var (
	// ErrNoQuota is returned when a tenant has no configured quota row;
	// the control plane fails closed instead of granting unlimited.
	ErrNoQuota = errors.New("no quota configured for tenant")
	// ErrReservationConflict is returned when a workspace already holds a
	// reservation with a different resource vector.
	ErrReservationConflict = errors.New("workspace already holds a different reservation")
	// ErrProofRequired is returned by Release when the caller cannot prove
	// the runtime is absent.
	ErrProofRequired = errors.New("runtime absence proof required")
)

// AbsenceProof states how the caller proved that a workspace's runtime no
// longer consumes compute. Release refuses ProofUnspecified: a failed or
// timed-out request alone never frees quota.
type AbsenceProof string

const (
	// ProofUnspecified is invalid for Release.
	ProofUnspecified AbsenceProof = ""
	// ProofNeverCreated means the create intent never reached the runtime
	// layer (still pending in the outbox or never appended), so no runtime
	// ever existed.
	ProofNeverCreated AbsenceProof = "never_created"
	// ProofRuntimeAbsent means the runtime layer was observed and reported
	// the workload gone (operator inventory / delete completed).
	ProofRuntimeAbsent AbsenceProof = "runtime_absent_observed"
)

// Valid reports whether p may be passed to Release.
func (p AbsenceProof) Valid() bool {
	return p == ProofNeverCreated || p == ProofRuntimeAbsent
}

// SetQuota upserts a tenant's limits. Call inside the tenant-provisioning
// transaction.
func SetQuota(ctx context.Context, tx store.Tx, tenantID string, limit ResourceVector) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO tenant_quota (tenant_id, max_running_slots, max_cpu_millis, max_memory_bytes, max_disk_bytes)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (tenant_id) DO UPDATE SET
			max_running_slots = EXCLUDED.max_running_slots,
			max_cpu_millis    = EXCLUDED.max_cpu_millis,
			max_memory_bytes  = EXCLUDED.max_memory_bytes,
			max_disk_bytes    = EXCLUDED.max_disk_bytes,
			updated_at        = now()`,
		tenantID, limit.RunningSlots, limit.CPUMillis, limit.MemoryBytes, limit.DiskBytes)
	return err
}

// Reserve holds v for workspaceID against tenantID's configured limits.
// It must run inside the caller's transaction together with the outbox
// intent append so a crash can never produce an intent without its
// reservation. Concurrent reserves serialize on the tenant_quota row
// lock, so at most `limit` reservations succeed.
//
// Retry-safe: a held reservation for the same workspace and vector
// returns nil; the same workspace with a different vector is a conflict.
func Reserve(ctx context.Context, tx store.Tx, tenantID, workspaceID string, v ResourceVector) error {
	var limit ResourceVector
	err := tx.QueryRow(ctx, `
		SELECT max_running_slots, max_cpu_millis, max_memory_bytes, max_disk_bytes
		FROM tenant_quota WHERE tenant_id = $1 FOR UPDATE`, tenantID).
		Scan(&limit.RunningSlots, &limit.CPUMillis, &limit.MemoryBytes, &limit.DiskBytes)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNoQuota
	}
	if err != nil {
		return fmt.Errorf("reserve: lock quota %w", err)
	}

	var held ResourceVector
	var state string
	err = tx.QueryRow(ctx, `
		SELECT running_slots, cpu_millis, memory_bytes, disk_bytes, state
		FROM quota_reservation WHERE workspace_id = $1`, workspaceID).
		Scan(&held.RunningSlots, &held.CPUMillis, &held.MemoryBytes, &held.DiskBytes, &state)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// fresh reservation below
	case err != nil:
		return fmt.Errorf("reserve: read reservation %w", err)
	case state == "held" && held == v:
		return nil // idempotent retry
	case state == "held":
		return ErrReservationConflict
	}

	var used ResourceVector
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(SUM(running_slots), 0), COALESCE(SUM(cpu_millis), 0),
		       COALESCE(SUM(memory_bytes), 0), COALESCE(SUM(disk_bytes), 0)
		FROM quota_reservation WHERE tenant_id = $1 AND state = 'held'`, tenantID).
		Scan(&used.RunningSlots, &used.CPUMillis, &used.MemoryBytes, &used.DiskBytes); err != nil {
		return fmt.Errorf("reserve: usage %w", err)
	}

	if dim := v.Exceeds(used, limit); dim != "" {
		e := &QuotaExceededError{TenantID: tenantID, Dimension: dim, Requested: v.RunningSlots}
		e.Used, e.Limit = used.RunningSlots, limit.RunningSlots
		switch dim {
		case "cpuMillis":
			e.Used, e.Limit, e.Requested = used.CPUMillis, limit.CPUMillis, v.CPUMillis
		case "memoryBytes":
			e.Used, e.Limit, e.Requested = used.MemoryBytes, limit.MemoryBytes, v.MemoryBytes
		case "diskBytes":
			e.Used, e.Limit, e.Requested = used.DiskBytes, limit.DiskBytes, v.DiskBytes
		}
		return e
	}

	if state == "released" {
		// Restart path: a previously released reservation for the same
		// workspace is re-held with the new vector.
		_, err = tx.Exec(ctx, `
			UPDATE quota_reservation SET
				state = 'held', running_slots = $3, cpu_millis = $4,
				memory_bytes = $5, disk_bytes = $6,
				release_proof = NULL, released_at = NULL
			WHERE workspace_id = $1 AND tenant_id = $2`,
			workspaceID, tenantID, v.RunningSlots, v.CPUMillis, v.MemoryBytes, v.DiskBytes)
	} else {
		_, err = tx.Exec(ctx, `
			INSERT INTO quota_reservation
				(workspace_id, tenant_id, running_slots, cpu_millis, memory_bytes, disk_bytes)
			VALUES ($1, $2, $3, $4, $5, $6)`,
			workspaceID, tenantID, v.RunningSlots, v.CPUMillis, v.MemoryBytes, v.DiskBytes)
	}
	if err != nil {
		return fmt.Errorf("reserve: insert %w", err)
	}
	return nil
}

// Release frees a held reservation. proof must evidence that the runtime
// no longer consumes compute; ProofUnspecified is rejected so a failed
// request or HTTP timeout never silently frees quota.
func Release(ctx context.Context, tx store.Tx, tenantID, workspaceID string, proof AbsenceProof) error {
	if !proof.Valid() {
		return ErrProofRequired
	}
	tag, err := tx.Exec(ctx, `
		UPDATE quota_reservation
		SET state = 'released', release_proof = $3, released_at = now()
		WHERE workspace_id = $1 AND tenant_id = $2 AND state = 'held'`,
		workspaceID, tenantID, string(proof))
	if err != nil {
		return fmt.Errorf("release: %w", err)
	}
	_ = tag // already-released rows are an idempotent no-op
	return nil
}

// HeldUsage returns the sum of held reservations for a tenant.
func HeldUsage(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, tenantID string) (ResourceVector, error) {
	var used ResourceVector
	err := q.QueryRow(ctx, `
		SELECT COALESCE(SUM(running_slots), 0), COALESCE(SUM(cpu_millis), 0),
		       COALESCE(SUM(memory_bytes), 0), COALESCE(SUM(disk_bytes), 0)
		FROM quota_reservation WHERE tenant_id = $1 AND state = 'held'`, tenantID).
		Scan(&used.RunningSlots, &used.CPUMillis, &used.MemoryBytes, &used.DiskBytes)
	return used, err
}
