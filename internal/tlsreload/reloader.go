// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

// Package tlsreload serves a TLS certificate from disk and reloads it when
// the underlying files change. Detection hashes file contents rather than
// comparing mtimes: Kubernetes Secret volumes swap a symlink atomically and
// mtime can move backwards across the swap.
package tlsreload

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"log/slog"
	"os"
	"sync/atomic"
	"time"
)

// Reloader serves the newest certificate found at certFile/keyFile.
type Reloader struct {
	certFile string
	keyFile  string
	interval time.Duration
	log      *slog.Logger

	cur  atomic.Pointer[tls.Certificate]
	hash [32]byte
}

// Option configures a Reloader.
type Option func(*Reloader)

// WithInterval sets how often the files are checked (default 30s).
func WithInterval(d time.Duration) Option {
	return func(r *Reloader) { r.interval = d }
}

// WithLogger reports reload successes and failures.
func WithLogger(l *slog.Logger) Option {
	return func(r *Reloader) { r.log = l }
}

// New loads the pair once and fails if it is unusable.
func New(certFile, keyFile string, opts ...Option) (*Reloader, error) {
	r := &Reloader{
		certFile: certFile,
		keyFile:  keyFile,
		interval: 30 * time.Second,
		log:      slog.Default(),
	}
	for _, o := range opts {
		o(r)
	}
	cert, hash, err := loadPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	r.cur.Store(cert)
	r.hash = hash
	return r, nil
}

// GetCertificate is assigned to tls.Config.GetCertificate.
func (r *Reloader) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return r.cur.Load(), nil
}

// GetClientCertificate is assigned to tls.Config.GetClientCertificate.
func (r *Reloader) GetClientCertificate(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
	return r.cur.Load(), nil
}

// Run re-reads the files every interval until ctx is done. A failed reload
// keeps the previous certificate and logs the error.
func (r *Reloader) Run(ctx context.Context) {
	t := time.NewTicker(r.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.reload()
		}
	}
}

func (r *Reloader) reload() {
	cert, hash, err := loadPair(r.certFile, r.keyFile)
	if err != nil {
		r.log.Error("tlsreload: reload failed, keeping previous certificate",
			"certFile", r.certFile, "keyFile", r.keyFile, "err", err)
		return
	}
	if hash == r.hash {
		return
	}
	r.cur.Store(cert)
	r.hash = hash
	r.log.Info("tlsreload: reloaded certificate", "certFile", r.certFile, "keyFile", r.keyFile)
}

func loadPair(certFile, keyFile string) (*tls.Certificate, [32]byte, error) {
	certPEM, err := os.ReadFile(certFile)
	if err != nil {
		return nil, [32]byte{}, err
	}
	keyPEM, err := os.ReadFile(keyFile)
	if err != nil {
		return nil, [32]byte{}, err
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, [32]byte{}, err
	}
	return &cert, sha256.Sum256(append(certPEM, keyPEM...)), nil
}
