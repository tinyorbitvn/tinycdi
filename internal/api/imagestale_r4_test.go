// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package api

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	crcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	workspacesv1alpha1 "github.com/tinyorbitvn/tinycdi/api/v1alpha1"
	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
)

// countingCatalog counts Resolve/List calls and answers every Resolve with
// ErrTemplateNotFound — the state after helm deleted a superseded revision.
type countingCatalog struct {
	staleCatalog
	resolves atomic.Int64
}

func (c *countingCatalog) Resolve(ctx context.Context, tenantID, id string) (TemplateEntry, error) {
	c.resolves.Add(1)
	return c.staleCatalog.Resolve(ctx, tenantID, id)
}

// listCache is the one controller-runtime cache method K8sStatusView reads.
type listCache struct {
	crcache.Cache
	items []workspacesv1alpha1.Workspace
}

func (c listCache) List(_ context.Context, list client.ObjectList, _ ...client.ListOption) error {
	list.(*workspacesv1alpha1.WorkspaceList).Items = c.items
	return nil
}

// TestImageStale_SurvivesSupersededRevision: the template object behind the
// workspace is gone (superseded revision deleted by helm), yet the view still
// carries imageBuiltAt/imageStale because the age travelled with the
// workspace CR as an annotation read from the informer cache.
func TestImageStale_SurvivesSupersededRevision(t *testing.T) {
	now := time.Now().UTC()
	builtAt := now.Add(-20 * 24 * time.Hour).Format(time.RFC3339)
	cr := workspacesv1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "w", Namespace: "ns-a",
			Annotations: map[string]string{provisioning.AnnotationWorkspaceImageBuiltAt: builtAt},
		},
		Status: workspacesv1alpha1.WorkspaceStatus{Phase: workspacesv1alpha1.WorkspacePhaseReady},
	}
	sv := &K8sStatusView{
		cache: listCache{items: []workspacesv1alpha1.Workspace{cr}},
		now:   time.Now, maxStale: time.Minute,
		lastKnown: map[string]ObservedStatus{},
	}
	sv.MarkSynced()

	cat := &countingCatalog{staleCatalog: staleCatalog{entries: map[string]TemplateEntry{}}} // template gone
	be := newFakeBackend()
	env, h, _ := newStaleEnv(t, be, cat)
	h.WithStatusView(sv)
	sess, csrf := login(t, env, "user-a")
	owner := env.issuer.URL() + "|" + env.issuer.Subject
	be.recs["ws_super0001"] = provisioning.WorkspaceRecord{
		ID: "ws_super0001", TenantID: "tenant-a", Owner: owner,
		OwnerIssuer: env.issuer.URL(), OwnerSub: env.issuer.Subject, Name: "w-super",
		Template: provisioning.TemplateInfo{ID: "tpl_old", Name: "old", Revision: 1,
			Runtime: "LinuxContainer", Experience: "Desktop"},
		Phase: "Running", DesiredState: "Running", DataPolicy: "Ephemeral",
		Revision: 1, CreatedAt: now.Add(-time.Hour), UpdatedAt: now,
	}

	r := doReq(t, env, sess, csrf, http.MethodGet, "/v1/workspaces/ws_super0001", "", nil)
	v := decodeBody[WorkspaceView](t, r)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200", r.StatusCode)
	}
	if v.ImageBuiltAt == nil || v.ImageStale == nil || !*v.ImageStale {
		t.Fatalf("superseded revision: imageBuiltAt=%v imageStale=%v, want the 20-day-old image reported stale",
			v.ImageBuiltAt, v.ImageStale)
	}
	if n := cat.resolves.Load(); n != 0 {
		t.Fatalf("view did %d template GETs, want 0 (age comes from the workspace CR)", n)
	}
}

// TestList_NoTemplateGets: a 200-row list whose workspaces carry their image
// age (the create-time snapshot) never resolves a template. Rows without an
// age fall back to one lookup per template — TestList_AtMostOneLookupPerTemplate.
func TestList_NoTemplateGets(t *testing.T) {
	now := time.Now().UTC()
	cat := &countingCatalog{staleCatalog: staleCatalog{entries: map[string]TemplateEntry{
		"tenant-a/tpl_linuxdesktop": {ID: "tpl_linuxdesktop", Name: "linuxdesktop", Revision: 1,
			Runtime: "LinuxContainer", Experience: "Desktop", ImageBuiltAt: now.Format(time.RFC3339)},
	}}}
	be := newFakeBackend()
	env, _, _ := newStaleEnv(t, be, cat)
	sess, csrf := login(t, env, "user-a")
	owner := env.issuer.URL() + "|" + env.issuer.Subject
	for i := 0; i < 200; i++ {
		id := fmt.Sprintf("ws_list%07d", i)
		be.recs[id] = provisioning.WorkspaceRecord{
			ID: id, TenantID: "tenant-a", Owner: owner,
			OwnerIssuer: env.issuer.URL(), OwnerSub: env.issuer.Subject, Name: fmt.Sprintf("w-%d", i),
			Template: provisioning.TemplateInfo{ID: "tpl_linuxdesktop", Name: "linuxdesktop", Revision: 1,
				Runtime: "LinuxContainer", Experience: "Desktop", ImageBuiltAt: now.Format(time.RFC3339)},
			Phase: "Running", DesiredState: "Running", DataPolicy: "Ephemeral",
			Revision: 1, CreatedAt: now, UpdatedAt: now,
		}
	}
	r := doReq(t, env, sess, csrf, http.MethodGet, "/v1/workspaces?limit=200", "", nil)
	list := decodeBody[WorkspaceList](t, r)
	if r.StatusCode != http.StatusOK || len(list.Items) != 200 {
		t.Fatalf("status=%d items=%d, want 200/200", r.StatusCode, len(list.Items))
	}
	if n := cat.resolves.Load(); n != 0 {
		t.Fatalf("list did %d template GETs, want 0", n)
	}
}

// oldWorkspaces seeds n workspace records that predate the create-time image
// snapshot (no ImageBuiltAt anywhere on them), spread over the given template
// IDs round-robin.
func oldWorkspaces(be *fakeWorkspaceBackend, env *testEnv, n int, tplIDs ...string) {
	now := time.Now().UTC()
	owner := env.issuer.URL() + "|" + env.issuer.Subject
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("ws_old%08d", i)
		tpl := tplIDs[i%len(tplIDs)]
		be.recs[id] = provisioning.WorkspaceRecord{
			ID: id, TenantID: "tenant-a", Owner: owner,
			OwnerIssuer: env.issuer.URL(), OwnerSub: env.issuer.Subject, Name: fmt.Sprintf("old-%d", i),
			Template: provisioning.TemplateInfo{ID: tpl, Name: tpl[4:], Revision: 1,
				Runtime: "LinuxContainer", Experience: "Desktop"},
			Phase: "Running", DesiredState: "Running", DataPolicy: "Ephemeral",
			Revision: 1, CreatedAt: now, UpdatedAt: now,
		}
	}
}

// TestImageStale_FallbackForOldWorkspace (R9e): a workspace created before
// the image-built-at annotation existed (every v0.1 workspace at upgrade
// time) has no age on its CR or its record; the view falls back to its
// template's age instead of reporting nothing.
func TestImageStale_FallbackForOldWorkspace(t *testing.T) {
	now := time.Now().UTC()
	cat := &countingCatalog{staleCatalog: staleCatalog{entries: map[string]TemplateEntry{
		"tenant-a/tpl_linuxdesktop": {ID: "tpl_linuxdesktop", Name: "linuxdesktop", Revision: 1,
			Runtime: "LinuxContainer", Experience: "Desktop",
			ImageBuiltAt: now.Add(-20 * 24 * time.Hour).Format(time.RFC3339)},
	}}}
	be := newFakeBackend()
	env, _, _ := newStaleEnv(t, be, cat)
	sess, csrf := login(t, env, "user-a")
	oldWorkspaces(be, env, 1, "tpl_linuxdesktop")

	r := doReq(t, env, sess, csrf, http.MethodGet, "/v1/workspaces/ws_old00000000", "", nil)
	v := decodeBody[WorkspaceView](t, r)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200", r.StatusCode)
	}
	if v.ImageBuiltAt == nil || v.ImageStale == nil || !*v.ImageStale {
		t.Fatalf("old workspace: imageBuiltAt=%v imageStale=%v, want the template's 20-day-old age reported stale",
			v.ImageBuiltAt, v.ImageStale)
	}
}

// TestList_AtMostOneLookupPerTemplate (R9e): the fallback resolves each
// distinct template once per request, however many rows share it — and a
// template that cannot be resolved is not retried per row either.
func TestList_AtMostOneLookupPerTemplate(t *testing.T) {
	now := time.Now().UTC()
	cat := &countingCatalog{staleCatalog: staleCatalog{entries: map[string]TemplateEntry{
		"tenant-a/tpl_one": {ID: "tpl_one", Name: "one", Revision: 1,
			Runtime: "LinuxContainer", Experience: "Desktop", ImageBuiltAt: now.Format(time.RFC3339)},
		"tenant-a/tpl_two": {ID: "tpl_two", Name: "two", Revision: 1,
			Runtime: "LinuxContainer", Experience: "Desktop", ImageBuiltAt: now.Format(time.RFC3339)},
		// tpl_gone is deliberately absent: its lookup fails once, not 50 times.
	}}}
	be := newFakeBackend()
	env, _, _ := newStaleEnv(t, be, cat)
	sess, csrf := login(t, env, "user-a")
	oldWorkspaces(be, env, 150, "tpl_one", "tpl_two", "tpl_gone")

	r := doReq(t, env, sess, csrf, http.MethodGet, "/v1/workspaces?limit=200", "", nil)
	list := decodeBody[WorkspaceList](t, r)
	if r.StatusCode != http.StatusOK || len(list.Items) != 150 {
		t.Fatalf("status=%d items=%d, want 200/150", r.StatusCode, len(list.Items))
	}
	if n := cat.resolves.Load(); n != 3 {
		t.Fatalf("list of 150 rows over 3 templates did %d template lookups, want exactly 3", n)
	}
	withAge := 0
	for _, it := range list.Items {
		if it.ImageBuiltAt != nil {
			withAge++
		}
	}
	if withAge != 100 {
		t.Fatalf("%d rows carry imageBuiltAt, want the 100 whose template resolves", withAge)
	}
}

// TestCreate_SnapshotsImageBuiltAt: the template's image age is captured at
// create time and handed to the provisioning layer, which copies it onto the
// Workspace CR.
func TestCreate_SnapshotsImageBuiltAt(t *testing.T) {
	builtAt := time.Now().UTC().Add(-48 * time.Hour).Format(time.RFC3339)
	cat := staleCatalog{entries: map[string]TemplateEntry{
		"tenant-a/tpl_linuxdesktop": {ID: "tpl_linuxdesktop", Name: "linuxdesktop", Revision: 1,
			Runtime: "LinuxContainer", Experience: "Desktop", ImageBuiltAt: builtAt},
	}}
	be := newFakeBackend()
	env, _, _ := newStaleEnv(t, be, cat)
	sess, csrf := login(t, env, "user-a")
	r := doReq(t, env, sess, csrf, http.MethodPost, "/v1/workspaces",
		`{"name":"snap","templateRef":"tpl_linuxdesktop"}`, map[string]string{"Idempotency-Key": "key-snap-0001"})
	if r.StatusCode != http.StatusCreated && r.StatusCode != http.StatusAccepted {
		t.Fatalf("status=%d", r.StatusCode)
	}
	if len(be.gotCreate) != 1 || be.gotCreate[0].Template.ImageBuiltAt != builtAt {
		t.Fatalf("create request template=%+v, want ImageBuiltAt=%q", be.gotCreate, builtAt)
	}
}

// TestTemplateFamily_StableAcrossRevisions: two immutable revisions of one
// catalog family (distinct template ids, same catalog name) report the same
// 'family' on GET /v1/templates and on WorkspaceView.template, so clients can
// group a workspace with the catalog entry it was created from.
func TestTemplateFamily_StableAcrossRevisions(t *testing.T) {
	now := time.Now().UTC()
	cat := staleCatalog{entries: map[string]TemplateEntry{
		"tenant-a/tpl_linux-aaaaaaaa": {ID: "tpl_linux-aaaaaaaa", Name: "linux", Revision: 1,
			Runtime: "LinuxContainer", Experience: "Desktop"},
		"tenant-a/tpl_linux-bbbbbbbb": {ID: "tpl_linux-bbbbbbbb", Name: "linux", Revision: 2,
			Runtime: "LinuxContainer", Experience: "Desktop"},
	}}
	be := newFakeBackend()
	env, _, _ := newStaleEnv(t, be, cat)
	sess, csrf := login(t, env, "user-a")
	owner := env.issuer.URL() + "|" + env.issuer.Subject

	r := doReq(t, env, sess, csrf, http.MethodGet, "/v1/templates", "", nil)
	list := decodeBody[templateList](t, r)
	if len(list.Items) != 2 {
		t.Fatalf("got %d templates, want 2", len(list.Items))
	}
	for _, v := range list.Items {
		if v.Family != "linux" {
			t.Fatalf("template %s family=%q, want linux", v.ID, v.Family)
		}
	}

	for i, tpl := range []string{"tpl_linux-aaaaaaaa", "tpl_linux-bbbbbbbb"} {
		id := fmt.Sprintf("ws_family%04d", i)
		be.recs[id] = provisioning.WorkspaceRecord{
			ID: id, TenantID: "tenant-a", Owner: owner,
			OwnerIssuer: env.issuer.URL(), OwnerSub: env.issuer.Subject, Name: fmt.Sprintf("fam-%d", i),
			Template: provisioning.TemplateInfo{ID: tpl, Name: "linux", Revision: int64(i + 1),
				Runtime: "LinuxContainer", Experience: "Desktop"},
			Phase: "Running", DesiredState: "Running", DataPolicy: "Ephemeral",
			Revision: 1, CreatedAt: now, UpdatedAt: now,
		}
	}
	r = doReq(t, env, sess, csrf, http.MethodGet, "/v1/workspaces", "", nil)
	wl := decodeBody[WorkspaceList](t, r)
	if len(wl.Items) != 2 {
		t.Fatalf("got %d workspaces, want 2", len(wl.Items))
	}
	for _, v := range wl.Items {
		if v.Template.Family != "linux" {
			t.Fatalf("workspace %s template.family=%q, want linux", v.ID, v.Template.Family)
		}
	}
}
