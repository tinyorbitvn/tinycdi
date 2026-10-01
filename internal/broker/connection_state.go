// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package broker

import (
	"context"
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
}

// leaseStaleAfter is the renewal-freshness window: the gateway renews every
// ~10 s against a 30 s TTL, so a renew older than 20 s means the gateway is
// gone while the lease row is still nominally alive.
const leaseStaleAfter = 20 * time.Second

// ConnectionState reports the workspace's connection state from the lease
// and activity tables. It is a pure read — it never mutates lease state,
// refreshes expiry, or slides any timer.
func (b *Broker) ConnectionState(ctx context.Context, workspaceUID PlatformID) (ConnectionState, error) {
	now := b.now()
	var (
		expiresAt    time.Time
		lastRenewed  *time.Time
		openStreams  int
	)
	err := b.db.Pool().QueryRow(ctx, `
		SELECT l.expires_at, l.last_renewed_at,
			COALESCE(a.open_streams, 0)
		FROM connection_lease l
		LEFT JOIN workspace_activity a
			ON a.workspace_id = l.workspace_id
			AND a.runtime_generation = l.runtime_generation
		WHERE l.workspace_id = $1 AND l.state = 'active'`, workspaceUID).
		Scan(&expiresAt, &lastRenewed, &openStreams)
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
	st := ConnectionState{LeaseActive: true, LastRenewedAt: lastRenewed}
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
// handler's workspace lookup — so any error is internal.
func (s PublicStater) ConnectionState(ctx context.Context, workspaceUID string) (api.ConnectionStatus, *api.Error) {
	st, err := s.B.ConnectionState(ctx, PlatformID(workspaceUID))
	if err != nil {
		return api.ConnectionStatus{}, api.NewError(api.CodeInternal, "internal error")
	}
	return api.ConnectionStatus{
		State:         st.State,
		LeaseActive:   st.LeaseActive,
		LastRenewedAt: st.LastRenewedAt,
	}, nil
}
