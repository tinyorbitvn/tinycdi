// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package backend

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/gateway"
	"github.com/tinyorbitvn/tinycdi/internal/tlsreload"
)

// Shutdown runs against ONE shared deadline, kept under the pod's
// terminationGracePeriodSeconds (30 s): drain first (at most drainBudget),
// then every listener's Shutdown in parallel under whatever remains, then the
// background loops and closers. Run returns by shutdownDeadline even if a
// request or stream is still in flight (the listeners are then closed hard).
const (
	shutdownDeadline = 24 * time.Second
	drainBudget      = 10 * time.Second
)

// namedServer is one bound listener with its server and optional TLS
// configuration (nil = plain HTTP).
type namedServer struct {
	name   string
	srv    *http.Server
	ln     net.Listener
	tlsCfg *tls.Config
}

// Backend owns the four listeners (app, session, internal, metrics) and the
// lifecycle of everything behind them: store, Kubernetes cache, provisioning
// loops, broker, session gateway.
type Backend struct {
	cfg Config
	log *slog.Logger

	// ready gates /readyz on the app and session listeners: true only while
	// Run is serving AND the Workspace informer cache has synced, and cleared
	// first at shutdown so routers stop sending new work before sockets
	// drain. Derived from the three flags below by updateReady.
	ready atomic.Bool

	readyMu  sync.Mutex
	serving  bool // Run has started serving
	synced   bool // Workspace informer cache synced (or none needed)
	draining bool // shutdown has begun

	gw        *gateway.Gateway // nil when the session listener is off
	servers   []namedServer    // bound in New, served in Run
	reloaders []*tlsreload.Reloader

	// Handlers built by wire; a nil handler means the listener is off.
	appHandler      http.Handler
	sessionHandler  http.Handler
	internalHandler http.Handler
	internalTLSCfg  *tls.Config

	// bg holds the ctx-bound background loops started by Run.
	bg []func(ctx context.Context)
	// singletons are the control loops that must run in exactly one replica
	// (outbox dispatcher, sweepers, recovery, expiry planner); electSingletons
	// gates them on the leader lock.
	singletons []func(ctx context.Context)
	// nextRecoveryTick is the unix time of the next scheduled recovery
	// pass, published by the recovery singleton for the API's Retry-After
	// estimate on release-pending quota refusals. 0 = unknown.
	nextRecoveryTick atomic.Int64
	// closers run in reverse order after the servers stop (DB, caches…).
	closers []func()
}

// recoveryTickETA estimates seconds until the next recovery pass for a
// Retry-After header: ceil of the published tick time, floored at 1, and
// 30 when the pass schedule is unknown (no leader yet / single-shot mode).
func (b *Backend) recoveryTickETA() int {
	t := b.nextRecoveryTick.Load()
	if t <= 0 {
		return 30
	}
	secs := int(time.Until(time.Unix(t, 0)).Seconds()) + 1
	if secs < 1 {
		return 1
	}
	return secs
}

// New wires the backend and binds every enabled listener (pass ":0" for an
// ephemeral port; read the result back with Addrs). It does not serve — call
// Run. Nothing outside Backend construction may retain the listeners.
func New(ctx context.Context, cfg Config, log *slog.Logger) (*Backend, error) {
	if log == nil {
		log = slog.Default()
	}
	b := &Backend{cfg: cfg, log: log}
	if err := b.wire(ctx); err != nil {
		b.closeAll()
		return nil, err
	}
	return b, nil
}

// Addrs returns the bound addresses in listener order; a disabled listener
// reports "".
func (b *Backend) Addrs() (app, session, internal, metrics string) {
	for _, s := range b.servers {
		switch s.name {
		case "app":
			app = s.ln.Addr().String()
		case "session":
			session = s.ln.Addr().String()
		case "internal":
			internal = s.ln.Addr().String()
		case "metrics":
			metrics = s.ln.Addr().String()
		}
	}
	return app, session, internal, metrics
}

// Run serves until ctx is done, then shuts down in order (all under one shared
// deadline, see shutdownDeadline): stop readiness,
// drain the session gateway (disconnects reported, leases kept), graceful
// http.Server.Shutdown on every listener in parallel, stop the background
// loops, then close the broker side and DB.
// A serve failure on any listener also triggers the same shutdown path and
// is returned.
func (b *Backend) Run(ctx context.Context) error {
	defer b.closeAll()

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var bgWG sync.WaitGroup
	for _, r := range b.reloaders {
		bgWG.Add(1)
		go func() { defer bgWG.Done(); r.Run(runCtx) }()
	}
	for _, start := range b.bg {
		bgWG.Add(1)
		go func() { defer bgWG.Done(); start(runCtx) }()
	}

	type serveErr struct {
		name string
		err  error
	}
	errCh := make(chan serveErr, len(b.servers))
	for _, s := range b.servers {
		go func() {
			b.log.Info("listening", "listener", s.name, "addr", s.ln.Addr().String(),
				"tls", s.tlsCfg != nil)
			var err error
			if s.tlsCfg != nil {
				err = serveTLS(s.srv, s.ln, s.tlsCfg)
			} else {
				err = s.srv.Serve(s.ln)
			}
			if errors.Is(err, http.ErrServerClosed) {
				err = nil
			}
			errCh <- serveErr{s.name, err}
		}()
	}
	// The listeners are bound (New) and their accept loops are running.
	b.markServing()

	var serveFailure error
	select {
	case <-ctx.Done():
	case se := <-errCh:
		if se.err != nil {
			serveFailure = se.err
			b.log.Error("listener failed", "listener", se.name, "err", se.err)
		}
	}

	// One deadline for the whole shutdown, under the pod's grace period.
	shCtx, shCancel := context.WithTimeout(context.Background(), shutdownDeadline)
	defer shCancel()

	// 1. Stop readiness: /readyz now fails so routers stop sending new
	//    work before sockets close.
	b.beginShutdown()

	// 2. Drain the session gateway: close every open stream and report
	//    disconnect for each, without revoking leases — the same cookie
	//    reconnects on another replica. At most drainBudget.
	if b.gw != nil {
		drainCtx, dcancel := context.WithTimeout(shCtx, drainBudget)
		b.gw.Drain(drainCtx)
		dcancel()
	}

	// 3. Graceful shutdown of every listener, in parallel, under the time
	//    that remains.
	var shWG sync.WaitGroup
	for _, s := range b.servers {
		shWG.Add(1)
		go func() {
			defer shWG.Done()
			if err := s.srv.Shutdown(shCtx); err != nil {
				b.log.Error("listener shutdown", "listener", s.name, "err", err)
			}
		}()
	}
	shWG.Wait()

	// 4. Stop the background loops (singletons, reloaders, informer cache)
	//    and wait for them before the closers release the database; bounded
	//    by the same deadline.
	cancel()
	bgDone := make(chan struct{})
	go func() { bgWG.Wait(); close(bgDone) }()
	select {
	case <-bgDone:
	case <-shCtx.Done():
		b.log.Error("background loops did not stop before the shutdown deadline")
	}
	// 5. deferred closeAll: hard-close any listener still open, then closers.
	return serveFailure
}

// markServing records that Run is accepting connections.
func (b *Backend) markServing() { b.setReadiness(func() { b.serving = true }) }

// markCacheSynced records that the Workspace informer cache has synced.
func (b *Backend) markCacheSynced() { b.setReadiness(func() { b.synced = true }) }

// beginShutdown fails /readyz for good.
func (b *Backend) beginShutdown() { b.setReadiness(func() { b.draining = true }) }

func (b *Backend) setReadiness(mutate func()) {
	b.readyMu.Lock()
	defer b.readyMu.Unlock()
	mutate()
	b.ready.Store(b.serving && b.synced && !b.draining)
}

// closeAll releases resources acquired by New, in reverse order.
func (b *Backend) closeAll() {
	for _, s := range b.servers {
		_ = s.srv.Close()
		_ = s.ln.Close()
	}
	for i := len(b.closers) - 1; i >= 0; i-- {
		b.closers[i]()
	}
}

// readyz wraps a listener handler: /readyz answers 200 while the backend is
// serving and 503 once shutdown begins, so the chart's readiness probe drops
// the pod before the drain. /healthz always answers 200 (liveness) — even on
// the app listener, which has no health endpoint of its own. All other paths
// pass through to the real handler.
func (b *Backend) readyz(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		case "/readyz":
			w.Header().Set("Content-Type", "application/json")
			if !b.ready.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte(`{"status":"draining"}`))
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		default:
			next.ServeHTTP(w, r)
		}
	})
}
