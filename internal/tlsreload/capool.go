// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package tlsreload

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"log/slog"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

// CAPool serves the newest CA bundle found at file. Like Reloader it
// detects rotation by content hash, so atomic Secret-volume swaps are
// picked up; a bundle that stops parsing keeps the previous pool so a
// half-applied rotation never drops all trust.
type CAPool struct {
	file string
	options

	cur   atomic.Pointer[x509.CertPool]
	certs atomic.Pointer[[]*x509.Certificate]
	hash  [32]byte

	mu   sync.Mutex
	subs []func(*x509.CertPool)
}

// NewCAPool loads the bundle once and fails if it is unusable.
func NewCAPool(file string, opts ...Option) (*CAPool, error) {
	p := &CAPool{
		file:    file,
		options: options{interval: 30 * time.Second, log: slog.Default()},
	}
	for _, o := range opts {
		o(&p.options)
	}
	pool, certs, hash, err := loadPool(file)
	if err != nil {
		return nil, err
	}
	p.cur.Store(pool)
	p.certs.Store(&certs)
	p.hash = hash
	return p, nil
}

// Certs returns the newest parsed bundle's certificates (the blocklist's
// known-issuer check reads it).
func (p *CAPool) Certs() []*x509.Certificate {
	return *p.certs.Load()
}

// Subscribe registers fn to run with the new pool after every swap to a
// changed bundle — the internal listener severs live connections whose
// chain the new bundle no longer verifies (TM-38).
func (p *CAPool) Subscribe(fn func(*x509.CertPool)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.subs = append(p.subs, fn)
}

// Pool returns the newest parsed pool. Callers re-read it per use — the
// internal listener pulls it inside tls.Config.GetConfigForClient so each
// handshake verifies against the current bundle (E5).
func (p *CAPool) Pool() *x509.CertPool {
	return p.cur.Load()
}

// Run re-reads the file every interval until ctx is done. A failed reload
// keeps the previous pool and logs the error.
func (p *CAPool) Run(ctx context.Context) {
	t := time.NewTicker(p.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.reload()
		}
	}
}

func (p *CAPool) reload() {
	pool, certs, hash, err := loadPool(p.file)
	if err != nil {
		p.log.Error("tlsreload: CA reload failed, keeping previous pool",
			"file", p.file, "err", err)
		return
	}
	if hash == p.hash {
		return
	}
	p.cur.Store(pool)
	p.certs.Store(&certs)
	p.hash = hash
	p.log.Info("tlsreload: reloaded CA bundle", "file", p.file)
	p.mu.Lock()
	subs := slices.Clone(p.subs)
	p.mu.Unlock()
	for _, fn := range subs {
		fn(pool)
	}
}

func loadPool(file string) (*x509.CertPool, []*x509.Certificate, [32]byte, error) {
	pemBytes, err := os.ReadFile(file)
	if err != nil {
		return nil, nil, [32]byte{}, err
	}
	pool := x509.NewCertPool()
	var certs []*x509.Certificate
	rest := pemBytes
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" || len(block.Headers) != 0 {
			continue // the blocks AppendCertsFromPEM skips
		}
		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			continue
		}
		pool.AddCert(c)
		certs = append(certs, c)
	}
	if len(certs) == 0 {
		return nil, nil, [32]byte{}, errors.New("tlsreload: CA file contains no PEM certificates")
	}
	return pool, certs, sha256.Sum256(pemBytes), nil
}
