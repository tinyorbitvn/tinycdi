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
	ID                string `json:"leaseId"`
	WorkspaceUID      string `json:"workspaceUID"`
	TenantID          string `json:"tenantId"`
	PrincipalSubject  string `json:"principalSubject"`
	RuntimeGeneration uint64 `json:"runtimeGeneration"`
	RuntimeUID        string `json:"runtimeUID"`
	FencingVersion    uint64 `json:"fencingVersion"`
	GatewayID         string `json:"gatewayId"`
	StreamEpoch       uint64 `json:"streamEpoch"`
	// StreamOwnerTab is the tab id the current stream was claimed with —
	// populated only while the stored id still names the current stream
	// (stream_owner_epoch = stream_epoch); "" without a claim or when the
	// claim carried no valid id. Gateway-side correlator only: never a
	// metric label or log field.
	StreamOwnerTab string `json:"-"`
	// PortalSessionDigest is the sessions.id key form (hex of the SHA-256
	// of the portal session id) the lease was minted under — populated on
	// every loadLease read so input activity can be credited to exactly
	// that session (SR-1-F3). "" for pre-migration-018 rows minted before
	// the binding column existed.
	PortalSessionDigest string `json:"-"`
	// ClipboardPolicy is the workspace template's clipboard policy as
	// recorded on the ticket at issue — populated only on redemption, so
	// the gateway's post-redemption redirect can re-assert the client's
	// clipboard flags (V3.24). "" on every other lease read.
	ClipboardPolicy string    `json:"clipboardPolicy,omitempty"`
	ExpiresAt       time.Time `json:"expiresAt"`
}

// portalSessionCheck is the liveness of the lease's bound portal session,
// evaluated in the same statement that loads the lease row.
type portalSessionCheck string

const (
	// portalSessionOK — no binding recorded, or the bound row is live.
	portalSessionOK portalSessionCheck = "ok"
	// portalSessionAbsent — the bound sessions row is gone.
	portalSessionAbsent portalSessionCheck = "absent"
	// portalSessionInvalid — the bound row exists but fails the epoch,
	// absolute-expiry or idle-window check (same semantics as
	// RedeemTicket's re-check; idle needs WithSessionIdle).
	portalSessionInvalid portalSessionCheck = "invalid"
)

// currentSessionEpochSQL resolves the session epoch inside the lease read,
// mirroring store.currentEpochSQL: a rotation (the restore procedure)
// invalidates every bound lease on its very next check.
const currentSessionEpochSQL = `(SELECT value FROM platform_meta WHERE key = 'session_epoch')`

// portalSessionLiveSQL returns the predicate asserting that the joined
// sessions row (alias s) backing a ticket/lease is still live: current
// epoch (post-restore rotations fail), inside its absolute expiry, and —
// when the broker knows the portal idle window (WithSessionIdle) — a
// last_seen_at still inside it (FIX-IDLE / SR-1-F2): a session that has
// idled out under RequireAuth is dead to the lease layer on the same
// terms. argIdx is the bind index the idle interval occupies; the second
// return carries the interval argument to append, or nil when no idle
// window is configured.
func (b *Broker) portalSessionLiveSQL(argIdx int) (string, []any) {
	live := `s.epoch IS NOT DISTINCT FROM ` + currentSessionEpochSQL + `
				  AND (s.expires_at IS NULL OR s.expires_at > now())`
	if b.sessionIdle <= 0 {
		return live, nil
	}
	return live + fmt.Sprintf(`
				  AND s.last_seen_at > now() - $%d::interval`, argIdx),
		[]any{fmt.Sprintf("%dms", b.sessionIdle.Milliseconds())}
}

// loadLease fetches the lease row including its lifecycle state and the
// liveness of its bound portal session (S17 defence-in-depth): the CASE
// rides the same row read, joined to sessions by primary key, so the check
// costs one extra indexed probe per call — no additional round trip.
func (b *Broker) loadLease(ctx context.Context, leaseID string) (Lease, string, portalSessionCheck, error) {
	var (
		l          Lease
		state      string
		ownerTab   *string
		ownerEpoch *int64
		portalSess portalSessionCheck
		digestHex  *string
	)
	livePred, liveArgs := b.portalSessionLiveSQL(2)
	args := append([]any{leaseID}, liveArgs...)
	err := b.db.Pool().QueryRow(ctx,
		`SELECT l.id, l.workspace_id, l.tenant_id, l.principal_subject,
			l.runtime_generation, l.runtime_uid, l.fencing_version, l.gateway_id,
			l.state, l.expires_at, l.stream_epoch, l.stream_owner_tab, l.stream_owner_epoch,
			encode(l.portal_session_digest, 'hex'),
			CASE
				WHEN l.portal_session_digest IS NULL THEN 'ok'
				WHEN s.id IS NULL THEN 'absent'
				WHEN NOT (`+livePred+`) THEN 'invalid'
				ELSE 'ok'
			END
		 FROM connection_lease l
		 LEFT JOIN sessions s ON s.id = encode(l.portal_session_digest, 'hex')
		 WHERE l.id = $1`, args...).
		Scan(&l.ID, &l.WorkspaceUID, &l.TenantID, &l.PrincipalSubject,
			&l.RuntimeGeneration, &l.RuntimeUID, &l.FencingVersion,
			&l.GatewayID, &state, &l.ExpiresAt, &l.StreamEpoch, &ownerTab, &ownerEpoch,
			&digestHex, &portalSess)
	if errors.Is(err, pgx.ErrNoRows) {
		return Lease{}, "", "", ErrLeaseInvalid
	}
	if err != nil {
		return Lease{}, "", "", fmt.Errorf("broker: lease lookup: %w", err)
	}
	// The stored owner id counts only while it names the current stream
	// (stream_owner_epoch = stream_epoch): a replica predating the columns
	// bumps the epoch without naming them, so a stale id reads as absent
	// (R-V3c) — same rule ConnectionState applies.
	if ownerTab != nil && ownerEpoch != nil && *ownerEpoch >= 0 &&
		uint64(*ownerEpoch) == l.StreamEpoch {
		l.StreamOwnerTab = *ownerTab
	}
	if digestHex != nil {
		l.PortalSessionDigest = *digestHex
	}
	return l, state, portalSess, nil
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
// bound portal session gone or invalid -> the lease is revoked on the spot
// and -> ErrRevoked (S17 defence-in-depth); foreign gateway -> ErrDenied.
// Time-expired rows are lazily marked 'expired' so the partial unique
// index frees the workspace.
//
// The lazy expiry is authoritative for drain accounting, like RevokeLease:
// the lease's streams can no longer report their close (every gateway path
// refuses a dead lease), so the active->expired transition zeroes the bound
// generation's open_streams in the same transaction and anchors
// disconnected_since, keeping the disconnect grace semantics of §8. A
// concurrent transition (RowsAffected = 0) skips the accounting — whoever
// moved the row owned it.
func (b *Broker) liveLease(ctx context.Context, gw GatewayIdentity, leaseID string, now time.Time) (Lease, error) {
	l, state, portalSess, err := b.loadLease(ctx, leaseID)
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
	// S17 defence-in-depth: a lease minted under a portal session that is
	// gone or no longer valid (signed out, absolutely expired, or carrying
	// a pre-rotation epoch after a database restore) must die however it is
	// reached — the sign-out revoke is one barrier, this check is the
	// second, so a revoked lease resurrected by a DB restore cannot be
	// renewed past its residual TTL and a copied cookie can never
	// rehydrate it. The check rode the lease read (no extra round trip).
	// The revoke runs before the gateway-identity check so ANY caller —
	// renew, attach, claim, resolve, foreign probe — kills the dead row.
	//
	// Leases with NULL portal_session_digest are exempt by design: rows
	// minted before the binding column existed carry no session to verify
	// against, and revoking them on sight would mass-kill sessions still
	// being renewed by pre-upgrade replicas during a rolling deploy. They
	// keep the plain TTL lifecycle — a dying gateway stops renewing and
	// the lease lapses inside one TTL — and the DR runbook's unconditional
	// post-restore lease sweep covers them (RevokePortalSession cannot:
	// it matches on the digest they lack).
	if portalSess != portalSessionOK {
		b.revokeDeadSessionLease(ctx, l, portalSess)
		return Lease{}, ErrRevoked
	}
	if l.GatewayID != gw.ID {
		return Lease{}, ErrDenied
	}
	return l, nil
}

// revokeDeadSessionLease revokes a live lease whose bound portal session
// failed the liveness check — the same single-statement revoke plus
// in-transaction drain accounting RevokeLeaseChanged performs — then
// counts the detection and emits one log line. A concurrent transition
// (another replica's renew won the revoke) counts nothing: the winner
// already logged it.
func (b *Broker) revokeDeadSessionLease(ctx context.Context, l Lease, reason portalSessionCheck) {
	changed, err := b.RevokeLeaseChanged(ctx, l.ID)
	switch {
	case err != nil:
		if b.log != nil {
			b.log.Warn("portal-session lease revoke failed",
				"lease", l.ID, "reason", string(reason), "err", err)
		}
	case changed:
		if b.metrics != nil {
			b.metrics.IncLeaseSessionMissing(string(reason))
		}
		if b.log != nil {
			b.log.Info("lease revoked: bound portal session no longer valid",
				"lease", l.ID, "reason", string(reason))
		}
	}
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
	_, err := b.RevokeLeaseChanged(ctx, leaseID)
	return err
}

// RevokeLeaseChanged is RevokeLease that also reports whether a live lease
// was actually revoked: false for an unknown or already-dead lease, where
// nothing changed.
func (b *Broker) RevokeLeaseChanged(ctx context.Context, leaseID string) (bool, error) {
	now := b.now()
	changed := false
	err := b.db.WithTx(ctx, func(tx store.Tx) error {
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
		changed = true
		// The revoked lease was the workspace's only active lease (partial
		// unique index), so every stream counted on its generation is now
		// closing but cannot report it — close the accounting atomically.
		if err := closeStreamsTx(ctx, tx, wsUID, uint64(gen), now); err != nil {
			return fmt.Errorf("broker: close revoked streams: %w", err)
		}
		return nil
	})
	return changed && err == nil, err
}
