// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package gateway

// Session rehydration, cross-replica stream fencing and drain (design §3.6,
// decisions D19/D23, plan refinements P3/P6):
//
//   - launch binds SHA-256(cookie value) to the lease row, so any replica
//     can rebuild a session from a cookie it has never seen;
//   - a cookie miss falls back to a directory lookup; concurrent requests
//     for the same digest share one lookup, and a miss allocates nothing;
//   - each admitted stream bumps the lease's stream epoch; a renew that
//     observes a newer epoch means another replica owns the stream — this
//     process closes its connections but keeps the session alive;
//   - Drain closes every open stream and reports disconnect for each
//     without revoking leases, so a restart or roll never kills sessions.

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/broker"
	"github.com/tinyorbitvn/tinycdi/internal/observability"
)

// SessionDirectory is the optional broker surface that lets a session
// survive a gateway restart and be served by any replica. With a nil
// directory the gateway behaves as in v0.1 (sessions live in one process).
type SessionDirectory interface {
	BindSession(ctx context.Context, gw broker.GatewayIdentity, leaseID string, d broker.SessionDigest) error
	LeaseBySession(ctx context.Context, gw broker.GatewayIdentity, d broker.SessionDigest) (broker.Lease, error)
	// ClaimStream bumps the lease's stream epoch for the claiming tab:
	// ownerTab is the tab id the client sent on the upgrade ("" or a
	// malformed value is stored as NULL — never a matchable owner).
	ClaimStream(ctx context.Context, gw broker.GatewayIdentity, leaseID string, fence broker.Fence, ownerTab string) (uint64, error)
}

// sessionDigest derives the stored lookup key for a session cookie value —
// the cookie value itself is never persisted (D19).
func sessionDigest(cookieValue string) broker.SessionDigest {
	return broker.SessionDigest(sha256.Sum256([]byte(cookieValue)))
}

// rehydrateCall is an in-flight LeaseBySession lookup shared by every
// request carrying the same cookie digest.
type rehydrateCall struct {
	done chan struct{}
	sess *session // set before done closes; nil unless the lookup built a session
	err  error    // set before done closes; nil on success and on a clean "no session"
}

// errRehydrateAborted is what waiters see when the request that owned the
// lookup died (a panic) before recording a result.
var errRehydrateAborted = errors.New("gateway: session lookup aborted")

// rehydrateTimeout bounds the shared directory lookup.
const rehydrateTimeout = 10 * time.Second

// rehydrate resolves an unseen cookie through the session directory. The
// lookup runs once per digest under g.inflight and is detached from the
// first request's cancellation, so a client that hangs up cannot fail the
// waiters. The result distinguishes three outcomes: (session, nil) — a live
// session; (nil, nil) — definitively no session (unknown digest, dead lease,
// foreign workspace: 401, and a bad cookie allocates nothing); (nil, err) —
// the directory could not answer (503), nothing is cached and the next
// request asks again.
// wsID is the workspace the request Host names: a digest resolving to a
// lease owned by another workspace is a foreign cookie on this host (D11)
// and rehydrates nothing.
func (g *Gateway) rehydrate(r *http.Request, cookieValue, wsID string) (*session, error) {
	d := sessionDigest(cookieValue)
	g.mu.Lock()
	if s := g.sessions[cookieValue]; s != nil {
		g.mu.Unlock()
		if s.live(g) {
			return s, nil
		}
		return nil, nil
	}
	if c, ok := g.inflight[d]; ok {
		g.inflightWaiters++
		g.mu.Unlock()
		defer func() {
			g.mu.Lock()
			g.inflightWaiters--
			g.mu.Unlock()
		}()
		select {
		case <-c.done:
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
		if c.err != nil {
			return nil, c.err
		}
		if c.sess != nil && c.sess.live(g) {
			return c.sess, nil
		}
		return nil, nil
	}
	c := &rehydrateCall{done: make(chan struct{}), err: errRehydrateAborted}
	g.inflight[d] = c
	g.mu.Unlock()
	// Release the waiters and drop the entry however the lookup ends —
	// including a panic, which leaves c.err at errRehydrateAborted.
	defer func() {
		g.mu.Lock()
		delete(g.inflight, d)
		g.mu.Unlock()
		close(c.done)
	}()

	c.sess, c.err = g.fetchSession(r, d, cookieValue, wsID)
	if g.cfg.Metrics != nil {
		// E8: one count per directory lookup — the requests parked on this
		// shared call are waiters, not lookups of their own.
		switch {
		case c.err != nil:
			g.cfg.Metrics.IncRehydration("error")
		case c.sess != nil:
			g.cfg.Metrics.IncRehydration("ok")
		default:
			g.cfg.Metrics.IncRehydration("miss")
		}
	}
	if c.err != nil {
		return nil, c.err
	}
	if c.sess != nil && c.sess.live(g) {
		return c.sess, nil
	}
	return nil, nil
}

// fetchSession performs the directory lookup and, on a live lease, rebuilds
// the session and registers it in the three maps exactly as a launch would.
// A definitive lease death (invalid, revoked, denied) is "no session" and
// returns (nil, nil); any other lookup error is returned as is.
// A superseded session occupying the workspace slot is not killed here —
// its own renew loop sees the dead lease within one interval and tears it
// down (the same cross-replica fence a takeover relies on).
func (g *Gateway) fetchSession(r *http.Request, d broker.SessionDigest, cookieValue, wsID string) (*session, error) {
	// The lookup is shared by every waiter, so it must not die with the
	// first request: detach from its cancellation, keep its values.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), rehydrateTimeout)
	l, err := g.cfg.Sessions.LeaseBySession(ctx, g.cfg.Identity, d)
	cancel()
	if err != nil {
		if terminalBrokerErr(err) {
			return nil, nil
		}
		return nil, err
	}
	if l.WorkspaceUID != wsID {
		// Host binding (D11): the cookie is valid but belongs to a
		// different workspace's host — replay or confusion. Answer as if
		// no session existed and allocate nothing on this replica; the
		// session stays live on its own host.
		g.audit(r, "session.host_mismatch", l.WorkspaceUID, observability.OutcomeDenied, "host_mismatch")
		return nil, nil
	}
	s := newSession(cookieValue, l, g.now())
	g.mu.Lock()
	if cur := g.sessions[cookieValue]; cur != nil {
		g.mu.Unlock()
		return cur, nil // identical digest registered meanwhile; reuse it
	}
	g.sessions[cookieValue] = s
	g.byLease[l.ID] = s
	g.byWorkspace[l.WorkspaceUID] = s
	g.mu.Unlock()
	if g.cfg.Metrics != nil {
		g.cfg.Metrics.AddSessionsActive(1)
	}
	go g.renewLoop(s)
	go g.activitySender(s)
	return s, nil
}

// isDraining reports whether Drain has started: new WebSocket upgrades are
// refused while the replica sheds its streams.
func (g *Gateway) isDraining() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.draining
}

// drainRetryAfter is the Retry-After hint on drain refusals: the client
// should land on a sibling replica within seconds, not back off long.
const drainRetryAfter = "5"

// writeDraining answers the ONLY requests a draining replica refuses —
// new launch redemptions and new WebSocket upgrades — with a retryable
// 503. Every other request keeps serving through the drain window
// (pre-stop drain, V3.24).
func writeDraining(w http.ResponseWriter) {
	w.Header().Set("Retry-After", drainRetryAfter)
	writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "draining"})
}

// Drain closes every open stream and reports "disconnect" for each, within
// ctx's deadline. It does not revoke leases, so the same cookie reconnects
// on another replica. The gateway refuses new upgrades after Drain starts.
func (g *Gateway) Drain(ctx context.Context) {
	g.mu.Lock()
	g.draining = true
	all := make([]*session, 0, len(g.sessions))
	for _, s := range g.sessions {
		all = append(all, s)
	}
	g.mu.Unlock()

	poll := time.NewTicker(5 * time.Millisecond)
	defer poll.Stop()
	for {
		quiet := true
		for _, s := range all {
			// Repeat: a handshake admitted just before Drain started can
			// still land its conn — every pass closes what accumulated.
			s.dropStreams()
			if s.streamBusy() || s.pendingActivity() > 0 {
				quiet = false
			}
		}
		if quiet {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-poll.C:
		}
	}
}
