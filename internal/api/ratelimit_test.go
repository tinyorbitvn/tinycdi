// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/ratelimit"
)

// TestLogin_RateLimited (E7): the login-family routes share a per-client
// token bucket — the request past the burst inside the window answers
// 429 RATE_LIMITED with Retry-After.
func TestLogin_RateLimited(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	// -login-rate defaults to 30/min with burst 10; 11 requests from one
	// client inside the window exhaust it.
	l := ratelimit.New(30, 10, 100, func() time.Time { return now })
	srv := httptest.NewServer(RateLimit(l, nil, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})))
	defer srv.Close()

	var last *http.Response
	for i := 1; i <= 11; i++ {
		resp, err := srv.Client().Get(srv.URL + "/v1/login")
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		if last != nil {
			last.Body.Close()
		}
		last = resp
		if i <= 10 && resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			t.Fatalf("request %d = %d, want 200 inside the burst", i, resp.StatusCode)
		}
	}
	defer last.Body.Close()

	if last.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("request 11 = %d, want 429", last.StatusCode)
	}
	var body Error
	if err := json.NewDecoder(last.Body).Decode(&body); err != nil {
		t.Fatalf("decode 429 body: %v", err)
	}
	if body.Code != CodeRateLimited {
		t.Fatalf("code = %q, want RATE_LIMITED", body.Code)
	}
	if !body.Retryable {
		t.Fatal("RATE_LIMITED must be retryable")
	}
	if ra := last.Header.Get("Retry-After"); ra == "" {
		t.Fatal("429 carries no Retry-After")
	}
}

// TestSessionProbe_RateLimited (backlog 12): GET /v1/session is anonymous
// and passive but sits behind the same limiter once wrapped.
func TestSessionProbe_RateLimited(t *testing.T) {
	env := newTestEnv(t, nil)
	l := ratelimit.New(30, 2, 100, nil)
	mux := http.NewServeMux()
	MountSessionProbeRoute(mux, env.auth, RateLimit(l, nil, nil))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	for i := 1; i <= 3; i++ {
		resp, err := srv.Client().Get(srv.URL + "/v1/session")
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		if i <= 2 {
			if resp.StatusCode != http.StatusOK {
				resp.Body.Close()
				t.Fatalf("request %d = %d, want 200", i, resp.StatusCode)
			}
			resp.Body.Close()
			continue
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("request 3 = %d, want 429", resp.StatusCode)
		}
	}
}
