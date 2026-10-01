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
	"sync/atomic"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/gateway"
	"github.com/tinyorbitvn/tinycdi/internal/tlsreload"
)

// shutdownBudget is the per-phase grace on shutdown: drain, then server
// Shutdown per listener.
const shutdownBudget = 10 * time.Second

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

	// ready gates /readyz on the app and session listeners; cleared first
	// at shutdown so routers stop sending new work before sockets drain.
	ready atomic.Bool

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
	// closers run in reverse order after the servers stop (DB, caches…).
	closers []func()
}

// New wires the backend and binds every enabled listener (pass ":0" for an
// ephemeral port; read the result back with Addrs). It does not serve — call
// Run. Nothing outside Backend construction may retain the listeners.
func New(ctx context.Context, cfg Config, log *slog.Logger) (*Backend, error) {
	if log == nil {
		log = slog.Default()
	}
	b := &Backend{cfg: cfg, log: log}
	b.ready.Store(true)
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

// Run serves until ctx is done, then shuts down in order: stop readiness,
// drain the session gateway (disconnects reported, leases kept), graceful
// http.Server.Shutdown on every listener, then close the broker side and DB.
// A serve failure on any listener also triggers the same shutdown path and
// is returned.
func (b *Backend) Run(ctx context.Context) error {
	defer b.closeAll()

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	for _, r := range b.reloaders {
		go r.Run(runCtx)
	}
	for _, start := range b.bg {
		go start(runCtx)
	}

	type serveErr struct {
		name string
		err  error
	}
	errCh := make(chan serveErr, len(b.servers))
	for _, s := range b.servers {
		s := s
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

	var serveFailure error
	select {
	case <-ctx.Done():
	case se := <-errCh:
		if se.err != nil {
			serveFailure = se.err
			b.log.Error("listener failed", "listener", se.name, "err", se.err)
		}
	}

	// 1. Stop readiness: /readyz now fails so routers stop sending new
	//    work before sockets close.
	b.ready.Store(false)

	// 2. Drain the session gateway: close every open stream and report
	//    disconnect for each, without revoking leases — the same cookie
	//    reconnects on another replica.
	if b.gw != nil {
		drainCtx, dcancel := context.WithTimeout(context.Background(), shutdownBudget)
		b.gw.Drain(drainCtx)
		dcancel()
	}

	// 3. Graceful shutdown of every listener.
	for _, s := range b.servers {
		shCtx, cancel := context.WithTimeout(context.Background(), shutdownBudget)
		if err := s.srv.Shutdown(shCtx); err != nil {
			b.log.Error("listener shutdown", "listener", s.name, "err", err)
		}
		cancel()
	}
	return serveFailure
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
