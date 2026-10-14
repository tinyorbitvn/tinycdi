// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package backend

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/observability"
	"github.com/tinyorbitvn/tinycdi/internal/ratelimit"
	"github.com/tinyorbitvn/tinycdi/internal/tlsreload"
)

// Listener timeout budgets (SEC-23). The app, internal and metrics
// listeners bound every request phase — they serve no streaming or
// WebSocket endpoints. The session listener bounds the whole request read
// (sessionReadTimeout) but not writes: net/http clears a conn's deadlines
// when a handler hijacks it (server.go hijackLocked), so an admitted
// WebSocket stream outlives the bound, and the transfer leg extends its own
// read and write deadlines after its claim (gateway transferStreamBudget).

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

// sessionReadTimeout is the session listener's whole-request read bound
// (R1-05): headers plus body must arrive within it, so an unauthenticated
// client that stalls a declared body cannot hold a conn or a serve
// goroutine. A var so tests can shorten it.
var sessionReadTimeout = 30 * time.Second

// sessionServer builds the session listener. ReadHeaderTimeout bounds
// header parsing, ReadTimeout bounds the whole request read (including the
// body drain net/http runs after a handler that left the body unread),
// IdleTimeout bounds idle keep-alive conns and MaxHeaderBytes caps header
// size. WriteTimeout stays zero: proxied desktop documents and downloads
// stream for as long as the client reads.
func sessionServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       sessionReadTimeout,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
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

// errClientCARotated refuses a handshake whose chain verified against a
// client-CA bundle that was swapped out before the conn registered.
var errClientCARotated = errors.New("internal mTLS: client CA bundle rotated during the handshake")

// clientConns is the internal listener's live verified-conn registry
// (TM-38). Every conn whose client certificate verified is recorded with
// the chain it presented; a client-CA swap re-verifies each one against
// the new bundle and closes those that no longer verify, so rotating a CA
// out ends its established connections as well as new handshakes.
type clientConns struct {
	current func() *x509.CertPool

	// mu serializes registration against the swap walk: a conn verified
	// against the old bundle either fails track's re-verification or is
	// in the map when the walk runs.
	mu    sync.Mutex
	conns map[net.Conn][]*x509.Certificate
}

// newClientConns builds the registry over pool and subscribes it to the
// pool's swaps.
func newClientConns(pool *tlsreload.CAPool) *clientConns {
	c := &clientConns{current: pool.Pool, conns: map[net.Conn][]*x509.Certificate{}}
	pool.Subscribe(c.sever)
	return c
}

// verifyClientChain reports whether chain (leaf first, as presented)
// verifies as a client certificate against roots.
func verifyClientChain(chain []*x509.Certificate, roots *x509.CertPool) bool {
	inter := x509.NewCertPool()
	for _, c := range chain[1:] {
		inter.AddCert(c)
	}
	_, err := chain[0].Verify(x509.VerifyOptions{
		Roots:         roots,
		Intermediates: inter,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	})
	return err == nil
}

// track re-verifies chain against the current bundle and registers conn.
// A nil conn checks without registering.
func (c *clientConns) track(conn net.Conn, chain []*x509.Certificate) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !verifyClientChain(chain, c.current()) {
		return errClientCARotated
	}
	if conn != nil {
		c.conns[conn] = chain
	}
	return nil
}

// sever closes every registered conn whose chain pool no longer verifies.
func (c *clientConns) sever(pool *x509.CertPool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for conn, chain := range c.conns {
		if !verifyClientChain(chain, pool) {
			_ = conn.Close()
			delete(c.conns, conn)
		}
	}
}

func (c *clientConns) drop(conn net.Conn) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.conns, conn)
}

// tracked reports the registry size.
func (c *clientConns) tracked() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.conns)
}

// installClientConnTracking wraps cfg.GetConfigForClient so every
// per-handshake clone registers its verified conn in conns (after any
// VerifyConnection already on the clone). Absent certs pass untouched:
// they reach the handler's 401.
func installClientConnTracking(cfg *tls.Config, conns *clientConns) {
	inner := cfg.GetConfigForClient
	if inner == nil {
		inner = func(*tls.ClientHelloInfo) (*tls.Config, error) {
			return cfg.Clone(), nil
		}
	}
	cfg.GetConfigForClient = func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
		c, err := inner(hello)
		if err != nil {
			return nil, err
		}
		prev := c.VerifyConnection
		c.VerifyConnection = func(cs tls.ConnectionState) error {
			if prev != nil {
				if err := prev(cs); err != nil {
					return err
				}
			}
			if len(cs.PeerCertificates) == 0 {
				return nil
			}
			return conns.track(hello.Conn, cs.PeerCertificates)
		}
		return c, nil
	}
}

const (
	// revokedLogPerMinute is the sustained warn-line budget for the
	// revoked-serial rejection log (1/second) and revokedLogBurst its
	// bucket capacity — the same token-budget shape httpapi's rejectAuth
	// uses (ADR 0008). The single key is revokedWarnKey.
	revokedLogPerMinute = 60
	revokedLogBurst     = 10
	// revokedWarnKey is the rate-limit key of the rejection warn line —
	// one reason class ("revoked_serial"), matching rejectAuth's keying.
	revokedWarnKey = "revoked_serial"
)

// installRevocation wraps cfg.GetConfigForClient (ADR 0008, SR-3-F5):
// every per-handshake clone carries a VerifyConnection that aborts the
// handshake when the verified peer leaf's (issuer, serial) is on the
// blocklist, and registers every verified conn in the blocklist's
// live-conn registry so a reload severs established connections on a
// newly blocked serial. VerifyConnection also runs on resumed handshakes
// (Go repopulates PeerCertificates from the session state), so a resumed
// conn is checked and registered too. A VerifyConnection already on the
// clone (installClientConnTracking's) runs after a passing check. It is
// only installed when the feature is configured — with the flag unset no
// revocation hook is installed; the client-CA conn tracking
// (installClientConnTracking) still runs.
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
		next := c.VerifyConnection
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
				if next != nil {
					return next(cs)
				}
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

// internalConnState returns the internal listener's ConnState hook: it
// drops a conn from every live-conn registry (the client-CA registry and
// the ADR 0008 blocklist, either may be nil) when it leaves the server
// (StateClosed/StateHijacked), so neither registry can leak. Both key on
// the raw conn the handshake saw; the server's conn is the *tls.Conn
// wrapper, unwrapped here.
func internalConnState(conns *clientConns, bl *tlsreload.Blocklist) func(net.Conn, http.ConnState) {
	return func(c net.Conn, st http.ConnState) {
		if st != http.StateClosed && st != http.StateHijacked {
			return
		}
		if tc, ok := c.(*tls.Conn); ok {
			c = tc.NetConn()
		}
		if conns != nil {
			conns.drop(c)
		}
		if bl != nil {
			bl.Drop(c)
		}
	}
}

// connStateDrop is internalConnState for the blocklist alone.
func connStateDrop(bl *tlsreload.Blocklist) func(net.Conn, http.ConnState) {
	return internalConnState(nil, bl)
}
