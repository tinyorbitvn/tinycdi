package broker

import (
	"context"
	"fmt"
	"time"

	toolscache "k8s.io/client-go/tools/cache"
	crcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cdiv1 "github.com/tinyorbitvn/tinycdi/api/v1"
	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
	"github.com/tinyorbitvn/tinycdi/internal/watchhealth"
)

// K8sBindingSource is the production BindingSource: it projects Workspace CR
// status through a controller-runtime informer cache scoped to the managed
// tenant namespaces.
//
// Freshness contract: CurrentBinding reports ErrFreshness until the informer
// has synced and whenever the cache cannot be shown to match the API server
// (internal/watchhealth), and the returned RuntimeBinding.ObservedAt is the
// last time it was shown to — the broker refuses issue/renew once that
// stamp ages past MaxBindingAge, so a stalled watch fails closed. Informer
// events, resync replays included, are never a heartbeat.
type K8sBindingSource struct {
	cache  crcache.Cache
	now    func() time.Time // nil: time.Now
	health watchhealth.Tracker
}

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
		AddFunc:    func(interface{}) { s.health.Delivered() },
		UpdateFunc: s.onUpdate,
		DeleteFunc: func(interface{}) { s.health.Delivered() },
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
func (s *K8sBindingSource) MarkSynced() { s.health.MarkSynced(s.clock()) }

// onUpdate records a delivered watch event unless the update is a resync
// replay.
func (s *K8sBindingSource) onUpdate(oldObj, newObj interface{}) {
	if watchhealth.NewVersion(oldObj, newObj) {
		s.health.Delivered()
	}
}

// NoteWatchError feeds a list/watch failure from the cache's watch error
// handler (see watchhealth.Tracker.NoteWatchError).
func (s *K8sBindingSource) NoteWatchError(typeDescription string, err error) {
	s.health.NoteWatchError(s.clock(), typeDescription, err)
}

// RunLiveness runs the consistency probe every interval until ctx ends: api
// (watchhealth.WorkspaceMetadataVersions over an uncached client) against
// the informer cache. See internal/watchhealth for the rule and its cost.
func (s *K8sBindingSource) RunLiveness(ctx context.Context, api watchhealth.Versions, every time.Duration) {
	s.health.Run(ctx, s.clock, api, s.cache, every)
}

// livenessTick runs one probe with grace one interval.
func (s *K8sBindingSource) livenessTick(ctx context.Context, api watchhealth.Versions, grace time.Duration) {
	s.health.Probe(ctx, s.clock, api, s.cache, grace)
}

// CurrentBinding returns the RuntimeBinding for workspaceUID projected from
// the cached Workspace CR. It fails with ErrFreshness when the cache has
// never synced or its watch is unhealthy (see K8sBindingSource), and ErrNotFound
// when no managed Workspace carries the UID label.
func (s *K8sBindingSource) CurrentBinding(ctx context.Context, workspaceUID PlatformID) (RuntimeBinding, error) {
	fresh, last := s.health.Health(s.clock(), MaxBindingAge)
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
