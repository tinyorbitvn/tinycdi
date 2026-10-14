// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package provisioning

import (
	"context"

	cdiv1 "github.com/tinyorbitvn/tinycdi/api/v1"
	"github.com/tinyorbitvn/tinycdi/internal/store"
)

// Session policy values — the tenant ceiling's channel states and the
// resolved vector's enum spellings. Direction names are from the user's
// point of view, the openapi.yaml convention: clipboard Send is
// client→workspace, Receive is workspace→client; fileTransfer Upload is
// device→workspace home, Download is workspace→device.
const (
	// PolicyFollowTemplate is the ceiling's "not narrowed" state. It is
	// never stored: the row holds NULL for a followed channel and is
	// absent when every channel follows. It appears only on the wire
	// (the admin API's read/write enum).
	PolicyFollowTemplate = "FollowTemplate"
	PolicyDisabled       = "Disabled"
	PolicyBidirectional  = "Bidirectional"

	// Clipboard channel directions.
	ClipboardSend    = "Send"
	ClipboardReceive = "Receive"

	// File-transfer channel directions (ADR 0010 T11).
	FileTransferUpload   = "Upload"
	FileTransferDownload = "Download"
)

// SessionPolicy is the resolved (effective) session data-transfer vector
// for a (tenant, template) pair — the meet of the template's own policy
// and the tenant ceiling (ADR 0010 §1). It is recorded wherever a
// decision is taken: on the launch_ticket row (what the client was
// offered), on the Workspace CR spec.sessionPolicy (what the runtime was
// built with), and — from v1.3 — on each file-transfer row. Audit reads
// the record; it never re-derives.
type SessionPolicy struct {
	Clipboard    string                   `json:"clipboard"`
	FileTransfer cdiv1.FileTransferPolicy `json:"fileTransfer"`
	// MaxFileBytes is the per-file bound: the ceiling's file_max_bytes
	// clipped to the platform bound (-file-transfer-max-bytes); 0 when
	// FileTransfer is Disabled.
	MaxFileBytes int64 `json:"maxFileBytes,omitempty"`
}

// meet resolves the shared direction lattice: Disabled is bottom,
// Bidirectional is top, the two single directions (dirA/dirB) are
// incomparable and meet to Disabled (Send ∩ Receive = Disabled, Upload ∩
// Download = Disabled). The ceiling can only narrow — it never widens
// past the template:
//
//	template \ ceiling | FollowTemplate | Disabled | dirA / dirB | Bidirectional
//	any T              | T              | Disabled | meet(T,dir)  | T
//
// ceiling "" or an unrecognized value means FollowTemplate and returns
// template unchanged: the ceiling column is CHECK-constrained at the
// store and enum-validated at the API, so anything but a real direction
// narrows nothing — treating "no ceiling" as "denied" would conflate the
// two states the lattice exists to keep apart.
func meet(template, ceiling, dirA, dirB string) string {
	switch ceiling {
	case "", PolicyFollowTemplate:
		return template
	case PolicyDisabled:
		return PolicyDisabled
	case PolicyBidirectional:
		return template
	case dirA:
		if template == dirA || template == PolicyBidirectional {
			return dirA
		}
		return PolicyDisabled
	case dirB:
		if template == dirB || template == PolicyBidirectional {
			return dirB
		}
		return PolicyDisabled
	}
	return template
}

// meetCeiling applies a stored ceiling row to a template's own policy —
// the whole resolved vector in one place. Absent row is FollowTemplate
// on every channel: the upgrade default, and the only state "no row"
// may resolve to. bound is the platform per-file bound
// (-file-transfer-max-bytes) the ceiling's file_max_bytes clips to; a
// resolved-Disabled channel carries 0.
func meetCeiling(tpl TemplateCatalogEntry, c store.SessionPolicyCeiling, bound int64) SessionPolicy {
	var clipCeil, ftCeil string
	if c.Clipboard != nil {
		clipCeil = *c.Clipboard
	}
	if c.FileTransfer != nil {
		ftCeil = *c.FileTransfer
	}
	ftTpl := tpl.FileTransferPolicy
	if ftTpl == "" {
		ftTpl = PolicyDisabled
	}
	sp := SessionPolicy{
		Clipboard:    meet(tpl.ClipboardPolicy, clipCeil, ClipboardSend, ClipboardReceive),
		FileTransfer: cdiv1.FileTransferPolicy(meet(ftTpl, ftCeil, FileTransferUpload, FileTransferDownload)),
	}
	if sp.FileTransfer != cdiv1.FileTransferDisabled {
		sp.MaxFileBytes = bound
		if c.FileMaxBytes != nil && *c.FileMaxBytes < bound {
			sp.MaxFileBytes = *c.FileMaxBytes
		}
	}
	return sp
}

// resolveSessionPolicy returns the effective data-transfer policy for a
// (tenant, template) pair: the meet of the template spec and the tenant
// ceiling row (absent = FollowTemplate). It is the ONLY place the meet
// is computed — consumers never re-implement the lattice (ADR 0010 §2).
// resolveSessionPolicyTx resolves inside the intent transaction, so the
// policy a create/start stamps onto the Workspace CR is read under the
// same snapshot as the rest of the lifecycle write.
func resolveSessionPolicyTx(ctx context.Context, db *store.DB, tx store.Tx, tenantID string, tpl TemplateCatalogEntry, bound int64) (SessionPolicy, error) {
	c, err := store.NewSessionPolicyStore(db).CeilingTx(ctx, tx, tenantID)
	if err != nil {
		return SessionPolicy{}, err
	}
	return meetCeiling(tpl, c, bound), nil
}

// ResolveSessionPolicy is the pool-side resolver the ticket-issue path
// calls: the meet of the tenant ceiling and the template's own policy,
// computed in one place for every consumer.
func (s *Service) ResolveSessionPolicy(ctx context.Context, tenantID string, tpl TemplateCatalogEntry) (SessionPolicy, error) {
	c, err := store.NewSessionPolicyStore(s.db).Ceiling(ctx, tenantID)
	if err != nil {
		return SessionPolicy{}, err
	}
	return meetCeiling(tpl, c, s.fileBound), nil
}

// PolicyWithin reports whether a workspace running with applied grants
// nothing the effective policy does not: every channel of applied is at or
// below effective in the lattice, and its size bound is no larger. F3 uses
// it to find running workspaces a tightened ceiling has made too loose.
func PolicyWithin(applied, effective SessionPolicy) bool {
	return meet(applied.Clipboard, effective.Clipboard, ClipboardSend, ClipboardReceive) == applied.Clipboard &&
		meet(string(applied.FileTransfer), string(effective.FileTransfer), FileTransferUpload, FileTransferDownload) == string(applied.FileTransfer) &&
		applied.MaxFileBytes <= effective.MaxFileBytes
}

// DirectionAllows reports whether policy permits a transfer in direction
// ("upload" = device to workspace, "download" = workspace to device).
func DirectionAllows(policy cdiv1.FileTransferPolicy, direction string) bool {
	switch direction {
	case "upload":
		return policy == cdiv1.FileTransferUpload || policy == cdiv1.FileTransferBidirectional
	case "download":
		return policy == cdiv1.FileTransferDownload || policy == cdiv1.FileTransferBidirectional
	}
	return false
}
