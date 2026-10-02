// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package gateway

// Test-only accessors for the external test package (the export_test.go
// pattern): they expose internal bookkeeping so tests can prove negative
// space — that an unseen cookie mints no session object and leaves the
// session indexes empty, and that concurrent cookie misses rendezvous on
// one shared directory lookup.

// SessionMints reports how many session objects newSession has minted in
// this process — a monotonic counter, so callers assert the delta.
func SessionMints() int64 {
	return sessionMints.Load()
}

// InflightWaiters reports how many requests are currently parked on a
// shared in-flight session-directory lookup's done channel.
func (g *Gateway) InflightWaiters() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.inflightWaiters
}

// SessionMapCounts reports the occupancy of the session indexes and the
// in-flight lookup table; a request that may allocate nothing must leave
// all of them empty.
func (g *Gateway) SessionMapCounts() (sessions, byLease, byWorkspace, inflight int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.sessions), len(g.byLease), len(g.byWorkspace), len(g.inflight)
}
