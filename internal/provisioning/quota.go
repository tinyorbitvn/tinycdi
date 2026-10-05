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

// minus returns v - o per dimension, clamped at zero (o is a bound on what
// will be freed, not a precise future usage).
func (v ResourceVector) minus(o ResourceVector) ResourceVector {
	sub := func(a, b int64) int64 {
		if a-b < 0 {
			return 0
		}
		return a - b
	}
	return ResourceVector{
		RunningSlots: sub(v.RunningSlots, o.RunningSlots),
		CPUMillis:    sub(v.CPUMillis, o.CPUMillis),
		MemoryBytes:  sub(v.MemoryBytes, o.MemoryBytes),
		DiskBytes:    sub(v.DiskBytes, o.DiskBytes),
	}
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
	// ReleasePending is set when the refusal would disappear once the
	// reservations held by deleted or stopped workspaces are settled —
	// their release is already pending with Recovery, so the caller may
	// retry shortly instead of treating the quota as hard-exhausted.
	ReleasePending bool
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

// IsReleasePending reports whether err is a quota refusal that resolves
// itself: the shortfall is covered by reservations whose release is already
// pending with Recovery, so retrying shortly may succeed.
func IsReleasePending(err error) bool {
	var q *QuotaExceededError
	return errors.As(err, &q) && q.ReleasePending
}

// UserLimitError is returned when a reservation would push the workspace's
// owner over their effective per-principal running-workspace limit (the
// owner override when one is stored, else the tenant default). The API maps
// it to 409 QUOTA_EXHAUSTED with details.reason UserLimitReached.
type UserLimitError struct {
	TenantID  string
	Owner     string // issuer|sub, as workspaces.owner_subject stores it
	Limit     int64
	Current   int64
	Requested int64
	// ReleasePending is set when the refusal would disappear once the
	// owner's own teardown-pending reservations are settled — see
	// QuotaExceededError.ReleasePending.
	ReleasePending bool
}

func (e *UserLimitError) Error() string {
	return fmt.Sprintf("per-user limit reached for %q in tenant %q: running %d + requested %d > limit %d",
		e.Owner, e.TenantID, e.Current, e.Requested, e.Limit)
}

// IsUserLimit reports whether err is a per-principal limit rejection.
func IsUserLimit(err error) bool {
	var u *UserLimitError
	return errors.As(err, &u)
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
	// ProofQuotaSettled marks a reservation released because every held
	// dimension reached zero — recorded by the retained-disk quota paths
	// and the recovery sweep as the row's terminal state. It is never
	// accepted as input to Release: it carries no absence proof.
	ProofQuotaSettled AbsenceProof = "quota_settled"
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
		e := newQuotaExceeded(tenantID, dim, v, used, limit)
		// A refusal caused only by reservations whose release is pending —
		// held compute (or, for Ephemeral, the whole vector) on deleted or
		// stopped workspaces awaiting the runtime-absence proof — is
		// transient: flag it so the API can signal a retry instead of a
		// hard rejection.
		pending, perr := pendingReleaseVector(ctx, tx, tenantID)
		if perr != nil {
			return fmt.Errorf("reserve: pending release %w", perr)
		}
		if v.Exceeds(used.minus(pending), limit) == "" {
			e.ReleasePending = true
		}
		return e
	}

	if err := checkUserLimit(ctx, tx, tenantID, workspaceID, v.RunningSlots); err != nil {
		return err
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

// newQuotaExceeded describes the dimension a request would overrun.
func newQuotaExceeded(tenantID, dim string, v, used, limit ResourceVector) *QuotaExceededError {
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

// convertToDiskOnly turns a stopped Retain workspace's held reservation into a
// disk-only hold once the pod is proven gone: the compute vector moves to
// restart_* (so a start re-acquires exactly it) and the compute columns are
// zeroed; disk_bytes stays. It runs under the workspace row lock and only
// while the workspace is active and Stopped, so a start that lands between the
// absence proof and this write keeps its compute. Idempotent: a row already
// converted (or released) is untouched. It reports whether it converted.
func convertToDiskOnly(ctx context.Context, tx store.Tx, tenantID, workspaceID string) (bool, error) {
	var desired, state string
	err := tx.QueryRow(ctx, `
		SELECT desired_state, state FROM workspaces
		WHERE id = $1 AND tenant_id = $2 FOR UPDATE`, workspaceID, tenantID).Scan(&desired, &state)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("convert: lock workspace %w", err)
	}
	if state != "active" || desired != "Stopped" {
		return false, nil
	}
	tag, err := tx.Exec(ctx, `
		UPDATE quota_reservation
		SET restart_slots = running_slots, restart_cpu_millis = cpu_millis,
		    restart_memory_bytes = memory_bytes,
		    running_slots = 0, cpu_millis = 0, memory_bytes = 0
		WHERE workspace_id = $1 AND tenant_id = $2 AND state = 'held'
		  AND (running_slots > 0 OR cpu_millis > 0 OR memory_bytes > 0)`,
		workspaceID, tenantID)
	if err != nil {
		return false, fmt.Errorf("convert: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// reacquireCompute adds a converted workspace's stored compute vector back to
// its held reservation (the disk is already counted) and clears the stored
// vector. It serializes with other reservations on the tenant_quota row and
// checks only the compute dimensions.
func reacquireCompute(ctx context.Context, tx store.Tx, tenantID, workspaceID string, v ResourceVector) error {
	var limit ResourceVector
	err := tx.QueryRow(ctx, `
		SELECT max_running_slots, max_cpu_millis, max_memory_bytes, max_disk_bytes
		FROM tenant_quota WHERE tenant_id = $1 FOR UPDATE`, tenantID).
		Scan(&limit.RunningSlots, &limit.CPUMillis, &limit.MemoryBytes, &limit.DiskBytes)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNoQuota
	}
	if err != nil {
		return fmt.Errorf("reacquire: lock quota %w", err)
	}
	used, err := HeldUsage(ctx, tx, tenantID)
	if err != nil {
		return fmt.Errorf("reacquire: usage %w", err)
	}
	limit.DiskBytes = used.DiskBytes // disk is already held; only compute can overrun
	if dim := v.Exceeds(used, limit); dim != "" {
		e := newQuotaExceeded(tenantID, dim, v, used, limit)
		pending, perr := pendingReleaseVector(ctx, tx, tenantID)
		if perr != nil {
			return fmt.Errorf("reacquire: pending release %w", perr)
		}
		if v.Exceeds(used.minus(pending), limit) == "" {
			e.ReleasePending = true
		}
		return e
	}
	if err := checkUserLimit(ctx, tx, tenantID, workspaceID, v.RunningSlots); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE quota_reservation
		SET running_slots = $3, cpu_millis = $4, memory_bytes = $5,
		    restart_slots = NULL, restart_cpu_millis = NULL, restart_memory_bytes = NULL
		WHERE workspace_id = $1 AND tenant_id = $2 AND state = 'held'`,
		workspaceID, tenantID, v.RunningSlots, v.CPUMillis, v.MemoryBytes); err != nil {
		return fmt.Errorf("reacquire: %w", err)
	}
	return nil
}

// Release frees a held reservation and drops any stored restart vector (a
// disk-only hold that is released belongs to a deleted workspace). proof must
// evidence that the runtime no longer consumes compute; ProofUnspecified is rejected so a failed
// request or HTTP timeout never silently frees quota.
func Release(ctx context.Context, tx store.Tx, tenantID, workspaceID string, proof AbsenceProof) error {
	if !proof.Valid() {
		return ErrProofRequired
	}
	tag, err := tx.Exec(ctx, `
		UPDATE quota_reservation
		SET state = 'released', release_proof = $3, released_at = now(),
		    restart_slots = NULL, restart_cpu_millis = NULL, restart_memory_bytes = NULL
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

// pendingReleaseVector sums the quota that settle candidates (held
// reservations on deleted or stopped workspaces — the same predicate
// Recovery uses, see settleCandidateSQL) will free once their runtime
// absence is proven. Compute always frees; disk frees only for Ephemeral
// workspaces — a deleted Retain workspace keeps counting its retained
// datasets' bytes, so its disk is not pending.
func pendingReleaseVector(ctx context.Context, tx store.Tx, tenantID string) (ResourceVector, error) {
	var p ResourceVector
	err := tx.QueryRow(ctx, `
		SELECT COALESCE(SUM(qr.running_slots), 0), COALESCE(SUM(qr.cpu_millis), 0),
		       COALESCE(SUM(qr.memory_bytes), 0),
		       COALESCE(SUM(qr.disk_bytes) FILTER (WHERE w.data_policy = 'Ephemeral'), 0)
		FROM quota_reservation qr
		JOIN workspaces w ON w.id = qr.workspace_id
		WHERE qr.tenant_id = $1 AND (`+settleCandidateSQL+`)`, tenantID).
		Scan(&p.RunningSlots, &p.CPUMillis, &p.MemoryBytes, &p.DiskBytes)
	return p, err
}

// checkUserLimit refuses a grant of `slots` running slots when the
// workspace's owner would exceed their effective per-principal limit: the
// override stored in user_session_limit when one exists, else the tenant
// default in tenant_user_limit_default, else unlimited (no rows — the
// upgrade default). It runs under the caller's tenant_quota row lock, so
// the owner's count cannot change between the read here and the
// reservation write that follows — the same serialization the tenant-wide
// check relies on.
//
// The count is derived live from quota_reservation: 'held' rows with
// running slots still held, exactly the workspaces that occupy a running
// slot under the tenant quota. A stopped Retain workspace's disk-only
// hold (running_slots = 0), retained data disks and released rows never
// count, so the existing settle/release paths decrement the count with no
// extra bookkeeping. Requests that hold no running slot (pure disk)
// always pass.
func checkUserLimit(ctx context.Context, tx store.Tx, tenantID, workspaceID string, slots int64) error {
	if slots <= 0 {
		return nil
	}
	// The workspaces row is written before its reservation on every path
	// (create, attach, restart), so its owner is visible in this
	// transaction. A missing row breaks that invariant — fail closed:
	// skipping the check here would let a slot in over the principal's
	// limit.
	var owner string
	err := tx.QueryRow(ctx, `
		SELECT owner_subject FROM workspaces WHERE id = $1`, workspaceID).Scan(&owner)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("user limit: workspace %s: %w", workspaceID, ErrWorkspaceNotFound)
	}
	if err != nil {
		return fmt.Errorf("user limit: read owner %w", err)
	}
	var limit *int64
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(
			(SELECT max_running FROM user_session_limit
			 WHERE tenant_id = $1 AND owner_subject = $2),
			(SELECT max_running FROM tenant_user_limit_default
			 WHERE tenant_id = $1))`,
		tenantID, owner).Scan(&limit); err != nil {
		return fmt.Errorf("user limit: read limit %w", err)
	}
	if limit == nil {
		return nil
	}
	var used int64
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(SUM(qr.running_slots), 0)
		FROM quota_reservation qr
		JOIN workspaces w ON w.id = qr.workspace_id
		WHERE qr.tenant_id = $1 AND w.owner_subject = $2
		  AND qr.state = 'held' AND qr.running_slots > 0`,
		tenantID, owner).Scan(&used); err != nil {
		return fmt.Errorf("user limit: usage %w", err)
	}
	if used+slots <= *limit {
		return nil
	}
	e := &UserLimitError{
		TenantID: tenantID, Owner: owner,
		Limit: *limit, Current: used, Requested: slots,
	}
	// Transient when the owner's own teardown-pending holds cover the
	// shortfall — the same release-pending signal as the tenant check, so
	// the API can ask the client to retry instead of failing hard.
	pending, perr := userPendingReleaseSlots(ctx, tx, tenantID, owner)
	if perr != nil {
		return fmt.Errorf("user limit: pending release %w", perr)
	}
	if used-pending+slots <= *limit {
		e.ReleasePending = true
	}
	return e
}

// userPendingReleaseSlots sums the running slots the owner's settle
// candidates (settleCandidateSQL — held compute on deleted or stopped
// workspaces) will free once their runtime absence is proven. It is the
// per-principal analogue of pendingReleaseVector.
func userPendingReleaseSlots(ctx context.Context, tx store.Tx, tenantID, owner string) (int64, error) {
	var n int64
	err := tx.QueryRow(ctx, `
		SELECT COALESCE(SUM(qr.running_slots), 0)
		FROM quota_reservation qr
		JOIN workspaces w ON w.id = qr.workspace_id
		WHERE qr.tenant_id = $1 AND w.owner_subject = $2 AND (`+settleCandidateSQL+`)`,
		tenantID, owner).Scan(&n)
	return n, err
}
