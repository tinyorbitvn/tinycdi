// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package tlsreload

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"errors"
	"log/slog"
	"os"
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

	cur  atomic.Pointer[x509.CertPool]
	hash [32]byte
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
	pool, hash, err := loadPool(file)
	if err != nil {
		return nil, err
	}
	p.cur.Store(pool)
	p.hash = hash
	return p, nil
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
	pool, hash, err := loadPool(p.file)
	if err != nil {
		p.log.Error("tlsreload: CA reload failed, keeping previous pool",
			"file", p.file, "err", err)
		return
	}
	if hash == p.hash {
		return
	}
	p.cur.Store(pool)
	p.hash = hash
	p.log.Info("tlsreload: reloaded CA bundle", "file", p.file)
}

func loadPool(file string) (*x509.CertPool, [32]byte, error) {
	pemBytes, err := os.ReadFile(file)
	if err != nil {
		return nil, [32]byte{}, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemBytes) {
		return nil, [32]byte{}, errors.New("tlsreload: CA file contains no PEM certificates")
	}
	return pool, sha256.Sum256(pemBytes), nil
}
