// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package opclient_test

// Server-CA hot-reload tests (FX-R33): the operator verifies the broker's
// internal listener against the CA bundle file's newest parsed contents,
// so rotating the internal CA never needs a client restart.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/broker/httpapi"
	"github.com/tinyorbitvn/tinycdi/internal/broker/httpapi/opclient"
)

// caPEM returns the PEM encoding of the pki's CA certificate.
func (p *pki) caPEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: p.caCert.Raw})
}

// fileClient wires an httptest server (TLS via srvCfg) plus an opclient
// built from on-disk material, with both reload loops running against a
// 10 ms interval. clientCA issues the operator's client certificate;
// caPEM is the initial server-CA bundle. Returns the server, the client
// and the CA bundle path for rotations.
func fileClient(t *testing.T, clientCA *pki, caPEM []byte, srvCfg *tls.Config) (*httptest.Server, *opclient.Client, string) {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"openStreams":0,"drained":true}`))
	}))
	srv.TLS = srvCfg
	srv.StartTLS()
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	caFile := filepath.Join(dir, "ca.pem")
	certFile := filepath.Join(dir, "tls.crt")
	keyFile := filepath.Join(dir, "tls.key")
	writeAtomic(t, caFile, caPEM)
	writeClientPair(t, certFile, keyFile, clientCA.issue(t, httpapi.DefaultOperatorCN, false))

	c, err := opclient.New(opclient.Config{
		BaseURL:        srv.URL,
		CertFile:       certFile,
		KeyFile:        keyFile,
		CAFile:         caFile,
		ReloadInterval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("opclient.New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go c.Run(ctx)
	return srv, c, caFile
}

// pollDrain forces a fresh handshake per attempt (CloseClientConnections)
// until want(err) holds; on the 5 s deadline it fails the test. The last
// error is returned for further assertions.
func pollDrain(t *testing.T, srv *httptest.Server, c *opclient.Client, want func(error) bool, what string) error {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		srv.CloseClientConnections()
		_, _, err := c.DrainStatus(context.Background(), "ws-1")
		if want(err) {
			return err
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: last err = %v", what, err)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// TestOpClient_ReloadsServerCA: after the CA bundle gains CA2 and the
// listener re-issues under it, a new handshake succeeds with no client
// restart.
func TestOpClient_ReloadsServerCA(t *testing.T) {
	p1, p2 := newPKI(t), newPKI(t)
	var srvCert atomic.Value // tls.Certificate
	srvCert.Store(p1.issue(t, "broker-internal", true))
	// GetConfigForClient (not GetCertificate): IP dialing sends no SNI, so
	// GetCertificate would fall through to httptest's default certificate.
	srv, c, caFile := fileClient(t, p1, p1.caPEM(), &tls.Config{
		GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
			return &tls.Config{
				Certificates: []tls.Certificate{srvCert.Load().(tls.Certificate)},
				ClientAuth:   tls.VerifyClientCertIfGiven,
				ClientCAs:    p1.pool,
			}, nil
		},
	})

	if _, _, err := c.DrainStatus(context.Background(), "ws-1"); err != nil {
		t.Fatalf("drain before rotation: %v", err)
	}

	// Rotate: bundle gains CA2, then the listener serves a CA2 cert.
	bundle := append(append([]byte{}, p1.caPEM()...), p2.caPEM()...)
	writeAtomic(t, caFile, bundle)
	srvCert.Store(p2.issue(t, "broker-internal", true))
	pollDrain(t, srv, c, func(err error) bool { return err == nil },
		"server cert re-issued under CA2 was never accepted")
}

// TestOpClient_DropsRemovedServerCA: once CA1 leaves the bundle, a server
// still presenting a CA1 certificate is refused.
func TestOpClient_DropsRemovedServerCA(t *testing.T) {
	p1, p2 := newPKI(t), newPKI(t)
	bundle := append(append([]byte{}, p1.caPEM()...), p2.caPEM()...)
	srv, c, caFile := fileClient(t, p1, bundle, &tls.Config{
		Certificates: []tls.Certificate{p1.issue(t, "broker-internal", true)},
		ClientAuth:   tls.VerifyClientCertIfGiven,
		ClientCAs:    p1.pool,
	})

	if _, _, err := c.DrainStatus(context.Background(), "ws-1"); err != nil {
		t.Fatalf("drain before CA removal: %v", err)
	}

	writeAtomic(t, caFile, p2.caPEM())
	err := pollDrain(t, srv, c, func(err error) bool { return err != nil },
		"old-CA server cert still accepted after CA1 left the bundle")
	var oe *opclient.Error
	if !errors.As(err, &oe) || oe.Code != "TRANSPORT" {
		t.Fatalf("refusal err = %v, want opclient TRANSPORT", err)
	}
}

// TestOpClient_RejectsServerHostnameMismatch: a certificate chained to the
// trusted CA but issued for a different name is refused — verification is
// not skipped.
func TestOpClient_RejectsServerHostnameMismatch(t *testing.T) {
	p := newPKI(t)
	_, c, _ := fileClient(t, p, p.caPEM(), &tls.Config{
		Certificates: []tls.Certificate{p.issueSAN(t, "broker-internal",
			[]x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
			[]string{"not-this-listener.internal"}, nil)},
		ClientAuth: tls.VerifyClientCertIfGiven,
		ClientCAs:  p.pool,
	})

	_, _, err := c.DrainStatus(context.Background(), "ws-1")
	var oe *opclient.Error
	if !errors.As(err, &oe) || oe.Code != "TRANSPORT" {
		t.Fatalf("hostname-mismatched handshake err = %v, want opclient TRANSPORT", err)
	}
}

// TestOpClient_KeepsPoolOnBadCABundle: a half-written bundle keeps the
// previous pool — the reload loop logs and skips the bad read.
func TestOpClient_KeepsPoolOnBadCABundle(t *testing.T) {
	p := newPKI(t)
	srv, c, caFile := fileClient(t, p, p.caPEM(), &tls.Config{
		Certificates: []tls.Certificate{p.issue(t, "broker-internal", true)},
		ClientAuth:   tls.VerifyClientCertIfGiven,
		ClientCAs:    p.pool,
	})

	if _, _, err := c.DrainStatus(context.Background(), "ws-1"); err != nil {
		t.Fatalf("drain before bad bundle write: %v", err)
	}
	writeAtomic(t, caFile, []byte("-----BEGIN CERTIFICATE-----\ntruncated"))
	time.Sleep(150 * time.Millisecond) // several 10 ms reload ticks

	srv.CloseClientConnections()
	if _, _, err := c.DrainStatus(context.Background(), "ws-1"); err != nil {
		t.Fatalf("drain after bad bundle write: %v", err)
	}
}

// TestNew_RejectsBaseURLWithoutHostname: a BaseURL that parses but carries
// no hostname (e.g. "https://:9443") is rejected — otherwise x509 would
// treat the empty DNSName as "skip the name check".
func TestNew_RejectsBaseURLWithoutHostname(t *testing.T) {
	for _, u := range []string{"https://:9443", "https://user@:9443"} {
		if _, err := opclient.New(opclient.Config{BaseURL: u}); err == nil {
			t.Fatalf("BaseURL %q accepted without a hostname", u)
		}
	}
}
