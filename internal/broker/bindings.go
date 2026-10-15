package broker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	toolscache "k8s.io/client-go/tools/cache"
	crcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cdiv1 "github.com/tinyorbitvn/tinycdi/api/v1"
	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
)

// K8sBindingSource is the production BindingSource: it projects Workspace CR
// status through a controller-runtime informer cache scoped to the managed
// tenant namespaces.
//
// Freshness contract: CurrentBinding reports ErrFreshness until the informer
// has synced and whenever the cache cannot be shown to match the API server,
// and the returned RuntimeBinding.ObservedAt is the last time it was shown
// to — the broker refuses issue/renew once that stamp ages past
// MaxBindingAge, so a stalled watch fails closed. Freshness means "the cache
// matches the API server", not "an event arrived recently". The source is
// fresh while
//
//   - a consistency probe (RunLiveness) succeeded within MaxBindingAge: an
//     uncached metadata-only list of the watched Workspaces agrees with the
//     informer cache on membership and every resourceVersion, apart from
//     differences still inside their grace period (see below). An idle but
//     healthy watch therefore stays fresh, and informer events, resync
//     replays included, are never evidence on their own;
//   - no difference has outlived its grace: a Workspace whose API-server
//     version differs from the cache (changed, created or deleted) while
//     the cache copy stays unchanged for longer than one probe interval
//     means the watch is not delivering — a silent stall, a watch retried
//     after a refusal or 429, or one namespace's watch stuck while others
//     flow. The cache moving on (a delivered event) restarts the grace;
//   - no watch failure streak (NoteWatchError, fed by the cache's watch error
//     handler) has lasted longer than MaxBindingAge. A streak ends on the
//     next delivered event or watchRetryWindow after its last report.
//
// A change the watch never delivers is caught at the probe that sees it
// plus one interval of grace: with the 5 s probe, within 15 s.
type K8sBindingSource struct {
	cache crcache.Cache
	now   func() time.Time // nil: time.Now

	mu        sync.Mutex
	synced    bool
	lastEvent time.Time // last proof the cache matched the API server
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

// watchRetryWindow is how long one reported watch failure keeps the source
// suspect. A reflector that is still failing re-reports at least every
// [30, 60) s (client-go's backoff cap), so a failure older than this with no
// successor means the watch re-established.
const watchRetryWindow = time.Minute

// workspaceTypeDescription is the reflector type description of the
// Workspace informer, used to pick its failures out of the shared cache's
// watch error handler.
var workspaceTypeDescription = reflect.TypeOf(&cdiv1.Workspace{}).String()

// NewK8sBindingSource registers a Workspace event handler on the cache's
// informer. The caller starts the cache, calls MarkSynced once
// WaitForCacheSync reports true, feeds the cache's watch error handler to
// NoteWatchError and runs RunLiveness.
func NewK8sBindingSource(ctx context.Context, c crcache.Cache) (*K8sBindingSource, error) {
	inf, err := c.GetInformer(ctx, &cdiv1.Workspace{})
	if err != nil {
		return nil, fmt.Errorf("broker: workspace informer: %w", err)
	}
	s := &K8sBindingSource{cache: c}
	if _, err := inf.AddEventHandler(toolscache.ResourceEventHandlerFuncs{
		AddFunc:    func(interface{}) { s.note() },
		UpdateFunc: s.onUpdate,
		DeleteFunc: func(interface{}) { s.note() },
	}); err != nil {
		return nil, fmt.Errorf("broker: informer handler: %w", err)
	}
	return s, nil
}

func (s *K8sBindingSource) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// MarkSynced records that the informer cache has completed its initial sync
// (a full list: the cache matches the API server at this moment).
func (s *K8sBindingSource) MarkSynced() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.synced = true
	s.lastEvent = s.clock()
}

// onUpdate treats an informer update as a delivered watch event only when it
// carries a new object version: a resync replay is no evidence the watch is
// alive.
func (s *K8sBindingSource) onUpdate(oldObj, newObj interface{}) {
	o, ok1 := oldObj.(metav1.Object)
	n, ok2 := newObj.(metav1.Object)
	if !ok1 || !ok2 || o.GetResourceVersion() != n.GetResourceVersion() {
		s.note()
	}
}

// note records a delivered watch event, which ends any watch failure streak.
// It is not a freshness heartbeat: only a matching probe is.
func (s *K8sBindingSource) note() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failSince, s.failLast = time.Time{}, time.Time{}
}

// NoteWatchError records a list/watch failure reported by the cache's watch
// error handler for the informer of typeDescription (the reflector's
// TypeDescription). Failures of other informers are ignored, and so are the
// routine closes client-go itself treats as benign (EOF, an expired resource
// version): the reflector resumes from them without a gap.
func (s *K8sBindingSource) NoteWatchError(typeDescription string, err error) {
	if typeDescription != workspaceTypeDescription ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		apierrors.IsResourceExpired(err) || apierrors.IsGone(err) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clock()
	if s.failSince.IsZero() || now.Sub(s.failLast) > watchRetryWindow {
		s.failSince = now
	}
	s.failLast = now
}

// WorkspaceVersions lists the API server's Workspaces as "namespace/name" ->
// resourceVersion.
type WorkspaceVersions func(context.Context) (map[string]string, error)

// RunLiveness runs the consistency probe every interval until ctx ends (see
// K8sBindingSource). Cost per probe: one paged metadata-only list per
// watched namespace (cluster-wide when none) returning every Workspace's
// metadata (~1 KiB each, no spec/status), plus one no-copy read of the
// informer cache — linear in the Workspace count, every interval.
func (s *K8sBindingSource) RunLiveness(ctx context.Context, api WorkspaceVersions, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		s.livenessTick(ctx, api, every)
	}
}

// livenessTick runs one probe: list the API server, then the cache, and
// compare. A difference is in flight until the cache copy has stayed the same
// for longer than grace while the API server disagrees. A failed list
// records nothing, so an unreachable API server goes stale by age.
func (s *K8sBindingSource) livenessTick(ctx context.Context, api WorkspaceVersions, grace time.Duration) {
	s.mu.Lock()
	synced := s.synced
	s.mu.Unlock()
	if !synced {
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
	if err := s.cache.List(pctx, &cached, client.UnsafeDisableDeepCopyOption(true)); err != nil {
		return
	}
	have := make(map[string]string, len(cached.Items))
	for i := range cached.Items {
		ws := &cached.Items[i]
		have[ws.Namespace+"/"+ws.Name] = ws.ResourceVersion
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clock()
	pending := map[string]driftEntry{}
	drifted := false
	check := func(key string) {
		if want[key] == have[key] {
			return
		}
		e, ok := s.pending[key]
		if !ok || e.cacheRV != have[key] {
			e = driftEntry{cacheRV: have[key], since: now}
		}
		pending[key] = e
		if now.Sub(e.since) > grace {
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
	s.pending, s.drifted = pending, drifted
	if !drifted && now.After(s.lastEvent) {
		s.lastEvent = now
	}
}

// WorkspaceMetadataVersions returns the probe's API-server side: a paged,
// metadata-only list of Workspaces in each namespace (cluster-wide when
// namespaces is empty) through reader, which must be an uncached client.
func WorkspaceMetadataVersions(reader client.Reader, namespaces []string) WorkspaceVersions {
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

// health reports whether the source is fresh (see K8sBindingSource) and the
// time it was last shown to match the API server.
func (s *K8sBindingSource) health() (bool, time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clock()
	if !s.synced || s.drifted || now.Sub(s.lastEvent) > MaxBindingAge {
		return false, s.lastEvent
	}
	if !s.failSince.IsZero() && now.Sub(s.failLast) <= watchRetryWindow && now.Sub(s.failSince) > MaxBindingAge {
		return false, s.lastEvent
	}
	return true, s.lastEvent
}

// CurrentBinding returns the RuntimeBinding for workspaceUID projected from
// the cached Workspace CR. It fails with ErrFreshness when the cache has
// never synced or its watch is unhealthy (see K8sBindingSource), and ErrNotFound
// when no managed Workspace carries the UID label.
func (s *K8sBindingSource) CurrentBinding(ctx context.Context, workspaceUID PlatformID) (RuntimeBinding, error) {
	fresh, last := s.health()
	if !fresh {
		return RuntimeBinding{}, ErrFreshness
	}
	var list cdiv1.WorkspaceList
	if err := s.cache.List(ctx, &list,
		client.MatchingLabels{provisioning.LabelWorkspaceUID: string(workspaceUID)}); err != nil {
		return RuntimeBinding{}, fmt.Errorf("broker: list workspaces: %w", err)
	}
	if len(list.Items) == 0 {
		return RuntimeBinding{}, ErrNotFound
	}
	if len(list.Items) > 1 {
		return RuntimeBinding{}, fmt.Errorf("broker: %d Workspace CRs carry uid %s", len(list.Items), workspaceUID)
	}
	ws := &list.Items[0]
	b := RuntimeBinding{
		WorkspaceUID:      workspaceUID,
		CRUID:             ws.UID,
		TenantID:          ws.Labels[provisioning.LabelTenant],
		OwnerSubject:      ws.Spec.OwnerSubject.Issuer + "|" + ws.Spec.OwnerSubject.Subject,
		Phase:             string(ws.Status.Phase),
		RuntimeGeneration: uint64(ws.Status.ObservedRuntimeGeneration),
		RuntimeUID:        ws.Status.RuntimeUID,
		ObservedAt:        last,
		Namespace:         ws.Namespace,
	}
	if ref := ws.Status.ServiceRef; ref != nil {
		b.ServiceName = ref.Name
		b.ServicePort = ref.Port
	}
	return b, nil
}
