package provisioning

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	workspacev1alpha1 "github.com/tinyorbitvn/tinycdi/api/v1alpha1"
)

// TenantNamespaces maps a verified tenant ID to the Kubernetes namespace
// that holds its Workspace CRs. Unknown tenants are absent from the map.
type TenantNamespaces map[string]string

// Namespace resolves the mapping; ok=false for unmapped tenants.
func (m TenantNamespaces) Namespace(tenantID string) (ns string, ok bool) {
	ns, ok = m[tenantID]
	return
}

// Label/annotation names stamped on every Workspace CR the API creates.
const (
	LabelWorkspaceUID   = "workspaces.cdi.tinyorbit.vn/workspace-uid"
	LabelTenant         = "workspaces.cdi.tinyorbit.vn/tenant"
	AnnotationRequestID = "workspaces.cdi.tinyorbit.vn/request-id"
)

// PlatformID is the platform workspace identity the API mints
// ("ws_<hex>", stored as workspaces.id). It is stamped into the
// workspace-uid label ON THE Workspace CR and is what every broker/DB seam
// (tickets, leases, revocations, stop intents) routes on. It is a
// DIFFERENT identity from the Workspace CR's metadata.uid — the runtime
// backend names child objects from the CR UID and stamps the same label
// key with it on those children (see linux.CRUID). The two identities
// never convert implicitly.
type PlatformID string

// ErrCRConflict is returned when a Workspace CR with the deterministic
// name exists but belongs to a different request (name reuse across
// workspaces must never silently adopt foreign resources).
var ErrCRConflict = errors.New("workspace CR exists with different request ID")

// WorkspaceCRName is the deterministic, DNS-1123-safe CR name for a
// platform workspace ID (ws_<hex> -> ws-<hex>).
func WorkspaceCRName(workspaceUID PlatformID) string {
	return strings.ToLower(strings.ReplaceAll(string(workspaceUID), "_", "-"))
}

// K8sApplier is the production WorkspaceApplier: it projects intents into
// Workspace CRs via a controller-runtime client. End users never hold
// credentials that can write CRs; this runs under the API/operator service
// account.
type K8sApplier struct {
	client  client.Client
	tenants TenantNamespaces
}

// NewK8sApplier builds an applier; tenants maps tenantID -> namespace.
func NewK8sApplier(c client.Client, tenants TenantNamespaces) *K8sApplier {
	return &K8sApplier{client: c, tenants: tenants}
}

// Apply implements WorkspaceApplier. All operations are idempotent:
// create replays succeed when the existing CR carries the same request ID
// and fencing numbers; start/stop never move spec.intentRevision
// backwards; delete tolerates NotFound.
func (a *K8sApplier) Apply(ctx context.Context, in Intent) error {
	ns, ok := a.tenants.Namespace(in.TenantID)
	if !ok {
		return fmt.Errorf("apply %s: no namespace for tenant %q", in.Kind, in.TenantID)
	}
	name := WorkspaceCRName(in.WorkspaceUID)
	key := client.ObjectKey{Namespace: ns, Name: name}

	switch in.Kind {
	case IntentCreate:
		return a.applyCreate(ctx, key, in)
	case IntentStart, IntentStop:
		return a.applySignal(ctx, key, in)
	case IntentDelete:
		return a.applyDelete(ctx, key, in)
	}
	return fmt.Errorf("apply: unknown kind %q", in.Kind)
}

func (a *K8sApplier) applyCreate(ctx context.Context, key client.ObjectKey, in Intent) error {
	ws := &workspacev1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{
			Name:      key.Name,
			Namespace: key.Namespace,
			Labels: map[string]string{
				LabelWorkspaceUID: string(in.WorkspaceUID),
				LabelTenant:       in.TenantID,
			},
			Annotations: map[string]string{
				AnnotationRequestID: in.RequestID,
			},
		},
		Spec: workspacev1alpha1.WorkspaceSpec{
			TemplateRef: workspacev1alpha1.TemplateReference{Name: in.Spec.TemplateName},
			OwnerSubject: workspacev1alpha1.OwnerSubject{
				Issuer:  in.Spec.OwnerIssuer,
				Subject: in.Spec.OwnerSubject,
			},
			DesiredState:      workspacev1alpha1.DesiredState(in.DesiredState),
			DataPolicy:        workspacev1alpha1.DataPolicy(in.Spec.DataPolicy),
			RuntimeGeneration: in.RuntimeGeneration,
			IntentRevision:    int64(in.Revision),
		},
	}
	err := a.client.Create(ctx, ws)
	switch {
	case err == nil:
		return nil
	case apierrors.IsAlreadyExists(err):
		// Replay: converge only if this is the same request.
		var existing workspacev1alpha1.Workspace
		if err := a.client.Get(ctx, key, &existing); err != nil {
			return err
		}
		if existing.Labels[LabelWorkspaceUID] != string(in.WorkspaceUID) ||
			existing.Annotations[AnnotationRequestID] != in.RequestID {
			return ErrCRConflict
		}
		return a.patchForward(ctx, &existing, in)
	default:
		return fmt.Errorf("apply create %s: %w", key, err)
	}
}

// applySignal forwards desiredState/runtimeGeneration/intentRevision for
// start and stop, never moving intentRevision backwards.
func (a *K8sApplier) applySignal(ctx context.Context, key client.ObjectKey, in Intent) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var ws workspacev1alpha1.Workspace
		err := a.client.Get(ctx, key, &ws)
		if apierrors.IsNotFound(err) {
			// Nothing to signal (e.g. delete raced ahead and closed the
			// stream): applying is a no-op.
			return nil
		}
		if err != nil {
			return err
		}
		if int64(in.Revision) <= ws.Spec.IntentRevision {
			return nil // stale or already applied
		}
		ws.Spec.DesiredState = workspacev1alpha1.DesiredState(in.DesiredState)
		if in.RuntimeGeneration > ws.Spec.RuntimeGeneration {
			ws.Spec.RuntimeGeneration = in.RuntimeGeneration
		}
		ws.Spec.IntentRevision = int64(in.Revision)
		return a.client.Update(ctx, &ws)
	})
}

func (a *K8sApplier) applyDelete(ctx context.Context, key client.ObjectKey, in Intent) error {
	var ws workspacev1alpha1.Workspace
	err := a.client.Get(ctx, key, &ws)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if ws.Labels[LabelWorkspaceUID] != string(in.WorkspaceUID) {
		return ErrCRConflict
	}
	if err := a.client.Delete(ctx, &ws); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("apply delete %s: %w", key, err)
	}
	return nil
}

// patchForward merges an intent into an existing CR during create replay:
// when the CR is behind (e.g. create applied but not fully persisted, or a
// retry after partial failure), move the fencing numbers forward. A CR
// already at or beyond the intent revision is left untouched.
func (a *K8sApplier) patchForward(ctx context.Context, ws *workspacev1alpha1.Workspace, in Intent) error {
	if int64(in.Revision) <= ws.Spec.IntentRevision {
		return nil
	}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := a.client.Get(ctx, client.ObjectKeyFromObject(ws), ws); err != nil {
			return err
		}
		if int64(in.Revision) <= ws.Spec.IntentRevision {
			return nil
		}
		ws.Spec.DesiredState = workspacev1alpha1.DesiredState(in.DesiredState)
		if in.RuntimeGeneration > ws.Spec.RuntimeGeneration {
			ws.Spec.RuntimeGeneration = in.RuntimeGeneration
		}
		ws.Spec.IntentRevision = int64(in.Revision)
		return a.client.Update(ctx, ws)
	})
}

// ---------------------------------------------------------------------------
// Template catalog backed by WorkspaceTemplate CRs (read-only).
// ---------------------------------------------------------------------------

// TemplateCatalogEntry is a published template revision offered to users.
type TemplateCatalogEntry struct {
	ID                string // public ID: "tpl_" + CR name
	Name              string // CR name
	Description       string
	Revision          int64
	Runtime           string
	Experience        string
	CPUMillis         int64
	MemoryBytes       int64
	DiskBytes         int64
	IdleTimeout       time.Duration
	DisconnectGrace   time.Duration
	MaxRunning        time.Duration
	DataPolicyDefault string
	ClipboardPolicy   string
	NetworkProfile    string
	PublishedAt       time.Time
}

// K8sTemplateCatalog resolves/list templates from WorkspaceTemplate CRs in
// the tenant's namespace. Public IDs are "tpl_" + CR name.
type K8sTemplateCatalog struct {
	client  client.Client
	tenants TenantNamespaces
}

// NewK8sTemplateCatalog builds the catalog reader.
func NewK8sTemplateCatalog(c client.Client, tenants TenantNamespaces) *K8sTemplateCatalog {
	return &K8sTemplateCatalog{client: c, tenants: tenants}
}

// LabelCatalogName ties the immutable revision objects of one logical
// template together. Seeded templates are published as
// "<name>-<hash8>" objects (spec is CEL-immutable), so the catalog
// resolves the stable public id tpl_<name> to the newest published
// revision and lists one entry per catalog name.
const LabelCatalogName = "workspaces.cdi.tinyorbit.vn/catalog-name"

// TemplateCRName converts a public template ID to its CR name; ok=false
// when id lacks the tpl_ prefix.
func TemplateCRName(id string) (name string, ok bool) {
	if !strings.HasPrefix(id, "tpl_") {
		return "", false
	}
	return id[len("tpl_"):], true
}

// catalogName returns the logical template name: the catalog-name label
// when published as an immutable revision, otherwise the object name
// itself (admin-published singletons).
func catalogName(t *workspacev1alpha1.WorkspaceTemplate) string {
	if n := t.Labels[LabelCatalogName]; n != "" {
		return n
	}
	return t.Name
}

// latestRevision picks the newest revision: latest creationTimestamp,
// tie-broken by name for determinism.
func latestRevision(items []workspacev1alpha1.WorkspaceTemplate) *workspacev1alpha1.WorkspaceTemplate {
	var best *workspacev1alpha1.WorkspaceTemplate
	for i := range items {
		t := &items[i]
		if best == nil ||
			t.CreationTimestamp.After(best.CreationTimestamp.Time) ||
			(t.CreationTimestamp.Equal(&best.CreationTimestamp) && t.Name > best.Name) {
			best = t
		}
	}
	return best
}

// Get returns the template for the public ID, or nil when unknown. An ID
// naming a catalog base (tpl_<name>) resolves to the newest published
// revision of that name — including after an upgrade swapped the
// immutable revision object.
func (c *K8sTemplateCatalog) Get(ctx context.Context, tenantID, id string) (*TemplateCatalogEntry, error) {
	ns, ok := c.tenants.Namespace(tenantID)
	if !ok {
		return nil, fmt.Errorf("no namespace for tenant %q", tenantID)
	}
	name, ok := TemplateCRName(id)
	if !ok {
		return nil, nil
	}
	var tpl workspacev1alpha1.WorkspaceTemplate
	err := c.client.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &tpl)
	switch {
	case err == nil:
		e := templateEntry(&tpl)
		return &e, nil
	case !apierrors.IsNotFound(err):
		return nil, err
	}
	// Not a live object name: try the catalog-name label so tpl_<name>
	// resolves through revision churn.
	var list workspacev1alpha1.WorkspaceTemplateList
	if err := c.client.List(ctx, &list, client.InNamespace(ns),
		client.MatchingLabels{LabelCatalogName: name}); err != nil {
		return nil, err
	}
	latest := latestRevision(list.Items)
	if latest == nil {
		return nil, nil
	}
	e := templateEntry(latest)
	return &e, nil
}

// List returns the catalog the tenant sees: one entry per logical
// template — the newest published revision when several immutable
// revision objects share a catalog name.
func (c *K8sTemplateCatalog) List(ctx context.Context, tenantID, runtimeFilter string) ([]TemplateCatalogEntry, error) {
	ns, ok := c.tenants.Namespace(tenantID)
	if !ok {
		return nil, fmt.Errorf("no namespace for tenant %q", tenantID)
	}
	var list workspacev1alpha1.WorkspaceTemplateList
	if err := c.client.List(ctx, &list, client.InNamespace(ns)); err != nil {
		return nil, err
	}
	byCatalog := map[string][]workspacev1alpha1.WorkspaceTemplate{}
	order := []string{}
	for i := range list.Items {
		t := &list.Items[i]
		cn := catalogName(t)
		if _, seen := byCatalog[cn]; !seen {
			order = append(order, cn)
		}
		byCatalog[cn] = append(byCatalog[cn], *t)
	}
	var out []TemplateCatalogEntry
	for _, cn := range order {
		e := templateEntry(latestRevision(byCatalog[cn]))
		if runtimeFilter != "" && e.Runtime != runtimeFilter {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

func templateEntry(t *workspacev1alpha1.WorkspaceTemplate) TemplateCatalogEntry {
	var rev int64
	fmt.Sscanf(t.Spec.Revision, "%d", &rev)
	e := TemplateCatalogEntry{
		ID:          "tpl_" + t.Name,
		Name:        catalogName(t),
		Revision:    rev,
		Runtime:     string(t.Spec.Runtime),
		Experience:  string(t.Spec.Experience),
		PublishedAt: t.CreationTimestamp.Time,
	}
	e.CPUMillis = t.Spec.Resources.CPU.MilliValue()
	e.MemoryBytes = t.Spec.Resources.Memory.Value()
	e.DiskBytes = t.Spec.Resources.Storage.Value()
	e.IdleTimeout = t.Spec.Lifecycle.IdleTimeout.Duration
	e.DisconnectGrace = t.Spec.Lifecycle.DisconnectTimeout.Duration
	e.MaxRunning = t.Spec.Lifecycle.MaxDuration.Duration
	e.DataPolicyDefault = string(t.Spec.Lifecycle.DataPolicy)
	e.ClipboardPolicy = string(t.Spec.ClipboardPolicy)
	e.NetworkProfile = string(t.Spec.NetworkProfile)
	return e
}

// Vector converts catalog resources to the quota reservation vector.
func (e TemplateCatalogEntry) Vector() ResourceVector {
	return ResourceVector{
		RunningSlots: 1,
		CPUMillis:    e.CPUMillis,
		MemoryBytes:  e.MemoryBytes,
		DiskBytes:    e.DiskBytes,
	}
}
