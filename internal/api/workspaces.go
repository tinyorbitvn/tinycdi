package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/observability"
	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
	"github.com/tinyorbitvn/tinycdi/internal/store"
)

// respondJSON serializes v as an application/json response.
func respondJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// decodeJSON decodes exactly one JSON document from body into v: unknown
// fields are rejected and trailing data after the first value is an error.
// The returned detail is for server-side logs only — it is never echoed to
// the client (SEC-I7); callers respond with a generic "invalid request
// body".
func decodeJSON(body []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("json: unexpected trailing data")
		}
		return err
	}
	return nil
}

// TenantResolver maps a verified tenant ID to the Kubernetes namespace
// holding its Workspace CRs. Unknown tenants are rejected with FORBIDDEN.
type TenantResolver interface {
	Namespace(tenantID string) (ns string, ok bool)
}

// StaticTenantResolver is a config-driven TenantResolver.
type StaticTenantResolver map[string]string

// Namespace implements TenantResolver.
func (m StaticTenantResolver) Namespace(tenantID string) (string, bool) {
	ns, ok := m[tenantID]
	return ns, ok
}

// TenantAdminGroup is the verified group claim that widens list/get to the
// whole tenant instead of the caller's own workspaces.
const TenantAdminGroup = "tenant-admin"

// TemplateEntry is a catalog entry resolvable to a templateRef.
type TemplateEntry struct {
	ID          string
	Name        string // catalog (family) name
	Description string
	Revision    int64
	// RevisionLabel is the raw spec.revision identifier ("2026-10-b");
	// Revision above parses only a leading integer prefix.
	RevisionLabel string
	Runtime       string
	Experience    string
	CPUMillis     int64
	MemoryMiB     int64
	StorageGiB    int64
	// StorageBytes is the untruncated disk size; StorageGiB is the same
	// value rounded down for the public view.
	StorageBytes           int64
	IdleTimeoutSeconds     int64
	DisconnectGraceSeconds int64
	MaxRunningSeconds      int64
	DataPolicyDefault      string
	ClipboardPolicy        string
	NetworkProfile         string
	// ImageUpdate is the revision's lifecycle.imageUpdate policy
	// (OnStart | Pinned); "" reads as the OnStart default.
	ImageUpdate string
	PublishedAt time.Time
	// ImageBuiltAt is the raw image-built-at annotation value carried by
	// the resolved WorkspaceTemplate (RFC 3339 when well formed).
	ImageBuiltAt string
	// ImageEngines carries the browser engine versions baked into the
	// runtime image (e.g. {"chromium": "...", "firefox": "..."}); nil when
	// the template declares none.
	ImageEngines map[string]string
}

// ErrTemplateNotFound means templateRef does not resolve in the caller's
// tenant catalog.
var ErrTemplateNotFound = errors.New("template not found")

// TemplateCatalog resolves and lists templates for a tenant.
type TemplateCatalog interface {
	Resolve(ctx context.Context, tenantID, templateID string) (TemplateEntry, error)
	List(ctx context.Context, tenantID, runtimeFilter, cursor string, limit int) ([]TemplateEntry, string, error)
}

// FamilyCatalog is the optional catalog surface a TemplateCatalog may add
// to resolve the newest published revision of a template family. When the
// handler's catalog does not implement it, updateAvailable stays false.
type FamilyCatalog interface {
	NewestInFamily(ctx context.Context, tenantID, family string) (TemplateEntry, error)
}

// workspaceBackend is the provisioning.Service surface the handler needs;
// an interface so unit tests can run without Postgres.
type workspaceBackend interface {
	CreateWorkspace(ctx context.Context, tenantID, idemKey string, req provisioning.CreateRequest, bodyHash []byte) (provisioning.WorkspaceRecord, error)
	// AttachRetained serves creates carrying retainedDataRef — the same
	// owner-scoped, claimed transition as POST /v1/data/{id}/attach.
	AttachRetained(ctx context.Context, tenantID, caller, ownerScope, dataID, idemKey string, req provisioning.AttachRequest, bodyHash []byte) (provisioning.WorkspaceRecord, error)
	GetWorkspace(ctx context.Context, tenantID, ownerScope, id string) (provisioning.WorkspaceRecord, error)
	ListWorkspaces(ctx context.Context, tenantID, ownerScope, phase, cursor string, limit int) ([]provisioning.WorkspaceRecord, string, error)
	SignalWorkspace(ctx context.Context, tenantID, caller, ownerScope, wsID, idemKey string, kind provisioning.IntentKind, bodyHash []byte) (provisioning.WorkspaceRecord, error)
}

// WorkspaceHandler implements /v1/workspaces per openapi.yaml.
type WorkspaceHandler struct {
	backend workspaceBackend
	catalog TemplateCatalog
	// imageCatalog serves the image-age fallback (WithImageCatalog); nil
	// falls back to catalog.
	imageCatalog TemplateCatalog
	tenants      TenantResolver
	statusView   StatusView
	directory    Directory
	intentLog    IntentLog
	maxBody      int64
	staleAfter   time.Duration
	// blockAfter is the E3 stale-image admission limit
	// (-image-block-after): creates resolving to a runtime image older
	// than it are refused 409 IMAGE_STALE. <=0 disables the block.
	blockAfter time.Duration
	now        func() time.Time
	log        *slog.Logger
	// releaseRetryAfter estimates seconds until the next recovery pass for
	// the Retry-After header of a release-pending QUOTA_EXHAUSTED. Nil
	// reports the 30 s cadence ceiling.
	releaseRetryAfter func() int
	// audit is the dedicated audit-event sink the mutating routes emit
	// through (nil = no domain audit events).
	audit observability.AuditSink
}

// WithAuditSink attaches the audit sink the workspace mutation routes
// write their dedicated audit events to.
func (h *WorkspaceHandler) WithAuditSink(s observability.AuditSink) *WorkspaceHandler {
	h.audit = s
	return h
}

// WithReleaseRetryAfter sets the Retry-After estimate used for a
// release-pending QUOTA_EXHAUSTED response (nil → 30 s ceiling).
func (h *WorkspaceHandler) WithReleaseRetryAfter(f func() int) *WorkspaceHandler {
	h.releaseRetryAfter = f
	return h
}

// retryAfterSeconds resolves the Retry-After estimate for release-pending
// quota refusals.
func (h *WorkspaceHandler) retryAfterSeconds() int {
	if h.releaseRetryAfter != nil {
		return h.releaseRetryAfter()
	}
	return 30
}

// WithDirectory attaches the principal directory that fills owner display
// names on views. Nil falls back to bare subjects.
func (h *WorkspaceHandler) WithDirectory(d Directory) *WorkspaceHandler {
	h.directory = d
	return h
}

// NewWorkspaceHandler wires the handler. catalog may be nil when
// /v1/workspaces create is not served (tests); tenants is required.
func NewWorkspaceHandler(b workspaceBackend, c TemplateCatalog, t TenantResolver) *WorkspaceHandler {
	return &WorkspaceHandler{backend: b, catalog: c, tenants: t,
		maxBody: 64 << 10, staleAfter: DefaultImageStaleAfter, now: time.Now}
}

// WithImageStaleAfter sets the age after which a workspace's runtime image
// reports imageStale (the -image-stale-after flag). Non-positive keeps the
// default.
func (h *WorkspaceHandler) WithImageStaleAfter(d time.Duration) *WorkspaceHandler {
	if d > 0 {
		h.staleAfter = d
	}
	return h
}

// WithImageBlockAfter sets the E3 stale-image admission limit
// (-image-block-after). d <= 0 disables the block entirely; a handler
// built without it does not block.
func (h *WorkspaceHandler) WithImageBlockAfter(d time.Duration) *WorkspaceHandler {
	h.blockAfter = d
	return h
}

// MountWorkspaceRoutes registers the workspace/template routes with the
// authn middleware applied: RequireAuth on reads, RequireAuth+RequireCSRF
// on writes.
func MountWorkspaceRoutes(mux *http.ServeMux, authn *Authenticator, h *WorkspaceHandler, th *TemplateHandler) {
	safe := func(h http.Handler) http.Handler { return authn.RequireAuth(h) }
	// unsafe mounts an audited mutation route: RequireAuth verifies the
	// principal, audited records the request's outcome on the shared
	// routeAudit cell, RequireCSRF guards the state change itself, so a
	// CSRF refusal is emitted as a denied audit event too.
	unsafe := func(pattern string, next http.Handler) {
		mux.Handle(pattern, authn.RequireAuth(
			audited(h.audit, pattern, authn.RequireCSRF(next))))
	}
	mux.Handle("GET /v1/workspaces", safe(http.HandlerFunc(h.List)))
	unsafe(routeWorkspaceCreate, http.HandlerFunc(h.Create))
	mux.Handle("GET /v1/workspaces/{id}", safe(http.HandlerFunc(h.Get)))
	mux.Handle("GET /v1/workspaces/{id}/events", safe(http.HandlerFunc(h.Events)))
	unsafe(routeWorkspaceDelete, http.HandlerFunc(h.Delete))
	unsafe(routeWorkspaceStart, http.HandlerFunc(h.Start))
	unsafe(routeWorkspaceStop, http.HandlerFunc(h.Stop))
	if th != nil {
		mux.Handle("GET /v1/templates", safe(http.HandlerFunc(th.List)))
	}
}

// ---------------------------------------------------------------------------
// Public JSON shapes (must match openapi.yaml exactly).
// ---------------------------------------------------------------------------

type templateSummary struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Family is the catalog name shared by every revision of one template.
	Family     string `json:"family"`
	Revision   int64  `json:"revision"`
	Runtime    string `json:"runtime"`
	Experience string `json:"experience"`
}

type workspaceCondition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason"`
	Message string `json:"message,omitempty"`
	// Params carries the structured values the message interpolates (for
	// example the teardown step a CleanupRetry names) so clients can
	// localize the full text without parsing English. Projected from the
	// operator's condition-params annotation, absent when none apply.
	Params             map[string]string `json:"params,omitempty"`
	LastTransitionTime time.Time         `json:"lastTransitionTime"`
}

// WorkspaceView is the public workspace record (openapi WorkspaceView).
type WorkspaceView struct {
	ID              string               `json:"id"`
	Name            string               `json:"name"`
	Owner           Owner                `json:"owner"`
	Template        templateSummary      `json:"template"`
	Phase           string               `json:"phase"`
	Conditions      []workspaceCondition `json:"conditions"`
	DesiredState    string               `json:"desiredState"`
	DataPolicy      string               `json:"dataPolicy"`
	RetainedDataRef string               `json:"retainedDataRef,omitempty"`
	FailureReason   string               `json:"failureReason,omitempty"`
	CreatedAt       time.Time            `json:"createdAt"`
	UpdatedAt       time.Time            `json:"updatedAt"`
	ImageBuiltAt    *time.Time           `json:"imageBuiltAt,omitempty"`
	ImageStale      *bool                `json:"imageStale,omitempty"`
	// TemplateRevision is the raw revision identifier of the template the
	// workspace is on (spec.revision verbatim, e.g. "2026-10-b").
	TemplateRevision string `json:"templateRevision"`
	// UpdateAvailable is true when the workspace's template family
	// published a newer revision a start would move to.
	UpdateAvailable bool `json:"updateAvailable"`
}

// WorkspaceList is the paginated list response.
type WorkspaceList struct {
	Items         []WorkspaceView `json:"items"`
	NextPageToken string          `json:"nextPageToken,omitempty"`
}

func recordToView(r *provisioning.WorkspaceRecord) WorkspaceView {
	return WorkspaceView{
		ID:    r.ID,
		Name:  r.Name,
		Owner: ownerFallback(r.Owner),
		Template: templateSummary{
			ID:         r.Template.ID,
			Name:       r.Template.Name,
			Family:     r.Template.Name,
			Revision:   r.Template.Revision,
			Runtime:    r.Template.Runtime,
			Experience: r.Template.Experience,
		},
		Phase:            r.Phase,
		Conditions:       []workspaceCondition{},
		DesiredState:     r.DesiredState,
		DataPolicy:       r.DataPolicy,
		RetainedDataRef:  r.RetainedDataRef,
		FailureReason:    r.FailureReason,
		CreatedAt:        r.CreatedAt,
		UpdatedAt:        r.UpdatedAt,
		TemplateRevision: templateRevisionOf(&r.Template),
	}
}

// templateRevisionOf reports the workspace's template revision as the
// verbatim spec.revision label, falling back to the numeric revision on
// rows written before v0.3 carried the label.
func templateRevisionOf(t *provisioning.TemplateInfo) string {
	if t.RevisionLabel != "" {
		return t.RevisionLabel
	}
	if t.Revision > 0 {
		return strconv.FormatInt(t.Revision, 10)
	}
	return ""
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

var (
	workspaceIDPattern   = regexp.MustCompile(`^ws_[A-Za-z0-9]{8,64}$`)
	workspaceNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,126}[a-z0-9]$|^[a-z0-9]$`)
	templateRefPattern   = regexp.MustCompile(`^tpl_[A-Za-z0-9][A-Za-z0-9-]{6,62}[A-Za-z0-9]$`)
	retainedRefPattern   = regexp.MustCompile(`^rd_[A-Za-z0-9]{8,64}$`)
)

type createWorkspaceRequest struct {
	Name            string `json:"name"`
	TemplateRef     string `json:"templateRef"`
	DesiredState    string `json:"desiredState,omitempty"`
	DataPolicy      string `json:"dataPolicy,omitempty"`
	RetainedDataRef string `json:"retainedDataRef,omitempty"`
}

// ownerScope returns the caller's owner ref, or "" for tenant admins.
func ownerScope(p Principal) string {
	if p.InGroup(TenantAdminGroup) {
		return ""
	}
	return p.Owner()
}

// listScope resolves the ?scope= list parameter into the owner filter the
// store applies (D34): omitted keeps the caller's natural scope (admins
// see the tenant, users their own rows), "mine" forces the caller's own
// rows even for admins, and "tenant" widens to the whole tenant — which
// requires the tenant-admin role, else 403 FORBIDDEN. Unknown values are
// a 400. Shared by /v1/workspaces and /v1/data.
func listScope(w http.ResponseWriter, r *http.Request, p Principal) (string, bool) {
	switch r.URL.Query().Get("scope") {
	case "":
		return ownerScope(p), true
	case "mine":
		return p.Owner(), true
	case "tenant":
		if !p.InGroup(TenantAdminGroup) {
			writeError(w, r, CodeForbidden, "scope=tenant requires the tenant-admin role")
			return "", false
		}
		return "", true
	default:
		writeError(w, r, CodeInvalidRequest, "scope must be mine or tenant")
		return "", false
	}
}

func (h *WorkspaceHandler) principalOrFail(w http.ResponseWriter, r *http.Request) (Principal, bool) {
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

// List handles GET /v1/workspaces.
func (h *WorkspaceHandler) List(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principalOrFail(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	limit := 50
	if v := q.Get("limit"); v != "" {
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
	phase := q.Get("phase")
	recs, next, err := h.backend.ListWorkspaces(r.Context(), p.TenantID,
		scope, phase, q.Get("pageToken"), limit)
	if err != nil {
		h.writeBackendError(w, r, err)
		return
	}
	owners := h.resolveOwnerRefs(r.Context(), p.TenantID, recs)
	out := WorkspaceList{Items: make([]WorkspaceView, 0, len(recs)), NextPageToken: next}
	ctx := withImageAgeMemo(withUpdateCheckMemo(r.Context()))
	for i := range recs {
		v := h.viewWithStatus(ctx, &recs[i])
		v.Owner = owners[recs[i].Owner]
		out.Items = append(out.Items, v)
	}
	respondJSON(w, out)
}

// resolveOwnerRefs batches one directory lookup for a page of records so
// the list path does not query per row.
func (h *WorkspaceHandler) resolveOwnerRefs(ctx context.Context, tenantID string, recs []provisioning.WorkspaceRecord) map[string]Owner {
	refs := make([]string, 0, len(recs))
	for i := range recs {
		refs = append(refs, recs[i].Owner)
	}
	return resolveOwners(ctx, h.directory, tenantID, refs)
}

// Create handles POST /v1/workspaces.
func (h *WorkspaceHandler) Create(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principalOrFail(w, r)
	if !ok {
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
	var req createWorkspaceRequest
	if decodeJSON(body, &req) != nil {
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
	if req.DataPolicy != "" && req.DataPolicy != "Ephemeral" && req.DataPolicy != "Retain" {
		writeError(w, r, CodeInvalidRequest, "dataPolicy must be Ephemeral or Retain")
		return
	}
	if req.RetainedDataRef != "" {
		if !retainedRefPattern.MatchString(req.RetainedDataRef) {
			writeError(w, r, CodeInvalidRequest, "retainedDataRef must be an rd_ identifier")
			return
		}
		// An attached disk is retained data by definition; an explicit
		// Ephemeral policy contradicts the ref.
		if req.DataPolicy == "Ephemeral" {
			writeError(w, r, CodeInvalidRequest, "dataPolicy must be Retain when retainedDataRef is set")
			return
		}
	}
	tpl, err := h.catalog.Resolve(r.Context(), p.TenantID, req.TemplateRef)
	if err != nil && !errors.Is(err, ErrTemplateNotFound) {
		h.writeBackendError(w, r, err)
		return
	}
	if err != nil || tpl.ID == "" {
		writeError(w, r, CodeInvalidTemplate, "unknown template")
		return
	}
	// E3: a create resolving to an image older than -image-block-after is
	// refused; a missing or malformed image-built-at never blocks.
	if err := provisioning.CheckImageBlock(tpl.ImageBuiltAt, h.blockAfter, h.now(), tpl.Name, false); err != nil {
		writeImageStale(w, r, err)
		return
	}
	dataPolicy := req.DataPolicy
	if dataPolicy == "" {
		dataPolicy = tpl.DataPolicyDefault
	}
	tplInfo := provisioning.TemplateInfo{
		ID: tpl.ID, Name: tpl.Name, Revision: tpl.Revision,
		RevisionLabel: tpl.RevisionLabel,
		Runtime:       tpl.Runtime, Experience: tpl.Experience,
		ImageBuiltAt: tpl.ImageBuiltAt,
	}
	vector := provisioning.ResourceVector{
		RunningSlots: 1,
		CPUMillis:    tpl.CPUMillis,
		MemoryBytes:  tpl.MemoryMiB << 20,
		DiskBytes:    tpl.StorageGiB << 30,
	}
	sum := sha256.Sum256(body)
	var rec provisioning.WorkspaceRecord
	if req.RetainedDataRef != "" {
		// SEC-01: retainedDataRef shares the claimed attach path —
		// owner/tenant-admin scope, state Retained, runtime match and the
		// atomic Retained->Attaching claim all happen inside the store.
		rec, err = h.backend.AttachRetained(r.Context(), p.TenantID, p.Owner(), ownerScope(p),
			req.RetainedDataRef, key, provisioning.AttachRequest{
				Name:         req.Name,
				Template:     tplInfo,
				Vector:       vector,
				DesiredState: req.DesiredState,
			}, sum[:])
	} else {
		rec, err = h.backend.CreateWorkspace(r.Context(), p.TenantID, key, provisioning.CreateRequest{
			OwnerIssuer:  p.Issuer,
			OwnerSubject: p.Subject,
			Name:         req.Name,
			Template:     tplInfo,
			Vector:       vector,
			DesiredState: req.DesiredState,
			DataPolicy:   dataPolicy,
		}, sum[:])
	}
	if err != nil {
		h.writeBackendError(w, r, err)
		return
	}
	auditSetTarget(r.Context(), rec.ID)
	if req.RetainedDataRef != "" {
		auditSetDetail(r.Context(), "retained_data", req.RetainedDataRef)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	v := h.viewWithStatus(r.Context(), &rec)
	v.Owner = resolveOwner(r.Context(), h.directory, p.TenantID, rec.Owner)
	_ = json.NewEncoder(w).Encode(v)
}

// Get handles GET /v1/workspaces/{id}.
func (h *WorkspaceHandler) Get(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principalOrFail(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !workspaceIDPattern.MatchString(id) {
		writeError(w, r, CodeInvalidRequest, "bad workspace id")
		return
	}
	rec, err := h.backend.GetWorkspace(r.Context(), p.TenantID, ownerScope(p), id)
	if err != nil {
		h.writeBackendError(w, r, err)
		return
	}
	v := h.viewWithStatus(r.Context(), &rec)
	v.Owner = resolveOwner(r.Context(), h.directory, p.TenantID, rec.Owner)
	respondJSON(w, v)
}

// signal shares the start/stop/delete path.
func (h *WorkspaceHandler) signal(w http.ResponseWriter, r *http.Request, kind provisioning.IntentKind, keyRequired bool) {
	p, ok := h.principalOrFail(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !workspaceIDPattern.MatchString(id) {
		writeError(w, r, CodeInvalidRequest, "bad workspace id")
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if keyRequired && (len(key) < 8 || len(key) > 128) {
		writeError(w, r, CodeInvalidRequest, "Idempotency-Key header required (8..128 chars)")
		return
	}
	if key != "" && (len(key) < 8 || len(key) > 128) {
		writeError(w, r, CodeInvalidRequest, "Idempotency-Key must be 8..128 chars")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, h.maxBody))
	if err != nil {
		writeError(w, r, CodeInvalidRequest, "unreadable or oversized body")
		return
	}
	// Signal endpoints declare no request body; a non-empty one must still
	// be a single well-formed JSON document (SEC-I7).
	if len(bytes.TrimSpace(body)) > 0 {
		var v json.RawMessage
		if decodeJSON(body, &v) != nil {
			writeError(w, r, CodeInvalidRequest, "invalid request body")
			return
		}
	}
	sum := sha256.Sum256(body)
	rec, err := h.backend.SignalWorkspace(r.Context(), p.TenantID, p.Owner(), ownerScope(p), id, key, kind, sum[:])
	if err != nil {
		h.writeBackendError(w, r, err)
		return
	}
	status := http.StatusOK
	if kind == provisioning.IntentDelete {
		status = http.StatusAccepted
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	v := h.viewWithStatus(r.Context(), &rec)
	v.Owner = resolveOwner(r.Context(), h.directory, p.TenantID, rec.Owner)
	_ = json.NewEncoder(w).Encode(v)
}

// Start handles POST /v1/workspaces/{id}/start.
func (h *WorkspaceHandler) Start(w http.ResponseWriter, r *http.Request) {
	h.signal(w, r, provisioning.IntentStart, true)
}

// Stop handles POST /v1/workspaces/{id}/stop.
func (h *WorkspaceHandler) Stop(w http.ResponseWriter, r *http.Request) {
	h.signal(w, r, provisioning.IntentStop, false)
}

// Delete handles DELETE /v1/workspaces/{id}.
func (h *WorkspaceHandler) Delete(w http.ResponseWriter, r *http.Request) {
	h.signal(w, r, provisioning.IntentDelete, false)
}

// writeBackendError maps provisioning errors onto the stable error model.
func (h *WorkspaceHandler) writeBackendError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, provisioning.ErrWorkspaceNotFound):
		writeError(w, r, CodeNotFound, "workspace not found")
	case errors.Is(err, provisioning.ErrRetainedNotFound):
		writeError(w, r, CodeNotFound, "retained data record not found")
	case errors.Is(err, provisioning.ErrRetainedState):
		writeError(w, r, CodeInvalidState, err.Error())
	case errors.Is(err, provisioning.ErrRuntimeMismatch):
		writeError(w, r, CodeInvalidTemplate, "template runtime does not match the retained disk")
	case errors.Is(err, provisioning.ErrBadCursor):
		writeError(w, r, CodeInvalidRequest, "bad pageToken")
	case errors.Is(err, provisioning.ErrWorkspaceClosed),
		errors.Is(err, provisioning.ErrInvalidState):
		writeError(w, r, CodeInvalidState, err.Error())
	case errors.Is(err, provisioning.ErrNoQuota):
		writeError(w, r, CodeQuotaNotConfigured, quotaNotConfiguredMessage)
	case provisioning.IsQuotaExceeded(err):
		writeQuotaExceeded(w, r, err, h.retryAfterSeconds())
	case provisioning.IsUserLimit(err):
		writeUserLimitReached(w, r, err, h.retryAfterSeconds())
	case provisioning.IsImageStale(err):
		writeImageStale(w, r, err)
	case provisioning.IsIdempotencyConflict(err):
		writeError(w, r, CodeIdempotencyConflict, "idempotency key reused with a different request")
	case errors.Is(err, provisioning.ErrNameTaken):
		writeError(w, r, CodeInvalidState, "workspace name already in use")
	case errors.Is(err, provisioning.ErrReservationConflict):
		writeError(w, r, CodeInvalidState, err.Error())
	case store.IsTransient(err):
		// Postgres did not answer (outage/failover): retryable 503, not a
		// permanent 500 the portal would surface as a failure.
		writeError(w, r, CodeUnavailable, "service unavailable")
	default:
		writeError(w, r, CodeInternal, "internal error")
	}
}
