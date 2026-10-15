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
// has synced and whenever its watch is unhealthy, and the returned
// RuntimeBinding.ObservedAt is the last heartbeat — the broker refuses
// issue/renew once that stamp ages past MaxBindingAge, so a stalled watch
// fails closed. Freshness reflects watch health, not how recently a
// Workspace changed. The source is fresh while
//
//   - a heartbeat arrived within MaxBindingAge: an informer event that
//     carries a new object version, or, while the informer is quiet, a
//     successful uncached probe of the API server (RunLiveness). A periodic
//     resync replays the local store with unchanged resource versions and
//     keeps firing while the watch is down, so it never counts; and
//   - no watch failure streak (NoteWatchError, fed by the cache's watch error
//     handler) has lasted longer than MaxBindingAge. A streak ends on the
//     next event or watchRetryWindow after its last report.
//
// client-go retries a refused or throttled watch inside the reflector with
// the resync still running and without reporting it, so before this rule a
// broker cut off from its API server kept acting on cached bindings for as
// long as the outage lasted.
type K8sBindingSource struct {
	cache crcache.Cache
	now   func() time.Time // nil: time.Now

	mu        sync.Mutex
	synced    bool
	lastEvent time.Time // last heartbeat: informer event or liveness probe success
	failSince time.Time // first watch failure of the current streak; zero when none
	failLast  time.Time // latest watch failure of the current streak
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

// MarkSynced records that the informer cache has completed its initial sync.
func (s *K8sBindingSource) MarkSynced() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.synced = true
	s.lastEvent = s.clock()
}

// onUpdate counts an informer update as a heartbeat only when it carries a
// new object version: a resync replay is no evidence the watch is alive.
func (s *K8sBindingSource) onUpdate(oldObj, newObj interface{}) {
	o, ok1 := oldObj.(metav1.Object)
	n, ok2 := newObj.(metav1.Object)
	if !ok1 || !ok2 || o.GetResourceVersion() != n.GetResourceVersion() {
		s.note()
	}
}

// note records a watch event: the informer is live and emitting, which also
// ends any watch failure streak.
func (s *K8sBindingSource) note() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.synced = true
	s.lastEvent = s.clock()
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

// RunLiveness probes the API server every interval in which the informer
// delivered no event and counts a success as a heartbeat; it returns when
// ctx ends. This keeps an idle but healthy watch fresh: silence alone cannot
// tell it from a dead one. probe must bypass the informer cache (see
// WorkspaceListProbe).
func (s *K8sBindingSource) RunLiveness(ctx context.Context, probe func(context.Context) error, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		s.livenessTick(ctx, probe, every)
	}
}

// livenessTick runs one RunLiveness round: probe only while synced and quiet
// for at least every, and record a success as a heartbeat.
func (s *K8sBindingSource) livenessTick(ctx context.Context, probe func(context.Context) error, every time.Duration) {
	s.mu.Lock()
	quiet := s.synced && s.clock().Sub(s.lastEvent) >= every
	s.mu.Unlock()
	if !quiet {
		return
	}
	pctx, cancel := context.WithTimeout(ctx, every)
	err := probe(pctx)
	cancel()
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if now := s.clock(); now.After(s.lastEvent) {
		s.lastEvent = now
	}
}

// WorkspaceListProbe returns a liveness probe that lists at most one
// Workspace in each namespace (cluster-wide when namespaces is empty)
// through reader, which must be an uncached client: it proves the API server
// still serves the resource the informer watches.
func WorkspaceListProbe(reader client.Reader, namespaces []string) func(context.Context) error {
	return func(ctx context.Context) error {
		if len(namespaces) == 0 {
			return reader.List(ctx, &cdiv1.WorkspaceList{}, client.Limit(1))
		}
		for _, ns := range namespaces {
			if err := reader.List(ctx, &cdiv1.WorkspaceList{}, client.InNamespace(ns), client.Limit(1)); err != nil {
				return err
			}
		}
		return nil
	}
}

// health reports whether the source is fresh (see K8sBindingSource) and the
// time of the last heartbeat.
func (s *K8sBindingSource) health() (bool, time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clock()
	if !s.synced || now.Sub(s.lastEvent) > MaxBindingAge {
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
