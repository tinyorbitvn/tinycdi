// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package provisioning

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	toolscache "k8s.io/client-go/tools/cache"

	workspacev1alpha1 "github.com/tinyorbitvn/tinycdi/api/v1alpha1"
	"github.com/tinyorbitvn/tinycdi/internal/store"
)

// settlePoll is the settle worker's default drain cadence: the coalescing
// set means bursts cost one pass, and the 30 s recovery tick remains the
// catch-all for anything missed.
const settlePoll = time.Second

// SettleTrigger is the non-blocking event side of event-driven quota
// settlement: the Workspace informer's handler records the platform id of
// every add/update/delete — the operator's observed runtime absence is
// written to CR status or the CR's removal — and a leader-gated
// SettleWorker drains the deduplicated set. Enqueue never blocks and never
// allocates per event beyond the map entry: informer delivery must not be
// held up by control-plane bookkeeping.
type SettleTrigger struct {
	mu  sync.Mutex
	set map[PlatformID]struct{}
}

// NewSettleTrigger builds the trigger; wire Handler onto the Workspace
// informer before the cache starts.
func NewSettleTrigger() *SettleTrigger {
	return &SettleTrigger{set: map[PlatformID]struct{}{}}
}

// Handler returns the informer callbacks that feed the trigger.
func (t *SettleTrigger) Handler() toolscache.ResourceEventHandler {
	return toolscache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { t.EnqueueObject(obj) },
		UpdateFunc: func(_, obj interface{}) { t.EnqueueObject(obj) },
		DeleteFunc: func(obj interface{}) { t.EnqueueObject(obj) },
	}
}

// Enqueue records a workspace id for the next drain. Idempotent by
// construction — the set coalesces bursts.
func (t *SettleTrigger) Enqueue(uid PlatformID) {
	t.mu.Lock()
	t.set[uid] = struct{}{}
	t.mu.Unlock()
}

// EnqueueObject maps an informer event object to the platform workspace id
// carried in its workspace-uid label and enqueues it. Delete events may
// arrive as DeletedFinalStateUnknown tombstones.
func (t *SettleTrigger) EnqueueObject(obj interface{}) {
	ws, ok := obj.(*workspacev1alpha1.Workspace)
	if !ok {
		if tomb, isTomb := obj.(toolscache.DeletedFinalStateUnknown); isTomb {
			ws, _ = tomb.Obj.(*workspacev1alpha1.Workspace)
		}
	}
	if ws == nil {
		return
	}
	if uid := ws.Labels[LabelWorkspaceUID]; uid != "" {
		t.Enqueue(PlatformID(uid))
	}
}

// Pending reports the coalesced-but-undrained count — observability and
// tests only.
func (t *SettleTrigger) Pending() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.set)
}

// drain atomically swaps the pending set out.
func (t *SettleTrigger) drain() []PlatformID {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]PlatformID, 0, len(t.set))
	for uid := range t.set {
		out = append(out, uid)
	}
	t.set = map[PlatformID]struct{}{}
	return out
}

// SettleWorker is the leader-gated drain loop: it runs only in the replica
// holding the singleton advisory lock, like every read-modify-write loop.
// For each triggered workspace it re-checks the recovery settle predicate
// (settleCandidateSQL) and calls Recovery.SettleQuota — the event is only
// a trigger, the same positive runtime-absence proof that gates the 30 s
// tick gates the release. A lost event or a dropped (failed) settle is
// settled by the next recovery pass instead — the event path adds latency
// only, it never becomes the sole retry path.
// warnSuppression bounds failure WARNs to one per UID per minute: a
// dropped UID is retried by the 30 s recovery tick, so repeated event
// bursts on a poison row must not spam the log.
const warnSuppression = time.Minute

type SettleWorker struct {
	DB      *store.DB
	Rec     *Recovery
	Trigger *SettleTrigger
	Log     *slog.Logger
	// Poll bounds the drain cadence; <=0 uses 1 s.
	Poll time.Duration

	warnMu sync.Mutex
	warned map[PlatformID]time.Time
}

// Run drains the trigger until ctx is cancelled.
func (w *SettleWorker) Run(ctx context.Context) error {
	poll := w.Poll
	if poll <= 0 {
		poll = settlePoll
	}
	tick := time.NewTicker(poll)
	defer tick.Stop()
	for {
		w.DrainOnce(ctx)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

// settleCandidate reports whether the workspace's reservation currently
// matches the recovery settle predicate, plus its tenant.
func (w *SettleWorker) settleCandidate(ctx context.Context, uid PlatformID) (string, bool, error) {
	var tenantID string
	var ok bool
	err := w.DB.Pool().QueryRow(ctx, `
		SELECT w.tenant_id, EXISTS(
			SELECT 1 FROM quota_reservation qr
			WHERE qr.workspace_id = w.id AND (`+settleCandidateSQL+`))
		FROM workspaces w WHERE w.id = $1`, uid).Scan(&tenantID, &ok)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return tenantID, ok, err
}

// DrainOnce settles every currently-triggered candidate once. It is
// exported for tests. A failing UID is DROPPED — the periodic recovery
// pass sweeps the same predicate (settleCandidateSQL) and owns retries —
// and the failure warns at most once per UID per minute.
func (w *SettleWorker) DrainOnce(ctx context.Context) {
	for _, uid := range w.Trigger.drain() {
		tenantID, ok, err := w.settleCandidate(ctx, uid)
		switch {
		case err != nil:
			w.warnOnce(uid, "quota settle: candidate check", "err", err)
			continue
		case !ok:
			continue // not a settle candidate — the tick predicate matches
		}
		if err := w.Rec.SettleQuota(ctx, tenantID, uid); err != nil {
			if errors.Is(err, ErrRuntimeNotProvenGone) {
				continue // absence unproven: later events or the tick retry
			}
			w.warnOnce(uid, "quota settle failed", "err", err)
			continue
		}
		w.log().Info("quota settled on observed runtime absence", "workspace", string(uid))
	}
}

// warnOnce logs msg for uid at most once per warnSuppression window.
func (w *SettleWorker) warnOnce(uid PlatformID, msg string, args ...interface{}) {
	w.warnMu.Lock()
	if w.warned == nil {
		w.warned = map[PlatformID]time.Time{}
	}
	now := time.Now()
	if last, ok := w.warned[uid]; ok && now.Sub(last) < warnSuppression {
		w.warnMu.Unlock()
		return
	}
	w.warned[uid] = now
	for id, last := range w.warned { // bound the suppression map
		if now.Sub(last) >= warnSuppression {
			delete(w.warned, id)
		}
	}
	w.warnMu.Unlock()
	w.log().Warn(msg, append([]interface{}{"workspace", string(uid)}, args...)...)
}

func (w *SettleWorker) log() *slog.Logger {
	if w.Log != nil {
		return w.Log
	}
	return slog.Default()
}
