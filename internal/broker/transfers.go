// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package broker

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/tinyorbitvn/tinycdi/internal/observability"
	"github.com/tinyorbitvn/tinycdi/internal/store"
)

var (
	// ErrTransferInvalid: unknown, spent, expired, foreign or mismatched grant.
	ErrTransferInvalid = errors.New("broker: transfer invalid, used or expired")
	// ErrTransferDenied: the policy in force now refuses the direction or size.
	ErrTransferDenied = errors.New("broker: transfer denied by policy")
	// ErrTransferBusy: another transfer stream is live on the workspace.
	ErrTransferBusy = errors.New("broker: a transfer is already in progress")
)

// TransferPolicy is the file-transfer leg of the policy in force now.
type TransferPolicy struct {
	Mode     string // Disabled | Upload | Download | Bidirectional
	MaxBytes int64
}

// TransferPolicyResolver re-resolves the workspace's policy at claim time
// (template ∧ current tenant ceiling ∧ platform bound).
type TransferPolicyResolver func(ctx context.Context, tenantID, workspaceID string) (TransferPolicy, error)

// TransferGrant is what a successful claim lets the gateway move.
type TransferGrant struct {
	ID           string `json:"id"`
	WorkspaceUID string `json:"workspaceUID"`
	Direction    string `json:"direction"`
	Path         string `json:"path"`
	MaxBytes     int64  `json:"maxBytes"`
}

// WithTransferPolicy installs the claim-time policy resolver. Without one,
// every claim is refused (fail closed).
func (b *Broker) WithTransferPolicy(r TransferPolicyResolver) *Broker {
	b.transferPolicy = r
	return b
}

// directionAllowed mirrors provisioning.DirectionAllows on the string mode
// the resolver returns (the broker does not import provisioning).
func directionAllowed(mode, direction string) bool {
	switch direction {
	case "upload":
		return mode == "Upload" || mode == "Bidirectional"
	case "download":
		return mode == "Download" || mode == "Bidirectional"
	}
	return false
}

func methodDirection(method string) string {
	switch method {
	case "PUT":
		return "upload"
	case "GET":
		return "download"
	}
	return ""
}

// ClaimTransfer spends a minted grant for the gateway session holding
// leaseID: pending → claimed in one conditional update, after checking the
// lease is live and bound to the current incarnation, the grant belongs to
// the lease's workspace, principal and incarnation, the method matches the
// direction, no other transfer stream is live on the workspace (the
// workspace row lock serializes claims), and the policy in force NOW still
// allows the direction and the declared size. A policy refusal marks the
// row failed. Success and every refusal of a grant in the lease's own
// workspace are audited in the same transaction, so a failed audit insert
// fails the claim (fail closed).
func (b *Broker) ClaimTransfer(ctx context.Context, gw GatewayIdentity, leaseID, transferID, method string) (TransferGrant, error) {
	if b.transferPolicy == nil {
		return TransferGrant{}, ErrTransferDenied
	}
	now := b.now()
	l, err := b.liveLease(ctx, gw, leaseID, now)
	if err != nil {
		return TransferGrant{}, err
	}
	binding, err := b.boundCurrent(ctx, l, now)
	if err != nil {
		return TransferGrant{}, err
	}
	// Resolved before the row lock so the resolver's own reads never run
	// under it. An error fails the claim closed.
	pol, err := b.transferPolicy(ctx, l.TenantID, l.WorkspaceUID)
	if err != nil {
		return TransferGrant{}, fmt.Errorf("broker: resolve transfer policy: %w", err)
	}
	want := methodDirection(method)
	iss, sub, _ := strings.Cut(l.PrincipalSubject, "|")
	actor := observability.ActorRef(iss, sub)

	var grant TransferGrant
	var refusal error
	err = b.db.WithTx(ctx, func(tx store.Tx) error {
		// The first statement: serialize claims per workspace. NO KEY UPDATE
		// does not block foreign-key lookups on the workspace row.
		if _, err := tx.Exec(ctx, `SELECT 1 FROM workspaces WHERE id = $1 FOR NO KEY UPDATE`, l.WorkspaceUID); err != nil {
			return fmt.Errorf("broker: lock workspace %w", err)
		}
		var (
			ws, rowIss, rowSub, rtUID, dir, path string
			gen, declared                        int64
		)
		err := tx.QueryRow(ctx, `
			SELECT workspace_id, principal_issuer, principal_subject, runtime_generation, runtime_uid,
			       direction, path, declared_bytes
			FROM session_file_transfer
			WHERE id = $1 AND status = 'pending' AND expires_at > $2 FOR UPDATE`, transferID, now).
			Scan(&ws, &rowIss, &rowSub, &gen, &rtUID, &dir, &path, &declared)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && ws != l.WorkspaceUID) {
			// Unknown, spent, expired, or another workspace's grant: nothing
			// of this workspace to audit.
			return ErrTransferInvalid
		}
		if err != nil {
			return fmt.Errorf("broker: load transfer %w", err)
		}
		details := map[string]string{
			"phase": "claim", "direction": dir, "path": path,
			"declared": strconv.FormatInt(declared, 10), "policy": pol.Mode,
		}
		emit := func(outcome, errCode, reason string) error {
			if reason != "" {
				details["reason"] = reason
			}
			_, err := store.InsertAudit(ctx, tx, store.AuditRecord{
				Tenant: l.TenantID, ActorRef: actor, Action: "file.transfer." + dir,
				TargetKind: "workspace", TargetID: l.WorkspaceUID, RequestID: transferID,
				Outcome: outcome, ErrorCode: errCode, Details: details,
			})
			return err
		}
		if rowIss+"|"+rowSub != l.PrincipalSubject ||
			uint64(gen) != binding.RuntimeGeneration || rtUID != binding.RuntimeUID || dir != want {
			refusal = ErrTransferInvalid // the audit row commits, the grant stays pending
			return emit("denied", "INVALID_STATE", "mismatch")
		}
		var live int
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM session_file_transfer
			WHERE workspace_id = $1 AND status = 'claimed' AND claimed_at > $2`,
			ws, now.Add(-store.FileTransferClaimDeadAfter)).Scan(&live); err != nil {
			return fmt.Errorf("broker: live transfers %w", err)
		}
		if live > 0 {
			refusal = ErrTransferBusy
			return emit("denied", "INVALID_STATE", "busy")
		}
		if !directionAllowed(pol.Mode, dir) || pol.MaxBytes <= 0 || (dir == "upload" && declared > pol.MaxBytes) {
			if _, err := tx.Exec(ctx,
				`UPDATE session_file_transfer SET status = 'failed', completed_at = $2 WHERE id = $1 AND status = 'pending'`,
				transferID, now); err != nil {
				return fmt.Errorf("broker: fail denied transfer %w", err)
			}
			refusal = ErrTransferDenied
			return emit("denied", "FORBIDDEN", "policy")
		}
		tag, err := tx.Exec(ctx, `
			UPDATE session_file_transfer SET status = 'claimed', claimed_at = $2
			WHERE id = $1 AND status = 'pending'`, transferID, now)
		if err != nil {
			return fmt.Errorf("broker: claim transfer %w", err)
		}
		if tag.RowsAffected() != 1 {
			return ErrTransferInvalid
		}
		if err := emit("success", "", ""); err != nil {
			return err
		}
		capBytes := pol.MaxBytes
		if dir == "upload" {
			capBytes = declared // declared <= pol.MaxBytes, checked above
		}
		grant = TransferGrant{ID: transferID, WorkspaceUID: ws, Direction: dir, Path: path, MaxBytes: capBytes}
		return nil
	})
	if err != nil {
		return TransferGrant{}, err
	}
	if refusal != nil {
		return TransferGrant{}, refusal
	}
	return grant, nil
}

// CompleteTransfer closes a claimed grant with the byte count the gateway
// moved and records the outcome. The lease is loaded without the liveness
// gate: the stream was authorized at claim, so a lease revoked mid-stream may
// still close its row. The gateway identity and the lease's workspace still
// bind, so one workspace's lease can never close another's grant.
func (b *Broker) CompleteTransfer(ctx context.Context, gw GatewayIdentity, leaseID, transferID string, bytesDone int64, ok bool) error {
	l, _, _, err := b.loadLease(ctx, leaseID)
	if err != nil {
		return err
	}
	if l.GatewayID != gw.ID {
		return ErrDenied
	}
	if bytesDone < 0 {
		bytesDone = 0
	}
	now := b.now()
	status, outcome := "completed", "success"
	if !ok {
		status, outcome = "failed", "failure"
	}
	return b.db.WithTx(ctx, func(tx store.Tx) error {
		var iss, sub, dir, path, policy string
		var declared int64
		err := tx.QueryRow(ctx, `
			UPDATE session_file_transfer SET status = $2, bytes_done = $3, completed_at = $4
			WHERE id = $1 AND status = 'claimed' AND workspace_id = $5
			RETURNING principal_issuer, principal_subject, direction, path, declared_bytes, resolved_policy`,
			transferID, status, bytesDone, now, l.WorkspaceUID).
			Scan(&iss, &sub, &dir, &path, &declared, &policy)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrTransferInvalid
		}
		if err != nil {
			return fmt.Errorf("broker: complete transfer %w", err)
		}
		_, err = store.InsertAudit(ctx, tx, store.AuditRecord{
			Tenant: l.TenantID, ActorRef: observability.ActorRef(iss, sub), Action: "file.transfer." + dir,
			TargetKind: "workspace", TargetID: l.WorkspaceUID, RequestID: transferID,
			Outcome: outcome,
			Details: map[string]string{
				"phase": "complete", "direction": dir, "path": path, "policy": policy,
				"declared": strconv.FormatInt(declared, 10), "bytes": strconv.FormatInt(bytesDone, 10),
			},
		})
		return err
	})
}
