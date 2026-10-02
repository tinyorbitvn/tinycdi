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
