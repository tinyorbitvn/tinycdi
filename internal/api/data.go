package api

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/observability"
	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
	"github.com/tinyorbitvn/tinycdi/internal/store"
)

// The retained-record domain types live in provisioning so the
// Postgres-backed store (internal/provisioning/retained.go, migration 007)
// and this API surface share one representation. The aliases keep the
// api.Retained* spellings the contract tests pin.
type (
	// RetainedDataState is the retained-record state machine (design §5):
	// Retained -> Attaching -> Attached and Retained -> Purging -> Purged.
	// Transitions are conditional updates in the same transaction as the
	// quota reservation and the outbox intent, so attach and purge exclude
	// each other and a disk has at most one consumer.
	RetainedDataState = provisioning.RetainedDataState
	// RetainedRecord is the store read model for one retained dataset.
	// Dataset identity is (PVCNamespace, PVCUID) + SourceWorkspaceID —
	// never a bare PVC name, which is reusable and spoofable.
	RetainedRecord = provisioning.RetainedRecord
	// RetainedDiskInfo is the operator-supplied description of a dataset
	// that entered the retained inventory (from PVC metadata — the source
	// of truth for identity). ImportRetained consumes it.
	RetainedDiskInfo = provisioning.RetainedDiskInfo
	// AttachRequest is the resolved attach intent the store consumes; the
	// handler resolves templateRef into Template and Vector first. The new
	// workspace's dataPolicy is Retain by definition.
	AttachRequest = provisioning.AttachRequest
)

const (
	RetainedStateRetained  = provisioning.RetainedStateRetained
	RetainedStateAttaching = provisioning.RetainedStateAttaching
	RetainedStateAttached  = provisioning.RetainedStateAttached
	RetainedStatePurging   = provisioning.RetainedStatePurging
	RetainedStatePurged    = provisioning.RetainedStatePurged
)

// Domain errors mapped onto the stable error model by writeDataError.
var (
	// ErrRetainedNotFound covers unknown ids AND records owned by someone
	// else: a foreign id is indistinguishable from a missing one, matching
	// the workspace convention (existence is never leaked). Tenant admins
	// (ownerScope "") see the whole tenant.
	ErrRetainedNotFound = provisioning.ErrRetainedNotFound
	// ErrRetainedState is returned when the record's state does not allow
	// the requested transition (attach or purge on a non-Retained record,
	// illegal transition attempts). Maps to 409 INVALID_STATE.
	ErrRetainedState = provisioning.ErrRetainedState
	// ErrPurgeNonce is returned when the confirmation nonce is missing,
	// malformed, expired, issued to a different principal, bound to a
	// stale record state, or already consumed by an earlier transition.
	// Maps to 400 INVALID_REQUEST.
	ErrPurgeNonce = provisioning.ErrPurgeNonce
	// ErrRuntimeMismatch is returned when the requested template's runtime
	// differs from the retained disk's runtime (openapi: 422
	// INVALID_TEMPLATE).
	ErrRuntimeMismatch = provisioning.ErrRuntimeMismatch
)

// RetainedDataStore is the transactional contract behind /v1/data. The
// Postgres implementation (migration 007) must satisfy:
//   - every transition is a conditional UPDATE ... WHERE state = <expected>
//     committed in the SAME transaction as the quota reservation and the
//     outbox intent, so attach and purge exclude each other atomically;
//   - disk quota stays 'held' (against the source workspace's reservation)
//     until CompletePurge observes actual volume deletion;
//   - AttachRetained creates the new workspace row + reservation + create
//     intent in one transaction with the Retained -> Attaching claim, and
//     re-keys the held disk reservation to the new workspace (no double
//     counting);
//   - replayed attach/purge (same Idempotency-Key + same body) return the
//     stored result; same key + different body is ErrIdempotencyConflict.
type RetainedDataStore interface {
	// ListRetained returns records in tenantID visible to ownerScope (""
	// = tenant-wide, admin only), paginated by cursor. Each record carries
	// a fresh PurgeNonce bound to caller (iss|sub), the record id and the
	// current transition epoch.
	ListRetained(ctx context.Context, tenantID, caller, ownerScope, cursor string, limit int) ([]RetainedRecord, string, error)

	// ReadRetained returns one record visible to ownerScope ("" = tenant-wide,
	// admin only) with a fresh PurgeNonce bound to caller, exactly as a list
	// row. Unknown ids, foreign tenants and (for non-admins) other owners'
	// records all fail ErrRetainedNotFound.
	ReadRetained(ctx context.Context, tenantID, caller, ownerScope, dataID string) (RetainedRecord, error)

	// ImportRetained records a dataset the operator moved into the
	// inventory (workspace delete with dataPolicy=Retain, or inventory
	// reconstruction after API DB loss). Idempotent on
	// (PVCNamespace, PVCUID): re-importing the same dataset returns the
	// existing record.
	ImportRetained(ctx context.Context, info RetainedDiskInfo) (RetainedRecord, error)

	// AttachRetained claims the record (Retained -> Attaching) and creates
	// a new workspace consuming the disk, atomically with quota + outbox.
	// Fails with ErrRetainedNotFound (unknown/foreign), ErrRetainedState
	// (not Retained), ErrRuntimeMismatch, QuotaExceededError /
	// ErrNoQuota, or ErrIdempotencyConflict. ownerScope "" means admin;
	// caller is the authenticated principal and binds the idempotency key.
	AttachRetained(ctx context.Context, tenantID, caller, ownerScope, dataID, idemKey string, req AttachRequest, bodyHash []byte) (provisioning.WorkspaceRecord, error)

	// PurgeRetained transitions Retained -> Purging in one transaction
	// with the outbox destroy intent. nonce must match the nonce issued to
	// caller on the latest read. Replaying the consumed nonce (idempotent
	// retry, with or without Idempotency-Key) returns the current record;
	// any other nonce fails ErrPurgeNonce. Records in Attaching/Attached
	// fail ErrRetainedState.
	PurgeRetained(ctx context.Context, tenantID, caller, ownerScope, dataID, nonce, idemKey string, bodyHash []byte) (RetainedRecord, error)

	// CompleteAttach settles Attaching -> Attached once the operator
	// reports the disk mounted by the consuming workspace.
	CompleteAttach(ctx context.Context, dataID, workspaceUID string) error

	// ReturnToRetained rolls Attaching/Attached back to Retained when the
	// consuming workspace is deleted before the attach completed. No-op
	// when the record is not bound to workspaceUID.
	ReturnToRetained(ctx context.Context, dataID, workspaceUID string) error

	// CompletePurge settles Purging -> Purged after the operator proved
	// the volume is gone; releases the held disk reservation in the same
	// transaction.
	CompletePurge(ctx context.Context, dataID string) error

	// RetainedPVCUIDs maps live (non-Purged) record PVC UIDs to record ids
	// so the operator inventory can flag PVCs without records and records
	// without PVCs.
	RetainedPVCUIDs(ctx context.Context, tenantID string) (map[string]string, error)
}

// NewRetainedStore returns the Postgres-backed retained-data store.
func NewRetainedStore(db *store.DB) RetainedDataStore {
	return provisioning.NewRetainedStore(db)
}

var _ RetainedDataStore = (*provisioning.RetainedStore)(nil)

// ---------------------------------------------------------------------------
// Public JSON shapes (must match openapi.yaml exactly).
// ---------------------------------------------------------------------------

// retainedDataView is the public record (openapi RetainedDataView).
type retainedDataView struct {
	ID                     string    `json:"id"`
	State                  string    `json:"state"`
	Owner                  Owner     `json:"owner"`
	SizeGiB                int64     `json:"sizeGib"`
	Runtime                string    `json:"runtime"`
	SourceWorkspaceName    string    `json:"sourceWorkspaceName"`
	ConsumingWorkspaceID   string    `json:"consumingWorkspaceId,omitempty"`
	RetainedAt             time.Time `json:"retainedAt"`
	PurgeConfirmationNonce string    `json:"purgeConfirmationNonce"`
}

// retainedDataList is the paginated list response (openapi RetainedDataList).
type retainedDataList struct {
	Items         []retainedDataView `json:"items"`
	NextPageToken string             `json:"nextPageToken,omitempty"`
}

func recordToRetainedView(r *RetainedRecord) retainedDataView {
	return retainedDataView{
		ID:                     r.ID,
		State:                  string(r.State),
		Owner:                  ownerFallback(r.Owner),
		SizeGiB:                (r.SizeBytes + (1 << 30) - 1) / (1 << 30),
		Runtime:                r.Runtime,
		SourceWorkspaceName:    r.SourceWorkspaceName,
		ConsumingWorkspaceID:   r.ConsumingWorkspaceID,
		RetainedAt:             r.RetainedAt,
		PurgeConfirmationNonce: r.PurgeNonce,
	}
}

var retainedIDPattern = regexp.MustCompile(`^rd_[A-Za-z0-9]{8,64}$`)

type attachDataRequest struct {
	Name         string `json:"name"`
	TemplateRef  string `json:"templateRef"`
	DesiredState string `json:"desiredState,omitempty"`
}

type purgeDataRequest struct {
	ConfirmationNonce string `json:"confirmationNonce"`
}

// ---------------------------------------------------------------------------
// Handler — /v1/data per openapi.yaml.
// ---------------------------------------------------------------------------

// DataHandler implements /v1/data per openapi.yaml.
type DataHandler struct {
	data      RetainedDataStore
	catalog   TemplateCatalog
	tenants   TenantResolver
	directory Directory
	maxBody   int64
	now       func() time.Time
	// releaseRetryAfter estimates seconds until the next recovery pass for
	// the Retry-After header of a release-pending QUOTA_EXHAUSTED. Nil
	// reports the 30 s cadence ceiling.
	releaseRetryAfter func() int
	// audit is the dedicated audit-event sink the mutating routes emit
	// through (nil = no domain audit events).
	audit observability.AuditSink
}

// NewDataHandler wires the handler. catalog resolves the attach
// templateRef; tenants gates tenant provisioning like the workspace routes.
func NewDataHandler(d RetainedDataStore, c TemplateCatalog, t TenantResolver) *DataHandler {
	return &DataHandler{data: d, catalog: c, tenants: t,
		maxBody: 64 << 10, now: time.Now}
}

// WithAuditSink attaches the audit sink the retained-data mutation routes
// write their dedicated audit events to.
func (h *DataHandler) WithAuditSink(s observability.AuditSink) *DataHandler {
	h.audit = s
	return h
}

// WithReleaseRetryAfter sets the Retry-After estimate used for a
// release-pending QUOTA_EXHAUSTED response (nil → 30 s ceiling).
func (h *DataHandler) WithReleaseRetryAfter(f func() int) *DataHandler {
	h.releaseRetryAfter = f
	return h
}

// retryAfterSeconds resolves the Retry-After estimate for release-pending
// quota refusals.
func (h *DataHandler) retryAfterSeconds() int {
	if h.releaseRetryAfter != nil {
		return h.releaseRetryAfter()
	}
	return 30
}

// WithDirectory attaches the principal directory that fills owner display
// names on views. Nil falls back to bare subjects.
func (h *DataHandler) WithDirectory(d Directory) *DataHandler {
	h.directory = d
	return h
}

// MountDataRoutes registers the retained-data routes: RequireAuthPassive
// on the reads (the portal polls them), RequireAuth+RequireCSRF on
// attach/purge writes.
func MountDataRoutes(mux *http.ServeMux, authn *Authenticator, h *DataHandler) {
	safe := func(h http.Handler) http.Handler { return authn.RequireAuthPassive(h) }
	// unsafe mounts an audited mutation route (see MountWorkspaceRoutes).
	unsafe := func(pattern string, next http.Handler) {
		mux.Handle(pattern, authn.RequireAuth(
			audited(h.audit, pattern, authn.RequireCSRF(next))))
	}
	mux.Handle("GET /v1/data", safe(http.HandlerFunc(h.List)))
	mux.Handle("GET /v1/data/{dataId}", safe(http.HandlerFunc(h.Get)))
	unsafe(routeDataAttach, http.HandlerFunc(h.Attach))
	unsafe(routeDataPurge, http.HandlerFunc(h.Purge))
}

// principalOrFail resolves the verified principal and gates tenant
// provisioning, mirroring the workspace handler's convention.
func (h *DataHandler) principalOrFail(w http.ResponseWriter, r *http.Request) (Principal, bool) {
	p, ok := PrincipalFromContext(r.Context())
	if !ok {
		writeError(w, r, CodeUnauthenticated, "authentication required")
		return p, false
	}
	if _, ok := h.tenants.Namespace(p.TenantID); !ok {
		writeError(w, r, CodeForbidden, "tenant is not provisioned")
		return p, false
	}
	return p, true
}

// List handles GET /v1/data.
func (h *DataHandler) List(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principalOrFail(w, r)
	if !ok {
		return
	}
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 200 {
			writeError(w, r, CodeInvalidRequest, "limit must be 1..200")
			return
		}
		limit = n
	}
	scope, ok := listScope(w, r, p)
	if !ok {
		return
	}
	recs, next, err := h.data.ListRetained(r.Context(), p.TenantID,
		p.Owner(), scope, r.URL.Query().Get("pageToken"), limit)
	if err != nil {
		h.writeDataError(w, r, err)
		return
	}
	refs := make([]string, 0, len(recs))
	for i := range recs {
		refs = append(refs, recs[i].Owner)
	}
	owners := resolveOwners(r.Context(), h.directory, p.TenantID, refs)
	out := retainedDataList{Items: make([]retainedDataView, 0, len(recs)), NextPageToken: next}
	for i := range recs {
		v := recordToRetainedView(&recs[i])
		v.Owner = owners[recs[i].Owner]
		out.Items = append(out.Items, v)
	}
	respondJSON(w, out)
}

// Get handles GET /v1/data/{dataId}: one record with a fresh purge nonce.
// Visibility is the list's — the owner, or a tenant-admin of the same
// tenant; anyone else (and any other tenant) gets a 404.
func (h *DataHandler) Get(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principalOrFail(w, r)
	if !ok {
		return
	}
	dataID := r.PathValue("dataId")
	if !retainedIDPattern.MatchString(dataID) {
		writeError(w, r, CodeInvalidRequest, "bad retained data id")
		return
	}
	rec, err := h.data.ReadRetained(r.Context(), p.TenantID, p.Owner(), ownerScope(p), dataID)
	if err != nil {
		h.writeDataError(w, r, err)
		return
	}
	v := recordToRetainedView(&rec)
	v.Owner = resolveOwners(r.Context(), h.directory, p.TenantID, []string{rec.Owner})[rec.Owner]
	respondJSON(w, v)
}

// Attach handles POST /v1/data/{dataId}/attach.
func (h *DataHandler) Attach(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principalOrFail(w, r)
	if !ok {
		return
	}
	dataID := r.PathValue("dataId")
	if !retainedIDPattern.MatchString(dataID) {
		writeError(w, r, CodeInvalidRequest, "bad retained data id")
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if len(key) < 8 || len(key) > 128 {
		writeError(w, r, CodeInvalidRequest, "Idempotency-Key header required (8..128 chars)")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, h.maxBody))
	if err != nil {
		writeError(w, r, CodeInvalidRequest, "unreadable or oversized body")
		return
	}
	var req attachDataRequest
	if !decodeJSON(body, &req) {
		writeError(w, r, CodeInvalidRequest, "invalid request body")
		return
	}
	if !workspaceNamePattern.MatchString(req.Name) {
		writeError(w, r, CodeInvalidRequest, "name must be a DNS-label-style name")
		return
	}
	if !templateRefPattern.MatchString(req.TemplateRef) {
		writeError(w, r, CodeInvalidRequest, "templateRef must be a tpl_ identifier")
		return
	}
	if req.DesiredState != "" && req.DesiredState != "Running" && req.DesiredState != "Stopped" {
		writeError(w, r, CodeInvalidRequest, "desiredState must be Running or Stopped")
		return
	}
	tpl, err := h.catalog.Resolve(r.Context(), p.TenantID, req.TemplateRef)
	if err != nil && !errors.Is(err, ErrTemplateNotFound) {
		h.writeDataError(w, r, err)
		return
	}
	if err != nil || tpl.ID == "" {
		writeError(w, r, CodeInvalidTemplate, "unknown template")
		return
	}
	sum := sha256.Sum256(body)
	rec, err := h.data.AttachRetained(r.Context(), p.TenantID, p.Owner(), ownerScope(p), dataID, key, AttachRequest{
		Name: req.Name,
		Template: provisioning.TemplateInfo{
			ID: tpl.ID, Name: tpl.Name, Revision: tpl.Revision,
			RevisionLabel: tpl.RevisionLabel,
			Runtime:       tpl.Runtime, Experience: tpl.Experience,
			ImageBuiltAt: tpl.ImageBuiltAt,
		},
		Vector: provisioning.ResourceVector{
			RunningSlots: 1,
			CPUMillis:    tpl.CPUMillis,
			MemoryBytes:  tpl.MemoryMiB << 20,
			DiskBytes:    tpl.StorageGiB << 30,
		},
		DesiredState: req.DesiredState,
	}, sum[:])
	if err != nil {
		h.writeDataError(w, r, err)
		return
	}
	auditSetDetail(r.Context(), "workspace", rec.ID)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	v := recordToView(&rec)
	v.Owner = resolveOwner(r.Context(), h.directory, p.TenantID, rec.Owner)
	_ = json.NewEncoder(w).Encode(v)
}

// Purge handles POST /v1/data/{dataId}/purge.
func (h *DataHandler) Purge(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principalOrFail(w, r)
	if !ok {
		return
	}
	dataID := r.PathValue("dataId")
	if !retainedIDPattern.MatchString(dataID) {
		writeError(w, r, CodeInvalidRequest, "bad retained data id")
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if key != "" && (len(key) < 8 || len(key) > 128) {
		writeError(w, r, CodeInvalidRequest, "Idempotency-Key must be 8..128 chars")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, h.maxBody))
	if err != nil {
		writeError(w, r, CodeInvalidRequest, "unreadable or oversized body")
		return
	}
	var req purgeDataRequest
	if !decodeJSON(body, &req) {
		writeError(w, r, CodeInvalidRequest, "invalid request body")
		return
	}
	if len(req.ConfirmationNonce) < 8 || len(req.ConfirmationNonce) > 512 {
		writeError(w, r, CodeInvalidRequest, "invalid or consumed confirmation nonce")
		return
	}
	sum := sha256.Sum256(body)
	rec, err := h.data.PurgeRetained(r.Context(), p.TenantID, p.Owner(),
		ownerScope(p), dataID, req.ConfirmationNonce, key, sum[:])
	if err != nil {
		h.writeDataError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	v := recordToRetainedView(&rec)
	v.Owner = resolveOwner(r.Context(), h.directory, p.TenantID, rec.Owner)
	_ = json.NewEncoder(w).Encode(v)
}

// writeDataError maps retained-store errors onto the stable error model.
func (h *DataHandler) writeDataError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrRetainedNotFound):
		writeError(w, r, CodeNotFound, "retained data record not found")
	case errors.Is(err, ErrRetainedState):
		writeError(w, r, CodeInvalidState, err.Error())
	case errors.Is(err, ErrPurgeNonce):
		writeError(w, r, CodeInvalidRequest, "invalid or consumed confirmation nonce")
	case errors.Is(err, ErrRuntimeMismatch):
		writeError(w, r, CodeInvalidTemplate, "template runtime does not match the retained disk")
	case errors.Is(err, provisioning.ErrBadCursor):
		writeError(w, r, CodeInvalidRequest, "bad pageToken")
	case errors.Is(err, provisioning.ErrNoQuota):
		writeError(w, r, CodeQuotaNotConfigured, quotaNotConfiguredMessage)
	case provisioning.IsQuotaExceeded(err):
		writeQuotaExceeded(w, r, err, h.retryAfterSeconds())
	case provisioning.IsIdempotencyConflict(err):
		writeError(w, r, CodeIdempotencyConflict, "idempotency key reused with a different request")
	case errors.Is(err, provisioning.ErrNameTaken):
		writeError(w, r, CodeInvalidState, "workspace name already in use")
	default:
		writeError(w, r, CodeInternal, "internal error")
	}
}
