// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/ratelimit"
)

// FX-R30: the login family keys authenticated requests on a digest of the
// session (or the validated OIDC state on the callback) so users sharing
// one NAT address keep their own budgets; anonymous and unverifiable
// requests keep sharing the per-IP bucket.

// sessionKeyTestAuth builds an Authenticator over an in-memory store with
// no OIDC wiring — the session-key resolver only needs the cookie name,
// the store and the clock.
func sessionKeyTestAuth(store SessionStore) *Authenticator {
	return &Authenticator{
		cfg:      &AuthConfig{SessionCookieName: "__Host-tcdi_session"},
		sessions: store,
		now:      time.Now,
	}
}

// saveSession stores a live session and returns the cookie a browser of
// that session would carry.
func saveSession(t *testing.T, store SessionStore, id string) *http.Cookie {
	t.Helper()
	now := time.Now()
	err := store.Save(context.Background(), &Session{
		ID:         id,
		CreatedAt:  now,
		LastSeenAt: now,
		ExpiresAt:  now.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("Save session %s: %v", id, err)
	}
	return &http.Cookie{Name: "__Host-tcdi_session", Value: id}
}

func probeRequest(t *testing.T, srv *httptest.Server, session *http.Cookie) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/v1/session", nil)
	if err != nil {
		t.Fatalf("build probe: %v", err)
	}
	if session != nil {
		req.AddCookie(session)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("probe roundtrip: %v", err)
	}
	return resp
}

// TestSessionProbe_SessionKeyedBehindNAT (FX-R30 test 1): twenty users on
// ONE client address, each with a valid session, probe at a cadence the
// per-IP burst cannot absorb — every request still passes because each
// keys on its own session digest.
func TestSessionProbe_SessionKeyedBehindNAT(t *testing.T) {
	store := NewInMemorySessionStore(30 * time.Minute)
	a := sessionKeyTestAuth(store)
	// Production shape (60/min, burst 20): 60 probes from one address
	// keyed by IP would refuse 40.
	l := ratelimit.New(60, 20, 1000, nil)
	mux := http.NewServeMux()
	MountSessionProbeRoute(mux, a, RateLimitWithKey(l, nil, nil, a.SessionRateLimitKey()))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	for i := 0; i < 20; i++ {
		sess := saveSession(t, store, "sess-nat-"+strconv.Itoa(i))
		for j := 0; j < 3; j++ {
			resp := probeRequest(t, srv, sess)
			var v sessionProbeView
			_ = json.NewDecoder(resp.Body).Decode(&v)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("user %d probe %d = %d, want 200 — session-keyed users must not share the IP bucket", i, j, resp.StatusCode)
			}
			if !v.Authenticated {
				t.Fatalf("user %d probe %d not authenticated", i, j)
			}
		}
	}
}

// TestLogin_IPKeyedNoStoreRead (FX-R30 review): /v1/login is the anonymous
// login start — it stays on the plain client-IP key and must not spend a
// store read on any cookie, valid or forged, before the refusal.
func TestLogin_IPKeyedNoStoreRead(t *testing.T) {
	counter := &peekCounter{SessionStore: NewInMemorySessionStore(30 * time.Minute)}
	now := time.Unix(1_700_000_000, 0)
	l := ratelimit.New(30, 2, 1000, func() time.Time { return now }) // burst 2, frozen
	mux := http.NewServeMux()
	mux.Handle("GET /v1/login", RateLimit(l, nil, nil)(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusFound) })))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	valid := saveSession(t, counter, "sess-login")
	forged := &http.Cookie{Name: "__Host-tcdi_session", Value: "forged-not-a-session"}
	// A valid cookie earns nothing on this route and a forged one buys no
	// store read: all three share the single client-IP bucket — the third
	// request is refused no matter what it carries.
	for _, tc := range []struct {
		name   string
		cookie *http.Cookie
		want   int
	}{
		{"valid cookie", valid, http.StatusFound},
		{"forged cookie", forged, http.StatusFound},
		{"valid cookie again", valid, http.StatusTooManyRequests},
	} {
		req, err := http.NewRequest(http.MethodGet, srv.URL+"/v1/login", nil)
		if err != nil {
			t.Fatalf("build login: %v", err)
		}
		req.AddCookie(tc.cookie)
		resp, err := noRedirectClient().Do(req)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Fatalf("%s = %d, want %d", tc.name, resp.StatusCode, tc.want)
		}
	}
	if counter.peeks != 0 {
		t.Fatalf("Peek calls = %d, want 0 — /v1/login must not read the session store", counter.peeks)
	}
}

// TestSessionProbe_AbusiveSessionKeyed (FX-R30 tests 2+3+6): one session
// flooding the probe starves only its own bucket — a neighbour on the
// same address keeps its budget, anonymous and forged-cookie requests
// keep sharing the per-IP one, and the raw session id never reaches the
// log.
func TestSessionProbe_AbusiveSessionKeyed(t *testing.T) {
	store := NewInMemorySessionStore(30 * time.Minute)
	a := sessionKeyTestAuth(store)
	now := time.Unix(1_700_000_000, 0)
	l := ratelimit.New(30, 2, 1000, func() time.Time { return now }) // burst 2, frozen — no refill
	mux := http.NewServeMux()
	MountSessionProbeRoute(mux, a, RateLimitWithKey(l, nil, nil, a.SessionRateLimitKey()))

	logBuf := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(logBuf, nil))
	srv := httptest.NewServer(RequestID(AuditWithSink(logger, nil)(mux)))
	defer srv.Close()

	abuser := saveSession(t, store, "sess-abuser")
	neighbour := saveSession(t, store, "sess-neighbour")

	if resp := probeRequest(t, srv, neighbour); resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("neighbour probe = %d, want 200", resp.StatusCode)
	} else {
		resp.Body.Close()
	}

	// The abuser spends its own burst of two and then pays 429 — nobody
	// else's bucket is touched.
	for i, want := range []int{http.StatusOK, http.StatusOK, http.StatusTooManyRequests, http.StatusTooManyRequests, http.StatusTooManyRequests} {
		resp := probeRequest(t, srv, abuser)
		if i == 2 && resp.Header.Get("Retry-After") == "" {
			resp.Body.Close()
			t.Fatal("session-keyed 429 carries no Retry-After")
		}
		if resp.StatusCode != want {
			resp.Body.Close()
			t.Fatalf("abuser probe %d = %d, want %d", i, resp.StatusCode, want)
		}
		resp.Body.Close()
	}

	if resp := probeRequest(t, srv, neighbour); resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("neighbour probe after abuser flood = %d, want 200 — the flood must not starve other sessions", resp.StatusCode)
	} else {
		resp.Body.Close()
	}

	// Anonymous probes and a forged cookie still share the per-IP budget:
	// a value that does not verify can never mint a fresh key.
	forged := &http.Cookie{Name: "__Host-tcdi_session", Value: "forged-not-a-session"}
	for _, tc := range []struct {
		name   string
		cookie *http.Cookie
		want   int
	}{
		{"anonymous 1", nil, http.StatusOK},
		{"forged 1", forged, http.StatusOK},
		{"anonymous 2", nil, http.StatusTooManyRequests},
		{"forged 2", forged, http.StatusTooManyRequests},
	} {
		resp := probeRequest(t, srv, tc.cookie)
		if resp.StatusCode != tc.want {
			resp.Body.Close()
			t.Fatalf("%s = %d, want %d", tc.name, resp.StatusCode, tc.want)
		}
		resp.Body.Close()
	}

	if strings.Contains(logBuf.String(), abuser.Value) {
		t.Fatal("audit log contains the raw session id")
	}
}

// peekCounter wraps a SessionStore and counts Peek calls, proving the
// probe reuses the rate-limit lookup instead of a second store read.
type peekCounter struct {
	SessionStore
	peeks int
}

func (s *peekCounter) Peek(ctx context.Context, id string) (*Session, error) {
	s.peeks++
	return s.SessionStore.Peek(ctx, id)
}

// TestSessionProbe_ReusesRateLimitLookup (FX-R30): the session key
// resolver's Peek is the same read the probe performs — the verified
// session rides the request context downstream, so one limited probe is
// exactly one store read.
func TestSessionProbe_ReusesRateLimitLookup(t *testing.T) {
	counter := &peekCounter{SessionStore: NewInMemorySessionStore(30 * time.Minute)}
	a := sessionKeyTestAuth(counter)
	l := ratelimit.New(30, 10, 1000, nil)
	mux := http.NewServeMux()
	MountSessionProbeRoute(mux, a, RateLimitWithKey(l, nil, nil, a.SessionRateLimitKey()))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	sess := saveSession(t, counter, "sess-reuse")
	resp := probeRequest(t, srv, sess)
	var v sessionProbeView
	_ = json.NewDecoder(resp.Body).Decode(&v)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !v.Authenticated {
		t.Fatalf("probe = %d authenticated=%v, want 200 true", resp.StatusCode, v.Authenticated)
	}
	if counter.peeks != 1 {
		t.Fatalf("Peek calls = %d, want exactly 1 — the probe must reuse the resolver's lookup", counter.peeks)
	}
}

// TestCallback_StateKeyed (FX-R30 test 4): each completed login keys its
// callback on the validated OIDC state — twenty logins through one client
// address all pass a burst of five — while a callback whose state does
// not verify falls back to the per-IP bucket.
func TestCallback_StateKeyed(t *testing.T) {
	env := newTestEnv(t, nil)
	now := time.Unix(1_700_000_000, 0)
	l := ratelimit.New(30, 5, 1000, func() time.Time { return now })         // burst 5, frozen
	ceiling := ratelimit.New(300, 50, 1000, func() time.Time { return now }) // 10x, as wired
	mux := http.NewServeMux()
	mux.Handle("GET /auth/callback", RateLimitWithCeiling(l, ceiling, nil, nil, env.auth.CallbackRateLimitKey())(
		http.HandlerFunc(env.auth.CallbackHandler)))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// Twenty valid logins: every callback carries the login cookie sealing
	// its state, so each keys on a distinct digest — four times the IP
	// burst would 429 fifteen of them keyed by address.
	for i := 0; i < 20; i++ {
		query, cookies := env.callbackQueryForLogin(t)
		req, err := http.NewRequest(http.MethodGet, srv.URL+"/auth/callback?"+query, nil)
		if err != nil {
			t.Fatalf("login %d build callback: %v", i, err)
		}
		req.AddCookie(findCookie(cookies, env.auth.LoginCookieName()))
		resp, err := noRedirectClient().Do(req)
		if err != nil {
			t.Fatalf("login %d callback: %v", i, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusFound {
			t.Fatalf("login %d callback = %d, want 302 — a validated state must key the bucket, not the shared IP", i, resp.StatusCode)
		}
	}

	// A browser that never started a login keys on the client IP: five
	// bogus-state callbacks pass the bucket to the handler's own 401, the
	// sixth is refused by the limiter.
	for i := 0; i < 6; i++ {
		resp, err := noRedirectClient().Get(srv.URL + "/auth/callback?state=bogus&code=bogus")
		if err != nil {
			t.Fatalf("bogus callback %d: %v", i, err)
		}
		want := http.StatusUnauthorized
		if i == 5 {
			want = http.StatusTooManyRequests
		}
		if resp.StatusCode != want {
			resp.Body.Close()
			t.Fatalf("bogus callback %d = %d, want %d", i, resp.StatusCode, want)
		}
		resp.Body.Close()
	}

	// A valid login cookie whose query state does not match the sealed
	// state is "unvalidated" too — it keys on the same (now empty) IP
	// bucket, not on a fresh digest.
	_, cookies := env.callbackQueryForLogin(t)
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/auth/callback?state=wrong&code=bogus", nil)
	if err != nil {
		t.Fatalf("build mismatched callback: %v", err)
	}
	req.AddCookie(findCookie(cookies, env.auth.LoginCookieName()))
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatalf("mismatched callback: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("mismatched-state callback = %d, want 429 — an unvalidated state must fall back to the client-IP key", resp.StatusCode)
	}
}

// TestCallback_ValidatedStateCeiling (FX-R30 review): validated states are
// mintable — each costs one IP-limited login start — so a spray of
// distinct valid states from ONE client address must still hit the per-IP
// ceiling instead of scaling without bound.
func TestCallback_ValidatedStateCeiling(t *testing.T) {
	env := newTestEnv(t, nil)
	now := time.Unix(1_700_000_000, 0)
	l := ratelimit.New(30, 5, 1000, func() time.Time { return now })         // login-family bucket
	ceiling := ratelimit.New(300, 15, 1000, func() time.Time { return now }) // ceiling burst 15
	mux := http.NewServeMux()
	mux.Handle("GET /auth/callback", RateLimitWithCeiling(l, ceiling, nil, nil, env.auth.CallbackRateLimitKey())(
		http.HandlerFunc(env.auth.CallbackHandler)))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// Twenty distinct logins complete their callbacks: each state bucket
	// has room (own key), but the shared ceiling admits only fifteen —
	// logins 16-20 pay 429.
	for i := 0; i < 20; i++ {
		query, cookies := env.callbackQueryForLogin(t)
		req, err := http.NewRequest(http.MethodGet, srv.URL+"/auth/callback?"+query, nil)
		if err != nil {
			t.Fatalf("login %d build callback: %v", i, err)
		}
		req.AddCookie(findCookie(cookies, env.auth.LoginCookieName()))
		resp, err := noRedirectClient().Do(req)
		if err != nil {
			t.Fatalf("login %d callback: %v", i, err)
		}
		want := http.StatusFound
		if i >= 15 {
			want = http.StatusTooManyRequests
		}
		if resp.StatusCode != want {
			resp.Body.Close()
			t.Fatalf("login %d callback = %d, want %d — valid states must still pass the per-IP ceiling", i, resp.StatusCode, want)
		}
		resp.Body.Close()
	}
}
