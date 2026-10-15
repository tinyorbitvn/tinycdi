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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	cdiv1 "github.com/tinyorbitvn/tinycdi/api/v1"
	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
	"github.com/tinyorbitvn/tinycdi/internal/watchhealth"
)

// healthView is an idle, synced status view with a controllable clock over
// a cache holding one Ready Workspace of tenant-a. The returned apiServer
// is the API server's view the probe lists; it starts equal to the cache.
func healthView(t *testing.T) (*K8sStatusView, *time.Time, *apiServer) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := cdiv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	ws := &cdiv1.Workspace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ws-x", Namespace: "tenant-a",
			Labels: map[string]string{
				provisioning.LabelTenant:       "tenant-a",
				provisioning.LabelWorkspaceUID: "ws_x",
			},
		},
		Status: cdiv1.WorkspaceStatus{Phase: cdiv1.WorkspacePhaseReady},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ws).WithStatusSubresource(ws).Build()
	clock := time.Now()
	sv := &K8sStatusView{
		cache: selectorCache{c: c},
		now:   func() time.Time { return clock }, maxStale: maxObservedStale,
		lastKnown: map[string]ObservedStatus{},
	}
	sv.MarkSynced()
	api := &apiServer{c: c, versions: map[string]string{}}
	api.sync(t)
	return sv, &clock, api
}

// apiServer is the probe's API-server side: versions as the API server
// reports them, or a refused connection while down.
type apiServer struct {
	c        client.Client // the informer cache's backing store
	versions map[string]string
	down     bool
}

func (a *apiServer) list(context.Context) (map[string]string, error) {
	if a.down {
		return nil, errors.New("dial tcp: connection refused")
	}
	out := make(map[string]string, len(a.versions))
	for k, v := range a.versions {
		out[k] = v
	}
	return out, nil
}

// sync makes the API server's view equal to the cache (a healthy watch).
func (a *apiServer) sync(t *testing.T) {
	t.Helper()
	var l cdiv1.WorkspaceList
	if err := a.c.List(context.Background(), &l); err != nil {
		t.Fatal(err)
	}
	a.versions = map[string]string{}
	for _, ws := range l.Items {
		a.versions[ws.Namespace+"/"+ws.Name] = ws.ResourceVersion
	}
}

func countsFresh(t *testing.T, sv *K8sStatusView) bool {
	t.Helper()
	got, err := sv.TenantCounts(context.Background(), "tenant-a", StuckFinalizerAfter)
	if err != nil {
		t.Fatal(err)
	}
	return got.Fresh
}

// TestK8sStatusView_IdleHealthyWatchStaysFresh: a tenant with no
// Workspace changes emits no watch events, but while the API server's
// metadata list matches the cache the overview stays fresh — staleness is
// about the cache matching the API server, not time since the last change.
func TestK8sStatusView_IdleHealthyWatchStaysFresh(t *testing.T) {
	sv, clock, api := healthView(t)
	ctx := context.Background()
	for i := 0; i < 24; i++ { // two minutes of silence, probed every 5 s
		*clock = clock.Add(5 * time.Second)
		sv.livenessTick(ctx, api.list, 5*time.Second)
	}
	if !countsFresh(t, sv) {
		t.Fatal("idle tenant with a healthy watch reported stale")
	}
	if obs, _ := sv.WorkspaceStatus(ctx, "tenant-a", "ws_x"); !obs.Fresh || !obs.Found {
		t.Fatalf("WorkspaceStatus on an idle healthy watch = %+v, want fresh and found", obs)
	}
	if _, fresh, _ := sv.RunningPolicies(ctx, "tenant-a"); !fresh {
		t.Fatal("RunningPolicies on an idle healthy watch reported stale")
	}
}

// TestK8sStatusView_SilentStalledWatchGoesStale: the watch stops
// delivering with no error while lists succeed. A change on the API server
// the cache never sees (an update, a new Workspace) turns the view stale
// within maxObservedStale of the change, though the probe keeps answering.
func TestK8sStatusView_SilentStalledWatchGoesStale(t *testing.T) {
	cases := map[string]func(a *apiServer){
		"updated": func(a *apiServer) { a.versions["tenant-a/ws-x"] += "1" },
		"created": func(a *apiServer) { a.versions["tenant-a/ws-new"] = "9" },
		"deleted": func(a *apiServer) { delete(a.versions, "tenant-a/ws-x") },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			sv, clock, api := healthView(t)
			ctx := context.Background()
			change(api)
			changedAt := *clock
			for clock.Sub(changedAt) < maxObservedStale {
				*clock = clock.Add(5 * time.Second)
				sv.livenessTick(ctx, api.list, 5*time.Second)
			}
			if countsFresh(t, sv) {
				t.Fatal("stalled watch: overview still fresh")
			}
			if obs, _ := sv.WorkspaceStatus(ctx, "tenant-a", "ws_x"); obs.Fresh {
				t.Fatalf("stalled watch: WorkspaceStatus = %+v, want fresh:false", obs)
			}
			if _, fresh, _ := sv.RunningPolicies(ctx, "tenant-a"); fresh {
				t.Fatal("stalled watch: RunningPolicies still fresh")
			}
			api.sync(t) // the watch recovers: the cache matches again
			*clock = clock.Add(5 * time.Second)
			sv.livenessTick(ctx, api.list, 5*time.Second)
			if !countsFresh(t, sv) {
				t.Fatal("not fresh again after the cache caught up")
			}
		})
	}
}

// TestK8sStatusView_InFlightChangeStaysFresh: the API server is always one
// change ahead of the cache, and the cache catches up within the probe
// interval. The cache keeps moving, so the view stays fresh.
func TestK8sStatusView_InFlightChangeStaysFresh(t *testing.T) {
	sv, clock, api := healthView(t)
	ctx := context.Background()
	for i := 0; i < 12; i++ {
		var ws cdiv1.Workspace
		if err := api.c.Get(ctx, client.ObjectKey{Namespace: "tenant-a", Name: "ws-x"}, &ws); err != nil {
			t.Fatal(err)
		}
		ws.Status.ObservedRuntimeGeneration++
		if err := api.c.Status().Update(ctx, &ws); err != nil { // the last change arrives
			t.Fatal(err)
		}
		api.versions["tenant-a/ws-x"] = ws.ResourceVersion + "-next" // the next one is in flight
		*clock = clock.Add(5 * time.Second)
		sv.livenessTick(ctx, api.list, 5*time.Second)
		if !countsFresh(t, sv) {
			t.Fatalf("in-flight change %d: overview stale", i)
		}
	}
}

// TestK8sStatusView_UnreachableAPIServerGoesStale: client-go retries a
// refused watch silently, so the failing probe is what turns the view
// stale once maxObservedStale passes without a matching probe.
func TestK8sStatusView_UnreachableAPIServerGoesStale(t *testing.T) {
	sv, clock, api := healthView(t)
	ctx := context.Background()
	api.down = true
	for i := 0; i < 3; i++ { // 15 s: still inside the budget
		*clock = clock.Add(5 * time.Second)
		sv.livenessTick(ctx, api.list, 5*time.Second)
	}
	if !countsFresh(t, sv) {
		t.Fatal("stale before the freshness budget ran out")
	}
	*clock = clock.Add(5 * time.Second)
	sv.livenessTick(ctx, api.list, 5*time.Second)
	if countsFresh(t, sv) {
		t.Fatal("unreachable API server still fresh after 20 s")
	}
	api.down = false
	sv.livenessTick(ctx, api.list, 5*time.Second)
	if !countsFresh(t, sv) {
		t.Fatal("not fresh again after the probe recovered")
	}
}

// TestK8sStatusView_WatchFailureStreakGoesStale: the API server answers
// lists and the cache matches, but the Workspace watch keeps failing — a
// streak older than maxObservedStale is stale; a delivered event ends it at
// once, otherwise it ends WatchRetryWindow after the last report.
func TestK8sStatusView_WatchFailureStreakGoesStale(t *testing.T) {
	sv, clock, api := healthView(t)
	ctx := context.Background()
	fail := errors.New(`failed to list *v1.Workspace: workspaces is forbidden`)
	tick := func() {
		*clock = clock.Add(5 * time.Second)
		sv.livenessTick(ctx, api.list, 5*time.Second)
	}

	sv.NoteWatchError(watchhealth.WorkspaceTypeDescription, fail)
	tick()
	tick()
	if !countsFresh(t, sv) {
		t.Fatal("a 10 s old failure already stale: transient blips must be tolerated")
	}
	sv.NoteWatchError(watchhealth.WorkspaceTypeDescription, fail)
	tick()
	tick()
	if countsFresh(t, sv) {
		t.Fatal("watch failing for 20 s still fresh")
	}
	sv.watch.Delivered() // the informer delivered an event: the watch is back
	if !countsFresh(t, sv) {
		t.Fatal("a delivered event did not end the streak")
	}

	sv.NoteWatchError(watchhealth.WorkspaceTypeDescription, fail)
	for i := 0; i < 4; i++ {
		tick()
	}
	if countsFresh(t, sv) {
		t.Fatal("watch failing for 20 s still fresh")
	}
	for i := 0; i < 13; i++ { // no further report for > WatchRetryWindow
		tick()
	}
	if !countsFresh(t, sv) {
		t.Fatal("streak did not end after WatchRetryWindow without a report")
	}
}

// TestK8sStatusView_BenignAndForeignWatchErrorsIgnored: routine closes
// (EOF, expired resource version) and failures of other informers in the
// shared cache do not start a streak.
func TestK8sStatusView_BenignAndForeignWatchErrorsIgnored(t *testing.T) {
	sv, clock, api := healthView(t)
	ctx := context.Background()
	gr := schema.GroupResource{Group: cdiv1.GroupVersion.Group, Resource: "workspaces"}
	td := watchhealth.WorkspaceTypeDescription
	for i := 0; i < 6; i++ {
		sv.NoteWatchError(td, io.EOF)
		sv.NoteWatchError(td, io.ErrUnexpectedEOF)
		sv.NoteWatchError(td, apierrors.NewResourceExpired("too old resource version"))
		sv.NoteWatchError(td, apierrors.NewGone("gone"))
		sv.NoteWatchError("*v1.WorkspaceTemplate", apierrors.NewForbidden(gr, "", errors.New("no")))
		*clock = clock.Add(5 * time.Second)
		sv.livenessTick(ctx, api.list, 5*time.Second)
	}
	if !countsFresh(t, sv) {
		t.Fatal("benign or foreign watch errors made the view stale")
	}
	if td != "*v1.Workspace" {
		t.Fatalf("WorkspaceTypeDescription = %q, want the reflector's *v1.Workspace", td)
	}
}
