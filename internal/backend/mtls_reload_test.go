// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package backend

// E5 tests: the internal listener's client-CA bundle hot-reloads — a CA
// added to the file starts accepting its clients and a removed CA stops
// being trusted, all without a restart.

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/broker/httpapi"
	"github.com/tinyorbitvn/tinycdi/internal/observability"
	"github.com/tinyorbitvn/tinycdi/internal/tlsreload"
)

// serveInternalTLS binds the production internal-listener stack on :0
// with the mTLS config wire.go builds: ServerTLSConfig plus the
// per-handshake CA pool read (hotReloadClientCAs). The handler answers
// 200 only to a verified client cert — absent or rejected certs get 401,
// mirroring the listener's error model. Returns the bound address.
func serveInternalTLS(t *testing.T, srvCert tls.Certificate, caPEM []byte, pool *tlsreload.CAPool) string {
	return serveInternalTLSList(t, srvCert, caPEM, pool, nil, nil, nil)
}

// serveInternalTLSList is serveInternalTLS plus the ADR 0008 leg: when bl
// is non-nil the listener installs the revocation VerifyConnection
// (installRevocation) and the ConnState drop hook, plus the TM-38
// client-CA conn registry, exactly as bind() wires them, and, when log or
// metrics is non-nil, the TM-40 handshake ErrorLog.
func serveInternalTLSList(t *testing.T, srvCert tls.Certificate, caPEM []byte, pool *tlsreload.CAPool,
	bl *tlsreload.Blocklist, log *slog.Logger, metrics *observability.Metrics) string {
	t.Helper()
	tlsCfg, err := httpapi.ServerTLSConfig(srvCert, caPEM)
	if err != nil {
		t.Fatalf("ServerTLSConfig: %v", err)
	}
	hotReloadClientCAs(tlsCfg, pool)
	conns := newClientConns(pool)
	installClientConnTracking(tlsCfg, conns)
	if bl != nil {
		installRevocation(tlsCfg, bl, log, metrics)
	}
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
	srv.ConnState = internalConnState(conns, bl)
	if log != nil || metrics != nil {
		srv.ErrorLog = internalErrorLog(log, metrics)
	}
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

// waitSevered polls conn until a read fails with something other than a
// deadline timeout (EOF or reset: the server closed it); it fails the
// test after d. A timeout means the conn is still open and idle.
func waitSevered(t *testing.T, conn net.Conn, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	buf := make([]byte, 1)
	for time.Now().Before(deadline) {
		_ = conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		_, err := conn.Read(buf)
		var ne net.Error
		if err != nil && !(errors.As(err, &ne) && ne.Timeout()) {
			return
		}
	}
	t.Fatal("established conn was not severed")
}

// TestInternalListener_RemovedCASeversLiveConn (R1-30, TM-38): when CA1
// leaves the client-CA bundle, an established keep-alive conn
// authenticated by a CA1 cert is closed on the same reload pass — a
// rotation must not leave a stolen cert's live conn served — while a conn
// from the still-trusted CA2 keeps answering.
func TestInternalListener_RemovedCASeversLiveConn(t *testing.T) {
	dir := t.TempDir()
	ca1 := newTestCANamed(t, "internal-ca-1")
	ca2 := newTestCANamed(t, "internal-ca-2")
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

	conn1 := dialMTLS(t, addr, clientTLSCfg(ca1.sign(t, "operator")))
	br1 := bufio.NewReader(conn1)
	conn2 := dialMTLS(t, addr, clientTLSCfg(ca2.sign(t, "operator")))
	br2 := bufio.NewReader(conn2)
	if mtlsGet(t, conn1, br1) != http.StatusOK || mtlsGet(t, conn2, br2) != http.StatusOK {
		t.Fatal("setup request failed")
	}

	if err := writeFileAtomic(caFile, ca2.pem); err != nil {
		t.Fatal(err)
	}
	waitSevered(t, conn1, 5*time.Second)
	if mtlsGet(t, conn2, br2) != http.StatusOK {
		t.Fatal("conn from the still-trusted CA2 was severed")
	}
	_ = conn2.Close()
}
