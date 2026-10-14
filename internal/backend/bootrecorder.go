package backend

import (
	"context"
	"log/slog"
	"time"

	toolscache "k8s.io/client-go/tools/cache"
	crcache "sigs.k8s.io/controller-runtime/pkg/cache"

	cdiv1 "github.com/tinyorbitvn/tinycdi/api/v1"
	"github.com/tinyorbitvn/tinycdi/internal/observability"
	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
	"github.com/tinyorbitvn/tinycdi/internal/store"
)

// bootReadyTransition reports the (workspace id, generation, startedAt) of
// an incarnation that just became Ready, or ok=false.
func bootReadyTransition(oldObj, newObj any) (string, int64, time.Time, bool) {
	nw, ok := newObj.(*cdiv1.Workspace)
	if !ok || nw.Status.Phase != cdiv1.WorkspacePhaseReady || nw.Status.StartedAt == nil {
		return "", 0, time.Time{}, false
	}
	if ow, ok := oldObj.(*cdiv1.Workspace); ok && ow.Status.Phase == cdiv1.WorkspacePhaseReady &&
		ow.Status.ObservedRuntimeGeneration == nw.Status.ObservedRuntimeGeneration {
		return "", 0, time.Time{}, false
	}
	id := nw.Labels[provisioning.LabelWorkspaceUID]
	if id == "" {
		return "", 0, time.Time{}, false
	}
	return id, nw.Status.ObservedRuntimeGeneration, nw.Status.StartedAt.Time, true
}

// registerBootRecorder writes workspace_boot.ready_at and feeds the
// provisioning-latency histogram when a Workspace incarnation turns Ready.
func registerBootRecorder(ctx context.Context, c crcache.Cache, db *store.DB, m *observability.Metrics, log *slog.Logger) error {
	inf, err := c.GetInformer(ctx, &cdiv1.Workspace{})
	if err != nil {
		return err
	}
	// At most four writes in flight: a backend restart replays every Ready CR as
	// an Add event, and each of those is a (no-op) UPDATE.
	sem := make(chan struct{}, 4)
	mark := func(id string, gen int64, at time.Time) {
		select {
		case sem <- struct{}{}:
			defer func() { <-sem }()
		case <-ctx.Done():
			return
		}
		mctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		d, ok, err := db.MarkBootReady(mctx, id, gen, at)
		if err != nil {
			log.Warn("boot recorder: mark ready failed", "workspace", id, "generation", gen, "err", err)
			return
		}
		if ok && m != nil {
			m.ObserveProvisioningLatency("success", d)
		}
	}
	_, err = inf.AddEventHandler(toolscache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			if id, gen, at, ok := bootReadyTransition(nil, obj); ok {
				go mark(id, gen, at)
			}
		},
		UpdateFunc: func(oldObj, newObj any) {
			if id, gen, at, ok := bootReadyTransition(oldObj, newObj); ok {
				go mark(id, gen, at)
			}
		},
	})
	return err
}
