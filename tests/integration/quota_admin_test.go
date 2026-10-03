//go:build integration

// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package integration

import (
	"context"
	"errors"
	"testing"

	"github.com/tinyorbitvn/tinycdi/internal/api"
	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
)

// TestAdminQuotaStore_IfMatch exercises the conditional write on a real
// Postgres: the version token comes from the row's xmin, so it changes on
// every committed update, and the check rides in the same statement as the
// write — a stale If-Match cannot race the update it tried to guard.
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
	v1 := rep.Version

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
}
