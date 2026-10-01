package provisioning_test

import (
	"context"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	workspacev1alpha1 "github.com/tinyorbitvn/tinycdi/api/v1alpha1"
	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
)

func newFakeK8sApplier(t *testing.T, objs ...client.Object) (client.Client, *provisioning.K8sApplier) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := workspacev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	applier := provisioning.NewK8sApplier(c, provisioning.TenantNamespaces{"tenant-a": "ns-a"})
	return c, applier
}

func getWorkspace(t *testing.T, c client.Client, ns, name string) *workspacev1alpha1.Workspace {
	t.Helper()
	ws := &workspacev1alpha1.Workspace{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, ws); err != nil {
		t.Fatalf("get workspace %s/%s: %v", ns, name, err)
	}
	return ws
}

func createIntent(uid provisioning.PlatformID, reqID string, rev uint64) provisioning.Intent {
	return provisioning.Intent{
		WorkspaceUID:      uid,
		TenantID:          "tenant-a",
		Revision:          rev,
		Kind:              provisioning.IntentCreate,
		RequestID:         reqID,
		DesiredState:      "Running",
		RuntimeGeneration: 1,
		Spec: provisioning.IntentSpec{
			WorkspaceName: "research-desktop",
			TemplateName:  "tmpl-linux",
			OwnerIssuer:   "https://issuer.test",
			OwnerSubject:  "user-1",
			DataPolicy:    "Ephemeral",
		},
	}
}

// TestK8sApplierCreateIdempotent: a replayed create with the same request
// ID on the same workspace converges to exactly one CR with the same spec.
func TestK8sApplierCreateIdempotent(t *testing.T) {
	c, applier := newFakeK8sApplier(t)
	ctx := context.Background()
	in := createIntent("ws_0123456789abcdef0123456789abcdef", "req-det-1", 1)

	if err := applier.Apply(ctx, in); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	if err := applier.Apply(ctx, in); err != nil {
		t.Fatalf("replay apply: %v", err)
	}
	ws := getWorkspace(t, c, "ns-a", provisioning.WorkspaceCRName(in.WorkspaceUID))
	if ws.Spec.IntentRevision != 1 || ws.Spec.RuntimeGeneration != 1 {
		t.Fatalf("spec = %+v, want intentRevision=1 runtimeGeneration=1", ws.Spec)
	}
	if ws.Spec.DesiredState != workspacev1alpha1.DesiredStateRunning {
		t.Fatalf("desiredState = %q", ws.Spec.DesiredState)
	}
	if ws.Spec.OwnerSubject.Subject != "user-1" || ws.Spec.TemplateRef.Name != "tmpl-linux" {
		t.Fatalf("spec fields wrong: %+v", ws.Spec)
	}

	// A second create intent with a DIFFERENT request ID but same name is a
	// conflict, not a silent overwrite.
	dup := in
	dup.RequestID = "req-other"
	dup.Revision = 9
	if err := applier.Apply(ctx, dup); err == nil {
		t.Fatalf("create with foreign request ID should conflict")
	}
}

// TestK8sApplierLifecycle: start/stop patch desiredState + fencing numbers
// monotonically; delete removes the CR; replays of older revisions do not
// roll the spec back.
func TestK8sApplierLifecycle(t *testing.T) {
	c, applier := newFakeK8sApplier(t)
	ctx := context.Background()
	uid := provisioning.PlatformID("ws_aaaabbbbccccdddd0000111122223333")
	if err := applier.Apply(ctx, createIntent(uid, "req-1", 1)); err != nil {
		t.Fatalf("create: %v", err)
	}
	name := provisioning.WorkspaceCRName(uid)

	if err := applier.Apply(ctx, provisioning.Intent{
		WorkspaceUID: uid, TenantID: "tenant-a", Revision: 2,
		Kind: provisioning.IntentStop, DesiredState: "Stopped", RuntimeGeneration: 1,
	}); err != nil {
		t.Fatalf("stop: %v", err)
	}
	ws := getWorkspace(t, c, "ns-a", name)
	if ws.Spec.DesiredState != workspacev1alpha1.DesiredStateStopped || ws.Spec.IntentRevision != 2 {
		t.Fatalf("after stop spec = %+v", ws.Spec)
	}

	// Delayed replay of rev-1 create must not flip desiredState back.
	stale := createIntent(uid, "req-1", 1)
	if err := applier.Apply(ctx, stale); err != nil {
		t.Fatalf("stale replay should be a no-op, got %v", err)
	}
	ws = getWorkspace(t, c, "ns-a", name)
	if ws.Spec.DesiredState != workspacev1alpha1.DesiredStateStopped || ws.Spec.IntentRevision != 2 {
		t.Fatalf("stale create rewound spec: %+v", ws.Spec)
	}

	if err := applier.Apply(ctx, provisioning.Intent{
		WorkspaceUID: uid, TenantID: "tenant-a", Revision: 3,
		Kind: provisioning.IntentStart, DesiredState: "Running", RuntimeGeneration: 2,
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	ws = getWorkspace(t, c, "ns-a", name)
	if ws.Spec.RuntimeGeneration != 2 || ws.Spec.IntentRevision != 3 {
		t.Fatalf("after start spec = %+v", ws.Spec)
	}

	if err := applier.Apply(ctx, provisioning.Intent{
		WorkspaceUID: uid, TenantID: "tenant-a", Revision: 4, Kind: provisioning.IntentDelete,
	}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := c.Get(ctx, client.ObjectKey{Namespace: "ns-a", Name: name}, &workspacev1alpha1.Workspace{}); !apierrors.IsNotFound(err) {
		t.Fatalf("CR still present after delete: %v", err)
	}
	// delete is idempotent
	if err := applier.Apply(ctx, provisioning.Intent{
		WorkspaceUID: uid, TenantID: "tenant-a", Revision: 4, Kind: provisioning.IntentDelete,
	}); err != nil {
		t.Fatalf("delete replay: %v", err)
	}
}

func TestK8sApplierUnknownTenant(t *testing.T) {
	_, applier := newFakeK8sApplier(t)
	in := createIntent("ws_0123456789abcdef0123456789abcdef", "req-1", 1)
	in.TenantID = "tenant-unknown"
	if err := applier.Apply(context.Background(), in); err == nil {
		t.Fatal("expected error for unmapped tenant")
	}
}

// catalogTmpl builds a minimal WorkspaceTemplate revision object as the
// chart publishes it: immutable spec, hash-suffixed name, catalog-name
// label.
func catalogTmpl(name, catalog, rev string, created time.Time) *workspacev1alpha1.WorkspaceTemplate {
	lbls := map[string]string{}
	if catalog != "" {
		lbls[provisioning.LabelCatalogName] = catalog
	}
	return &workspacev1alpha1.WorkspaceTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "ns-a", Labels: lbls,
			CreationTimestamp: metav1.NewTime(created),
		},
		Spec: workspacev1alpha1.WorkspaceTemplateSpec{
			Revision:   rev,
			Runtime:    workspacev1alpha1.RuntimeLinuxContainer,
			Experience: workspacev1alpha1.ExperienceDesktop,
		},
	}
}

// TestTemplateCatalogImmutableRevisions covers defect F1: seeded templates
// are published as "<name>-<hash8>" immutable objects tied together by the
// workspaces.cdi.tinyorbit.vn/catalog-name label. The stable public ID
// tpl_<name> resolves to the newest published revision and the listing
// shows one entry per catalog name — a superseded revision never shadows
// the current one.
func TestTemplateCatalogImmutableRevisions(t *testing.T) {
	t0 := time.Now().Add(-time.Hour)
	old := catalogTmpl("linuxdesk1-aaaa0000", "linuxdesk1", "w6a-1", t0)
	cur := catalogTmpl("linuxdesk1-bbbb1111", "linuxdesk1", "w6a-2", t0.Add(30*time.Minute))
	solo := catalogTmpl("admintpl", "", "1", t0)

	scheme := runtime.NewScheme()
	if err := workspacev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(old, cur, solo).Build()
	cat := provisioning.NewK8sTemplateCatalog(c, provisioning.TenantNamespaces{"tenant-a": "ns-a"})
	ctx := context.Background()

	// The stable ID resolves through revision churn to the newest object;
	// its public name is the catalog base, its public ID names the live
	// revision object.
	e, err := cat.Get(ctx, "tenant-a", "tpl_linuxdesk1")
	if err != nil || e == nil {
		t.Fatalf("get tpl_linuxdesk1: %v/%v", e, err)
	}
	if e.ID != "tpl_linuxdesk1-bbbb1111" || e.Name != "linuxdesk1" {
		t.Fatalf("tpl_linuxdesk1 -> %+v, want newest revision bbbb1111 named linuxdesk1", e)
	}
	// Exact revision names still resolve while the object exists.
	e, err = cat.Get(ctx, "tenant-a", "tpl_linuxdesk1-aaaa0000")
	if err != nil || e == nil || e.ID != "tpl_linuxdesk1-aaaa0000" {
		t.Fatalf("get exact revision: %v/%v", e, err)
	}
	// Unknown names stay unknown.
	if e, err = cat.Get(ctx, "tenant-a", "tpl_nonexistent"); err != nil || e != nil {
		t.Fatalf("unknown get: %v/%v, want nil", e, err)
	}

	// List shows one entry per catalog name — newest revision only.
	list, err := cat.List(ctx, "tenant-a", "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	got := map[string]string{}
	for _, e := range list {
		got[e.Name] = e.ID
	}
	if len(list) != 2 || got["linuxdesk1"] != "tpl_linuxdesk1-bbbb1111" || got["admintpl"] == "" {
		t.Fatalf("list = %v, want {linuxdesk1->tpl_linuxdesk1-bbbb1111, admintpl}", got)
	}
}
