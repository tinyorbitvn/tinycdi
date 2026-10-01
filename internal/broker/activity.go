package broker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
	"github.com/tinyorbitvn/tinycdi/internal/store"
)

// ErrActivityType is returned when ReportActivity receives an event type it
// does not recognize. Unknown signals (WebSocket pings, video frames,
// keep-alives) are never recorded and never extend a session — only
// keyboard/mouse input does.
var ErrActivityType = errors.New("broker: unrecognized activity event type")

// ActivityEventType classifies a gateway-reported session signal
// (design §8). Only these three values are valid.
type ActivityEventType string

const (
	// ActivityInput is authenticated keyboard/mouse input from the session
	// adapter — the only signal that resets the input-idle clock.
	ActivityInput ActivityEventType = "input"
	// ActivityDisconnect marks the last interactive stream closing; it
	// starts the disconnect grace window.
	ActivityDisconnect ActivityEventType = "disconnect"
	// ActivityConnected marks an interactive stream opening; inside the
	// grace window it cancels a pending disconnect stop.
	ActivityConnected ActivityEventType = "connected"
)

// ActivityEvent is one signal reported by a session gateway. ReceivedAt is
// the SERVER receipt time assigned by the broker from its own clock:
// client-supplied timestamps are never trusted and are overwritten.
type ActivityEvent struct {
	Type       ActivityEventType
	ReceivedAt time.Time // server receipt time; broker-assigned, client input ignored
}

// StopReason identifies which deadline (or explicit request) produced a
// stop decision. Reasons are machine-readable and surface into conditions.
type StopReason string

const (
	// StopReasonIdleTimeout — no input for TimeoutPolicy.IdleTimeout.
	StopReasonIdleTimeout StopReason = "idle_timeout"
	// StopReasonDisconnectTimeout — disconnected for the full grace window.
	StopReasonDisconnectTimeout StopReason = "disconnect_timeout"
	// StopReasonMaxDuration — runtime reached its absolute duration cap.
	StopReasonMaxDuration StopReason = "max_duration"
	// StopReasonRequested — explicit stop (user or admin action).
	StopReasonRequested StopReason = "requested"
)

// TimeoutPolicy is the per-workspace lifecycle budget, taken from the
// WorkspaceTemplate's lifecycle defaults snapshot at admit time. The
// template may override the platform defaults within policy bounds.
type TimeoutPolicy struct {
	// IdleTimeout stops a workspace with no input for this long
	// (default 30 m).
	IdleTimeout time.Duration
	// DisconnectTimeout is the grace window after the last stream closes;
	// a reconnect inside it cancels the pending stop (default 10 m).
	DisconnectTimeout time.Duration
	// MaxDuration is the absolute cap on a runtime generation; input does
	// NOT extend it (default 8 h).
	MaxDuration time.Duration
}

// DefaultTimeoutPolicy is the design §8 baseline when a template leaves a
// field unset: 30 min no input, 10 min disconnect, 8 h max.
var DefaultTimeoutPolicy = TimeoutPolicy{
	IdleTimeout:       30 * time.Minute,
	DisconnectTimeout: 10 * time.Minute,
	MaxDuration:       8 * time.Hour,
}

// StopIntent is a durable stop decision pinned to one runtime generation.
// Because it carries RuntimeGeneration, a delayed intent can be re-checked
// before it is applied — it must never stop a newer runtime.
type StopIntent struct {
	WorkspaceUID      PlatformID
	RuntimeGeneration uint64
	Reason            StopReason
	// Deadline is the moment the budget was crossed (server clock).
	Deadline time.Time
}

// ReportActivity records one gateway-observed session signal for the
// lease's bound incarnation. The fence must equal the lease's current
// binding: a stale runtimeGeneration, runtimeUID or fencing version is
// refused with ErrStaleBinding, so a dead incarnation can never keep a
// newer runtime alive. A different gateway identity is refused with
// ErrDenied and an expired/revoked lease with ErrLeaseInvalid.
//
// Only ActivityInput resets the input-idle clock. ActivityConnected /
// ActivityDisconnect open and close the disconnect grace window. Any other
// event type is rejected with ErrActivityType — WebSocket pings, video
// frames and merely-open sockets are not user activity.
func (b *Broker) ReportActivity(ctx context.Context, gw GatewayIdentity, leaseID string, fence Fence, ev ActivityEvent) error {
	switch ev.Type {
	case ActivityInput, ActivityDisconnect, ActivityConnected:
	default:
		return ErrActivityType
	}
	now := b.now()
	// Unlike liveLease, activity does NOT enforce the sliding lease TTL:
	// the idle/disconnect clocks must keep accepting signals from a stream
	// whose renew loop merely hiccuped. A revoked/superseded/expired lease
	// (a closed session) still fails with ErrLeaseInvalid — rendered 410 by
	// the internal API.
	l, state, err := b.loadLease(ctx, leaseID)
	if err != nil {
		return err
	}
	if state != "active" {
		return ErrLeaseInvalid
	}
	if l.GatewayID != gw.ID {
		return ErrDenied
	}
	if fence.WorkspaceUID != l.WorkspaceUID ||
		fence.RuntimeGeneration != l.RuntimeGeneration ||
		fence.RuntimeUID != l.RuntimeUID ||
		fence.FencingVersion != l.FencingVersion {
		return ErrStaleBinding
	}
	// Re-check the lease's pinned incarnation against the operator's current
	// view. Unlike RenewLease this skips the freshness gate: recording a
	// signal is not granting access, and a stale informer must not silently
	// drop activity (a dead incarnation is still refused).
	if b.src == nil {
		return ErrFreshness
	}
	binding, err := b.src.CurrentBinding(ctx, PlatformID(l.WorkspaceUID))
	switch {
	case errors.Is(err, ErrNotFound):
		return ErrStaleBinding
	case err != nil:
		return fmt.Errorf("broker: activity binding lookup: %w", err)
	}
	if binding.Phase != PhaseReady ||
		binding.RuntimeGeneration != l.RuntimeGeneration ||
		binding.RuntimeUID != l.RuntimeUID {
		return ErrStaleBinding
	}
	if err := b.recordActivity(ctx, l.ID, PlatformID(l.WorkspaceUID), l.RuntimeGeneration, ev.Type, now); err != nil {
		return err
	}
	// Desktop input extends the owning user's PORTAL session idle timer
	// (D18): the hook receives the lease's principal — the "iss|sub" owner
	// string — after the event is durably recorded.
	if ev.Type == ActivityInput && b.inputHook != nil {
		b.inputHook(ctx, l.PrincipalSubject)
	}
	return nil
}

// recordActivity upserts the (workspace, generation) activity row. All
// timestamps are the broker's receipt time, never the client's.
//
// The write is serialized against revocation: it locks the lease row
// FOR UPDATE inside the transaction, so a lease revoked between
// ReportActivity's initial state check and this write drops the event
// instead of re-opening drain accounting that the revoke already closed —
// a revoked lease's streams are closed authoritatively at revoke time.
func (b *Broker) recordActivity(ctx context.Context, leaseID string, wsUID PlatformID, gen uint64, t ActivityEventType, now time.Time) error {
	var q string
	switch t {
	case ActivityInput:
		q = `INSERT INTO workspace_activity
			(workspace_id, runtime_generation, last_input_at, updated_at)
		 VALUES ($1, $2, $3, $3)
		 ON CONFLICT (workspace_id, runtime_generation) DO UPDATE SET
			last_input_at = GREATEST(workspace_activity.last_input_at, EXCLUDED.last_input_at),
			updated_at = EXCLUDED.updated_at`
	case ActivityConnected:
		// A connected stream cancels a pending disconnect: the grace window
		// does not keep ticking underneath (design §8).
		q = `INSERT INTO workspace_activity
			(workspace_id, runtime_generation, connected_at, open_streams, updated_at)
		 VALUES ($1, $2, $3, 1, $3)
		 ON CONFLICT (workspace_id, runtime_generation) DO UPDATE SET
			connected_at = EXCLUDED.connected_at,
			open_streams = workspace_activity.open_streams + 1,
			disconnected_since = NULL,
			updated_at = EXCLUDED.updated_at`
	case ActivityDisconnect:
		// The grace window anchors on the FIRST transition to zero streams;
		// duplicate disconnects keep the earliest deadline.
		q = `INSERT INTO workspace_activity
			(workspace_id, runtime_generation, open_streams, disconnected_since, updated_at)
		 VALUES ($1, $2, 0, $3, $3)
		 ON CONFLICT (workspace_id, runtime_generation) DO UPDATE SET
			open_streams = GREATEST(workspace_activity.open_streams - 1, 0),
			disconnected_since = CASE
				WHEN workspace_activity.open_streams - 1 <= 0
					THEN COALESCE(workspace_activity.disconnected_since, EXCLUDED.disconnected_since)
				ELSE NULL
			END,
			updated_at = EXCLUDED.updated_at`
	}
	return b.db.WithTx(ctx, func(tx store.Tx) error {
		var locked string
		err := tx.QueryRow(ctx,
			`SELECT id FROM connection_lease WHERE id = $1 AND state = 'active' FOR UPDATE`,
			leaseID).Scan(&locked)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			// The lease died between ReportActivity's check and this write;
			// the revoke path owns the drain accounting now — drop the
			// event rather than resurrect a closed stream count.
			return nil
		case err != nil:
			return fmt.Errorf("broker: activity lease lock: %w", err)
		}
		if _, err := tx.Exec(ctx, q, wsUID, int64(gen), now); err != nil {
			return fmt.Errorf("broker: record activity: %w", err)
		}
		return nil
	})
}

// RequestStop is the generation-fenced entry point for stop decisions that
// did not travel the ordered intent stream — expiry planner output, admin
// actions. It re-checks runtimeGeneration against the workspace's CURRENT
// binding at request time: a stale generation is refused with
// ErrStaleBinding and has no side effects, so a delayed expiry can never
// stop a freshly restarted runtime. On success the stop intent is recorded
// durably for the lifecycle pipeline to drain.
func (b *Broker) RequestStop(ctx context.Context, workspaceUID PlatformID, runtimeGeneration uint64, reason StopReason) error {
	now := b.now()
	binding, err := b.currentBinding(ctx, workspaceUID, now)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return ErrStaleBinding
		}
		return err
	}
	if binding.RuntimeGeneration != runtimeGeneration {
		return ErrStaleBinding
	}
	_, err = b.db.Pool().Exec(ctx, `
		INSERT INTO stop_intent (workspace_id, runtime_generation, reason, deadline)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (workspace_id, runtime_generation, reason) DO NOTHING`,
		workspaceUID, int64(runtimeGeneration), string(reason), now)
	if err != nil {
		return fmt.Errorf("broker: record stop intent: %w", err)
	}
	return nil
}

// RevokeWorkspaceLeases revokes every live lease of the workspace bound to
// a runtime generation <= runtimeGeneration (the operator's observed
// generation) and records the revocation so no new ticket may be issued —
// and no outstanding ticket redeemed — for a generation the revocation
// covers. A lease pinned to a NEWER generation survives: a stale teardown
// never fences a freshly restarted runtime. Returns the revoked count.
//
// Revocation is authoritative for drain accounting: streams pinned
// to the revoked leases can never report their close — ReportActivity
// refuses a dead lease — so the covered generations' open_streams are
// zeroed in the same transaction and DrainStatus never waits on them. The
// disconnect grace anchor is set on the transition to zero, preserving the
// §8 grace-window semantics if the workspace is not actually torn down.
func (b *Broker) RevokeWorkspaceLeases(ctx context.Context, workspaceUID PlatformID, runtimeGeneration uint64) (int, error) {
	now := b.now()
	var n int64
	err := b.db.WithTx(ctx, func(tx store.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE connection_lease SET state = 'revoked', closed_at = $3
			WHERE workspace_id = $1 AND runtime_generation <= $2 AND state = 'active'`,
			workspaceUID, int64(runtimeGeneration), now)
		if err != nil {
			return fmt.Errorf("broker: revoke workspace leases: %w", err)
		}
		n = tag.RowsAffected()
		if _, err := tx.Exec(ctx, `
			INSERT INTO workspace_revocation (workspace_id, runtime_generation, revoked_at)
			SELECT $1, $2, $3 WHERE EXISTS (SELECT 1 FROM workspaces WHERE id = $1)
			ON CONFLICT DO NOTHING`, workspaceUID, int64(runtimeGeneration), now); err != nil {
			return fmt.Errorf("broker: record workspace revocation: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE workspace_activity SET
				open_streams = 0,
				disconnected_since = COALESCE(disconnected_since, $3),
				updated_at = $3
			WHERE workspace_id = $1 AND runtime_generation <= $2 AND open_streams > 0`,
			workspaceUID, int64(runtimeGeneration), now); err != nil {
			return fmt.Errorf("broker: close revoked streams: %w", err)
		}
		return nil
	})
	return int(n), err
}

// revokedGeneration reports whether a revocation covering the given
// runtime generation exists: a revocation for generation N blocks access
// bound to any generation <= N.
func (b *Broker) revokedGeneration(ctx context.Context, workspaceUID PlatformID, gen uint64) (bool, error) {
	var revoked bool
	err := b.db.Pool().QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM workspace_revocation
			WHERE workspace_id = $1 AND runtime_generation >= $2)`,
		workspaceUID, int64(gen)).Scan(&revoked)
	if err != nil {
		return false, fmt.Errorf("broker: revocation check: %w", err)
	}
	return revoked, nil
}

// DrainStatus reports the open interactive stream count for the workspace,
// summed across generations NOT covered by a workspace revocation.
// Revocation is authoritative for drain accounting: a stream pinned
// to a revoked generation can never report its close — a dead lease
// refuses ReportActivity and the gateway's tail flush is best-effort — so
// revoked generations count as closed even if a stale row still carries
// them. Generations that were never revoked still rely on the gateway's
// connected/disconnect reports; a crashed gateway may leak a count, which
// the finalizer's bounded drain window absorbs.
func (b *Broker) DrainStatus(ctx context.Context, workspaceUID PlatformID) (openStreams int, drained bool, err error) {
	err = b.db.Pool().QueryRow(ctx, `
		SELECT COALESCE(SUM(a.open_streams), 0)
		FROM workspace_activity a
		WHERE a.workspace_id = $1
		  AND NOT EXISTS (
			SELECT 1 FROM workspace_revocation r
			WHERE r.workspace_id = a.workspace_id
			  AND r.runtime_generation >= a.runtime_generation)`,
		workspaceUID).Scan(&openStreams)
	if err != nil {
		return 0, false, fmt.Errorf("broker: drain status: %w", err)
	}
	return openStreams, openStreams == 0, nil
}

// RunningWorkspace is one live runtime generation the planner must watch.
// The caller supplies it from Workspace CR status (status.startedAt is the
// generation's Ready time) plus the template-snapshot TimeoutPolicy; the
// planner joins it against recorded activity.
type RunningWorkspace struct {
	WorkspaceUID      PlatformID
	RuntimeGeneration uint64
	// StartedAt anchors the MaxDuration cap — the moment this generation
	// became Ready (server-side observation, never client input).
	StartedAt time.Time
	Policy    TimeoutPolicy
}

// RunningSource lists the live runtime generations the expiry planner must
// watch (production: the informer-backed projection in running_source.go).
type RunningSource interface {
	RunningWorkspaces(ctx context.Context) ([]RunningWorkspace, error)
}

// ExpiryPlanner turns recorded activity into stop intents. It shares the
// broker's store and clock, so tests drive it with the same fake clock as
// ReportActivity. It is pure evaluation — it never mutates a runtime.
type ExpiryPlanner struct {
	b *Broker
}

// NewExpiryPlanner returns the planner bound to this broker's activity
// records and clock.
func NewExpiryPlanner(b *Broker) *ExpiryPlanner {
	return &ExpiryPlanner{b: b}
}

// Scan evaluates the supplied running workspaces against recorded activity
// and returns one StopIntent per workspace whose deadlines are crossed at
// the broker's clock:
//
//   - no ActivityInput for Policy.IdleTimeout  -> idle_timeout
//   - disconnected for Policy.DisconnectTimeout -> disconnect_timeout
//   - generation age past Policy.MaxDuration    -> max_duration (wins over
//     the others: input does NOT extend the absolute cap)
//
// Each intent is pinned to the generation the activity was recorded under;
// the apply path re-checks it via RequestStop. A workspace reconnected
// inside its disconnect grace window produces no disconnect intent — the
// reconnect cancels the timer. Scan is idempotent and does not consume the
// intents it reports.
func (p *ExpiryPlanner) Scan(ctx context.Context, running []RunningWorkspace) ([]StopIntent, error) {
	now := p.b.now()
	var out []StopIntent
	for _, rw := range running {
		var (
			lastInput      *time.Time
			disconnSince   *time.Time
			lastTransition time.Time
		)
		err := p.b.db.Pool().QueryRow(ctx, `
			SELECT a.last_input_at, a.disconnected_since, w.updated_at
			FROM workspaces w
			LEFT JOIN workspace_activity a
				ON a.workspace_id = w.id AND a.runtime_generation = $2
			WHERE w.id = $1`,
			rw.WorkspaceUID, int64(rw.RuntimeGeneration)).Scan(&lastInput, &disconnSince, &lastTransition)
		if errors.Is(err, pgx.ErrNoRows) {
			continue // no workspace row: nothing to bound
		}
		if err != nil {
			return nil, fmt.Errorf("broker: activity scan %s: %w", rw.WorkspaceUID, err)
		}
		in := StopIntent{WorkspaceUID: rw.WorkspaceUID, RuntimeGeneration: rw.RuntimeGeneration}
		switch {
		case !now.Before(rw.StartedAt.Add(rw.Policy.MaxDuration)):
			in.Reason = StopReasonMaxDuration
			in.Deadline = rw.StartedAt.Add(rw.Policy.MaxDuration)
		case disconnSince != nil && !now.Before(disconnSince.Add(rw.Policy.DisconnectTimeout)):
			in.Reason = StopReasonDisconnectTimeout
			in.Deadline = disconnSince.Add(rw.Policy.DisconnectTimeout)
		default:
			// The idle clock anchors on the last recorded input. A
			// generation that never produced input anchors on its start —
			// the earlier of the caller-supplied Ready time and the
			// workspace row's last lifecycle transition: silence is not
			// keep-alive.
			anchor := rw.StartedAt
			if lastInput != nil {
				anchor = *lastInput
			} else if lastTransition.Before(anchor) {
				anchor = lastTransition
			}
			if !now.Before(anchor.Add(rw.Policy.IdleTimeout)) {
				in.Reason = StopReasonIdleTimeout
				in.Deadline = anchor.Add(rw.Policy.IdleTimeout)
			} else {
				continue
			}
		}
		out = append(out, in)
	}
	return out, nil
}

// DrainStops returns the stop intents recorded by RequestStop that the
// lifecycle pipeline has not yet consumed, in recording order, and marks
// them consumed. It is the delivery seam between the broker and the
// provisioning outbox.
func (p *ExpiryPlanner) DrainStops(ctx context.Context) ([]StopIntent, error) {
	var out []StopIntent
	err := p.b.db.WithTx(ctx, func(tx store.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id, workspace_id, runtime_generation, reason, deadline
			FROM stop_intent WHERE drained_at IS NULL ORDER BY id FOR UPDATE`)
		if err != nil {
			return err
		}
		var ids []int64
		for rows.Next() {
			var (
				id     int64
				in     StopIntent
				reason string
			)
			if err := rows.Scan(&id, &in.WorkspaceUID, &in.RuntimeGeneration,
				&reason, &in.Deadline); err != nil {
				rows.Close()
				return err
			}
			in.Reason = StopReason(reason)
			ids = append(ids, id)
			out = append(out, in)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if len(ids) > 0 {
			if _, err := tx.Exec(ctx,
				`UPDATE stop_intent SET drained_at = $2 WHERE id = ANY($1)`,
				ids, p.b.now()); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("broker: drain stops: %w", err)
	}
	return out, nil
}

// Sweep is one planner pass over the running set: deadlines become durable
// StopIntents via RequestStop (generation re-checked), then drained intents
// are emitted into the provisioning outbox as generation-fenced stops.
// Per-workspace failures are skipped — the next tick retries them — while
// systemic failures (source/scan/drain) propagate.
func (p *ExpiryPlanner) Sweep(ctx context.Context, src RunningSource) (int, error) {
	running, err := src.RunningWorkspaces(ctx)
	if err != nil {
		return 0, fmt.Errorf("broker: list running workspaces: %w", err)
	}
	intents, err := p.Scan(ctx, running)
	if err != nil {
		return 0, err
	}
	for _, in := range intents {
		_ = p.b.RequestStop(ctx, in.WorkspaceUID, in.RuntimeGeneration, in.Reason)
	}
	drained, err := p.DrainStops(ctx)
	if err != nil {
		return 0, err
	}
	emitted := 0
	for _, in := range drained {
		ok, err := p.b.emitStop(ctx, in)
		if err != nil {
			return emitted, fmt.Errorf("broker: emit stop %s gen %d: %w",
				in.WorkspaceUID, in.RuntimeGeneration, err)
		}
		if ok {
			emitted++
		}
	}
	return emitted, nil
}

// emitStop turns a drained stop intent into an outbox stop intent. The
// workspaces row is flipped to Stopped and the intent appended in ONE
// transaction — matching the SignalWorkspace contract — but only while the
// row still pins the intent's generation: a delayed expiry for generation
// N can never stop a runtime already restarted to N+1 (design §8). Never a
// direct CR mutation: delivery flows through provisioning.AppendIntent.
func (b *Broker) emitStop(ctx context.Context, in StopIntent) (bool, error) {
	var emitted bool
	err := b.db.WithTx(ctx, func(tx store.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE workspaces SET desired_state = 'Stopped', phase = 'Stopping', updated_at = $3
			WHERE id = $1 AND state = 'active' AND desired_state = 'Running'
			  AND runtime_generation = $2`,
			in.WorkspaceUID, int64(in.RuntimeGeneration), b.now())
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return nil // already stopped/deleted or a newer generation runs
		}
		_, err = provisioning.AppendIntent(ctx, tx, in.WorkspaceUID, provisioning.IntentStop)
		if err != nil {
			return err
		}
		emitted = true
		return nil
	})
	return emitted, err
}

// Run drives the periodic planner: every interval it sweeps the running
// set and emits due stop intents. It returns when ctx is cancelled.
func (p *ExpiryPlanner) Run(ctx context.Context, src RunningSource, interval time.Duration, log *slog.Logger) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		n, err := p.Sweep(ctx, src)
		if err != nil && log != nil && !errors.Is(err, context.Canceled) {
			log.Warn("expiry sweep failed", "err", err)
		}
		if n > 0 && log != nil {
			log.Info("expiry emitted stop intents", "count", n)
		}
	}
}
