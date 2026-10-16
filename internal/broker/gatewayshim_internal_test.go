// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package broker

// Unit tests for the in-process gateway shim: identity pinning, lease-id
// validation and the error classification the former mTLS client applied.

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type recordingSurface struct {
	gotIDs []GatewayIdentity
	calls  []string
	err    error
}

func (r *recordingSurface) RedeemTicket(_ context.Context, gw GatewayIdentity, _ string) (Lease, error) {
	r.gotIDs = append(r.gotIDs, gw)
	r.calls = append(r.calls, "redeem")
	return Lease{ID: "lease-1"}, r.err
}

func (r *recordingSurface) RenewLease(_ context.Context, gw GatewayIdentity, _ string, _ Fence) (Lease, error) {
	r.gotIDs = append(r.gotIDs, gw)
	r.calls = append(r.calls, "renew")
	return Lease{ID: "lease-1"}, r.err
}

func (r *recordingSurface) ResolveTarget(_ context.Context, gw GatewayIdentity, _ string) (Target, error) {
	r.gotIDs = append(r.gotIDs, gw)
	r.calls = append(r.calls, "target")
	return Target{}, r.err
}

func (r *recordingSurface) RevokeLeaseChanged(ctx context.Context, id string) (bool, error) {
	return true, r.RevokeLease(ctx, id)
}

func (r *recordingSurface) RevokeLease(_ context.Context, _ string) error {
	r.calls = append(r.calls, "revoke")
	return r.err
}

func (r *recordingSurface) ReportActivity(_ context.Context, gw GatewayIdentity, _ string, _ Fence, _ ActivityEvent) error {
	r.gotIDs = append(r.gotIDs, gw)
	r.calls = append(r.calls, "activity")
	return r.err
}

func (r *recordingSurface) BindSession(_ context.Context, gw GatewayIdentity, _ string, _ SessionDigest) error {
	r.gotIDs = append(r.gotIDs, gw)
	r.calls = append(r.calls, "bind")
	return r.err
}

func (r *recordingSurface) LeaseBySession(_ context.Context, gw GatewayIdentity, _ SessionDigest) (Lease, error) {
	r.gotIDs = append(r.gotIDs, gw)
	r.calls = append(r.calls, "session")
	return Lease{ID: "lease-1"}, r.err
}

func (r *recordingSurface) ClaimStream(_ context.Context, gw GatewayIdentity, _ string, _ Fence, _ string) (uint64, error) {
	r.gotIDs = append(r.gotIDs, gw)
	r.calls = append(r.calls, "claim")
	return 1, r.err
}

func (r *recordingSurface) ReleaseIdleLease(_ context.Context, gw GatewayIdentity, _ string, _ Fence, _ uint64) (bool, error) {
	r.gotIDs = append(r.gotIDs, gw)
	r.calls = append(r.calls, "release")
	return true, r.err
}

func (r *recordingSurface) ClaimTransfer(_ context.Context, gw GatewayIdentity, _, _, _ string) (TransferGrant, error) {
	r.gotIDs = append(r.gotIDs, gw)
	r.calls = append(r.calls, "transfer-claim")
	return TransferGrant{}, r.err
}

func (r *recordingSurface) CompleteTransfer(_ context.Context, gw GatewayIdentity, _, _ string, _ int64, _ bool) error {
	r.gotIDs = append(r.gotIDs, gw)
	r.calls = append(r.calls, "transfer-complete")
	return r.err
}

var pinned = GatewayIdentity{ID: "gateway", Audience: "session.example.test"}

func TestLocalGateway_RequiresIdentity(t *testing.T) {
	if _, err := NewLocalGateway(nil, pinned); err == nil {
		t.Fatal("nil broker accepted")
	}
	if _, err := NewLocalGateway(&recordingSurface{}, GatewayIdentity{ID: "gateway"}); err == nil {
		t.Fatal("empty audience accepted")
	}
	if _, err := NewLocalGateway(&recordingSurface{}, GatewayIdentity{Audience: "a"}); err == nil {
		t.Fatal("empty gateway ID accepted")
	}
	if _, err := NewLocalGateway(&recordingSurface{}, GatewayIdentity{ID: "operator", Audience: "a"}, "operator"); err == nil {
		t.Fatal("reserved operator identity accepted as gateway")
	}
}

// The identity the caller passes is ignored — the shim always presents the
// identity pinned at construction (the mTLS listener used the verified cert).
func TestLocalGateway_PinsIdentity(t *testing.T) {
	rs := &recordingSurface{}
	lg, err := NewLocalGateway(rs, pinned, "operator")
	if err != nil {
		t.Fatal(err)
	}
	spoof := GatewayIdentity{ID: "operator", Audience: "evil.example.test"}
	ctx := context.Background()
	_, _ = lg.RedeemTicket(ctx, spoof, "tkt")
	_, _ = lg.RenewLease(ctx, spoof, "lease-1", Fence{})
	_, _ = lg.ResolveTarget(ctx, spoof, "lease-1")
	_ = lg.ReportActivity(ctx, spoof, "lease-1", Fence{}, ActivityEvent{Type: ActivityInput})
	_ = lg.BindSession(ctx, spoof, "lease-1", SessionDigest{})
	_, _ = lg.LeaseBySession(ctx, spoof, SessionDigest{})
	_, _ = lg.ClaimStream(ctx, spoof, "lease-1", Fence{}, "")
	_, _ = lg.ReleaseIdleLease(ctx, spoof, "lease-1", Fence{}, 1)
	if len(rs.gotIDs) != 8 {
		t.Fatalf("calls=%v", rs.calls)
	}
	for i, id := range rs.gotIDs {
		if id != pinned {
			t.Fatalf("call %s presented identity %+v, want pinned %+v", rs.calls[i], id, pinned)
		}
	}
	if lg.Identity() != pinned {
		t.Fatalf("Identity()=%+v", lg.Identity())
	}
}

func TestLocalGateway_TransferPinsIdentityAndBoundsIDs(t *testing.T) {
	const tid = "xf_abcdefghijklmnopqrstuvwxyz"
	rs := &recordingSurface{}
	lg, _ := NewLocalGateway(rs, pinned, "operator")
	spoof := GatewayIdentity{ID: "operator", Audience: "evil.example.test"}
	ctx := context.Background()
	_, _ = lg.ClaimTransfer(ctx, spoof, "lease-1", tid, "PUT")
	_ = lg.CompleteTransfer(ctx, spoof, "lease-1", tid, 3, true)
	if len(rs.gotIDs) != 2 || rs.gotIDs[0] != pinned || rs.gotIDs[1] != pinned {
		t.Fatalf("transfer calls presented %+v, want the pinned identity", rs.gotIDs)
	}
	for _, bad := range []string{"", "xf_bad", "xf_ABCDEFGHIJKLMNOPQRSTUVWXYZ", "xf_abcdefghijklmnopqrstuvwxy1", "../" + tid} {
		if _, err := lg.ClaimTransfer(ctx, pinned, "lease-1", bad, "GET"); !errors.Is(err, ErrTransferInvalid) {
			t.Errorf("claim %q err=%v", bad, err)
		}
		if err := lg.CompleteTransfer(ctx, pinned, "lease-1", bad, 0, true); !errors.Is(err, ErrTransferInvalid) {
			t.Errorf("complete %q err=%v", bad, err)
		}
	}
	if _, err := lg.ClaimTransfer(ctx, pinned, "a/b", tid, "GET"); !errors.Is(err, ErrLeaseInvalid) {
		t.Errorf("bad lease id err=%v", err)
	}
	if len(rs.calls) != 2 {
		t.Fatalf("invalid input reached the broker: %v", rs.calls)
	}
}

func TestLocalGateway_RejectsBadLeaseIDs(t *testing.T) {
	rs := &recordingSurface{}
	lg, _ := NewLocalGateway(rs, pinned)
	ctx := context.Background()
	for _, id := range []string{"", "a/b", "a?b", "a#b", strings.Repeat("x", 129)} {
		if _, err := lg.RenewLease(ctx, pinned, id, Fence{}); !errors.Is(err, ErrLeaseInvalid) {
			t.Errorf("renew %q err=%v", id, err)
		}
		if _, err := lg.ResolveTarget(ctx, pinned, id); !errors.Is(err, ErrLeaseInvalid) {
			t.Errorf("target %q err=%v", id, err)
		}
		if err := lg.RevokeLease(ctx, id); !errors.Is(err, ErrLeaseInvalid) {
			t.Errorf("revoke %q err=%v", id, err)
		}
		if err := lg.ReportActivity(ctx, pinned, id, Fence{}, ActivityEvent{}); !errors.Is(err, ErrLeaseInvalid) {
			t.Errorf("activity %q err=%v", id, err)
		}
		if err := lg.BindSession(ctx, pinned, id, SessionDigest{}); !errors.Is(err, ErrLeaseInvalid) {
			t.Errorf("bind %q err=%v", id, err)
		}
		if _, err := lg.ClaimStream(ctx, pinned, id, Fence{}, ""); !errors.Is(err, ErrLeaseInvalid) {
			t.Errorf("claim %q err=%v", id, err)
		}
		if _, err := lg.ReleaseIdleLease(ctx, pinned, id, Fence{}, 1); !errors.Is(err, ErrLeaseInvalid) {
			t.Errorf("release %q err=%v", id, err)
		}
	}
	if _, err := lg.RedeemTicket(ctx, pinned, ""); !errors.Is(err, ErrTicketInvalid) {
		t.Errorf("empty ticket err=%v", err)
	}
	if len(rs.calls) != 0 {
		t.Fatalf("invalid input reached the broker: %v", rs.calls)
	}
}

// Classification mirrors the former remote client: ErrNotReady was a 409
// INVALID_STATE → stale binding (terminal), an expired ticket was an
// invalid ticket, a dead lease on activity was a 410 revocation. The
// original error stays matchable; transient errors gain no terminal alias.
func TestLocalGateway_ClassifiesErrors(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name    string
		err     error
		call    func(*LocalGateway) error
		want    []error
		notWant []error
	}{
		{"renew not ready", ErrNotReady, func(l *LocalGateway) error {
			_, err := l.RenewLease(ctx, pinned, "l", Fence{})
			return err
		}, []error{ErrNotReady, ErrStaleBinding}, nil},
		{"redeem expired", ErrTicketExpired, func(l *LocalGateway) error {
			_, err := l.RedeemTicket(ctx, pinned, "t")
			return err
		}, []error{ErrTicketExpired, ErrTicketInvalid}, nil},
		{"activity dead lease", ErrLeaseInvalid, func(l *LocalGateway) error {
			return l.ReportActivity(ctx, pinned, "l", Fence{}, ActivityEvent{})
		}, []error{ErrLeaseInvalid, ErrRevoked}, nil},
		{"renew freshness stays transient", ErrFreshness, func(l *LocalGateway) error {
			_, err := l.RenewLease(ctx, pinned, "l", Fence{})
			return err
		}, []error{ErrFreshness}, []error{ErrStaleBinding, ErrRevoked, ErrDenied}},
		{"target revoked", ErrRevoked, func(l *LocalGateway) error {
			_, err := l.ResolveTarget(ctx, pinned, "l")
			return err
		}, []error{ErrRevoked}, nil},
		{"redeem in use", ErrConnectionInUse, func(l *LocalGateway) error {
			_, err := l.RedeemTicket(ctx, pinned, "t")
			return err
		}, []error{ErrConnectionInUse}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lg, _ := NewLocalGateway(&recordingSurface{err: tc.err}, pinned)
			err := tc.call(lg)
			for _, w := range tc.want {
				if !errors.Is(err, w) {
					t.Errorf("err=%v does not match %v", err, w)
				}
			}
			for _, nw := range tc.notWant {
				if errors.Is(err, nw) {
					t.Errorf("err=%v unexpectedly matches %v", err, nw)
				}
			}
		})
	}
	lg, _ := NewLocalGateway(&recordingSurface{}, pinned)
	if _, err := lg.RenewLease(ctx, pinned, "l", Fence{}); err != nil {
		t.Fatalf("success path err=%v", err)
	}
}
