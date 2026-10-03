// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package backend

import (
	"crypto/tls"
	"net"
	"net/http"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/tlsreload"
)

// Listener timeout budgets (SEC-23). The app, internal and metrics
// listeners bound every request phase — they serve no streaming or
// WebSocket endpoints. The session listener carries hijacked WebSocket
// desktop streams, so its Read/WriteTimeout must stay zero: Go applies
// them as absolute conn deadlines that survive Hijack().

// appServer builds the public API listener.
func appServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}
}

// sessionServer builds the session listener. ReadHeaderTimeout bounds
// header parsing, IdleTimeout bounds idle keep-alive conns and
// MaxHeaderBytes caps header size; Read/WriteTimeout stay zero for the
// hijacked desktop streams.
func sessionServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
}

// internalServer builds the mTLS broker listener; same bounded budget as
// the app server.
func internalServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}
}

// metricsServer builds the metrics listener: scrape-only, so every request
// phase is bounded.
func metricsServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
}

// serveTLS serves srv on ln wrapped in tlsCfg. The tls.Config's
// GetCertificate is expected to come from a tlsreload.Reloader so cert
// rotation takes effect on the next handshake without a restart (D21).
func serveTLS(srv *http.Server, ln net.Listener, tlsCfg *tls.Config) error {
	return srv.Serve(tls.NewListener(ln, tlsCfg))
}

// hotReloadClientCAs installs a GetConfigForClient on cfg that clones it
// per handshake with pool's current bundle as ClientCAs (E5): rotating
// the client-CA file adds and removes trust without a restart. The static
// ClientCAs on cfg is the boot-time bundle only.
func hotReloadClientCAs(cfg *tls.Config, pool *tlsreload.CAPool) {
	cfg.GetConfigForClient = func(*tls.ClientHelloInfo) (*tls.Config, error) {
		c := cfg.Clone()
		c.ClientCAs = pool.Pool()
		return c, nil
	}
}
