// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package api

// V3.1 (E1): WorkspaceView.updateAvailable reports whether a start would
// move the workspace to a newer published revision of its template family,
// and templateRevision carries the verbatim spec.revision label.

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
)

// familyCatalog serves revision objects by public ID plus the newest
// revision per family.
type familyCatalog struct {
	byID   map[string]TemplateEntry
	newest map[string]TemplateEntry
}

func newFamilyCatalog() *familyCatalog {
	return &familyCatalog{byID: map[string]TemplateEntry{}, newest: map[string]TemplateEntry{}}
}

func (f *familyCatalog) add(e TemplateEntry) {
	f.byID[e.ID] = e
	f.newest[e.Name] = e
}

func (f *familyCatalog) Resolve(_ context.Context, _, id string) (TemplateEntry, error) {
	if e, ok := f.byID[id]; ok {
		return e, nil
	}
	return TemplateEntry{}, ErrTemplateNotFound
}

func (f *familyCatalog) NewestInFamily(_ context.Context, _, family string) (TemplateEntry, error) {
	if e, ok := f.newest[family]; ok {
		return e, nil
	}
	return TemplateEntry{}, ErrTemplateNotFound
}

func (f *familyCatalog) List(_ context.Context, _, _, _ string, _ int) ([]TemplateEntry, string, error) {
	var out []TemplateEntry
	for _, e := range f.newest {
		out = append(out, e)
	}
	return out, "", nil
}

func familyView(id string, be *fakeWorkspaceBackend, env *testEnv, sess, csrf *http.Cookie, t *testing.T) WorkspaceView {
	t.Helper()
	r := doReq(t, env, sess, csrf, http.MethodGet, "/v1/workspaces/"+id, "", nil)
	v := decodeBody[WorkspaceView](t, r)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200", r.StatusCode)
	}
	return v
}

func seedFamilyWorkspace(be *fakeWorkspaceBackend, issuer, subject, id, tplID, revLabel string) {
	be.recs[id] = provisioning.WorkspaceRecord{
		ID: id, TenantID: "tenant-a", Owner: issuer + "|" + subject,
		OwnerIssuer: issuer, OwnerSub: subject, Name: "fam-ws",
		Template: provisioning.TemplateInfo{
			ID: tplID, Name: "linuxdesk", Revision: 1, RevisionLabel: revLabel,
			Runtime: "LinuxContainer", Experience: "Desktop",
		},
		Phase: "Stopped", DesiredState: "Stopped", DataPolicy: "Ephemeral",
		Revision: 2, CreatedAt: time.Now().UTC().Add(-time.Hour), UpdatedAt: time.Now().UTC(),
	}
}

// TestWorkspaceView_UpdateAvailable: true when the family published a newer
// revision the workspace is not on (and a start would adopt); false when
// already on the newest or the recorded revision pins the image.
func TestWorkspaceView_UpdateAvailable(t *testing.T) {
	be := newFakeBackend()
	cat := newFamilyCatalog()
	cat.add(TemplateEntry{
		ID: "tpl_linuxdesk-aaaa1111", Name: "linuxdesk", Revision: 1,
		RevisionLabel: "2026-10-a", Runtime: "LinuxContainer", Experience: "Desktop",
		DataPolicyDefault: "Ephemeral", StorageGiB: 5, StorageBytes: 5 << 30, ImageUpdate: "OnStart",
	})
	env := newWorkspaceEnv(t, be, cat, defaultTenants())
	sess, csrf := login(t, env, "user-a")
	owner := env.issuer.URL()

	// On the only (== newest) revision: no update.
	seedFamilyWorkspace(be, owner, "user-a", "ws_upd0000001", "tpl_linuxdesk-aaaa1111", "2026-10-a")
	if v := familyView("ws_upd0000001", be, env, sess, csrf, t); v.UpdateAvailable {
		t.Fatal("updateAvailable on the newest revision, want false")
	} else if v.TemplateRevision != "2026-10-a" {
		t.Fatalf("templateRevision = %q, want 2026-10-a", v.TemplateRevision)
	}

	// Publish revision B of the same family: the view reports the update.
	cat.add(TemplateEntry{
		ID: "tpl_linuxdesk-bbbb2222", Name: "linuxdesk", Revision: 2,
		RevisionLabel: "2026-10-b", Runtime: "LinuxContainer", Experience: "Desktop",
		DataPolicyDefault: "Ephemeral", StorageGiB: 5, StorageBytes: 5 << 30, ImageUpdate: "OnStart",
	})
	if v := familyView("ws_upd0000001", be, env, sess, csrf, t); !v.UpdateAvailable {
		t.Fatal("updateAvailable false with a newer published revision, want true")
	}

	// A pinned recorded revision reports no update.
	seedFamilyWorkspace(be, owner, "user-a", "ws_upd0000002", "tpl_linuxdesk-cccc3333", "2026-09-p")
	cat.byID["tpl_linuxdesk-cccc3333"] = TemplateEntry{
		ID: "tpl_linuxdesk-cccc3333", Name: "linuxdesk", Revision: 0,
		RevisionLabel: "2026-09-p", Runtime: "LinuxContainer", Experience: "Desktop",
		DataPolicyDefault: "Ephemeral", StorageGiB: 5, StorageBytes: 5 << 30, ImageUpdate: "Pinned",
	}
	if v := familyView("ws_upd0000002", be, env, sess, csrf, t); v.UpdateAvailable {
		t.Fatal("updateAvailable on a pinned workspace, want false")
	}

	// A recorded revision whose object was deleted counts as updatable: a
	// start adopts the newest published revision.
	seedFamilyWorkspace(be, owner, "user-a", "ws_upd0000003", "tpl_linuxdesk-dead9999", "2026-08-z")
	if v := familyView("ws_upd0000003", be, env, sess, csrf, t); !v.UpdateAvailable {
		t.Fatal("updateAvailable false on a deleted recorded revision, want true")
	}

	// The guard compares bytes, not truncated GiB: a recorded revision
	// whose disk is 5GiB+1MiB must not report an update to a 5GiB-newest.
	seedFamilyWorkspace(be, owner, "user-a", "ws_upd0000004", "tpl_linuxdesk-dddd4444", "2026-09-x")
	cat.byID["tpl_linuxdesk-dddd4444"] = TemplateEntry{
		ID: "tpl_linuxdesk-dddd4444", Name: "linuxdesk", Revision: 0,
		RevisionLabel: "2026-09-x", Runtime: "LinuxContainer", Experience: "Desktop",
		DataPolicyDefault: "Ephemeral", StorageGiB: 5, StorageBytes: 5<<30 + 1<<20,
		ImageUpdate: "OnStart",
	}
	if v := familyView("ws_upd0000004", be, env, sess, csrf, t); v.UpdateAvailable {
		t.Fatal("updateAvailable true on a sub-GiB storage shrink, want false")
	}
}
