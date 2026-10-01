package broker

import (
	"context"
	"fmt"
	"sync"
	"time"

	toolscache "k8s.io/client-go/tools/cache"
	crcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	workspacesv1alpha1 "github.com/tinyorbitvn/tinycdi/api/v1alpha1"
	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
)

// K8sBindingSource is the production BindingSource: it projects Workspace CR
// status through a controller-runtime informer cache scoped to the managed
// tenant namespaces. The cache must be configured with a resync period well
// below MaxBindingAge so the event stream doubles as a liveness heartbeat.
//
// Freshness contract: CurrentBinding reports ErrFreshness until the informer
// has synced, and the returned RuntimeBinding.ObservedAt is the timestamp of
// the last observed watch event — the broker refuses issue/renew once that
// stamp ages past MaxBindingAge, so a stalled watch fails closed.
type K8sBindingSource struct {
	cache crcache.Cache

	mu        sync.Mutex
	synced    bool
	lastEvent time.Time
}

// NewK8sBindingSource registers a Workspace event handler on the cache's
// informer. The caller starts the cache and calls MarkSynced once
// WaitForCacheSync reports true.
func NewK8sBindingSource(ctx context.Context, c crcache.Cache) (*K8sBindingSource, error) {
	inf, err := c.GetInformer(ctx, &workspacesv1alpha1.Workspace{})
	if err != nil {
		return nil, fmt.Errorf("broker: workspace informer: %w", err)
	}
	s := &K8sBindingSource{cache: c}
	if _, err := inf.AddEventHandler(toolscache.ResourceEventHandlerFuncs{
		AddFunc:    func(interface{}) { s.note() },
		UpdateFunc: func(_, _ interface{}) { s.note() },
		DeleteFunc: func(interface{}) { s.note() },
	}); err != nil {
		return nil, fmt.Errorf("broker: informer handler: %w", err)
	}
	return s, nil
}

// MarkSynced records that the informer cache has completed its initial sync.
func (s *K8sBindingSource) MarkSynced() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.synced = true
	s.lastEvent = time.Now()
}

// note records a watch event: the informer is live and emitting.
func (s *K8sBindingSource) note() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.synced = true
	s.lastEvent = time.Now()
}

// CurrentBinding returns the RuntimeBinding for workspaceUID projected from
// the cached Workspace CR. It fails with ErrFreshness when the cache has
// never synced or has gone quiet past the freshness budget, and ErrNotFound
// when no managed Workspace carries the UID label.
func (s *K8sBindingSource) CurrentBinding(ctx context.Context, workspaceUID PlatformID) (RuntimeBinding, error) {
	s.mu.Lock()
	synced, last := s.synced, s.lastEvent
	s.mu.Unlock()
	if !synced || time.Since(last) > MaxBindingAge {
		return RuntimeBinding{}, ErrFreshness
	}
	var list workspacesv1alpha1.WorkspaceList
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
