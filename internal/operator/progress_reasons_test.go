// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

// envtest coverage for the lifecycle step reasons the portal's progress UI
// reads (V3.27 PR-A). Conditions and their reason tokens are the whole
// contract: no schema or CRD change.
//
// Run: KUBEBUILDER_ASSETS=<repo>/bin/k8s/1.37.0-linux-amd64 \
//      go test ./internal/operator/ -run 'TestProgress' -v

package operator

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	workspacesv1alpha1 "github.com/tinyorbitvn/tinycdi/api/v1alpha1"
	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
	tcdiruntime "github.com/tinyorbitvn/tinycdi/internal/runtime"
	linux "github.com/tinyorbitvn/tinycdi/internal/runtime/linux"
)

// flakyBackend wraps the real Linux backend and fails Ensure/Stop on demand.
type flakyBackend struct {
	tcdiruntime.Backend
	mu        sync.Mutex
	ensureErr error
	stopErr   error
}

func (f *flakyBackend) set(ensure, stop error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureErr, f.stopErr = ensure, stop
}

func (f *flakyBackend) Ensure(ctx context.Context, ws *workspacesv1alpha1.Workspace, tpl *workspacesv1alpha1.WorkspaceTemplate) (tcdiruntime.Observation, error) {
	f.mu.Lock()
	err := f.ensureErr
	f.mu.Unlock()
	if err != nil {
		return tcdiruntime.Observation{}, err
	}
	return f.Backend.Ensure(ctx, ws, tpl)
}

func (f *flakyBackend) Stop(ctx context.Context, ws *workspacesv1alpha1.Workspace) error {
	f.mu.Lock()
	err := f.stopErr
	f.mu.Unlock()
	if err != nil {
		return err
	}
	return f.Backend.Stop(ctx, ws)
}

func reconcileErr(r *WorkspaceReconciler, key types.NamespacedName) error {
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	return err
}

// TestProgress_BackendError (G3): an error from the runtime backend used to
// leave status untouched, so the workspace looked frozen until the boot
// deadline. It now surfaces as RuntimeReady=False/BackendError with a fixed
// message (SEC-I6: never the raw error), and the error is still returned so
// controller-runtime retries with backoff.
func TestProgress_BackendError(t *testing.T) {
	env, c := startEnv(t)
	defer func() {
		if err := env.Stop(); err != nil {
			t.Logf("envtest stop: %v", err)
		}
	}()
	ns := newNamespace(t, c)
	newTemplate(t, c, ns, "tpl-be", nil)
	const secret = "exceeded quota: 10.1.2.3 node-secret"

	t.Run("ensure failure while starting", func(t *testing.T) {
		ws := newWorkspace(t, c, ns, "ws-be-start", "tpl-be", nil)
		key := types.NamespacedName{Name: ws.Name, Namespace: ns}
		be := &flakyBackend{Backend: linux.New(c, linux.Options{})}
		be.set(errors.New(secret), nil)
		r := &WorkspaceReconciler{Client: c, Scheme: testScheme, Backend: be}

		if err := reconcileErr(r, key); err == nil {
			t.Fatal("reconcile swallowed the backend error; controller-runtime must retry")
		}
		got := getWorkspace(t, c, key)
		rc := condition(got, workspacesv1alpha1.ConditionRuntimeReady)
		if rc == nil || rc.Status != metav1.ConditionFalse || rc.Reason != ReasonBackendError {
			t.Fatalf("RuntimeReady = %+v, want False/%s", rc, ReasonBackendError)
		}
		if strings.Contains(rc.Message, "10.1.2.3") || strings.Contains(rc.Message, "quota") {
			t.Fatalf("condition message leaks the raw error: %q", rc.Message)
		}
		if d := condition(got, workspacesv1alpha1.ConditionDegraded); d != nil && d.Status == metav1.ConditionTrue {
			t.Fatalf("a retrying backend error must not mark Degraded: %+v", d)
		}
		if got.Status.Phase == workspacesv1alpha1.WorkspacePhaseReady || got.Status.Phase == workspacesv1alpha1.WorkspacePhaseFailed {
			t.Fatalf("phase = %q; a backend error must not change the lifecycle phase", got.Status.Phase)
		}

		// Recovery clears it.
		be.set(nil, nil)
		reconcile(t, r, key)
		got = getWorkspace(t, c, key)
		if rc := condition(got, workspacesv1alpha1.ConditionRuntimeReady); rc == nil || rc.Reason == ReasonBackendError {
			t.Fatalf("RuntimeReady still %+v after the backend recovered", rc)
		}
	})

	t.Run("stop failure while stopping", func(t *testing.T) {
		ws := newWorkspace(t, c, ns, "ws-be-stop", "tpl-be", nil)
		key := types.NamespacedName{Name: ws.Name, Namespace: ns}
		be := &flakyBackend{Backend: linux.New(c, linux.Options{})}
		r := &WorkspaceReconciler{Client: c, Scheme: testScheme, Backend: be}
		reconcile(t, r, key)
		markPodReady(t, c, thePod(t, c, ns, ws.UID))
		reconcile(t, r, key)

		cur := getWorkspace(t, c, key)
		cur.Spec.DesiredState = workspacesv1alpha1.DesiredStateStopped
		cur.Spec.IntentRevision = 2
		if err := c.Update(context.Background(), cur); err != nil {
			t.Fatalf("stop intent: %v", err)
		}
		be.set(nil, errors.New(secret))
		if err := reconcileErr(r, key); err == nil {
			t.Fatal("stop failure swallowed")
		}
		got := getWorkspace(t, c, key)
		if rc := condition(got, workspacesv1alpha1.ConditionRuntimeReady); rc == nil ||
			rc.Status != metav1.ConditionFalse || rc.Reason != ReasonBackendError {
			t.Fatalf("RuntimeReady = %+v, want False/%s", rc, ReasonBackendError)
		}
	})

	t.Run("a ready workspace is not flipped by a transient error", func(t *testing.T) {
		ws := newWorkspace(t, c, ns, "ws-be-ready", "tpl-be", nil)
		key := types.NamespacedName{Name: ws.Name, Namespace: ns}
		be := &flakyBackend{Backend: linux.New(c, linux.Options{})}
		r := &WorkspaceReconciler{Client: c, Scheme: testScheme, Backend: be}
		reconcile(t, r, key)
		markPodReady(t, c, thePod(t, c, ns, ws.UID))
		reconcile(t, r, key)
		if p := getWorkspace(t, c, key).Status.Phase; p != workspacesv1alpha1.WorkspacePhaseReady {
			t.Fatalf("phase = %q, want Ready", p)
		}
		be.set(errors.New(secret), nil)
		if err := reconcileErr(r, key); err == nil {
			t.Fatal("error swallowed")
		}
		got := getWorkspace(t, c, key)
		if rc := condition(got, workspacesv1alpha1.ConditionRuntimeReady); rc == nil || rc.Status != metav1.ConditionTrue {
			t.Fatalf("RuntimeReady = %+v; a Ready workspace's runtime is still up", rc)
		}
	})

	t.Run("optimistic-lock conflicts are routine and write nothing", func(t *testing.T) {
		ws := newWorkspace(t, c, ns, "ws-be-conflict", "tpl-be", nil)
		key := types.NamespacedName{Name: ws.Name, Namespace: ns}
		be := &flakyBackend{Backend: linux.New(c, linux.Options{})}
		be.set(apierrors.NewConflict(schema.GroupResource{Resource: "pods"}, "x", errors.New("the object has been modified")), nil)
		r := &WorkspaceReconciler{Client: c, Scheme: testScheme, Backend: be}
		if err := reconcileErr(r, key); err == nil {
			t.Fatal("conflict swallowed")
		}
		got := getWorkspace(t, c, key)
		if rc := condition(got, workspacesv1alpha1.ConditionRuntimeReady); rc != nil && rc.Reason == ReasonBackendError {
			t.Fatalf("a conflict was reported as BackendError: %+v", rc)
		}
	})
}

// TestProgress_FailedKeepsStalledStep (G4): once a workspace is Failed the
// step conditions freeze, so the portal can still say which step stalled
// after the +5 min incarnation cleanup (when the pod is gone and the
// observation would otherwise read "Provisioning").
func TestProgress_FailedKeepsStalledStep(t *testing.T) {
	env, c := startEnv(t)
	defer func() {
		if err := env.Stop(); err != nil {
			t.Logf("envtest stop: %v", err)
		}
	}()
	ns := newNamespace(t, c)
	newTemplate(t, c, ns, "tpl-g4", func(tpl *workspacesv1alpha1.WorkspaceTemplate) {
		tpl.Spec.BootDeadline = metav1.Duration{Duration: 300 * time.Millisecond}
	})
	ws := newWorkspace(t, c, ns, "ws-g4", "tpl-g4", nil)
	key := types.NamespacedName{Name: ws.Name, Namespace: ns}
	now := time.Now()
	r := newReconciler(c)
	r.Now = func() time.Time { return now }

	reconcile(t, r, key)
	pod := thePod(t, c, ns, ws.UID)
	pod.Status.Phase = corev1.PodPending
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:  "desktop",
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}},
	}}
	if err := c.Status().Update(context.Background(), pod); err != nil {
		t.Fatalf("pod status: %v", err)
	}
	now = now.Add(time.Second) // past the 300 ms boot deadline
	reconcile(t, r, key)
	got := getWorkspace(t, c, key)
	if got.Status.Phase != workspacesv1alpha1.WorkspacePhaseFailed {
		t.Fatalf("phase = %q, want Failed", got.Status.Phase)
	}
	if cc := condition(got, workspacesv1alpha1.ConditionConnectionReady); cc == nil || cc.Reason != "ImagePullBackOff" {
		t.Fatalf("ConnectionReady at failure = %+v, want reason ImagePullBackOff", cc)
	}

	// FailedCleanupDelay later the incarnation is deleted; the stalled step
	// must survive both the cleanup pass and the next observation.
	now = now.Add(FailedCleanupDelay + time.Minute)
	reconcile(t, r, key)
	reconcile(t, r, key)
	got = getWorkspace(t, c, key)
	if d := condition(got, workspacesv1alpha1.ConditionDegraded); d == nil || d.Reason != ReasonFailedCleanup {
		t.Fatalf("Degraded = %+v, want reason %s", d, ReasonFailedCleanup)
	}
	for _, typ := range []string{workspacesv1alpha1.ConditionRuntimeReady, workspacesv1alpha1.ConditionConnectionReady} {
		cc := condition(got, typ)
		if cc == nil || cc.Status != metav1.ConditionFalse || cc.Reason == "Provisioning" {
			t.Fatalf("%s after cleanup = %+v; the stalled step was overwritten", typ, cc)
		}
	}
	if cc := condition(got, workspacesv1alpha1.ConditionConnectionReady); cc.Reason != "ImagePullBackOff" {
		t.Fatalf("ConnectionReady after cleanup = %q, want ImagePullBackOff", cc.Reason)
	}

	// A new start intent unfreezes: the next attempt reports its own steps.
	cur := getWorkspace(t, c, key)
	cur.Spec.IntentRevision = 2
	cur.Spec.RuntimeGeneration = 2
	if err := c.Update(context.Background(), cur); err != nil {
		t.Fatalf("restart intent: %v", err)
	}
	reconcile(t, r, key)
	got = getWorkspace(t, c, key)
	if got.Status.Phase == workspacesv1alpha1.WorkspacePhaseFailed {
		t.Fatalf("phase = %q after a new intent", got.Status.Phase)
	}
	if cc := condition(got, workspacesv1alpha1.ConditionConnectionReady); cc == nil || cc.Reason == "ImagePullBackOff" {
		t.Fatalf("ConnectionReady = %+v; a new attempt must not inherit the old stalled step", cc)
	}
}

// probeSeams implements every finalizer seam and records, for each step, the
// RuntimeReady reason visible on the object while that step runs.
type probeSeams struct {
	t    *testing.T
	c    client.Client
	key  types.NamespacedName
	mu   sync.Mutex
	seen map[FinalizerStep]string
}

func (p *probeSeams) probe(step FinalizerStep) error {
	got := &workspacesv1alpha1.Workspace{}
	if err := p.c.Get(context.Background(), p.key, got); err != nil {
		p.t.Errorf("probe get: %v", err)
		return nil
	}
	reason := ""
	if rc := condition(got, workspacesv1alpha1.ConditionRuntimeReady); rc != nil && rc.Status == metav1.ConditionFalse {
		reason = rc.Reason
	}
	p.mu.Lock()
	p.seen[step] = reason
	p.mu.Unlock()
	return nil
}

func (p *probeSeams) hit(step FinalizerStep) error { return p.probe(step) }
func (p *probeSeams) BlockConnects(context.Context, provisioning.PlatformID) error {
	return p.probe(StepBlockConnects)
}
func (p *probeSeams) RevokeAllForWorkspace(context.Context, provisioning.PlatformID, int64) (int, error) {
	return 0, p.probe(StepRevokeLeases)
}
func (p *probeSeams) DrainStatus(context.Context, provisioning.PlatformID) (int, bool, error) {
	return 0, true, p.probe(StepDrainStreams)
}
func (p *probeSeams) ApplyRetention(context.Context, *workspacesv1alpha1.Workspace) error {
	return p.probe(StepRetention)
}

// TestProgress_TeardownStepReasons (G5): while the finalizer runs a step,
// RuntimeReady is False with a reason naming that step, so the portal can
// show five delete steps instead of "Deleting".
func TestProgress_TeardownStepReasons(t *testing.T) {
	env, c := startEnv(t)
	defer func() {
		if err := env.Stop(); err != nil {
			t.Logf("envtest stop: %v", err)
		}
	}()
	ns := newNamespace(t, c)
	newTemplate(t, c, ns, "tpl-g5", nil)
	ws, key := deletingWorkspace(t, c, ns, "ws-g5", "tpl-g5", nil)

	p := &probeSeams{t: t, c: c, key: key, seen: map[FinalizerStep]string{}}
	f := &Finalizer{Client: c, Backend: &fakeBackend{}, Connects: p, Leases: p, Drainer: p, Retention: p}
	done, err := f.Run(context.Background(), ws)
	if err != nil || !done {
		t.Fatalf("Run: done=%v err=%v", done, err)
	}
	want := map[FinalizerStep]string{
		StepBlockConnects: "BlockingConnects",
		StepRevokeLeases:  "RevokingLeases",
		StepDrainStreams:  "DrainingStreams",
		StepStopRuntime:   "StoppingRuntime",
		StepRetention:     "ApplyingRetention",
		StepCleanup:       "CleaningUp",
	}
	for step, reason := range want {
		if got := p.seen[step]; got != reason {
			t.Errorf("RuntimeReady reason during %s = %q, want %q", step, got, reason)
		}
	}
}

// TestProgress_TeardownReasonsAreTokens: every token the UI maps is a valid
// condition reason (CamelCase, no spaces), and the table covers every step.
func TestProgress_TeardownReasonsAreTokens(t *testing.T) {
	for _, step := range FinalizerOrder {
		reason := teardownStepReason(step)
		if reason == "" || strings.ContainsAny(reason, " -_:") || !(reason[0] >= 'A' && reason[0] <= 'Z') {
			t.Errorf("step %s has reason %q; want a CamelCase token", step, reason)
		}
	}
}

// failingStatusClient fails every status write (Update and Patch) with err and
// counts the attempts; everything else goes to the real client.
type failingStatusClient struct {
	client.Client
	err      error
	attempts atomic.Int64
}

func (f *failingStatusClient) Status() client.SubResourceWriter {
	return &failingStatusWriter{SubResourceWriter: f.Client.Status(), owner: f}
}

type failingStatusWriter struct {
	client.SubResourceWriter
	owner *failingStatusClient
}

func (w *failingStatusWriter) Update(context.Context, client.Object, ...client.SubResourceUpdateOption) error {
	w.owner.attempts.Add(1)
	return w.owner.err
}

func (w *failingStatusWriter) Patch(context.Context, client.Object, client.Patch, ...client.SubResourcePatchOption) error {
	w.owner.attempts.Add(1)
	return w.owner.err
}

// countingStatusClient counts successful status writes.
type countingStatusClient struct {
	client.Client
	writes atomic.Int64
}

func (f *countingStatusClient) Status() client.SubResourceWriter {
	return &countingStatusWriter{SubResourceWriter: f.Client.Status(), owner: f}
}

type countingStatusWriter struct {
	client.SubResourceWriter
	owner *countingStatusClient
}

func (w *countingStatusWriter) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	err := w.SubResourceWriter.Update(ctx, obj, opts...)
	if err == nil {
		w.owner.writes.Add(1)
	}
	return err
}

func statusConflict() error {
	return apierrors.NewConflict(schema.GroupResource{Group: "workspaces.cdi.tinyorbit.vn", Resource: "workspaces"},
		"ws", errors.New("the object has been modified; please apply your changes to the latest version"))
}

// TestProgress_TeardownCompletesWhenEveryStatusWriteConflicts: the per-step
// status marks are best effort. With every status write failing with a
// conflict a reconcile still runs all six steps in order and removes the
// finalizer, so the workspace is gone.
func TestProgress_TeardownCompletesWhenEveryStatusWriteConflicts(t *testing.T) {
	env, c := startEnv(t)
	defer func() { _ = env.Stop() }()
	ns := newNamespace(t, c)
	newTemplate(t, c, ns, "tpl-bw", nil)
	_, key := deletingWorkspace(t, c, ns, "ws-bw", "tpl-bw", nil)

	failing := &failingStatusClient{Client: c, err: statusConflict()}
	rec := newStepRecorder()
	r := newReconciler(failing)
	r.Backend = &fakeBackend{}
	r.Connects, r.Leases, r.Drainer, r.Retention = rec, rec, rec, rec

	reconcile(t, r, key)

	got := rec.sequence()
	if len(got) != len(FinalizerOrder) {
		t.Fatalf("steps run = %v, want each of %v once", got, FinalizerOrder)
	}
	for i, want := range FinalizerOrder {
		if got[i] != want {
			t.Fatalf("step order = %v, want %v", got, FinalizerOrder)
		}
	}
	if failing.attempts.Load() < int64(len(FinalizerOrder)) {
		t.Fatalf("only %d status writes attempted; the marks were not exercised", failing.attempts.Load())
	}
	if err := c.Get(context.Background(), key, &workspacesv1alpha1.Workspace{}); !apierrors.IsNotFound(err) {
		t.Fatalf("workspace still present after teardown with failing status writes: %v", err)
	}
}

// TestProgress_BackendErrorWriteFailureKeepsOriginalError: a failed
// RuntimeReady=BackendError write is logged, never returned in place of the
// backend's own error, so controller-runtime retries on the real cause.
func TestProgress_BackendErrorWriteFailureKeepsOriginalError(t *testing.T) {
	env, c := startEnv(t)
	defer func() { _ = env.Stop() }()
	ns := newNamespace(t, c)
	newTemplate(t, c, ns, "tpl-bw2", nil)
	ws := newWorkspace(t, c, ns, "ws-bw2", "tpl-bw2", nil)
	key := types.NamespacedName{Name: ws.Name, Namespace: ns}

	sentinel := errors.New("cluster refused the pod")
	for name, werr := range map[string]error{
		"status write conflicts": statusConflict(),
		"status write fails":     errors.New("apiserver unavailable"),
	} {
		t.Run(name, func(t *testing.T) {
			failing := &failingStatusClient{Client: c, err: werr}
			be := &flakyBackend{Backend: linux.New(c, linux.Options{})}
			be.set(sentinel, nil)
			r := &WorkspaceReconciler{Client: failing, Scheme: testScheme, Backend: be}

			err := reconcileErr(r, key)
			if !errors.Is(err, sentinel) {
				t.Fatalf("reconcile returned %v, want the original backend error", err)
			}
			if failing.attempts.Load() == 0 {
				t.Fatal("the BackendError write was never attempted")
			}
		})
	}
}

// TestProgress_TeardownStatusWritesDoNotLoop: the step marks are status
// writes, and the controller watches Workspaces without a predicate, so each
// one wakes the reconciler. The wake-ups must neither re-run a completed step
// nor keep producing writes. Reconcile is driven the way the watch drives it:
// again and again while the drain window is open, then once the gateway
// reports drained.
func TestProgress_TeardownStatusWritesDoNotLoop(t *testing.T) {
	env, c := startEnv(t)
	defer func() { _ = env.Stop() }()
	ns := newNamespace(t, c)
	newTemplate(t, c, ns, "tpl-loop", nil)
	_, key := deletingWorkspace(t, c, ns, "ws-loop", "tpl-loop", nil)

	counting := &countingStatusClient{Client: c}
	rec := newStepRecorder()
	rec.drained = false // the drain window stays open
	r := newReconciler(counting)
	r.Backend = &fakeBackend{}
	r.Connects, r.Leases, r.Drainer, r.Retention = rec, rec, rec, rec

	reconcile(t, r, key) // blocks, revokes, then waits in the drain window
	if n := rec.calls(StepStopRuntime); n != 0 {
		t.Fatalf("stop-runtime ran inside the drain window")
	}
	// Every later wake-up is a no-op for the object: no new status writes
	// that change it, so no new watch event, so no loop.
	rvAfterFirst := getWorkspace(t, c, key).ResourceVersion
	for i := 0; i < 5; i++ {
		reconcile(t, r, key)
	}
	if rv := getWorkspace(t, c, key).ResourceVersion; rv != rvAfterFirst {
		t.Fatalf("resourceVersion moved %s -> %s across idle wake-ups: the reconciler is generating its own events", rvAfterFirst, rv)
	}
	if b, rv := rec.calls(StepBlockConnects), rec.calls(StepRevokeLeases); b != 1 || rv != 1 {
		t.Fatalf("completed steps re-ran on wake-ups: block-connects=%d revoke-leases=%d, want 1/1", b, rv)
	}

	rec.drained = true
	reconcile(t, r, key)
	for _, step := range []FinalizerStep{StepBlockConnects, StepRevokeLeases, StepStopRuntime, StepRetention, StepCleanup} {
		if n := rec.calls(step); n != 1 {
			t.Errorf("step %s ran %d times, want exactly 1", step, n)
		}
	}
	if err := c.Get(context.Background(), key, &workspacesv1alpha1.Workspace{}); !apierrors.IsNotFound(err) {
		t.Fatalf("workspace still present after the drain finished: %v", err)
	}
	// Bounded: one mark per step plus the drain's own condition writes.
	if w := counting.writes.Load(); w > 12 {
		t.Fatalf("%d status writes for one teardown; the marks are not bounded", w)
	}
}
