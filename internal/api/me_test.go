package api

// Contract tests for GET /v1/me and the v0.2 session surface (D17, D18, P1):
// the CSRF token is derived from the session ID and returned by /v1/me,
// legacy non-__Host- cookies are deleted at login, and desktop input slides
// the portal idle timer without ever reviving an expired session.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// testSessionDomain is the session domain the shared test env mounts /v1/me
// with (a bare host[:port] per sessionhost.Domain.String()).
const testSessionDomain = "session.test"

type meResponse struct {
	Subject       string   `json:"subject"`
	DisplayName   string   `json:"displayName"`
	Email         string   `json:"email"`
	Tenant        string   `json:"tenant"`
	Roles         []string `json:"roles"`
	CSRFToken     string   `json:"csrfToken"`
	SessionDomain string   `json:"sessionDomain"`
}

func decodeMe(t *testing.T, r *http.Response) meResponse {
	t.Helper()
	var me meResponse
	if err := json.NewDecoder(r.Body).Decode(&me); err != nil {
		t.Fatalf("decode /v1/me: %v", err)
	}
	return me
}

// TestMe_Shape: the bootstrap payload carries every required field; the CSRF
// token equals the token derived from the raw session ID and sessionDomain
// echoes the configured domain.
func TestMe_Shape(t *testing.T) {
	env := newTestEnv(t, nil)
	sess, _ := login(t, env, "alice")

	r := env.authedGet(t, sess, "/v1/me")
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200", r.StatusCode)
	}
	me := decodeMe(t, r)
	if me.Subject != "alice" {
		t.Fatalf("subject = %q, want alice", me.Subject)
	}
	if me.DisplayName != "alice" {
		t.Fatalf("displayName = %q, want subject until the principal directory lands", me.DisplayName)
	}
	if me.Tenant != "tenant-a" {
		t.Fatalf("tenant = %q, want tenant-a", me.Tenant)
	}
	if len(me.Roles) != 1 || me.Roles[0] != "user" {
		t.Fatalf("roles = %v, want [user]", me.Roles)
	}
	if me.CSRFToken == "" || me.CSRFToken != csrfTokenFor(sess.Value) {
		t.Fatalf("csrfToken = %q, want csrfTokenFor(session ID)", me.CSRFToken)
	}
	if me.SessionDomain != testSessionDomain {
		t.Fatalf("sessionDomain = %q, want %q", me.SessionDomain, testSessionDomain)
	}
}

// TestMe_Unauthenticated: no session cookie -> 401.
func TestMe_Unauthenticated(t *testing.T) {
	env := newTestEnv(t, nil)
	r := env.authedGet(t, nil, "/v1/me")
	defer r.Body.Close()
	if r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status=%d, want 401", r.StatusCode)
	}
}

// TestMe_TenantAdminRole: membership in the tenant-admin group adds the role;
// a plain member carries only "user".
func TestMe_TenantAdminRole(t *testing.T) {
	env := newTestEnv(t, nil)

	env.issuer.Groups = []string{TenantAdminGroup}
	sess, _ := login(t, env, "admin-1")
	r := env.authedGet(t, sess, "/v1/me")
	me := decodeMe(t, r)
	r.Body.Close()
	if len(me.Roles) != 2 || me.Roles[0] != "user" || me.Roles[1] != TenantAdminGroup {
		t.Fatalf("admin roles = %v, want [user tenant-admin]", me.Roles)
	}

	env.issuer.Groups = []string{"devs"}
	sess, _ = login(t, env, "alice")
	r = env.authedGet(t, sess, "/v1/me")
	me = decodeMe(t, r)
	r.Body.Close()
	if len(me.Roles) != 1 || me.Roles[0] != "user" {
		t.Fatalf("member roles = %v, want [user]", me.Roles)
	}
}

// TestCSRF_DerivedTokenAccepted: the token returned by /v1/me satisfies
// RequireCSRF on a mutating request (P1).
func TestCSRF_DerivedTokenAccepted(t *testing.T) {
	env := newTestEnv(t, nil)
	sess, _ := login(t, env, "alice")

	r := env.authedGet(t, sess, "/v1/me")
	me := decodeMe(t, r)
	r.Body.Close()

	req, _ := http.NewRequest(http.MethodPost, env.server.URL+"/v1/echo-owner", strings.NewReader("{}"))
	req.AddCookie(sess)
	req.Header.Set(env.auth.CSRFHeader(), me.CSRFToken)
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200 — /v1/me token must satisfy RequireCSRF", resp.StatusCode)
	}
}

// TestCSRF_OtherSessionsTokenRejected: a token derived from session X's ID is
// not valid on session Y.
func TestCSRF_OtherSessionsTokenRejected(t *testing.T) {
	env := newTestEnv(t, nil)
	sessX, _ := login(t, env, "alice")
	sessY, _ := login(t, env, "bob")

	req, _ := http.NewRequest(http.MethodPost, env.server.URL+"/v1/echo-owner", strings.NewReader("{}"))
	req.AddCookie(sessY)
	req.Header.Set(env.auth.CSRFHeader(), csrfTokenFor(sessX.Value))
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status=%d, want 403 for a foreign session's token", resp.StatusCode)
	}
}

// TestLogin_SetsOnlyHostPrefixedCookies: every cookie the callback writes is
// either __Host-prefixed or a deletion (Max-Age=0) of a v0.1 legacy name (D17).
func TestLogin_SetsOnlyHostPrefixedCookies(t *testing.T) {
	env := newTestEnv(t, nil)
	resp, _ := env.login(t)
	defer resp.Body.Close()

	legacy := map[string]bool{"tcdi_csrf": true, "tcdi_session_origin": true}
	for _, h := range resp.Header.Values("Set-Cookie") {
		name, _, _ := strings.Cut(h, "=")
		if strings.HasPrefix(name, "__Host-") {
			if !strings.Contains(h, "HttpOnly") || !strings.Contains(h, "Secure") {
				t.Fatalf("__Host- cookie without Secure/HttpOnly: %q", h)
			}
			continue
		}
		if !legacy[name] || !strings.Contains(h, "Max-Age=0") {
			t.Fatalf("non-__Host- live cookie on portal origin: %q", h)
		}
	}
}

// inputEnv returns an env whose session store and authenticator run on a
// shared fake clock, plus the broker input hook wired to the session store.
func inputEnv(t *testing.T) (*testEnv, *fakeClock) {
	t.Helper()
	env := newTestEnv(t, nil)
	fc := &fakeClock{now: time.Now()}
	env.store.WithClock(fc.Now)
	env.auth.now = fc.Now
	return env, fc
}

// TestInputActivity_ExtendsIdle: a desktop "input" event slides the portal
// idle window; the 12 h absolute cap still applies (D18).
func TestInputActivity_ExtendsIdle(t *testing.T) {
	env, fc := inputEnv(t)
	sess, _ := login(t, env, "alice")
	hook := env.auth.InputHook()

	fc.Advance(25 * time.Minute)
	hook(context.Background(), env.issuer.URL()+"|alice")

	fc.Advance(15 * time.Minute) // t = 40 min — inside idle only because of the input
	r := env.authedGet(t, sess, "/v1/me")
	r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("session idle-expired despite input at minute 25: %d", r.StatusCode)
	}

	fc.Advance(12*time.Hour - 40*time.Minute + time.Minute) // t = 12 h + 1 min
	r = env.authedGet(t, sess, "/v1/me")
	r.Body.Close()
	if r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("absolute-expired session accepted: %d", r.StatusCode)
	}
}

// TestInputActivity_DoesNotReviveExpiredSession: input arriving after the
// idle deadline does not resurrect the session.
func TestInputActivity_DoesNotReviveExpiredSession(t *testing.T) {
	env, fc := inputEnv(t)
	sess, _ := login(t, env, "alice")
	hook := env.auth.InputHook()

	fc.Advance(31 * time.Minute) // past the 30 m idle window
	hook(context.Background(), env.issuer.URL()+"|alice")

	r := env.authedGet(t, sess, "/v1/me")
	defer r.Body.Close()
	if r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("idle-dead session revived by input: %d", r.StatusCode)
	}
}

// TestInputActivity_OtherPrincipalUntouched: input on user X's lease never
// slides user Y's session — including equal sub under a different issuer.
func TestInputActivity_OtherPrincipalUntouched(t *testing.T) {
	env, fc := inputEnv(t)
	sessX, _ := login(t, env, "alice")
	now := fc.Now
	// A second principal sharing alice's sub under a different issuer.
	err := env.store.Save(context.Background(), &Session{
		ID:        "other-issuer-session",
		Principal: Principal{Issuer: "https://other-idp.example", Subject: "alice", TenantID: "tenant-a"},
		CreatedAt: now(), LastSeenAt: now(), ExpiresAt: now().Add(12 * time.Hour),
	})
	if err != nil {
		t.Fatalf("plant session: %v", err)
	}
	sessY := &http.Cookie{Name: env.auth.SessionCookieName(), Value: "other-issuer-session"}

	hook := env.auth.InputHook()
	fc.Advance(25 * time.Minute)
	hook(context.Background(), env.issuer.URL()+"|alice")

	fc.Advance(10 * time.Minute) // t = 35 min: X fresh (25 m), Y dead (35 m)
	r := env.authedGet(t, sessX, "/v1/me")
	r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("session X rejected after own input: %d", r.StatusCode)
	}
	r = env.authedGet(t, sessY, "/v1/me")
	r.Body.Close()
	if r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("session Y slid by X's input: %d", r.StatusCode)
	}
}
