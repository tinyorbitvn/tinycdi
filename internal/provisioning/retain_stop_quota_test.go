// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package provisioning_test

// FX-R25 — the quota model: running slots, CPU and memory are held exactly
// while a runtime incarnation exists (from the start intent to the proven
// absence of the pod); disk is held while the volume exists, whatever the
// workspace state. These tests assert that rule for a Retain workspace.

import (
	"context"
	"testing"

	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
	"github.com/tinyorbitvn/tinycdi/internal/store"
)

const retainTenant = "tenant-r"

// retainEnv is a migrated database with one Retain workspace admitted
// Running with quotaVec, plus the helpers the tests drive it with.
type retainEnv struct {
	t   *testing.T
	db  *store.DB
	svc *provisioning.Service
	rec *provisioning.Recovery
	ws  string
}

func newRetainEnv(t *testing.T, policy string) *retainEnv {
	t.Helper()
	db := recoveryDB(t)
	ctx := context.Background()
	if err := db.WithTx(ctx, func(tx store.Tx) error {
		return provisioning.SetQuota(ctx, tx, retainTenant, provisioning.ResourceVector{
			RunningSlots: 2, CPUMillis: 2000, MemoryBytes: 4 << 30, DiskBytes: 16 << 30})
	}); err != nil {
		t.Fatalf("set quota: %v", err)
	}
	e := &retainEnv{t: t, db: db, svc: provisioning.NewService(db),
		rec: provisioning.NewRecovery(db, &fakeObserver{gone: true})}
	res, err := e.svc.CreateWorkspace(ctx, retainTenant, "", provisioning.CreateRequest{
		OwnerIssuer: "iss", OwnerSubject: "sub", Name: "ws-" + policy,
		Template: provisioning.TemplateInfo{ID: "t", Name: "tpl"},
		Vector:   quotaVec, DesiredState: "Running", DataPolicy: policy,
	}, []byte("b"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	e.ws = res.ID
	return e
}

func (e *retainEnv) signal(kind provisioning.IntentKind) {
	e.t.Helper()
	if _, err := e.svc.SignalWorkspace(context.Background(), retainTenant, "iss|sub", "", e.ws, "", kind, nil); err != nil {
		e.t.Fatalf("signal %s: %v", kind, err)
	}
}

func (e *retainEnv) recover() {
	e.t.Helper()
	if _, err := e.rec.Recover(context.Background(), noopApplier{}); err != nil {
		e.t.Fatalf("recover: %v", err)
	}
}

func (e *retainEnv) usage() provisioning.ResourceVector {
	e.t.Helper()
	u, err := provisioning.HeldUsage(context.Background(), e.db.Pool(), retainTenant)
	if err != nil {
		e.t.Fatalf("HeldUsage: %v", err)
	}
	return u
}

func (e *retainEnv) want(label string, v provisioning.ResourceVector) {
	e.t.Helper()
	if got := e.usage(); got != v {
		e.t.Fatalf("%s: held usage = %+v, want %+v", label, got, v)
	}
}

type noopApplier struct{}

func (noopApplier) Apply(context.Context, provisioning.Intent) error { return nil }

var diskOnly = provisioning.ResourceVector{DiskBytes: quotaVec.DiskBytes}

// A stopped Retain workspace releases compute and keeps a disk-only hold.
func TestRetainStop_ReleasesComputeKeepsDisk(t *testing.T) {
	e := newRetainEnv(t, "Retain")
	e.want("running", quotaVec)
	e.signal(provisioning.IntentStop)
	e.want("stopped, before the absence proof", quotaVec)
	e.recover()
	e.want("stopped, pod gone", diskOnly)
}

// Restart re-reserves exactly the admitted vector on the same reservation,
// and a second stop converts it again.
func TestRetainStop_RestartReReservesAdmittedVector(t *testing.T) {
	e := newRetainEnv(t, "Retain")
	for i := 0; i < 2; i++ {
		e.signal(provisioning.IntentStop)
		e.recover()
		e.want("stopped", diskOnly)
		e.signal(provisioning.IntentStart)
		e.want("restarted", quotaVec)
	}
}

// A restart that no longer fits the compute quota is refused and leaves the
// disk-only hold untouched.
func TestRetainStop_RestartRespectsComputeQuota(t *testing.T) {
	e := newRetainEnv(t, "Retain")
	e.signal(provisioning.IntentStop)
	e.recover()
	ctx := context.Background()
	if err := e.db.WithTx(ctx, func(tx store.Tx) error {
		return provisioning.SetQuota(ctx, tx, retainTenant, provisioning.ResourceVector{
			RunningSlots: 0, CPUMillis: 2000, MemoryBytes: 4 << 30, DiskBytes: 16 << 30})
	}); err != nil {
		t.Fatal(err)
	}
	_, err := e.svc.SignalWorkspace(ctx, retainTenant, "iss|sub", "", e.ws, "", provisioning.IntentStart, nil)
	if !provisioning.IsQuotaExceeded(err) {
		t.Fatalf("start over quota = %v, want quota exceeded", err)
	}
	e.want("after refused start", diskOnly)
}

// Delete after stop: the disk hold moves to the retained_data hold once —
// the admitted 4 GiB is replaced by the dataset's real 1 GiB, never added.
func TestRetainStop_DeleteAfterStopConvertsDiskOnce(t *testing.T) {
	e := newRetainEnv(t, "Retain")
	e.signal(provisioning.IntentStop)
	e.recover()
	e.want("stopped", diskOnly)

	e.signal(provisioning.IntentDelete)
	if _, err := provisioning.NewRetainedStore(e.db).ImportRetained(context.Background(), provisioning.RetainedDiskInfo{
		PVCNamespace: "ns", PVCName: "home", PVCUID: "pvc-1", TenantID: retainTenant, Owner: "iss|sub",
		SourceWorkspaceID: e.ws, SourceWorkspaceName: "n", Runtime: "LinuxContainer", SizeBytes: 1 << 30,
	}); err != nil {
		t.Fatalf("import: %v", err)
	}
	e.recover()
	e.want("deleted, dataset retained", provisioning.ResourceVector{DiskBytes: 1 << 30})
	e.recover() // idempotent: the disk hold is not released or doubled
	e.want("second pass", provisioning.ResourceVector{DiskBytes: 1 << 30})
}

// A recovery pass on an already-converted row changes nothing.
func TestRetainStop_RecoveryIdempotentOnConvertedRow(t *testing.T) {
	e := newRetainEnv(t, "Retain")
	e.signal(provisioning.IntentStop)
	e.recover()
	pending, err := e.rec.PendingRecovery(context.Background())
	if err != nil || len(pending) != 0 {
		t.Fatalf("PendingRecovery after conversion = %v err=%v, want none", pending, err)
	}
	e.recover()
	e.recover()
	e.want("three passes", diskOnly)
	e.signal(provisioning.IntentStart)
	e.want("restart after repeated passes", quotaVec)
}

// Ephemeral is unchanged: the whole reservation is released, and a restart
// re-acquires it from the kept vector.
func TestRetainStop_EphemeralUnchanged(t *testing.T) {
	e := newRetainEnv(t, "Ephemeral")
	e.signal(provisioning.IntentStop)
	e.recover()
	e.want("ephemeral stopped", provisioning.ResourceVector{})
	e.signal(provisioning.IntentStart)
	e.want("ephemeral restarted", quotaVec)
}

// A converted Retain workspace is never converted while it is Running: a
// start that lands between the proof and the write must keep its compute.
func TestRetainStop_StartedBeforeConversionKeepsCompute(t *testing.T) {
	e := newRetainEnv(t, "Retain")
	e.signal(provisioning.IntentStop)
	e.signal(provisioning.IntentStart)
	if err := e.rec.SettleQuota(context.Background(), retainTenant, provisioning.PlatformID(e.ws)); err != nil {
		t.Fatalf("SettleQuota on a running workspace: %v", err)
	}
	e.want("running again", quotaVec)
}
