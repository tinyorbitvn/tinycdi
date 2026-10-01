package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// fakeIntentLog serves a canned API-side lifecycle history.
type fakeIntentLog struct {
	recs []IntentRecord
	err  error
}

func (f *fakeIntentLog) IntentHistory(_ context.Context, _, _ string) ([]IntentRecord, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.recs, nil
}

func newEventsEnv(t *testing.T, be workspaceBackend, sv StatusView, il IntentLog) *testEnv {
	t.Helper()
	return newWorkspaceEnv(t, be, defaultCatalog(), defaultTenants(),
		func(h *WorkspaceHandler) {
			h.WithStatusView(sv)
			h.WithIntentLog(il)
		})
}

// TestEvents_Curated: an operator failure whose raw text leaks a node name
// and an image reference must surface only curated messages — never the raw
// Kubernetes/operator strings.
func TestEvents_Curated(t *testing.T) {
	be := newFakeBackend()
	sv := &fakeStatusView{}
	il := &fakeIntentLog{recs: []IntentRecord{
		{Kind: "create", Revision: 1, At: time.Now().Add(-time.Hour)},
	}}
	env := newEventsEnv(t, be, sv, il)
	sess, csrf := login(t, env, "user-a")

	r := doReq(t, env, sess, csrf, http.MethodPost, "/v1/workspaces",
		`{"name":"doomed","templateRef":"tpl_linuxdesktop","desiredState":"Running"}`,
		map[string]string{"Idempotency-Key": "key-evt-10000"})
	created := decodeBody[WorkspaceView](t, r)
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("create status=%d", r.StatusCode)
	}

	const nodeName = "node-42.lab.internal"
	const imageRef = "registry.local/kasmweb/core@sha256:deadbeef"
	sv.set(created.ID, ObservedStatus{
		Fresh:         true,
		Found:         true,
		Phase:         "Failed",
		FailureReason: "BootDeadlineExceeded",
		Conditions: []workspaceCondition{
			{Type: "Admitted", Status: "True", Reason: "TemplateResolved",
				LastTransitionTime: time.Now().Add(-time.Hour)},
			{Type: "Degraded", Status: "True", Reason: "ReconcileError",
				Message: "pod ws-x failed on node " + nodeName +
					": pull " + imageRef + " timed out",
				LastTransitionTime: time.Now()},
		},
	})

	r = doReq(t, env, sess, csrf, http.MethodGet,
		"/v1/workspaces/"+created.ID+"/events", "", nil)
	list := decodeBody[WorkspaceEventList](t, r)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200", r.StatusCode)
	}
	if len(list.Items) == 0 {
		t.Fatal("events list is empty")
	}
	sawWarning := false
	for _, e := range list.Items {
		if strings.Contains(e.Message, nodeName) || strings.Contains(e.Message, imageRef) {
			t.Fatalf("event message leaks operator internals: %q", e.Message)
		}
		if strings.Contains(e.Reason, nodeName) || strings.Contains(e.Reason, imageRef) {
			t.Fatalf("event reason leaks operator internals: %q", e.Reason)
		}
		if e.Type != "Normal" && e.Type != "Warning" {
			t.Fatalf("bad event type %q", e.Type)
		}
		if e.Type == "Warning" {
			sawWarning = true
		}
	}
	if !sawWarning {
		t.Fatal("failed workspace produced no Warning event")
	}
}

// TestEvents_NotOwner: events share the workspace read's ownership rule —
// a foreign workspace id is indistinguishable from a missing one.
func TestEvents_NotOwner(t *testing.T) {
	be := newFakeBackend()
	env := newEventsEnv(t, be, &fakeStatusView{}, &fakeIntentLog{})
	sessA, csrfA := login(t, env, "user-a")

	r := doReq(t, env, sessA, csrfA, http.MethodPost, "/v1/workspaces",
		`{"name":"a-desktop","templateRef":"tpl_linuxdesktop"}`,
		map[string]string{"Idempotency-Key": "key-evt-20000"})
	created := decodeBody[WorkspaceView](t, r)
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("create status=%d", r.StatusCode)
	}

	sessB, csrfB := login(t, env, "user-b")
	r = doReq(t, env, sessB, csrfB, http.MethodGet,
		"/v1/workspaces/"+created.ID+"/events", "", nil)
	body := decodeBody[Error](t, r)
	if r.StatusCode != http.StatusNotFound {
		t.Fatalf("status=%d, want 404", r.StatusCode)
	}
	if body.Code != CodeNotFound {
		t.Fatalf("code=%q, want NOT_FOUND", body.Code)
	}
}

// TestEvents_NewestFirst: the list is ordered newest first (OpenAPI
// WorkspaceEventList contract).
func TestEvents_NewestFirst(t *testing.T) {
	be := newFakeBackend()
	base := time.Now().Add(-time.Hour)
	il := &fakeIntentLog{recs: []IntentRecord{
		{Kind: "create", Revision: 1, At: base},
		{Kind: "start", Revision: 2, At: base.Add(10 * time.Minute)},
		{Kind: "stop", Revision: 3, At: base.Add(20 * time.Minute)},
	}}
	env := newEventsEnv(t, be, &fakeStatusView{}, il)
	sess, csrf := login(t, env, "user-a")

	r := doReq(t, env, sess, csrf, http.MethodPost, "/v1/workspaces",
		`{"name":"cycle","templateRef":"tpl_linuxdesktop"}`,
		map[string]string{"Idempotency-Key": "key-evt-30000"})
	created := decodeBody[WorkspaceView](t, r)
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("create status=%d", r.StatusCode)
	}

	r = doReq(t, env, sess, csrf, http.MethodGet,
		"/v1/workspaces/"+created.ID+"/events", "", nil)
	list := decodeBody[WorkspaceEventList](t, r)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200", r.StatusCode)
	}
	if len(list.Items) < 3 {
		t.Fatalf("events len=%d, want >=3", len(list.Items))
	}
	for i := 1; i < len(list.Items); i++ {
		prev := list.Items[i-1].LastTimestamp
		cur := list.Items[i].LastTimestamp
		if prev != nil && cur != nil && cur.After(*prev) {
			t.Fatalf("events not newest-first at %d: %v > %v", i, cur, prev)
		}
	}
}
