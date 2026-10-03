package provisioning

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tinyorbitvn/tinycdi/internal/store"
)

// Service composes idempotency, quota reservations and the outbox into
// the atomic create/lifecycle write path.
type Service struct {
	db       *store.DB
	retained *RetainedStore
	// templates resolves template revisions/families for the OnStart
	// re-point; nil keeps the workspace on its recorded revision (tests).
	templates TemplateLookup
	log       *slog.Logger
}

// NewService wraps db.
func NewService(db *store.DB) *Service {
	return &Service{db: db, retained: NewRetainedStore(db)}
}

// WithTemplateLookup attaches the template catalog the start path consults
// to re-point a workspace onto the newest published revision of its
// template family (lifecycle.imageUpdate = OnStart; E1).
func (s *Service) WithTemplateLookup(l TemplateLookup) *Service {
	s.templates = l
	return s
}

// WithLogger attaches a logger for lifecycle warnings; nil keeps
// slog.Default.
func (s *Service) WithLogger(l *slog.Logger) *Service {
	s.log = l
	return s
}

var (
	// ErrInvalidState is returned when the workspace's lifecycle state does
	// not allow the requested transition.
	ErrInvalidState = errors.New("workspace state does not allow this operation")
	// ErrNameTaken is returned when the owner already has a live workspace
	// with that display name.
	ErrNameTaken = errors.New("workspace name already in use by this owner")
)

// TemplateInfo is the catalog entry snapshot captured at create time and
// refreshed by a start that moves the workspace to a newer revision.
type TemplateInfo struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Revision int64  `json:"revision"`
	// RevisionLabel is the raw spec.revision identifier ("2026-10-b");
	// absent on rows written before v0.3.
	RevisionLabel string `json:"revisionLabel,omitempty"`
	Runtime       string `json:"runtime"`
	Experience    string `json:"experience"`
	// ImageBuiltAt is the template's raw image-built-at annotation value
	// (RFC 3339 when well formed), snapshotted at create time.
	ImageBuiltAt string `json:"imageBuiltAt,omitempty"`
}

// WorkspaceRecord is the API-facing read model of a workspace row.
type WorkspaceRecord struct {
	ID              string         `json:"id"`
	TenantID        string         `json:"tenantId"`
	Owner           string         `json:"owner"` // issuer|sub
	OwnerIssuer     string         `json:"ownerIssuer"`
	OwnerSub        string         `json:"ownerSub"`
	Name            string         `json:"name"`
	Template        TemplateInfo   `json:"template"`
	Vector          ResourceVector `json:"vector"`
	DesiredState    string         `json:"desiredState"`
	DataPolicy      string         `json:"dataPolicy"`
	Phase           string         `json:"phase"`
	FailureReason   string         `json:"failureReason,omitempty"`
	RetainedDataRef string         `json:"retainedDataRef,omitempty"`
	RequestID       string         `json:"requestId"`
	Revision        uint64         `json:"revision"`
	Replayed        bool           `json:"replayed,omitempty"`
	CreatedAt       time.Time      `json:"createdAt"`
	UpdatedAt       time.Time      `json:"updatedAt"`
}

// CreateRequest is the admission input for a workspace create. The owner
// identity comes from the verified principal, never from the body.
type CreateRequest struct {
	OwnerIssuer     string         `json:"ownerIssuer"`
	OwnerSubject    string         `json:"ownerSubject"`
	Name            string         `json:"name"`
	Template        TemplateInfo   `json:"template"`
	Vector          ResourceVector `json:"vector"`
	DesiredState    string         `json:"desiredState"`
	DataPolicy      string         `json:"dataPolicy"`
	RetainedDataRef string         `json:"retainedDataRef,omitempty"`
}

// CreateWorkspace atomically: binds the idempotency key, inserts the
// workspace row, reserves quota and appends the create intent. bodyHash is
// the caller-computed fingerprint of the raw request body. Replaying the
// same key with the same body returns the stored record; the same key with
// a different body fails with ErrIdempotencyConflict.
func (s *Service) CreateWorkspace(ctx context.Context, tenantID, idemKey string, req CreateRequest, bodyHash []byte) (res WorkspaceRecord, err error) {
	if req.RetainedDataRef != "" {
		// SEC-01: a create carrying retainedDataRef must go through the
		// claimed attach path (AttachRetained), which is the only place the
		// record's owner/state are checked and the Retained->Attaching
		// claim is taken atomically.
		return res, errors.New("create: retainedDataRef requires the attach path")
	}
	desired := req.DesiredState
	if desired == "" {
		desired = "Stopped"
	}
	if desired != "Running" && desired != "Stopped" {
		return res, fmt.Errorf("create: bad desiredState %q: %w", desired, ErrInvalidState)
	}
	dataPolicy := req.DataPolicy
	if dataPolicy == "" {
		dataPolicy = "Ephemeral"
	}
	var gen int64
	if desired == "Running" {
		gen = 1
	}

	err = s.db.WithTx(ctx, func(tx store.Tx) error {
		// SEC-21: idempotency keys are scoped to the authenticated
		// principal — the same client key under another owner is an
		// independent namespace and can never replay their result.
		scopedKey := scopedIdemKey(req.OwnerIssuer+"|"+req.OwnerSubject, idemKey)
		rec, replayed, err := BeginIdempotent(ctx, tx, tenantID, scopedKey, "create", bodyHash)
		if err != nil {
			return err
		}
		if replayed {
			if err := json.Unmarshal(rec.Result, &res); err != nil {
				return fmt.Errorf("create: stored result %w", err)
			}
			res.Replayed = true
			return nil
		}

		res.ID = newWorkspaceID()
		res.RequestID = rec.RequestID
		tpl, _ := json.Marshal(req.Template)
		var retained *string
		if req.RetainedDataRef != "" {
			retained = &req.RetainedDataRef
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO workspaces (id, tenant_id, owner_subject, owner_issuer, owner_sub,
				request_id, name, template, data_policy, desired_state,
				runtime_generation, phase, retained_data_ref)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, 'Pending', $12)`,
			res.ID, tenantID, req.OwnerIssuer+"|"+req.OwnerSubject, req.OwnerIssuer, req.OwnerSubject,
			rec.RequestID, req.Name, tpl, dataPolicy, desired, gen, retained); err != nil {
			if isUniqueViolation(err) {
				return ErrNameTaken
			}
			return fmt.Errorf("create: workspace %w", err)
		}
		if err := Reserve(ctx, tx, tenantID, res.ID, req.Vector); err != nil {
			return err
		}
		rev, err := appendIntent(ctx, tx, PlatformID(res.ID), IntentCreate, &IntentSpec{
			WorkspaceName:   req.Name,
			TemplateName:    req.Template.Name,
			OwnerIssuer:     req.OwnerIssuer,
			OwnerSubject:    req.OwnerSubject,
			DataPolicy:      dataPolicy,
			RetainedDataRef: req.RetainedDataRef,
			ImageBuiltAt:    req.Template.ImageBuiltAt,
		})
		if err != nil {
			return err
		}
		res.Revision = rev
		out, err := getWorkspaceTx(ctx, tx, tenantID, "", res.ID)
		if err != nil {
			return err
		}
		res = *out
		return CompleteIdempotent(ctx, tx, tenantID, scopedKey, res)
	})
	return res, err
}

// GetWorkspace returns one workspace. ownerScope, when non-empty, restricts
// to records owned by that owner; a foreign ID yields ErrWorkspaceNotFound
// so existence is not leaked.
//
// A deleted workspace stays readable (phase Terminating) while its runtime
// is still being torn down and answers ErrWorkspaceNotFound once the runtime
// is proven gone; see visibleWorkspaceSQL.
func (s *Service) GetWorkspace(ctx context.Context, tenantID, ownerScope, id string) (WorkspaceRecord, error) {
	rec, err := getWorkspace(ctx, s.db.Pool(), tenantID, ownerScope, id, true)
	if err != nil {
		return WorkspaceRecord{}, err
	}
	return *rec, nil
}

// visibleWorkspaceSQL is the read-side visibility rule for queries over the
// workspaces table (columns are qualified with the table name). A deleted row is a tombstone: the delete
// intent flips state to 'deleted' and phase to 'Terminating' and nothing
// rewrites the phase afterwards. The row is shown as "Deleting" only while
// the runtime may still be tearing down, i.e. while its quota reservation
// is held; Recovery releases the reservation on a positive runtime-absence
// proof, which is the terminal signal that removes the row from the API.
// Retained data is unaffected: it lives in retained_data, not here.
const visibleWorkspaceSQL = `(workspaces.state = 'active' OR EXISTS (
	SELECT 1 FROM quota_reservation qr
	WHERE qr.workspace_id = workspaces.id AND qr.state = 'held' AND ` + holdsComputeSQL + `))`

// ListWorkspaces returns workspaces for tenantID scoped to ownerScope (or
// all tenant workspaces when ownerScope is ""), ordered by creation time,
// paginated by an opaque cursor. Deleted workspaces whose runtime is proven
// gone are omitted (visibleWorkspaceSQL).
func (s *Service) ListWorkspaces(ctx context.Context, tenantID, ownerScope, phase, cursor string, limit int) ([]WorkspaceRecord, string, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var rows []WorkspaceRecord
	var next string

	conds := []string{"tenant_id = $1", visibleWorkspaceSQL}
	args := []any{tenantID}
	n := 2
	if ownerScope != "" {
		conds = append(conds, fmt.Sprintf("owner_subject = $%d", n))
		args = append(args, ownerScope)
		n++
	}
	if phase != "" {
		conds = append(conds, fmt.Sprintf("phase = $%d", n))
		args = append(args, phase)
		n++
	}
	if cursor != "" {
		ts, id, err := decodeCursor(cursor)
		if err != nil {
			return nil, "", fmt.Errorf("%w: %v", ErrBadCursor, err)
		}
		conds = append(conds, fmt.Sprintf("(created_at, id) > ($%d, $%d)", n, n+1))
		args = append(args, ts, id)
		n += 2
	}
	q := `SELECT id, tenant_id, owner_subject, owner_issuer, owner_sub, name,
		template, desired_state, data_policy, phase, COALESCE(failure_reason, ''),
		COALESCE(retained_data_ref, ''), request_id, intent_revision, created_at, updated_at,
		COALESCE((SELECT running_slots FROM quota_reservation WHERE workspace_id = workspaces.id AND state = 'held'), 0),
		COALESCE((SELECT cpu_millis FROM quota_reservation WHERE workspace_id = workspaces.id AND state = 'held'), 0),
		COALESCE((SELECT memory_bytes FROM quota_reservation WHERE workspace_id = workspaces.id AND state = 'held'), 0),
		COALESCE((SELECT disk_bytes FROM quota_reservation WHERE workspace_id = workspaces.id AND state = 'held'), 0)
		FROM workspaces WHERE ` + strings.Join(conds, " AND ") +
		fmt.Sprintf(` ORDER BY created_at, id LIMIT %d`, limit+1)
	r, err := s.db.Pool().Query(ctx, q, args...)
	if err != nil {
		return nil, "", err
	}
	defer r.Close()
	for r.Next() {
		rec, err := scanWorkspace(r)
		if err != nil {
			return nil, "", err
		}
		rows = append(rows, *rec)
	}
	if err := r.Err(); err != nil {
		return nil, "", err
	}
	if len(rows) > limit {
		last := rows[limit-1]
		next = encodeCursor(last.CreatedAt, last.ID)
		rows = rows[:limit]
	}
	return rows, next, nil
}

// AttachRetained delegates to the retained store so the workspace create
// route can share the claimed attach path (POST /v1/data/{id}/attach).
func (s *Service) AttachRetained(ctx context.Context, tenantID, caller, ownerScope, dataID, idemKey string, req AttachRequest, bodyHash []byte) (WorkspaceRecord, error) {
	return s.retained.AttachRetained(ctx, tenantID, caller, ownerScope, dataID, idemKey, req, bodyHash)
}

// SignalWorkspace records a lifecycle intent (start/stop/delete) in one
// transaction with the state transition. caller is the authenticated
// principal (iss|sub) and binds the idempotency key; ownerScope "" means
// tenant-wide (admin); a foreign workspace yields ErrWorkspaceNotFound
// before any idempotent replay. Idempotent: repeated delete returns the
// terminating record; start on Running and stop on Stopped are no-ops
// returning the current record. An idempotency key, when given, records
// the result.
func (s *Service) SignalWorkspace(ctx context.Context, tenantID, caller, ownerScope, wsID, idemKey string, kind IntentKind, bodyHash []byte) (res WorkspaceRecord, err error) {
	if kind == IntentCreate {
		return res, errors.New("signal: use CreateWorkspace for create")
	}
	if !kind.Valid() {
		return res, fmt.Errorf("signal: bad kind %q", kind)
	}
	err = s.db.WithTx(ctx, func(tx store.Tx) error {
		// SEC-21: the owner-scoped lookup runs BEFORE idempotent replay so
		// a foreign workspace can never be short-circuited into existence.
		rec, err := getWorkspaceTx(ctx, tx, tenantID, ownerScope, wsID, "FOR UPDATE")
		if err != nil {
			return err
		}
		scopedKey := ""
		if idemKey != "" {
			scopedKey = scopedIdemKey(caller, idemKey)
			idem, replayed, err := BeginIdempotent(ctx, tx, tenantID, scopedKey, string(kind)+":"+wsID, bodyHash)
			if err != nil {
				return err
			}
			if replayed {
				if err := json.Unmarshal(idem.Result, &res); err != nil {
					return fmt.Errorf("signal: stored result %w", err)
				}
				res.Replayed = true
				return nil
			}
		}

		if rec.isDeleted() {
			if kind == IntentDelete {
				res = *rec
			} else {
				err = ErrWorkspaceClosed
			}
			if err == nil && idemKey != "" {
				return CompleteIdempotent(ctx, tx, tenantID, scopedKey, res)
			}
			return err
		}

		var apply bool
		switch kind {
		case IntentStart:
			switch {
			case rec.DesiredState == "Stopped":
				apply = true
				rec.DesiredState = "Running"
				rec.Phase = "Provisioning"
			default:
				// already Running/Provisioning: no-op
			}
		case IntentStop:
			switch {
			case rec.DesiredState == "Running":
				apply = true
				rec.DesiredState = "Stopped"
				rec.Phase = "Stopping"
			default:
				// already stopped/stopping: no-op
			}
		case IntentDelete:
			apply = true
			rec.Phase = "Terminating"
			// A deleted workspace is never wanted Running: the row must not
			// advertise a desired state the platform will not act on.
			rec.DesiredState = "Stopped"
		}
		if apply {
			var genIncr int64
			var sigSpec *IntentSpec
			var skipReason string
			if kind == IntentStart {
				genIncr = 1
				// E1: a start may re-point the workspace at the newest
				// published revision of its template family; the move is
				// decided and written in this same transaction.
				if s.templates != nil {
					next, objName, skipped, err := startTemplateTarget(ctx, s.templates, tenantID, rec, s.log)
					if err != nil {
						return err
					}
					if next != nil {
						rec.Template = *next
						sigSpec = &IntentSpec{
							TemplateName: objName,
							ImageBuiltAt: next.ImageBuiltAt,
						}
					} else {
						skipReason = skipped
					}
				}
				// A stop releases the running-quota reservation once the
				// runtime's absence is proven; the restart re-acquires it in
				// this same transaction so a start intent can never exist
				// without its reservation.
				if err := reserveForStart(ctx, tx, tenantID, wsID); err != nil {
					return err
				}
			}
			tpl, err := json.Marshal(rec.Template)
			if err != nil {
				return fmt.Errorf("signal: workspace template snapshot %w", err)
			}
			if _, err := tx.Exec(ctx, `
				UPDATE workspaces SET desired_state = $3,
					runtime_generation = runtime_generation + $4,
					phase = $5, template = $6, updated_at = now()
				WHERE id = $1 AND tenant_id = $2`,
				wsID, tenantID, rec.DesiredState, genIncr, rec.Phase, tpl); err != nil {
				return err
			}
			rev, err := appendIntent(ctx, tx, PlatformID(wsID), kind, sigSpec)
			if err != nil {
				return err
			}
			if skipReason != "" {
				// E2: the guard refused the family re-point — the cause
				// stays with the start intent so the workspace events can
				// name it (TemplateUpdateSkipped).
				if err := SetIntentReason(ctx, tx, PlatformID(wsID), rev, skipReason); err != nil {
					return err
				}
			}
			if kind == IntentDelete {
				if err := markDeleted(ctx, tx, PlatformID(wsID)); err != nil {
					return err
				}
			}
		}
		out, err := getWorkspaceTx(ctx, tx, tenantID, "", wsID)
		if err != nil {
			return err
		}
		res = *out
		if idemKey != "" {
			return CompleteIdempotent(ctx, tx, tenantID, scopedKey, res)
		}
		return nil
	})
	return res, err
}

// reserveForStart re-acquires the running-quota reservation a stopped
// workspace gave up. Two shapes exist:
//   - released row (Ephemeral): Release keeps the granted vector on the row,
//     so re-holding reserves exactly what the original admission granted;
//   - disk-only hold (stopped Retain, see convertToDiskOnly): the row is still
//     held for its disk and the admitted compute sits in restart_*; it is
//     added back to the same row and the stored vector cleared, so a second
//     stop copies it again.
//
// A workspace with no reservation row at all cannot have passed admission —
// that is an internal inconsistency, not a grant of zero quota.
func reserveForStart(ctx context.Context, tx store.Tx, tenantID, workspaceID string) error {
	var v ResourceVector
	var state string
	var rs, rc, rm *int64
	err := tx.QueryRow(ctx, `
		SELECT running_slots, cpu_millis, memory_bytes, disk_bytes, state,
		       restart_slots, restart_cpu_millis, restart_memory_bytes
		FROM quota_reservation WHERE workspace_id = $1`, workspaceID).
		Scan(&v.RunningSlots, &v.CPUMillis, &v.MemoryBytes, &v.DiskBytes, &state, &rs, &rc, &rm)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("start: workspace %s has no reservation row to re-acquire", workspaceID)
	}
	if err != nil {
		return fmt.Errorf("start: read reservation %w", err)
	}
	if state == "held" && rs != nil && rc != nil && rm != nil {
		return reacquireCompute(ctx, tx, tenantID, workspaceID, ResourceVector{
			RunningSlots: *rs, CPUMillis: *rc, MemoryBytes: *rm})
	}
	return Reserve(ctx, tx, tenantID, workspaceID, v)
}

// ReleaseQuota frees a workspace's reservation only with a runtime
// absence proof; see Release.
func (s *Service) ReleaseQuota(ctx context.Context, tenantID, workspaceUID string, proof AbsenceProof) error {
	return s.db.WithTx(ctx, func(tx store.Tx) error {
		return Release(ctx, tx, tenantID, workspaceUID, proof)
	})
}

// ---------------------------------------------------------------------------

func (r *WorkspaceRecord) isDeleted() bool { return r.Phase == "Terminating" }

type rowScanner interface {
	Scan(dest ...any) error
}

func scanWorkspace(row rowScanner) (*WorkspaceRecord, error) {
	var rec WorkspaceRecord
	var tpl []byte
	var rev int64
	err := row.Scan(&rec.ID, &rec.TenantID, &rec.Owner, &rec.OwnerIssuer, &rec.OwnerSub,
		&rec.Name, &tpl, &rec.DesiredState, &rec.DataPolicy, &rec.Phase,
		&rec.FailureReason, &rec.RetainedDataRef, &rec.RequestID, &rev,
		&rec.CreatedAt, &rec.UpdatedAt,
		&rec.Vector.RunningSlots, &rec.Vector.CPUMillis, &rec.Vector.MemoryBytes, &rec.Vector.DiskBytes)
	if err != nil {
		return nil, err
	}
	rec.Revision = uint64(rev)
	if len(tpl) > 0 {
		if err := json.Unmarshal(tpl, &rec.Template); err != nil {
			return nil, fmt.Errorf("workspace template jsonb: %w", err)
		}
	}
	return &rec, nil
}

type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func getWorkspaceTx(ctx context.Context, q querier, tenantID, ownerScope, id string, lock ...string) (*WorkspaceRecord, error) {
	return getWorkspace(ctx, q, tenantID, ownerScope, id, false, lock...)
}

// getWorkspace reads one row. With visibleOnly it applies the read-side
// visibility rule (visibleWorkspaceSQL); writers pass false so a repeated
// delete or an idempotent replay still finds the tombstone.
func getWorkspace(ctx context.Context, q querier, tenantID, ownerScope, id string, visibleOnly bool, lock ...string) (*WorkspaceRecord, error) {
	query := `SELECT id, tenant_id, owner_subject, owner_issuer, owner_sub, name,
		template, desired_state, data_policy, phase, COALESCE(failure_reason, ''),
		COALESCE(retained_data_ref, ''), request_id, intent_revision, created_at, updated_at,
		COALESCE((SELECT running_slots FROM quota_reservation WHERE workspace_id = workspaces.id AND state = 'held'), 0),
		COALESCE((SELECT cpu_millis FROM quota_reservation WHERE workspace_id = workspaces.id AND state = 'held'), 0),
		COALESCE((SELECT memory_bytes FROM quota_reservation WHERE workspace_id = workspaces.id AND state = 'held'), 0),
		COALESCE((SELECT disk_bytes FROM quota_reservation WHERE workspace_id = workspaces.id AND state = 'held'), 0)
		FROM workspaces WHERE id = $1 AND tenant_id = $2`
	args := []any{id, tenantID}
	if ownerScope != "" {
		query += ` AND owner_subject = $3`
		args = append(args, ownerScope)
	}
	if visibleOnly {
		query += ` AND ` + visibleWorkspaceSQL
	}
	if len(lock) > 0 {
		query += " " + lock[0]
	}
	rec, err := scanWorkspace(q.QueryRow(ctx, query, args...))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrWorkspaceNotFound
	}
	if err != nil {
		return nil, err
	}
	return rec, nil
}

func encodeCursor(t time.Time, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(t.UnixNano(), 36) + "|" + id))
}

func decodeCursor(s string) (time.Time, string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return time.Time{}, "", err
	}
	ts, id, ok := strings.Cut(string(raw), "|")
	if !ok {
		return time.Time{}, "", errors.New("malformed cursor")
	}
	n, err := strconv.ParseInt(ts, 36, 64)
	if err != nil {
		return time.Time{}, "", err
	}
	return time.Unix(0, n), id, nil
}

// isUniqueViolation reports a Postgres 23505 without pulling in pgconn.
func isUniqueViolation(err error) bool {
	var i interface{ SQLState() string }
	return errors.As(err, &i) && i.SQLState() == "23505"
}

// newWorkspaceID returns a ws_ prefixed random ID matching the public
// contract pattern ^ws_[A-Za-z0-9]{8,64}$.
func newWorkspaceID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return "ws_" + hex.EncodeToString(b[:])
}
