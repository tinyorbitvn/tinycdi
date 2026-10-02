// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package backend

import (
	"context"
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
	cfg := db.Pool().Config().ConnConfig.Copy()
	// Detect a half-open lock connection in ~10 s rather than the dialer's
	// default of minutes.
	cfg.DialFunc = func(ctx context.Context, network, addr string) (net.Conn, error) {
		d := net.Dialer{KeepAliveConfig: net.KeepAliveConfig{
			Enable: true, Idle: 4 * time.Second, Interval: 2 * time.Second, Count: 3,
		}}
		return d.DialContext(ctx, network, addr)
	}
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

	// Nothing else uses this connection, so a blocking read on it returns the
	// moment the server terminates the session or the socket dies. A
	// notification (none are ever sent) is ignored.
	watchDone := make(chan error, 1)
	go func() {
		for {
			if _, err := conn.WaitForNotification(loopCtx); err != nil {
				watchDone <- err
				return
			}
		}
	}()

	select {
	case <-ctx.Done():
	case err := <-watchDone:
		b.log.Warn("leader election: lock connection lost, stopping singleton loops", "err", err)
		watchDone <- err // keep one value for the drain below
	}
	cancel()
	<-watchDone
	// The loops must be fully stopped before another replica can start its
	// own: wait for them before this function releases anything.
	loopWG.Wait()
	b.log.Info("leader election: singleton loops stopped")
}
