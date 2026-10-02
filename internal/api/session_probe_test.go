package api

// Tests for GET /v1/session (FX-R13b): the anonymous, passive "am I signed
// in?" probe the portal calls before /v1/me so a signed-out load never
// produces a failed (401) request in the browser console.

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"
)

func probeSession(t *testing.T, env *testEnv, cookie *http.Cookie) (*http.Response, []byte) {
	t.Helper()
	r := env.authedGet(t, cookie, "/v1/session")
	defer r.Body.Close()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read /v1/session: %v", err)
	}
	return r, body
}

func requireAuthenticated(t *testing.T, r *http.Response, body []byte, want bool) {
	t.Helper()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200 (body %s)", r.StatusCode, body)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if got["authenticated"] != want {
		t.Fatalf("authenticated = %v, want %v (body %s)", got["authenticated"], want, body)
	}
}

func TestSessionProbe_Anonymous(t *testing.T) {
	env := newTestEnv(t, nil)
	r, body := probeSession(t, env, nil)
	requireAuthenticated(t, r, body, false)
}

func TestSessionProbe_UnknownCookie(t *testing.T) {
	env := newTestEnv(t, nil)
	r, body := probeSession(t, env, &http.Cookie{Name: env.auth.SessionCookieName(), Value: "not-a-session"})
	requireAuthenticated(t, r, body, false)
}

func TestSessionProbe_ValidSession(t *testing.T) {
	env := newTestEnv(t, nil)
	sess, _ := login(t, env, "alice")
	r, body := probeSession(t, env, sess)
	requireAuthenticated(t, r, body, true)
}

func TestSessionProbe_ExpiredSession(t *testing.T) {
	env, fc := inputEnv(t)
	sess, _ := login(t, env, "alice")
	fc.Advance(31 * time.Minute) // past the 30 m idle window
	r, body := probeSession(t, env, sess)
	requireAuthenticated(t, r, body, false)
}

// TestSessionProbe_ExactlyOneKey: nothing but the boolean leaks — no subject,
// tenant, roles or token — for either answer.
func TestSessionProbe_ExactlyOneKey(t *testing.T) {
	env := newTestEnv(t, nil)
	sess, _ := login(t, env, "alice")
	for name, cookie := range map[string]*http.Cookie{"anonymous": nil, "signed-in": sess} {
		t.Run(name, func(t *testing.T) {
			_, body := probeSession(t, env, cookie)
			var got map[string]json.RawMessage
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatalf("decode: %v (%s)", err, body)
			}
			if _, ok := got["authenticated"]; !ok || len(got) != 1 {
				t.Fatalf("body = %s, want exactly the key \"authenticated\"", body)
			}
		})
	}
}

// TestSessionProbe_HeadersAndNoCookie: no-store, JSON, and the probe never
// sets a cookie.
func TestSessionProbe_HeadersAndNoCookie(t *testing.T) {
	env := newTestEnv(t, nil)
	sess, _ := login(t, env, "alice")
	for name, cookie := range map[string]*http.Cookie{"anonymous": nil, "signed-in": sess} {
		t.Run(name, func(t *testing.T) {
			r, _ := probeSession(t, env, cookie)
			if cc := r.Header.Get("Cache-Control"); cc != "no-store" {
				t.Fatalf("Cache-Control = %q, want no-store", cc)
			}
			if ct := r.Header.Get("Content-Type"); ct != "application/json" {
				t.Fatalf("Content-Type = %q, want application/json", ct)
			}
			if sc := r.Header.Values("Set-Cookie"); len(sc) != 0 {
				t.Fatalf("Set-Cookie = %v, want none", sc)
			}
		})
	}
}

// TestSessionProbe_DoesNotSlideIdleTimer: 100 probes leave the session's
// last-activity untouched, so an unattended portal tab cannot keep a session
// alive by probing.
func TestSessionProbe_DoesNotSlideIdleTimer(t *testing.T) {
	env, fc := inputEnv(t)
	sess, _ := login(t, env, "alice")
	before, err := env.store.Peek(t.Context(), sess.Value)
	if err != nil {
		t.Fatalf("peek before: %v", err)
	}
	for i := 0; i < 100; i++ {
		fc.Advance(10 * time.Second) // 100 × 10 s ≈ 16 min, still inside the idle window
		r, body := probeSession(t, env, sess)
		requireAuthenticated(t, r, body, true)
	}
	after, err := env.store.Peek(t.Context(), sess.Value)
	if err != nil {
		t.Fatalf("peek after: %v", err)
	}
	if !after.LastSeenAt.Equal(before.LastSeenAt) {
		t.Fatalf("LastSeenAt moved %v -> %v", before.LastSeenAt, after.LastSeenAt)
	}
	// And the idle clock really runs out: 31 min after login the session is dead.
	fc.Advance(15 * time.Minute)
	r, body := probeSession(t, env, sess)
	requireAuthenticated(t, r, body, false)
}

// TestSessionProbe_MeStays401: the API contract of /v1/me is unchanged.
func TestSessionProbe_MeStays401(t *testing.T) {
	env := newTestEnv(t, nil)
	r := env.authedGet(t, nil, "/v1/me")
	r.Body.Close()
	if r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("/v1/me anonymous status=%d, want 401", r.StatusCode)
	}
}

// TestSessionProbe_NoCSRFNeeded: only GET is routed; a POST is rejected by
// the mux, never treated as a probe.
func TestSessionProbe_PostNotAllowed(t *testing.T) {
	env := newTestEnv(t, nil)
	resp, err := http.Post(env.server.URL+"/v1/session", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST status=%d, want 405", resp.StatusCode)
	}
}
