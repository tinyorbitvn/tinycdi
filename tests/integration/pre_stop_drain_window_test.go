//go:build integration

// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package integration

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// launchTry POSTs a ticket to the replica's session listener and returns
// the response whatever its status — the drain-window probe must see the
// refusal, not fatal on it.
func launchTry(t *testing.T, f *restartFixture, r *replica, ticket string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, r.sessURL+"/v1/launch",
		strings.NewReader(url.Values{"ticket": {ticket}}.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = f.sessionHost(t)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", restartPortalOrigin)
	resp, err := r.client.Do(req)
	if err != nil {
		return nil
	}
	return resp
}

// TestTwoReplicas_PreStopDrainServesWholeWindow: the drain window is a
// duration, not a shed budget — on SIGTERM readiness fails at once, but
// BOTH listeners keep serving for the whole -drain-window regardless of
// how fast the shed quiets. With NO open streams (nothing else can hold
// the window), a terminating replica answers every read and refuses only
// new launches and WebSocket upgrades (503 + Retry-After) — a poll sees
// zero connection errors inside the window. rc.2 product contract.
func TestTwoReplicas_PreStopDrainServesWholeWindow(t *testing.T) {
	f := newRestartFixture(t)
	const window = 1500 * time.Millisecond
	a := f.startReplicaWith(t, "a", "-drain-window", window.String())
	defer a.cleanup(t)
	b := f.startReplica(t, "b")
	defer b.cleanup(t)

	sess, csrf := f.portalLogin(t, a, a)
	sessionTicket := f.issueTicket(t, a, sess, csrf)
	// Issued before the session launch: a live lease gates new tickets
	// (409 CONNECTION_IN_USE), so the launch-refusal probe's ticket must
	// exist before this workspace has a lease.
	freshTicket := f.issueTicket(t, a, sess, csrf)
	cookie := f.launch(t, a, sessionTicket)

	start := time.Now()
	a.cancel() // SIGTERM — Run drains, then holds the window

	// Readiness flips at once: /readyz answering 503 is the drain-started
	// barrier — every poll below lands inside the window, never before it.
	eventually(t, "replica a reports draining", 5*time.Second, func() bool {
		resp, err := a.client.Get(a.appURL + "/readyz")
		if err != nil {
			return false
		}
		drainBody(resp)
		return resp.StatusCode == http.StatusServiceUnavailable
	})

	// A new launch redemption inside the window gets the retryable 503.
	if resp := launchTry(t, f, a, freshTicket); resp == nil {
		t.Fatal("launch probe hit a closed listener inside the drain window")
	} else {
		code := resp.StatusCode
		retryAfter := resp.Header.Get("Retry-After")
		drainBody(resp)
		if code != http.StatusServiceUnavailable || retryAfter == "" {
			t.Fatalf("launch inside drain window = %d (Retry-After %q), want 503 + Retry-After", code, retryAfter)
		}
	}

	// Poll every 50 ms for the whole window, measured from the cancel —
	// the true window ends strictly after this deadline, so every poll is
	// inside it: reads must be 200, upgrades 503 + Retry-After, and no
	// connection may be refused.
	var connErrs, reads, refused int
	for deadline := start.Add(window); time.Now().Before(deadline); {
		if resp, err := f.sessionGetTry(t, a, "/", cookie); err != nil {
			connErrs++
		} else {
			code := resp.StatusCode
			drainBody(resp)
			if code != http.StatusOK {
				t.Fatalf("read inside drain window = %d, want 200", code)
			}
			reads++
		}
		if resp := f.wsTry(a, cookie); resp == nil {
			connErrs++
		} else {
			code := resp.StatusCode
			retryAfter := resp.Header.Get("Retry-After")
			drainBody(resp)
			if code != http.StatusServiceUnavailable || retryAfter == "" {
				t.Fatalf("upgrade inside drain window = %d (Retry-After %q), want 503 + Retry-After", code, retryAfter)
			}
			refused++
		}
		if resp, err := a.client.Get(a.appURL + "/healthz"); err != nil {
			connErrs++
		} else {
			code := resp.StatusCode
			drainBody(resp)
			if code != http.StatusOK {
				t.Fatalf("app listener /healthz inside drain window = %d, want 200", code)
			}
		}
		time.Sleep(50 * time.Millisecond)
	}

	select {
	case err := <-a.done:
		if err != nil {
			t.Fatalf("replica a run: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("replica a did not shut down")
	}
	a.stopped = true

	if connErrs != 0 {
		t.Fatalf("%d refused connections inside the drain window — listeners closed before it ended", connErrs)
	}
	if reads == 0 || refused == 0 {
		t.Fatalf("window probes landed nothing: reads=%d refused=%d", reads, refused)
	}
	if d := time.Since(start); d < window {
		t.Fatalf("Run returned %v after cancel, before the %v drain window ended", d, window)
	}

	// Past the window the socket is gone; the sibling still serves the
	// same session cookie (the refused ticket's non-consumption is covered
	// by TestDrain_RefusesLaunchWithoutConsumingTicket).
	if resp := f.wsTry(a, cookie); resp != nil {
		drainBody(resp)
		t.Fatal("session listener still answering after the drain window ended")
	}
	resp := f.sessionGet(t, b, "/", cookie)
	drainBody(resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / on sibling after drain = %d, want 200", resp.StatusCode)
	}
}
