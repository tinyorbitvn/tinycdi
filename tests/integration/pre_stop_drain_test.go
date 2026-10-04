//go:build integration

// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package integration

import (
	"net/http"
	"testing"
	"time"
)

// TestTwoReplicas_PreStopDrainKeepsReadsZeroNon2xx (V3.24): while a replica
// drains, a client polling reads sees zero non-2xx — only new launch
// redemptions and WebSocket upgrades are refused (retryable 503), every
// other request keeps serving. After the replica exits, the sibling
// serves the same cookie (rehydrate) and re-claims the stream.
func TestTwoReplicas_PreStopDrainKeepsReadsZeroNon2xx(t *testing.T) {
	f := newRestartFixture(t)
	a := f.startReplica(t, "a")
	defer a.cleanup(t)
	b := f.startReplica(t, "b")
	defer b.cleanup(t)

	sess, csrf := f.portalLogin(t, a, a)
	cookie := f.launch(t, a, f.issueTicket(t, a, sess, csrf))
	// The stream stays open into the drain: its conn's disconnect report is
	// what holds the drain window open below.
	stream := f.wsOpen(t, a, cookie)
	defer stream.Body.Close()

	done := make(chan struct{})
	codes := make(chan int, 512)
	// Read poll: every completed GET through the whole shutdown must be 2xx.
	go func() {
		defer close(codes)
		for {
			select {
			case <-done:
				return
			default:
			}
			if resp, err := f.sessionGetTry(t, a, "/", cookie); err == nil {
				codes <- resp.StatusCode
				drainBody(resp)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}()

	// Drain returns as soon as every session is quiet — with nothing left
	// to shed the refusal window collapses inside one poll tick, which is
	// what let a free-running probe miss the 503 entirely. Hold the window
	// open deterministically: the still-open stream's disconnect report
	// must lock the lease row FOR UPDATE before the broker records it, so
	// while the test holds that row the report cannot land, the session
	// stays non-quiet and the session listener keeps serving.
	release := f.holdLeaseLock(t, f.activeLeaseID(t))
	defer release()

	a.beginStop(t)

	// Readiness flips first on SIGTERM: /readyz answering 503 is the
	// drain-started barrier — the probe provably lands inside the window
	// and never before it.
	eventually(t, "replica a reports draining", 5*time.Second, func() bool {
		resp, err := a.client.Get(a.appURL + "/readyz")
		if err != nil {
			return false
		}
		drainBody(resp)
		return resp.StatusCode == http.StatusServiceUnavailable
	})

	// Inside the held window a new WebSocket upgrade MUST get the
	// retryable 503 + Retry-After — the only refusal a drain may give a
	// read-adjacent client. An upgrade admitted in the gap before Drain
	// flipped still gets 101; close it and probe again — the held window
	// outlasts this bound by orders of magnitude.
	var sawRefusal bool
	for deadline := time.Now().Add(3 * time.Second); !sawRefusal && time.Now().Before(deadline); {
		resp := f.wsTry(a, cookie)
		if resp == nil {
			t.Fatal("session listener closed while the drain window was still held open")
		}
		code := resp.StatusCode
		switch code {
		case http.StatusSwitchingProtocols:
			// Admitted just ahead of the drain flip — do not park on the
			// live socket, close it and probe again.
			resp.Body.Close()
		case http.StatusServiceUnavailable:
			retryAfter := resp.Header.Get("Retry-After")
			drainBody(resp)
			if retryAfter == "" {
				t.Fatal("drain refusal carried no Retry-After hint")
			}
			sawRefusal = true
		default:
			drainBody(resp)
			t.Fatalf("upgrade inside the drain window = %d, want 503", code)
		}
	}
	// The refusal half is part of the contract, not a nice-to-have: if the
	// probe never saw the retryable 503 the drain window is untested, and
	// that must be a failure — the read-poll side alone would pass against
	// a build that refused nothing or refused everything.
	if !sawRefusal {
		t.Error("upgrade probe never observed the drain's retryable 503")
	}

	// Reads keep serving inside the window: the drain refuses only new
	// launches and upgrades.
	resp := f.sessionGet(t, a, "/", cookie)
	drainBody(resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / inside the drain window = %d, want 200", resp.StatusCode)
	}

	// Let the disconnect report land: the session goes quiet, Drain
	// returns and Run completes.
	release()
	a.waitStopped(t)
	close(done)

	for code := range codes {
		if code/100 != 2 {
			t.Fatalf("read poll saw non-2xx during drain: %d", code)
		}
	}

	// The sibling serves the same cookie and re-claims the stream (the
	// epoch bump is the rollout re-claim path).
	resp = f.sessionGet(t, b, "/", cookie)
	drainBody(resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / on sibling after drain = %d, want 200", resp.StatusCode)
	}
	again := f.wsTry(b, cookie)
	if again == nil {
		t.Fatal("stream re-claim on sibling failed")
	}
	defer again.Body.Close()
	if again.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade on sibling = %d, want 101", again.StatusCode)
	}
	wsEcho(t, again)
}

// TestTwoReplicas_DrainPropagationWait (FX-R34): the drain order is
// readiness drop -> endpoint-propagation wait -> stream close -> window
// hold. Inside the propagation wait a new WebSocket upgrade is still
// SERVED — the replica may legitimately still be routed, and refusing
// would waste the client's single in-frame retry on a 503 — while a
// probe after the wait gets the retryable 503, and Run takes at least
// delay + window before returning.
func TestTwoReplicas_DrainPropagationWait(t *testing.T) {
	f := newRestartFixture(t)
	const delay = 800 * time.Millisecond
	a := f.startReplicaWith(t, "a",
		"-drain-propagation-delay", delay.String(),
		"-drain-window", "400ms")
	defer a.cleanup(t)
	b := f.startReplica(t, "b")
	defer b.cleanup(t)

	sess, csrf := f.portalLogin(t, a, a)
	cookie := f.launch(t, a, f.issueTicket(t, a, sess, csrf))

	start := time.Now()
	a.beginStop(t)

	// Readiness drops first — the probe below lands inside the wait.
	eventually(t, "replica a reports draining", 5*time.Second, func() bool {
		resp, err := a.client.Get(a.appURL + "/readyz")
		if err != nil {
			return false
		}
		drainBody(resp)
		return resp.StatusCode == http.StatusServiceUnavailable
	})
	if d := time.Since(start); d >= delay {
		t.Fatalf("readiness barrier took %v, past the %v propagation delay", d, delay)
	}

	// Still inside the wait: a new upgrade is served (101), not refused.
	if resp := f.wsTry(a, cookie); resp == nil {
		t.Fatal("session listener refused a connection inside the propagation wait")
	} else {
		code := resp.StatusCode
		resp.Body.Close()
		if code != http.StatusSwitchingProtocols {
			t.Fatalf("upgrade inside the propagation wait = %d, want 101 (serving, not 503)", code)
		}
	}

	// After the wait the drain refuses new upgrades with the retryable
	// 503 — the first refusal must not precede the delay.
	saw503At := time.Time{}
	eventually(t, "upgrade refused after the propagation wait", delay+3*time.Second, func() bool {
		resp := f.wsTry(a, cookie)
		if resp == nil {
			return false
		}
		code := resp.StatusCode
		drainBody(resp)
		if code == http.StatusServiceUnavailable {
			saw503At = time.Now()
			return true
		}
		if code == http.StatusSwitchingProtocols {
			return false // still inside the wait: served
		}
		t.Fatalf("upgrade after propagation wait = %d, want 503 or 101", code)
		return false
	})
	if d := saw503At.Sub(start); d < delay {
		t.Fatalf("first 503 %v after cancel, before the %v propagation delay ended", d, delay)
	}

	a.waitStopped(t)
	if d := time.Since(start); d < delay+300*time.Millisecond {
		t.Fatalf("Run returned %v after cancel, before propagation delay + drain window", d)
	}
}
