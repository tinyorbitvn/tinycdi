package provisioning

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tinyorbitvn/tinycdi/internal/store"
)

// IntentKind is a lifecycle command carried by the outbox.
type IntentKind string

const (
	IntentCreate IntentKind = "create"
	IntentStart  IntentKind = "start"
	IntentStop   IntentKind = "stop"
	IntentDelete IntentKind = "delete"
)

// Valid reports whether k is a known lifecycle intent.
func (k IntentKind) Valid() bool {
	switch k {
	case IntentCreate, IntentStart, IntentStop, IntentDelete:
		return true
	}
	return false
}

// IntentSpec carries the create-time inputs the runtime side needs to
// build the Workspace CR. Snapshotted into the intent payload so later
// catalog or workspace-row changes cannot rewrite history.
type IntentSpec struct {
	WorkspaceName   string `json:"workspaceName,omitempty"`
	TemplateName    string `json:"templateName,omitempty"` // K8s WorkspaceTemplate name
	OwnerIssuer     string `json:"ownerIssuer,omitempty"`
	OwnerSubject    string `json:"ownerSubject,omitempty"`
	DataPolicy      string `json:"dataPolicy,omitempty"`
	RetainedDataRef string `json:"retainedDataRef,omitempty"`
	// ImageBuiltAt is the template's raw image-built-at annotation value at
	// create time; the applier copies it onto the Workspace CR.
	ImageBuiltAt string `json:"imageBuiltAt,omitempty"`
}

// intentPayload is the JSON envelope stored in outbox_intent.payload.
type intentPayload struct {
	DesiredState      string     `json:"desiredState,omitempty"`
	RuntimeGeneration int64      `json:"runtimeGeneration,omitempty"`
	Spec              IntentSpec `json:"spec,omitempty"`
}

// Intent is one outbox record delivered to the runtime side. Revision is
// the per-workspace monotone sequence; consumers must drop any intent
// whose Revision <= lastAppliedIntentRevision for that workspace.
type Intent struct {
	// WorkspaceUID is the PLATFORM workspace id (PlatformID) — never the
	// CR's metadata.uid; the applier derives the CR NAME from it while the
	// runtime backend names children from the CR UID.
	WorkspaceUID PlatformID
	TenantID     string
	Revision     uint64
	Kind         IntentKind
	RequestID    string // deterministic request ID, used for create dedup
	// DesiredState and RuntimeGeneration are the fencing snapshot taken at
	// append time (the workspace row's values inside the same tx).
	DesiredState      string
	RuntimeGeneration int64
	Spec              IntentSpec
	// CRAnnotations are extra annotations the applier stamps on the
	// Workspace CR IN THE SAME WRITE that creates it. They are never read
	// from or written to the outbox: a wrapping applier (RetainedApplier)
	// sets them after verifying what they assert, so the operator never
	// sees a CR that lacks them (FX-R20).
	CRAnnotations map[string]string
}

var (
	// ErrWorkspaceNotFound is returned when appending to an unknown
	// workspace intent stream.
	ErrWorkspaceNotFound = errors.New("workspace not found")
	// ErrWorkspaceClosed is returned when appending to a workspace whose
	// intent stream was closed by delete.
	ErrWorkspaceClosed = errors.New("workspace intent stream closed")
)

// AppendIntent increments the workspace's intent_revision inside tx and
// inserts the matching outbox_intent row, keeping (workspace, revision)
// gapless. Callers commit quota/idempotency changes in the same tx so an
// intent never exists without its reservation.
func AppendIntent(ctx context.Context, tx store.Tx, workspaceUID PlatformID, kind IntentKind) (uint64, error) {
	return appendIntent(ctx, tx, workspaceUID, kind, nil)
}

// appendIntent is AppendIntent plus a create-spec snapshot for the CR
// builder. desiredState/runtimeGeneration are always snapshotted from the
// workspace row inside the same transaction.
func appendIntent(ctx context.Context, tx store.Tx, workspaceUID PlatformID, kind IntentKind, spec *IntentSpec) (uint64, error) {
	if !kind.Valid() {
		return 0, fmt.Errorf("append intent: bad kind %q", kind)
	}
	var (
		rev       uint64
		tenantID  string
		requestID string
		state     string
		desired   string
		gen       int64
	)
	err := tx.QueryRow(ctx, `
		UPDATE workspaces SET intent_revision = intent_revision + 1
		WHERE id = $1
		RETURNING intent_revision, tenant_id, request_id, state, desired_state, runtime_generation`,
		workspaceUID).
		Scan(&rev, &tenantID, &requestID, &state, &desired, &gen)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrWorkspaceNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("append intent: bump revision %w", err)
	}
	if state != "active" {
		return 0, ErrWorkspaceClosed
	}
	var sp IntentSpec
	if spec != nil {
		sp = *spec
	}
	payload, err := json.Marshal(intentPayload{
		DesiredState:      desired,
		RuntimeGeneration: gen,
		Spec:              sp,
	})
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO outbox_intent (workspace_id, tenant_id, revision, kind, request_id, payload)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		workspaceUID, tenantID, rev, string(kind), requestID, payload); err != nil {
		return 0, fmt.Errorf("append intent: insert %w", err)
	}
	return rev, nil
}

// markDeleted flips the workspace row to deleted inside tx; called after
// the delete intent is appended in the same transaction.
func markDeleted(ctx context.Context, tx store.Tx, workspaceUID PlatformID) error {
	tag, err := tx.Exec(ctx, `
		UPDATE workspaces SET state = 'deleted', deleted_at = now()
		WHERE id = $1 AND state = 'active'`, workspaceUID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrWorkspaceNotFound
	}
	return nil
}

// WorkspaceApplier is the Kubernetes-side contract. The real
// implementation applies Workspace CRs; tests use a fake. Apply must
// be idempotent per (workspaceUID, revision) and per create RequestID
// because delivery is at-least-once.
type WorkspaceApplier interface {
	Apply(ctx context.Context, in Intent) error
}

// ackTimeout bounds recording one delivered intent (see serve).
const ackTimeout = 5 * time.Second

// IntentStore is the dispatcher's read/mark view of the outbox so the
// dispatcher can be unit-tested without PostgreSQL.
type IntentStore interface {
	// PendingWorkspaces lists workspace UIDs having at least one
	// undispatched intent.
	PendingWorkspaces(ctx context.Context) ([]PlatformID, error)
	// PendingIntents lists a workspace's undispatched intents in
	// increasing revision order.
	PendingIntents(ctx context.Context, workspaceUID PlatformID) ([]Intent, error)
	// MarkDispatched records successful delivery of one intent.
	MarkDispatched(ctx context.Context, workspaceUID PlatformID, revision uint64) error
}

// Outbox is the PostgreSQL-backed IntentStore.
type Outbox struct {
	db *store.DB
}

// NewOutbox wraps db.
func NewOutbox(db *store.DB) *Outbox { return &Outbox{db: db} }

// PendingWorkspaces implements IntentStore.
func (o *Outbox) PendingWorkspaces(ctx context.Context) ([]PlatformID, error) {
	rows, err := o.db.Pool().Query(ctx, `
		SELECT DISTINCT workspace_id FROM outbox_intent
		WHERE dispatched_at IS NULL ORDER BY workspace_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var uids []PlatformID
	for rows.Next() {
		var id PlatformID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		uids = append(uids, id)
	}
	return uids, rows.Err()
}

// PendingIntents implements IntentStore.
func (o *Outbox) PendingIntents(ctx context.Context, workspaceUID PlatformID) ([]Intent, error) {
	rows, err := o.db.Pool().Query(ctx, `
		SELECT workspace_id, tenant_id, revision, kind, request_id, payload
		FROM outbox_intent
		WHERE workspace_id = $1 AND dispatched_at IS NULL
		ORDER BY revision`, workspaceUID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Intent
	for rows.Next() {
		var in Intent
		var kind string
		var payload []byte
		if err := rows.Scan(&in.WorkspaceUID, &in.TenantID, &in.Revision,
			&kind, &in.RequestID, &payload); err != nil {
			return nil, err
		}
		in.Kind = IntentKind(kind)
		if len(payload) > 0 {
			var p intentPayload
			if err := json.Unmarshal(payload, &p); err != nil {
				return nil, fmt.Errorf("outbox payload %s rev %d: %w", in.WorkspaceUID, in.Revision, err)
			}
			in.DesiredState = p.DesiredState
			in.RuntimeGeneration = p.RuntimeGeneration
			in.Spec = p.Spec
		}
		out = append(out, in)
	}
	return out, rows.Err()
}

// MarkDispatched implements IntentStore.
func (o *Outbox) MarkDispatched(ctx context.Context, workspaceUID PlatformID, revision uint64) error {
	_, err := o.db.Pool().Exec(ctx, `
		UPDATE outbox_intent SET dispatched_at = now()
		WHERE workspace_id = $1 AND revision = $2`, workspaceUID, revision)
	return err
}

// Dispatcher delivers outbox intents to a WorkspaceApplier. Each
// workspace UID is a partition: intents for one workspace are applied in
// strictly increasing revision by a single goroutine, while different
// workspaces proceed in parallel. Delivery is at-least-once: an intent
// is marked dispatched only after Apply succeeds, so a crash between the
// two steps replays it and the consumer dedups.
type Dispatcher struct {
	store   IntentStore
	applier WorkspaceApplier

	poll    time.Duration
	backoff time.Duration

	// afterApply, when set, runs after Apply succeeds and before
	// MarkDispatched. A non-nil return simulates a crash: the intent is
	// left undispatched. Test hook only.
	afterApply func(Intent) error
}

// DispatcherOption tunes a Dispatcher.
type DispatcherOption func(*Dispatcher)

// WithPollInterval sets the idle scan interval.
func WithPollInterval(d time.Duration) DispatcherOption {
	return func(dp *Dispatcher) { dp.poll = d }
}

// WithApplyBackoff sets the retry delay after a failed Apply.
func WithApplyBackoff(d time.Duration) DispatcherOption {
	return func(dp *Dispatcher) { dp.backoff = d }
}

// WithAfterApplyHook installs the crash-simulation hook.
func WithAfterApplyHook(h func(Intent) error) DispatcherOption {
	return func(dp *Dispatcher) { dp.afterApply = h }
}

// NewDispatcher builds a dispatcher over the given intent store.
func NewDispatcher(s IntentStore, a WorkspaceApplier, opts ...DispatcherOption) *Dispatcher {
	d := &Dispatcher{
		store:   s,
		applier: a,
		poll:    25 * time.Millisecond,
		backoff: 50 * time.Millisecond,
	}
	for _, o := range opts {
		o(d)
	}
	return d
}

// Run scans the outbox until ctx is cancelled. Each partition goroutine
// exits when its workspace has no pending intents, so the loop only
// re-spawns workers for workspaces with new work.
func (d *Dispatcher) Run(ctx context.Context) error {
	r := &dispatchRun{d: d, inflight: map[PlatformID]bool{}}
	tick := time.NewTicker(d.poll)
	defer tick.Stop()
	for {
		uids, err := d.store.PendingWorkspaces(ctx)
		if err != nil && !errors.Is(err, context.Canceled) {
			// Transient scan failure: keep polling.
		}
		for _, uid := range uids {
			r.maybeSpawn(ctx, uid)
		}
		select {
		case <-ctx.Done():
			r.wg.Wait()
			return ctx.Err()
		case <-tick.C:
		}
	}
}

type dispatchRun struct {
	d *Dispatcher

	mu       sync.Mutex
	inflight map[PlatformID]bool
	wg       sync.WaitGroup
}

func (r *dispatchRun) maybeSpawn(ctx context.Context, uid PlatformID) {
	r.mu.Lock()
	if r.inflight[uid] {
		r.mu.Unlock()
		return
	}
	r.inflight[uid] = true
	r.mu.Unlock()
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer func() {
			r.mu.Lock()
			delete(r.inflight, uid)
			r.mu.Unlock()
		}()
		r.serve(ctx, uid)
	}()
}

// serve drains one workspace's pending intents in revision order. On
// Apply failure it re-lists after a backoff so a poison intent retries
// without losing order.
func (r *dispatchRun) serve(ctx context.Context, uid PlatformID) {
	for ctx.Err() == nil {
		intents, err := r.d.store.PendingIntents(ctx, uid)
		if err != nil {
			return
		}
		if len(intents) == 0 {
			return
		}
		failed := false
		for _, in := range intents {
			if err := r.d.applier.Apply(ctx, in); err != nil {
				failed = true
				break
			}
			if r.d.afterApply != nil {
				if err := r.d.afterApply(in); err != nil {
					return // simulated crash: leave undispatched
				}
			}
			// Record the delivery even if ctx was cancelled during Apply (the
			// leader lost its lock or is shutting down): the intent was
			// applied, and a failed ack here would make the next leader
			// replay it. The caller waits for this goroutine before it
			// releases leadership, so the ack lands before a successor can
			// start; ackTimeout bounds it.
			ackCtx, ackCancel := context.WithTimeout(context.WithoutCancel(ctx), ackTimeout)
			err := r.d.store.MarkDispatched(ackCtx, in.WorkspaceUID, in.Revision)
			ackCancel()
			if err != nil {
				failed = true
				break
			}
			if in.Kind == IntentDelete {
				return // delete closes this workspace's stream
			}
		}
		if !failed {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(r.d.backoff):
		}
	}
}

// Consumer enforces the intent-ordering contract in front of a
// WorkspaceApplier: it tracks lastAppliedIntentRevision per workspace and
// drops any intent with revision <= lastApplied. Once a delete intent is
// applied the workspace's stream is closed and all further intents for it
// are dropped.
type Consumer struct {
	next WorkspaceApplier

	mu          sync.Mutex
	lastApplied map[PlatformID]uint64
	closed      map[PlatformID]bool
	dropped     int
}

// NewConsumer wraps applier with the ordering contract.
func NewConsumer(applier WorkspaceApplier) *Consumer {
	return &Consumer{
		next:        applier,
		lastApplied: map[PlatformID]uint64{},
		closed:      map[PlatformID]bool{},
	}
}

// Apply drops stale/closed-stream intents, else forwards to the wrapped
// applier and records the applied revision.
func (c *Consumer) Apply(ctx context.Context, in Intent) error {
	c.mu.Lock()
	if c.closed[in.WorkspaceUID] || in.Revision <= c.lastApplied[in.WorkspaceUID] {
		c.dropped++
		c.mu.Unlock()
		return nil
	}
	c.mu.Unlock()

	if err := c.next.Apply(ctx, in); err != nil {
		return err
	}

	c.mu.Lock()
	c.lastApplied[in.WorkspaceUID] = in.Revision
	if in.Kind == IntentDelete {
		c.closed[in.WorkspaceUID] = true
	}
	c.mu.Unlock()
	return nil
}

// LastApplied returns the recorded lastAppliedIntentRevision.
func (c *Consumer) LastApplied(workspaceUID PlatformID) (uint64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	rev, ok := c.lastApplied[workspaceUID]
	return rev, ok
}

// Closed reports whether the workspace's intent stream ended with delete.
func (c *Consumer) Closed(workspaceUID PlatformID) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed[workspaceUID]
}

// Dropped counts intents dropped by the ordering contract.
func (c *Consumer) Dropped() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dropped
}

// IntentRecord is one recorded lifecycle intent, read back for the
// workspace events projection (GET /v1/workspaces/{id}/events).
type IntentRecord struct {
	Kind      IntentKind
	Revision  uint64
	CreatedAt time.Time
}

// IntentHistory returns the workspace's recorded intents newest first,
// bounded to the latest 200. tenantID is part of the predicate so a
// caller can never read another tenant's history by guessing a UID.
func (s *Service) IntentHistory(ctx context.Context, tenantID string, workspaceUID PlatformID) ([]IntentRecord, error) {
	rows, err := s.db.Pool().Query(ctx, `
		SELECT kind, revision, created_at
		FROM outbox_intent
		WHERE workspace_id = $1 AND tenant_id = $2
		ORDER BY revision DESC
		LIMIT 200`, string(workspaceUID), tenantID)
	if err != nil {
		return nil, fmt.Errorf("intent history: %w", err)
	}
	defer rows.Close()
	var out []IntentRecord
	for rows.Next() {
		var rec IntentRecord
		var kind string
		if err := rows.Scan(&kind, &rec.Revision, &rec.CreatedAt); err != nil {
			return nil, fmt.Errorf("intent history: %w", err)
		}
		rec.Kind = IntentKind(kind)
		out = append(out, rec)
	}
	return out, rows.Err()
}
