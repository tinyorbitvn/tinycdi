// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package provisioning_test

// USER-LIMITS — per-principal running-workspace limits (v0.5): the tenant
// default row plus per-owner overrides, enforced inside the reservation
// transaction under the same tenant_quota row lock that serializes tenant
// quota — concurrent launches by one user can never overshoot.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
	"github.com/tinyorbitvn/tinycdi/internal/store"
)

// ulSetDefault/ulSetOverride write a limit row; nil clears it. They go
// through the real Postgres store so the tests cover the write path too.
func ulSetDefault(t *testing.T, db *store.DB, tenant string, n *int64) {
	t.Helper()
	s := store.NewUserLimitStore(db)
	var err error
	if n == nil {
		err = s.ClearDefault(context.Background(), tenant)
	} else {
		err = s.SetDefault(context.Background(), tenant, *n)
	}
	if err != nil {
		t.Fatalf("set default user limit: %v", err)
	}
}

func ulSetOverride(t *testing.T, db *store.DB, tenant, owner string, n *int64) {
	t.Helper()
	s := store.NewUserLimitStore(db)
	var err error
	if n == nil {
		err = s.ClearOverride(context.Background(), tenant, owner)
	} else {
		err = s.SetOverride(context.Background(), tenant, owner, *n)
	}
	if err != nil {
		t.Fatalf("set user limit override: %v", err)
	}
}

func i64(v int64) *int64 { return &v }

// ulEnv seeds a generous tenant quota (per-user limits must be the only
// bound) and returns a create closure for one owner.
func ulEnv(t *testing.T, tenant string) (*store.DB, *provisioning.Service, func(owner, name string) error) {
	t.Helper()
	db := recoveryDB(t)
	ctx := context.Background()
	if err := db.WithTx(ctx, func(tx store.Tx) error {
		return provisioning.SetQuota(ctx, tx, tenant, provisioning.ResourceVector{
			RunningSlots: 64, CPUMillis: 64000, MemoryBytes: 128 << 30, DiskBytes: 4 << 40})
	}); err != nil {
		t.Fatalf("set quota: %v", err)
	}
	svc := provisioning.NewService(db)
	create := func(owner, name string) error {
		iss, sub := "iss", owner
		_, err := svc.CreateWorkspace(ctx, tenant, "key-"+owner+"-"+name, provisioning.CreateRequest{
			OwnerIssuer: iss, OwnerSubject: sub, Name: name,
			Template: provisioning.TemplateInfo{ID: "t", Name: "tpl"},
			Vector:   quotaVec, DesiredState: "Running", DataPolicy: "Ephemeral",
		}, []byte(owner+name))
		return err
	}
	return db, svc, create
}

// TestUserLimit_DefaultEnforced: the tenant default caps each principal at
// its running count; other principals are unaffected, and clearing the
// default restores unlimited — the upgrade default.
func TestUserLimit_DefaultEnforced(t *testing.T) {
	db, _, create := ulEnv(t, "tenant-ul1")
	const owner = "iss|sub"

	ulSetDefault(t, db, "tenant-ul1", i64(1))

	if err := create("sub", "ws-1"); err != nil {
		t.Fatalf("first create: %v", err)
	}
	var u *provisioning.UserLimitError
	err := create("sub", "ws-2")
	if !errors.As(err, &u) {
		t.Fatalf("second create: want UserLimitError, got %v", err)
	}
	if u.Limit != 1 || u.Current != 1 || u.ReleasePending {
		t.Fatalf("refusal = %+v, want limit=1 current=1 not pending", u)
	}

	// A different owner is a different principal: unaffected.
	if err := create("other", "ws-3"); err != nil {
		t.Fatalf("other owner's create: %v", err)
	}

	// Clearing the default restores unlimited.
	ulSetDefault(t, db, "tenant-ul1", nil)
	if err := create("sub", "ws-4"); err != nil {
		t.Fatalf("create after clearing default: %v", err)
	}
}

// TestUserLimit_OverrideBeatsDefault: a stored override wins over the
// tenant default in both directions — raising above and lowering below.
func TestUserLimit_OverrideBeatsDefault(t *testing.T) {
	db, _, create := ulEnv(t, "tenant-ul2")

	ulSetDefault(t, db, "tenant-ul2", i64(1))
	ulSetOverride(t, db, "tenant-ul2", "iss|sub", i64(2))

	// sub runs to the override (2), while other stays on the default (1).
	if err := create("sub", "ws-1"); err != nil {
		t.Fatalf("create 1: %v", err)
	}
	if err := create("sub", "ws-2"); err != nil {
		t.Fatalf("create 2 under override: %v", err)
	}
	var u *provisioning.UserLimitError
	if err := create("sub", "ws-3"); !errors.As(err, &u) || u.Limit != 2 {
		t.Fatalf("third create: want UserLimitError limit=2, got %v", err)
	}
	if err := create("other", "ws-4"); err != nil {
		t.Fatalf("other's first create: %v", err)
	}
	if err := create("other", "ws-5"); !errors.As(err, &u) || u.Limit != 1 {
		t.Fatalf("other's second create: want UserLimitError limit=1, got %v", err)
	}

	// Clearing the override drops sub back to the default — and a limit
	// below current usage refuses new work without touching the held ones.
	ulSetOverride(t, db, "tenant-ul2", "iss|sub", nil)
	if err := create("sub", "ws-6"); !errors.As(err, &u) || u.Limit != 1 {
		t.Fatalf("create after clearing override: want UserLimitError limit=1, got %v", err)
	}
}

// TestUserLimit_ConcurrentLaunches: N parallel creates by one owner with a
// limit of L must never produce more than L running workspaces — the check
// serializes on the tenant_quota row lock inside each transaction.
func TestUserLimit_ConcurrentLaunches(t *testing.T) {
	db, _, create := ulEnv(t, "tenant-ul3")
	const n, limit = 12, 3

	ulSetDefault(t, db, "tenant-ul3", i64(limit))

	var wg sync.WaitGroup
	var okCount, refuseCount, otherErr atomic.Int64
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			err := create("sub", fmt.Sprintf("ws-%02d", i))
			switch {
			case err == nil:
				okCount.Add(1)
			case provisioning.IsUserLimit(err):
				refuseCount.Add(1)
			default:
				otherErr.Add(1)
			}
		}(i)
	}
	wg.Wait()

	if otherErr.Load() != 0 {
		t.Fatalf("%d unexpected errors", otherErr.Load())
	}
	if okCount.Load() != limit {
		t.Fatalf("admitted %d, want exactly %d", okCount.Load(), limit)
	}
	if refuseCount.Load() != n-limit {
		t.Fatalf("refused %d, want %d", refuseCount.Load(), n-limit)
	}
	used, err := provisioning.HeldUsage(context.Background(), db.Pool(), "tenant-ul3")
	if err != nil {
		t.Fatalf("held usage: %v", err)
	}
	if used.RunningSlots != limit {
		t.Fatalf("held running slots = %d, want %d", used.RunningSlots, limit)
	}
}

// TestUserLimit_ReleasePending: a refusal covered only by the owner's own
// teardown-pending holds flags ReleasePending — transient, same signal as
// the tenant-level variant; a refusal over live workspaces does not.
func TestUserLimit_ReleasePending(t *testing.T) {
	db, svc, _ := ulEnv(t, "tenant-ul4")
	ctx := context.Background()

	ulSetDefault(t, db, "tenant-ul4", i64(1))

	res, err := svc.CreateWorkspace(ctx, "tenant-ul4", "key-a-1", provisioning.CreateRequest{
		OwnerIssuer: "iss", OwnerSubject: "sub", Name: "a-1",
		Template: provisioning.TemplateInfo{ID: "t", Name: "tpl"},
		Vector:   quotaVec, DesiredState: "Running", DataPolicy: "Ephemeral",
	}, []byte("a-1"))
	if err != nil {
		t.Fatalf("create a-1: %v", err)
	}

	// Live workspace fills the slot: a genuine refusal.
	create2 := func() error {
		_, err := svc.CreateWorkspace(ctx, "tenant-ul4", "key-a-2", provisioning.CreateRequest{
			OwnerIssuer: "iss", OwnerSubject: "sub", Name: "a-2",
			Template: provisioning.TemplateInfo{ID: "t", Name: "tpl"},
			Vector:   quotaVec, DesiredState: "Running", DataPolicy: "Ephemeral",
		}, []byte("a-2"))
		return err
	}
	var u *provisioning.UserLimitError
	if err := create2(); !errors.As(err, &u) || u.ReleasePending {
		t.Fatalf("over live workspace: want non-pending UserLimitError, got %v", err)
	}

	// Deleted but not yet proven gone: still counted, transiently.
	if _, err := svc.SignalWorkspace(ctx, "tenant-ul4", "iss|sub", "", res.ID, "",
		provisioning.IntentDelete, nil); err != nil {
		t.Fatalf("delete a-1: %v", err)
	}
	if err := create2(); !errors.As(err, &u) || !u.ReleasePending {
		t.Fatalf("over pending release: want ReleasePending UserLimitError, got %v", err)
	}

	// Absence proven: the slot frees and the create fits.
	rec := provisioning.NewRecovery(db, &fakeObserver{gone: true})
	if err := rec.SettleQuota(ctx, "tenant-ul4", provisioning.PlatformID(res.ID)); err != nil {
		t.Fatalf("settle a-1: %v", err)
	}
	if err := create2(); err != nil {
		t.Fatalf("create after settle: %v", err)
	}
}

// TestUserLimit_RetainDiskOnlyHold: a stopped Retain workspace's disk-only
// hold does not occupy the running slot; its start re-acquires and is
// refused when the owner is again at the limit.
func TestUserLimit_RetainDiskOnlyHold(t *testing.T) {
	db, svc, _ := ulEnv(t, "tenant-ul5")
	ctx := context.Background()

	ulSetDefault(t, db, "tenant-ul5", i64(1))

	res, err := svc.CreateWorkspace(ctx, "tenant-ul5", "key-r-1", provisioning.CreateRequest{
		OwnerIssuer: "iss", OwnerSubject: "sub", Name: "r-1",
		Template: provisioning.TemplateInfo{ID: "t", Name: "tpl"},
		Vector:   quotaVec, DesiredState: "Running", DataPolicy: "Retain",
	}, []byte("r-1"))
	if err != nil {
		t.Fatalf("create r-1: %v", err)
	}
	if _, err := svc.SignalWorkspace(ctx, "tenant-ul5", "iss|sub", "", res.ID, "",
		provisioning.IntentStop, nil); err != nil {
		t.Fatalf("stop r-1: %v", err)
	}
	rec := provisioning.NewRecovery(db, &fakeObserver{gone: true})
	if err := rec.SettleQuota(ctx, "tenant-ul5", provisioning.PlatformID(res.ID)); err != nil {
		t.Fatalf("settle r-1: %v", err)
	}
	// The reservation is now a disk-only hold: the running slot is free.
	var held int64
	if err := db.Pool().QueryRow(ctx, `
		SELECT running_slots FROM quota_reservation WHERE workspace_id = $1`, res.ID).Scan(&held); err != nil {
		t.Fatalf("read reservation: %v", err)
	}
	if held != 0 {
		t.Fatalf("stopped Retain holds %d running slots, want disk-only (0)", held)
	}
	if _, err := svc.CreateWorkspace(ctx, "tenant-ul5", "key-r-2", provisioning.CreateRequest{
		OwnerIssuer: "iss", OwnerSubject: "sub", Name: "r-2",
		Template: provisioning.TemplateInfo{ID: "t", Name: "tpl"},
		Vector:   quotaVec, DesiredState: "Running", DataPolicy: "Ephemeral",
	}, []byte("r-2")); err != nil {
		t.Fatalf("create r-2 over disk-only hold: %v", err)
	}

	// Starting r-1 re-acquires compute — refused: the slot is taken.
	var u *provisioning.UserLimitError
	if _, err := svc.SignalWorkspace(ctx, "tenant-ul5", "iss|sub", "", res.ID, "",
		provisioning.IntentStart, nil); !errors.As(err, &u) {
		t.Fatalf("start r-1 over limit: want UserLimitError, got %v", err)
	}
}

// TestUserLimit_NoRowsUnlimited: with no limit rows anywhere — the state
// an upgrade lands in — admission behaves exactly as before (tenant quota
// is the only bound).
func TestUserLimit_NoRowsUnlimited(t *testing.T) {
	_, _, create := ulEnv(t, "tenant-ul6")
	for i := 0; i < 4; i++ {
		if err := create("sub", fmt.Sprintf("ws-%d", i)); err != nil {
			t.Fatalf("create %d with no limit rows: %v", i, err)
		}
	}
}
