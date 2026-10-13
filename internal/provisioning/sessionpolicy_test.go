// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package provisioning_test

import (
	"context"
	"testing"

	cdiv1 "github.com/tinyorbitvn/tinycdi/api/v1"
	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
	"github.com/tinyorbitvn/tinycdi/internal/store"
)

// The pool-side resolver over real ceiling rows (pg-backed; skips without
// docker). The meet matrix itself is white-box-tested next to the lattice
// in sessionpolicy_internal_test.go.

func strPtr(s string) *string { return &s }

func TestResolveSessionPolicy_NoRowIsFollowTemplate(t *testing.T) {
	db := recoveryDB(t)
	svc := provisioning.NewService(db)

	got, err := svc.ResolveSessionPolicy(context.Background(), "tenant-a",
		provisioning.TemplateCatalogEntry{ClipboardPolicy: provisioning.PolicyBidirectional})
	if err != nil {
		t.Fatal(err)
	}
	if got.Clipboard != provisioning.PolicyBidirectional {
		t.Fatalf("no ceiling row must resolve the template's own policy, got %+v", got)
	}
	if got.FileTransfer != cdiv1.FileTransferDisabled {
		t.Fatalf("fileTransfer resolves Disabled until T11: %+v", got.FileTransfer)
	}
}

func TestResolveSessionPolicy_CeilingNarrows(t *testing.T) {
	db := recoveryDB(t)
	svc := provisioning.NewService(db)
	ctx := context.Background()
	sps := store.NewSessionPolicyStore(db)

	for _, tc := range []struct {
		name     string
		template string
		ceiling  string
		want     string
	}{
		{"Disabled over Bidirectional", provisioning.PolicyBidirectional, provisioning.PolicyDisabled, provisioning.PolicyDisabled},
		{"Send over Bidirectional", provisioning.PolicyBidirectional, provisioning.ClipboardSend, provisioning.ClipboardSend},
		{"Receive over Send", provisioning.ClipboardSend, provisioning.ClipboardReceive, provisioning.PolicyDisabled},
		{"Bidirectional ceiling cannot widen", provisioning.ClipboardSend, provisioning.PolicyBidirectional, provisioning.ClipboardSend},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := sps.SetCeiling(ctx, "tenant-a",
				store.SessionPolicyWrite{Clipboard: strPtr(tc.ceiling)}, "", true); err != nil {
				t.Fatal(err)
			}
			got, err := svc.ResolveSessionPolicy(ctx, "tenant-a",
				provisioning.TemplateCatalogEntry{ClipboardPolicy: tc.template})
			if err != nil {
				t.Fatal(err)
			}
			if got.Clipboard != tc.want {
				t.Fatalf("template %s ∧ ceiling %s = %q, want %q",
					tc.template, tc.ceiling, got.Clipboard, tc.want)
			}
			// Re-tighten for the next case (each subtest rewrites the row).
			if _, err := sps.SetCeiling(ctx, "tenant-a", store.SessionPolicyWrite{},
				mustVersion(t, sps, "tenant-a"), false); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func mustVersion(t *testing.T, sps *store.SessionPolicyStore, tenantID string) string {
	t.Helper()
	c, err := sps.Ceiling(context.Background(), tenantID)
	if err != nil {
		t.Fatal(err)
	}
	return c.Version
}

// The resolved policy rides the lifecycle intents: the create intent
// carries the meet stamped at admission, and a start intent re-stamps it
// — so a ceiling tightened while a workspace was stopped lands on the
// pod's next boot via spec.sessionPolicy (ADR 0010 §3).
func TestService_SessionPolicyRidesIntents(t *testing.T) {
	db := recoveryDB(t)
	ctx := context.Background()
	const tenant = "tenant-a"
	if err := db.WithTx(ctx, func(tx store.Tx) error {
		return provisioning.SetQuota(ctx, tx, tenant, provisioning.ResourceVector{
			RunningSlots: 4, CPUMillis: 8000, MemoryBytes: 16 << 30, DiskBytes: 40 << 30})
	}); err != nil {
		t.Fatalf("set quota: %v", err)
	}
	svc := provisioning.NewService(db)
	sps := store.NewSessionPolicyStore(db)
	if _, err := sps.SetCeiling(ctx, tenant,
		store.SessionPolicyWrite{Clipboard: strPtr(provisioning.ClipboardSend)},
		"", true); err != nil {
		t.Fatalf("set ceiling: %v", err)
	}

	req := provisioning.CreateRequest{
		OwnerIssuer: "iss", OwnerSubject: "sub", Name: "sp-ws",
		Template: provisioning.TemplateInfo{
			ID: "tpl_x", Name: "x", Runtime: "linux", Experience: "desktop",
			ClipboardPolicy: provisioning.PolicyBidirectional,
		},
		Vector:       quotaVec,
		DesiredState: "Running",
		DataPolicy:   "Ephemeral",
	}
	res, err := svc.CreateWorkspace(ctx, tenant, "key-sp-1", req, provisioning.RequestHash(req))
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	pending := mustPending(t, db, res.ID)
	var create *provisioning.Intent
	for i := range pending {
		if pending[i].Kind == provisioning.IntentCreate {
			create = &pending[i]
		}
	}
	if create == nil || create.Spec.SessionPolicy == nil ||
		create.Spec.SessionPolicy.Clipboard != provisioning.ClipboardSend {
		t.Fatalf("create intent sessionPolicy = %+v, want Send (Bidirectional ∧ Send)", create)
	}

	// Tighten to Disabled while the workspace is stopped; the start
	// intent re-resolves and carries the new meet.
	if _, err := svc.SignalWorkspace(ctx, tenant, "iss|sub", "", res.ID,
		"key-sp-stop", provisioning.IntentStop, []byte("{}")); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if _, err := sps.SetCeiling(ctx, tenant,
		store.SessionPolicyWrite{Clipboard: strPtr(provisioning.PolicyDisabled)},
		mustVersion(t, sps, tenant), false); err != nil {
		t.Fatalf("tighten ceiling: %v", err)
	}
	if _, err := svc.SignalWorkspace(ctx, tenant, "iss|sub", "", res.ID,
		"key-sp-start", provisioning.IntentStart, []byte("{}")); err != nil {
		t.Fatalf("start: %v", err)
	}
	var start *provisioning.Intent
	for i, in := range mustPending(t, db, res.ID) {
		if in.Kind == provisioning.IntentStart {
			start = &mustPending(t, db, res.ID)[i]
		}
	}
	if start == nil || start.Spec.SessionPolicy == nil ||
		start.Spec.SessionPolicy.Clipboard != provisioning.PolicyDisabled {
		t.Fatalf("start intent sessionPolicy = %+v, want Disabled", start)
	}
}

func mustPending(t *testing.T, db *store.DB, wsID string) []provisioning.Intent {
	t.Helper()
	list, err := provisioning.NewOutbox(db).PendingIntents(context.Background(), provisioning.PlatformID(wsID))
	if err != nil {
		t.Fatal(err)
	}
	return list
}
