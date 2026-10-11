// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package backend

import (
	"crypto/tls"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/observability"
	"github.com/tinyorbitvn/tinycdi/internal/ratelimit"
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

const (
	// revokedLogPerMinute is the sustained warn-line budget for the
	// revoked-serial rejection log (1/second) and revokedLogBurst its
	// bucket capacity — the same token-budget shape httpapi's rejectAuth
	// uses (ADR 0009). The single key is revokedWarnKey.
	revokedLogPerMinute = 60
	revokedLogBurst     = 10
	// revokedWarnKey is the rate-limit key of the rejection warn line —
	// one reason class ("revoked_serial"), matching rejectAuth's keying.
	revokedWarnKey = "revoked_serial"
)

// installRevocation wraps cfg.GetConfigForClient (ADR 0009, SR-3-F5):
// every per-handshake clone carries a VerifyConnection that aborts the
// handshake when the verified peer leaf's (issuer, serial) is on the
// blocklist, and registers every verified conn in the blocklist's
// live-conn registry so a reload severs established connections on a
// newly blocked serial. VerifyConnection also runs on resumed handshakes
// (Go repopulates PeerCertificates from the session state), so a resumed
// conn is checked and registered too. It is only installed when the
// feature is configured — with no blocklist the clone is byte-identical
// to today (no VerifyConnection).
func installRevocation(cfg *tls.Config, bl *tlsreload.Blocklist, log *slog.Logger, metrics *observability.Metrics) {
	inner := cfg.GetConfigForClient
	if inner == nil {
		inner = func(*tls.ClientHelloInfo) (*tls.Config, error) {
			return cfg.Clone(), nil
		}
	}
	warn := ratelimit.New(revokedLogPerMinute, revokedLogBurst, 8, nil)
	cfg.GetConfigForClient = func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
		c, err := inner(hello)
		if err != nil {
			return nil, err
		}
		c.VerifyConnection = func(cs tls.ConnectionState) error {
			// VerifyClientCertIfGiven lets an absent cert through — the
			// request then reaches identify and answers the listener's
			// 401. The check no-ops on empty PeerCertificates.
			if len(cs.PeerCertificates) == 0 {
				return nil
			}
			leaf := cs.PeerCertificates[0]
			err := bl.Track(hello.Conn, leaf)
			if err == nil {
				return nil
			}
			if metrics != nil {
				metrics.IncInternalRevokedSerial("internal")
			}
			if log != nil {
				if ok, _ := warn.Allow(revokedWarnKey); ok {
					peer := ""
					if hello.Conn != nil {
						peer = hello.Conn.RemoteAddr().String()
					}
					serial := ""
					if leaf.SerialNumber != nil {
						serial = leaf.SerialNumber.Text(16)
					}
					log.Warn("internal_mtls_revoked_cert",
						"peer", peer,
						"serial", serial,
						"issuer", leaf.Issuer.String())
				}
			}
			return err
		}
		return c, nil
	}
}

// connStateDrop returns the http.Server ConnState hook that drops a
// conn's blocklist registration when it leaves the server
// (StateClosed/StateHijacked), so the live-conn registry cannot leak. The
// registry keys on the raw conn the handshake saw; the server's conn is
// the *tls.Conn wrapper, unwrapped here.
func connStateDrop(bl *tlsreload.Blocklist) func(net.Conn, http.ConnState) {
	return func(c net.Conn, st http.ConnState) {
		if st != http.StateClosed && st != http.StateHijacked {
			return
		}
		if tc, ok := c.(*tls.Conn); ok {
			c = tc.NetConn()
		}
		bl.Drop(c)
	}
}
