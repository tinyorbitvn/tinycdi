package broker

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tinyorbitvn/tinycdi/internal/store"
)

// Fence is the incarnation identity a gateway presents when renewing or
// resolving. It must match (WorkspaceUID, RuntimeGeneration, RuntimeUID) of
// the lease's bound incarnation plus the lease's current FencingVersion; a
// stale fence means the runtime was replaced and the lease is dead.
type Fence struct {
	WorkspaceUID      string `json:"workspaceUID"`
	RuntimeGeneration uint64 `json:"runtimeGeneration"`
	RuntimeUID        string `json:"runtimeUID"`
	FencingVersion    uint64 `json:"version"`
}

// Lease is a claimed connection grant: at most one active lease exists per
// workspace (unique index in migration 002). It expires unless renewed every
// ~10 s and must be treated as dead once revoked, superseded or past expiry.
type Lease struct {
	ID                string    `json:"leaseId"`
	WorkspaceUID      string    `json:"workspaceUID"`
	TenantID          string    `json:"tenantId"`
	PrincipalSubject  string    `json:"principalSubject"`
	RuntimeGeneration uint64    `json:"runtimeGeneration"`
	RuntimeUID        string    `json:"runtimeUID"`
	FencingVersion    uint64    `json:"fencingVersion"`
	GatewayID         string    `json:"gatewayId"`
	StreamEpoch       uint64    `json:"streamEpoch"`
	ExpiresAt         time.Time `json:"expiresAt"`
}

// loadLease fetches the lease row including its lifecycle state.
func (b *Broker) loadLease(ctx context.Context, leaseID string) (Lease, string, error) {
	var (
		l     Lease
		state string
	)
	err := b.db.Pool().QueryRow(ctx,
		`SELECT id, workspace_id, tenant_id, principal_subject,
			runtime_generation, runtime_uid, fencing_version, gateway_id,
			state, expires_at, stream_epoch
		 FROM connection_lease WHERE id = $1`, leaseID).
		Scan(&l.ID, &l.WorkspaceUID, &l.TenantID, &l.PrincipalSubject,
			&l.RuntimeGeneration, &l.RuntimeUID, &l.FencingVersion,
			&l.GatewayID, &state, &l.ExpiresAt, &l.StreamEpoch)
	if errors.Is(err, pgx.ErrNoRows) {
		return Lease{}, "", ErrLeaseInvalid
	}
	if err != nil {
		return Lease{}, "", fmt.Errorf("broker: lease lookup: %w", err)
	}
	return l, state, nil
}

// closeStreamsTx zeroes a runtime generation's open_streams and anchors
// disconnected_since, inside tx. Every transition that kills a lease runs
// it: the lease's streams can no longer report their close (every gateway
// path refuses a dead lease), so the kill owns the drain accounting and the
// §8 disconnect grace semantics survive a replica dying mid-stream.
func closeStreamsTx(ctx context.Context, tx store.Tx, wsUID string, gen uint64, now time.Time) error {
	_, err := tx.Exec(ctx, `
		UPDATE workspace_activity SET
			open_streams = 0,
			disconnected_since = COALESCE(disconnected_since, $3),
			updated_at = $3
		WHERE workspace_id = $1 AND runtime_generation = $2 AND open_streams > 0`,
		wsUID, int64(gen), now)
	return err
}

// liveLease validates a loaded lease: unknown -> ErrLeaseInvalid;
// revoked/superseded/time-expired -> ErrRevoked (the lease is dead);
// foreign gateway -> ErrDenied. Time-expired rows are lazily marked
// 'expired' so the partial unique index frees the workspace.
//
// The lazy expiry is authoritative for drain accounting, like RevokeLease:
// the lease's streams can no longer report their close (every gateway path
// refuses a dead lease), so the active->expired transition zeroes the bound
// generation's open_streams in the same transaction and anchors
// disconnected_since, keeping the disconnect grace semantics of §8. A
// concurrent transition (RowsAffected = 0) skips the accounting — whoever
// moved the row owned it.
func (b *Broker) liveLease(ctx context.Context, gw GatewayIdentity, leaseID string, now time.Time) (Lease, error) {
	l, state, err := b.loadLease(ctx, leaseID)
	if err != nil {
		return Lease{}, err
	}
	if state != "active" {
		return Lease{}, ErrRevoked
	}
	if !l.ExpiresAt.After(now) {
		_ = b.db.WithTx(ctx, func(tx store.Tx) error {
			tag, err := tx.Exec(ctx,
				`UPDATE connection_lease SET state = 'expired', closed_at = $2
				 WHERE id = $1 AND state = 'active'`, l.ID, now)
			if err != nil || tag.RowsAffected() == 0 {
				return err
			}
			if err := closeStreamsTx(ctx, tx, l.WorkspaceUID, l.RuntimeGeneration, now); err != nil {
				return fmt.Errorf("broker: close expired streams: %w", err)
			}
			return nil
		})
		return Lease{}, ErrRevoked
	}
	if l.GatewayID != gw.ID {
		return Lease{}, ErrDenied
	}
	return l, nil
}

// boundCurrent verifies the lease's pinned incarnation is still the one the
// binding source currently reports for a Ready workspace.
func (b *Broker) boundCurrent(ctx context.Context, l Lease, now time.Time) (RuntimeBinding, error) {
	binding, err := b.currentBinding(ctx, PlatformID(l.WorkspaceUID), now)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return RuntimeBinding{}, ErrStaleBinding
		}
		return RuntimeBinding{}, err
	}
	if binding.Phase != PhaseReady ||
		binding.RuntimeGeneration != l.RuntimeGeneration ||
		binding.RuntimeUID != l.RuntimeUID {
		return RuntimeBinding{}, ErrStaleBinding
	}
	return binding, nil
}

// RenewLease slides the lease expiry forward (~LeaseTTL) when the fence
// matches the current binding. It fails with ErrDenied for a different
// gateway, ErrStaleBinding when the fence no longer matches the reported
// incarnation, ErrRevoked for a revoked/superseded lease and ErrFreshness
// when the observed state is too stale to extend access (fail closed).
func (b *Broker) RenewLease(ctx context.Context, gw GatewayIdentity, leaseID string, fence Fence) (Lease, error) {
	now := b.now()
	l, err := b.liveLease(ctx, gw, leaseID, now)
	if err != nil {
		return Lease{}, err
	}
	if fence.WorkspaceUID != l.WorkspaceUID ||
		fence.RuntimeGeneration != l.RuntimeGeneration ||
		fence.RuntimeUID != l.RuntimeUID ||
		fence.FencingVersion != l.FencingVersion {
		return Lease{}, ErrStaleBinding
	}
	if _, err := b.boundCurrent(ctx, l, now); err != nil {
		return Lease{}, err
	}
	l.ExpiresAt = now.Add(b.leaseTTL)
	if _, err := b.db.Pool().Exec(ctx,
		`UPDATE connection_lease SET expires_at = $2, last_renewed_at = $3
		 WHERE id = $1 AND state = 'active'`, l.ID, l.ExpiresAt, now); err != nil {
		return Lease{}, fmt.Errorf("broker: renew lease: %w", err)
	}
	return l, nil
}

// RevokeLease revokes the lease: every open stream bound to it must close
// (design budget ≤30 s) and renewals fail permanently. Revoking an
// already-dead lease is a no-op; it never touches another live lease.
//
// Like RevokeWorkspaceLeases, revocation is authoritative for drain
// accounting: the lease's streams can no longer report their close,
// so when the lease actually transitions active->revoked its bound
// generation's open_streams are zeroed in the same transaction. A revoke
// of an already-dead lease skips the accounting — a live successor lease
// may own those streams.
func (b *Broker) RevokeLease(ctx context.Context, leaseID string) error {
	now := b.now()
	return b.db.WithTx(ctx, func(tx store.Tx) error {
		var (
			wsUID string
			gen   int64
		)
		err := tx.QueryRow(ctx, `
			UPDATE connection_lease SET state = 'revoked', closed_at = $2
			WHERE id = $1 AND state = 'active'
			RETURNING workspace_id, runtime_generation`, leaseID, now).
			Scan(&wsUID, &gen)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return nil // already dead — nothing this lease owned remains
		case err != nil:
			return fmt.Errorf("broker: revoke lease: %w", err)
		}
		// The revoked lease was the workspace's only active lease (partial
		// unique index), so every stream counted on its generation is now
		// closing but cannot report it — close the accounting atomically.
		if err := closeStreamsTx(ctx, tx, wsUID, uint64(gen), now); err != nil {
			return fmt.Errorf("broker: close revoked streams: %w", err)
		}
		return nil
	})
}
