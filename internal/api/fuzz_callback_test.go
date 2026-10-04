// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/api/loginstate"
	"github.com/tinyorbitvn/tinycdi/internal/api/oidctest"
	"github.com/tinyorbitvn/tinycdi/internal/ratelimit"
)

// FuzzCallbackParsing drives the OIDC callback's request parsing and
// validation boundary with arbitrary query strings and Cookie headers:
// state/code/error params, the sealed login-state cookie opened with the
// shared test keys, and — for inputs that pass state binding — the code
// exchange against the in-process test issuer (loopback only; no external
// IdP). Invariants: the handler never panics, the outcome is
// deterministic, and every response is exactly the status the documented
// rejection order demands — a callback can never succeed for a code the
// issuer never minted.
func FuzzCallbackParsing(f *testing.F) {
	iss, err := oidctest.NewIssuer()
	if err != nil {
		f.Fatalf("oidctest.NewIssuer: %v", err)
	}
	f.Cleanup(iss.Close)
	sealer, err := loginstate.NewSealer(testLoginKeys...)
	if err != nil {
		f.Fatalf("loginstate.NewSealer: %v", err)
	}
	auth, err := NewAuthenticator(context.Background(), AuthConfig{
		Issuer:      iss.URL(),
		ClientID:    iss.ClientID,
		RedirectURL: "https://portal.test/auth/callback",
		LoginSealer: sealer,
	}, NewInMemorySessionStore(30*time.Minute),
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		f.Fatalf("NewAuthenticator: %v", err)
	}

	// A well-formed sealed login cookie for a known OAuth state; the
	// callback must bind ?state= to it before any exchange is attempted.
	validState := "fuzz-oauth-state-1"
	validTok, err := sealer.Seal(loginstate.State{
		OAuthState: validState,
		Nonce:      "nonce-1",
		Verifier:   "verifier-1",
		Expires:    time.Now().Add(time.Hour).Unix(),
	})
	if err != nil {
		f.Fatalf("seal login state: %v", err)
	}
	expiredTok, err := sealer.Seal(loginstate.State{
		OAuthState: validState,
		Nonce:      "nonce-1",
		Verifier:   "verifier-1",
		Expires:    time.Now().Add(-time.Minute).Unix(),
	})
	if err != nil {
		f.Fatalf("seal expired login state: %v", err)
	}
	loginCookie := auth.LoginCookieName()

	for _, c := range [][2]string{
		// rawQuery, Cookie header — seeded from auth_test.go cases plus
		// parser edge inputs.
		{"state=" + validState + "&code=abc", loginCookie + "=" + validTok},
		{"state=wrong-state&code=abc", loginCookie + "=" + validTok},
		{"state=" + validState + "&code=", loginCookie + "=" + validTok},
		{"code=abc", loginCookie + "=" + validTok},
		{"state=" + validState, loginCookie + "=" + validTok},
		{"error=access_denied&state=" + validState + "&code=abc", loginCookie + "=" + validTok},
		{"error_description=oops&state=" + validState + "&code=abc", loginCookie + "=" + validTok},
		{"state=" + validState + "&code=abc", ""},
		{"state=" + validState + "&code=abc", loginCookie + "="},
		{"state=" + validState + "&code=abc", loginCookie + "=garbage!!"},
		{"state=" + validState + "&code=abc", loginCookie + "=" + expiredTok},
		{"state=" + validState + "&code=abc", "other=x; " + loginCookie + "=" + validTok},
		{"state=" + validState + "&code=abc", loginCookie + "=x; " + loginCookie + "=" + validTok},
		{"state=" + validState + "&code=abc&error=x", loginCookie + "=" + validTok},
		{"state=first&state=" + validState + "&code=abc", loginCookie + "=" + validTok},
		{"state=" + validState + "&code=abc&code=def", loginCookie + "=" + validTok},
		{"%zz", loginCookie + "=" + validTok},
		{";;;", loginCookie + "=" + validTok},
		{"state=%00&code=%00", loginCookie + "=" + validTok},
		{"state=" + validState + "&code=" + strings.Repeat("c", 8192), loginCookie + "=" + validTok},
		{"state=" + validState + "&code=abc", loginCookie + "=" + validTok + "tail"},
		{"state=" + validState + "&code=abc", loginCookie + "=" + strings.Repeat("A", 4096)},
	} {
		f.Add(c[0], c[1])
	}
	f.Fuzz(func(t *testing.T, rawQuery, cookieHdr string) {
		// Wire-realistic bounds: a query string or Cookie header a client
		// can actually send stays far under 64 KiB (net/http caps the
		// whole header block at 1 MiB, browsers much lower). Bigger
		// mutator-grown strings only add parse cost and stall the run's
		// minimization rounds — they cannot reach the handler.
		if len(rawQuery) > 64<<10 || len(cookieHdr) > 64<<10 {
			return
		}
		u, err := url.ParseRequestURI("/auth/callback?" + rawQuery)
		if err != nil {
			return // net/http rejects the request line the same way
		}
		newReq := func() *http.Request {
			r := &http.Request{Method: http.MethodGet, URL: u, Header: http.Header{}}
			if cookieHdr != "" {
				r.Header["Cookie"] = []string{cookieHdr}
			}
			return r
		}
		rec := httptest.NewRecorder()
		auth.CallbackHandler(rec, newReq())
		got := rec.Code

		// Expected status, recomputed in the documented rejection order:
		// IdP error > missing params > cookie open > state binding >
		// exchange (which can only fail for a code the issuer never saw).
		q := u.Query()
		want := http.StatusUnauthorized
		if e := q.Get("error"); e == "" {
			if q.Get("state") == "" || q.Get("code") == "" {
				want = http.StatusBadRequest
			}
		}
		if got != want {
			t.Fatalf("callback query=%q cookie=%q: status %d, want %d",
				rawQuery, cookieHdr, got, want)
		}

		// Determinism: replaying the identical request yields the same
		// outcome — the sealed cookie is stateless and a rejected
		// exchange consumes nothing server-side.
		rec2 := httptest.NewRecorder()
		auth.CallbackHandler(rec2, newReq())
		if rec2.Code != got {
			t.Fatalf("callback not deterministic: %d then %d", got, rec2.Code)
		}

		// The callback rate-limit key resolver shares the same cookie
		// open + state bind: a key is minted iff the state validates, and
		// is always the digest form, never the raw state.
		key := auth.CallbackRateLimitKey()(newReq())
		if c, err := newReq().Cookie(loginCookie); err == nil {
			if st, err := sealer.Open(c.Value, time.Now()); err == nil && st.OAuthState == q.Get("state") && q.Get("state") != "" {
				want := ratelimit.KeyDigest("oidc:", q.Get("state"))
				if key != want {
					t.Fatalf("rate-limit key = %q, want %q", key, want)
				}
			} else if key != "" {
				t.Fatalf("rate-limit key %q minted for unvalidated state", key)
			}
		} else if key != "" {
			t.Fatalf("rate-limit key %q minted without a login cookie", key)
		}
	})
}
