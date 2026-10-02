// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package backend

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tinyorbitvn/tinycdi/internal/store"
)

// leaderLockKey is the session advisory lock held by the replica that runs
// the singleton control loops ("tcdi" + 0x02). The migration lock uses a
// different key (store.migrateLockKey).
const leaderLockKey int64 = 0x7463_6469_0002

// leaderRetryInterval is how often a replica without the lock tries again.
const leaderRetryInterval = 5 * time.Second

// leaderConnCloseTimeout bounds closing the lock connection.
const leaderConnCloseTimeout = 5 * time.Second

// leaderCheckInterval is how often a holder re-checks, on its lock
// connection, that it still holds the lock; leaderCheckTimeout bounds each
// check. A holder cut off by a silent partition therefore stops its loops
// within leaderCheckInterval+leaderCheckTimeout (3 s) — well before the
// server reaps the dead session (about 11 s with the keepalive parameters
// below) and lets another replica take the lock.
const (
	leaderCheckInterval = time.Second
	leaderCheckTimeout  = 2 * time.Second
)

// electSingletons registers the leader election that gates b.singletons.
// The outbox dispatcher, purge sweeper, retained syncer, idempotency prune,
// recovery and expiry planner are read-modify-write loops without a claim, so
// they run only in the replica holding a Postgres session advisory lock on a
// dedicated connection. A replica without the lock retries every retry; if
// the holder's connection drops, the loops' context is cancelled (and waited
// for) and the replica goes back to trying. The API, session gateway and
// broker surface are unaffected and stay active on every replica.
func (b *Backend) electSingletons(db *store.DB, retry time.Duration) {
	if len(b.singletons) == 0 {
		return
	}
	cfg := leaderConnConfig(db)
	loops := append([]func(context.Context){}, b.singletons...)
	b.bg = append(b.bg, func(ctx context.Context) {
		for {
			b.holdLeadership(ctx, cfg, loops)
			select {
			case <-ctx.Done():
				return
			case <-time.After(retry):
			}
		}
	})
}

// leaderConnConfig is the lock connection's configuration. Besides the
// client-side keepalives it asks the server to probe the client (idle 5 s,
// then every 2 s, 3 misses), so a client that vanished without a FIN/RST does
// not leave a zombie session holding the lock for the kernel's default of
// hours.
func leaderConnConfig(db *store.DB) *pgx.ConnConfig {
	cfg := db.Pool().Config().ConnConfig.Copy()
	// Detect a half-open lock connection in ~10 s rather than the dialer's
	// default of minutes.
	cfg.DialFunc = func(ctx context.Context, network, addr string) (net.Conn, error) {
		d := net.Dialer{KeepAliveConfig: net.KeepAliveConfig{
			Enable: true, Idle: 4 * time.Second, Interval: 2 * time.Second, Count: 3,
		}}
		return d.DialContext(ctx, network, addr)
	}
	if cfg.RuntimeParams == nil {
		cfg.RuntimeParams = map[string]string{}
	}
	cfg.RuntimeParams["tcp_keepalives_idle"] = "5"
	cfg.RuntimeParams["tcp_keepalives_interval"] = "2"
	cfg.RuntimeParams["tcp_keepalives_count"] = "3"
	return cfg
}

// lockStillHeld reports whether the session behind conn still holds the
// leader lock. It runs on the lock connection itself with a short timeout, so
// a connection that is silently dead fails the check instead of blocking.
func lockStillHeld(ctx context.Context, conn *pgx.Conn) error {
	cctx, cancel := context.WithTimeout(ctx, leaderCheckTimeout)
	defer cancel()
	var held bool
	if err := conn.QueryRow(cctx, `
		SELECT EXISTS (
			SELECT 1 FROM pg_locks
			WHERE locktype = 'advisory' AND granted AND pid = pg_backend_pid()
			  AND ((classid::bigint << 32) | objid::bigint) = $1)`, leaderLockKey).Scan(&held); err != nil {
		return err
	}
	if !held {
		return errLockNotHeld
	}
	return nil
}

var errLockNotHeld = errors.New("advisory lock no longer held by this session")

// holdLeadership makes one attempt at the lock. If it wins, it runs the loops
// until ctx ends or the lock connection is lost, stops them, and returns.
func (b *Backend) holdLeadership(ctx context.Context, cfg *pgx.ConnConfig, loops []func(context.Context)) {
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		if ctx.Err() == nil {
			b.log.Warn("leader election: connect", "err", err)
		}
		return
	}
	// Closing the connection releases the session lock.
	defer func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), leaderConnCloseTimeout)
		defer cancel()
		_ = conn.Close(cctx)
	}()

	var held bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, leaderLockKey).Scan(&held); err != nil {
		if ctx.Err() == nil {
			b.log.Warn("leader election: lock query", "err", err)
		}
		return
	}
	if !held {
		return
	}
	b.log.Info("leader election: lock acquired, starting singleton loops")

	loopCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var loopWG sync.WaitGroup
	for _, l := range loops {
		loopWG.Add(1)
		go func() { defer loopWG.Done(); l(loopCtx) }()
	}

	// While the loops run, this goroutine is the only user of conn. It
	// blocks reading, so a terminated session surfaces at once as a read
	// error; every leaderCheckInterval of quiet (a notification is never
	// sent) it re-checks the lock with a short-timeout query, so a silent
	// partition fails the check instead of blocking. Either way the loops
	// are stopped. A read timeout does not close a pgx connection.
	var lost error
	for lost == nil && ctx.Err() == nil {
		wctx, wcancel := context.WithTimeout(ctx, leaderCheckInterval)
		_, err := conn.WaitForNotification(wctx)
		timedOut := wctx.Err() != nil
		wcancel()
		switch {
		case ctx.Err() != nil:
		case err != nil && !timedOut:
			lost = err
		default:
			lost = lockStillHeld(ctx, conn)
		}
	}
	if lost != nil && ctx.Err() == nil {
		b.log.Warn("leader election: lock lost, stopping singleton loops", "err", lost)
	}
	cancel()
	// Wait for the loops before releasing anything (the deferred Close drops
	// the lock). An advisory lock is not a fencing token: if the server lost
	// the session before this replica noticed (check interval plus timeout,
	// at most 3 s), another replica may already be running its loops and the
	// two overlap for that window. The check above keeps the window short;
	// it cannot close it.
	loopWG.Wait()
	b.log.Info("leader election: singleton loops stopped")
}
