//go:build integration

// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package integration

import (
	"context"
	"testing"

	"github.com/tinyorbitvn/tinycdi/internal/broker"
	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
)

// noBindings is a BindingSource with no runtime bindings: the sweep under
// test only reconciles operator stops and never needs one.
type noBindings struct{}

func (noBindings) CurrentBinding(context.Context, broker.PlatformID) (broker.RuntimeBinding, error) {
	return broker.RuntimeBinding{}, broker.ErrNotFound
}

// operatorStops is a RunningSource reporting a fixed set of operator stops.
type operatorStops []broker.OperatorStopped

func (operatorStops) RunningWorkspaces(context.Context) ([]broker.RunningWorkspace, error) {
	return nil, nil
}

func (o operatorStops) OperatorStoppedWorkspaces(context.Context) ([]broker.OperatorStopped, error) {
	return o, nil
}

// TestOperatorStoppedRowSelfHeals (FX-R24): a start the operator's
// max-duration backstop stopped on its own leaves the workspaces row
// Running/Provisioning with the quota held, and Start is a no-op on it.
// The expiry sweep brings the row in line with the CR with no click, the
// recovery pass releases the quota on the runtime-absence proof, and Start
// then works. A stale view of the CR never overrides a newer start.
func TestOperatorStoppedRowSelfHeals(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	svc := provisioning.NewService(db)
	rec := provisioning.NewRecovery(db, goneObserver{})
	planner := broker.NewExpiryPlanner(broker.New(db, noBindings{}))
	tenant := "tenant-fxr24"
	owner := "issuer|sub-fxr24"
	setQuota(t, db, tenant, provisioning.ResourceVector{RunningSlots: 2, CPUMillis: 1 << 30, MemoryBytes: 1 << 40, DiskBytes: 1 << 40})

	req := provisioning.CreateRequest{
		OwnerIssuer: "issuer", OwnerSubject: "sub-fxr24", Name: "fxr24",
		Template:     provisioning.TemplateInfo{ID: "tpl_linuxdesktop", Name: "linux", Revision: 1, Runtime: "LinuxContainer", Experience: "Desktop"},
		Vector:       provisioning.ResourceVector{RunningSlots: 1, CPUMillis: 2000, MemoryBytes: 4 << 30, DiskBytes: 1 << 30},
		DesiredState: "Running", DataPolicy: "Retain",
	}
	res, err := svc.CreateWorkspace(ctx, tenant, "create-fxr24", req, provisioning.RequestHash(req))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	markDispatched(t, db, res.ID)

	var gen, rev int64
	if err := db.Pool().QueryRow(ctx,
		`SELECT runtime_generation, intent_revision FROM workspaces WHERE id=$1`, res.ID).Scan(&gen, &rev); err != nil {
		t.Fatal(err)
	}
	// What the CR looks like once the operator stopped this intent itself.
	stale := operatorStops{{WorkspaceUID: broker.PlatformID(res.ID), RuntimeGeneration: uint64(gen), IntentRevision: uint64(rev)}}

	// Before: the row is stuck Running with the quota held, and Start does nothing.
	if held := heldSlots(t, db, tenant); held != 1 {
		t.Fatalf("held=%d want 1", held)
	}
	st, err := svc.SignalWorkspace(ctx, tenant, owner, owner, res.ID, "start-stuck", provisioning.IntentStart, startHash)
	if err != nil || st.DesiredState != "Running" {
		t.Fatalf("start on the stuck row: %+v err=%v", st, err)
	}
	var intents int
	if err := db.Pool().QueryRow(ctx, `SELECT count(*) FROM outbox_intent WHERE workspace_id=$1`, res.ID).Scan(&intents); err != nil || intents != 1 {
		t.Fatalf("intents=%d err=%v; Start on a Running row must be a no-op (the reason the row needs the sweep)", intents, err)
	}

	// The sweep flips the row; no click.
	n, err := planner.Sweep(ctx, stale)
	if err != nil || n != 1 {
		t.Fatalf("Sweep = %d, %v; want 1", n, err)
	}
	got, err := svc.GetWorkspace(ctx, tenant, owner, res.ID)
	if err != nil || got.DesiredState != "Stopped" {
		t.Fatalf("row after sweep: %+v err=%v; want desired Stopped", got, err)
	}

	// Recovery redelivers the stop and releases the quota on the absence proof.
	if _, err := rec.Recover(ctx, noopApplier{}); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if held := heldSlots(t, db, tenant); held != 0 {
		t.Fatalf("held=%d after recovery, want 0 (quota released)", held)
	}

	// Start works now: new generation, quota re-held, a start intent recorded.
	st, err = svc.SignalWorkspace(ctx, tenant, owner, owner, res.ID, "start-after-heal", provisioning.IntentStart, startHash)
	if err != nil || st.DesiredState != "Running" || st.Phase != "Provisioning" {
		t.Fatalf("start after heal: %+v err=%v", st, err)
	}
	if held := heldSlots(t, db, tenant); held != 1 {
		t.Fatalf("held=%d after start, want 1", held)
	}

	// Race: the sweep still holds the OLD view of the CR (previous
	// generation/revision). The start that just landed must win.
	n, err = planner.Sweep(ctx, stale)
	if err != nil || n != 0 {
		t.Fatalf("stale Sweep = %d, %v; want 0 (a newer start wins)", n, err)
	}
	got, err = svc.GetWorkspace(ctx, tenant, owner, res.ID)
	if err != nil || got.DesiredState != "Running" {
		t.Fatalf("row after stale sweep: %+v err=%v; the start must stand", got, err)
	}
	if held := heldSlots(t, db, tenant); held != 1 {
		t.Fatalf("held=%d after stale sweep, want 1", held)
	}
}
