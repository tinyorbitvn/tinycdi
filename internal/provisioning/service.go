package provisioning

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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
	// blockAfter is the E3 stale-image admission limit
	// (-image-block-after): a start whose resolved revision's image is
	// older fails with *ImageStaleError. <=0 disables the block.
	blockAfter time.Duration
	// fileBound is the platform per-file bound
	// (-file-transfer-max-bytes) every resolved session policy is clipped
	// to (ADR 0010 T11).
	fileBound int64
	// revokeGeneration fences the previous runtime generation inside the
	// RestartWorkspace transaction (F1); nil refuses a restart.
	revokeGeneration GenerationRevoker
	log              *slog.Logger
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

// WithImageBlockAfter sets the E3 stale-image admission limit
// (-image-block-after): a start resolving to a runtime image older than d
// fails with *ImageStaleError (the API maps it to 409 IMAGE_STALE).
// d <= 0 disables the block.
func (s *Service) WithImageBlockAfter(d time.Duration) *Service {
	s.blockAfter = d
	return s
}

// WithFileTransferMaxBytes sets the platform per-file bound
// (-file-transfer-max-bytes) every resolved policy is clipped to.
func (s *Service) WithFileTransferMaxBytes(bound int64) *Service {
	s.fileBound = bound
	s.retained.fileBound = bound
	return s
}

// FileTransferMaxBytes returns the platform per-file bound.
func (s *Service) FileTransferMaxBytes() int64 { return s.fileBound }

// WithLogger attaches a logger for lifecycle warnings; nil keeps
// slog.Default.
func (s *Service) WithLogger(l *slog.Logger) *Service {
	s.log = l
	return s
}

// ImageStaleError is the E3 admission refusal: create, and a start that
// cannot move to a fresher revision, fail when the resolved template's
// image is older than -image-block-after. The API maps it to 409
// IMAGE_STALE.
type ImageStaleError struct {
	// TemplateName names the resolved template/catalog entry whose image
	// is stale — the message always names it.
	TemplateName string
	// AgeDays is the resolved image's age in whole days; LimitDays the
	// configured limit in whole days.
	AgeDays, LimitDays int
	// Pinned marks a workspace that cannot move to a fresher revision —
	// imageUpdate Pinned, or a move the compatibility guard refused — so
	// it can start again only after an administrator publishes a fresh
	// image. False on creates (the caller picked the template) and on
	// starts resolved to the family's newest.
	Pinned bool
}

func (e *ImageStaleError) Error() string {
	msg := fmt.Sprintf("the resolved runtime image is %d days old (limit %d days)",
		e.AgeDays, e.LimitDays)
	if e.TemplateName != "" {
		msg = fmt.Sprintf("template %q runtime image is %d days old (limit %d days)",
			e.TemplateName, e.AgeDays, e.LimitDays)
	}
	if e.Pinned {
		msg += " — the workspace is pinned to this revision and can start again only after an administrator publishes a fresh image"
	}
	return msg
}

// IsImageStale reports whether err carries an E3 stale-image refusal.
func IsImageStale(err error) bool {
	var e *ImageStaleError
	return errors.As(err, &e)
}

// CheckImageBlock parses the raw image-built-at value (RFC 3339) and
// returns *ImageStaleError when its age exceeds limit. It returns nil when
// the block is disabled (limit <= 0) or the value is missing or malformed
// — a missing imageBuiltAt never blocks (E3). templateName/pinned fill the
// refusal's context: the template it names and whether the caller cannot
// move to a fresher revision.
func CheckImageBlock(raw string, limit time.Duration, now time.Time, templateName string, pinned bool) error {
	if limit <= 0 || raw == "" {
		return nil
	}
	built, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return nil
	}
	age := now.Sub(built)
	if age <= limit {
		return nil
	}
	return &ImageStaleError{
		TemplateName: templateName,
		AgeDays:      int(age / (24 * time.Hour)),
		LimitDays:    int(limit / (24 * time.Hour)),
		Pinned:       pinned,
	}
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
	// ClipboardPolicy is the template revision's spec.clipboardPolicy,
	// snapshotted at create and refreshed by a re-pointing start — the
	// template leg the session-policy meet is computed against (ADR 0010).
	// Empty on rows written before the field existed.
	ClipboardPolicy string `json:"clipboardPolicy,omitempty"`
	// FileTransferPolicy is the template revision's spec.fileTransferPolicy,
	// snapshotted like ClipboardPolicy; empty on rows written before the
	// field existed.
	FileTransferPolicy string `json:"fileTransferPolicy,omitempty"`
}

// templateRefName is the spec.templateRef value a create intent stamps:
// the resolved WorkspaceTemplate OBJECT name, not the catalog (family)
// name. The API resolves the family reference to a concrete revision at
// admission — req.Template.ID is "tpl_<object name>" — so the operator's
// exact lookup never re-derives the family's newest member (the legacy
// catalog-name fallback is gone). The row keeps the family name in
// Template.Name for the start path's family resolution.
func templateRefName(t TemplateInfo) string {
	if name, ok := TemplateCRName(t.ID); ok {
		return name
	}
	return t.Name
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
	// RuntimeGeneration is the row's runtime_generation — the fence a
	// restart is issued against; internal only, never serialized.
	RuntimeGeneration int64 `json:"-"`
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
	// E3: a create resolving to an image older than -image-block-after is
	// refused; a missing or malformed image-built-at never blocks.
	if err := CheckImageBlock(req.Template.ImageBuiltAt, s.blockAfter, time.Now(), req.Template.Name, false); err != nil {
		return res, err
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
		// The resolved session policy rides the create intent: the applier
		// stamps it as spec.sessionPolicy so pod building reads the meet —
		// never re-derived (ADR 0010 §2). Resolution happens inside this
		// transaction so the stamped value is the snapshot the workspace
		// was admitted under.
		sp, err := resolveSessionPolicyTx(ctx, s.db, tx, tenantID, TemplateCatalogEntry{
			ClipboardPolicy:    req.Template.ClipboardPolicy,
			FileTransferPolicy: req.Template.FileTransferPolicy,
		}, s.fileBound)
		if err != nil {
			return fmt.Errorf("create: resolve session policy: %w", err)
		}
		rev, err := appendIntent(ctx, tx, PlatformID(res.ID), IntentCreate, &IntentSpec{
			WorkspaceName:   req.Name,
			TemplateName:    templateRefName(req.Template),
			OwnerIssuer:     req.OwnerIssuer,
			OwnerSubject:    req.OwnerSubject,
			DataPolicy:      dataPolicy,
			RetainedDataRef: req.RetainedDataRef,
			ImageBuiltAt:    req.Template.ImageBuiltAt,
			SessionPolicy:   &sp,
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

// ListWorkspaces returns one page of workspaces for tenantID scoped to
// ownerScope (or all tenant workspaces when ownerScope is ""), ordered by
// (p.SortField, id) — createdAt or name — and positioned by the keyset
// p.AfterKey/p.AfterID. nextKey/nextID locate the last returned row and are
// set only when another page exists. Deleted workspaces whose runtime is
// proven gone are omitted (visibleWorkspaceSQL).
func (s *Service) ListWorkspaces(ctx context.Context, tenantID, ownerScope, phase string, p ListPage) ([]WorkspaceRecord, string, string, error) {
	col, ok := workspaceSortColumns[p.SortField]
	if !ok {
		return nil, "", "", fmt.Errorf("list workspaces: unsupported sort field %q", p.SortField)
	}
	conds := []string{"tenant_id = $1", visibleWorkspaceSQL}
	args := []any{tenantID}
	if ownerScope != "" {
		args = append(args, ownerScope)
		conds = append(conds, fmt.Sprintf("owner_subject = $%d", len(args)))
	}
	if phase != "" {
		args = append(args, phase)
		conds = append(conds, fmt.Sprintf("phase = $%d", len(args)))
	}
	ks, err := p.keyset(col, len(args)+1)
	if err != nil {
		return nil, "", "", err
	}
	if ks.where != "" {
		conds = append(conds, ks.where)
		args = append(args, ks.args...)
	}
	q := `SELECT ` + workspaceSelectCols + ` FROM workspaces WHERE ` + strings.Join(conds, " AND ") + ks.orderLimit
	r, err := s.db.Pool().Query(ctx, q, args...)
	if err != nil {
		return nil, "", "", err
	}
	defer r.Close()
	var rows []WorkspaceRecord
	for r.Next() {
		rec, err := scanWorkspace(r)
		if err != nil {
			return nil, "", "", err
		}
		rows = append(rows, *rec)
	}
	if err := r.Err(); err != nil {
		return nil, "", "", err
	}
	var nextKey, nextID string
	if len(rows) > ks.limit {
		rows = rows[:ks.limit]
		last := &rows[ks.limit-1]
		nextKey, nextID = workspaceSortKey(last, p.SortField), last.ID
	}
	return rows, nextKey, nextID, nil
}

// workspaceSortColumns maps the public sort fields onto columns; client
// input never reaches the SQL text.
var workspaceSortColumns = map[string]sortColumn{
	"createdAt": {name: "created_at", timestamp: true},
	"name":      {name: "name"},
}

func workspaceSortKey(rec *WorkspaceRecord, field string) string {
	if field == "name" {
		return rec.Name
	}
	return formatSortTime(rec.CreatedAt)
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
				resolvedBuiltAt := rec.Template.ImageBuiltAt
				// E1: a start may re-point the workspace at the newest
				// published revision of its template family; the move is
				// decided and written in this same transaction.
				var pinned bool
				if s.templates != nil {
					next, objName, skipped, builtAt, immovable, err := startTemplateTarget(ctx, s.templates, tenantID, rec, s.log)
					if err != nil {
						return err
					}
					resolvedBuiltAt = builtAt
					pinned = immovable
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
				// E3: a start whose resolved revision's image is older than
				// -image-block-after is refused here, inside the
				// transaction — no state change, reservation or intent is
				// written. The API maps the refusal to 409 IMAGE_STALE. A
				// pinned (or guard-refused) start can never move, so the
				// refusal says an administrator must publish a fresh image.
				if err := CheckImageBlock(resolvedBuiltAt, s.blockAfter, time.Now(), rec.Template.Name, pinned); err != nil {
					return err
				}
				// A stop releases the running-quota reservation once the
				// runtime's absence is proven; the restart re-acquires it in
				// this same transaction so a start intent can never exist
				// without its reservation.
				if err := reserveForStart(ctx, tx, tenantID, wsID); err != nil {
					return err
				}
				// The resolved session policy rides every start intent: the
				// applier re-stamps spec.sessionPolicy so a tightened ceiling
				// lands on the pod's next start (ADR 0010 §3 — server-side
				// enforcement is per-boot).
				sp, err := s.startSessionPolicy(ctx, tx, tenantID, rec)
				if err != nil {
					return err
				}
				if sp != nil {
					if sigSpec == nil {
						sigSpec = &IntentSpec{}
					}
					sigSpec.SessionPolicy = sp
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

// GenerationRevoker fences a runtime generation inside the caller's
// transaction (broker.RevokeGenerationTx in production).
type GenerationRevoker func(ctx context.Context, tx store.Tx, workspaceID string, generation int64) error

// ErrNotRunning refuses a restart of a workspace whose desired state is
// not Running (a stopped workspace uses a plain start).
var ErrNotRunning = errors.New("workspace is not running")

// WithGenerationRevoker installs the revoker RestartWorkspace calls.
func (s *Service) WithGenerationRevoker(r GenerationRevoker) *Service {
	s.revokeGeneration = r
	return s
}

// RestartWorkspace mints a new runtime generation for a workspace that is
// wanted Running but whose current incarnation is lost or failed (F1), or
// whose session policy must be re-applied (F3). In one transaction it:
// bumps runtime_generation, revokes the previous generation's leases and
// tickets, re-stamps the resolved session policy, and appends a start
// intent; the applier moves spec.runtimeGeneration and the operator
// replaces the pod on the same home volume.
//
// fromGeneration is the generation the caller observed. When the row has
// already moved past it (a concurrent restart won), the call is a no-op
// returning the current record — a double-click bumps once. The running
// quota reservation is kept, because the workspace never stopped holding it.
// The template is not re-pointed, because a restart is not an upgrade.
func (s *Service) RestartWorkspace(ctx context.Context, tenantID, caller, ownerScope, wsID, idemKey string, fromGeneration int64, bodyHash []byte) (res WorkspaceRecord, err error) {
	if s.revokeGeneration == nil {
		return res, errors.New("restart: no generation revoker configured")
	}
	err = s.db.WithTx(ctx, func(tx store.Tx) error {
		rec, err := getWorkspaceTx(ctx, tx, tenantID, ownerScope, wsID, "FOR UPDATE")
		if err != nil {
			return err
		}
		scopedKey := ""
		if idemKey != "" {
			scopedKey = scopedIdemKey(caller, idemKey)
			idem, replayed, err := BeginIdempotent(ctx, tx, tenantID, scopedKey, string(IntentStart)+":"+wsID, bodyHash)
			if err != nil {
				return err
			}
			if replayed {
				if err := json.Unmarshal(idem.Result, &res); err != nil {
					return fmt.Errorf("restart: stored result %w", err)
				}
				res.Replayed = true
				return nil
			}
		}
		if rec.isDeleted() {
			return ErrWorkspaceClosed
		}
		if rec.DesiredState != "Running" {
			return ErrNotRunning
		}
		if rec.RuntimeGeneration == fromGeneration {
			if err := CheckImageBlock(rec.Template.ImageBuiltAt, s.blockAfter, time.Now(), rec.Template.Name, true); err != nil {
				return err
			}
			sp, err := s.startSessionPolicy(ctx, tx, tenantID, rec)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `
				UPDATE workspaces SET runtime_generation = runtime_generation + 1,
					phase = 'Provisioning', updated_at = now()
				WHERE id = $1 AND tenant_id = $2`, wsID, tenantID); err != nil {
				return fmt.Errorf("restart: bump generation %w", err)
			}
			if err := s.revokeGeneration(ctx, tx, wsID, rec.RuntimeGeneration); err != nil {
				return err
			}
			if _, err := appendIntent(ctx, tx, PlatformID(wsID), IntentStart, &IntentSpec{SessionPolicy: sp}); err != nil {
				return err
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

// ListRunningWorkspaces returns up to limit active workspaces of tenantID
// whose desired state is Running, oldest first, in ONE query with the same
// projection as GetWorkspace (F3 policy apply). Tenant scoping is in the
// WHERE clause; a workspace being deleted is excluded.
func (s *Service) ListRunningWorkspaces(ctx context.Context, tenantID string, limit int) ([]WorkspaceRecord, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := s.db.Pool().Query(ctx, `
		SELECT `+workspaceSelectCols+` FROM workspaces
		WHERE tenant_id = $1 AND state = 'active' AND desired_state = 'Running' AND phase <> 'Terminating'
		ORDER BY created_at, id LIMIT $2`, tenantID, limit)
	if err != nil {
		return nil, fmt.Errorf("list running: %w", err)
	}
	defer rows.Close()
	var out []WorkspaceRecord
	for rows.Next() {
		rec, err := scanWorkspace(rows)
		if err != nil {
			return nil, fmt.Errorf("list running: %w", err)
		}
		out = append(out, *rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list running: %w", err)
	}
	return out, nil
}

// startSessionPolicy resolves the session data-transfer policy a start
// intent carries: the meet of the tenant ceiling and the clipboard policy
// of the revision the start runs on — rec.Template is already the
// post-re-point snapshot when the family moved. A workspace row predating
// the snapshot field (ClipboardPolicy "") consults the live catalog for
// the recorded revision; when that lookup also yields nothing the start
// carries no stamped policy (nil) — the pod falls back to the operator's
// template snapshot, the pre-field behaviour.
func (s *Service) startSessionPolicy(ctx context.Context, tx store.Tx, tenantID string, rec *WorkspaceRecord) (*SessionPolicy, error) {
	clipboard := rec.Template.ClipboardPolicy
	fileTransfer := rec.Template.FileTransferPolicy
	if clipboard == "" && s.templates != nil {
		if cur, err := s.templates.Get(ctx, tenantID, rec.Template.ID); err == nil && cur != nil {
			clipboard = cur.ClipboardPolicy
			fileTransfer = cur.FileTransferPolicy
		}
	}
	if clipboard == "" {
		return nil, nil
	}
	sp, err := resolveSessionPolicyTx(ctx, s.db, tx, tenantID, TemplateCatalogEntry{
		ClipboardPolicy:    clipboard,
		FileTransferPolicy: fileTransfer,
	}, s.fileBound)
	if err != nil {
		return nil, fmt.Errorf("start: resolve session policy: %w", err)
	}
	return &sp, nil
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
		&rec.Vector.RunningSlots, &rec.Vector.CPUMillis, &rec.Vector.MemoryBytes, &rec.Vector.DiskBytes,
		&rec.RuntimeGeneration)
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

// workspaceSelectCols is the projection scanWorkspace reads (keep the
// order in step with it).
const workspaceSelectCols = `id, tenant_id, owner_subject, owner_issuer, owner_sub, name,
	template, desired_state, data_policy, phase, COALESCE(failure_reason, ''),
	COALESCE(retained_data_ref, ''), request_id, intent_revision, created_at, updated_at,
	COALESCE((SELECT running_slots FROM quota_reservation WHERE workspace_id = workspaces.id AND state = 'held'), 0),
	COALESCE((SELECT cpu_millis FROM quota_reservation WHERE workspace_id = workspaces.id AND state = 'held'), 0),
	COALESCE((SELECT memory_bytes FROM quota_reservation WHERE workspace_id = workspaces.id AND state = 'held'), 0),
	COALESCE((SELECT disk_bytes FROM quota_reservation WHERE workspace_id = workspaces.id AND state = 'held'), 0),
	runtime_generation`

// getWorkspace reads one row. With visibleOnly it applies the read-side
// visibility rule (visibleWorkspaceSQL); writers pass false so a repeated
// delete or an idempotent replay still finds the tombstone.
func getWorkspace(ctx context.Context, q querier, tenantID, ownerScope, id string, visibleOnly bool, lock ...string) (*WorkspaceRecord, error) {
	query := `SELECT ` + workspaceSelectCols + ` FROM workspaces WHERE id = $1 AND tenant_id = $2`
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

// ListPage is one keyset page request: Limit rows ordered by (SortField,
// id), descending when Desc, strictly after (AfterKey, AfterID) when
// AfterID is set. AfterKey is the sort column's value as the list's
// sort-key function rendered it (RFC 3339 with nanoseconds for timestamps).
type ListPage struct {
	Limit     int
	SortField string
	Desc      bool
	AfterKey  string
	AfterID   string
}

// Page size bounds: the default for an unset Limit, and the ceiling
// whatever the caller asked for.
const (
	defaultListLimit = 50
	maxListLimit     = 500
)

// sortColumn is one allowlisted ORDER BY column.
type sortColumn struct {
	name      string
	timestamp bool
}

// keysetQuery is the SQL a ListPage contributes: the position predicate
// (with its args, numbered from the caller's next placeholder) and the
// ORDER BY ... LIMIT tail fetching one extra row to detect a next page.
type keysetQuery struct {
	where      string
	args       []any
	orderLimit string
	limit      int
}

func (p ListPage) keyset(col sortColumn, next int) (keysetQuery, error) {
	k := keysetQuery{limit: min(p.Limit, maxListLimit)}
	if k.limit <= 0 {
		k.limit = defaultListLimit
	}
	dir, cmp := "", ">"
	if p.Desc {
		dir, cmp = " DESC", "<"
	}
	if p.AfterID != "" {
		var key any = p.AfterKey
		if col.timestamp {
			t, err := time.Parse(time.RFC3339Nano, p.AfterKey)
			if err != nil {
				return k, fmt.Errorf("%w: %v", ErrBadCursor, err)
			}
			key = t
		}
		k.where = fmt.Sprintf("(%s, id) %s ($%d, $%d)", col.name, cmp, next, next+1)
		k.args = []any{key, p.AfterID}
	}
	k.orderLimit = fmt.Sprintf(" ORDER BY %s%s, id%s LIMIT %d", col.name, dir, dir, k.limit+1)
	return k, nil
}

// formatSortTime renders a timestamp sort key losslessly.
func formatSortTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

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
