// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package broker

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// GatewaySurface is the broker surface a session gateway drives: ticket
// redemption, lease renew/resolve/revoke, session-digest binding/lookup,
// stream epochs and activity reports. *Broker implements it; it matches the
// session gateway's BrokerClient contract structurally, so the gateway
// package stays free of a broker-internals dependency and the two halves
// remain separable.
type GatewaySurface interface {
	RedeemTicket(ctx context.Context, gw GatewayIdentity, opaque string) (Lease, error)
	RenewLease(ctx context.Context, gw GatewayIdentity, leaseID string, fence Fence) (Lease, error)
	ResolveTarget(ctx context.Context, gw GatewayIdentity, leaseID string) (Target, error)
	RevokeLease(ctx context.Context, leaseID string) error
	RevokeLeaseChanged(ctx context.Context, leaseID string) (bool, error)
	ReportActivity(ctx context.Context, gw GatewayIdentity, leaseID string, fence Fence, ev ActivityEvent) error
	BindSession(ctx context.Context, gw GatewayIdentity, leaseID string, d SessionDigest) error
	LeaseBySession(ctx context.Context, gw GatewayIdentity, d SessionDigest) (Lease, error)
	ClaimStream(ctx context.Context, gw GatewayIdentity, leaseID string, fence Fence, ownerTab string) (uint64, error)
}

var _ GatewaySurface = (*Broker)(nil)

// maxLeaseIDLen bounds lease ids accepted from the gateway, mirroring the
// internal HTTP API's path-value validation.
const maxLeaseIDLen = 128

// LocalGateway is the in-process broker client of the co-located session
// gateway (cmd/backend). It replaces the former gateway->broker mTLS hop
// while keeping that hop's guarantees:
//
//   - Identity is pinned at construction from trusted wiring, exactly as
//     the mTLS listener derived it from the verified client certificate:
//     the identity argument the gateway passes on each call is ignored.
//   - Lease ids are validated with the same bounds the HTTP route applied.
//   - Errors are classified the way the remote client classified the HTTP
//     status/code pairs, so the gateway's terminal-vs-transient decisions
//     (fail closed on revoke/stale/denied, retry budget on transient) are
//     unchanged. The original broker error stays in the chain.
type LocalGateway struct {
	b  GatewaySurface
	id GatewayIdentity
}

// NewLocalGateway wires an in-process gateway client for identity id.
// reservedIDs lists identities that must never act as a gateway (the
// operator CN on the internal listener); id may not be one of them.
func NewLocalGateway(b GatewaySurface, id GatewayIdentity, reservedIDs ...string) (*LocalGateway, error) {
	if b == nil {
		return nil, errors.New("broker: LocalGateway requires a broker")
	}
	if strings.TrimSpace(id.ID) == "" || strings.TrimSpace(id.Audience) == "" {
		return nil, errors.New("broker: LocalGateway requires a gateway ID and audience")
	}
	for _, r := range reservedIDs {
		if r != "" && id.ID == r {
			return nil, fmt.Errorf("broker: gateway ID %q is reserved for another identity", id.ID)
		}
	}
	return &LocalGateway{b: b, id: id}, nil
}

// Identity returns the pinned gateway identity.
func (l *LocalGateway) Identity() GatewayIdentity { return l.id }

// RedeemTicket redeems an opaque launch ticket for the pinned identity.
func (l *LocalGateway) RedeemTicket(ctx context.Context, _ GatewayIdentity, opaque string) (Lease, error) {
	if opaque == "" {
		return Lease{}, ErrTicketInvalid
	}
	lease, err := l.b.RedeemTicket(ctx, l.id, opaque)
	return lease, classify("redeem", err)
}

// RenewLease renews the lease for the pinned identity.
func (l *LocalGateway) RenewLease(ctx context.Context, _ GatewayIdentity, leaseID string, fence Fence) (Lease, error) {
	if !leaseIDOK(leaseID) {
		return Lease{}, ErrLeaseInvalid
	}
	lease, err := l.b.RenewLease(ctx, l.id, leaseID, fence)
	return lease, classify("renew", err)
}

// ResolveTarget resolves the runtime upstream for the pinned identity.
func (l *LocalGateway) ResolveTarget(ctx context.Context, _ GatewayIdentity, leaseID string) (Target, error) {
	if !leaseIDOK(leaseID) {
		return Target{}, ErrLeaseInvalid
	}
	t, err := l.b.ResolveTarget(ctx, l.id, leaseID)
	return t, classify("target", err)
}

// RevokeLease revokes a lease (best effort from the gateway's side).
func (l *LocalGateway) RevokeLease(ctx context.Context, leaseID string) error {
	if !leaseIDOK(leaseID) {
		return ErrLeaseInvalid
	}
	return classify("revoke", l.b.RevokeLease(ctx, leaseID))
}

// RevokeLeaseChanged is RevokeLease that also reports whether a live lease
// was actually revoked.
func (l *LocalGateway) RevokeLeaseChanged(ctx context.Context, leaseID string) (bool, error) {
	if !leaseIDOK(leaseID) {
		return false, ErrLeaseInvalid
	}
	changed, err := l.b.RevokeLeaseChanged(ctx, leaseID)
	return changed, classify("revoke", err)
}

// ReportActivity forwards one gateway-observed session signal; the broker
// stamps the receipt time.
func (l *LocalGateway) ReportActivity(ctx context.Context, _ GatewayIdentity, leaseID string, fence Fence, ev ActivityEvent) error {
	if !leaseIDOK(leaseID) {
		return ErrLeaseInvalid
	}
	return classify("activity", l.b.ReportActivity(ctx, l.id, leaseID, fence, ev))
}

// BindSession binds the session-cookie digest d to the lease for the pinned
// identity.
func (l *LocalGateway) BindSession(ctx context.Context, _ GatewayIdentity, leaseID string, d SessionDigest) error {
	if !leaseIDOK(leaseID) {
		return ErrLeaseInvalid
	}
	return classify("bind", l.b.BindSession(ctx, l.id, leaseID, d))
}

// LeaseBySession resolves the live lease bound to the session-cookie digest
// d for the pinned identity.
func (l *LocalGateway) LeaseBySession(ctx context.Context, _ GatewayIdentity, d SessionDigest) (Lease, error) {
	lease, err := l.b.LeaseBySession(ctx, l.id, d)
	return lease, classify("session", err)
}

// ClaimStream claims the next stream epoch on the lease for the pinned
// identity; ownerTab is the claiming tab's id (validated at the broker).
func (l *LocalGateway) ClaimStream(ctx context.Context, _ GatewayIdentity, leaseID string, fence Fence, ownerTab string) (uint64, error) {
	if !leaseIDOK(leaseID) {
		return 0, ErrLeaseInvalid
	}
	epoch, err := l.b.ClaimStream(ctx, l.id, leaseID, fence, ownerTab)
	return epoch, classify("claim", err)
}

func leaseIDOK(id string) bool {
	return id != "" && len(id) <= maxLeaseIDLen && !strings.ContainsAny(id, "/?#")
}

// classifiedError keeps the broker's own error and adds the sentinel the
// remote client would have surfaced for it, so errors.Is matches both.
type classifiedError struct {
	op    string
	err   error
	alias error
}

func (e *classifiedError) Error() string   { return "broker " + e.op + ": " + e.err.Error() }
func (e *classifiedError) Unwrap() []error { return []error{e.err, e.alias} }

// classify maps broker errors onto the classification the former mTLS
// client derived from the internal API's status codes:
//
//   - ErrNotReady (409 INVALID_STATE) was a stale binding — terminal.
//   - ErrTicketExpired (401 on redeem) was an invalid ticket.
//   - A dead lease on the activity route (410) was a revocation.
//
// Everything else already carries the right sentinel (or none, for the
// transient cases such as ErrFreshness) and passes through wrapped.
func classify(op string, err error) error {
	if err == nil {
		return nil
	}
	var alias error
	switch {
	case errors.Is(err, ErrNotReady):
		alias = ErrStaleBinding
	case op == "redeem" && errors.Is(err, ErrTicketExpired):
		alias = ErrTicketInvalid
	case op == "activity" && errors.Is(err, ErrLeaseInvalid):
		alias = ErrRevoked
	default:
		return fmt.Errorf("broker %s: %w", op, err)
	}
	return &classifiedError{op: op, err: err, alias: alias}
}
