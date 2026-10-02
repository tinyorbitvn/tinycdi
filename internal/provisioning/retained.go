package provisioning

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tinyorbitvn/tinycdi/internal/store"
)

// Retained-data state machine (design §5): a dataset moves
// Retained -> Attaching -> Attached when a new workspace consumes it, or
// Retained -> Purging -> Purged when its owner destroys it. Every
// transition is a conditional update committed in the SAME transaction as
// the quota reservation and the outbox intent, so attach and purge
// exclude each other atomically and a disk has at most one consumer.
type RetainedDataState string

const (
	RetainedStateRetained  RetainedDataState = "Retained"
	RetainedStateAttaching RetainedDataState = "Attaching"
	RetainedStateAttached  RetainedDataState = "Attached"
	RetainedStatePurging   RetainedDataState = "Purging"
	RetainedStatePurged    RetainedDataState = "Purged"
)

var (
	// ErrRetainedNotFound covers unknown ids AND records owned by someone
	// else: a foreign id is indistinguishable from a missing one (the same
	// non-leaking convention as workspaces). Tenant admins (ownerScope "")
	// see the whole tenant.
	ErrRetainedNotFound = errors.New("retained data record not found")
	// ErrRetainedState is returned when the record's state does not allow
	// the requested transition (attach or purge on a non-Retained record,
	// illegal transition attempts).
	ErrRetainedState = errors.New("retained data state does not allow this operation")
	// ErrPurgeNonce is returned when the confirmation nonce is missing,
	// malformed, expired, issued to a different principal, or bound to a
	// stale record epoch (consumed by an earlier transition).
	ErrPurgeNonce = errors.New("purge confirmation nonce is invalid or consumed")
	// ErrRuntimeMismatch is returned when the requested template's runtime
	// differs from the retained disk's runtime.
	ErrRuntimeMismatch = errors.New("template runtime does not match the retained disk")
)

// RetainedRecord is the read model of one retained_data row. Dataset
// identity is (PVCNamespace, PVCUID) + SourceWorkspaceID — never a bare
// PVC name, which is reusable and spoofable.
type RetainedRecord struct {
	ID       string            `json:"id"` // rd_ identifier
	TenantID string            `json:"tenantId"`
	Owner    string            `json:"owner"` // iss|sub of the retaining owner
	State    RetainedDataState `json:"state"`
	// TransitionSeq increments on every state change; it is the epoch the
	// purge confirmation nonce is bound to (single-use by construction).
	TransitionSeq        int64     `json:"-"`
	PVCNamespace         string    `json:"-"`
	PVCName              string    `json:"-"`
	PVCUID               string    `json:"-"`
	SourceWorkspaceID    string    `json:"-"`
	SourceWorkspaceName  string    `json:"sourceWorkspaceName"`
	ConsumingWorkspaceID string    `json:"consumingWorkspaceId,omitempty"`
	Runtime              string    `json:"runtime"` // LinuxContainer | WindowsVM
	SizeBytes            int64     `json:"-"`
	RetainedAt           time.Time `json:"retainedAt"`
	UpdatedAt            time.Time `json:"updatedAt"`
	// PurgeNonce is minted fresh on every read for the calling principal
	// and echoed back as purgeConfirmationNonce. It is never stored raw.
	PurgeNonce string `json:"-"`
}

// RetainedDiskInfo is the operator-supplied description of a dataset that
// entered the retained inventory (from PVC metadata — the source of truth
// for identity). ImportRetained consumes it.
type RetainedDiskInfo struct {
	PVCNamespace        string
	PVCName             string
	PVCUID              string
	TenantID            string
	Owner               string // iss|sub recorded on the PVC at retain time
	SourceWorkspaceID   string
	SourceWorkspaceName string
	Runtime             string
	SizeBytes           int64
}

// AttachRequest is the resolved attach intent the store consumes; the API
// handler resolves templateRef into Template and Vector first. The new
// workspace's dataPolicy is Retain by definition.
type AttachRequest struct {
	Name         string
	Template     TemplateInfo
	Vector       ResourceVector
	DesiredState string
}

// purgeNonceTTL bounds how long an issued purge confirmation nonce stays
// usable. The nonce is also bound to the record's transition epoch, so any
// state change (including the purge itself) invalidates it sooner.
const purgeNonceTTL = 15 * time.Minute

// RetainedStore is the Postgres-backed retained-data store (migration
// 007). Every state transition is a conditional UPDATE committed in the
// same transaction as the quota reservation and the outbox intent:
//
//   - AttachRetained claims the record (Retained -> Attaching), creates the
//     new workspace row, re-keys the held disk reservation to the
//     consuming workspace (no double counting) and appends the create
//     intent — all atomically;
//   - PurgeRetained transitions Retained -> Purging in one transaction;
//     the Purging state itself is the durable destroy intent — the
//     cluster-side volume deletion happens asynchronously and only its
//     completion proof (CompletePurge) advances the record to Purged and
//     releases the held disk bytes;
//   - disk quota stays held until the volume is actually gone: the source
//     workspace's quota_reservation keeps the disk bytes while the record
//     is Retained/Attaching/Purging (see restoreDiskQuota on import).
type RetainedStore struct {
	db *store.DB

	keyMu  sync.Mutex
	macKey []byte // cached purge-nonce MAC key (migration 008)
}

// NewRetainedStore returns the Postgres-backed retained-data store.
func NewRetainedStore(db *store.DB) *RetainedStore { return &RetainedStore{db: db} }

const retainedCols = `id, tenant_id, owner_subject, state, transition_seq,
	pvc_namespace, pvc_name, pvc_uid, source_workspace_id, source_workspace_name,
	COALESCE(consuming_workspace_id, ''), runtime, size_bytes, retained_at, updated_at`

func scanRetained(row rowScanner) (*RetainedRecord, error) {
	rec := &RetainedRecord{}
	err := row.Scan(&rec.ID, &rec.TenantID, &rec.Owner, &rec.State, &rec.TransitionSeq,
		&rec.PVCNamespace, &rec.PVCName, &rec.PVCUID, &rec.SourceWorkspaceID,
		&rec.SourceWorkspaceName, &rec.ConsumingWorkspaceID, &rec.Runtime,
		&rec.SizeBytes, &rec.RetainedAt, &rec.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return rec, nil
}

// getRetainedTx reads one record. tenantID "" and ownerScope "" widen the
// lookup (internal transitions); a non-empty ownerScope restricts to the
// owner's records so a foreign id is indistinguishable from a missing one.
func getRetainedTx(ctx context.Context, q querier, tenantID, ownerScope, id string, lock ...string) (*RetainedRecord, error) {
	query := `SELECT ` + retainedCols + ` FROM retained_data WHERE id = $1`
	args := []any{id}
	n := 2
	if tenantID != "" {
		query += fmt.Sprintf(` AND tenant_id = $%d`, n)
		args = append(args, tenantID)
		n++
	}
	if ownerScope != "" {
		query += fmt.Sprintf(` AND owner_subject = $%d`, n)
		args = append(args, ownerScope)
		n++
	}
	if len(lock) > 0 {
		query += " " + lock[0]
	}
	rec, err := scanRetained(q.QueryRow(ctx, query, args...))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrRetainedNotFound
	}
	if err != nil {
		return nil, err
	}
	return rec, nil
}

// newRetainedID returns an rd_ prefixed random ID matching the public
// contract pattern ^rd_[A-Za-z0-9]{8,64}$.
func newRetainedID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return "rd_" + hex.EncodeToString(b[:])
}

// ---------------------------------------------------------------------------
// Purge confirmation nonces (stateless MAC)
// ---------------------------------------------------------------------------

// nonceKey loads the shared MAC key, creating it on first use. The key
// lives in migration 008's single-row table so every API replica verifies
// nonces minted by any other. Rotating the key invalidates outstanding
// nonces — a documented, acceptable operation.
func (s *RetainedStore) nonceKey(ctx context.Context) ([]byte, error) {
	s.keyMu.Lock()
	defer s.keyMu.Unlock()
	if s.macKey != nil {
		return s.macKey, nil
	}
	var key []byte
	err := s.db.Pool().QueryRow(ctx,
		`SELECT key FROM retained_nonce_key WHERE id = true`).Scan(&key)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		key = make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, fmt.Errorf("retained: generate nonce key %w", err)
		}
		if _, err := s.db.Pool().Exec(ctx,
			`INSERT INTO retained_nonce_key (id, key) VALUES (true, $1)
			 ON CONFLICT (id) DO NOTHING`, key); err != nil {
			return nil, fmt.Errorf("retained: store nonce key %w", err)
		}
		// Re-read: a racing replica may have won the insert; both sides
		// must share the row's key, never their local candidate.
		if err := s.db.Pool().QueryRow(ctx,
			`SELECT key FROM retained_nonce_key WHERE id = true`).Scan(&key); err != nil {
			return nil, fmt.Errorf("retained: read nonce key %w", err)
		}
	case err != nil:
		return nil, fmt.Errorf("retained: read nonce key %w", err)
	}
	s.macKey = key
	return key, nil
}

func callerDigest(caller string) string {
	sum := sha256.Sum256([]byte(caller))
	return hex.EncodeToString(sum[:8])
}

// mintPurgeNonce issues a fresh nonce bound to the record id, the caller's
// digest, the record's transition epoch and the issue time.
// Format: base64url(id|seq|unix|callerDigest) + "." + base64url(mac).
func mintPurgeNonce(key []byte, caller string, r *RetainedRecord, now time.Time) string {
	payload := fmt.Sprintf("%s|%d|%d|%s", r.ID, r.TransitionSeq, now.Unix(), callerDigest(caller))
	enc := base64.RawURLEncoding.EncodeToString([]byte(payload))
	m := hmac.New(sha256.New, key)
	m.Write([]byte(enc))
	return enc + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func verifyPurgeNonce(key []byte, caller string, r *RetainedRecord, nonce string, now time.Time) bool {
	enc, sig, ok := strings.Cut(nonce, ".")
	if !ok || enc == "" || sig == "" {
		return false
	}
	gotMAC, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil {
		return false
	}
	m := hmac.New(sha256.New, key)
	m.Write([]byte(enc))
	if !hmac.Equal(gotMAC, m.Sum(nil)) {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(enc)
	if err != nil {
		return false
	}
	parts := strings.Split(string(raw), "|")
	if len(parts) != 4 {
		return false
	}
	if parts[0] != r.ID || parts[3] != callerDigest(caller) {
		return false
	}
	seq, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || seq != r.TransitionSeq {
		return false // bound to a stale epoch: consumed or pre-transition
	}
	issued, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return false
	}
	t := time.Unix(issued, 0)
	if t.After(now.Add(2*time.Minute)) || now.Sub(t) > purgeNonceTTL {
		return false
	}
	return true
}

// ---------------------------------------------------------------------------
// Quota plumbing — the retained disk's held bytes move with the record.
// ---------------------------------------------------------------------------

// releaseDiskQuota subtracts size from a workspace's held disk
// reservation. It is a no-op when the reservation is absent or already
// released — a released row never re-enters 'held' here.
func releaseDiskQuota(ctx context.Context, tx store.Tx, workspaceID string, size int64) error {
	_, err := tx.Exec(ctx, `
		UPDATE quota_reservation
		SET disk_bytes = GREATEST(0, disk_bytes - $2)
		WHERE workspace_id = $1 AND state = 'held'`,
		workspaceID, size)
	return err
}

// restoreDiskQuota re-holds the retained disk's bytes under the source
// workspace's reservation when that reservation was fully released (e.g.
// the workspace was deleted and its compute released, but the disk still
// exists and must keep counting). A live 'held' reservation already counts
// the disk — touching it would double-count — so it is left alone. A
// resurrected row holds disk only: compute was released on the absence
// proof and must not come back.
func restoreDiskQuota(ctx context.Context, tx store.Tx, tenantID, workspaceID string, size int64) error {
	_, err := tx.Exec(ctx, `
		UPDATE quota_reservation
		SET state = 'held', running_slots = 0, cpu_millis = 0, memory_bytes = 0,
		    disk_bytes = $3, release_proof = NULL, released_at = NULL
		WHERE workspace_id = $1 AND tenant_id = $2 AND state = 'released'`,
		workspaceID, tenantID, size)
	return err
}

// returnDiskQuota puts the disk's bytes back on the source workspace's
// reservation when an attach is undone. If the source reservation is
// still held the bytes are added back (AttachRetained had subtracted
// them); if it was released meanwhile it is resurrected disk-only.
func returnDiskQuota(ctx context.Context, tx store.Tx, tenantID, workspaceID string, size int64) error {
	_, err := tx.Exec(ctx, `
		UPDATE quota_reservation
		SET state = 'held', release_proof = NULL, released_at = NULL,
		    disk_bytes = CASE WHEN state = 'released' THEN $3
		                      ELSE disk_bytes + $3 END,
		    running_slots = CASE WHEN state = 'released' THEN 0 ELSE running_slots END,
		    cpu_millis    = CASE WHEN state = 'released' THEN 0 ELSE cpu_millis END,
		    memory_bytes  = CASE WHEN state = 'released' THEN 0 ELSE memory_bytes END
		WHERE workspace_id = $1 AND tenant_id = $2`,
		workspaceID, tenantID, size)
	return err
}

// ---------------------------------------------------------------------------
// Store operations
// ---------------------------------------------------------------------------

// ListRetained returns the records in tenantID visible to ownerScope
// ("" = tenant-wide, admin only), paginated by cursor. Each record carries
// a fresh PurgeNonce bound to caller (iss|sub), the record id and the
// current transition epoch.
func (s *RetainedStore) ListRetained(ctx context.Context, tenantID, caller, ownerScope, cursor string, limit int) ([]RetainedRecord, string, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	conds := []string{"tenant_id = $1", "state <> 'Purged'"}
	args := []any{tenantID}
	n := 2
	if ownerScope != "" {
		conds = append(conds, fmt.Sprintf("owner_subject = $%d", n))
		args = append(args, ownerScope)
		n++
	}
	if cursor != "" {
		ts, id, err := decodeCursor(cursor)
		if err != nil {
			return nil, "", fmt.Errorf("%w: %v", ErrBadCursor, err)
		}
		conds = append(conds, fmt.Sprintf("(retained_at, id) > ($%d, $%d)", n, n+1))
		args = append(args, ts, id)
		n += 2
	}
	q := `SELECT ` + retainedCols + ` FROM retained_data WHERE ` +
		strings.Join(conds, " AND ") +
		fmt.Sprintf(` ORDER BY retained_at, id LIMIT %d`, limit+1)
	rows, err := s.db.Pool().Query(ctx, q, args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	var recs []RetainedRecord
	for rows.Next() {
		rec, err := scanRetained(rows)
		if err != nil {
			return nil, "", err
		}
		recs = append(recs, *rec)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	var next string
	if len(recs) > limit {
		next = encodeCursor(recs[limit-1].RetainedAt, recs[limit-1].ID)
		recs = recs[:limit]
	}
	key, err := s.nonceKey(ctx)
	if err != nil {
		return nil, "", err
	}
	now := time.Now().UTC()
	for i := range recs {
		recs[i].PurgeNonce = mintPurgeNonce(key, caller, &recs[i], now)
	}
	return recs, next, nil
}

// ReadRetained returns one record visible to ownerScope ("" = tenant-wide,
// admin only) carrying a fresh PurgeNonce bound to caller — the single-record
// form of ListRetained behind GET /v1/data/{dataId}. Foreign tenants and, for
// non-admins, other owners' records are ErrRetainedNotFound.
func (s *RetainedStore) ReadRetained(ctx context.Context, tenantID, caller, ownerScope, dataID string) (RetainedRecord, error) {
	rec, err := getRetainedTx(ctx, s.db.Pool(), tenantID, ownerScope, dataID)
	if err != nil {
		return RetainedRecord{}, err
	}
	if rec.State == RetainedStatePurged {
		return RetainedRecord{}, ErrRetainedNotFound
	}
	key, err := s.nonceKey(ctx)
	if err != nil {
		return RetainedRecord{}, err
	}
	rec.PurgeNonce = mintPurgeNonce(key, caller, rec, time.Now().UTC())
	return *rec, nil
}

// GetRetained returns one record by id within a tenant ("" = any tenant).
// Used by the dispatcher-side attach plumbing; the API handlers go through
// the owner-scoped paths.
func (s *RetainedStore) GetRetained(ctx context.Context, tenantID, id string) (RetainedRecord, error) {
	rec, err := getRetainedTx(ctx, s.db.Pool(), tenantID, "", id)
	if err != nil {
		return RetainedRecord{}, err
	}
	return *rec, nil
}

// RetainedRefOf returns the retained_data_ref recorded on a workspace row
// ("" when the workspace does not consume a retained disk). The row is
// read regardless of lifecycle state — a deleted workspace still reports
// its disk so the dispatcher can return it to the inventory.
func (s *RetainedStore) RetainedRefOf(ctx context.Context, workspaceUID string) (string, error) {
	var ref string
	err := s.db.Pool().QueryRow(ctx,
		`SELECT COALESCE(retained_data_ref, '') FROM workspaces WHERE id = $1`,
		workspaceUID).Scan(&ref)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return ref, err
}

// ImportRetained records a dataset the operator moved into the inventory.
// Idempotent on (PVCNamespace, PVCUID): re-importing the same dataset
// returns the existing record. When the source workspace's reservation
// was already released, the disk's bytes are re-held against it — the
// disk exists, so it must keep counting until it is actually gone.
func (s *RetainedStore) ImportRetained(ctx context.Context, info RetainedDiskInfo) (rec RetainedRecord, err error) {
	err = s.db.WithTx(ctx, func(tx store.Tx) error {
		row, err := scanRetained(tx.QueryRow(ctx, `
			INSERT INTO retained_data (id, tenant_id, owner_subject,
				pvc_namespace, pvc_name, pvc_uid,
				source_workspace_id, source_workspace_name, runtime, size_bytes)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
			ON CONFLICT (pvc_namespace, pvc_uid) DO NOTHING
			RETURNING `+retainedCols,
			newRetainedID(), info.TenantID, info.Owner,
			info.PVCNamespace, info.PVCName, info.PVCUID,
			info.SourceWorkspaceID, info.SourceWorkspaceName,
			info.Runtime, info.SizeBytes))
		switch {
		case err == nil:
			rec = *row
		case errors.Is(err, pgx.ErrNoRows):
			// The dataset is already imported — idempotent re-import.
			r2, serr := scanRetained(tx.QueryRow(ctx, `
				SELECT `+retainedCols+` FROM retained_data
				WHERE pvc_namespace = $1 AND pvc_uid = $2`,
				info.PVCNamespace, info.PVCUID))
			if serr != nil {
				return fmt.Errorf("retained: re-import %w", serr)
			}
			rec = *r2
		default:
			return fmt.Errorf("retained: import %w", err)
		}
		return restoreDiskQuota(ctx, tx, info.TenantID, info.SourceWorkspaceID, info.SizeBytes)
	})
	return rec, err
}

// AttachRetained claims the record (Retained -> Attaching) and creates a
// new workspace consuming the disk, atomically with the quota reservation
// and the create intent. The source workspace's held disk bytes move to
// the new workspace's reservation — counted once, never twice.
func (s *RetainedStore) AttachRetained(ctx context.Context, tenantID, caller, ownerScope, dataID, idemKey string, req AttachRequest, bodyHash []byte) (res WorkspaceRecord, err error) {
	desired := req.DesiredState
	if desired == "" {
		desired = "Stopped"
	}
	if desired != "Running" && desired != "Stopped" {
		return res, fmt.Errorf("attach: bad desiredState %q: %w", desired, ErrInvalidState)
	}
	if bodyHash == nil {
		bodyHash = []byte{}
	}
	err = s.db.WithTx(ctx, func(tx store.Tx) error {
		requestID := ""
		scopedKey := ""
		if idemKey != "" {
			scopedKey = scopedIdemKey(caller, idemKey)
			idem, replayed, err := BeginIdempotent(ctx, tx, tenantID, scopedKey, "attach:"+dataID, bodyHash)
			if err != nil {
				return err
			}
			requestID = idem.RequestID
			if replayed {
				if err := json.Unmarshal(idem.Result, &res); err != nil {
					return fmt.Errorf("attach: stored result %w", err)
				}
				// SEC-21: a replayed result must still be inside the
				// caller's owner scope — owner check before replay.
				if ownerScope != "" && res.Owner != ownerScope {
					return ErrRetainedNotFound
				}
				res.Replayed = true
				return nil
			}
		}
		if requestID == "" {
			requestID = newRetainedID() // any unique string satisfies request_id UNIQUE
		}

		rec, err := getRetainedTx(ctx, tx, tenantID, ownerScope, dataID, "FOR UPDATE")
		if err != nil {
			return err
		}
		if rec.State != RetainedStateRetained {
			return ErrRetainedState
		}
		if rec.Runtime != req.Template.Runtime {
			return ErrRuntimeMismatch
		}

		issuer, sub, _ := strings.Cut(rec.Owner, "|")
		res.ID = newWorkspaceID()
		res.RequestID = requestID
		tpl, _ := json.Marshal(req.Template)
		var gen int64
		if desired == "Running" {
			gen = 1
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO workspaces (id, tenant_id, owner_subject, owner_issuer, owner_sub,
				request_id, name, template, data_policy, desired_state,
				runtime_generation, phase, retained_data_ref)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'Retain', $9, $10, 'Pending', $11)`,
			res.ID, tenantID, rec.Owner, issuer, sub,
			requestID, req.Name, tpl, desired, gen, dataID); err != nil {
			if isUniqueViolation(err) {
				return ErrNameTaken
			}
			return fmt.Errorf("attach: workspace %w", err)
		}
		// The disk's held bytes move to the consuming workspace's
		// reservation: release them under the source, then reserve the
		// full vector under the consumer. Compute dimensions follow the
		// normal per-workspace reservation; the disk dimension is the
		// retained dataset's real size, not the template default.
		if err := releaseDiskQuota(ctx, tx, rec.SourceWorkspaceID, rec.SizeBytes); err != nil {
			return err
		}
		if err := Reserve(ctx, tx, tenantID, res.ID, ResourceVector{
			RunningSlots: req.Vector.RunningSlots,
			CPUMillis:    req.Vector.CPUMillis,
			MemoryBytes:  req.Vector.MemoryBytes,
			DiskBytes:    rec.SizeBytes,
		}); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `
			UPDATE retained_data
			SET state = 'Attaching', consuming_workspace_id = $3,
			    transition_seq = transition_seq + 1, updated_at = now()
			WHERE id = $1 AND tenant_id = $2 AND state = 'Retained'`,
			dataID, tenantID, res.ID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrRetainedState // raced a purge: the claim is lost
		}
		rev, err := appendIntent(ctx, tx, PlatformID(res.ID), IntentCreate, &IntentSpec{
			WorkspaceName:   req.Name,
			TemplateName:    req.Template.Name,
			OwnerIssuer:     issuer,
			OwnerSubject:    sub,
			DataPolicy:      "Retain",
			RetainedDataRef: dataID,
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
		if idemKey != "" {
			return CompleteIdempotent(ctx, tx, tenantID, scopedKey, res)
		}
		return nil
	})
	return res, err
}

// PurgeRetained transitions Retained -> Purging. nonce must match the
// nonce issued to caller on the latest read (bound to the record's
// transition epoch — a transition consumes it). Records already
// Purging/Purged replay idempotently under a fresh nonce; records in
// Attaching/Attached fail ErrRetainedState.
func (s *RetainedStore) PurgeRetained(ctx context.Context, tenantID, caller, ownerScope, dataID, nonce, idemKey string, bodyHash []byte) (rec RetainedRecord, err error) {
	if bodyHash == nil {
		bodyHash = []byte{}
	}
	err = s.db.WithTx(ctx, func(tx store.Tx) error {
		scopedKey := ""
		if idemKey != "" {
			scopedKey = scopedIdemKey(caller, idemKey)
			idem, replayed, err := BeginIdempotent(ctx, tx, tenantID, scopedKey, "purge:"+dataID, bodyHash)
			if err != nil {
				return err
			}
			if replayed {
				if err := json.Unmarshal(idem.Result, &rec); err != nil {
					return fmt.Errorf("purge: stored result %w", err)
				}
				// SEC-21: owner check before replay — a replayed result
				// outside the caller's scope is never returned.
				if ownerScope != "" && rec.Owner != ownerScope {
					return ErrRetainedNotFound
				}
				return nil
			}
		}
		r, err := getRetainedTx(ctx, tx, tenantID, ownerScope, dataID, "FOR UPDATE")
		if err != nil {
			return err
		}
		key, err := s.nonceKey(ctx)
		if err != nil {
			return err
		}
		if !verifyPurgeNonce(key, caller, r, nonce, time.Now().UTC()) {
			return ErrPurgeNonce
		}
		switch r.State {
		case RetainedStateRetained:
			tag, err := tx.Exec(ctx, `
				UPDATE retained_data
				SET state = 'Purging', transition_seq = transition_seq + 1, updated_at = now()
				WHERE id = $1 AND state = 'Retained'`, dataID)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				return ErrRetainedState
			}
			r.State = RetainedStatePurging
			r.TransitionSeq++
			r.UpdatedAt = time.Now().UTC()
		case RetainedStatePurging, RetainedStatePurged:
			// Naturally idempotent replay under a fresh nonce.
		default:
			return ErrRetainedState
		}
		r.PurgeNonce = mintPurgeNonce(key, caller, r, time.Now().UTC())
		rec = *r
		if idemKey != "" {
			return CompleteIdempotent(ctx, tx, tenantID, scopedKey, rec)
		}
		return nil
	})
	return rec, err
}

// CompleteAttach settles Attaching -> Attached once the disk is verified
// mounted by the consuming workspace.
func (s *RetainedStore) CompleteAttach(ctx context.Context, dataID, workspaceUID string) error {
	return s.db.WithTx(ctx, func(tx store.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE retained_data
			SET state = 'Attached', transition_seq = transition_seq + 1, updated_at = now()
			WHERE id = $1 AND state = 'Attaching' AND consuming_workspace_id = $2`,
			dataID, workspaceUID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() > 0 {
			return nil
		}
		var exists bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM retained_data WHERE id = $1)`,
			dataID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrRetainedNotFound
		}
		return ErrRetainedState
	})
}

// ReturnToRetained rolls Attaching/Attached back to Retained when the
// consuming workspace is deleted. It is a no-op when the record is not
// bound to workspaceUID. The disk's held bytes return to the source
// workspace's reservation.
func (s *RetainedStore) ReturnToRetained(ctx context.Context, dataID, workspaceUID string) error {
	return s.db.WithTx(ctx, func(tx store.Tx) error {
		rec, err := getRetainedTx(ctx, tx, "", "", dataID, "FOR UPDATE")
		if err != nil {
			return err
		}
		if rec.ConsumingWorkspaceID != workspaceUID {
			return nil // not bound to that workspace: no-op
		}
		if rec.State != RetainedStateAttaching && rec.State != RetainedStateAttached {
			return ErrRetainedState
		}
		if _, err := tx.Exec(ctx, `
			UPDATE retained_data
			SET state = 'Retained', consuming_workspace_id = NULL,
			    transition_seq = transition_seq + 1, updated_at = now()
			WHERE id = $1`, dataID); err != nil {
			return err
		}
		if err := releaseDiskQuota(ctx, tx, workspaceUID, rec.SizeBytes); err != nil {
			return err
		}
		return returnDiskQuota(ctx, tx, rec.TenantID, rec.SourceWorkspaceID, rec.SizeBytes)
	})
}

// CompletePurge settles Purging -> Purged after the operator proved the
// volume is gone; the held disk reservation is released in the same
// transaction — never before the deletion proof.
func (s *RetainedStore) CompletePurge(ctx context.Context, dataID string) error {
	return s.db.WithTx(ctx, func(tx store.Tx) error {
		rec, err := getRetainedTx(ctx, tx, "", "", dataID, "FOR UPDATE")
		if err != nil {
			return err
		}
		if rec.State != RetainedStatePurging {
			return ErrRetainedState
		}
		if _, err := tx.Exec(ctx, `
			UPDATE retained_data
			SET state = 'Purged', purged_at = now(),
			    transition_seq = transition_seq + 1, updated_at = now()
			WHERE id = $1 AND state = 'Purging'`, dataID); err != nil {
			return err
		}
		return releaseDiskQuota(ctx, tx, rec.SourceWorkspaceID, rec.SizeBytes)
	})
}

// RetainedPVCUIDs maps live (non-Purged) record PVC UIDs to record ids so
// the operator inventory can flag PVCs without records and records
// without PVCs.
func (s *RetainedStore) RetainedPVCUIDs(ctx context.Context, tenantID string) (map[string]string, error) {
	rows, err := s.db.Pool().Query(ctx, `
		SELECT pvc_uid, id FROM retained_data
		WHERE tenant_id = $1 AND state <> 'Purged'`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var uid, id string
		if err := rows.Scan(&uid, &id); err != nil {
			return nil, err
		}
		out[uid] = id
	}
	return out, rows.Err()
}

// PendingPurges lists records in state Purging — the durable destroy
// intents awaiting cluster-side volume deletion. tenantID "" sweeps all
// tenants.
func (s *RetainedStore) PendingPurges(ctx context.Context, tenantID string) ([]RetainedRecord, error) {
	query := `SELECT ` + retainedCols + ` FROM retained_data WHERE state = 'Purging'`
	args := []any{}
	if tenantID != "" {
		query += ` AND tenant_id = $1`
		args = append(args, tenantID)
	}
	rows, err := s.db.Pool().Query(ctx, query+` ORDER BY retained_at, id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RetainedRecord
	for rows.Next() {
		rec, err := scanRetained(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *rec)
	}
	return out, rows.Err()
}
