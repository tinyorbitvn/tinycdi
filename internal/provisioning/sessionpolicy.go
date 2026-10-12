// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package provisioning

import (
	"context"

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

	// File-transfer channel directions (inert until T11 lands the
	// WorkspaceTemplate leg in v1.3).
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
	Clipboard    string             `json:"clipboard"`
	FileTransfer FileTransferPolicy `json:"fileTransfer"`
}

// FileTransferPolicy is the resolved file-transfer channel. Inert until
// T11 (v1.3): no WorkspaceTemplate fileTransferPolicy leg exists yet, so
// the template side reads Disabled — today's posture — and Mode resolves
// Disabled under every ceiling. The shape ships now so the recorded
// vector is stable when the channel goes live.
type FileTransferPolicy struct {
	Mode string `json:"mode"`
	// MaxFileBytes bounds a single transferred file; nil is the platform
	// default bound (chosen with T11).
	MaxFileBytes *int64 `json:"maxFileBytes"`
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
// may resolve to.
func meetCeiling(tpl TemplateCatalogEntry, c store.SessionPolicyCeiling) SessionPolicy {
	var clipCeil, fileCeil string
	if c.Clipboard != nil {
		clipCeil = *c.Clipboard
	}
	if c.FileTransfer != nil {
		fileCeil = *c.FileTransfer
	}
	return SessionPolicy{
		Clipboard: meet(tpl.ClipboardPolicy, clipCeil, ClipboardSend, ClipboardReceive),
		FileTransfer: FileTransferPolicy{
			// No WorkspaceTemplate fileTransferPolicy exists yet (T11):
			// the template leg is Disabled — today's posture — so the
			// channel resolves Disabled under every ceiling.
			Mode:         meet(PolicyDisabled, fileCeil, FileTransferUpload, FileTransferDownload),
			MaxFileBytes: c.FileMaxBytes,
		},
	}
}

// resolveSessionPolicy returns the effective data-transfer policy for a
// (tenant, template) pair: the meet of the template spec and the tenant
// ceiling row (absent = FollowTemplate). It is the ONLY place the meet
// is computed — consumers never re-implement the lattice (ADR 0010 §2).
// resolveSessionPolicyTx resolves inside the intent transaction, so the
// policy a create/start stamps onto the Workspace CR is read under the
// same snapshot as the rest of the lifecycle write.
func resolveSessionPolicyTx(ctx context.Context, db *store.DB, tx store.Tx, tenantID string, tpl TemplateCatalogEntry) (SessionPolicy, error) {
	c, err := store.NewSessionPolicyStore(db).CeilingTx(ctx, tx, tenantID)
	if err != nil {
		return SessionPolicy{}, err
	}
	return meetCeiling(tpl, c), nil
}

// ResolveSessionPolicy is the pool-side resolver the ticket-issue path
// calls: the meet of the tenant ceiling and the template's own policy,
// computed in one place for every consumer.
func (s *Service) ResolveSessionPolicy(ctx context.Context, tenantID string, tpl TemplateCatalogEntry) (SessionPolicy, error) {
	c, err := store.NewSessionPolicyStore(s.db).Ceiling(ctx, tenantID)
	if err != nil {
		return SessionPolicy{}, err
	}
	return meetCeiling(tpl, c), nil
}
