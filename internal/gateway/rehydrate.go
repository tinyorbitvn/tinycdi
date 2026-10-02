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
	ClaimStream(ctx context.Context, gw broker.GatewayIdentity, leaseID string, fence broker.Fence) (uint64, error)
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
	sess *session // set before done closes; nil on any lookup failure
}

// rehydrate resolves an unseen cookie through the session directory. The
// lookup runs once per digest under g.inflight; every failure — unknown
// digest, dead lease, foreign gateway, transient — maps to "no session",
// so a bad cookie allocates no state and spawns no goroutines.
// wsID is the workspace the request Host names: a digest resolving to a
// lease owned by another workspace is a foreign cookie on this host (D11)
// and rehydrates nothing.
func (g *Gateway) rehydrate(r *http.Request, cookieValue, wsID string) *session {
	d := sessionDigest(cookieValue)
	g.mu.Lock()
	if s := g.sessions[cookieValue]; s != nil {
		g.mu.Unlock()
		if s.live(g) {
			return s
		}
		return nil
	}
	if c, ok := g.inflight[d]; ok {
		g.mu.Unlock()
		<-c.done
		if c.sess != nil && c.sess.live(g) {
			return c.sess
		}
		return nil
	}
	c := &rehydrateCall{done: make(chan struct{})}
	g.inflight[d] = c
	g.mu.Unlock()

	c.sess = g.fetchSession(r, d, cookieValue, wsID)

	g.mu.Lock()
	delete(g.inflight, d)
	g.mu.Unlock()
	close(c.done)
	if c.sess != nil && c.sess.live(g) {
		return c.sess
	}
	return nil
}

// fetchSession performs the directory lookup and, on a live lease, rebuilds
// the session and registers it in the three maps exactly as a launch would.
// A superseded session occupying the workspace slot is not killed here —
// its own renew loop sees the dead lease within one interval and tears it
// down (the same cross-replica fence a takeover relies on).
func (g *Gateway) fetchSession(r *http.Request, d broker.SessionDigest, cookieValue, wsID string) *session {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	l, err := g.cfg.Sessions.LeaseBySession(ctx, g.cfg.Identity, d)
	cancel()
	if err != nil {
		return nil
	}
	if l.WorkspaceUID != wsID {
		// Host binding (D11): the cookie is valid but belongs to a
		// different workspace's host — replay or confusion. Answer as if
		// no session existed and allocate nothing on this replica; the
		// session stays live on its own host.
		g.audit(r, "session.host_mismatch", l.WorkspaceUID, observability.OutcomeDenied, "host_mismatch")
		return nil
	}
	s := newSession(cookieValue, l, g.now())
	g.mu.Lock()
	if cur := g.sessions[cookieValue]; cur != nil {
		g.mu.Unlock()
		return cur // identical digest registered meanwhile; reuse it
	}
	g.sessions[cookieValue] = s
	g.byLease[l.ID] = s
	g.byWorkspace[l.WorkspaceUID] = s
	g.mu.Unlock()
	go g.renewLoop(s)
	go g.activitySender(s)
	return s
}

// isDraining reports whether Drain has started: new WebSocket upgrades are
// refused while the replica sheds its streams.
func (g *Gateway) isDraining() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.draining
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
