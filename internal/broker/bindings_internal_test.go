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
)

func TestK8sBindingSource_FreshnessGate(t *testing.T) {
	src := &K8sBindingSource{}
	if _, err := src.CurrentBinding(context.Background(), "ws-1"); !errors.Is(err, ErrFreshness) {
		t.Fatalf("unsynced source = %v, want ErrFreshness", err)
	}

	src.MarkSynced()
	src.lastEvent = time.Now().Add(-MaxBindingAge - time.Second)
	if _, err := src.CurrentBinding(context.Background(), "ws-1"); !errors.Is(err, ErrFreshness) {
		t.Fatalf("stale source = %v, want ErrFreshness", err)
	}
}

// oneWorkspaceCache is a cache whose List always answers one Ready
// Workspace — the cached copy a broken watch would leave behind.
type oneWorkspaceCache struct {
	crcache.Cache
	ws cdiv1.Workspace
}

func (c *oneWorkspaceCache) List(_ context.Context, list client.ObjectList, _ ...client.ListOption) error {
	l, ok := list.(*cdiv1.WorkspaceList)
	if !ok {
		return fmt.Errorf("unexpected list %T", list)
	}
	l.Items = []cdiv1.Workspace{c.ws}
	return nil
}

type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time          { return c.t }
func (c *fakeClock) Advance(d time.Duration) { c.t = c.t.Add(d) }

const testWS PlatformID = "ws_fresh01"

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

// syncedSource returns a synced source over a one-Workspace cache, a broker
// reading through it, and the shared fake clock.
func syncedSource(t *testing.T) (*K8sBindingSource, *Broker, *fakeClock) {
	t.Helper()
	clk := &fakeClock{t: time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)}
	src := &K8sBindingSource{cache: &oneWorkspaceCache{ws: *cachedWorkspace("100")}, now: clk.Now}
	src.MarkSynced()
	return src, New(nil, src, WithClock(clk)), clk
}

var (
	probeOK      = func(context.Context) error { return nil }
	probeRefused = func(context.Context) error {
		return fmt.Errorf("dial tcp 10.0.0.1:6443: %w", syscall.ECONNREFUSED)
	}
)

// TestK8sBindingSource_IdleHealthyWatchStaysFresh: a tenant whose
// Workspaces never change emits no watch events (and resync replays do not
// count); the uncached liveness probe keeps the healthy watch fresh, so the
// broker keeps serving well past MaxBindingAge.
func TestK8sBindingSource_IdleHealthyWatchStaysFresh(t *testing.T) {
	src, b, clk := syncedSource(t)
	const every = 5 * time.Second
	ws := cachedWorkspace("100")
	for i := 0; i < 12; i++ { // one minute of idle watch
		clk.Advance(every)
		src.onUpdate(ws, ws) // resync replay
		src.livenessTick(context.Background(), probeOK, every)
	}
	got, err := b.currentBinding(context.Background(), testWS, clk.Now())
	if err != nil {
		t.Fatalf("idle healthy watch: currentBinding = %v, want a binding", err)
	}
	if got.Phase != PhaseReady || got.RuntimeUID != "rt-1" || clk.Now().Sub(got.ObservedAt) > every {
		t.Fatalf("binding = %+v, want Ready rt-1 observed within %s", got, every)
	}
}

// TestK8sBindingSource_ResyncDoesNotMaskDeadWatch: the API server refuses
// connections; client-go retries the watch without reporting it while the
// 5 s resync keeps replaying the cached Workspace. The replays are not a
// heartbeat and the probe fails, so the source goes stale past
// MaxBindingAge and the broker refuses (issue, renew, target resolution and
// stop all go through currentBinding).
func TestK8sBindingSource_ResyncDoesNotMaskDeadWatch(t *testing.T) {
	src, b, clk := syncedSource(t)
	const every = 5 * time.Second
	ws := cachedWorkspace("100")
	for i := 0; i < 4; i++ { // 20 s > MaxBindingAge
		clk.Advance(every)
		src.onUpdate(ws, ws)
		src.livenessTick(context.Background(), probeRefused, every)
	}
	if _, err := src.CurrentBinding(context.Background(), testWS); !errors.Is(err, ErrFreshness) {
		t.Fatalf("dead watch with resync replays: CurrentBinding = %v, want ErrFreshness", err)
	}
	if _, err := b.currentBinding(context.Background(), testWS, clk.Now()); !errors.Is(err, ErrFreshness) {
		t.Fatalf("broker currentBinding = %v, want ErrFreshness", err)
	}
	if err := b.RequestStop(context.Background(), testWS, 1, "idle"); !errors.Is(err, ErrFreshness) {
		t.Fatalf("RequestStop on stale binding = %v, want ErrFreshness", err)
	}

	// A real watch event (new resource version) is fresh again.
	src.onUpdate(ws, cachedWorkspace("101"))
	if _, err := b.currentBinding(context.Background(), testWS, clk.Now()); err != nil {
		t.Fatalf("after a delivered event: currentBinding = %v, want a binding", err)
	}
}

// TestK8sBindingSource_WatchFailureStreak: the watch keeps failing while
// uncached lists still succeed (so the probe and the re-lists keep the
// heartbeat alive). A non-benign failure streak longer than MaxBindingAge is
// stale; benign closes, other informers' failures and a streak whose last
// report is older than watchRetryWindow are not.
func TestK8sBindingSource_WatchFailureStreak(t *testing.T) {
	src, b, clk := syncedSource(t)
	const every = 5 * time.Second
	forbidden := apierrors.NewForbidden(schema.GroupResource{Group: "cdi.tinyorbit.vn", Resource: "workspaces"}, "", errors.New("watch denied"))

	src.NoteWatchError("*v1.Secret", forbidden)
	src.NoteWatchError(workspaceTypeDescription, apierrors.NewResourceExpired("too old"))
	for i := 0; i < 4; i++ {
		clk.Advance(every)
		src.livenessTick(context.Background(), probeOK, every)
	}
	if _, err := b.currentBinding(context.Background(), testWS, clk.Now()); err != nil {
		t.Fatalf("benign/foreign watch errors: currentBinding = %v, want a binding", err)
	}

	src.NoteWatchError(workspaceTypeDescription, forbidden)
	for i := 0; i < 4; i++ { // streak reaches 20 s > MaxBindingAge
		clk.Advance(every)
		src.livenessTick(context.Background(), probeOK, every)
		src.NoteWatchError(workspaceTypeDescription, forbidden)
	}
	if _, err := b.currentBinding(context.Background(), testWS, clk.Now()); !errors.Is(err, ErrFreshness) {
		t.Fatalf("failing watch: currentBinding = %v, want ErrFreshness", err)
	}

	// No new report for longer than watchRetryWindow: the watch re-established.
	for d := time.Duration(0); d <= watchRetryWindow; d += every {
		clk.Advance(every)
		src.livenessTick(context.Background(), probeOK, every)
	}
	if _, err := b.currentBinding(context.Background(), testWS, clk.Now()); err != nil {
		t.Fatalf("after the streak lapsed: currentBinding = %v, want a binding", err)
	}
}
