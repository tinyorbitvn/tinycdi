// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package provisioning_test

// QS-FLAKE — Reserve flags a refusal whose shortfall is covered only by
// reservations still held by deleted or stopped workspaces (release
// pending, QuotaExceededError.ReleasePending): transient, unlike a real
// over-limit. The flag drives the API's retryable release_pending signal.

import (
	"context"
	"errors"
	"testing"

	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
	"github.com/tinyorbitvn/tinycdi/internal/store"
)

// TestReserve_ReleasePending: with one slot of quota, a create over a
// deleted-but-unsettled workspace is refused as release-pending; the same
// refusal over a live workspace is a real exhaustion; after the absence
// proof settles, the create fits.
func TestReserve_ReleasePending(t *testing.T) {
	db := recoveryDB(t)
	ctx := context.Background()
	const tenant = "tenant-rp"
	if err := db.WithTx(ctx, func(tx store.Tx) error {
		return provisioning.SetQuota(ctx, tx, tenant, provisioning.ResourceVector{
			RunningSlots: 1, CPUMillis: 1000, MemoryBytes: 2 << 30, DiskBytes: 16 << 30})
	}); err != nil {
		t.Fatalf("set quota: %v", err)
	}
	svc := provisioning.NewService(db)
	create := func(name string, v provisioning.ResourceVector) error {
		_, err := svc.CreateWorkspace(ctx, tenant, "key-"+name, provisioning.CreateRequest{
			OwnerIssuer: "iss", OwnerSubject: "sub", Name: name,
			Template: provisioning.TemplateInfo{ID: "t", Name: "tpl"},
			Vector:   v, DesiredState: "Running", DataPolicy: "Ephemeral",
		}, []byte(name))
		return err
	}
	var q *provisioning.QuotaExceededError

	res, err := svc.CreateWorkspace(ctx, tenant, "key-ws-1", provisioning.CreateRequest{
		OwnerIssuer: "iss", OwnerSubject: "sub", Name: "ws-1",
		Template: provisioning.TemplateInfo{ID: "t", Name: "tpl"},
		Vector:   quotaVec, DesiredState: "Running", DataPolicy: "Ephemeral",
	}, []byte("ws-1"))
	if err != nil {
		t.Fatalf("create ws-1: %v", err)
	}

	// Quota full with a live workspace: a genuine refusal, nothing pending.
	err = create("ws-2", quotaVec)
	if !errors.As(err, &q) {
		t.Fatalf("over-limit create: want QuotaExceededError, got %v", err)
	}
	if q.ReleasePending || provisioning.IsReleasePending(err) {
		t.Fatal("no teardown-held quota: ReleasePending must be false")
	}

	// Delete ws-1: the API tombstones it but the reservation stays held
	// until the runtime absence is proven — the same create is now refused
	// as release-pending.
	if _, err := svc.SignalWorkspace(ctx, tenant, "iss|sub", "", res.ID, "",
		provisioning.IntentDelete, nil); err != nil {
		t.Fatalf("delete ws-1: %v", err)
	}
	err = create("ws-2", quotaVec)
	if !errors.As(err, &q) {
		t.Fatalf("create over pending release: want QuotaExceededError, got %v", err)
	}
	if !q.ReleasePending || !provisioning.IsReleasePending(err) {
		t.Fatal("delete-held reservation covers the request: ReleasePending must be true")
	}

	// A request that stays over even after the pending releases is still a
	// genuine exhaustion.
	err = create("ws-3", provisioning.ResourceVector{
		RunningSlots: 2, CPUMillis: 1000, MemoryBytes: 2 << 30, DiskBytes: 1 << 30})
	if !errors.As(err, &q) || q.ReleasePending {
		t.Fatalf("request beyond pending releases: want non-pending QuotaExceededError, got %v", err)
	}

	// Absence proven: the reservation releases and the create fits.
	rec := provisioning.NewRecovery(db, &fakeObserver{gone: true})
	if err := rec.SettleQuota(ctx, tenant, provisioning.PlatformID(res.ID)); err != nil {
		t.Fatalf("settle ws-1: %v", err)
	}
	if err := create("ws-2", quotaVec); err != nil {
		t.Fatalf("create after settle: %v", err)
	}
}
