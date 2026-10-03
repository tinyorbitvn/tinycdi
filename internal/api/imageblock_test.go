// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package api

// V3.3 (E3): -image-block-after — create, and a start that cannot move to
// a fresher revision, refuse a runtime image older than the limit with
// 409 IMAGE_STALE. A missing imageBuiltAt never blocks; 0 disables the
// block. Running workspaces are never touched.

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
)

const testImageBlockAfter = 45 * 24 * time.Hour

func daysAgoRFC(n int) string {
	return time.Now().UTC().Add(-time.Duration(n) * 24 * time.Hour).Format(time.RFC3339)
}

// TestCreate_BlockedWhenStale: a template built 46 days ago refuses create
// with 409 IMAGE_STALE and the message names the image age in days.
func TestCreate_BlockedWhenStale(t *testing.T) {
	cat := staleCatalog{entries: map[string]TemplateEntry{
		"tenant-a/tpl_staleimg": {
			ID: "tpl_staleimg", Name: "staleimg", Revision: 1,
			Runtime: "LinuxContainer", Experience: "Desktop",
			ImageBuiltAt: daysAgoRFC(46),
		},
	}}
	env, h, _ := newStaleEnv(t, newFakeBackend(), cat)
	h.WithImageBlockAfter(testImageBlockAfter)
	sess, csrf := login(t, env, "user-a")

	r := doReq(t, env, sess, csrf, http.MethodPost, "/v1/workspaces",
		`{"name":"stale-box","templateRef":"tpl_staleimg"}`,
		map[string]string{"Idempotency-Key": "key-block-0001"})
	e := decodeBody[Error](t, r)
	if r.StatusCode != http.StatusConflict {
		t.Fatalf("status=%d, want 409", r.StatusCode)
	}
	if e.Code != CodeImageStale {
		t.Fatalf("code=%q, want IMAGE_STALE", e.Code)
	}
	if !strings.Contains(e.Message, "46 days") {
		t.Fatalf("message %q must name the image age in days", e.Message)
	}
	if e.Retryable {
		t.Fatal("IMAGE_STALE must not be retryable")
	}
}

// TestCreate_NotBlockedWithoutBuiltAt: no image-built-at annotation never
// blocks — the create is admitted.
func TestCreate_NotBlockedWithoutBuiltAt(t *testing.T) {
	cat := staleCatalog{entries: map[string]TemplateEntry{
		"tenant-a/tpl_nobuilty": {
			ID: "tpl_nobuilty", Name: "nobuilt", Revision: 1,
			Runtime: "LinuxContainer", Experience: "Desktop",
		},
		"tenant-a/tpl_malformd": {
			ID: "tpl_malformd", Name: "malformed", Revision: 1,
			Runtime: "LinuxContainer", Experience: "Desktop",
			ImageBuiltAt: "not a timestamp",
		},
	}}
	env, h, _ := newStaleEnv(t, newFakeBackend(), cat)
	h.WithImageBlockAfter(testImageBlockAfter)
	sess, csrf := login(t, env, "user-a")

	for i, tpl := range []string{"tpl_nobuilty", "tpl_malformd"} {
		r := doReq(t, env, sess, csrf, http.MethodPost, "/v1/workspaces",
			fmt.Sprintf(`{"name":"noblock-%d","templateRef":%q}`, i, tpl),
			map[string]string{"Idempotency-Key": fmt.Sprintf("key-noblock-%02d", i)})
		r.Body.Close()
		if r.StatusCode != http.StatusCreated {
			t.Fatalf("%s: status=%d, want 201 (missing/malformed builtAt never blocks)", tpl, r.StatusCode)
		}
	}
}

// TestCreate_StaleAdvisoryBelowBlockLimit: an image over -image-stale-after
// but under -image-block-after reports imageStale yet is still admitted —
// the two thresholds are independent.
func TestCreate_StaleAdvisoryBelowBlockLimit(t *testing.T) {
	cat := staleCatalog{entries: map[string]TemplateEntry{
		"tenant-a/tpl_warnimgx": {
			ID: "tpl_warnimgx", Name: "warnimg", Revision: 1,
			Runtime: "LinuxContainer", Experience: "Desktop",
			ImageBuiltAt: daysAgoRFC(20), // stale vs 14d, fresh vs 45d
		},
	}}
	env, h, _ := newStaleEnv(t, newFakeBackend(), cat)
	h.WithImageBlockAfter(testImageBlockAfter)
	sess, csrf := login(t, env, "user-a")

	r := doReq(t, env, sess, csrf, http.MethodPost, "/v1/workspaces",
		`{"name":"warn-box","templateRef":"tpl_warnimgx"}`,
		map[string]string{"Idempotency-Key": "key-warn-00001"})
	r.Body.Close()
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("status=%d, want 201 (advisory stale does not block)", r.StatusCode)
	}
}

// TestStart_BlockedMapsToImageStale: a start refused by the E3 block (the
// resolved revision is stale and cannot move to a fresher one) surfaces
// as 409 IMAGE_STALE.
func TestStart_BlockedMapsToImageStale(t *testing.T) {
	be := newFakeBackend()
	be.signalErr = &provisioning.ImageStaleError{AgeDays: 60, LimitDays: 45}
	env := newWorkspaceEnv(t, be, defaultCatalog(), defaultTenants(),
		func(h *WorkspaceHandler) { h.WithImageBlockAfter(testImageBlockAfter) })
	sess, csrf := login(t, env, "user-a")
	owner := env.issuer.URL() + "|user-a"
	be.recs["ws_stalepin1"] = provisioning.WorkspaceRecord{
		ID: "ws_stalepin1", TenantID: "tenant-a", Owner: owner,
		OwnerIssuer: env.issuer.URL(), OwnerSub: "user-a", Name: "pinned-stale",
		Template: provisioning.TemplateInfo{
			ID: "tpl_linuxdesktop", Name: "linuxdesktop", Revision: 1,
			Runtime: "LinuxContainer", Experience: "Desktop",
			ImageBuiltAt: daysAgoRFC(60),
		},
		Phase: "Stopped", DesiredState: "Stopped", DataPolicy: "Ephemeral",
		Revision:  1,
		CreatedAt: time.Now().UTC().Add(-time.Hour), UpdatedAt: time.Now().UTC(),
	}

	r := doReq(t, env, sess, csrf, http.MethodPost, "/v1/workspaces/ws_stalepin1/start", "",
		map[string]string{"Idempotency-Key": "key-start-0001"})
	e := decodeBody[Error](t, r)
	if r.StatusCode != http.StatusConflict || e.Code != CodeImageStale {
		t.Fatalf("status=%d code=%q, want 409 IMAGE_STALE", r.StatusCode, e.Code)
	}
	if !strings.Contains(e.Message, "60 days") {
		t.Fatalf("message %q must name the image age in days", e.Message)
	}
}

// TestBlockDisabled_API: with -image-block-after=0 nothing ever answers
// 409 IMAGE_STALE — create on a 90-day-old image succeeds and the
// templates view reports imageBlocked false.
func TestBlockDisabled_API(t *testing.T) {
	cat := staleCatalog{entries: map[string]TemplateEntry{
		"tenant-a/tpl_ancientx": {
			ID: "tpl_ancientx", Name: "ancient", Revision: 1,
			Runtime: "LinuxContainer", Experience: "Desktop",
			ImageBuiltAt: daysAgoRFC(90),
		},
	}}
	env, h, th := newStaleEnv(t, newFakeBackend(), cat)
	h.WithImageBlockAfter(0)
	th.WithImageBlockAfter(0)
	sess, csrf := login(t, env, "user-a")

	r := doReq(t, env, sess, csrf, http.MethodPost, "/v1/workspaces",
		`{"name":"ancient-box","templateRef":"tpl_ancientx"}`,
		map[string]string{"Idempotency-Key": "key-ancient-01"})
	r.Body.Close()
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("disabled block: status=%d, want 201", r.StatusCode)
	}

	r = doReq(t, env, sess, csrf, http.MethodGet, "/v1/templates", "", nil)
	list := decodeBody[templateList](t, r)
	if r.StatusCode != http.StatusOK || len(list.Items) != 1 {
		t.Fatalf("templates status=%d n=%d", r.StatusCode, len(list.Items))
	}
	if v := list.Items[0]; v.ImageBlocked == nil || *v.ImageBlocked {
		t.Fatalf("disabled block: imageBlocked=%v, want false present", v.ImageBlocked)
	}
}

// TestTemplateView_ImageBlocked: the view field mirrors the block limit —
// over the limit true, under it false, missing annotation absent.
func TestTemplateView_ImageBlocked(t *testing.T) {
	cat := staleCatalog{entries: map[string]TemplateEntry{
		"tenant-a/tpl_blockme1": {
			ID: "tpl_blockme1", Name: "blockme", Revision: 1,
			Runtime: "LinuxContainer", Experience: "Desktop",
			ImageBuiltAt: daysAgoRFC(46),
		},
		"tenant-a/tpl_okimgxxx": {
			ID: "tpl_okimgxxx", Name: "okimg", Revision: 1,
			Runtime: "LinuxContainer", Experience: "Desktop",
			ImageBuiltAt: daysAgoRFC(20),
		},
		"tenant-a/tpl_noimgxxx": {
			ID: "tpl_noimgxxx", Name: "noimg", Revision: 1,
			Runtime: "LinuxContainer", Experience: "Desktop",
		},
	}}
	env, _, th := newStaleEnv(t, newFakeBackend(), cat)
	th.WithImageBlockAfter(testImageBlockAfter)
	sess, csrf := login(t, env, "user-a")

	r := doReq(t, env, sess, csrf, http.MethodGet, "/v1/templates", "", nil)
	list := decodeBody[templateList](t, r)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200", r.StatusCode)
	}
	byID := map[string]templateView{}
	for _, v := range list.Items {
		byID[v.ID] = v
	}
	if v := byID["tpl_blockme1"]; v.ImageBlocked == nil || !*v.ImageBlocked {
		t.Fatalf("46-day image: imageBlocked=%v, want true", v.ImageBlocked)
	}
	if v := byID["tpl_okimgxxx"]; v.ImageBlocked == nil || *v.ImageBlocked {
		t.Fatalf("20-day image: imageBlocked=%v, want false", v.ImageBlocked)
	}
	if v := byID["tpl_noimgxxx"]; v.ImageBlocked != nil {
		t.Fatalf("no annotation: imageBlocked=%v, want absent", *v.ImageBlocked)
	}
}

// TestRunningWorkspaceUntouched: a running workspace on a stale image
// keeps running and its connection endpoint still issues a launch ticket.
func TestRunningWorkspaceUntouched(t *testing.T) {
	be := newFakeBackend()
	env := newWorkspaceEnv(t, be, defaultCatalog(), defaultTenants(),
		func(h *WorkspaceHandler) { h.WithImageBlockAfter(testImageBlockAfter) })
	sess, csrf := login(t, env, "user-a")
	owner := env.issuer.URL() + "|user-a"
	now := time.Now().UTC()
	be.recs["ws_running01"] = provisioning.WorkspaceRecord{
		ID: "ws_running01", TenantID: "tenant-a", Owner: owner,
		OwnerIssuer: env.issuer.URL(), OwnerSub: "user-a", Name: "running-stale",
		Template: provisioning.TemplateInfo{
			ID: "tpl_linuxdesktop", Name: "linuxdesktop", Revision: 1,
			Runtime: "LinuxContainer", Experience: "Desktop",
			ImageBuiltAt: daysAgoRFC(60),
		},
		Phase: "Running", DesiredState: "Running", DataPolicy: "Ephemeral",
		Revision: 2, CreatedAt: now.Add(-2 * time.Hour), UpdatedAt: now,
	}

	// The workspace still reports Running.
	r := doReq(t, env, sess, csrf, http.MethodGet, "/v1/workspaces/ws_running01", "", nil)
	v := decodeBody[WorkspaceView](t, r)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("get status=%d, want 200", r.StatusCode)
	}
	if v.ImageStale == nil || !*v.ImageStale {
		t.Fatalf("60-day image: imageStale=%v, want true (the view still reports it)", v.ImageStale)
	}

	// And the connection endpoint — never gated by the image block —
	// still issues launch tickets for it.
	iss := &fakeIssuer{ticket: IssuedTicket{
		WorkspaceID: "ws_running01", Token: "tok",
		ExpiresAt: now.Add(time.Minute),
	}}
	cenv := newConnectionEnv(t, iss)
	sess2, csrf2 := login(t, cenv, "user-a")
	r = doReq(t, cenv, sess2, csrf2, http.MethodPost, "/v1/workspaces/ws_running01/connections",
		"{}", nil)
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("connections status=%d, want 201 — a running workspace on a stale image still connects", r.StatusCode)
	}
	r.Body.Close()
	if iss.calls != 1 || iss.gotWS != "ws_running01" {
		t.Fatalf("issuer calls=%d ws=%q, want one ticket for ws_running01", iss.calls, iss.gotWS)
	}
}
