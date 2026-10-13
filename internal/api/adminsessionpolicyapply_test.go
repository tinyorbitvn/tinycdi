// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package api

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
)

type fakeApplyBackend struct {
	running   []provisioning.WorkspaceRecord
	effective provisioning.SessionPolicy
	live      map[string]bool
	restarted map[string]int64
	// movedIDs are workspaces whose generation already moved: RestartWorkspace
	// no-ops and returns the current (moved) record.
	movedIDs map[string]bool
}

func (f *fakeApplyBackend) ListRunningWorkspaces(_ context.Context, _ string, limit int) ([]provisioning.WorkspaceRecord, error) {
	if len(f.running) > limit {
		return f.running[:limit], nil
	}
	return f.running, nil
}

func (f *fakeApplyBackend) ResolveSessionPolicy(context.Context, string, provisioning.TemplateCatalogEntry) (provisioning.SessionPolicy, error) {
	return f.effective, nil
}

func (f *fakeApplyBackend) RestartWorkspace(_ context.Context, _, _, _, id, _ string, gen int64, _ []byte) (provisioning.WorkspaceRecord, error) {
	if f.movedIDs[id] {
		return provisioning.WorkspaceRecord{ID: id, RuntimeGeneration: gen + 7}, nil
	}
	f.restarted[id] = gen
	return provisioning.WorkspaceRecord{ID: id, RuntimeGeneration: gen + 1}, nil
}

func (f *fakeApplyBackend) HasLiveLease(_ context.Context, id string, _ time.Time) (bool, error) {
	return f.live[id], nil
}

type fakeRunningPolicies map[string]AppliedPolicy

func (f fakeRunningPolicies) RunningPolicies(context.Context, string) (map[string]AppliedPolicy, bool, error) {
	return f, true, nil
}

func applyFixture() (*fakeApplyBackend, fakeRunningPolicies) {
	tight := provisioning.SessionPolicy{Clipboard: "Disabled", FileTransfer: "Disabled"}
	loose := provisioning.SessionPolicy{Clipboard: "Bidirectional", FileTransfer: "Disabled"}
	be := &fakeApplyBackend{
		effective: tight,
		live:      map[string]bool{"ws_live": true},
		restarted: map[string]int64{},
		running: []provisioning.WorkspaceRecord{
			{ID: "ws_loose", Name: "a", Owner: "https://idp|alice"},
			{ID: "ws_live", Name: "b", Owner: "https://idp|bob"},
			{ID: "ws_ok", Name: "c", Owner: "https://idp|carol"},
		},
	}
	cr := fakeRunningPolicies{
		"ws_loose": {Policy: loose, RuntimeGeneration: 3, IntentSettled: true},
		"ws_live":  {Policy: loose, RuntimeGeneration: 5, IntentSettled: true},
		"ws_ok":    {Policy: tight, RuntimeGeneration: 1, IntentSettled: true},
	}
	return be, cr
}

// newApplyEnv mounts the session-policy routes over a ceiling row
// (version v1, clipboard Disabled) with the apply seam wired to the fakes,
// and returns the row's ETag.
func newApplyEnv(t *testing.T, be *fakeApplyBackend, cr fakeRunningPolicies) (*testEnv, string) {
	t.Helper()
	src := &fakeSessionPolicySource{hasRow: true, version: 1, clip: "Disabled"}
	env := newSessionPolicyEnv(t, src, func(h *AdminSessionPolicyHandler) { h.WithApply(be, cr, nil) })
	return env, ETagOf("v1")
}

const applyPath = adminSessionPolicyPath + ":apply"

type applyResultJSON struct {
	DryRun    bool `json:"dryRun"`
	Truncated bool `json:"truncated"`
	Affected  []struct {
		WorkspaceID string `json:"workspaceId"`
		Live        bool   `json:"live"`
		Result      string `json:"result"`
	} `json:"affected"`
}

func TestApplySessionPolicy_DryRunListsLooserOnly(t *testing.T) {
	be, cr := applyFixture()
	env, etag := newApplyEnv(t, be, cr)
	sess, csrf := loginAdmin(t, env, "admin-a")
	res := doReq(t, env, sess, csrf, http.MethodPost, applyPath, `{"dryRun":true}`, map[string]string{"If-Match": etag})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d", res.StatusCode)
	}
	out := decodeBody[applyResultJSON](t, res)
	if len(out.Affected) != 2 || len(be.restarted) != 0 {
		t.Fatalf("affected=%+v restarted=%v; dry run lists ws_loose and ws_live and restarts nothing", out.Affected, be.restarted)
	}
	for _, a := range out.Affected {
		if a.Result != "pending" {
			t.Fatalf("dry-run result %q", a.Result)
		}
	}
}

func TestApplySessionPolicy_ConfirmSkipsLiveUnlessIncluded(t *testing.T) {
	be, cr := applyFixture()
	env, etag := newApplyEnv(t, be, cr)
	sess, csrf := loginAdmin(t, env, "admin-a")
	res := doReq(t, env, sess, csrf, http.MethodPost, applyPath, `{"dryRun":false}`, map[string]string{"If-Match": etag})
	out := decodeBody[applyResultJSON](t, res)
	results := map[string]string{}
	for _, a := range out.Affected {
		results[a.WorkspaceID] = a.Result
	}
	if results["ws_loose"] != "restarted" || results["ws_live"] != "skipped_active" || be.restarted["ws_loose"] != 3 {
		t.Fatalf("results=%v restarted=%v", results, be.restarted)
	}
	if _, ok := be.restarted["ws_live"]; ok {
		t.Fatal("a live session was restarted without includeActive")
	}
	res = doReq(t, env, sess, csrf, http.MethodPost, applyPath, `{"dryRun":false,"includeActive":true}`, map[string]string{"If-Match": etag})
	res.Body.Close()
	if res.StatusCode != http.StatusOK || be.restarted["ws_live"] != 5 {
		t.Fatalf("includeActive: status %d restarted=%v", res.StatusCode, be.restarted)
	}
}

// A workspace whose generation moved before our restart (another restart
// won) is reported skipped_moved, not restarted.
func TestApplySessionPolicy_MovedGeneration(t *testing.T) {
	be, cr := applyFixture()
	be.movedIDs = map[string]bool{"ws_loose": true}
	env, etag := newApplyEnv(t, be, cr)
	sess, csrf := loginAdmin(t, env, "admin-a")
	res := doReq(t, env, sess, csrf, http.MethodPost, applyPath, `{"dryRun":false}`, map[string]string{"If-Match": etag})
	out := decodeBody[applyResultJSON](t, res)
	results := map[string]string{}
	for _, a := range out.Affected {
		results[a.WorkspaceID] = a.Result
	}
	if results["ws_loose"] != "skipped_moved" {
		t.Fatalf("results=%v, want ws_loose skipped_moved", results)
	}
	if _, ok := be.restarted["ws_loose"]; ok {
		t.Fatal("a moved workspace was restarted")
	}
}

// A CR whose latest intent the operator has not reconciled is mid-change
// (an earlier restart or stop/start): listed, never restarted again.
func TestApplySessionPolicy_UnsettledIntentIsNotRestarted(t *testing.T) {
	be, cr := applyFixture()
	u := cr["ws_loose"]
	u.IntentSettled = false
	cr["ws_loose"] = u
	env, etag := newApplyEnv(t, be, cr)
	sess, csrf := loginAdmin(t, env, "admin-a")
	res := doReq(t, env, sess, csrf, http.MethodPost, applyPath, `{"dryRun":false,"includeActive":true}`, map[string]string{"If-Match": etag})
	out := decodeBody[applyResultJSON](t, res)
	results := map[string]string{}
	for _, a := range out.Affected {
		results[a.WorkspaceID] = a.Result
	}
	if results["ws_loose"] != "skipped_moved" || results["ws_live"] != "restarted" {
		t.Fatalf("results=%v, want ws_loose skipped_moved and ws_live restarted", results)
	}
	if _, ok := be.restarted["ws_loose"]; ok {
		t.Fatal("an unsettled workspace was restarted")
	}
}

func TestApplySessionPolicy_TruncatesAtCap(t *testing.T) {
	be, cr := applyFixture()
	be.running = nil
	for i := 0; i < maxPolicyApply+5; i++ {
		id := fmt.Sprintf("ws_%04d", i)
		be.running = append(be.running, provisioning.WorkspaceRecord{ID: id, Name: id, Owner: "https://idp|u"})
		cr[id] = AppliedPolicy{Policy: provisioning.SessionPolicy{Clipboard: "Bidirectional", FileTransfer: "Disabled"}, RuntimeGeneration: 1, IntentSettled: true}
	}
	env, etag := newApplyEnv(t, be, cr)
	sess, csrf := loginAdmin(t, env, "admin-a")
	res := doReq(t, env, sess, csrf, http.MethodPost, applyPath, `{"dryRun":true}`, map[string]string{"If-Match": etag})
	out := decodeBody[applyResultJSON](t, res)
	if len(out.Affected) != maxPolicyApply || !out.Truncated {
		t.Fatalf("affected=%d truncated=%v, want %d/true", len(out.Affected), out.Truncated, maxPolicyApply)
	}
}

func TestApplySessionPolicy_Preconditions(t *testing.T) {
	be, cr := applyFixture()
	env, _ := newApplyEnv(t, be, cr)
	sess, csrf := loginAdmin(t, env, "admin-a")
	if res := doReq(t, env, sess, csrf, http.MethodPost, applyPath, `{"dryRun":true}`, nil); res.StatusCode != http.StatusPreconditionRequired {
		t.Fatalf("no If-Match: %d", res.StatusCode)
	}
	if res := doReq(t, env, sess, csrf, http.MethodPost, applyPath, `{"dryRun":true}`, map[string]string{"If-Match": `"stale"`}); res.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("stale If-Match: %d", res.StatusCode)
	}
	if res := doReq(t, env, sess, csrf, http.MethodPost, applyPath, `{"dryRun":"yes"}`, map[string]string{"If-Match": `"x"`}); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad body: %d", res.StatusCode)
	}
	bad := doReq(t, env, sess, &http.Cookie{Value: "wrong-token"}, http.MethodPost, applyPath, `{"dryRun":false}`, map[string]string{"If-Match": ETagOf("v1")})
	if e := decodeErr(t, bad); bad.StatusCode != http.StatusForbidden || e.Code != CodeCSRFFailed {
		t.Fatalf("no CSRF: status=%d code=%q, want 403 CSRF_FAILED", bad.StatusCode, e.Code)
	}
	if len(be.restarted) != 0 {
		t.Fatalf("a refused call restarted %v", be.restarted)
	}
}

// Tenant isolation and role: a regular user and an admin of another tenant
// are refused before any workspace is read or restarted.
func TestApplySessionPolicy_AuthzAndTenantIsolation(t *testing.T) {
	be, cr := applyFixture()
	env, etag := newApplyEnv(t, be, cr)
	sessU, csrfU := login(t, env, "user-a")
	res := doReq(t, env, sessU, csrfU, http.MethodPost, applyPath, `{"dryRun":false}`, map[string]string{"If-Match": etag})
	if e := decodeErr(t, res); res.StatusCode != http.StatusForbidden || e.Code != CodeForbidden {
		t.Fatalf("user: status=%d code=%q", res.StatusCode, e.Code)
	}
	sessA, csrfA := loginAdmin(t, env, "admin-a")
	res = doReq(t, env, sessA, csrfA, http.MethodPost, "/v1/admin/tenants/tenant-b/session-policy:apply",
		`{"dryRun":false}`, map[string]string{"If-Match": etag})
	if e := decodeErr(t, res); res.StatusCode != http.StatusForbidden || e.Code != CodeForbidden {
		t.Fatalf("other tenant: status=%d code=%q", res.StatusCode, e.Code)
	}
	if len(be.restarted) != 0 {
		t.Fatalf("a refused call restarted %v", be.restarted)
	}
}
