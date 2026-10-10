// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package api

// FIX-IDLE: the portal's read endpoints are mounted behind
// RequireAuthPassive — the SPA polls workspace list/detail/events and the
// data lists on an 8-10 s interval, and under the old sliding mounts a
// visible-but-unattended tab kept its session alive forever. Only
// mutations and server-measured desktop input count as user activity (D18).

import (
	"net/http"
	"testing"
	"time"
)

// TestPolledReads_DoNotSlideIdleWindow: interval polls on the read surface
// authenticate but never slide — the +50 s poll succeeds yet the idle
// window still lapses at +60 s. Regression: on the unfixed mounts this
// poll slid the window and the session never expired.
func TestPolledReads_DoNotSlideIdleWindow(t *testing.T) {
	env := newWorkspaceEnv(t, newFakeBackend(), defaultCatalog(), defaultTenants())
	fc := &fakeClock{now: time.Now()}
	env.store.WithClock(fc.Now)
	env.auth.now = fc.Now
	env.store.idle = time.Minute

	sess, _ := login(t, env, "user-a")

	fc.Advance(50 * time.Second)
	r := env.authedGet(t, sess, "/v1/workspaces")
	r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("poll inside the idle window rejected: %d", r.StatusCode)
	}
	fc.Advance(15 * time.Second) // past idle — the poll did not slide
	r = env.authedGet(t, sess, "/v1/workspaces")
	r.Body.Close()
	if r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("idle-expired session kept alive by polling: %d", r.StatusCode)
	}
}

// TestMutation_StillSlidesIdleWindow: a POST on the write surface extends
// the window — the GETs in between never do, so the +50 s read lands
// inside idle only because the mutation slid.
func TestMutation_StillSlidesIdleWindow(t *testing.T) {
	env := newWorkspaceEnv(t, newFakeBackend(), defaultCatalog(), defaultTenants())
	fc := &fakeClock{now: time.Now()}
	env.store.WithClock(fc.Now)
	env.auth.now = fc.Now
	env.store.idle = time.Minute

	sess, csrf := login(t, env, "user-a")

	fc.Advance(50 * time.Second)
	r := doReq(t, env, sess, csrf, http.MethodPost, "/v1/workspaces",
		`{"name":"research-desktop","templateRef":"tpl_linuxdesktop"}`,
		map[string]string{"Idempotency-Key": "key-mut-slide-1"})
	r.Body.Close()
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("create inside the idle window rejected: %d", r.StatusCode)
	}
	fc.Advance(50 * time.Second) // inside idle only because the POST slid
	r = env.authedGet(t, sess, "/v1/workspaces")
	r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("session rejected inside the window a mutation opened: %d", r.StatusCode)
	}
	fc.Advance(61 * time.Second) // window lapses — nothing slid since the POST
	r = env.authedGet(t, sess, "/v1/workspaces")
	r.Body.Close()
	if r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("idle-expired session accepted: %d", r.StatusCode)
	}
}

// TestMeRead_DoesNotSlideIdleWindow: GET /v1/me is the portal's bootstrap
// read, re-issued by retry loops — it is passive too (FIX-IDLE).
func TestMeRead_DoesNotSlideIdleWindow(t *testing.T) {
	env := newTestEnv(t, nil)
	fc := &fakeClock{now: time.Now()}
	env.store.WithClock(fc.Now)
	env.auth.now = fc.Now
	env.store.idle = time.Minute

	resp, cookies := env.login(t)
	resp.Body.Close()
	sess := findCookie(cookies, env.auth.SessionCookieName())

	fc.Advance(50 * time.Second)
	r := env.authedGet(t, sess, "/v1/me")
	r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("read inside the idle window rejected: %d", r.StatusCode)
	}
	fc.Advance(15 * time.Second)
	r = env.authedGet(t, sess, "/v1/me")
	r.Body.Close()
	if r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("idle-expired session kept alive by /v1/me reads: %d", r.StatusCode)
	}
}

// TestSessionTouch_SlidesIdleWindow (FIX-IDLE): POST /v1/session:touch is
// the explicit user-activity beat — RequireAuth's sliding read extends the
// window while the passive reads around it never do. CSRF is required like
// every mutation; a missing token answers 403 and does not slide.
func TestSessionTouch_SlidesIdleWindow(t *testing.T) {
	env := newTestEnv(t, nil)
	fc := &fakeClock{now: time.Now()}
	env.store.WithClock(fc.Now)
	env.auth.now = fc.Now
	env.store.idle = time.Minute

	sess, csrf := login(t, env, "user-a")

	fc.Advance(50 * time.Second)
	r := doReq(t, env, sess, csrf, http.MethodPost, "/v1/session:touch", "", nil)
	r.Body.Close()
	if r.StatusCode != http.StatusNoContent {
		t.Fatalf("touch rejected: %d", r.StatusCode)
	}
	fc.Advance(50 * time.Second) // inside idle only because the touch slid
	r = env.authedGet(t, sess, "/v1/me")
	r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("session rejected inside the window a touch opened: %d", r.StatusCode)
	}
	fc.Advance(61 * time.Second)
	r = env.authedGet(t, sess, "/v1/me")
	r.Body.Close()
	if r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("idle-expired session accepted: %d", r.StatusCode)
	}

	// No CSRF token: denied, and the denied request must NOT slide — the
	// chain is RequireAuthPassive + RequireCSRF, so a cookie-only POST can
	// never extend the window. The session then expires on schedule.
	sess2, _ := login(t, env, "user-b")
	fc.Advance(50 * time.Second)
	r = doReq(t, env, sess2, &http.Cookie{Value: "forged"}, http.MethodPost,
		"/v1/session:touch", "", nil)
	r.Body.Close()
	if r.StatusCode != http.StatusForbidden {
		t.Fatalf("touch without CSRF: %d, want 403", r.StatusCode)
	}
	fc.Advance(15 * time.Second) // past idle — the denied touch did not slide
	r = env.authedGet(t, sess2, "/v1/me")
	r.Body.Close()
	if r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("denied touch slid the idle window: %d", r.StatusCode)
	}

	// Anonymous: 401.
	r = doReq(t, env, &http.Cookie{Value: "no-such-session"}, csrf,
		http.MethodPost, "/v1/session:touch", "", nil)
	r.Body.Close()
	if r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("touch without session: %d, want 401", r.StatusCode)
	}
}
