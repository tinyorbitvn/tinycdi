// Package watchhealth decides whether the Workspace informer cache can be
// trusted: the broker's binding source and the API's status view share it.
//
// Fresh means "the cache matches the API server", not "an event arrived
// recently". A Tracker is fresh while
//
//   - the informer has synced;
//   - a consistency probe (Probe) succeeded within the caller's bound: an
//     uncached metadata-only list of the watched Workspaces agrees with the
//     informer cache on membership and every resourceVersion, apart from
//     differences still inside their grace. An idle but healthy watch
//     therefore stays fresh, and informer events, resync replays included,
//     are never evidence on their own (a resync keeps replaying the local
//     store while the watch is down);
//   - no difference has outlived its grace: a Workspace whose API-server
//     version differs from the cache (changed, created or deleted) while the
//     cache copy stays unchanged for longer than one probe interval means
//     the watch is not delivering — a silent stall, a watch client-go
//     retries after a refusal or 429, or one namespace's watch stuck while
//     others flow. The cache moving on (a delivered event) restarts the
//     grace;
//   - no watch failure streak (NoteWatchError, fed by the cache's watch
//     error handler) has lasted longer than the bound. A streak ends on the
//     next delivered event or WatchRetryWindow after its last report.
//
// With a 5 s probe and a 15 s bound, a change the watch never delivers is
// caught within 15 s. Cost per probe: one paged metadata-only list per
// watched namespace (cluster-wide when none) returning every Workspace's
// metadata (~1 KiB each, no spec/status), plus one no-copy read of the
// informer cache — linear in the Workspace count, every interval.
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
	last      time.Time // last proof the cache matched the API server
	drifted   bool      // the last probe found a difference past its grace
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

// Synced reports whether MarkSynced was called.
func (t *Tracker) Synced() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
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
// for which NewVersion holds), which ends any watch failure streak. It is
// not a freshness heartbeat: only a matching probe is.
func (t *Tracker) Delivered() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.failSince, t.failLast = time.Time{}, time.Time{}
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
// the informer cache through cache, and compare. A difference is in flight
// until the cache copy has stayed the same for longer than grace while the
// API server disagrees. A failed list records nothing, so an unreachable API
// server goes stale by age. now is read after both lists.
func (t *Tracker) Probe(ctx context.Context, now func() time.Time, api Versions, cache client.Reader, grace time.Duration) {
	if !t.Synced() {
		return
	}
	pctx, cancel := context.WithTimeout(ctx, grace)
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
	at := now()
	pending := map[string]driftEntry{}
	drifted := false
	check := func(key string) {
		if want[key] == have[key] {
			return
		}
		e, ok := t.pending[key]
		if !ok || e.cacheRV != have[key] {
			e = driftEntry{cacheRV: have[key], since: at}
		}
		pending[key] = e
		if at.Sub(e.since) > grace {
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
	if !drifted && at.After(t.last) {
		t.last = at
	}
}

// Run calls Probe every interval until ctx ends.
func (t *Tracker) Run(ctx context.Context, now func() time.Time, api Versions, cache client.Reader, every time.Duration) {
	tk := time.NewTicker(every)
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
		}
		t.Probe(ctx, now, api, cache, every)
	}
}

// Health reports whether the cache is fresh within maxStale (see the
// package comment) and the time it was last shown to match the API server.
func (t *Tracker) Health(now time.Time, maxStale time.Duration) (bool, time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.synced || t.drifted || now.Sub(t.last) > maxStale {
		return false, t.last
	}
	if !t.failSince.IsZero() && now.Sub(t.failLast) <= WatchRetryWindow && now.Sub(t.failSince) > maxStale {
		return false, t.last
	}
	return true, t.last
}

// WorkspaceMetadataVersions returns the probe's API-server side: a paged,
// metadata-only list of Workspaces in each namespace (cluster-wide when
// namespaces is empty) through reader, which must be an uncached client.
func WorkspaceMetadataVersions(reader client.Reader, namespaces []string) Versions {
	return func(ctx context.Context) (map[string]string, error) {
		out := map[string]string{}
		scopes := namespaces
		if len(scopes) == 0 {
			scopes = []string{""}
		}
		for _, ns := range scopes {
			cont := ""
			for {
				list := &metav1.PartialObjectMetadataList{}
				list.SetGroupVersionKind(cdiv1.GroupVersion.WithKind("WorkspaceList"))
				opts := []client.ListOption{client.Limit(500), client.Continue(cont)}
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
					break
				}
			}
		}
		return out, nil
	}
}
