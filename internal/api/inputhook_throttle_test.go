// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package api

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// TestInputHookThrottle_Bounded: the per-session throttle map is an LRU of
// at most inputThrottleMaxEntries, so a stream of distinct sessions cannot
// grow the backend's memory without bound; inside the bound the one-write-per-
// minute throttle still holds.
func TestInputHookThrottle_Bounded(t *testing.T) {
	if inputThrottleMaxEntries != 10_000 {
		t.Fatalf("inputThrottleMaxEntries = %d, want 10000", inputThrottleMaxEntries)
	}
	env, fc := inputEnv(t)
	const max = 100
	hook, size := env.auth.newInputHook(max)
	ctx := context.Background()

	for i := 0; i < 3*max; i++ {
		hook(ctx, env.issuer.URL()+"|alice", fmt.Sprintf("digest-%d", i))
		if n := size(); n > max {
			t.Fatalf("throttle map holds %d entries after %d sessions, want <= %d", n, i+1, max)
		}
	}
	if n := size(); n != max {
		t.Fatalf("throttle map holds %d entries, want exactly %d (full LRU)", n, max)
	}

	// Inside the bound the throttle still collapses repeats: a recently seen
	// session does not write again within the minute…
	touches := 0
	counting := &countingTouchStore{SessionStore: env.auth.sessions, n: &touches}
	env.auth.sessions = counting
	principal := env.issuer.URL() + "|alice"
	recent := fmt.Sprintf("digest-%d", 3*max-1)
	hook(ctx, principal, recent)
	if touches != 0 {
		t.Fatalf("recent session touched the store again within the minute (%d writes)", touches)
	}
	// …and writes again once the minute has passed.
	fc.Advance(inputTouchMinInterval + time.Second)
	hook(ctx, principal, recent)
	if touches != 1 {
		t.Fatalf("session not touched after the throttle interval (%d writes)", touches)
	}
	// An empty digest names no session: no entry, no write.
	hook(ctx, principal, "")
	if touches != 1 || size() != max {
		t.Fatalf("empty digest touched the store (%d writes, %d entries)", touches, size())
	}
}

type countingTouchStore struct {
	SessionStore
	n *int
}

func (c *countingTouchStore) TouchSessionDigest(ctx context.Context, digestHex string) (int64, error) {
	*c.n++
	return c.SessionStore.TouchSessionDigest(ctx, digestHex)
}
