// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package broker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tinyorbitvn/tinycdi/internal/api"
)

// ConnectionState is the broker's read-only view of a workspace's session
// connection, served to the portal by GET /v1/workspaces/{id}/connection
// (P4). State is one of:
//
//	"none"         — no live lease
//	"connected"    — live lease, renewed inside the staleness window, streams open
//	"disconnected" — live lease, renewed inside the staleness window, no streams
//	"stale"        — lease row still active but last renew is older than 20 s
type ConnectionState struct {
	State         string
	LeaseActive   bool
	LastRenewedAt *time.Time
	// LeaseRef identifies the active lease without exposing its ID: the
	// first 16 hex chars of SHA-256 of the lease ID. Empty without a live
	// lease.
	LeaseRef string
	// StreamEpoch is the active lease's stream_epoch (0 without a lease).
	StreamEpoch uint64
	// StreamOwnerTab is the tab id the current stream was claimed with
	// (empty without a lease, or when the claim carried no valid id). It is
	// populated ONLY when the caller's owner ref is the lease's
	// principal_subject — another principal never learns it.
	StreamOwnerTab string
}

// leaseStaleAfter is the renewal-freshness window: the gateway renews every
// ~10 s against a 30 s TTL, so a renew older than 20 s means the gateway is
// gone while the lease row is still nominally alive.
const leaseStaleAfter = 20 * time.Second

// ConnectionState reports the workspace's connection state from the lease
// and activity tables. It is a pure read — it never mutates lease state,
// refreshes expiry, or slides any timer.
//
// owner is the caller's owner ref (issuer|subject): StreamOwnerTab is
// revealed only when it equals the lease's principal_subject — the field
// tells the caller's OWN tab apart from a foreign one, so it must never be
// served to another principal (nor appear in logs or metrics labels).
func (b *Broker) ConnectionState(ctx context.Context, workspaceUID PlatformID, owner string) (ConnectionState, error) {
	now := b.now()
	var (
		leaseID     string
		streamEpoch int64
		expiresAt   time.Time
		lastRenewed *time.Time
		openStreams int
		ownerTab    *string
		principal   string
	)
	err := b.db.Pool().QueryRow(ctx, `
		SELECT l.id, l.stream_epoch, l.expires_at, l.last_renewed_at,
			COALESCE(a.open_streams, 0), l.stream_owner_tab, l.principal_subject
		FROM connection_lease l
		LEFT JOIN workspace_activity a
			ON a.workspace_id = l.workspace_id
			AND a.runtime_generation = l.runtime_generation
		WHERE l.workspace_id = $1 AND l.state = 'active'`, workspaceUID).
		Scan(&leaseID, &streamEpoch, &expiresAt, &lastRenewed, &openStreams, &ownerTab, &principal)
	if errors.Is(err, pgx.ErrNoRows) {
		return ConnectionState{State: "none"}, nil
	}
	if err != nil {
		return ConnectionState{}, fmt.Errorf("broker: connection state: %w", err)
	}
	// A row past its sliding TTL is dead even before the lazy reaper marks
	// it — an expired lease is no lease.
	if !expiresAt.After(now) {
		return ConnectionState{State: "none"}, nil
	}
	sum := sha256.Sum256([]byte(leaseID))
	st := ConnectionState{
		LeaseActive:   true,
		LastRenewedAt: lastRenewed,
		LeaseRef:      hex.EncodeToString(sum[:])[:16],
		StreamEpoch:   uint64(streamEpoch),
	}
	if ownerTab != nil && principal == owner {
		st.StreamOwnerTab = *ownerTab
	}
	switch {
	case lastRenewed == nil || now.Sub(*lastRenewed) > leaseStaleAfter:
		st.State = "stale"
	case openStreams > 0:
		st.State = "connected"
	default:
		st.State = "disconnected"
	}
	return st, nil
}

// WithInputHook registers fn to be called with the lease's principal
// (principal_subject — the "issuer|subject" owner string) on each recorded
// "input" activity event. The portal wires this to slide the owning user's
// portal session idle timer (D18); connected/disconnect events and rejected
// reports never invoke it.
func WithInputHook(fn func(ctx context.Context, principal string)) Option {
	return func(b *Broker) { b.inputHook = fn }
}

// PublicStater adapts *Broker to the public API's api.ConnectionStater
// contract — same pattern as PublicIssuer, so internal/api never imports
// this package.
type PublicStater struct {
	B *Broker
}

// ConnectionState maps the broker view onto the public response shape. The
// read cannot produce domain denials — ownership was already enforced by the
// handler's workspace lookup — so any error is internal. owner is the
// caller's owner ref; the broker reveals StreamOwnerTab only to the lease's
// own principal.
func (s PublicStater) ConnectionState(ctx context.Context, workspaceUID, owner string) (api.ConnectionStatus, *api.Error) {
	st, err := s.B.ConnectionState(ctx, PlatformID(workspaceUID), owner)
	if err != nil {
		return api.ConnectionStatus{}, api.NewError(api.CodeInternal, "internal error")
	}
	return api.ConnectionStatus{
		State:          st.State,
		LeaseActive:    st.LeaseActive,
		LastRenewedAt:  st.LastRenewedAt,
		LeaseRef:       st.LeaseRef,
		StreamEpoch:    st.StreamEpoch,
		StreamOwnerTab: st.StreamOwnerTab,
	}, nil
}
