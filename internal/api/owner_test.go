package api

import (
	"net/http"
	"testing"

	"github.com/tinyorbitvn/tinycdi/internal/store"
)

// TestOwner_DisplayName: views carry owner{subject,displayName}; the
// display name comes from the principal directory and falls back to the
// subject when the directory has no entry.
func TestOwner_DisplayName(t *testing.T) {
	be := newFakeBackend()
	fs := newFakeRetainedStore()
	dir := newFakeDirectory()
	env := newScopeEnv(t, be, fs, dir)

	sess, csrf := login(t, env, "user-a")
	ownerRef := env.issuer.URL() + "|user-a"
	dir.entries["tenant-a\x00"+ownerRef] = store.DirectoryEntry{
		OwnerRef: ownerRef, Subject: "user-a", DisplayName: "Alice A"}

	r := doReq(t, env, sess, csrf, http.MethodPost, "/v1/workspaces",
		`{"name":"a-desktop","templateRef":"tpl_linuxdesktop"}`,
		map[string]string{"Idempotency-Key": "key-own-10000"})
	created := decodeBody[WorkspaceView](t, r)
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("create status=%d", r.StatusCode)
	}
	if created.Owner.Subject != "user-a" || created.Owner.DisplayName != "Alice A" {
		t.Fatalf("owner=%+v, want subject user-a / displayName Alice A", created.Owner)
	}

	// A second workspace owned by someone without a directory entry falls
	// back to the subject as the display name.
	sessB, csrfB := login(t, env, "user-b")
	r = doReq(t, env, sessB, csrfB, http.MethodPost, "/v1/workspaces",
		`{"name":"b-desktop","templateRef":"tpl_linuxdesktop"}`,
		map[string]string{"Idempotency-Key": "key-own-20000"})
	createdB := decodeBody[WorkspaceView](t, r)
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("create status=%d", r.StatusCode)
	}
	if createdB.Owner.Subject != "user-b" || createdB.Owner.DisplayName != "user-b" {
		t.Fatalf("owner=%+v, want subject fallback to displayName", createdB.Owner)
	}

	// The retained-data view carries the same owner block.
	fs.seed(RetainedRecord{
		ID: "rd_cccccccccccccccccccccccccc", TenantID: "tenant-a",
		Owner: ownerRef, State: RetainedStateRetained,
		SizeBytes: 1 << 30, Runtime: "LinuxContainer",
		SourceWorkspaceName: "a-desktop",
	})
	r = doReq(t, env, sess, csrf, http.MethodGet, "/v1/data", "", nil)
	dl := decodeBody[retainedDataList](t, r)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("data status=%d", r.StatusCode)
	}
	if len(dl.Items) != 1 {
		t.Fatalf("data items=%d, want 1", len(dl.Items))
	}
	if dl.Items[0].Owner.Subject != "user-a" || dl.Items[0].Owner.DisplayName != "Alice A" {
		t.Fatalf("retained owner=%+v, want subject user-a / displayName Alice A", dl.Items[0].Owner)
	}
}
