//go:build integration

// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package integration

import (
	"context"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	crcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	workspacev1alpha1 "github.com/tinyorbitvn/tinycdi/api/v1alpha1"
	"github.com/tinyorbitvn/tinycdi/internal/broker"
	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
	linux "github.com/tinyorbitvn/tinycdi/internal/runtime/linux"
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

// fxr24Clock is the operator's settable clock for the full-chain test.
type fxr24Clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fxr24Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fxr24Clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// TestMaxDurationBackstopToRestart (FX-R24): the whole chain against real
// PostgreSQL, the production operator and the envtest apiserver — the
// operator's max-duration backstop stops a Running workspace, the row is
// brought in line by the expiry sweep, recovery releases the quota, Start
// then reaches Ready with the NEW incarnation's startedAt (the cap is
// measured from it, not from the first start).
func TestMaxDurationBackstopToRestart(t *testing.T) {
	ctx := context.Background()
	db := newDB(t)
	ns := newRetainedNamespace(t)
	tenant := "tenant-fxr24-chain"
	owner := "issuer|sub-fxr24"
	tmap := provisioning.TenantNamespaces{tenant: ns}
	setQuota(t, db, tenant, provisioning.ResourceVector{RunningSlots: 2, CPUMillis: 1 << 30, MemoryBytes: 1 << 40, DiskBytes: 1 << 40})
	svc := provisioning.NewService(db)
	rec := provisioning.NewRecovery(db, provisioning.NewK8sRuntimeObserver(k8sClient, tmap))
	applier := provisioning.NewK8sApplier(k8sClient, tmap)
	planner := broker.NewExpiryPlanner(broker.New(db, noBindings{}))
	clock := &fxr24Clock{t: time.Now().Truncate(time.Second)}

	const tplName = "fxr24-chain"
	fxr20Template(t, ns, tplName)
	startOperatorClock(t, ns, clock.Now)

	// Informer-backed running source, as the backend wires it.
	scheme := k8sClient.Scheme()
	kcache, err := crcache.New(testEnvConfig, crcache.Options{
		Scheme: scheme, DefaultNamespaces: map[string]crcache.Config{ns: {}}})
	if err != nil {
		t.Fatalf("cache: %v", err)
	}
	cctx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	go func() { _ = kcache.Start(cctx) }()
	if !kcache.WaitForCacheSync(cctx) {
		t.Fatal("cache did not sync")
	}
	src := broker.NewK8sRunningSource(kcache)

	req := provisioning.CreateRequest{
		OwnerIssuer: "issuer", OwnerSubject: "sub-fxr24", Name: "fxr24-chain",
		Template:     provisioning.TemplateInfo{ID: "tpl_linuxdesktop", Name: tplName, Revision: 1, Runtime: "LinuxContainer", Experience: "Desktop"},
		Vector:       provisioning.ResourceVector{RunningSlots: 1, CPUMillis: 500, MemoryBytes: 1 << 30, DiskBytes: 1 << 30},
		DesiredState: "Running", DataPolicy: "Ephemeral",
	}
	res, err := svc.CreateWorkspace(ctx, tenant, "create-fxr24-chain", req, provisioning.RequestHash(req))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	recoverPass := func() {
		t.Helper()
		if _, err := rec.Recover(ctx, applier); err != nil {
			t.Fatalf("recover: %v", err)
		}
	}
	crOf := func() *workspacev1alpha1.Workspace { return workspaceCRByUID(t, res.ID) }
	ready := func(what string, startedAfter time.Time) *workspacev1alpha1.Workspace {
		t.Helper()
		eventually(t, what, 60*time.Second, func() bool {
			cr := crOf()
			if cr == nil {
				return false
			}
			pod := &corev1.Pod{}
			if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: linux.PodName(cr.UID)}, pod); err != nil {
				return false
			}
			if pod.Status.Phase != corev1.PodRunning {
				pod.Status.Phase = corev1.PodRunning
				pod.Status.Conditions = []corev1.PodCondition{
					{Type: corev1.PodScheduled, Status: corev1.ConditionTrue},
					{Type: corev1.PodReady, Status: corev1.ConditionTrue},
				}
				_ = k8sClient.Status().Update(ctx, pod)
			}
			return cr.Status.Phase == workspacev1alpha1.WorkspacePhaseReady &&
				cr.Status.StartedAt != nil && !cr.Status.StartedAt.Time.Before(startedAfter)
		})
		return crOf()
	}

	// 1. Create, deliver, Ready: startedAt = the first start.
	recoverPass()
	first := ready("first Ready", clock.Now())
	firstStart := first.Status.StartedAt.Time
	if !firstStart.Equal(clock.Now()) {
		t.Fatalf("first startedAt=%v want %v", firstStart, clock.Now())
	}

	// 2. The operator's backstop stops it at 8 h; the DB row never hears.
	clock.Advance(8*time.Hour + time.Minute)
	eventually(t, "operator backstop stop", 60*time.Second, func() bool {
		cr := crOf()
		if cr.Annotations == nil {
			return false
		}
		if cr.Status.Phase != workspacev1alpha1.WorkspacePhaseReady || cr.Status.StartedAt == nil {
			return cr.Status.Phase == workspacev1alpha1.WorkspacePhaseStopped
		}
		// Poke a reconcile: the cap's requeue is 8 real hours away.
		cr.Annotations["fxr24/poke"] = clock.Now().String()
		_ = k8sClient.Update(ctx, cr)
		return false
	})
	if got, err := svc.GetWorkspace(ctx, tenant, owner, res.ID); err != nil || got.DesiredState != "Running" {
		t.Fatalf("row after backstop: %+v err=%v; the DB row must still say Running (the defect)", got, err)
	}
	if held := heldSlots(t, db, tenant); held != 1 {
		t.Fatalf("held=%d after backstop, want 1 (the defect)", held)
	}

	// 3. The expiry sweep brings the row in line.
	eventually(t, "sweep flips the row", 30*time.Second, func() bool {
		n, err := planner.Sweep(ctx, src)
		return err == nil && n == 1
	})
	if got, err := svc.GetWorkspace(ctx, tenant, owner, res.ID); err != nil || got.DesiredState != "Stopped" {
		t.Fatalf("row after sweep: %+v err=%v; want desired Stopped", got, err)
	}
	hist, err := svc.IntentHistory(ctx, tenant, provisioning.PlatformID(res.ID))
	if err != nil || len(hist) == 0 || hist[0].Kind != provisioning.IntentStop || hist[0].Reason != "max_duration" {
		t.Fatalf("intent history = %+v err=%v; want the newest intent to be a stop with reason max_duration", hist, err)
	}

	// 4. Recovery releases the quota on the runtime-absence proof.
	eventually(t, "quota released", 30*time.Second, func() bool {
		recoverPass()
		return heldSlots(t, db, tenant) == 0
	})

	// 5. Start works and the new incarnation gets a fresh startedAt.
	st, err := svc.SignalWorkspace(ctx, tenant, owner, owner, res.ID, "start-chain", provisioning.IntentStart, startHash)
	if err != nil || st.DesiredState != "Running" {
		t.Fatalf("start: %+v err=%v", st, err)
	}
	recoverPass()
	restarted := ready("Ready after restart", clock.Now())
	if got := restarted.Status.StartedAt.Time; !got.Equal(clock.Now()) || !got.After(firstStart) {
		t.Fatalf("restarted startedAt=%v want the new start %v (first start %v)", got, clock.Now(), firstStart)
	}
	if restarted.Spec.DesiredState != workspacev1alpha1.DesiredStateRunning || restarted.Status.Phase != workspacev1alpha1.WorkspacePhaseReady {
		t.Fatalf("restarted CR = %s/%s, want Running/Ready", restarted.Spec.DesiredState, restarted.Status.Phase)
	}
	if held := heldSlots(t, db, tenant); held != 1 {
		t.Fatalf("held=%d after restart, want 1", held)
	}
}
