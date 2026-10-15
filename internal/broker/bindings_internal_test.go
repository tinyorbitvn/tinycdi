package broker

// Freshness-gate unit tests for K8sBindingSource — no envtest needed: the
// gate short-circuits before the cache is touched, and a fake cache answers
// the one List a fresh lookup makes.

import (
	"context"
	"errors"
	"fmt"
	"syscall"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	crcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cdiv1 "github.com/tinyorbitvn/tinycdi/api/v1"
	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
	"github.com/tinyorbitvn/tinycdi/internal/watchhealth"
)

func TestK8sBindingSource_FreshnessGate(t *testing.T) {
	src := &K8sBindingSource{}
	if _, err := src.CurrentBinding(context.Background(), "ws-1"); !errors.Is(err, ErrFreshness) {
		t.Fatalf("unsynced source = %v, want ErrFreshness", err)
	}

	clk := &fakeClock{t: time.Now()}
	src.now = clk.Now
	src.MarkSynced()
	clk.Advance(MaxBindingAge + time.Second)
	if _, err := src.CurrentBinding(context.Background(), "ws-1"); !errors.Is(err, ErrFreshness) {
		t.Fatalf("stale source = %v, want ErrFreshness", err)
	}
}

// workspaceCache is a fake informer cache holding Workspaces by
// "namespace/name"; List answers those carrying testWS, or every one when
// asked without a label selector (the probe's read).
type workspaceCache struct {
	crcache.Cache
	items map[string]*cdiv1.Workspace
}

func (c *workspaceCache) List(_ context.Context, list client.ObjectList, opts ...client.ListOption) error {
	l, ok := list.(*cdiv1.WorkspaceList)
	if !ok {
		return fmt.Errorf("unexpected list %T", list)
	}
	lo := (&client.ListOptions{}).ApplyOptions(opts)
	l.Items = nil
	for _, ws := range c.items {
		if lo.LabelSelector == nil || ws.Labels[provisioning.LabelWorkspaceUID] == string(testWS) {
			l.Items = append(l.Items, *ws)
		}
	}
	return nil
}

type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time          { return c.t }
func (c *fakeClock) Advance(d time.Duration) { c.t = c.t.Add(d) }

const (
	testWS  PlatformID = "ws_fresh01"
	testKey            = "tenant-a/ws-fresh01"
	every              = 5 * time.Second // production probe interval
)

func cachedWorkspace(rv string) *cdiv1.Workspace {
	return &cdiv1.Workspace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ws-fresh01", Namespace: "tenant-a", UID: "cr-1", ResourceVersion: rv,
			Labels: map[string]string{
				provisioning.LabelWorkspaceUID: string(testWS),
				provisioning.LabelTenant:       "tenant-a",
			},
		},
		Spec: cdiv1.WorkspaceSpec{OwnerSubject: cdiv1.OwnerSubject{Issuer: "https://idp", Subject: "alice"}},
		Status: cdiv1.WorkspaceStatus{
			Phase: cdiv1.WorkspacePhaseReady, ObservedRuntimeGeneration: 1, RuntimeUID: "rt-1",
		},
	}
}

// probeEnv is a synced source over a fake cache, a broker reading through
// it, the fake clock and the API server's view the probe lists.
type probeEnv struct {
	src   *K8sBindingSource
	b     *Broker
	clk   *fakeClock
	cache *workspaceCache
	api   map[string]string // namespace/name -> resourceVersion on the API server
	down  bool              // the API server refuses connections
}

func newProbeEnv(t *testing.T) *probeEnv {
	t.Helper()
	clk := &fakeClock{t: time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)}
	cache := &workspaceCache{items: map[string]*cdiv1.Workspace{testKey: cachedWorkspace("100")}}
	src := &K8sBindingSource{cache: cache, now: clk.Now}
	src.MarkSynced()
	return &probeEnv{src: src, b: New(nil, src, WithClock(clk)), clk: clk, cache: cache,
		api: map[string]string{testKey: "100"}}
}

func (e *probeEnv) versions(context.Context) (map[string]string, error) {
	if e.down {
		return nil, fmt.Errorf("dial tcp 10.0.0.1:6443: %w", syscall.ECONNREFUSED)
	}
	out := make(map[string]string, len(e.api))
	for k, v := range e.api {
		out[k] = v
	}
	return out, nil
}

// tick advances one probe interval, replays the cache like a resync does,
// and runs one probe.
func (e *probeEnv) tick() {
	e.clk.Advance(every)
	for _, ws := range e.cache.items {
		e.src.onUpdate(ws, ws)
	}
	e.src.livenessTick(context.Background(), e.versions, every)
}

func (e *probeEnv) fresh(t *testing.T, what string) {
	t.Helper()
	got, err := e.b.currentBinding(context.Background(), testWS, e.clk.Now())
	if err != nil {
		t.Fatalf("%s: currentBinding = %v, want a binding", what, err)
	}
	if e.clk.Now().Sub(got.ObservedAt) > every {
		t.Fatalf("%s: ObservedAt %v is older than one probe interval", what, got.ObservedAt)
	}
}

func (e *probeEnv) stale(t *testing.T, what string) {
	t.Helper()
	if _, err := e.src.CurrentBinding(context.Background(), testWS); !errors.Is(err, ErrFreshness) {
		t.Fatalf("%s: CurrentBinding = %v, want ErrFreshness", what, err)
	}
	if _, err := e.b.currentBinding(context.Background(), testWS, e.clk.Now()); !errors.Is(err, ErrFreshness) {
		t.Fatalf("%s: broker currentBinding = %v, want ErrFreshness", what, err)
	}
	if err := e.b.RequestStop(context.Background(), testWS, 1, "idle"); !errors.Is(err, ErrFreshness) {
		t.Fatalf("%s: RequestStop = %v, want ErrFreshness", what, err)
	}
}

// TestK8sBindingSource_IdleHealthyWatchStaysFresh: a tenant whose
// Workspaces never change emits no watch events (and resync replays do not
// count); the probe finds the cache matching the API server, so the broker
// keeps serving well past MaxBindingAge.
func TestK8sBindingSource_IdleHealthyWatchStaysFresh(t *testing.T) {
	e := newProbeEnv(t)
	for i := 0; i < 12; i++ { // one minute
		e.tick()
	}
	e.fresh(t, "idle healthy watch")
}

// TestK8sBindingSource_SilentStalledWatchGoesStale: the watch stops
// delivering without reporting anything (a silent stall, or client-go
// retrying a refused/429 watch) while lists still succeed and resync keeps
// replaying. A change on the API server the cache never sees — an update, a
// deletion, a new Workspace — goes stale within MaxBindingAge of the change,
// and the broker refuses; once the cache catches up it is fresh again.
func TestK8sBindingSource_SilentStalledWatchGoesStale(t *testing.T) {
	cases := map[string]func(e *probeEnv){
		"updated": func(e *probeEnv) { e.api[testKey] = "101" },
		"deleted": func(e *probeEnv) { delete(e.api, testKey) },
		"created": func(e *probeEnv) { e.api["tenant-a/ws-other"] = "102" },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			e := newProbeEnv(t)
			e.tick()
			e.fresh(t, "before the change")
			change(e)
			changedAt := e.clk.Now()
			for e.clk.Now().Sub(changedAt) < MaxBindingAge {
				e.tick()
			}
			e.stale(t, "stalled watch "+name)

			// The watch recovers and delivers: the cache matches again.
			e.cache.items = map[string]*cdiv1.Workspace{}
			for k, rv := range e.api {
				ws := cachedWorkspace(rv)
				ws.Name = k[len("tenant-a/"):]
				if k != testKey {
					ws.Labels = map[string]string{provisioning.LabelWorkspaceUID: "ws_other"}
				}
				e.cache.items[k] = ws
			}
			if _, ok := e.api[testKey]; !ok {
				return // the bound Workspace is gone; nothing to serve
			}
			e.tick()
			e.fresh(t, "after the cache caught up")
		})
	}
}

// TestK8sBindingSource_InFlightChangeStaysFresh: a busy Workspace changes
// at every probe and the cache always trails the API server by one version,
// delivered within the probe interval. The cache keeps moving, so no
// difference outlives its grace and the source stays fresh.
func TestK8sBindingSource_InFlightChangeStaysFresh(t *testing.T) {
	e := newProbeEnv(t)
	for i := 101; i < 113; i++ { // one minute
		e.cache.items[testKey] = cachedWorkspace(e.api[testKey]) // the last change arrives
		e.api[testKey] = fmt.Sprint(i)                           // the next one is in flight
		e.tick()
		e.fresh(t, fmt.Sprintf("in-flight change to %d", i))
	}
}

// TestK8sBindingSource_UnreachableAPIServerGoesStale: the API server
// refuses connections; client-go retries the watch without reporting it
// while resync keeps replaying the cache. The probe cannot list, so the
// source goes stale past MaxBindingAge.
func TestK8sBindingSource_UnreachableAPIServerGoesStale(t *testing.T) {
	e := newProbeEnv(t)
	e.down = true
	for i := 0; i < 4; i++ { // 20 s > MaxBindingAge
		e.tick()
	}
	e.stale(t, "unreachable API server")
	e.down = false
	e.tick()
	e.fresh(t, "API server back")
}

// TestK8sBindingSource_WatchFailureStreak: the watch keeps failing while
// lists still succeed and the cache still matches. A non-benign failure
// streak longer than MaxBindingAge is stale; benign closes, other informers'
// failures and a streak whose last report is older than watchhealth.WatchRetryWindow are
// not.
func TestK8sBindingSource_WatchFailureStreak(t *testing.T) {
	e := newProbeEnv(t)
	forbidden := apierrors.NewForbidden(schema.GroupResource{Group: "workspaces.cdi.tinyorbit.vn", Resource: "workspaces"}, "", errors.New("watch denied"))

	e.src.NoteWatchError("*v1.Secret", forbidden)
	e.src.NoteWatchError(watchhealth.WorkspaceTypeDescription, apierrors.NewResourceExpired("too old"))
	for i := 0; i < 4; i++ {
		e.tick()
	}
	e.fresh(t, "benign/foreign watch errors")

	e.src.NoteWatchError(watchhealth.WorkspaceTypeDescription, forbidden)
	for i := 0; i < 4; i++ { // streak reaches 20 s > MaxBindingAge
		e.tick()
		e.src.NoteWatchError(watchhealth.WorkspaceTypeDescription, forbidden)
	}
	e.stale(t, "failing watch")

	// No new report for longer than watchhealth.WatchRetryWindow: the watch re-established.
	for d := time.Duration(0); d <= watchhealth.WatchRetryWindow; d += every {
		e.tick()
	}
	e.fresh(t, "after the streak lapsed")
}
