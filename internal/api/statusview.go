package api

// API-side projection of Workspace CR status into WorkspaceView (design
// §3/§4/§6): Kubernetes is the source of observed state, PostgreSQL holds
// user-facing API state. The DB row's phase is written by intent handling
// only and can never become Ready on its own — observed status (phase +
// conditions incl. ConnectionReady) must be projected from the Workspace CR
// for the portal to ever see a connectable workspace.
//
// Freshness contract (observed state max 15 s old): while the informer has
// not synced or has gone quiet past maxObservedStale the view fails closed —
// it never claims Ready. It reports the last known non-Ready phase when one
// was observed (else the intent-side DB phase) plus a Degraded/Unknown
// StatusStale marker condition, and any True ConnectionReady is downgraded
// to Unknown. The projected view never carries runtime credentials, internal
// service refs, or fencing values (runtimeGeneration/runtimeUID) — those are
// dropped here, matching the WorkspaceView OpenAPI contract.

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"regexp"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	toolscache "k8s.io/client-go/tools/cache"
	crcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	workspacesv1alpha1 "github.com/tinyorbitvn/tinycdi/api/v1alpha1"
	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
)

// maxObservedStale is the freshness budget for observed Workspace status —
// the same 15 s bound the broker enforces on bindings (design §4). The
// informer cache is configured with a resync well below this so the event
// stream doubles as a liveness heartbeat.
const maxObservedStale = 15 * time.Second

// ReasonObservedStale marks conditions carried over from a stale observed
// snapshot: the informer can no longer prove they are current.
const ReasonObservedStale = "StatusStale"

// viewConditionTypes limits projected conditions to the OpenAPI
// ConditionType enum; anything else the CR carries stays internal.
var viewConditionTypes = map[string]bool{
	workspacesv1alpha1.ConditionAdmitted:        true,
	workspacesv1alpha1.ConditionStorageReady:    true,
	workspacesv1alpha1.ConditionRuntimeReady:    true,
	workspacesv1alpha1.ConditionConnectionReady: true,
	workspacesv1alpha1.ConditionDegraded:        true,
	workspacesv1alpha1.ConditionIntentBehind:    true,
}

// ObservedStatus is the projected, UI-safe slice of a Workspace CR's
// status. Fresh reports whether the informer that produced it is inside the
// freshness budget; when Fresh is false the other fields carry the last
// known snapshot (zero values when none was ever observed).
type ObservedStatus struct {
	Fresh         bool
	Found         bool
	Phase         string
	Conditions    []workspaceCondition
	FailureReason string
	ObservedAt    time.Time
	// ImageBuiltAt is the raw image-built-at annotation the Workspace CR
	// carries (copied from its template at create time); empty when absent.
	ImageBuiltAt string
}

// StatusView supplies observed Workspace CR status keyed by the platform
// workspace ID (the DB row id == workspaces.cdi.tinyorbit.vn/workspace-uid
// label). Implementations must never return runtime credentials or internal
// service refs.
type StatusView interface {
	WorkspaceStatus(ctx context.Context, tenantID, workspaceUID string) (ObservedStatus, error)
}

// K8sStatusView implements StatusView over a controller-runtime informer
// cache scoped to the managed tenant namespaces — in production the same
// cache the broker's BindingSource runs (in the backend). Like
// K8sBindingSource it
// treats the watch event stream as a heartbeat: the projection is Fresh only
// while the informer has synced and an event arrived within maxObservedStale.
//
// The last fresh projection per workspace is remembered so a stalled watch
// degrades to "last known state + StatusStale" instead of going blank.
type K8sStatusView struct {
	cache    crcache.Cache
	now      func() time.Time
	maxStale time.Duration

	mu        sync.Mutex
	synced    bool
	lastEvent time.Time
	lastKnown map[string]ObservedStatus // workspaceUID -> last fresh projection
}

// NewK8sStatusView registers a Workspace event handler on the cache's
// informer. The caller starts the cache and calls MarkSynced once
// WaitForCacheSync reports true.
func NewK8sStatusView(ctx context.Context, c crcache.Cache) (*K8sStatusView, error) {
	inf, err := c.GetInformer(ctx, &workspacesv1alpha1.Workspace{})
	if err != nil {
		return nil, err
	}
	v := &K8sStatusView{
		cache:     c,
		now:       time.Now,
		maxStale:  maxObservedStale,
		lastKnown: map[string]ObservedStatus{},
	}
	if _, err := inf.AddEventHandler(toolscache.ResourceEventHandlerFuncs{
		AddFunc:    func(interface{}) { v.note() },
		UpdateFunc: func(_, _ interface{}) { v.note() },
		DeleteFunc: func(interface{}) { v.note() },
	}); err != nil {
		return nil, err
	}
	return v, nil
}

// MarkSynced records that the informer cache completed its initial sync.
func (v *K8sStatusView) MarkSynced() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.synced = true
	v.lastEvent = v.now()
}

// note records a watch event: the informer is live and emitting.
func (v *K8sStatusView) note() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.synced = true
	v.lastEvent = v.now()
}

// WorkspaceStatus returns the observed status for the workspace UID. It
// never errors on staleness — a stale/unsynced informer yields the last
// known snapshot with Fresh=false. A cache read failure or an ambiguous
// (multi-CR) match fails closed the same way.
func (v *K8sStatusView) WorkspaceStatus(ctx context.Context, tenantID, workspaceUID string) (ObservedStatus, error) {
	v.mu.Lock()
	synced, last := v.synced, v.lastEvent
	v.mu.Unlock()
	if !synced || v.now().Sub(last) > v.maxStale {
		return v.snapshot(workspaceUID, last), nil
	}
	var list workspacesv1alpha1.WorkspaceList
	if err := v.cache.List(ctx, &list, client.MatchingLabels{
		provisioning.LabelWorkspaceUID: workspaceUID,
		provisioning.LabelTenant:       tenantID,
	}); err != nil || len(list.Items) > 1 {
		return v.snapshot(workspaceUID, last), nil
	}
	obs := ObservedStatus{Fresh: true, ObservedAt: last}
	if len(list.Items) == 0 {
		v.forget(workspaceUID)
		return obs, nil
	}
	ws := &list.Items[0]
	obs.Found = true
	obs.Phase = string(ws.Status.Phase)
	obs.Conditions = projectConditions(ws.Status.Conditions, ws.Annotations)
	obs.FailureReason = failureReasonOf(ws.Status.Conditions)
	obs.ImageBuiltAt = ws.Annotations[provisioning.AnnotationWorkspaceImageBuiltAt]
	if obs.Phase != "" {
		v.remember(workspaceUID, obs)
	}
	return obs, nil
}

// snapshot returns the last fresh projection for uid with Fresh=false, or a
// bare stale marker when nothing was ever observed.
func (v *K8sStatusView) snapshot(workspaceUID string, at time.Time) ObservedStatus {
	v.mu.Lock()
	defer v.mu.Unlock()
	obs := ObservedStatus{ObservedAt: at}
	if s, ok := v.lastKnown[workspaceUID]; ok {
		obs.Found = s.Found
		obs.Phase = s.Phase
		obs.Conditions = s.Conditions
		obs.FailureReason = s.FailureReason
		obs.ImageBuiltAt = s.ImageBuiltAt
	}
	return obs
}

func (v *K8sStatusView) remember(workspaceUID string, obs ObservedStatus) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.lastKnown[workspaceUID] = ObservedStatus{
		Found:         obs.Found,
		Phase:         obs.Phase,
		Conditions:    obs.Conditions,
		FailureReason: obs.FailureReason,
		ImageBuiltAt:  obs.ImageBuiltAt,
	}
}

func (v *K8sStatusView) forget(workspaceUID string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	delete(v.lastKnown, workspaceUID)
}

// projectConditions maps CR status conditions onto the public shape,
// restricted to the OpenAPI ConditionType enum. annotations is the CR's
// annotation set: the operator's condition-params annotation supplies the
// structured params of the one entry that matches each condition's type
// and reason.
func projectConditions(conds []metav1.Condition, annotations map[string]string) []workspaceCondition {
	params := conditionParamsOf(annotations)
	out := make([]workspaceCondition, 0, len(conds))
	for _, c := range conds {
		if !viewConditionTypes[c.Type] {
			continue
		}
		status := string(c.Status)
		if status != "True" && status != "False" {
			status = "Unknown"
		}
		reason := c.Reason
		if reason == "" {
			reason = "Unknown"
		}
		out = append(out, workspaceCondition{
			Type:               c.Type,
			Status:             status,
			Reason:             reason,
			Message:            c.Message,
			Params:             params[c.Type+"."+reason],
			LastTransitionTime: c.LastTransitionTime.Time,
		})
	}
	return out
}

// conditionParamName bounds the param keys the API forwards; values are
// capped at conditionParamValueMax runes and entries at
// conditionParamMax keys so an oversized annotation cannot bloat the view.
var conditionParamName = regexp.MustCompile(`^[a-z][a-zA-Z0-9]{0,63}$`)

const (
	conditionParamMax      = 16
	conditionParamValueMax = 256
)

// conditionParamsOf parses the operator's condition-params annotation into
// a sanitized "<type>.<reason>" -> params table. Anything malformed —
// corrupt JSON, over-long values, non-token param names — is dropped
// rather than forwarded.
func conditionParamsOf(annotations map[string]string) map[string]map[string]string {
	raw := annotations[provisioning.AnnotationConditionParams]
	if raw == "" {
		return nil
	}
	var m map[string]map[string]string
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil
	}
	out := make(map[string]map[string]string, len(m))
	for key, params := range m {
		clean := make(map[string]string, len(params))
		for k, v := range params {
			if len(clean) >= conditionParamMax {
				break
			}
			if !conditionParamName.MatchString(k) {
				continue
			}
			runes := []rune(v)
			if len(runes) > conditionParamValueMax {
				v = string(runes[:conditionParamValueMax])
			}
			clean[k] = v
		}
		if len(clean) > 0 {
			out[key] = clean
		}
	}
	return out
}

// failureReasonOf extracts the machine-readable cause for a Failed phase:
// the reason the operator stamped on Degraded=True (e.g.
// BootDeadlineExceeded).
func failureReasonOf(conds []metav1.Condition) string {
	for _, c := range conds {
		if c.Type == workspacesv1alpha1.ConditionDegraded &&
			c.Status == metav1.ConditionTrue && c.Reason != "" {
			return c.Reason
		}
	}
	return ""
}

// staleConditions degrades a last-known condition set for a stale view:
// ConnectionReady can never report True on stale data (the portal must not
// offer Connect), and Degraded=Unknown/StatusStale carries the staleness
// indication — both inside the OpenAPI enums.
func staleConditions(obs ObservedStatus) []workspaceCondition {
	out := make([]workspaceCondition, 0, len(obs.Conditions)+1)
	degraded := false
	for _, c := range obs.Conditions {
		switch {
		case c.Type == workspacesv1alpha1.ConditionConnectionReady && c.Status == "True":
			c.Status = "Unknown"
			c.Reason = ReasonObservedStale
			c.Message = "observed status is stale; connectivity is unverified"
			c.Params = nil
		case c.Type == workspacesv1alpha1.ConditionDegraded:
			degraded = true
			c.Status = "Unknown"
			c.Reason = ReasonObservedStale
			c.Message = "observed status is stale; showing last known state"
			c.Params = nil
		}
		out = append(out, c)
	}
	if !degraded {
		out = append(out, workspaceCondition{
			Type:               workspacesv1alpha1.ConditionDegraded,
			Status:             "Unknown",
			Reason:             ReasonObservedStale,
			Message:            "observed status is stale; showing last known state",
			LastTransitionTime: obs.ObservedAt,
		})
	}
	return out
}

// mergeObservedStatus folds an ObservedStatus into the DB-derived view.
// rec is the source row (its Phase/FailureReason are the intent-side
// fallback). The projection rules:
//   - a Terminating tombstone in the DB always stands: the operator's
//     finalizer never rewrites status.phase, so the CR keeps its last phase
//     for the whole teardown and must not override the delete (conditions
//     are still projected);
//   - fresh CR with a phase: CR phase + conditions win outright;
//   - fresh but missing/unreconciled CR: the intent-side DB phase stands;
//   - stale informer: never Ready — last known non-Ready phase if any,
//     else the DB phase, plus StatusStale-marked conditions.
func mergeObservedStatus(v *WorkspaceView, rec *provisioning.WorkspaceRecord, obs ObservedStatus) {
	switch {
	case obs.Fresh && obs.Found && obs.Phase != "":
		v.Phase = obs.Phase
		v.Conditions = obs.Conditions
	case obs.Fresh:
		// CR absent or the operator has not reconciled it yet: the DB
		// row's intent-side phase is the only truth.
	default:
		if obs.Phase != "" && obs.Phase != string(workspacesv1alpha1.WorkspacePhaseReady) {
			v.Phase = obs.Phase
		}
		v.Conditions = staleConditions(obs)
	}
	if rec.Phase == string(workspacesv1alpha1.WorkspacePhaseTerminating) {
		v.Phase = rec.Phase
	}
	v.FailureReason = ""
	if v.Phase == string(workspacesv1alpha1.WorkspacePhaseFailed) {
		if obs.FailureReason != "" {
			v.FailureReason = obs.FailureReason
		} else {
			v.FailureReason = rec.FailureReason
		}
	}
}

// WithStatusView attaches the observed-status projection to the handler.
// Wiring is optional so unit tests can run without Kubernetes — but an
// unwired handler degrades every view to the stale path (never Ready).
func (h *WorkspaceHandler) WithStatusView(v StatusView) *WorkspaceHandler {
	h.statusView = v
	return h
}

// viewWithStatus builds the public view: DB row fields merged with the
// observed CR status. A nil/failed StatusView degrades to stale semantics —
// the API never claims Ready on unverifiable state.
func (h *WorkspaceHandler) viewWithStatus(ctx context.Context, rec *provisioning.WorkspaceRecord) WorkspaceView {
	v := recordToView(rec)
	var obs ObservedStatus
	if h.statusView != nil {
		var err error
		obs, err = h.statusView.WorkspaceStatus(ctx, rec.TenantID, rec.ID)
		if err != nil {
			obs = ObservedStatus{}
		}
	}
	if obs.ObservedAt.IsZero() {
		obs.ObservedAt = h.now()
	}
	mergeObservedStatus(&v, rec, obs)
	h.mergeImageFreshness(ctx, &v, rec, obs)
	v.UpdateAvailable = h.updateAvailable(ctx, rec)
	return v
}

// mergeImageFreshness fills the optional imageBuiltAt/imageStale view
// fields from the image age that travels with the workspace: the annotation
// on its Workspace CR (informer cache), else the template snapshot taken at
// create time. A workspace created before either existed (every v0.1
// workspace at upgrade time) falls back to its template's age, looked up at
// most once per distinct template per request (imageAgeMemo) and served from
// the informer cache in production (WithImageCatalog). A missing or malformed
// age leaves both fields absent; freshness is advisory and never fails the
// request (D28).
func (h *WorkspaceHandler) mergeImageFreshness(ctx context.Context, v *WorkspaceView, rec *provisioning.WorkspaceRecord, obs ObservedStatus) {
	raw := obs.ImageBuiltAt
	if raw == "" {
		raw = rec.Template.ImageBuiltAt
	}
	if raw == "" {
		raw = h.templateImageAge(ctx, rec.TenantID, rec.Template.ID)
	}
	v.ImageBuiltAt, v.ImageStale = imageFreshness(h.log, raw, h.staleAfter, h.now(),
		"workspace", rec.ID, "template", rec.Template.ID)
}

// ---------------------------------------------------------------------------
// Template family / updateAvailable
// ---------------------------------------------------------------------------

// updateCheckMemo remembers the family-newest and current-revision lookups
// resolved during one request — including failures — so a page of
// workspaces costs one catalog read per distinct family and revision.
type updateCheckMemo struct {
	mu     sync.Mutex
	newest map[string]*TemplateEntry // tenantID/family -> newest revision (nil: none/error)
	cur    map[string]curRevision    // tenantID/templateID -> recorded revision
}

// curRevision is the memoized lookup of a workspace's recorded revision:
// entry==nil && !failed means the revision object is gone (a start adopts
// the newest); failed means the catalog read errored and no update is
// reported.
type curRevision struct {
	entry  *TemplateEntry
	failed bool
}

type updateCheckMemoKey struct{}

// withUpdateCheckMemo scopes an update-check memo to ctx (one request).
func withUpdateCheckMemo(ctx context.Context) context.Context {
	return context.WithValue(ctx, updateCheckMemoKey{}, &updateCheckMemo{
		newest: map[string]*TemplateEntry{},
		cur:    map[string]curRevision{},
	})
}

func (h *WorkspaceHandler) updateCheckCtx(ctx context.Context) *updateCheckMemo {
	memo, _ := ctx.Value(updateCheckMemoKey{}).(*updateCheckMemo)
	if memo == nil {
		memo = &updateCheckMemo{newest: map[string]*TemplateEntry{}, cur: map[string]curRevision{}}
	}
	return memo
}

// familyNewest resolves the newest published revision of a family once per
// request; nil means the family has no resolvable revision or the catalog
// read failed (no update is reported for either).
func (h *WorkspaceHandler) familyNewest(ctx context.Context, fc FamilyCatalog, tenantID, family string) *TemplateEntry {
	memo := h.updateCheckCtx(ctx)
	key := tenantID + "/" + family
	memo.mu.Lock()
	defer memo.mu.Unlock()
	if e, ok := memo.newest[key]; ok {
		return e
	}
	var entry *TemplateEntry
	e, err := fc.NewestInFamily(ctx, tenantID, family)
	if err == nil && e.ID != "" {
		entry = &e
	}
	memo.newest[key] = entry
	return entry
}

// recordedRevision resolves the workspace's recorded template revision once
// per request. ErrTemplateNotFound means the object was deleted (entry nil,
// failed false); any other error reports failed so no update is claimed on
// unverifiable state.
func (h *WorkspaceHandler) recordedRevision(ctx context.Context, cat TemplateCatalog, tenantID, templateID string) curRevision {
	memo := h.updateCheckCtx(ctx)
	key := tenantID + "/" + templateID
	memo.mu.Lock()
	defer memo.mu.Unlock()
	if r, ok := memo.cur[key]; ok {
		return r
	}
	var r curRevision
	e, err := cat.Resolve(ctx, tenantID, templateID)
	switch {
	case err == nil:
		r.entry = &e
	case errors.Is(err, ErrTemplateNotFound):
		// deleted revision object: a start adopts the newest (OnStart
		// default) — entry stays nil.
	default:
		r.failed = true
	}
	memo.cur[key] = r
	return r
}

// updateAvailable reports whether a Stopped -> Running start would move the
// workspace to a newer published revision of its template family: the
// family must resolve to a revision newer than the recorded one, the
// recorded revision must not pin the image, and the newest revision must
// pass the compatibility guard (same runtime/experience/data policy,
// storage not smaller; E1/E2). A recorded revision whose object was
// deleted counts as updatable — the start adopts the newest. Lookups are
// memoized per request and read through the informer-backed catalog in
// production (WithImageCatalog); they never fail the request.
func (h *WorkspaceHandler) updateAvailable(ctx context.Context, rec *provisioning.WorkspaceRecord) bool {
	cat := h.imageCatalog
	if cat == nil {
		cat = h.catalog
	}
	fc, ok := cat.(FamilyCatalog)
	if !ok || rec.Template.Name == "" || rec.Template.ID == "" {
		return false
	}
	newest := h.familyNewest(ctx, fc, rec.TenantID, rec.Template.Name)
	if newest == nil || newest.ID == rec.Template.ID {
		return false
	}
	cur := h.recordedRevision(ctx, cat, rec.TenantID, rec.Template.ID)
	if cur.failed {
		return false
	}
	if cur.entry != nil {
		if cur.entry.ImageUpdate == "Pinned" {
			return false
		}
		if cur.entry.Runtime != newest.Runtime ||
			cur.entry.Experience != newest.Experience ||
			cur.entry.DataPolicyDefault != newest.DataPolicyDefault ||
			newest.StorageBytes < cur.entry.StorageBytes {
			return false
		}
	}
	return true
}

// imageAgeMemo remembers the template image ages resolved during one
// request, including lookups that failed, so a page of workspaces costs one
// lookup per distinct template.
type imageAgeMemo struct {
	mu   sync.Mutex
	ages map[string]string
}

type imageAgeMemoKey struct{}

// withImageAgeMemo scopes a template-age memo to ctx (one request).
func withImageAgeMemo(ctx context.Context) context.Context {
	return context.WithValue(ctx, imageAgeMemoKey{}, &imageAgeMemo{ages: map[string]string{}})
}

// WithImageCatalog sets the catalog the image-age fallback resolves
// templates through; production passes an informer-cache-backed one so a
// list never costs API-server GETs. Nil keeps the handler's own catalog.
func (h *WorkspaceHandler) WithImageCatalog(c TemplateCatalog) *WorkspaceHandler {
	h.imageCatalog = c
	return h
}

// templateImageAge returns the raw image-built-at value of the template, or
// "" when the template is unknown, unreadable or carries none.
func (h *WorkspaceHandler) templateImageAge(ctx context.Context, tenantID, templateID string) string {
	cat := h.imageCatalog
	if cat == nil {
		cat = h.catalog
	}
	if cat == nil || templateID == "" {
		return ""
	}
	memo, _ := ctx.Value(imageAgeMemoKey{}).(*imageAgeMemo)
	if memo == nil {
		memo = &imageAgeMemo{ages: map[string]string{}}
	}
	key := tenantID + "/" + templateID
	memo.mu.Lock()
	defer memo.mu.Unlock()
	if raw, ok := memo.ages[key]; ok {
		return raw
	}
	raw := ""
	e, err := cat.Resolve(ctx, tenantID, templateID)
	switch {
	case err == nil:
		raw = e.ImageBuiltAt
	case !errors.Is(err, ErrTemplateNotFound):
		log := h.log
		if log == nil {
			log = slog.Default()
		}
		log.Warn("image age fallback: template lookup failed", "template", templateID, "error", err)
	}
	memo.ages[key] = raw
	return raw
}
