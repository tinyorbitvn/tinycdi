// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package api

import "net/http"

// APIContentSecurityPolicy is the CSP on every app-listener response: the
// API serves JSON and redirects only, so nothing may load, frame it, set a
// base URL or submit a form from it.
const APIContentSecurityPolicy = "default-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'"

// APICacheControl is the Cache-Control on every /v1 answer. no-store keeps
// tenant-admin listings and user data out of a shared browser's HTTP cache
// after a sign-out. no-transform (RFC 9111 §5.2.2.6) tells intermediaries
// not to recode the body: a CDN that compresses a response also weakens its
// strong ETag to W/"…", which then comes back in If-Match. It is set on all
// /v1 responses, not only those carrying an ETag, so no route (present or
// future) can forget it; the JSON bodies are small and paged, so giving up
// edge compression costs little.
const APICacheControl = "no-store, no-transform"

// APIBrowserHeaders pins the browser security policy on every response the
// app listener writes (TM-01): the edge routes /v1 on the portal host here,
// so these answers need the same framing and sniffing protection the
// frontend gives the SPA. Cache-Control defaults to APICacheControl, and a
// handler that sets its own Cache-Control wins, because
// it writes its headers after this middleware.
func APIBrowserHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Content-Security-Policy", APIContentSecurityPolicy)
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", APICacheControl)
		next.ServeHTTP(w, r)
	})
}
