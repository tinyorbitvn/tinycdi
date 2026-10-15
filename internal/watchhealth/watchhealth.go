// Package watchhealth decides whether the Workspace informer cache can be
// trusted: the broker's binding source and the API's status view share it.
//
// Fresh means "the cache matches the API server", not "an event arrived
// recently". A Tracker is fresh while
//
//   - the Workspace informer has synced (SyncedWhen / MarkSynced);
//   - a consistency probe (Probe) succeeded within the caller's bound: an
//     uncached metadata-only list of the watched Workspaces agrees with the
//     informer cache on membership and every resourceVersion, apart from
//     differences still inside their grace. An idle but healthy watch
//     therefore stays fresh, and informer events, resync replays included,
//     are never evidence on their own (a resync keeps replaying the local
//     store while the watch is down);
//   - no difference has outlived its grace: a Workspace whose API-server
//     version differs from the cache (changed, created or deleted) while the
//     cache copy stays unchanged for a full probe interval means
//     the watch is not delivering — a silent stall, a watch client-go
//     retries after a refusal or 429, or one namespace's watch stuck while
//     others flow. The cache moving on (a delivered event) restarts the
//     grace;
//   - no watch failure streak (NoteWatchError, fed by the cache's watch
//     error handler) has lasted longer than the bound. A streak ends on the
//     next delivered event or WatchRetryWindow after its last report.
//
// Timing: a probe's clock reading is taken when it starts, so drift is
// measured tick to tick and list latency cannot postpone it. With a 5 s
// probe and a 15 s bound, a change the watch never delivers is stale once
// the next probe still sees it: within 10 s of the change plus one list
// latency, inside the 15 s bound. A probe may take up to ProbeTimeout
// (7.5 s: half the bound, so steady-state freshness never flaps) before it
// counts as failed.
//
// Cost per probe: one paged metadata-only list per watched namespace
// (cluster-wide when none), at most ProbeParallelism in flight, returning
// every Workspace's metadata — no spec/status, but with managedFields, so
// several KiB per object — plus one no-copy read of the informer cache:
// linear in the Workspace count, every interval. The backend runs one
// Tracker for the broker and the API together.
//
// Scope: one Tracker covers every watched namespace. A namespace whose list
// is forbidden or whose watch is stuck makes the whole cache stale, and a
// delivered event in any namespace ends a watch failure streak for all
// (client-go reports failures per informer, and controller-runtime does not
// expose which namespace's reflector failed).
package watchhealth

import (
	"context"
	"errors"
	"io"
	"reflect"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	toolscache "k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cdiv1 "github.com/tinyorbitvn/tinycdi/api/v1"
)

// WatchRetryWindow is how long one reported watch failure keeps the tracker
// suspect. A reflector that is still failing re-reports at least every
// [30, 60) s (client-go's backoff cap), so a failure older than this with no
// successor means the watch re-established.
const WatchRetryWindow = time.Minute

// WorkspaceTypeDescription is the reflector type description of the
// Workspace informer, used to pick its failures out of the shared cache's
// watch error handler.
var WorkspaceTypeDescription = reflect.TypeOf(&cdiv1.Workspace{}).String()

// Versions lists Workspaces as "namespace/name" -> resourceVersion.
type Versions func(context.Context) (map[string]string, error)

// Tracker is the freshness state of one Workspace informer. The zero value
// is an unsynced tracker; every method takes the caller's clock reading.
type Tracker struct {
	mu        sync.Mutex
	synced    bool
	syncedFn  func() bool // SyncedWhen: the Workspace handler has the initial list
	last      time.Time   // last proof the cache matched the API server
	drifted   bool        // the last probe found a difference past its grace
	pending   map[string]driftEntry
	failSince time.Time // first watch failure of the current streak; zero when none
	failLast  time.Time // latest watch failure of the current streak
}

// driftEntry is one Workspace on which the cache and the API server
// disagreed: the cache's version at that moment ("" when absent) and when the
// disagreement with that cache version was first seen.
type driftEntry struct {
	cacheRV string
	since   time.Time
}

// MarkSynced records that the informer cache has completed its initial sync
// (a full list: the cache matches the API server at this moment).
func (t *Tracker) MarkSynced(now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.synced = true
	t.last = now
}

// SyncedWhen lets the tracker count as synced as soon as hasSynced reports
// true — the registration of its event handler on the Workspace informer
// (ResourceEventHandlerRegistration.HasSynced): the initial list is then a
// full, current copy. Without it only MarkSynced syncs the tracker, which
// waits for every informer of the cache.
func (t *Tracker) SyncedWhen(hasSynced func() bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.syncedFn = hasSynced
}

// syncedLocked reports whether the tracker has synced, marking it synced at
// now when the SyncedWhen condition first holds. t.mu must be held.
func (t *Tracker) syncedLocked(now time.Time) bool {
	if !t.synced && t.syncedFn != nil && t.syncedFn() {
		t.synced = true
		if now.After(t.last) {
			t.last = now
		}
	}
	return t.synced
}

// NewVersion reports whether an informer update carries a new object
// version; a resync replay does not.
func NewVersion(oldObj, newObj interface{}) bool {
	o, ok1 := oldObj.(metav1.Object)
	n, ok2 := newObj.(metav1.Object)
	return !ok1 || !ok2 || o.GetResourceVersion() != n.GetResourceVersion()
}

// Delivered records a delivered watch event (an add, a delete or an update
// for which NewVersion holds), which ends any watch failure streak — for
// every namespace, since streaks are not tracked per namespace. It is not a
// freshness heartbeat: only a matching probe is.
func (t *Tracker) Delivered() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.failSince, t.failLast = time.Time{}, time.Time{}
}

// Handler returns the informer callbacks that feed Delivered: adds,
// deletes and updates carrying a new object version (not resync replays).
func (t *Tracker) Handler() toolscache.ResourceEventHandlerFuncs {
	return toolscache.ResourceEventHandlerFuncs{
		AddFunc: func(interface{}) { t.Delivered() },
		UpdateFunc: func(oldObj, newObj interface{}) {
			if NewVersion(oldObj, newObj) {
				t.Delivered()
			}
		},
		DeleteFunc: func(interface{}) { t.Delivered() },
	}
}

// NoteWatchError records a list/watch failure reported by the cache's watch
// error handler for the informer of typeDescription (the reflector's
// TypeDescription). Failures of other informers are ignored, and so are the
// routine closes client-go itself treats as benign (EOF, an expired resource
// version): the reflector resumes from them without a gap.
func (t *Tracker) NoteWatchError(now time.Time, typeDescription string, err error) {
	if typeDescription != WorkspaceTypeDescription ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		apierrors.IsResourceExpired(err) || apierrors.IsGone(err) {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.failSince.IsZero() || now.Sub(t.failLast) > WatchRetryWindow {
		t.failSince = now
	}
	t.failLast = now
}

// Probe runs one consistency probe: list the API server through api, then
// the informer cache through cache, and compare. The clock is read once, at
// the start, and stamps both the heartbeat and any new difference. A
// difference is in flight until the cache copy has stayed the same for
// grace (one probe interval) while the API server disagrees; then the
// tracker is stale. The lists must finish within timeout; a failed or
// timed-out probe records nothing (pending differences carry over), so an
// unreachable API server goes stale by age.
func (t *Tracker) Probe(ctx context.Context, now func() time.Time, api Versions, cache client.Reader, grace, timeout time.Duration) {
	start := now()
	t.mu.Lock()
	synced := t.syncedLocked(start)
	t.mu.Unlock()
	if !synced {
		return
	}
	pctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	want, err := api(pctx)
	if err != nil {
		return
	}
	// Listed after the API server, so a change that reached the cache in
	// between shows as the cache ahead — in flight, not drift.
	var cached cdiv1.WorkspaceList
	if err := cache.List(pctx, &cached, client.UnsafeDisableDeepCopyOption(true)); err != nil {
		return
	}
	have := make(map[string]string, len(cached.Items))
	for i := range cached.Items {
		ws := &cached.Items[i]
		have[ws.Namespace+"/"+ws.Name] = ws.ResourceVersion
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	pending := map[string]driftEntry{}
	drifted := false
	check := func(key string) {
		if want[key] == have[key] {
			return
		}
		e, ok := t.pending[key]
		if !ok || e.cacheRV != have[key] {
			e = driftEntry{cacheRV: have[key], since: start}
		}
		pending[key] = e
		if start.Sub(e.since) >= grace {
			drifted = true
		}
	}
	for key := range want {
		check(key)
	}
	for key := range have {
		if _, ok := want[key]; !ok {
			check(key)
		}
	}
	t.pending, t.drifted = pending, drifted
	if !drifted && start.After(t.last) {
		t.last = start
	}
}

// ProbeTimeout is how long one probe's lists may take:
// min(maxStale − every, maxStale/2), at least every. The heartbeat is the
// start of the last good probe and lands when it ends, and the next probe
// starts no earlier than the previous one ended (the ticker holds one
// tick), so in steady state the heartbeat's age peaks at max(every, d) + d
// for a list taking d. Capping d at maxStale/2 keeps that peak within
// maxStale: a slow list either proves freshness on every cycle or fails
// outright — it never flaps between fresh and stale.
func ProbeTimeout(every, maxStale time.Duration) time.Duration {
	d := min(maxStale-every, maxStale/2)
	return max(d, every)
}

// Run calls Probe every interval until ctx ends, with grace one interval
// and ProbeTimeout: a slow list still proves freshness as long as it
// finishes inside the timeout.
func (t *Tracker) Run(ctx context.Context, now func() time.Time, api Versions, cache client.Reader, every, maxStale time.Duration) {
	timeout := ProbeTimeout(every, maxStale)
	tk := time.NewTicker(every)
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
		}
		t.Probe(ctx, now, api, cache, every, timeout)
	}
}

// Health reports whether the cache is fresh within maxStale (see the
// package comment) and the time it was last shown to match the API server.
func (t *Tracker) Health(now time.Time, maxStale time.Duration) (bool, time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.syncedLocked(now) || t.drifted || now.Sub(t.last) > maxStale {
		return false, t.last
	}
	if !t.failSince.IsZero() && now.Sub(t.failLast) <= WatchRetryWindow && now.Sub(t.failSince) > maxStale {
		return false, t.last
	}
	return true, t.last
}

// ProbeParallelism bounds the namespace lists one probe has in flight.
const ProbeParallelism = 4

// probePageSize is the page size of the metadata lists.
var probePageSize int64 = 500

// WorkspaceMetadataVersions returns the probe's API-server side: a paged,
// metadata-only list of Workspaces in each namespace (cluster-wide when
// namespaces is empty), at most ProbeParallelism namespaces at a time,
// through reader, which must be an uncached client with its own rate limit
// (see the backend wiring).
func WorkspaceMetadataVersions(reader client.Reader, namespaces []string) Versions {
	return func(ctx context.Context) (map[string]string, error) {
		scopes := namespaces
		if len(scopes) == 0 {
			scopes = []string{""}
		}
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		var (
			mu       sync.Mutex
			out      = map[string]string{}
			firstErr error
			wg       sync.WaitGroup
			sem      = make(chan struct{}, ProbeParallelism)
		)
		for _, ns := range scopes {
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
			}
			if ctx.Err() != nil {
				break
			}
			wg.Add(1)
			go func(ns string) {
				defer func() { <-sem; wg.Done() }()
				got, err := listNamespace(ctx, reader, ns)
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					if firstErr == nil {
						firstErr = err
						cancel()
					}
					return
				}
				for k, v := range got {
					out[k] = v
				}
			}(ns)
		}
		wg.Wait()
		if firstErr == nil {
			firstErr = ctx.Err()
		}
		if firstErr != nil {
			return nil, firstErr
		}
		return out, nil
	}
}

// listNamespace pages through the Workspace metadata of one namespace ("" =
// cluster-wide).
func listNamespace(ctx context.Context, reader client.Reader, ns string) (map[string]string, error) {
	out := map[string]string{}
	cont := ""
	for {
		list := &metav1.PartialObjectMetadataList{}
		list.SetGroupVersionKind(cdiv1.GroupVersion.WithKind("WorkspaceList"))
		opts := []client.ListOption{client.Limit(probePageSize), client.Continue(cont)}
		if ns != "" {
			opts = append(opts, client.InNamespace(ns))
		}
		if err := reader.List(ctx, list, opts...); err != nil {
			return nil, err
		}
		for i := range list.Items {
			m := &list.Items[i]
			out[m.Namespace+"/"+m.Name] = m.ResourceVersion
		}
		if cont = list.Continue; cont == "" {
			return out, nil
		}
	}
}
