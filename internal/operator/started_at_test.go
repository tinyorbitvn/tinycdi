// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package operator

// FX-R24: status.startedAt belongs to the running incarnation. A stopped
// workspace older than the template's maxDuration must start again and get
// a fresh cap, instead of being stopped by the previous incarnation's age.
//
// Run: KUBEBUILDER_ASSETS=<repo>/bin/k8s/1.37.0-linux-amd64 \
//      go test ./internal/operator/ -run 'TestStartedAt' -v

import (
	"context"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	workspacesv1alpha1 "github.com/tinyorbitvn/tinycdi/api/v1alpha1"
)

// fakeClock is a settable time source for the reconciler.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}

func (f *fakeClock) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.t = f.t.Add(d)
}

// setIntent writes a new spec intent the way the backend does.
func setIntent(t *testing.T, c client.Client, key types.NamespacedName, state workspacesv1alpha1.DesiredState, gen int64) {
	t.Helper()
	ws := getWorkspace(t, c, key)
	ws.Spec.DesiredState = state
	ws.Spec.RuntimeGeneration = gen
	ws.Spec.IntentRevision++
	if err := c.Update(context.Background(), ws); err != nil {
		t.Fatalf("set intent %s: %v", state, err)
	}
}

// bringToReady reconciles until the workspace is Ready, marking the pod ready.
func bringToReady(t *testing.T, c client.Client, r *WorkspaceReconciler, key types.NamespacedName, ns string) *workspacesv1alpha1.Workspace {
	t.Helper()
	reconcile(t, r, key)
	ws := getWorkspace(t, c, key)
	markPodReady(t, c, thePod(t, c, ns, ws.UID))
	reconcile(t, r, key)
	ws = getWorkspace(t, c, key)
	if ws.Status.Phase != workspacesv1alpha1.WorkspacePhaseReady {
		t.Fatalf("phase=%q want Ready (applied=%q)", ws.Status.Phase, ws.Annotations[AnnotationAppliedIntent])
	}
	return ws
}

// bringToStopped applies a stop intent and reconciles until Stopped.
func bringToStopped(t *testing.T, c client.Client, r *WorkspaceReconciler, key types.NamespacedName, gen int64) *workspacesv1alpha1.Workspace {
	t.Helper()
	setIntent(t, c, key, workspacesv1alpha1.DesiredStateStopped, gen)
	for i := 0; i < 5; i++ {
		reconcile(t, r, key)
	}
	ws := getWorkspace(t, c, key)
	if ws.Status.Phase != workspacesv1alpha1.WorkspacePhaseStopped {
		t.Fatalf("phase=%q want Stopped", ws.Status.Phase)
	}
	return ws
}

func TestStartedAt_IncarnationScoped(t *testing.T) {
	env, c := startEnv(t)
	defer func() {
		if err := env.Stop(); err != nil {
			t.Logf("envtest stop: %v", err)
		}
	}()
	const maxDur = 8 * time.Hour

	newRig := func(t *testing.T, name string) (*WorkspaceReconciler, *fakeClock, types.NamespacedName, string, *workspacesv1alpha1.Workspace) {
		ns := newNamespace(t, c)
		newTemplate(t, c, ns, "tpl-"+name, nil)
		ws := newWorkspace(t, c, ns, "ws-"+name, "tpl-"+name, nil)
		clock := &fakeClock{t: time.Now().Truncate(time.Second)}
		r := newReconciler(c)
		r.Now = clock.Now
		return r, clock, types.NamespacedName{Name: ws.Name, Namespace: ns}, ns, ws
	}

	t.Run("restart after maxDuration gets a fresh cap", func(t *testing.T) {
		r, clock, key, ns, _ := newRig(t, "restart")
		first := bringToReady(t, c, r, key, ns)
		firstStart := first.Status.StartedAt.Time

		clock.Advance(maxDur + time.Hour) // first start is now beyond maxDuration
		bringToStopped(t, c, r, key, 1)
		if got := getWorkspace(t, c, key).Status.StartedAt; got != nil {
			t.Fatalf("startedAt=%v on a Stopped workspace; it belongs to the ended incarnation", got)
		}

		setIntent(t, c, key, workspacesv1alpha1.DesiredStateRunning, 2)
		restarted := bringToReady(t, c, r, key, ns)
		if restarted.Annotations[AnnotationAppliedIntent] == "" {
			t.Fatal("applied intent missing")
		}
		applied, err := appliedIntent(restarted)
		if err != nil || applied.DesiredState != workspacesv1alpha1.DesiredStateRunning {
			t.Fatalf("applied=%+v err=%v; start intent was flipped back to Stopped", applied, err)
		}
		got := restarted.Status.StartedAt
		if got == nil || !got.Time.Equal(clock.Now()) || !got.Time.After(firstStart) {
			t.Fatalf("startedAt=%v want the new incarnation's start %v (first was %v)", got, clock.Now(), firstStart)
		}

		// The planner schedules the expiry from the new start.
		next := r.NextLifecycleRequeue(restarted)
		if next < maxDur-time.Minute || next > maxDur {
			t.Fatalf("NextLifecycleRequeue=%v want ~%v (cap from the new start)", next, maxDur)
		}

		// The cap still fires for the new incarnation.
		clock.Advance(maxDur + time.Second)
		for i := 0; i < 5; i++ {
			reconcile(t, r, key)
		}
		if ph := getWorkspace(t, c, key).Status.Phase; ph != workspacesv1alpha1.WorkspacePhaseStopped {
			t.Fatalf("phase=%q want Stopped once the new incarnation passes maxDuration", ph)
		}
	})

	// A row written by the pre-fix operator: Stopped, but startedAt still
	// carries the old incarnation's start beyond maxDuration.
	staleStopped := func(t *testing.T, name string) (*WorkspaceReconciler, *fakeClock, types.NamespacedName, string) {
		r, clock, key, ns, _ := newRig(t, name)
		bringToReady(t, c, r, key, ns)
		bringToStopped(t, c, r, key, 1)
		ws := getWorkspace(t, c, key)
		old := metav1.NewTime(clock.Now().Add(-maxDur - 10*time.Hour))
		ws.Status.StartedAt = &old
		if err := c.Status().Update(context.Background(), ws); err != nil {
			t.Fatalf("seed stale startedAt: %v", err)
		}
		return r, clock, key, ns
	}

	t.Run("pre-fix stale row self-heals on the next start", func(t *testing.T) {
		r, clock, key, ns := staleStopped(t, "stale")
		setIntent(t, c, key, workspacesv1alpha1.DesiredStateRunning, 2)
		// The very first pass after the start intent must not expire it.
		reconcile(t, r, key)
		applied, _ := appliedIntent(getWorkspace(t, c, key))
		if applied.DesiredState != workspacesv1alpha1.DesiredStateRunning {
			t.Fatalf("start intent was flipped to Stopped by the stale startedAt")
		}
		ws := bringToReady(t, c, r, key, ns)
		if got := ws.Status.StartedAt; got == nil || !got.Time.Equal(clock.Now()) {
			t.Fatalf("startedAt=%v want %v", got, clock.Now())
		}
	})

	t.Run("pre-fix stale row is cleaned by a plain reconcile of the stopped workspace", func(t *testing.T) {
		r, _, key, _ := staleStopped(t, "heal")
		reconcile(t, r, key) // e.g. the operator restart's initial list
		if got := getWorkspace(t, c, key).Status.StartedAt; got != nil {
			t.Fatalf("startedAt=%v still set on a Stopped workspace after reconcile", got)
		}
	})

	t.Run("planner ignores the ended incarnation's start", func(t *testing.T) {
		r, clock, key, ns, _ := newRig(t, "plan")
		ws := bringToReady(t, c, r, key, ns) // applied intent is Running
		// The state right after a start intent on a stopped workspace:
		// status still describes the ended incarnation.
		old := metav1.NewTime(clock.Now().Add(-maxDur - time.Hour))
		ws.Status.Phase = workspacesv1alpha1.WorkspacePhaseStopped
		ws.Status.StartedAt = &old
		if next := r.NextLifecycleRequeue(ws); next != 0 && next <= 2*requeueASAP {
			t.Fatalf("NextLifecycleRequeue=%v: a stale startedAt must not schedule an immediate expiry", next)
		}
	})
}
