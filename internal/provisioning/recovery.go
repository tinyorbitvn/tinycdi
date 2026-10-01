package provisioning

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tinyorbitvn/tinycdi/internal/store"
)

// ErrRuntimeNotProvenGone refuses quota settlement while the runtime's
// absence is unproven. Quota is released only on positive proof — a missing
// observation, an HTTP timeout, or an observer error frees nothing
// (design §8: reservation held until workload proven not consuming compute).
var ErrRuntimeNotProvenGone = errors.New("provisioning: runtime absence not proven")

// MaxIntentApplyAttempts bounds how many times recovery re-drives a
// pending intent before quarantining it for operator attention. It prevents
// an infinite retry loop from repeatedly attempting to create a new
// runtime for a poison intent.
const MaxIntentApplyAttempts = 10

// RuntimeObserver reports whether a workspace's runtime still consumes
// compute. The production implementation reads the Workspace CR status and
// operator inventory; tests inject fakes. An ambiguous answer (error) must
// always be treated as "not proven gone" — never as gone.
type RuntimeObserver interface {
	RuntimeGone(ctx context.Context, workspaceUID PlatformID) (bool, error)
}

// RecoveryAction describes one piece of repair the recovery pass decided.
// It is a read model for tests and observability; the mutations happen
// inside Recover's transactions.
type RecoveryAction struct {
	WorkspaceUID PlatformID
	Kind         RecoveryActionKind
	Detail       string
}

// RecoveryActionKind classifies a recovery decision.
type RecoveryActionKind string

const (
	// ActionRedeliverIntent — an undispatched outbox intent was re-driven
	// (dispatcher died between apply and MarkDispatched, or the API
	// crashed before dispatch).
	ActionRedeliverIntent RecoveryActionKind = "redeliver_intent"
	// ActionReleaseQuota — the runtime is proven gone and the held
	// reservation was released in-transaction.
	ActionReleaseQuota RecoveryActionKind = "release_quota"
	// ActionHoldQuota — the reservation stays held: runtime still present
	// or absence unproven.
	ActionHoldQuota RecoveryActionKind = "hold_quota"
	// ActionQuarantineIntent — an intent exceeded MaxIntentApplyAttempts
	// and was parked for manual attention instead of looping forever.
	ActionQuarantineIntent RecoveryActionKind = "quarantine_intent"
)

// Recovery rebuilds control-plane invariants after an API or operator
// restart. It is level-based: every decision derives from the workspace
// row, the outbox and a positive runtime-absence observation — never from
// in-memory state the crashed process may have held.
type Recovery struct {
	db  *store.DB
	obs RuntimeObserver
	// MaxApplyAttempts overrides MaxIntentApplyAttempts; <=0 uses the default.
	MaxApplyAttempts int
	// Now injects the clock; nil uses time.Now.
	Now func() time.Time
}

// NewRecovery binds a Recovery pass to db and the runtime observer.
func NewRecovery(db *store.DB, obs RuntimeObserver) *Recovery {
	return &Recovery{db: db, obs: obs, Now: time.Now}
}

func (r *Recovery) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Recovery) maxAttempts() int64 {
	if r.MaxApplyAttempts > 0 {
		return int64(r.MaxApplyAttempts)
	}
	return MaxIntentApplyAttempts
}

// pendingIntent is one undispatched outbox row plus its persisted recovery
// bookkeeping (apply attempts and quarantine flag, carried inside the
// intent payload so no schema change is needed and the state survives
// restarts).
type pendingIntent struct {
	Intent
	Attempts    int64
	Quarantined bool
}

// intentRecovery is the bookkeeping envelope nested under the payload's
// "recovery" key.
type intentRecovery struct {
	Attempts    int64 `json:"attempts,omitempty"`
	Quarantined bool  `json:"quarantined,omitempty"`
}

// decodeIntent reassembles an Intent from its outbox row, including the
// snapshotted payload fields, plus recovery bookkeeping.
func decodeIntent(wsID PlatformID, tenantID string, rev uint64, kind, requestID string, payload []byte) (pendingIntent, error) {
	pi := pendingIntent{Intent: Intent{
		WorkspaceUID: wsID, TenantID: tenantID,
		Revision: rev, Kind: IntentKind(kind), RequestID: requestID,
	}}
	if len(payload) == 0 {
		return pi, nil
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(payload, &raw); err != nil {
		return pi, fmt.Errorf("outbox payload %s rev %d: %w", wsID, rev, err)
	}
	var p intentPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return pi, fmt.Errorf("outbox payload %s rev %d: %w", wsID, rev, err)
	}
	pi.DesiredState = p.DesiredState
	pi.RuntimeGeneration = p.RuntimeGeneration
	pi.Spec = p.Spec
	if rec, ok := raw["recovery"]; ok {
		var m intentRecovery
		if err := json.Unmarshal(rec, &m); err != nil {
			return pi, fmt.Errorf("outbox payload %s rev %d recovery: %w", wsID, rev, err)
		}
		pi.Attempts = m.Attempts
		pi.Quarantined = m.Quarantined
	}
	return pi, nil
}

// pendingIntents lists every undispatched intent — including quarantined
// ones, so the pass can skip them deliberately — in stream order.
func (r *Recovery) pendingIntents(ctx context.Context) ([]pendingIntent, error) {
	rows, err := r.db.Pool().Query(ctx, `
		SELECT workspace_id, tenant_id, revision, kind, request_id, payload
		FROM outbox_intent
		WHERE dispatched_at IS NULL
		ORDER BY workspace_id, revision`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []pendingIntent
	for rows.Next() {
		var wsID PlatformID
		var tenantID, kind, requestID string
		var rev uint64
		var payload []byte
		if err := rows.Scan(&wsID, &tenantID, &rev, &kind, &requestID, &payload); err != nil {
			return nil, err
		}
		pi, err := decodeIntent(wsID, tenantID, rev, kind, requestID, payload)
		if err != nil {
			return nil, err
		}
		out = append(out, pi)
	}
	return out, rows.Err()
}

// quarantinedSQL excludes quarantined intents from candidate sets: a
// parked intent is parked until a human unparks it.
const quarantinedSQL = `COALESCE(payload->'recovery'->>'quarantined', 'false') <> 'true'`

// PendingRecovery lists workspace UIDs whose post-crash state needs
// recovery action: held reservations on workspaces whose lifecycle is
// terminal (deleted/stopped) or pending intents that never dispatched.
// Quarantined intents are parked for manual attention and do not keep a
// workspace in this list.
func (r *Recovery) PendingRecovery(ctx context.Context) ([]string, error) {
	rows, err := r.db.Pool().Query(ctx, `
		SELECT workspace_id FROM outbox_intent
		WHERE dispatched_at IS NULL AND `+quarantinedSQL+`
		UNION
		SELECT qr.workspace_id
		FROM quota_reservation qr JOIN workspaces w ON w.id = qr.workspace_id
		WHERE qr.state = 'held'
		  AND (w.state = 'deleted' OR w.desired_state = 'Stopped')
		ORDER BY 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var uids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		uids = append(uids, id)
	}
	return uids, rows.Err()
}

// pendingCreateIntent reports whether a create intent for the workspace
// never dispatched — proof the runtime layer never saw it.
func (r *Recovery) pendingCreateIntent(ctx context.Context, workspaceUID PlatformID) (bool, error) {
	var exists bool
	err := r.db.Pool().QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM outbox_intent
			WHERE workspace_id = $1 AND kind = 'create' AND dispatched_at IS NULL)`,
		workspaceUID).Scan(&exists)
	return exists, err
}

// SettleQuota releases the workspace's held reservation if and only if the
// observer proves the runtime is gone. Proof is mapped to AbsenceProof:
// observed-absent -> ProofRuntimeAbsent; a workspace whose create intent
// never dispatched -> ProofNeverCreated. Any observer error or
// still-present report returns ErrRuntimeNotProvenGone and releases
// nothing. It is idempotent: an already-released reservation is a no-op.
func (r *Recovery) SettleQuota(ctx context.Context, tenantID string, workspaceUID PlatformID) error {
	// A never-dispatched create intent is the strongest absence proof: no
	// runtime was ever materialized.
	pendingCreate, err := r.pendingCreateIntent(ctx, workspaceUID)
	if err != nil {
		return err
	}
	proof := ProofRuntimeAbsent
	if pendingCreate {
		proof = ProofNeverCreated
	} else {
		if r.obs == nil {
			return ErrRuntimeNotProvenGone
		}
		gone, err := r.obs.RuntimeGone(ctx, workspaceUID)
		if err != nil || !gone {
			// Ambiguity never frees quota.
			return ErrRuntimeNotProvenGone
		}
	}
	return r.db.WithTx(ctx, func(tx store.Tx) error {
		return Release(ctx, tx, tenantID, string(workspaceUID), proof)
	})
}

// recordApplyFailure bumps the persisted apply-attempts counter on the
// intent and quarantines it once the bound is reached. It returns the
// attempt count and whether the intent is now quarantined.
func (r *Recovery) recordApplyFailure(ctx context.Context, in pendingIntent) (attempts int64, quarantined bool, err error) {
	var payload []byte
	err = r.db.Pool().QueryRow(ctx, `
		SELECT payload FROM outbox_intent
		WHERE workspace_id = $1 AND revision = $2 AND dispatched_at IS NULL`,
		in.WorkspaceUID, in.Revision).Scan(&payload)
	if errors.Is(err, pgx.ErrNoRows) {
		return in.Attempts, in.Quarantined, nil // already dispatched elsewhere
	}
	if err != nil {
		return 0, false, err
	}
	raw := map[string]json.RawMessage{}
	if len(payload) > 0 {
		if err := json.Unmarshal(payload, &raw); err != nil {
			return 0, false, fmt.Errorf("outbox payload %s rev %d: %w", in.WorkspaceUID, in.Revision, err)
		}
	}
	var m intentRecovery
	if rec, ok := raw["recovery"]; ok {
		if err := json.Unmarshal(rec, &m); err != nil {
			return 0, false, fmt.Errorf("outbox payload %s rev %d recovery: %w", in.WorkspaceUID, in.Revision, err)
		}
	}
	m.Attempts++
	if m.Attempts >= r.maxAttempts() {
		m.Quarantined = true
	}
	recJSON, err := json.Marshal(m)
	if err != nil {
		return 0, false, err
	}
	raw["recovery"] = recJSON
	newPayload, err := json.Marshal(raw)
	if err != nil {
		return 0, false, err
	}
	if _, err := r.db.Pool().Exec(ctx, `
		UPDATE outbox_intent SET payload = $3
		WHERE workspace_id = $1 AND revision = $2`,
		in.WorkspaceUID, in.Revision, newPayload); err != nil {
		return 0, false, err
	}
	return m.Attempts, m.Quarantined, nil
}

// Recover runs one full recovery pass: re-delivers pending intents through
// applier (bounded by MaxIntentApplyAttempts, then quarantined) and settles
// quota for terminal workspaces whose runtime is proven gone. It returns
// the actions taken so tests and operators can audit the pass. Re-delivered
// intents are marked dispatched only after a successful apply — a pass
// interrupted mid-apply leaves the intent pending for the next one.
func (r *Recovery) Recover(ctx context.Context, applier WorkspaceApplier) ([]RecoveryAction, error) {
	var actions []RecoveryAction

	// --- re-drive pending intents, per-workspace stream order ------------
	pending, err := r.pendingIntents(ctx)
	if err != nil {
		return nil, err
	}
	broken := map[PlatformID]bool{} // streams already parked/failed this pass
	for _, in := range pending {
		if broken[in.WorkspaceUID] {
			continue // never jump ahead of a failed/parked revision
		}
		if in.Quarantined {
			broken[in.WorkspaceUID] = true
			continue
		}
		if err := applier.Apply(ctx, in.Intent); err != nil {
			attempts, quarantined, rerr := r.recordApplyFailure(ctx, in)
			if rerr != nil {
				return actions, rerr
			}
			if quarantined {
				actions = append(actions, RecoveryAction{
					WorkspaceUID: in.WorkspaceUID,
					Kind:         ActionQuarantineIntent,
					Detail: fmt.Sprintf("%s intent rev %d parked after %d apply attempts",
						in.Kind, in.Revision, attempts),
				})
			}
			broken[in.WorkspaceUID] = true
			continue
		}
		if _, err := r.db.Pool().Exec(ctx, `
			UPDATE outbox_intent SET dispatched_at = now()
			WHERE workspace_id = $1 AND revision = $2`,
			in.WorkspaceUID, in.Revision); err != nil {
			return actions, fmt.Errorf("mark dispatched %s rev %d: %w", in.WorkspaceUID, in.Revision, err)
		}
		actions = append(actions, RecoveryAction{
			WorkspaceUID: in.WorkspaceUID,
			Kind:         ActionRedeliverIntent,
			Detail:       fmt.Sprintf("%s intent rev %d re-delivered", in.Kind, in.Revision),
		})
	}

	// --- settle quota on terminal workspaces -----------------------------
	type heldRes struct {
		ws     PlatformID
		tenant string
	}
	rows, err := r.db.Pool().Query(ctx, `
		SELECT qr.workspace_id, qr.tenant_id
		FROM quota_reservation qr JOIN workspaces w ON w.id = qr.workspace_id
		WHERE qr.state = 'held'
		  AND (w.state = 'deleted' OR w.desired_state = 'Stopped')
		ORDER BY qr.workspace_id`)
	if err != nil {
		return actions, err
	}
	var held []heldRes
	for rows.Next() {
		var h heldRes
		if err := rows.Scan(&h.ws, &h.tenant); err != nil {
			rows.Close()
			return actions, err
		}
		held = append(held, h)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return actions, err
	}
	for _, h := range held {
		err := r.SettleQuota(ctx, h.tenant, h.ws)
		switch {
		case err == nil:
			actions = append(actions, RecoveryAction{
				WorkspaceUID: h.ws, Kind: ActionReleaseQuota,
				Detail: "held reservation released on proven runtime absence",
			})
		case errors.Is(err, ErrRuntimeNotProvenGone):
			actions = append(actions, RecoveryAction{
				WorkspaceUID: h.ws, Kind: ActionHoldQuota,
				Detail: "runtime absence unproven; reservation stays held",
			})
		default:
			return actions, err
		}
	}
	return actions, nil
}
