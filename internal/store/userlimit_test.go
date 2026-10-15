// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/tinyorbitvn/tinycdi/internal/store"
)

// Per-principal limit writes are conditional on the row's version: create
// only inserts a missing row, an update or delete matches only the version
// it names, and every miss is ErrVersionMismatch.

func TestUserLimitStore_OverrideConditionalWrites(t *testing.T) {
	db := newDB(t)
	s := store.NewUserLimitStore(db)
	q := store.NewQuotaReader(db)
	ctx := context.Background()
	const owner = "https://idp.test|alice"

	if _, err := s.SetOverride(ctx, "tenant-a", owner, 2, "2026-10-13T10:00:00Z", false); !errors.Is(err, store.ErrVersionMismatch) {
		t.Fatalf("update of a missing row: %v", err)
	}
	v1, err := s.SetOverride(ctx, "tenant-a", owner, 2, "", true)
	if err != nil || v1 == "" {
		t.Fatalf("create: version=%q err=%v", v1, err)
	}
	rep, err := q.Report(ctx, "tenant-a")
	if err != nil {
		t.Fatal(err)
	}
	if rep.UserLimits[owner] != 2 || rep.UserLimitVersions[owner] != v1 {
		t.Fatalf("report after create: limits=%v versions=%v, want 2 @ %q", rep.UserLimits, rep.UserLimitVersions, v1)
	}
	if _, err := s.SetOverride(ctx, "tenant-a", owner, 3, "", true); !errors.Is(err, store.ErrVersionMismatch) {
		t.Fatalf("create on an existing row: %v", err)
	}
	if _, err := s.SetOverride(ctx, "tenant-a", owner, 3, "not-a-version", false); !errors.Is(err, store.ErrVersionMismatch) {
		t.Fatalf("malformed version: %v", err)
	}
	v2, err := s.SetOverride(ctx, "tenant-a", owner, 3, v1, false)
	if err != nil || v2 == v1 {
		t.Fatalf("update: version=%q (was %q) err=%v", v2, v1, err)
	}
	if _, err := s.ClearOverride(ctx, "tenant-a", owner, v1, false); !errors.Is(err, store.ErrVersionMismatch) {
		t.Fatalf("stale delete: %v", err)
	}
	if _, err := s.ClearOverride(ctx, "tenant-a", owner, v2, false); err != nil {
		t.Fatalf("delete: %v", err)
	}
	rep, err = q.Report(ctx, "tenant-a")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := rep.UserLimits[owner]; ok {
		t.Fatalf("override survived its delete: %v", rep.UserLimits)
	}
}

func TestUserLimitStore_DefaultConditionalWrites(t *testing.T) {
	db := newDB(t)
	s := store.NewUserLimitStore(db)
	q := store.NewQuotaReader(db)
	ctx := context.Background()

	// Clearing a missing default under create is a no-op.
	if _, err := s.ClearDefault(ctx, "tenant-a", "", true); err != nil {
		t.Fatalf("clear of a missing default: %v", err)
	}
	v1, err := s.SetDefault(ctx, "tenant-a", 4, "", true)
	if err != nil || v1 == "" {
		t.Fatalf("create: version=%q err=%v", v1, err)
	}
	rep, err := q.Report(ctx, "tenant-a")
	if err != nil {
		t.Fatal(err)
	}
	if rep.DefaultUserLimit == nil || *rep.DefaultUserLimit != 4 || rep.DefaultUserLimitVersion != v1 {
		t.Fatalf("report: default=%v version=%q, want 4 @ %q", rep.DefaultUserLimit, rep.DefaultUserLimitVersion, v1)
	}
	if _, err := s.SetDefault(ctx, "tenant-a", 5, "", true); !errors.Is(err, store.ErrVersionMismatch) {
		t.Fatalf("create on an existing row: %v", err)
	}
	if _, err := s.ClearDefault(ctx, "tenant-a", "", true); !errors.Is(err, store.ErrVersionMismatch) {
		t.Fatalf("create-clear on an existing row: %v", err)
	}
	v2, err := s.SetDefault(ctx, "tenant-a", 5, v1, false)
	if err != nil || v2 == v1 {
		t.Fatalf("update: version=%q err=%v", v2, err)
	}
	if _, err := s.ClearDefault(ctx, "tenant-a", v1, false); !errors.Is(err, store.ErrVersionMismatch) {
		t.Fatalf("stale clear: %v", err)
	}
	if _, err := s.ClearDefault(ctx, "tenant-a", v2, false); err != nil {
		t.Fatalf("clear: %v", err)
	}
	rep, err = q.Report(ctx, "tenant-a")
	if err != nil {
		t.Fatal(err)
	}
	if rep.DefaultUserLimit != nil || rep.DefaultUserLimitVersion != "" {
		t.Fatalf("default survived its clear: %v %q", rep.DefaultUserLimit, rep.DefaultUserLimitVersion)
	}
}
