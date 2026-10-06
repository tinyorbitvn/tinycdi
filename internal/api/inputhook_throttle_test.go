// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package api

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// TestInputHookThrottle_Bounded: the per-principal throttle map is an LRU of
// at most inputThrottleMaxEntries, so a stream of distinct principals cannot
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
		hook(ctx, fmt.Sprintf("%s|user-%d", env.issuer.URL(), i), "")
		if n := size(); n > max {
			t.Fatalf("throttle map holds %d entries after %d principals, want <= %d", n, i+1, max)
		}
	}
	if n := size(); n != max {
		t.Fatalf("throttle map holds %d entries, want exactly %d (full LRU)", n, max)
	}

	// Inside the bound the throttle still collapses repeats: a recently seen
	// principal does not write again within the minute…
	touches := 0
	counting := &countingTouchStore{SessionStore: env.auth.sessions, n: &touches}
	env.auth.sessions = counting
	recent := fmt.Sprintf("%s|user-%d", env.issuer.URL(), 3*max-1)
	hook(ctx, recent, "")
	if touches != 0 {
		t.Fatalf("recent principal touched the store again within the minute (%d writes)", touches)
	}
	// …and writes again once the minute has passed.
	fc.Advance(inputTouchMinInterval + time.Second)
	hook(ctx, recent, "")
	if touches != 1 {
		t.Fatalf("principal not touched after the throttle interval (%d writes)", touches)
	}
}

type countingTouchStore struct {
	SessionStore
	n *int
}

func (c *countingTouchStore) TouchPrincipal(ctx context.Context, principal string) (int64, error) {
	*c.n++
	return c.SessionStore.TouchPrincipal(ctx, principal)
}
