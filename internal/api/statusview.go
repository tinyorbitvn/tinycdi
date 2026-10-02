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
	"errors"
	"log/slog"
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
// cache the broker's BindingSource runs (cmd/api). Like K8sBindingSource it
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
	obs.Conditions = projectConditions(ws.Status.Conditions)
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
// restricted to the OpenAPI ConditionType enum.
func projectConditions(conds []metav1.Condition) []workspaceCondition {
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
			LastTransitionTime: c.LastTransitionTime.Time,
		})
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
		case c.Type == workspacesv1alpha1.ConditionDegraded:
			degraded = true
			c.Status = "Unknown"
			c.Reason = ReasonObservedStale
			c.Message = "observed status is stale; showing last known state"
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
