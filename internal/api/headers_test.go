// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestAPIBrowserHeaders_SetOnEveryResponse (R1-10): the app-listener
// header middleware pins the browser policy on JSON, error and mux-404
// answers alike, and a handler's own Cache-Control wins over the default.
func TestAPIBrowserHeaders_SetOnEveryResponse(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/echo", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, r, CodeNotFound, "<img src=x onerror=alert(1)>")
	})
	mux.HandleFunc("GET /v1/cached", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "private, max-age=60")
		w.WriteHeader(http.StatusOK)
	})
	h := APIBrowserHeaders(mux)
	for _, path := range []string{"/v1/echo", "/v1/nope", "/v1/cached"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "https://portal.example.invalid"+path, nil))
		want := map[string]string{
			"X-Content-Type-Options":  "nosniff",
			"Content-Security-Policy": APIContentSecurityPolicy,
			"X-Frame-Options":         "DENY",
			"Referrer-Policy":         "no-referrer",
			"Cache-Control":           "no-store",
		}
		if path == "/v1/cached" {
			want["Cache-Control"] = "private, max-age=60"
		}
		for name, v := range want {
			if got := rec.Header().Get(name); got != v {
				t.Errorf("%s %d: %s = %q, want %q", path, rec.Code, name, got, v)
			}
		}
	}
}
