package broker

import (
	"context"
	"fmt"
	"sync"
	"time"

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
//
// In the backend the Tracker is shared with the API's status view and run
// once (internal/backend); a source built without one owns its own.
type K8sBindingSource struct {
	cache crcache.Cache
	now   func() time.Time // nil: time.Now

	mu     sync.Mutex
	health *watchhealth.Tracker // nil until first use: a private tracker
}

// NewK8sBindingSource builds the source over the cache's Workspace informer.
// With a shared tracker the caller feeds it (Tracker.Handler on the
// informer with SyncedWhen on its registration, the cache's watch error handler, MarkSynced, Run); with nil the
// source owns a tracker and registers its event handler itself, and the
// caller calls MarkSynced and NoteWatchError and runs the tracker's probe.
func NewK8sBindingSource(ctx context.Context, c crcache.Cache, health *watchhealth.Tracker) (*K8sBindingSource, error) {
	inf, err := c.GetInformer(ctx, &cdiv1.Workspace{})
	if err != nil {
		return nil, fmt.Errorf("broker: workspace informer: %w", err)
	}
	s := &K8sBindingSource{cache: c, health: health}
	if health == nil {
		tr := s.tracker()
		reg, err := inf.AddEventHandler(tr.Handler())
		if err != nil {
			return nil, fmt.Errorf("broker: informer handler: %w", err)
		}
		tr.SyncedWhen(reg.HasSynced)
	}
	return s, nil
}

// tracker returns the source's freshness tracker, creating a private one on
// first use.
func (s *K8sBindingSource) tracker() *watchhealth.Tracker {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.health == nil {
		s.health = &watchhealth.Tracker{}
	}
	return s.health
}

func (s *K8sBindingSource) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// MarkSynced records that the informer cache has completed its initial sync.
func (s *K8sBindingSource) MarkSynced() { s.tracker().MarkSynced(s.clock()) }

// NoteWatchError feeds a list/watch failure from the cache's watch error
// handler (see watchhealth.Tracker.NoteWatchError).
func (s *K8sBindingSource) NoteWatchError(typeDescription string, err error) {
	s.tracker().NoteWatchError(s.clock(), typeDescription, err)
}

// CurrentBinding returns the RuntimeBinding for workspaceUID projected from
// the cached Workspace CR. It fails with ErrFreshness when the cache has
// never synced or its watch is unhealthy (see K8sBindingSource), and ErrNotFound
// when no managed Workspace carries the UID label.
func (s *K8sBindingSource) CurrentBinding(ctx context.Context, workspaceUID PlatformID) (RuntimeBinding, error) {
	fresh, last := s.tracker().Health(s.clock(), MaxBindingAge)
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
