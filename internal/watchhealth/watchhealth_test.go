package watchhealth

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/util/flowcontrol"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	cdiv1 "github.com/tinyorbitvn/tinycdi/api/v1"
)

// TestNewVersion_ResyncIsNoEvent: a periodic resync replays the store with
// an unchanged resource version and keeps firing while the watch is down, so
// only a new version counts as a delivered event.
func TestNewVersion_ResyncIsNoEvent(t *testing.T) {
	a := &cdiv1.Workspace{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "41"}}
	b := &cdiv1.Workspace{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "42"}}
	if NewVersion(a, a.DeepCopy()) {
		t.Fatal("resync replay counted as a delivered event")
	}
	if !NewVersion(a, b) {
		t.Fatal("new object version not counted as a delivered event")
	}
}

// TestWorkspaceMetadataVersions: the probe's API side lists metadata per
// watched namespace (or cluster-wide), keys by namespace/name, and surfaces
// a failing API read.
func TestWorkspaceMetadataVersions(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := cdiv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	ws := func(ns, name string) *cdiv1.Workspace {
		return &cdiv1.Workspace{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(ws("ns-a", "one"), ws("ns-b", "two"), ws("ns-c", "three")).Build()
	ctx := context.Background()

	all, err := WorkspaceMetadataVersions(c, nil)(ctx)
	if err != nil || len(all) != 3 || all["ns-c/three"] == "" {
		t.Fatalf("cluster-wide = %v, %v; want 3 versioned entries", all, err)
	}
	some, err := WorkspaceMetadataVersions(c, []string{"ns-a", "ns-b"})(ctx)
	if err != nil || len(some) != 2 || some["ns-a/one"] != all["ns-a/one"] {
		t.Fatalf("namespaced = %v, %v; want ns-a and ns-b only", some, err)
	}
	broken := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return errors.New("connection refused")
		},
	}).Build()
	if _, err := WorkspaceMetadataVersions(broken, []string{"ns-a"})(ctx); err == nil {
		t.Fatal("probe hid a failing list")
	}
}

// metaReader is a fake uncached reader serving Workspace metadata per
// namespace. It honors Limit/Continue, can wait on a client-side rate
// limiter or sleep per call, and records peak concurrency.
type metaReader struct {
	byNS    map[string][]string // namespace -> names
	rv      string
	limiter flowcontrol.RateLimiter
	delay   time.Duration

	mu        sync.Mutex
	calls     int
	inFlight  int
	maxFlight int
}

func (r *metaReader) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return errors.New("not supported")
}

func (r *metaReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	r.mu.Lock()
	r.calls++
	r.inFlight++
	if r.inFlight > r.maxFlight {
		r.maxFlight = r.inFlight
	}
	r.mu.Unlock()
	defer func() { r.mu.Lock(); r.inFlight--; r.mu.Unlock() }()
	if r.limiter != nil {
		if err := r.limiter.Wait(ctx); err != nil {
			return err
		}
	}
	if r.delay > 0 {
		select {
		case <-time.After(r.delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	lo := (&client.ListOptions{}).ApplyOptions(opts)
	l := list.(*metav1.PartialObjectMetadataList)
	names := r.byNS[lo.Namespace]
	start := 0
	if lo.Continue != "" {
		start, _ = strconv.Atoi(lo.Continue)
	}
	end := len(names)
	if lo.Limit > 0 && int64(end-start) > lo.Limit {
		end = start + int(lo.Limit)
	}
	l.Items = nil
	for _, n := range names[start:end] {
		l.Items = append(l.Items, metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{
			Namespace: lo.Namespace, Name: n, ResourceVersion: r.rv}})
	}
	l.Continue = ""
	if end < len(names) {
		l.Continue = strconv.Itoa(end)
	}
	return nil
}

func namespaces(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("tenant-%02d", i)
	}
	return out
}

// TestWorkspaceMetadataVersions_Paging: a namespace larger than one page is
// read to the end through Continue.
func TestWorkspaceMetadataVersions_Paging(t *testing.T) {
	old := probePageSize
	probePageSize = 3
	t.Cleanup(func() { probePageSize = old })
	r := &metaReader{byNS: map[string][]string{"ns-a": {"a", "b", "c", "d", "e", "f", "g"}}, rv: "5"}
	got, err := WorkspaceMetadataVersions(r, []string{"ns-a"})(context.Background())
	if err != nil || len(got) != 7 || got["ns-a/g"] != "5" {
		t.Fatalf("paged versions = %v, %v; want all 7", got, err)
	}
	if r.calls != 3 {
		t.Fatalf("list calls = %d, want 3 pages", r.calls)
	}
}

// emptyCache is an informer cache with no Workspaces.
type emptyCache struct{ client.Reader }

func (emptyCache) List(_ context.Context, list client.ObjectList, _ ...client.ListOption) error {
	list.(*cdiv1.WorkspaceList).Items = nil
	return nil
}

// TestProbe_ManyNamespacesBehindALimiter: 30 namespaces behind a
// client-side rate limiter. On client-go's default limiter (QPS 5, burst 10)
// the lists cannot finish inside the probe timeout, so the probe fails and
// the tracker goes stale; on the backend's probe limiter (QPS 50, burst 100)
// they finish with at most ProbeParallelism in flight and the tracker stays
// fresh. Real time, timeout scaled down to 1 s.
func TestProbe_ManyNamespacesBehindALimiter(t *testing.T) {
	const timeout = time.Second
	run := func(qps float32, burst int) (bool, *metaReader) {
		byNS := map[string][]string{}
		for _, ns := range namespaces(30) {
			byNS[ns] = nil
		}
		r := &metaReader{byNS: byNS, limiter: flowcontrol.NewTokenBucketRateLimiter(qps, burst), delay: 10 * time.Millisecond}
		var tr Tracker
		synced := time.Now()
		tr.MarkSynced(synced)
		tr.Probe(context.Background(), time.Now, WorkspaceMetadataVersions(r, namespaces(30)), emptyCache{}, 5*time.Second, timeout)
		// Fresh only if this probe proved the cache current: judge at a
		// moment past the sync stamp's budget but inside the probe's.
		_, last := tr.Health(time.Now(), time.Hour)
		return last.After(synced), r
	}
	if ok, _ := run(5, 10); ok { // kc's client-go default
		t.Fatal("probe behind client-go's default limiter succeeded; the test does not model the outage")
	}
	ok, r := run(50, 100)
	if !ok {
		t.Fatal("probe behind the backend's probe limiter failed")
	}
	if r.maxFlight > ProbeParallelism {
		t.Fatalf("peak in-flight lists = %d, want <= %d", r.maxFlight, ProbeParallelism)
	}
}

// TestProbe_SlowListWithinBudget: a list slower than the probe interval but
// inside ProbeTimeout still proves freshness; one that overruns it does not.
func TestProbe_SlowListWithinBudget(t *testing.T) {
	const every, maxStale = 50 * time.Millisecond, 250 * time.Millisecond // scaled 5 s / 15 s
	probe := func(delay time.Duration) bool {
		r := &metaReader{byNS: map[string][]string{"ns-a": nil}, delay: delay}
		var tr Tracker
		synced := time.Now()
		tr.MarkSynced(synced)
		tr.Probe(context.Background(), time.Now, WorkspaceMetadataVersions(r, []string{"ns-a"}), emptyCache{}, every, ProbeTimeout(every, maxStale))
		_, last := tr.Health(time.Now(), time.Hour)
		return last.After(synced)
	}
	if !probe(90 * time.Millisecond) { // > interval, < ProbeTimeout (125 ms)
		t.Fatal("a list slower than the interval but inside the budget failed the probe")
	}
	if probe(300 * time.Millisecond) {
		t.Fatal("a list overrunning the budget still proved freshness")
	}
}

// fakeTime is a manual clock.
type fakeTime struct{ t time.Time }

func (c *fakeTime) now() time.Time { return c.t }

// staticVersions answers fixed API-server versions, optionally advancing the
// clock by a per-call list latency, or failing.
func staticVersions(clk *fakeTime, want map[string]string, latency *[]time.Duration, fail *bool) Versions {
	return func(context.Context) (map[string]string, error) {
		if latency != nil && len(*latency) > 0 {
			clk.t = clk.t.Add((*latency)[0])
			*latency = (*latency)[1:]
		}
		if fail != nil && *fail {
			return nil, errors.New("connection refused")
		}
		return want, nil
	}
}

// oneCache is an informer cache holding one Workspace at a fixed version.
type oneCache struct {
	client.Reader
	rv string
}

func (c oneCache) List(_ context.Context, list client.ObjectList, _ ...client.ListOption) error {
	list.(*cdiv1.WorkspaceList).Items = []cdiv1.Workspace{{ObjectMeta: metav1.ObjectMeta{
		Namespace: "ns-a", Name: "ws", ResourceVersion: c.rv}}}
	return nil
}

// TestProbe_UnevenListLatency: drift is timed from each probe's start, so a
// slow first list followed by a fast one cannot postpone detection: the
// second probe (one interval later) already reports stale.
func TestProbe_UnevenListLatency(t *testing.T) {
	const every, maxStale = 5 * time.Second, 15 * time.Second
	clk := &fakeTime{t: time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)}
	var tr Tracker
	tr.MarkSynced(clk.now())
	latency := []time.Duration{4 * time.Second, 0}
	api := staticVersions(clk, map[string]string{"ns-a/ws": "2"}, &latency, nil) // the cache never sees rv 2
	cache := oneCache{rv: "1"}

	tick0 := clk.t
	clk.t = tick0.Add(every)
	tr.Probe(context.Background(), clk.now, api, cache, every, maxStale-every) // list takes 4 s
	clk.t = tick0.Add(2 * every)
	tr.Probe(context.Background(), clk.now, api, cache, every, maxStale-every) // list is instant
	if fresh, _ := tr.Health(clk.now(), maxStale); fresh {
		t.Fatal("drift missed for one more interval because the first list was slow")
	}
}

// TestProbe_PendingSurvivesFailedProbe: a difference seen before a failed
// probe keeps its first-seen time, so the failure cannot restart its grace.
func TestProbe_PendingSurvivesFailedProbe(t *testing.T) {
	const every, maxStale = 5 * time.Second, 15 * time.Second
	clk := &fakeTime{t: time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)}
	var tr Tracker
	tr.MarkSynced(clk.now())
	fail := false
	api := staticVersions(clk, map[string]string{"ns-a/ws": "2"}, nil, &fail)
	cache := oneCache{rv: "1"}

	clk.t = clk.t.Add(every)
	tr.Probe(context.Background(), clk.now, api, cache, every, maxStale-every) // difference first seen
	fail = true
	clk.t = clk.t.Add(every)
	tr.Probe(context.Background(), clk.now, api, cache, every, maxStale-every) // API unreachable
	fail = false
	clk.t = clk.t.Add(every)
	tr.Probe(context.Background(), clk.now, api, cache, every, maxStale-every) // still different
	if fresh, _ := tr.Health(clk.now(), maxStale); fresh {
		t.Fatal("a failed probe restarted the grace of a pending difference")
	}
}

// TestSyncedWhen_HandlerRegistration: the tracker is synced — and fresh,
// stamped now — as soon as its Workspace handler registration has the
// initial list, without waiting for MarkSynced (every informer of the
// cache). A fresh replica serves connects once its bindings are known.
func TestSyncedWhen_HandlerRegistration(t *testing.T) {
	var tr Tracker
	listed := false
	tr.SyncedWhen(func() bool { return listed })
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	if fresh, _ := tr.Health(now, 15*time.Second); fresh {
		t.Fatal("fresh before the initial list")
	}
	listed = true
	fresh, last := tr.Health(now, 15*time.Second)
	if !fresh || !last.Equal(now) {
		t.Fatalf("after the initial list: fresh=%v last=%v, want fresh at %v", fresh, last, now)
	}
	if fresh, _ := tr.Health(now.Add(16*time.Second), 15*time.Second); fresh {
		t.Fatal("the sync stamp never ages: still fresh past the bound without a probe")
	}
}

// TestRun_SteadyStateSlowListNeverFlaps runs the real probe loop (scaled:
// interval 100 ms, bound 300 ms, so ProbeTimeout 150 ms) against a list
// that always succeeds but slowly, and samples Health throughout. A list
// under the timeout is fresh at every sample. A list over it — which the
// old maxStale − interval timeout (200 ms) accepted, letting the heartbeat
// age to 2d > maxStale between probes — now fails every probe: stale once
// the sync stamp ages out, at every later sample, never flapping back.
func TestRun_SteadyStateSlowListNeverFlaps(t *testing.T) {
	const every, maxStale = 100 * time.Millisecond, 300 * time.Millisecond
	if got := ProbeTimeout(every, maxStale); got != 150*time.Millisecond {
		t.Fatalf("ProbeTimeout = %v, want maxStale/2 = 150ms", got)
	}
	if got := ProbeTimeout(5*time.Second, 15*time.Second); got != 7500*time.Millisecond {
		t.Fatalf("production ProbeTimeout = %v, want 7.5s", got)
	}
	sample := func(delay time.Duration) (fresh, stale int) {
		r := &metaReader{byNS: map[string][]string{"ns-a": nil}, delay: delay}
		var tr Tracker
		tr.MarkSynced(time.Now())
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			tr.Run(ctx, time.Now, WorkspaceMetadataVersions(r, []string{"ns-a"}), emptyCache{}, every, maxStale)
		}()
		deadline := time.Now().Add(1500 * time.Millisecond)
		settle := time.Now().Add(maxStale + 50*time.Millisecond) // past the sync stamp
		for time.Now().Before(deadline) {
			ok, _ := tr.Health(time.Now(), maxStale)
			switch {
			case time.Now().Before(settle):
			case ok:
				fresh++
			default:
				stale++
			}
			time.Sleep(3 * time.Millisecond)
		}
		cancel()
		<-done
		return fresh, stale
	}
	if fresh, stale := sample(110 * time.Millisecond); stale != 0 || fresh == 0 {
		t.Fatalf("list under the timeout: %d stale of %d samples, want none", stale, fresh+stale)
	}
	if fresh, stale := sample(180 * time.Millisecond); fresh != 0 || stale == 0 {
		t.Fatalf("list over the timeout: %d fresh of %d samples after the sync stamp aged out, want none (no flapping)", fresh, fresh+stale)
	}
}
