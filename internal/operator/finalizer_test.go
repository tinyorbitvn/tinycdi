package operator

// Red contract tests for the the ordered finalizer
// (design §5: block connects -> revoke leases -> drain <=45 s -> stop
// runtime -> retention -> cleanup -> done). Driven directly through the
// Finalizer executor with fake seams and a real envtest client.
//
// Asserted contract:
//   - steps run exactly in FinalizerOrder and progress is persisted;
//   - a crash at ANY step resumes at that step after restart — completed
//     steps are never re-executed;
//   - the finalizer is never dropped while retention is unhandled or the
//     Broker / K8s / CSI seam is unavailable;
//   - stream drain is bounded at 45 s across restarts;
//   - partially-deleted resources are retried to completion;
//   - Retain marks persistent data for the inventory; Ephemeral removes it.
//
// Run: KUBEBUILDER_ASSETS=<repo>/bin/k8s/1.37.0-linux-amd64 \
//      go test ./internal/operator/ -run TestFinalizer -v

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	workspacesv1alpha1 "github.com/tinyorbitvn/tinycdi/api/v1alpha1"
	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
	tcdiruntime "github.com/tinyorbitvn/tinycdi/internal/runtime"
	linux "github.com/tinyorbitvn/tinycdi/internal/runtime/linux"
)

// fakeBackend is a tcdiruntime.Backend that records calls and can fail.
type fakeBackend struct {
	mu       sync.Mutex
	calls    []string
	stopErr  error
	delErr   error
	obs      tcdiruntime.Observation
	ensureFn func()
}

func (f *fakeBackend) record(s string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, s)
}

func (f *fakeBackend) callCount(s string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == s {
			n++
		}
	}
	return n
}

func (f *fakeBackend) Ensure(context.Context, *workspacesv1alpha1.Workspace, *workspacesv1alpha1.WorkspaceTemplate) (tcdiruntime.Observation, error) {
	f.record("ensure")
	if f.ensureFn != nil {
		f.ensureFn()
	}
	return f.obs, nil
}

func (f *fakeBackend) Observe(context.Context, *workspacesv1alpha1.Workspace) (tcdiruntime.Observation, error) {
	f.record("observe")
	return f.obs, nil
}

func (f *fakeBackend) Stop(context.Context, *workspacesv1alpha1.Workspace) error {
	f.record("stop")
	return f.stopErr
}

func (f *fakeBackend) DeleteRuntime(context.Context, *workspacesv1alpha1.Workspace) error {
	f.record("deleteRuntime")
	return f.delErr
}

// PodMatchesTemplate reports nothing adoptable: the fake keeps no pods.
func (f *fakeBackend) PodMatchesTemplate(context.Context, *workspacesv1alpha1.Workspace, *workspacesv1alpha1.WorkspaceTemplate) (bool, error) {
	return false, nil
}

// PodTemplateIdentity reports no owned pod: the fake keeps no pods.
func (f *fakeBackend) PodTemplateIdentity(context.Context, *workspacesv1alpha1.Workspace) (tcdiruntime.PodTemplateIdentity, error) {
	return tcdiruntime.PodTemplateIdentity{}, nil
}

// StampPodTemplateIdentity is a no-op: the fake keeps no pods.
func (f *fakeBackend) StampPodTemplateIdentity(context.Context, *workspacesv1alpha1.Workspace, *workspacesv1alpha1.WorkspaceTemplate) error {
	return nil
}

// stepRecorder implements every finalizer seam, recording call order and
// injecting failures: failLeft[step]>0 makes that step return an error and
// decrements the counter (simulating a crash inside the step).
type stepRecorder struct {
	mu       sync.Mutex
	order    []FinalizerStep
	uids     map[FinalizerStep]string // last workspace id arg per seam step
	failLeft map[FinalizerStep]int
	drained  bool
	// drainGateRevoke models the broker drain contract after revocation: streams
	// count as open until the revoke step has landed — revocation, not the
	// gateway's close report, is what closes drain accounting.
	drainGateRevoke bool
}

func newStepRecorder() *stepRecorder {
	return &stepRecorder{failLeft: map[FinalizerStep]int{},
		uids: map[FinalizerStep]string{}, drained: true}
}

func (r *stepRecorder) hit(step FinalizerStep) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.order = append(r.order, step)
	if r.failLeft[step] > 0 {
		r.failLeft[step]--
		return fmt.Errorf("injected crash inside step %s", step)
	}
	return nil
}

func (r *stepRecorder) calls(step FinalizerStep) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, s := range r.order {
		if s == step {
			n++
		}
	}
	return n
}

func (r *stepRecorder) sequence() []FinalizerStep {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]FinalizerStep{}, r.order...)
}

func (r *stepRecorder) uidFor(step FinalizerStep) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.uids[step]
}

func (r *stepRecorder) BlockConnects(_ context.Context, uid provisioning.PlatformID) error {
	r.mu.Lock()
	r.uids[StepBlockConnects] = string(uid)
	r.mu.Unlock()
	return r.hit(StepBlockConnects)
}
func (r *stepRecorder) RevokeAllForWorkspace(_ context.Context, uid provisioning.PlatformID, _ int64) (int, error) {
	r.mu.Lock()
	r.uids[StepRevokeLeases] = string(uid)
	r.mu.Unlock()
	return 0, r.hit(StepRevokeLeases)
}
func (r *stepRecorder) DrainStatus(_ context.Context, uid provisioning.PlatformID) (int, bool, error) {
	r.mu.Lock()
	r.uids[StepDrainStreams] = string(uid)
	r.mu.Unlock()
	if err := r.hit(StepDrainStreams); err != nil {
		return 0, false, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	drained := r.drained
	if r.drainGateRevoke && r.uids[StepRevokeLeases] == "" {
		drained = false
	}
	open := 0
	if !drained {
		open = 1
	}
	return open, drained, nil
}
func (r *stepRecorder) ApplyRetention(context.Context, *workspacesv1alpha1.Workspace) error {
	return r.hit(StepRetention)
}

// deletingWorkspace creates a running workspace, attaches the operator
// finalizer, then deletes it so it lingers in Terminating. The fixture
// stamps the platform workspace-id label the API applier would set —
// production teardown keys broker seams by it and never falls back to
// the k8s object UID.
func deletingWorkspace(t *testing.T, c client.Client, ns, name, tpl string, mutate func(*workspacesv1alpha1.Workspace)) (*workspacesv1alpha1.Workspace, types.NamespacedName) {
	t.Helper()
	ws := newWorkspace(t, c, ns, name, tpl, func(ws *workspacesv1alpha1.Workspace) {
		if ws.Labels == nil {
			ws.Labels = map[string]string{}
		}
		ws.Labels[provisioning.LabelWorkspaceUID] = "ws_it_" + name
		if mutate != nil {
			mutate(ws)
		}
	})
	key := types.NamespacedName{Name: name, Namespace: ns}
	ws = getWorkspace(t, c, key)
	ws.Finalizers = append(ws.Finalizers, FinalizerRuntimeCleanup)
	if err := c.Update(context.Background(), ws); err != nil {
		t.Fatalf("attach finalizer: %v", err)
	}
	if err := c.Delete(context.Background(), ws); err != nil {
		t.Fatalf("delete workspace: %v", err)
	}
	return getWorkspace(t, c, key), key
}

func newFinalizer(c client.Client, rec *stepRecorder, be tcdiruntime.Backend) *Finalizer {
	return &Finalizer{
		Client:    c,
		Backend:   be,
		Connects:  rec,
		Leases:    rec,
		Drainer:   rec,
		Retention: rec,
	}
}

// TestFinalizer_OrderedTeardown: a clean run executes every step exactly
// once, in the mandated order, and only then reports done.
func TestFinalizer_OrderedTeardown(t *testing.T) {
	env, c := startEnv(t)
	defer func() {
		if err := env.Stop(); err != nil {
			t.Logf("envtest stop: %v", err)
		}
	}()

	ns := newNamespace(t, c)
	newTemplate(t, c, ns, "tpl-fin", nil)
	ws, _ := deletingWorkspace(t, c, ns, "ws-fin", "tpl-fin", nil)

	rec := newStepRecorder()
	f := newFinalizer(c, rec, &fakeBackend{})

	done, err := f.Run(context.Background(), ws)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !done {
		t.Fatal("Run reported not-done with all seams healthy")
	}

	got := rec.sequence()
	if len(got) != len(FinalizerOrder) {
		t.Fatalf("steps executed = %v, want order %v", got, FinalizerOrder)
	}
	for i, want := range FinalizerOrder {
		if got[i] != want {
			t.Fatalf("step order = %v, want %v", got, FinalizerOrder)
		}
	}
}

// TestFinalizer_CrashAtEachStep: for every step, a crash inside it leaves
// the finalizer in place and the persisted progress stops before the failed
// step; a restarted Finalizer resumes at the failed step without
// re-executing completed ones.
func TestFinalizer_CrashAtEachStep(t *testing.T) {
	env, c := startEnv(t)
	defer func() {
		if err := env.Stop(); err != nil {
			t.Logf("envtest stop: %v", err)
		}
	}()

	for _, crashAt := range FinalizerOrder {
		crashAt := crashAt
		t.Run(string(crashAt), func(t *testing.T) {
			ns := newNamespace(t, c)
			newTemplate(t, c, ns, "tpl-"+string(crashAt), nil)
			ws, key := deletingWorkspace(t, c, ns, "ws-"+string(crashAt), "tpl-"+string(crashAt), nil)

			rec := newStepRecorder()
			rec.failLeft[crashAt] = 1 // first attempt crashes
			f1 := newFinalizer(c, rec, &fakeBackend{})

			done, err := f1.Run(context.Background(), ws)
			if err == nil {
				t.Fatalf("Run with crashing %s returned nil error", crashAt)
			}
			if done {
				t.Fatalf("Run reported done while step %s crashed", crashAt)
			}

			// Finalizer must still be on the object — never dropped mid-flight.
			cur := getWorkspace(t, c, key)
			found := false
			for _, fz := range cur.Finalizers {
				if fz == FinalizerRuntimeCleanup {
					found = true
				}
			}
			if !found {
				t.Fatalf("finalizer dropped while step %s was incomplete", crashAt)
			}

			crashIdx := -1
			for i, s := range FinalizerOrder {
				if s == crashAt {
					crashIdx = i
				}
			}

			// "Restart": a new Finalizer over the same persisted state.
			f2 := newFinalizer(c, rec, &fakeBackend{})
			done, err = f2.Run(context.Background(), cur)
			if err != nil {
				t.Fatalf("resumed Run after %s crash: %v", crashAt, err)
			}
			if !done {
				t.Fatalf("resumed Run after %s crash did not finish", crashAt)
			}

			for i, s := range FinalizerOrder {
				want := 1
				if i == crashIdx {
					want = 2 // crashed once, retried once
				}
				if got := rec.calls(s); got != want {
					t.Fatalf("step %s executed %d times, want %d (completed steps must not re-run)", s, got, want)
				}
			}
		})
	}
}

// TestFinalizer_UnavailableDepsNeverDrop: an unavailable dependency must
// fail the step loudly — the finalizer is never dropped silently while the
// Broker, K8s backend or retention handler is missing.
func TestFinalizer_UnavailableDepsNeverDrop(t *testing.T) {
	env, c := startEnv(t)
	defer func() {
		if err := env.Stop(); err != nil {
			t.Logf("envtest stop: %v", err)
		}
	}()

	cases := []struct {
		name   string
		mutate func(f *Finalizer, rec *stepRecorder, be *fakeBackend)
	}{
		{"broker connects seam missing", func(f *Finalizer, rec *stepRecorder, be *fakeBackend) { f.Connects = nil }},
		{"broker leases seam missing", func(f *Finalizer, rec *stepRecorder, be *fakeBackend) { f.Leases = nil }},
		{"drainer seam missing", func(f *Finalizer, rec *stepRecorder, be *fakeBackend) { f.Drainer = nil }},
		{"retention unhandled", func(f *Finalizer, rec *stepRecorder, be *fakeBackend) { f.Retention = nil }},
		{"k8s backend down", func(f *Finalizer, rec *stepRecorder, be *fakeBackend) {
			be.stopErr = errors.New("k8s api unavailable")
			be.delErr = errors.New("k8s api unavailable")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ns := newNamespace(t, c)
			newTemplate(t, c, ns, "tpl-dep", nil)
			ws, key := deletingWorkspace(t, c, ns, "ws-dep", "tpl-dep", nil)

			rec := newStepRecorder()
			be := &fakeBackend{}
			f := newFinalizer(c, rec, be)
			tc.mutate(f, rec, be)

			done, err := f.Run(context.Background(), ws)
			if done || err == nil {
				t.Fatalf("Run = done %v err %v with dependency missing; want blocked", done, err)
			}

			cur := getWorkspace(t, c, key)
			found := false
			for _, fz := range cur.Finalizers {
				if fz == FinalizerRuntimeCleanup {
					found = true
				}
			}
			if !found {
				t.Fatal("finalizer dropped while a teardown dependency was unavailable")
			}
			// The blockage must be visible on the object: Degraded=True with a
			// teardown reason — operators must not need logs to see it.
			if cond := condition(cur, workspacesv1alpha1.ConditionDegraded); cond == nil ||
				cond.Status != metav1.ConditionTrue {
				t.Fatalf("missing Degraded=True while teardown blocked; conds=%v", cur.Status.Conditions)
			}
		})
	}
}

// TestFinalizer_RetentionPendingKeepsFinalizer: while the retention step
// has not completed, the finalizer stays and the object reports
// RetentionPending — deleting a workspace must never strand its data
// bookkeeping.
func TestFinalizer_RetentionPendingKeepsFinalizer(t *testing.T) {
	env, c := startEnv(t)
	defer func() {
		if err := env.Stop(); err != nil {
			t.Logf("envtest stop: %v", err)
		}
	}()

	ns := newNamespace(t, c)
	newTemplate(t, c, ns, "tpl-ret", nil)
	ws, key := deletingWorkspace(t, c, ns, "ws-ret", "tpl-ret", func(ws *workspacesv1alpha1.Workspace) {
		ws.Spec.DataPolicy = workspacesv1alpha1.DataPolicyRetain
	})

	rec := newStepRecorder()
	rec.failLeft[StepRetention] = 2 // retention keeps failing (CSI down)
	f := newFinalizer(c, rec, &fakeBackend{})

	for i := 0; i < 2; i++ {
		done, err := f.Run(context.Background(), ws)
		if done || err == nil {
			t.Fatalf("Run %d = done %v err %v while retention failing", i, done, err)
		}
	}
	cur := getWorkspace(t, c, key)
	found := false
	for _, fz := range cur.Finalizers {
		if fz == FinalizerRuntimeCleanup {
			found = true
		}
	}
	if !found {
		t.Fatal("finalizer dropped with retention unhandled")
	}
	if cond := condition(cur, workspacesv1alpha1.ConditionDegraded); cond == nil ||
		cond.Reason != ReasonRetentionPending {
		t.Fatalf("want Degraded reason %q while retention pending, conds=%v",
			ReasonRetentionPending, cur.Status.Conditions)
	}
	// Steps after retention must never have run.
	if n := rec.calls(StepCleanup); n != 0 {
		t.Fatalf("cleanup ran %d times while retention was incomplete", n)
	}
}

// TestFinalizer_DrainBounded45s: the gateway gets at most 45 s to close
// streams; a restart does not reset the window, and once it expires
// teardown proceeds.
func TestFinalizer_DrainBounded45s(t *testing.T) {
	env, c := startEnv(t)
	defer func() {
		if err := env.Stop(); err != nil {
			t.Logf("envtest stop: %v", err)
		}
	}()

	ns := newNamespace(t, c)
	newTemplate(t, c, ns, "tpl-drain", nil)
	ws, key := deletingWorkspace(t, c, ns, "ws-drain", "tpl-drain", nil)

	now := time.Now()
	rec := newStepRecorder()
	rec.drained = false // gateway never closes its streams
	be := &fakeBackend{}
	f := newFinalizer(c, rec, be)
	f.Now = func() time.Time { return now }
	f.DrainBudget = MaxStreamDrain

	done, err := f.Run(context.Background(), ws)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if done {
		t.Fatal("Run finished while streams were still draining inside the window")
	}

	now = now.Add(30 * time.Second)
	done, err = f.Run(context.Background(), ws)
	if err != nil {
		t.Fatalf("Run at +30s: %v", err)
	}
	if done {
		t.Fatal("teardown proceeded before the 45 s drain window expired")
	}

	// Restart inside the window: the deadline anchors to the PERSISTED
	// drain start, not the restart time.
	fRestart := newFinalizer(c, rec, be)
	fRestart.Now = func() time.Time { return now }
	fRestart.DrainBudget = MaxStreamDrain
	now = now.Add(16 * time.Second) // 46 s since drain began
	done, err = fRestart.Run(context.Background(), ws)
	if err != nil {
		t.Fatalf("Run after drain deadline: %v", err)
	}
	if !done {
		t.Fatal("teardown still blocked after the 45 s drain budget expired")
	}
	// The runtime stop step ran exactly once even though the drain window
	// spanned a restart.
	if n := be.callCount("stop"); n != 1 {
		t.Fatalf("backend stop called %d times, want exactly 1", n)
	}
	cur := getWorkspace(t, c, key)
	if cond := condition(cur, workspacesv1alpha1.ConditionDegraded); cond == nil ||
		cond.Reason != ReasonDrainTimedOut {
		t.Fatalf("want Degraded reason %q after drain timeout, conds=%v",
			ReasonDrainTimedOut, cur.Status.Conditions)
	}
}

// TestFinalizer_PartialCleanupRetried: when teardown already removed some
// children before crashing, a retry finishes the remainder instead of
// failing on missing pieces or recreating anything.
func TestFinalizer_PartialCleanupRetried(t *testing.T) {
	env, c := startEnv(t)
	defer func() {
		if err := env.Stop(); err != nil {
			t.Logf("envtest stop: %v", err)
		}
	}()

	ns := newNamespace(t, c)
	newTemplate(t, c, ns, "tpl-partial", nil)
	ws0 := newWorkspace(t, c, ns, "ws-partial", "tpl-partial", func(ws *workspacesv1alpha1.Workspace) {
		ws.Labels = map[string]string{provisioning.LabelWorkspaceUID: "ws_it_partial"}
	})
	key := types.NamespacedName{Name: ws0.Name, Namespace: ns}

	// Provision real children with the real backend.
	rc := newReconciler(c)
	reconcile(t, rc, key)
	pods, svcs, secrets, netpols := countChildren(t, c, ns, ws0.UID)
	if pods+svcs+secrets+netpols == 0 {
		t.Fatal("expected provisioned children before teardown")
	}

	ws := getWorkspace(t, c, key)
	ws.Finalizers = append(ws.Finalizers, FinalizerRuntimeCleanup)
	if err := c.Update(context.Background(), ws); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(context.Background(), ws); err != nil {
		t.Fatal(err)
	}
	ws = getWorkspace(t, c, key)

	// Simulate partial teardown: the pod is already gone (earlier crash).
	_ = c.Delete(context.Background(), &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: linux.PodName(ws.UID), Namespace: ns,
	}})

	rec := newStepRecorder()
	f := newFinalizer(c, rec, linux.New(c, linux.Options{}))
	done, err := f.Run(context.Background(), ws)
	if err != nil {
		t.Fatalf("Run with partially-removed runtime: %v", err)
	}
	if !done {
		t.Fatal("teardown did not finish over a partially-removed runtime")
	}

	pods, svcs, secrets, netpols = countChildren(t, c, ns, ws.UID)
	if pods+svcs+secrets+netpols != 0 {
		t.Fatalf("ephemeral children remain after cleanup: %d pods %d svcs %d secrets %d netpols",
			pods, svcs, secrets, netpols)
	}
}

// TestFinalizer_BrokerSeamsUsePlatformID: every broker-facing seam
// (block-connects, revoke-leases, drain-status) is keyed by the platform
// workspace id from the CR's workspace-uid label — never by the CR's k8s
// UID, which the broker cannot resolve.
func TestFinalizer_BrokerSeamsUsePlatformID(t *testing.T) {
	env, c := startEnv(t)
	defer func() {
		if err := env.Stop(); err != nil {
			t.Logf("envtest stop: %v", err)
		}
	}()

	ns := newNamespace(t, c)
	newTemplate(t, c, ns, "tpl-uid", nil)
	ws, _ := deletingWorkspace(t, c, ns, "ws-uid", "tpl-uid", nil)

	rec := newStepRecorder()
	f := newFinalizer(c, rec, &fakeBackend{})
	if done, err := f.Run(context.Background(), ws); err != nil || !done {
		t.Fatalf("Run: done=%v err=%v", done, err)
	}
	wantUID := ws.Labels[provisioning.LabelWorkspaceUID]
	for _, step := range []FinalizerStep{StepBlockConnects, StepRevokeLeases, StepDrainStreams} {
		if got := rec.uidFor(step); got != wantUID {
			t.Fatalf("%s keyed by %q, want platform id %q (CR UID %s must never reach the broker)",
				step, got, wantUID, ws.UID)
		}
	}
}

// TestFinalizer_MissingWorkspaceIDDegrades: a CR without the platform
// workspace-id label is data-plane corruption — Run degrades the object
// with reason MissingWorkspaceID, calls no broker seam and reports
// not-done so the finalizer is kept for a human to restore the label.
func TestFinalizer_MissingWorkspaceIDDegrades(t *testing.T) {
	env, c := startEnv(t)
	defer func() {
		if err := env.Stop(); err != nil {
			t.Logf("envtest stop: %v", err)
		}
	}()

	ns := newNamespace(t, c)
	newTemplate(t, c, ns, "tpl-noid", nil)
	// Label-less CR: deletingWorkspace stamps the label, so build the
	// deleting object by hand.
	ws := newWorkspace(t, c, ns, "ws-noid", "tpl-noid", nil)
	key := types.NamespacedName{Name: ws.Name, Namespace: ns}
	ws = getWorkspace(t, c, key)
	ws.Finalizers = append(ws.Finalizers, FinalizerRuntimeCleanup)
	if err := c.Update(context.Background(), ws); err != nil {
		t.Fatalf("attach finalizer: %v", err)
	}
	if err := c.Delete(context.Background(), ws); err != nil {
		t.Fatalf("delete workspace: %v", err)
	}
	ws = getWorkspace(t, c, key)

	rec := newStepRecorder()
	f := newFinalizer(c, rec, &fakeBackend{})
	done, err := f.Run(context.Background(), ws)
	if err != nil {
		t.Fatalf("Run must degrade, not error: %v", err)
	}
	if done {
		t.Fatal("teardown completed despite the missing platform id")
	}
	for _, step := range []FinalizerStep{StepBlockConnects, StepRevokeLeases, StepDrainStreams} {
		if rec.calls(step) != 0 {
			t.Fatalf("broker seam %s called without a platform id", step)
		}
	}
	ws = getWorkspace(t, c, key)
	var cond *metav1.Condition
	for i := range ws.Status.Conditions {
		if ws.Status.Conditions[i].Type == workspacesv1alpha1.ConditionDegraded {
			cond = &ws.Status.Conditions[i]
		}
	}
	if cond == nil || cond.Reason != ReasonMissingWorkspaceID {
		t.Fatalf("Degraded condition = %+v, want reason %s", cond, ReasonMissingWorkspaceID)
	}
}

// TestFinalizer_RevokeClosesDrainAccounting: once lease revocation
// lands, the broker's drain accounting is already closed (revocation is
// authoritative): the very first DrainStatus poll reports drained, so
// teardown completes in a single pass — it neither waits the 45 s budget
// nor stamps Degraded(DrainTimedOut)/Degraded(StreamDraining) on a healthy
// delete.
func TestFinalizer_RevokeClosesDrainAccounting(t *testing.T) {
	env, c := startEnv(t)
	defer func() {
		if err := env.Stop(); err != nil {
			t.Logf("envtest stop: %v", err)
		}
	}()

	ns := newNamespace(t, c)
	newTemplate(t, c, ns, "tpl-rdrain", nil)
	ws, key := deletingWorkspace(t, c, ns, "ws-rdrain", "tpl-rdrain", nil)

	now := time.Now()
	rec := newStepRecorder()
	// Streams report open until revocation lands (the broker contract);
	// they never report closed on their own — the gateway's disconnect
	// report is lost to the killSession close-drop race.
	rec.drainGateRevoke = true
	f := newFinalizer(c, rec, &fakeBackend{})
	f.Now = func() time.Time { return now }
	f.DrainBudget = MaxStreamDrain

	done, err := f.Run(context.Background(), ws)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !done {
		t.Fatal("teardown still waiting after revocation closed drain accounting")
	}
	if got := rec.calls(StepRevokeLeases); got != 1 {
		t.Fatalf("revoke ran %d times, want 1", got)
	}
	if got := rec.calls(StepDrainStreams); got < 1 {
		t.Fatal("drain step never polled")
	}
	cur := getWorkspace(t, c, key)
	if cond := condition(cur, workspacesv1alpha1.ConditionDegraded); cond != nil &&
		(cond.Reason == ReasonDrainTimedOut || cond.Reason == ReasonStreamDraining) {
		t.Fatalf("healthy teardown degraded: %+v", cond)
	}
}
