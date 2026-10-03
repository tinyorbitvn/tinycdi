//go:build integration

// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package integration

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/api"
	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
	"github.com/tinyorbitvn/tinycdi/internal/store"
)

// TestAdminQuotaStore_IfMatch exercises the conditional write on a real
// Postgres: the version token is the row's updated_at (RFC3339Nano), so it
// changes on every committed update, and the check rides in the same
// statement as the write — a stale If-Match cannot race the update it
// tried to guard. The declarative upsert (-tenant-quotas) bumps the same
// token when it changes the row.
func TestAdminQuotaStore_IfMatch(t *testing.T) {
	db := newDB(t)
	src := api.NewAdminQuotaSource(db)
	ctx := context.Background()
	limits := provisioning.ResourceVector{
		RunningSlots: 4, CPUMillis: 8000, MemoryBytes: 8 << 30, DiskBytes: 100 << 30,
	}

	// No row: no version; only "*" creates.
	rep, err := src.Report(ctx, "tenant-a")
	if err != nil || rep.HasLimits || rep.Version != "" {
		t.Fatalf("empty report = %+v err=%v, want no row and no version", rep, err)
	}
	if err := src.SetLimits(ctx, "tenant-a", limits, "12"); !errors.Is(err, api.ErrQuotaVersionMismatch) {
		t.Fatalf("concrete version on missing row: %v, want mismatch", err)
	}
	if err := src.SetLimits(ctx, "tenant-a", limits, api.IfMatchCreate); err != nil {
		t.Fatalf("create with If-Match *: %v", err)
	}
	rep, err = src.Report(ctx, "tenant-a")
	if err != nil || !rep.HasLimits || rep.Version == "" {
		t.Fatalf("post-create report = %+v err=%v", rep, err)
	}
	// The token is the row's updated_at rendered RFC3339Nano — opaque to
	// clients, but it must round-trip through timestamptz equality.
	v1 := rep.Version
	if _, err := time.Parse(time.RFC3339Nano, v1); err != nil {
		t.Fatalf("version %q is not RFC3339Nano: %v", v1, err)
	}

	// "*" does not bypass the check on an existing row.
	if err := src.SetLimits(ctx, "tenant-a", limits, api.IfMatchCreate); !errors.Is(err, api.ErrQuotaVersionMismatch) {
		t.Fatalf("If-Match * on existing row: %v, want mismatch", err)
	}
	// A stale or invented token is refused; the row keeps v1's limits.
	if err := src.SetLimits(ctx, "tenant-a", limits, "999999"); !errors.Is(err, api.ErrQuotaVersionMismatch) {
		t.Fatalf("stale version: %v, want mismatch", err)
	}
	rep, _ = src.Report(ctx, "tenant-a")
	if rep.Version != v1 || rep.Limits.RunningSlots != 4 {
		t.Fatalf("refused write changed the row: version=%q limits=%+v", rep.Version, rep.Limits)
	}

	// The current token writes; the response version moves on.
	lower := provisioning.ResourceVector{
		RunningSlots: 2, CPUMillis: 4000, MemoryBytes: 4 << 30, DiskBytes: 50 << 30,
	}
	if err := src.SetLimits(ctx, "tenant-a", lower, v1); err != nil {
		t.Fatalf("matching If-Match: %v", err)
	}
	rep, _ = src.Report(ctx, "tenant-a")
	if rep.Version == v1 {
		t.Fatalf("version did not change after write: %q", rep.Version)
	}
	if rep.Limits.RunningSlots != 2 {
		t.Fatalf("limits after write = %+v", rep.Limits)
	}
	// The old token is dead.
	if err := src.SetLimits(ctx, "tenant-a", limits, v1); !errors.Is(err, api.ErrQuotaVersionMismatch) {
		t.Fatalf("replayed version: %v, want mismatch", err)
	}

	// The declarative path moves the same token: UpsertQuota with different
	// limits bumps the version, with identical limits it does not — an
	// idempotent re-apply must not churn If-Match tokens.
	v2 := rep.Version
	if err := db.WithTx(ctx, func(tx store.Tx) error {
		_, err := provisioning.UpsertQuota(ctx, tx, "tenant-a", limits)
		return err
	}); err != nil {
		t.Fatalf("declarative upsert: %v", err)
	}
	rep, _ = src.Report(ctx, "tenant-a")
	if rep.Version == v2 {
		t.Fatalf("declarative upsert did not bump the version: %q", rep.Version)
	}
	if _, err := time.Parse(time.RFC3339Nano, rep.Version); err != nil {
		t.Fatalf("version %q is not RFC3339Nano: %v", rep.Version, err)
	}
	if err := db.WithTx(ctx, func(tx store.Tx) error {
		_, err := provisioning.UpsertQuota(ctx, tx, "tenant-a", limits)
		return err
	}); err != nil {
		t.Fatalf("idempotent declarative upsert: %v", err)
	}
	rep2, _ := src.Report(ctx, "tenant-a")
	if rep2.Version != rep.Version {
		t.Fatalf("no-op upsert bumped the version: %q -> %q", rep.Version, rep2.Version)
	}
}
