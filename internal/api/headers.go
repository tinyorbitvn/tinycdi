// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package api

import "net/http"

// APIContentSecurityPolicy is the CSP on every app-listener response: the
// API serves JSON and redirects only, so nothing may load, frame it, set a
// base URL or submit a form from it.
const APIContentSecurityPolicy = "default-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'"

// APIBrowserHeaders pins the browser security policy on every response the
// app listener writes (TM-01): the edge routes /v1 on the portal host here,
// so these answers need the same framing and sniffing protection the
// frontend gives the SPA. Cache-Control defaults to no-store — tenant-admin
// listings and user data must not outlive a sign-out in a shared browser's
// HTTP cache — and a handler that sets its own Cache-Control wins, because
// it writes its headers after this middleware.
func APIBrowserHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Content-Security-Policy", APIContentSecurityPolicy)
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}
