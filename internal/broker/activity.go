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

// ErrGenerationUnbounded is returned when a workspace revocation names a
// runtimeGeneration beyond the workspace's recorded generation plus one
// (the observed-but-not-yet-projected slack). A revocation row covers
// every generation <= N permanently — the table has no delete path — so
// an unbounded value would fence every future runtime the workspace will
// ever have. The internal API maps it to 400 INVALID_REQUEST.
var ErrGenerationUnbounded = errors.New("broker: runtime generation beyond recorded bound")

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
	Type ActivityEventType
	// StreamEpoch is the lease stream epoch the reporting stream claimed
	// (the value ClaimStream returned; 0 when the gateway has no session
	// directory). It makes stream accounting epoch-aware: a report from a
	// stream an older epoch already fenced is ignored.
	StreamEpoch uint64
	ReceivedAt  time.Time // server receipt time; broker-assigned, client input ignored
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
	l, state, _, err := b.loadLease(ctx, leaseID)
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
	if err := b.recordActivity(ctx, l.ID, PlatformID(l.WorkspaceUID), l.RuntimeGeneration, ev, now); err != nil {
		return err
	}
	// Desktop input extends the owning session's PORTAL idle timer (D18):
	// the hook receives the lease's principal and the bound
	// portal_session_digest — input credits exactly the session the
	// stream was launched under (SR-1-F3); a legacy NULL digest degrades
	// to the principal-wide touch — after the event is durably recorded.
	if ev.Type == ActivityInput && b.inputHook != nil {
		b.inputHook(ctx, l.PrincipalSubject, l.PortalSessionDigest)
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
//
// Stream accounting is epoch-aware: ClaimStream zeroes the generation's
// open_streams (the previous stream is fenced by definition), so "connected"
// SETS the count to 1 for the claiming epoch rather than adding to a count a
// hard-killed replica never decremented (epoch-0 reports, sent when no
// session directory is wired, keep the +1/-1 arithmetic), and a connected/disconnect report
// from an epoch older than the lease's current stream_epoch belongs to a
// fenced stream and is dropped.
func (b *Broker) recordActivity(ctx context.Context, leaseID string, wsUID PlatformID, gen uint64, ev ActivityEvent, now time.Time) error {
	t := ev.Type
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
		// does not keep ticking underneath (design §8). Epoch >= 1 reports
		// SET the count (ClaimStream already zeroed it); epoch 0 means no
		// session directory is wired (split mode), nothing claims streams,
		// and the +1/-1 arithmetic is all there is.
		q = `INSERT INTO workspace_activity
			(workspace_id, runtime_generation, connected_at, open_streams, updated_at)
		 VALUES ($1, $2, $3, 1, $3)
		 ON CONFLICT (workspace_id, runtime_generation) DO UPDATE SET
			connected_at = EXCLUDED.connected_at,
			open_streams = 1,
			disconnected_since = NULL,
			updated_at = EXCLUDED.updated_at`
		if ev.StreamEpoch == 0 {
			q = `INSERT INTO workspace_activity
				(workspace_id, runtime_generation, connected_at, open_streams, updated_at)
			 VALUES ($1, $2, $3, 1, $3)
			 ON CONFLICT (workspace_id, runtime_generation) DO UPDATE SET
				connected_at = EXCLUDED.connected_at,
				open_streams = workspace_activity.open_streams + 1,
				disconnected_since = NULL,
				updated_at = EXCLUDED.updated_at`
		}
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
		var currentEpoch uint64
		err := tx.QueryRow(ctx,
			`SELECT stream_epoch FROM connection_lease WHERE id = $1 AND state = 'active' FOR UPDATE`,
			leaseID).Scan(&currentEpoch)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			// The lease died between ReportActivity's check and this write;
			// the revoke path owns the drain accounting now — drop the
			// event rather than resurrect a closed stream count.
			return nil
		case err != nil:
			return fmt.Errorf("broker: activity lease lock: %w", err)
		}
		if t != ActivityInput && ev.StreamEpoch < currentEpoch {
			// A fenced stream's report: a newer ClaimStream already
			// accounted for it, so it must not touch the live stream's count.
			return nil
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
//
// The fence itself is bounded: runtimeGeneration must not exceed the
// workspace's recorded generation plus one (checkRevokeBound), else
// ErrGenerationUnbounded — a revocation row is undeletable, so covering a
// generation that was never recorded would brick the workspace.
func (b *Broker) RevokeWorkspaceLeases(ctx context.Context, workspaceUID PlatformID, runtimeGeneration uint64) (int, error) {
	now := b.now()
	var n int64
	err := b.db.WithTx(ctx, func(tx store.Tx) error {
		if err := b.checkRevokeBound(ctx, tx, workspaceUID, runtimeGeneration); err != nil {
			return err
		}
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

// checkRevokeBound refuses a workspace revocation whose runtimeGeneration
// exceeds every generation the broker has recorded for the workspace plus
// one of observed-but-not-yet-projected slack. The bound is the highest
// generation either authoritative view reports: workspaces.runtime_generation
// (the mint counter, bumped inside the same transaction as AppendIntent)
// and the informer binding's observed generation — which covers a
// generation the CR already recorded but the row projection has not
// applied yet (DR skew, projection lag). Generation minting is monotonic,
// so a bound read mid-bump can only err toward refusal, never toward a
// wider fence. A binding lookup failure narrows nothing — the row bound
// still applies.
//
// When neither view knows the workspace the call keeps its historical
// no-op shape: the revocation insert below is EXISTS-guarded on the
// workspaces row, so no fence can ever be written for it — and no leases,
// tickets or activity rows can exist either (all reference the workspaces
// row), so the remaining statements are already no-ops.
func (b *Broker) checkRevokeBound(ctx context.Context, tx store.Tx, workspaceUID PlatformID, runtimeGeneration uint64) error {
	var (
		recorded  uint64
		haveBound bool
		rowGen    int64
	)
	switch err := tx.QueryRow(ctx,
		`SELECT runtime_generation FROM workspaces WHERE id = $1`,
		workspaceUID).Scan(&rowGen); {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return fmt.Errorf("broker: workspace generation lookup: %w", err)
	default:
		// runtime_generation is >= 0 by CHECK; the cast is exact.
		recorded, haveBound = uint64(rowGen), true
	}
	if b.src != nil {
		if binding, err := b.src.CurrentBinding(ctx, workspaceUID); err == nil &&
			binding.RuntimeGeneration > recorded {
			recorded, haveBound = binding.RuntimeGeneration, true
		}
	}
	// recorded <= max int64 on every path (row CHECK + CRD int64 fields),
	// so recorded+1 cannot wrap.
	if haveBound && runtimeGeneration > recorded+1 {
		if b.log != nil {
			b.log.Warn("workspace revocation refused: runtime generation beyond bound",
				"workspace", string(workspaceUID),
				"runtime_generation", runtimeGeneration,
				"recorded_generation", recorded,
				"request_id", requestID(ctx))
		}
		return ErrGenerationUnbounded
	}
	return nil
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

// OperatorStopped is a workspace whose operator stopped the applied intent
// on its own: the Workspace CR still carries the Running intent the
// workspaces row pinned (spec.intentRevision == the applied record's
// revision) but the applied record says Stopped. The workspaces row never
// heard about it, so without reconciliation it stays Running with the quota
// reservation held and a Start is a no-op.
type OperatorStopped struct {
	WorkspaceUID      PlatformID
	RuntimeGeneration uint64
	// IntentRevision is the outbox revision the CR was last handed.
	IntentRevision uint64
}

// OperatorStoppedSource is implemented by a RunningSource that can also list
// workspaces the operator stopped out-of-band (production: K8sRunningSource).
type OperatorStoppedSource interface {
	OperatorStoppedWorkspaces(ctx context.Context) ([]OperatorStopped, error)
}

// ExpiryPlanner turns recorded activity into stop intents. It shares the
// broker's store and clock, so tests drive it with the same fake clock as
// ReportActivity. It is pure evaluation — it never mutates a runtime.
type ExpiryPlanner struct {
	b *Broker

	// afterPending, when set, runs after the pending stop intents were
	// listed and before they are emitted. A cancel inside simulates the
	// leader losing its lock in that window. Test hook only.
	afterPending func()
}

// ExpiryPlannerOption tunes an ExpiryPlanner.
type ExpiryPlannerOption func(*ExpiryPlanner)

// WithAfterPendingHook installs the cancel-simulation hook.
func WithAfterPendingHook(h func()) ExpiryPlannerOption {
	return func(p *ExpiryPlanner) { p.afterPending = h }
}

// NewExpiryPlanner returns the planner bound to this broker's activity
// records and clock.
func NewExpiryPlanner(b *Broker, opts ...ExpiryPlannerOption) *ExpiryPlanner {
	p := &ExpiryPlanner{b: b}
	for _, o := range opts {
		o(p)
	}
	return p
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

// PendingStops returns the stop intents recorded by RequestStop that the
// lifecycle pipeline has not yet consumed, in recording order. Reading is
// not consuming: a row is marked drained inside the same transaction that
// emits it or proves it moot, so a sweep interrupted between listing and
// emitting replays the intent on the next pass instead of losing it.
func (p *ExpiryPlanner) PendingStops(ctx context.Context) ([]StopIntent, error) {
	rows, err := p.b.db.Pool().Query(ctx, `
		SELECT workspace_id, runtime_generation, reason, deadline
		FROM stop_intent WHERE drained_at IS NULL ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("broker: list pending stops: %w", err)
	}
	defer rows.Close()
	var out []StopIntent
	for rows.Next() {
		var in StopIntent
		var reason string
		if err := rows.Scan(&in.WorkspaceUID, &in.RuntimeGeneration,
			&reason, &in.Deadline); err != nil {
			return nil, err
		}
		in.Reason = StopReason(reason)
		out = append(out, in)
	}
	return out, rows.Err()
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
	pending, err := p.PendingStops(ctx)
	if err != nil {
		return 0, err
	}
	if p.afterPending != nil {
		p.afterPending()
	}
	emitted := 0
	for _, in := range pending {
		ok, err := p.b.emitStop(ctx, in)
		if err != nil {
			return emitted, fmt.Errorf("broker: emit stop %s gen %d: %w",
				in.WorkspaceUID, in.RuntimeGeneration, err)
		}
		if ok {
			emitted++
		}
	}
	if oss, ok := src.(OperatorStoppedSource); ok {
		n, err := p.reconcileOperatorStops(ctx, oss)
		emitted += n
		if err != nil {
			return emitted, err
		}
	}
	return emitted, nil
}

// reconcileOperatorStops brings the workspaces row in line with a stop the
// operator applied by itself. It goes through emitStop, so the row flips to
// Stopped and the stop intent is appended in one transaction, fenced on the
// row still being Running at the same runtime generation: a Stop, Start or
// Delete that landed in the meantime changes desired_state, the generation
// or the state and wins. On top of that, the row's intent revision must
// still be the one the CR holds: an intent appended but not yet dispatched
// wins too.
func (p *ExpiryPlanner) reconcileOperatorStops(ctx context.Context, src OperatorStoppedSource) (int, error) {
	stopped, err := src.OperatorStoppedWorkspaces(ctx)
	if err != nil {
		return 0, fmt.Errorf("broker: list operator-stopped workspaces: %w", err)
	}
	emitted := 0
	for _, os := range stopped {
		var latest int64
		if err := p.b.db.Pool().QueryRow(ctx,
			`SELECT intent_revision FROM workspaces WHERE id = $1`,
			os.WorkspaceUID).Scan(&latest); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			return emitted, fmt.Errorf("broker: intent revision %s: %w", os.WorkspaceUID, err)
		}
		if latest != int64(os.IntentRevision) {
			continue // the row moved on; the CR is not at the head
		}
		ok, err := p.b.emitStop(ctx, StopIntent{
			WorkspaceUID:      os.WorkspaceUID,
			RuntimeGeneration: os.RuntimeGeneration,
			Reason:            StopReasonMaxDuration,
			Deadline:          p.b.now(),
		})
		if err != nil {
			return emitted, fmt.Errorf("broker: emit stop %s gen %d: %w",
				os.WorkspaceUID, os.RuntimeGeneration, err)
		}
		if ok {
			emitted++
		}
	}
	return emitted, nil
}

// emitStop turns a recorded stop intent into an outbox stop intent and
// marks the stop_intent row consumed in the SAME transaction — whether the
// intent emitted or the workspaces row already moved past it (fenced out:
// already stopped/deleted, or a newer generation running). Consumption is
// atomic with the outcome: a crash or cancel can never leave a stop
// drained-but-unemitted, which the old drain-first order could — and the
// (workspace, generation, reason) uniqueness meant that stop could never
// be re-recorded. The workspaces row is flipped to Stopped and the intent
// appended while the row still pins the intent's generation (design §8);
// never a direct CR mutation: delivery flows through
// provisioning.AppendIntent.
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
		if tag.RowsAffected() > 0 {
			rev, err := provisioning.AppendIntent(ctx, tx, in.WorkspaceUID, provisioning.IntentStop)
			if err != nil {
				return err
			}
			// The cause stays with the intent so the workspace events can
			// say why the workspace stopped.
			if in.Reason != "" {
				if err := provisioning.SetIntentReason(ctx, tx, in.WorkspaceUID, rev, string(in.Reason)); err != nil {
					return err
				}
			}
			emitted = true
		}
		// A recorded intent is consumed with its outcome — emitted or
		// moot. Synthetic emits (the operator-stopped reconciliation)
		// carry no stop_intent row, so this UPDATE is a no-op for them —
		// or it drains a recorded twin of the stop just emitted, which is
		// equally moot.
		_, err = tx.Exec(ctx, `
			UPDATE stop_intent SET drained_at = $4
			WHERE workspace_id = $1 AND runtime_generation = $2 AND reason = $3
			  AND drained_at IS NULL`,
			in.WorkspaceUID, int64(in.RuntimeGeneration), string(in.Reason), b.now())
		return err
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
