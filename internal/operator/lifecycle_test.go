package operator

// Red contract tests for the operator-side lifecycle:
// generation-fenced RequestStop, deadline scheduling from persisted state,
// Failed-workspace cleanup with explicit retry, and no infinite
// runtime-recreation loop. Everything fails today on ErrNotImplemented or
// missing behaviour until lifecycle.go landed.
//
// Run: KUBEBUILDER_ASSETS=<repo>/bin/k8s/1.37.0-linux-amd64 \
//      go test ./internal/operator/ -run 'TestRequestStop|TestLifecycle' -v

import (
	"context"
	"errors"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/tinyorbitvn/tinycdi/internal/provisioning"

	corev1 "k8s.io/api/core/v1"

	workspacesv1alpha1 "github.com/tinyorbitvn/tinycdi/api/v1alpha1"
)

// TestRequestStop_StaleGeneration: a stop pinned to generation 1 must be
// refused while generation 2 is the live one — the "controller re-checks
// generation before Stop" rule (design §8).
func TestRequestStop_StaleGeneration(t *testing.T) {
	env, c := startEnv(t)
	defer func() {
		if err := env.Stop(); err != nil {
			t.Logf("envtest stop: %v", err)
		}
	}()

	ns := newNamespace(t, c)
	newTemplate(t, c, ns, "tpl-stop", nil)
	ws := newWorkspace(t, c, ns, "ws-stop", "tpl-stop", func(ws *workspacesv1alpha1.Workspace) {
		ws.Spec.RuntimeGeneration = 2
	})
	key := types.NamespacedName{Name: ws.Name, Namespace: ns}
	r := newReconciler(c)

	reconcile(t, r, key)
	thePod(t, c, ns, ws.UID) // generation-2 incarnation is up

	wsNow := getWorkspace(t, c, key)
	_, err := r.RequestStop(context.Background(), wsNow, StopRequest{
		WorkspaceUID:      provisioning.PlatformID(ws.Labels[provisioning.LabelWorkspaceUID]),
		RuntimeGeneration: 1,
		Reason:            "idle_timeout",
	})
	if !errors.Is(err, ErrStaleGeneration) {
		t.Fatalf("RequestStop(gen 1 while gen 2 runs) = %v, want ErrStaleGeneration", err)
	}

	// No side effects: desiredState untouched, runtime still there.
	wsNow = getWorkspace(t, c, key)
	if wsNow.Spec.DesiredState != workspacesv1alpha1.DesiredStateRunning {
		t.Fatalf("stale stop flipped desiredState to %q", wsNow.Spec.DesiredState)
	}
	if pods := listOwned(t, c, ns, ws.UID, &corev1.PodList{}, "pods"); pods != 1 {
		t.Fatalf("stale stop disturbed the running incarnation: %d pods", pods)
	}
}

// TestRequestStop_CurrentGeneration: a stop pinned to the live generation is
// applied — subsequent reconcile converges the runtime down.
func TestRequestStop_CurrentGeneration(t *testing.T) {
	env, c := startEnv(t)
	defer func() {
		if err := env.Stop(); err != nil {
			t.Logf("envtest stop: %v", err)
		}
	}()

	ns := newNamespace(t, c)
	newTemplate(t, c, ns, "tpl-stop2", nil)
	ws := newWorkspace(t, c, ns, "ws-stop2", "tpl-stop2", func(ws *workspacesv1alpha1.Workspace) {
		ws.Spec.RuntimeGeneration = 2
	})
	key := types.NamespacedName{Name: ws.Name, Namespace: ns}
	r := newReconciler(c)

	reconcile(t, r, key)
	thePod(t, c, ns, ws.UID)

	wsNow := getWorkspace(t, c, key)
	applied, err := r.RequestStop(context.Background(), wsNow, StopRequest{
		WorkspaceUID:      provisioning.PlatformID(ws.Labels[provisioning.LabelWorkspaceUID]),
		RuntimeGeneration: 2,
		Reason:            "idle_timeout",
	})
	if err != nil || !applied {
		t.Fatalf("RequestStop(gen 2) = applied %v, err %v; want applied", applied, err)
	}

	for i := 0; i < 5; i++ {
		reconcile(t, r, key)
	}
	wsNow = getWorkspace(t, c, key)
	if wsNow.Status.Phase != workspacesv1alpha1.WorkspacePhaseStopped {
		t.Fatalf("phase=%q after applied stop, want Stopped", wsNow.Status.Phase)
	}
	if pods := listOwned(t, c, ns, ws.UID, &corev1.PodList{}, "pods"); pods != 0 {
		t.Fatalf("pod still present after applied stop")
	}
}

// TestLifecycle_FailedCleanupDeadline: a Failed incarnation is torn down
// once the cleanup deadline passes — the deadline is recomputed from
// persisted status, never held in memory, so it survives restarts.
func TestLifecycle_FailedCleanupDeadline(t *testing.T) {
	env, c := startEnv(t)
	defer func() {
		if err := env.Stop(); err != nil {
			t.Logf("envtest stop: %v", err)
		}
	}()

	ns := newNamespace(t, c)
	newTemplate(t, c, ns, "tpl-fail", func(tpl *workspacesv1alpha1.WorkspaceTemplate) {
		tpl.Spec.BootDeadline = metav1.Duration{Duration: time.Millisecond}
	})
	ws := newWorkspace(t, c, ns, "ws-fail", "tpl-fail", nil)
	key := types.NamespacedName{Name: ws.Name, Namespace: ns}

	now := time.Now()
	r := newReconciler(c)
	r.Now = func() time.Time { return now }

	reconcile(t, r, key)       // admit + record applied intent
	now = now.Add(time.Second) // past the 1ms boot deadline
	reconcile(t, r, key)       // -> Failed
	wsNow := getWorkspace(t, c, key)
	if wsNow.Status.Phase != workspacesv1alpha1.WorkspacePhaseFailed {
		t.Fatalf("phase=%q want Failed", wsNow.Status.Phase)
	}
	thePod(t, c, ns, ws.UID) // failed incarnation still present

	// The operator must schedule the cleanup deadline from persisted state.
	delay := r.NextLifecycleRequeue(wsNow)
	if delay <= 0 || delay > FailedCleanupDelay {
		t.Fatalf("NextLifecycleRequeue on Failed ws = %v, want in (0,%v]",
			delay, FailedCleanupDelay)
	}

	// A "restarted" reconciler recomputes the same deadline (no memory).
	rB := newReconciler(c)
	rB.Now = func() time.Time { return now }
	if got := rB.NextLifecycleRequeue(wsNow); got != delay {
		t.Fatalf("restarted reconciler deadline %v != %v — deadlines must derive from state", got, delay)
	}

	// Past the deadline the failed incarnation is torn down and the
	// workspace reports cleanup, but the record itself stays Failed until
	// an explicit retry arrives.
	now = now.Add(FailedCleanupDelay + time.Second)
	reconcile(t, rB, key)
	if pods := listOwned(t, c, ns, ws.UID, &corev1.PodList{}, "pods"); pods != 0 {
		t.Fatalf("failed incarnation still present after cleanup deadline")
	}
}

// TestLifecycle_ExplicitRetryAfterFailed: a Failed workspace only restarts
// on an explicit new intent (higher intentRevision AND a new
// runtimeGeneration) — then a fresh incarnation converges normally. The new
// generation must produce a NEW incarnation (new runtimeUID) so tickets,
// leases and activity bound to the failed one stay fenced.
func TestLifecycle_ExplicitRetryAfterFailed(t *testing.T) {
	env, c := startEnv(t)
	defer func() {
		if err := env.Stop(); err != nil {
			t.Logf("envtest stop: %v", err)
		}
	}()

	ns := newNamespace(t, c)
	newTemplate(t, c, ns, "tpl-retry", func(tpl *workspacesv1alpha1.WorkspaceTemplate) {
		tpl.Spec.BootDeadline = metav1.Duration{Duration: 5 * time.Minute}
	})
	ws := newWorkspace(t, c, ns, "ws-retry", "tpl-retry", nil)
	key := types.NamespacedName{Name: ws.Name, Namespace: ns}

	now := time.Now()
	r := newReconciler(c)
	r.Now = func() time.Time { return now }

	reconcile(t, r, key)
	pod1 := thePod(t, c, ns, ws.UID)
	now = now.Add(6 * time.Minute) // past the 5 m boot deadline, never ready
	reconcile(t, r, key)
	wsNow := getWorkspace(t, c, key)
	if wsNow.Status.Phase != workspacesv1alpha1.WorkspacePhaseFailed {
		t.Fatalf("phase=%q want Failed", wsNow.Status.Phase)
	}

	// A Failed workspace carries a scheduled cleanup deadline — the
	// explicit retry below is the only other way forward.
	if got := r.NextLifecycleRequeue(wsNow); got <= 0 {
		t.Fatalf("NextLifecycleRequeue on Failed ws = %v, want >0", got)
	}

	// Explicit retry: the API issues a start intent — new generation, new
	// revision, same desiredState Running.
	wsNow = getWorkspace(t, c, key)
	wsNow.Spec.DesiredState = workspacesv1alpha1.DesiredStateRunning
	wsNow.Spec.RuntimeGeneration = 2
	wsNow.Spec.IntentRevision = 2
	if err := c.Update(context.Background(), wsNow); err != nil {
		t.Fatalf("retry intent: %v", err)
	}

	for i := 0; i < 5; i++ {
		reconcile(t, r, key)
	}
	wsNow = getWorkspace(t, c, key)
	if wsNow.Status.Phase == workspacesv1alpha1.WorkspacePhaseFailed {
		t.Fatalf("explicit retry did not recover the workspace; phase=Failed, conds=%v",
			wsNow.Status.Conditions)
	}
	if wsNow.Status.ObservedRuntimeGeneration != 2 {
		t.Fatalf("observedRuntimeGeneration=%d, want 2 for the retried generation",
			wsNow.Status.ObservedRuntimeGeneration)
	}

	// Generation 2 is a new incarnation: the pod must have been recreated
	// (new UID) so access bound to the failed generation stays fenced.
	pod2 := thePod(t, c, ns, ws.UID)
	if pod2.UID == pod1.UID {
		t.Fatalf("retried generation reused the failed incarnation (pod uid %s)", pod2.UID)
	}
	markPodReady(t, c, pod2)
	reconcile(t, r, key)
	wsNow = getWorkspace(t, c, key)
	if wsNow.Status.Phase != workspacesv1alpha1.WorkspacePhaseReady {
		t.Fatalf("phase=%q after retry+ready, want Ready", wsNow.Status.Phase)
	}
	if wsNow.Status.RuntimeUID != string(pod2.UID) {
		t.Fatalf("runtimeUID=%q want %q", wsNow.Status.RuntimeUID, pod2.UID)
	}
}

// TestLifecycle_NoInfiniteRetryCreatingRuntimes: while Failed, the operator
// must not keep recreating the runtime — deletion of the failed
// incarnation is terminal until an explicit start intent arrives.
func TestLifecycle_NoInfiniteRetryCreatingRuntimes(t *testing.T) {
	env, c := startEnv(t)
	defer func() {
		if err := env.Stop(); err != nil {
			t.Logf("envtest stop: %v", err)
		}
	}()

	ns := newNamespace(t, c)
	newTemplate(t, c, ns, "tpl-noloop", func(tpl *workspacesv1alpha1.WorkspaceTemplate) {
		tpl.Spec.BootDeadline = metav1.Duration{Duration: time.Millisecond}
	})
	ws := newWorkspace(t, c, ns, "ws-noloop", "tpl-noloop", nil)
	key := types.NamespacedName{Name: ws.Name, Namespace: ns}
	r := newReconciler(c)

	reconcile(t, r, key)
	time.Sleep(10 * time.Millisecond)
	reconcile(t, r, key)
	wsNow := getWorkspace(t, c, key)
	if wsNow.Status.Phase != workspacesv1alpha1.WorkspacePhaseFailed {
		t.Fatalf("phase=%q want Failed", wsNow.Status.Phase)
	}
	pod := thePod(t, c, ns, ws.UID)

	// Remove the failed incarnation: a Failed workspace must not respawn it
	// — that is the infinite-retry-creates-new-runtimes failure mode.
	if err := c.Delete(context.Background(), pod); err != nil {
		t.Fatalf("delete failed pod: %v", err)
	}
	for i := 0; i < 5; i++ {
		reconcile(t, r, key)
	}
	if pods := listOwned(t, c, ns, ws.UID, &corev1.PodList{}, "pods"); pods != 0 {
		t.Fatalf("Failed workspace respawned a runtime without an explicit retry")
	}
}
