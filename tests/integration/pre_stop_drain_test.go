//go:build integration

// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package integration

import (
	"net/http"
	"sync/atomic"
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
	stream := f.wsOpen(t, a, cookie)
	stream.Body.Close()

	done := make(chan struct{})
	codes := make(chan int, 512)
	var upgrade503 atomic.Bool
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
	// Upgrade probe: the ONLY refusal a drain may give a read-adjacent
	// client is the retryable 503 on a new stream.
	go func() {
		for {
			select {
			case <-done:
				return
			default:
			}
			if resp := f.wsTry(a, cookie); resp != nil {
				if resp.StatusCode == http.StatusServiceUnavailable &&
					resp.Header.Get("Retry-After") != "" {
					upgrade503.Store(true)
				}
				drainBody(resp)
			}
			time.Sleep(30 * time.Millisecond)
		}
	}()

	a.stop(t)
	close(done)

	for code := range codes {
		if code/100 != 2 {
			t.Fatalf("read poll saw non-2xx during drain: %d", code)
		}
	}
	t.Logf("upgrade drain-refusal observed: %v", upgrade503.Load())

	// The sibling serves the same cookie and re-claims the stream (the
	// epoch bump is the rollout re-claim path).
	resp := f.sessionGet(t, b, "/", cookie)
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
