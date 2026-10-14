// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package backend

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestSessionListener_HealthOnlyOnControlHosts (R1-26): /healthz and
// /readyz answer on the session listener's control hosts only. On a
// workspace host or a foreign Host the gateway answers instead — never the
// health body, and always with the pinned session-origin CSP.
func TestSessionListener_HealthOnlyOnControlHosts(t *testing.T) {
	_, h := testSessionHandler(t, newFakeBrokerClient(t))
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	get := func(host, path string) (int, string, http.Header) {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, srv.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = host
		resp, err := srv.Client().Transport.(*http.Transport).RoundTrip(req)
		if err != nil {
			t.Fatalf("GET %s on %s: %v", path, host, err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(raw), resp.Header
	}
	for _, host := range []string{"ws-0000000a.session.test", "attacker.example"} {
		for _, path := range []string{"/healthz", "/readyz"} {
			code, body, hdr := get(host, path)
			if code == http.StatusOK || strings.Contains(body, `"status"`) {
				t.Errorf("%s on %s = %d %q, want the gateway's refusal, not health", path, host, code, body)
			}
			if !strings.Contains(hdr.Get("Content-Security-Policy"), "frame-ancestors") {
				t.Errorf("%s on %s: CSP %q, want the pinned session CSP", path, host, hdr.Get("Content-Security-Policy"))
			}
		}
	}
	for _, path := range []string{"/healthz", "/readyz"} {
		if code, body, _ := get("session.test", path); code != http.StatusOK || !strings.Contains(body, `"ok"`) {
			t.Errorf("%s on the control host = %d %q, want 200 ok", path, code, body)
		}
	}
}
