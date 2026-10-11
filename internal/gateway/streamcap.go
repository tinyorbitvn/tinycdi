// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package gateway

// Per-workspace upstream stream bound (V11).
//
// Every request the session proxy admits to a workspace's runtime — a
// /websockify stream or a plain asset fetch — holds an upstream
// connection, and each connection costs the KasmVNC 1.5 runtime about
// three file descriptors inside the Xvnc process. That process crashes
// once its fd count crosses FD_SETSIZE (1024): FD_SET writes past the
// stack-allocated fd_set inside select(2) — ~330 concurrent connections
// on a clean pod, earlier under contention (V11-SPIKE). The runtime
// ships no connection cap, so the bound lives here: a request over the
// workspace's cap answers a retryable 429 instead of one more upstream
// fd, converting an environment-dependent upstream crash — which resets
// every live stream at once — into a deliberate client signal.
//
// The bound is per replica: N replicas admit at most N x cap upstream
// connections to one workspace. It counts plain HTTP requests too —
// they open real upstream connections, and a burst of them can cross
// the same crash boundary. The refusal is checked before any upgrade
// admission so it claims no stream epoch and fences no live conn.

import (
	"net/http"

	"github.com/tinyorbitvn/tinycdi/internal/observability"
)

// DefaultUpstreamStreamCap is the default per-workspace, per-replica
// bound. It stays an order of magnitude under the measured ~334-conn
// crash boundary — (FD_SETSIZE 1024 - ~23 baseline fds) / ~3 fds per
// conn — while real use is one stream plus a handful of parallel asset
// fetches.
const DefaultUpstreamStreamCap = 32

// streamCapRetryAfter is the Retry-After hint on a cap refusal: a slot
// frees the moment another request unwinds, so the client retries almost
// at once — the KasmVNC client's own in-frame reconnect absorbs it.
const streamCapRetryAfter = "1"

// acquireUpstreamStream takes one of the workspace's per-replica upstream
// slots; false when the workspace already holds UpstreamStreamCap of
// them. wsID is the workspace UID the request Host named — session host
// binding already proved the session's lease matches it.
func (g *Gateway) acquireUpstreamStream(wsID string) bool {
	g.streamMu.Lock()
	defer g.streamMu.Unlock()
	if g.upstreamSlots[wsID] >= g.cfg.UpstreamStreamCap {
		return false
	}
	g.upstreamSlots[wsID]++
	g.upstreamInFlight++
	if g.cfg.Metrics != nil {
		g.cfg.Metrics.SetUpstreamStreams(float64(g.upstreamInFlight))
	}
	return true
}

// releaseUpstreamStream frees the slot acquireUpstreamStream took. It is
// deferred by the one admission point in serveProxy, so every exit path —
// normal close, hijack teardown, upstream error, client disconnect, ctx
// cancel — releases the slot exactly once. A call with no slot held for
// the workspace is unpaired: it must NOT drive the counts negative (an
// under-count would admit past the cap), so it is refused with one loud
// log line — unreachable today, fail-loud if a second release site ever
// appears.
func (g *Gateway) releaseUpstreamStream(wsID string) {
	g.streamMu.Lock()
	defer g.streamMu.Unlock()
	if g.upstreamSlots[wsID] == 0 {
		if g.cfg.Logger != nil {
			g.cfg.Logger.Error("upstream stream released with no slot held — counts left unchanged", "ws", wsID)
		}
		return
	}
	if n := g.upstreamSlots[wsID]; n > 1 {
		g.upstreamSlots[wsID] = n - 1
	} else {
		delete(g.upstreamSlots, wsID)
	}
	g.upstreamInFlight--
	if g.cfg.Metrics != nil {
		g.cfg.Metrics.SetUpstreamStreams(float64(g.upstreamInFlight))
	}
}

// writeStreamCapRefused answers an over-cap request: 429 + Retry-After
// with the session-listener error shape, counted and audited exactly like
// the session-lookup limiter's refusals.
func (g *Gateway) writeStreamCapRefused(w http.ResponseWriter, r *http.Request, wsID string) {
	if g.cfg.Metrics != nil {
		g.cfg.Metrics.IncRateLimited("upstream_streams")
	}
	w.Header().Set("Retry-After", streamCapRetryAfter)
	g.audit(r, "session.stream", wsID, observability.OutcomeDenied, "too_many_streams")
	writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "too_many_streams"})
}
