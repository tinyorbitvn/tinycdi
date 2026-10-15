package api

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	cdiv1 "github.com/tinyorbitvn/tinycdi/api/v1"
)

// healthView is an idle, synced status view over an empty cache with a
// controllable clock.
func healthView(t *testing.T) (*K8sStatusView, *time.Time) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := cdiv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	clock := time.Now()
	sv := &K8sStatusView{
		cache: selectorCache{c: fake.NewClientBuilder().WithScheme(scheme).Build()},
		now:   func() time.Time { return clock }, maxStale: maxObservedStale,
		lastKnown: map[string]ObservedStatus{},
	}
	sv.MarkSynced()
	return sv, &clock
}

func countsFresh(t *testing.T, sv *K8sStatusView) bool {
	t.Helper()
	got, err := sv.TenantCounts(context.Background(), "tenant-a", StuckFinalizerAfter)
	if err != nil {
		t.Fatal(err)
	}
	return got.Fresh
}

var (
	probeOK   = func(context.Context) error { return nil }
	probeDown = func(context.Context) error { return errors.New("dial tcp: connection refused") }
)

// TestK8sStatusView_IdleHealthyWatchStaysFresh: a tenant with no
// Workspace changes (here: none at all) emits no watch events, but while
// the API server answers the liveness probe the overview stays fresh —
// staleness is about watch health, not time since the last change.
func TestK8sStatusView_IdleHealthyWatchStaysFresh(t *testing.T) {
	sv, clock := healthView(t)
	ctx := context.Background()
	for i := 0; i < 24; i++ { // two minutes of silence, probed every 5 s
		*clock = clock.Add(5 * time.Second)
		sv.livenessTick(ctx, probeOK, 5*time.Second)
	}
	if !countsFresh(t, sv) {
		t.Fatal("idle tenant with a healthy watch reported stale")
	}
	if obs, _ := sv.WorkspaceStatus(ctx, "tenant-a", "ws_x"); !obs.Fresh || obs.Found {
		t.Fatalf("WorkspaceStatus on an idle healthy watch = %+v, want fresh and not found", obs)
	}
	if _, fresh, _ := sv.RunningPolicies(ctx, "tenant-a"); !fresh {
		t.Fatal("RunningPolicies on an idle healthy watch reported stale")
	}
}

// TestK8sStatusView_UnreachableAPIServerGoesStale: client-go retries a
// refused watch silently, so the failing probe is what turns the view
// stale once maxObservedStale passes without a heartbeat.
func TestK8sStatusView_UnreachableAPIServerGoesStale(t *testing.T) {
	sv, clock := healthView(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ { // 15 s: still inside the budget
		*clock = clock.Add(5 * time.Second)
		sv.livenessTick(ctx, probeDown, 5*time.Second)
	}
	if !countsFresh(t, sv) {
		t.Fatal("stale before the freshness budget ran out")
	}
	*clock = clock.Add(5 * time.Second)
	sv.livenessTick(ctx, probeDown, 5*time.Second)
	if countsFresh(t, sv) {
		t.Fatal("unreachable API server still fresh after 20 s")
	}
	sv.livenessTick(ctx, probeOK, 5*time.Second)
	if !countsFresh(t, sv) {
		t.Fatal("not fresh again after the probe recovered")
	}
}

// TestK8sStatusView_WatchFailureStreakGoesStale: the API server answers
// lists but the Workspace watch keeps failing — a streak older than
// maxObservedStale is stale even with the probe succeeding; an informer
// event ends it at once, otherwise it ends watchRetryWindow after the last
// report.
func TestK8sStatusView_WatchFailureStreakGoesStale(t *testing.T) {
	sv, clock := healthView(t)
	ctx := context.Background()
	fail := errors.New(`failed to list *v1.Workspace: workspaces is forbidden`)
	tick := func() {
		*clock = clock.Add(5 * time.Second)
		sv.livenessTick(ctx, probeOK, 5*time.Second)
	}

	sv.NoteWatchError(workspaceTypeDescription, fail)
	tick()
	tick()
	if !countsFresh(t, sv) {
		t.Fatal("a 10 s old failure already stale: transient blips must be tolerated")
	}
	sv.NoteWatchError(workspaceTypeDescription, fail)
	tick()
	tick()
	if countsFresh(t, sv) {
		t.Fatal("watch failing for 20 s still fresh")
	}
	sv.note() // the informer delivered an event: the watch is back
	if !countsFresh(t, sv) {
		t.Fatal("an informer event did not end the streak")
	}

	sv.NoteWatchError(workspaceTypeDescription, fail)
	for i := 0; i < 4; i++ {
		tick()
	}
	if countsFresh(t, sv) {
		t.Fatal("watch failing for 20 s still fresh")
	}
	for i := 0; i < 9; i++ { // no further report for > watchRetryWindow
		tick()
	}
	if !countsFresh(t, sv) {
		t.Fatal("streak did not end after watchRetryWindow without a report")
	}
}

// TestK8sStatusView_BenignAndForeignWatchErrorsIgnored: routine closes
// (EOF, expired resource version) and failures of other informers in the
// shared cache do not start a streak.
func TestK8sStatusView_BenignAndForeignWatchErrorsIgnored(t *testing.T) {
	sv, clock := healthView(t)
	ctx := context.Background()
	gr := schema.GroupResource{Group: cdiv1.GroupVersion.Group, Resource: "workspaces"}
	for i := 0; i < 6; i++ {
		sv.NoteWatchError(workspaceTypeDescription, io.EOF)
		sv.NoteWatchError(workspaceTypeDescription, io.ErrUnexpectedEOF)
		sv.NoteWatchError(workspaceTypeDescription, apierrors.NewResourceExpired("too old resource version"))
		sv.NoteWatchError(workspaceTypeDescription, apierrors.NewGone("gone"))
		sv.NoteWatchError("*v1.WorkspaceTemplate", apierrors.NewForbidden(gr, "", errors.New("no")))
		*clock = clock.Add(5 * time.Second)
		sv.livenessTick(ctx, probeOK, 5*time.Second)
	}
	if !countsFresh(t, sv) {
		t.Fatal("benign or foreign watch errors made the view stale")
	}
	if workspaceTypeDescription != "*v1.Workspace" {
		t.Fatalf("workspaceTypeDescription = %q, want the reflector's *v1.Workspace", workspaceTypeDescription)
	}
}

// TestWatchDelivered_ResyncIsNoHeartbeat: a periodic resync replays the
// store with an unchanged resource version and keeps firing while the
// watch is down, so only a new version counts as a heartbeat.
func TestWatchDelivered_ResyncIsNoHeartbeat(t *testing.T) {
	a := &cdiv1.Workspace{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "41"}}
	b := &cdiv1.Workspace{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "42"}}
	if watchDelivered(a, a.DeepCopy()) {
		t.Fatal("resync replay counted as a heartbeat")
	}
	if !watchDelivered(a, b) {
		t.Fatal("new object version not counted as a heartbeat")
	}
}

// TestWorkspaceListProbe: the probe lists per managed namespace (or
// cluster-wide) and surfaces a failing API read.
func TestWorkspaceListProbe(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := cdiv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	ok := fake.NewClientBuilder().WithScheme(scheme).Build()
	ctx := context.Background()
	if err := WorkspaceListProbe(ok, nil)(ctx); err != nil {
		t.Fatalf("cluster-wide probe: %v", err)
	}
	if err := WorkspaceListProbe(ok, []string{"ns-a", "ns-b"})(ctx); err != nil {
		t.Fatalf("namespaced probe: %v", err)
	}
	broken := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build()
	if err := WorkspaceListProbe(broken, []string{"ns-a"})(ctx); err == nil {
		t.Fatal("probe hid a failing list")
	}
}
