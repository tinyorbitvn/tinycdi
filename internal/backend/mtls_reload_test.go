// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package backend

// E5 tests: the internal listener's client-CA bundle hot-reloads — a CA
// added to the file starts accepting its clients and a removed CA stops
// being trusted, all without a restart.

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/broker/httpapi"
	"github.com/tinyorbitvn/tinycdi/internal/tlsreload"
)

// serveInternalTLS binds the production internal-listener stack on :0
// with the mTLS config wire.go builds: ServerTLSConfig plus the
// per-handshake CA pool read (hotReloadClientCAs). The handler answers
// 200 only to a verified client cert — absent or rejected certs get 401,
// mirroring the listener's error model. Returns the bound address.
func serveInternalTLS(t *testing.T, srvCert tls.Certificate, caPEM []byte, pool *tlsreload.CAPool) string {
	t.Helper()
	tlsCfg, err := httpapi.ServerTLSConfig(srvCert, caPEM)
	if err != nil {
		t.Fatalf("ServerTLSConfig: %v", err)
	}
	hotReloadClientCAs(tlsCfg, pool)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := internalServer(ln.Addr().String(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(r.TLS.PeerCertificates) == 0 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	go func() { _ = serveTLS(srv, ln, tlsCfg) }()
	t.Cleanup(func() { _ = srv.Close() })
	return ln.Addr().String()
}

// callMTLS issues one HTTPS request presenting cert on a fresh
// connection. It reports true only when the server answered 200, i.e.
// the client certificate was verified. A rejected cert surfaces as a
// transport error (TLS 1.3 delivers the alert asynchronously, so
// tls.Dial alone cannot see it); a filtered-out cert reaches the handler
// with no peer certificate and gets 401.
func callMTLS(addr string, cert tls.Certificate) bool {
	hc := &http.Client{
		Timeout: 2 * time.Second,
		Transport: &http.Transport{
			DisableKeepAlives: true,
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true, // test only: the assertion is the server-side client-CA check
				Certificates:       []tls.Certificate{cert},
			},
		},
	}
	resp, err := hc.Get("https://" + addr + "/")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// writeFileAtomic writes data via a temp file + rename, mirroring the
// atomic swap Kubernetes Secret volumes perform.
func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func eventuallyCall(t *testing.T, d time.Duration, fn func() bool, want bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if fn() == want {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("condition not met before deadline (want=%v)", want)
}

// TestInternalListener_AcceptsNewCAAfterRotation (E5): a client cert
// signed by CA2 is refused, then accepted after the CA file gains CA2,
// with no restart.
func TestInternalListener_AcceptsNewCAAfterRotation(t *testing.T) {
	dir := t.TempDir()
	ca1 := newTestCA(t)
	ca2 := newTestCA(t)
	caFile := filepath.Join(dir, "clients-ca.pem")
	if err := writeFileAtomic(caFile, ca1.pem); err != nil {
		t.Fatal(err)
	}
	pool, err := tlsreload.NewCAPool(caFile, tlsreload.WithInterval(10*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go pool.Run(ctx)

	cf, kf := writeTestCert(t, dir, "internal.test")
	srvCert, err := tls.LoadX509KeyPair(cf, kf)
	if err != nil {
		t.Fatal(err)
	}
	addr := serveInternalTLS(t, srvCert, ca1.pem, pool)

	client2 := ca2.sign(t, "operator")
	if callMTLS(addr, client2) {
		t.Fatal("CA2 client accepted before the CA file gained CA2")
	}

	bundle := append(append([]byte(nil), ca1.pem...), ca2.pem...)
	if err := writeFileAtomic(caFile, bundle); err != nil {
		t.Fatal(err)
	}
	eventuallyCall(t, 5*time.Second, func() bool { return callMTLS(addr, client2) }, true)
}

// TestInternalListener_DropsRemovedCA (E5): after CA1 leaves the file, a
// CA1 client cert fails the handshake.
func TestInternalListener_DropsRemovedCA(t *testing.T) {
	dir := t.TempDir()
	ca1 := newTestCA(t)
	ca2 := newTestCA(t)
	caFile := filepath.Join(dir, "clients-ca.pem")
	bundle := append(append([]byte(nil), ca1.pem...), ca2.pem...)
	if err := writeFileAtomic(caFile, bundle); err != nil {
		t.Fatal(err)
	}
	pool, err := tlsreload.NewCAPool(caFile, tlsreload.WithInterval(10*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go pool.Run(ctx)

	cf, kf := writeTestCert(t, dir, "internal.test")
	srvCert, err := tls.LoadX509KeyPair(cf, kf)
	if err != nil {
		t.Fatal(err)
	}
	addr := serveInternalTLS(t, srvCert, bundle, pool)

	client1 := ca1.sign(t, "operator")
	if !callMTLS(addr, client1) {
		t.Fatal("CA1 client refused before rotation")
	}

	if err := writeFileAtomic(caFile, ca2.pem); err != nil {
		t.Fatal(err)
	}
	eventuallyCall(t, 5*time.Second, func() bool { return callMTLS(addr, client1) }, false)
}
