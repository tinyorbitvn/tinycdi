package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
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

// eventsFixture creates a workspace through the API and returns it with the
// logged-in session — the shared preamble of the R4d event tests.
func eventsFixture(t *testing.T, env *testEnv, key string) (WorkspaceView, *http.Cookie, *http.Cookie) {
	t.Helper()
	sess, csrf := login(t, env, "user-a")
	r := doReq(t, env, sess, csrf, http.MethodPost, "/v1/workspaces",
		`{"name":"evt-ws","templateRef":"tpl_linuxdesktop","desiredState":"Running"}`,
		map[string]string{"Idempotency-Key": key})
	created := decodeBody[WorkspaceView](t, r)
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("create status=%d", r.StatusCode)
	}
	return created, sess, csrf
}

func getEvents(t *testing.T, env *testEnv, sess, csrf *http.Cookie, id string) WorkspaceEventList {
	t.Helper()
	r := doReq(t, env, sess, csrf, http.MethodGet, "/v1/workspaces/"+id+"/events", "", nil)
	list := decodeBody[WorkspaceEventList](t, r)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("events status=%d, want 200", r.StatusCode)
	}
	return list
}

// TestEvents_IdsUnique: every event carries a stable id (kind + reason, plus
// the intent revision for lifecycle steps) and ids never collide within one
// list — two start requests, a Degraded condition and the synthesized
// failure event all keep distinct ids, and the ids are identical across calls.
func TestEvents_IdsUnique(t *testing.T) {
	now := time.Now()
	il := &fakeIntentLog{recs: []IntentRecord{
		{Kind: "create", Revision: 1, At: now.Add(-3 * time.Hour)},
		{Kind: "start", Revision: 2, At: now.Add(-2 * time.Hour)},
		{Kind: "stop", Revision: 3, At: now.Add(-90 * time.Minute)},
		{Kind: "start", Revision: 4, At: now.Add(-time.Hour)},
	}}
	sv := &fakeStatusView{}
	env := newEventsEnv(t, newFakeBackend(), sv, il)
	created, sess, csrf := eventsFixture(t, env, "key-evt-ids-0001")
	sv.set(created.ID, ObservedStatus{
		Fresh: true, Found: true, Phase: "Failed", FailureReason: "BootDeadlineExceeded",
		Conditions: []workspaceCondition{
			{Type: "Admitted", Status: "True", Reason: "TemplateResolved", LastTransitionTime: now.Add(-time.Hour)},
			{Type: "Degraded", Status: "True", Reason: "BootDeadlineExceeded", LastTransitionTime: now.Add(-time.Minute)},
		},
	})

	first := getEvents(t, env, sess, csrf, created.ID)
	if len(first.Items) < 6 {
		t.Fatalf("got %d events, want >= 6", len(first.Items))
	}
	seen := map[string]bool{}
	for _, e := range first.Items {
		if e.ID == "" {
			t.Fatalf("event %q has no id", e.Reason)
		}
		if seen[e.ID] {
			t.Fatalf("duplicate event id %q in %+v", e.ID, first.Items)
		}
		seen[e.ID] = true
	}
	second := getEvents(t, env, sess, csrf, created.ID)
	for i := range first.Items {
		if first.Items[i].ID != second.Items[i].ID {
			t.Fatalf("ids not stable across calls: %q vs %q", first.Items[i].ID, second.Items[i].ID)
		}
	}
}

// TestEvents_FailureTimestampStable: the synthesized failure event takes its
// timestamps from the Degraded condition's transition time, so two reads a
// minute apart return the same instants (ObservedAt moves with the clock).
func TestEvents_FailureTimestampStable(t *testing.T) {
	clock := time.Now().Truncate(time.Second)
	transition := clock.Add(-10 * time.Minute)
	sv := &fakeStatusView{}
	env := newWorkspaceEnv(t, newFakeBackend(), defaultCatalog(), defaultTenants(),
		func(h *WorkspaceHandler) {
			h.WithStatusView(sv)
			h.WithIntentLog(&fakeIntentLog{recs: []IntentRecord{{Kind: "create", Revision: 1, At: clock.Add(-time.Hour)}}})
		})
	created, sess, csrf := eventsFixture(t, env, "key-evt-fts-0001")
	read := func() WorkspaceEvent {
		sv.set(created.ID, ObservedStatus{
			Fresh: true, Found: true, Phase: "Failed", FailureReason: "BootDeadlineExceeded",
			ObservedAt: clock,
			Conditions: []workspaceCondition{
				{Type: "Degraded", Status: "True", Reason: "BootDeadlineExceeded", LastTransitionTime: transition},
			},
		})
		for _, e := range getEvents(t, env, sess, csrf, created.ID).Items {
			if strings.HasPrefix(e.ID, "Failed") {
				return e
			}
		}
		t.Fatal("no failure event")
		return WorkspaceEvent{}
	}
	a := read()
	clock = clock.Add(time.Minute) // a minute later: ObservedAt moved, the failure did not
	b := read()
	if a.FirstTimestamp == nil || b.FirstTimestamp == nil || a.LastTimestamp == nil || b.LastTimestamp == nil {
		t.Fatalf("failure event timestamps missing: %+v %+v", a, b)
	}
	if !a.FirstTimestamp.Equal(transition) || !a.LastTimestamp.Equal(transition) {
		t.Fatalf("failure timestamps %v/%v, want the condition transition time %v",
			a.FirstTimestamp, a.LastTimestamp, transition)
	}
	if !a.LastTimestamp.Equal(*b.LastTimestamp) || !a.FirstTimestamp.Equal(*b.FirstTimestamp) {
		t.Fatalf("failure timestamps changed between reads: %v -> %v", a.LastTimestamp, b.LastTimestamp)
	}
}

// TestEvents_StaleInformer: with an informer that cannot prove freshness the
// list is flagged stale: true and never claims the workspace is ready — no
// ready-typed condition reports True and the last-known ConnectionReady=True
// does not surface as a "ready" event.
func TestEvents_StaleInformer(t *testing.T) {
	now := time.Now()
	sv := &fakeStatusView{}
	il := &fakeIntentLog{recs: []IntentRecord{{Kind: "create", Revision: 1, At: now.Add(-time.Hour)}}}
	env := newEventsEnv(t, newFakeBackend(), sv, il)
	created, sess, csrf := eventsFixture(t, env, "key-evt-stale-001")
	sv.set(created.ID, ObservedStatus{
		Fresh: false, Found: true, Phase: "Ready", ObservedAt: now,
		Conditions: readyConditions(),
	})
	list := getEvents(t, env, sess, csrf, created.ID)
	if !list.Stale {
		t.Fatal("stale informer: list.stale = false, want true")
	}
	for _, e := range list.Items {
		if e.Reason == "Ready" || strings.Contains(e.Message, "is ready to connect.") ||
			strings.HasSuffix(e.Message, "is ready.") {
			t.Fatalf("stale informer emitted a ready event: %+v", e)
		}
	}

	// A fresh informer is not flagged stale and does emit the ready event.
	sv.set(created.ID, ObservedStatus{
		Fresh: true, Found: true, Phase: "Ready", ObservedAt: now, Conditions: readyConditions(),
	})
	fresh := getEvents(t, env, sess, csrf, created.ID)
	if fresh.Stale {
		t.Fatal("fresh informer: list.stale = true, want false")
	}
	ready := false
	for _, e := range fresh.Items {
		ready = ready || e.ID == "ConnectionReady.Ready"
	}
	if !ready {
		t.Fatalf("fresh informer: no ConnectionReady event in %+v", fresh.Items)
	}
}

// TestEvents_MaxDurationStop (FX-R24): a stop the platform recorded with the
// max_duration cause reads as MaxDurationReached; a user stop stays
// StopRequested.
func TestEvents_MaxDurationStop(t *testing.T) {
	be := newFakeBackend()
	base := time.Now().Add(-time.Hour)
	il := &fakeIntentLog{recs: []IntentRecord{
		{Kind: "create", Revision: 1, At: base},
		{Kind: "stop", Revision: 2, At: base.Add(10 * time.Minute)},
		{Kind: "stop", Revision: 3, At: base.Add(20 * time.Minute), Reason: "max_duration"},
	}}
	env := newEventsEnv(t, be, &fakeStatusView{}, il)
	created, sess, csrf := eventsFixture(t, env, "key-evt-fxr24-1")

	got := map[string]bool{}
	for _, ev := range getEvents(t, env, sess, csrf, created.ID).Items {
		got[ev.ID] = true
	}
	if !got["MaxDurationReached.3"] || !got["StopRequested.2"] {
		t.Fatalf("event ids = %v, want MaxDurationReached.3 and StopRequested.2", got)
	}
}

// TestEvents_TemplateUpdateSkipped (V3.2/E2): a start intent carrying a
// recorded guard-skip reason surfaces the curated TemplateUpdateSkipped
// warning — naming the cause token — alongside the start's own event.
func TestEvents_TemplateUpdateSkipped(t *testing.T) {
	base := time.Now().Add(-time.Hour)
	il := &fakeIntentLog{recs: []IntentRecord{
		{Kind: "create", Revision: 1, At: base},
		{Kind: "start", Revision: 2, At: base.Add(10 * time.Minute),
			Reason: provisioning.SkipReasonStorageSmaller},
	}}
	env := newEventsEnv(t, newFakeBackend(), &fakeStatusView{}, il)
	created, sess, csrf := eventsFixture(t, env, "key-evt-v32-001")

	got := map[string]WorkspaceEvent{}
	for _, ev := range getEvents(t, env, sess, csrf, created.ID).Items {
		got[ev.ID] = ev
	}
	if _, ok := got["StartRequested.2"]; !ok {
		t.Fatalf("start event missing from %+v", got)
	}
	skip, ok := got["TemplateUpdateSkipped.2"]
	if !ok {
		t.Fatalf("no TemplateUpdateSkipped event in %+v", got)
	}
	if skip.Reason != "TemplateUpdateSkipped" || skip.Type != "Warning" ||
		!strings.Contains(skip.Message, "storage-smaller") {
		t.Fatalf("skip event = %+v, want Warning TemplateUpdateSkipped naming storage-smaller", skip)
	}
}

// TestEvents_TemplateSkipReasons: every recorded E2 skip token curates to a
// distinct fixed message; an unrecognized token on a start intent yields no
// skip event (forward compatibility).
func TestEvents_TemplateSkipReasons(t *testing.T) {
	for _, reason := range []string{
		provisioning.SkipReasonRuntimeChanged,
		provisioning.SkipReasonExperienceChanged,
		provisioning.SkipReasonDataPolicyChanged,
		provisioning.SkipReasonStorageSmaller,
		provisioning.SkipReasonNetworkProfileChanged,
		provisioning.SkipReasonClipboardPolicyChanged,
		provisioning.SkipReasonAdapterChanged,
		provisioning.SkipReasonHostUsersChanged,
		provisioning.SkipReasonPlacementChanged,
		provisioning.SkipReasonSeccompProfileChanged,
		provisioning.SkipReasonAppArmorProfileChanged,
	} {
		ev, ok := templateSkipEvent(IntentRecord{Kind: "start", Revision: 7, Reason: reason})
		if !ok || ev.Reason != "TemplateUpdateSkipped" || !strings.Contains(ev.Message, reason) {
			t.Fatalf("reason %q -> %+v ok=%v, want TemplateUpdateSkipped naming it", reason, ev, ok)
		}
	}
	if _, ok := templateSkipEvent(IntentRecord{Kind: "start", Revision: 7, Reason: "future-cause"}); ok {
		t.Fatal("unknown reason must not produce a skip event")
	}
	if _, ok := templateSkipEvent(IntentRecord{Kind: "stop", Revision: 7, Reason: provisioning.SkipReasonStorageSmaller}); ok {
		t.Fatal("skip reason on a non-start intent must not produce a skip event")
	}
}

// ---------------------------------------------------------------------------
// B3-PARAMS: structured event params
// ---------------------------------------------------------------------------

// TestEvents_IntentParams: every intent-derived event carries the intent
// revision it embeds in its id; platform-initiated stops also report the
// recorded cause, with idle/disconnect/max-duration expiries mapping to
// their own tokens.
func TestEvents_IntentParams(t *testing.T) {
	base := time.Now()
	for _, tc := range []struct {
		name       string
		in         IntentRecord
		wantReason string
		wantCause  string // "" = no cause param
	}{
		{"create", IntentRecord{Kind: "create", Revision: 1, At: base}, "Created", ""},
		{"start", IntentRecord{Kind: "start", Revision: 4, At: base}, "StartRequested", ""},
		{"user stop", IntentRecord{Kind: "stop", Revision: 5, At: base}, "StopRequested", ""},
		{"idle stop", IntentRecord{Kind: "stop", Revision: 6, At: base, Reason: "idle_timeout"}, "IdleTimeout", "idle_timeout"},
		{"disconnect stop", IntentRecord{Kind: "stop", Revision: 7, At: base, Reason: "disconnect_timeout"}, "DisconnectTimeout", "disconnect_timeout"},
		{"max duration", IntentRecord{Kind: "stop", Revision: 8, At: base, Reason: "max_duration"}, "MaxDurationReached", "max_duration"},
		{"explicit requested", IntentRecord{Kind: "stop", Revision: 9, At: base, Reason: "requested"}, "StopRequested", "requested"},
		{"delete", IntentRecord{Kind: "delete", Revision: 10, At: base}, "DeleteRequested", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ev, ok := intentEvent(tc.in)
			if !ok {
				t.Fatal("intent produced no event")
			}
			if ev.Reason != tc.wantReason {
				t.Fatalf("reason = %q, want %q", ev.Reason, tc.wantReason)
			}
			if ev.Params["revision"] != fmt.Sprint(tc.in.Revision) {
				t.Fatalf("params.revision = %q, want %d", ev.Params["revision"], tc.in.Revision)
			}
			if got := ev.Params["cause"]; got != tc.wantCause {
				t.Fatalf("params.cause = %q, want %q", got, tc.wantCause)
			}
			// The id stays the reason+revision pair clients key on.
			if ev.ID != fmt.Sprintf("%s.%d", tc.wantReason, tc.in.Revision) {
				t.Fatalf("id = %q", ev.ID)
			}
		})
	}
}

// TestEvents_TemplateSkipParams: the skip event params the same guard token
// its message parenthesizes, plus the start intent's revision.
func TestEvents_TemplateSkipParams(t *testing.T) {
	ev, ok := templateSkipEvent(IntentRecord{Kind: "start", Revision: 7, Reason: provisioning.SkipReasonStorageSmaller})
	if !ok {
		t.Fatal("no skip event")
	}
	if ev.Params["skipReason"] != provisioning.SkipReasonStorageSmaller {
		t.Fatalf("params.skipReason = %q", ev.Params["skipReason"])
	}
	if ev.Params["revision"] != "7" {
		t.Fatalf("params.revision = %q", ev.Params["revision"])
	}
	if !strings.Contains(ev.Message, "("+ev.Params["skipReason"]+")") {
		t.Fatalf("message %q does not interpolate its skipReason param", ev.Message)
	}
}

// TestEvents_ConditionParams: a condition event params the type and status
// that selected its curated message, plus any operator-recorded params the
// condition itself carried (teardown step, drain budget).
func TestEvents_ConditionParams(t *testing.T) {
	c := workspaceCondition{
		Type: "Degraded", Status: "True", Reason: "CleanupRetry",
		Message: "teardown step cleanup blocked; retrying",
		Params:  map[string]string{"step": "cleanup"},
	}
	ev := conditionEvent(c)
	if ev.Params["condition"] != "Degraded" || ev.Params["status"] != "True" {
		t.Fatalf("params = %v, want condition+status", ev.Params)
	}
	if ev.Params["step"] != "cleanup" {
		t.Fatalf("operator params not forwarded: %v", ev.Params)
	}
}
