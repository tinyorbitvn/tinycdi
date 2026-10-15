// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package backend

// ADR 0008 (SR-3-F5) tests: per-certificate revocation on the internal
// mTLS listener. A blocked (issuer, serial) aborts the handshake — the
// client sees a transport error, not the listener's 401 — sibling certs
// pass, resumed handshakes are checked too, and a live conn on a newly
// blocked serial is severed on the reload swap. With -internal-revoked-list
// unset nothing is installed and the wire is byte-identical.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/tinyorbitvn/tinycdi/internal/broker/httpapi"
	"github.com/tinyorbitvn/tinycdi/internal/observability"
	"github.com/tinyorbitvn/tinycdi/internal/tlsreload"
)

// revokedEntryLine renders the blocklist line for a leaf cert:
// "<serial-hex> <issuer-dn>".
func revokedEntryLine(t *testing.T, cert tls.Certificate) string {
	t.Helper()
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	return leaf.SerialNumber.Text(16) + " " + leaf.Issuer.String()
}

// newInternalRevocation builds the CAPool + Blocklist pair wire.go builds
// and runs both, at test poll intervals.
func newInternalRevocation(t *testing.T, dir string, caPEM, list []byte) (*tlsreload.CAPool, *tlsreload.Blocklist) {
	t.Helper()
	caFile := filepath.Join(dir, "clients-ca.pem")
	if err := writeFileAtomic(caFile, caPEM); err != nil {
		t.Fatal(err)
	}
	pool, err := tlsreload.NewCAPool(caFile, tlsreload.WithInterval(10*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	listFile := filepath.Join(dir, "revoked-serials.txt")
	if err := writeFileAtomic(listFile, list); err != nil {
		t.Fatal(err)
	}
	bl, err := tlsreload.NewBlocklist(listFile, tlsreload.WithInterval(10*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go pool.Run(ctx)
	go bl.Run(ctx)
	return pool, bl
}

func blocklistPath(dir string) string { return filepath.Join(dir, "revoked-serials.txt") }

// dialMTLS opens one raw TLS conn presenting cert. The caller drives
// HTTP/1.1 over it by hand (mtlsGet) — a real conn is needed to assert
// keep-alive severing and session resumption, which http.Client's
// abstraction hides.
func dialMTLS(t *testing.T, addr string, cfg *tls.Config) *tls.Conn {
	t.Helper()
	conn, err := tls.Dial("tcp", addr, cfg)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return conn
}

// mtlsGet issues one HTTP/1.1 GET on conn and returns the status code.
// The reader must persist across calls on one conn (bufio may buffer).
func mtlsGet(t *testing.T, conn *tls.Conn, br *bufio.Reader) int {
	t.Helper()
	if _, err := io.WriteString(conn, "GET / HTTP/1.1\r\nHost: internal.test\r\n\r\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

func clientTLSCfg(cert tls.Certificate) *tls.Config {
	return &tls.Config{ // test only: the assertion is the server-side check
		InsecureSkipVerify: true,
		Certificates:       []tls.Certificate{cert},
		MinVersion:         tls.VersionTLS13,
	}
}

// TestRevokedSerialHandshakeAborts: a leaf whose (issuer, serial) is
// listed dies at the handshake — the client sees a transport error, never
// the listener's 401 — while sibling certs of the same CA still pass.
func TestRevokedSerialHandshakeAborts(t *testing.T) {
	dir := t.TempDir()
	ca := newTestCANamed(t, "internal-ca")
	blocked := ca.signSerial(t, "operator", 0x4f8b)
	open := ca.signSerial(t, "operator", 0x99)

	pool, bl := newInternalRevocation(t, dir, ca.pem,
		[]byte(revokedEntryLine(t, blocked)+"\n"))

	cf, kf := writeTestCert(t, dir, "internal.test")
	srvCert, err := tls.LoadX509KeyPair(cf, kf)
	if err != nil {
		t.Fatal(err)
	}
	addr := serveInternalTLSList(t, srvCert, ca.pem, pool, bl, nil, nil)

	if callMTLS(addr, blocked) {
		t.Fatal("blocked (issuer, serial) was accepted")
	}
	if !callMTLS(addr, open) {
		t.Fatal("unblocked sibling serial was refused")
	}
}

// TestRevokedSerialIssuerPairing: the same serial under a different
// issuer does NOT match — serials are unique per issuer only, and the
// client-CA bundle may legitimately carry several CAs during rotation.
func TestRevokedSerialIssuerPairing(t *testing.T) {
	dir := t.TempDir()
	ca1 := newTestCANamed(t, "internal-ca-one")
	ca2 := newTestCANamed(t, "internal-ca-two")
	blocked := ca1.signSerial(t, "operator", 0x4f8b)
	sameSerialOtherIssuer := ca2.signSerial(t, "operator", 0x4f8b)

	bundle := append(append([]byte(nil), ca1.pem...), ca2.pem...)
	pool, bl := newInternalRevocation(t, dir, bundle,
		[]byte(revokedEntryLine(t, blocked)+"\n"))

	cf, kf := writeTestCert(t, dir, "internal.test")
	srvCert, err := tls.LoadX509KeyPair(cf, kf)
	if err != nil {
		t.Fatal(err)
	}
	addr := serveInternalTLSList(t, srvCert, bundle, pool, bl, nil, nil)

	if callMTLS(addr, blocked) {
		t.Fatal("blocked (issuer, serial) was accepted")
	}
	if !callMTLS(addr, sameSerialOtherIssuer) {
		t.Fatal("same serial under a different issuer must not be blocked")
	}
}

// TestRevokedListReloadBlocks: an appended entry takes effect on the next
// poll — no restart.
func TestRevokedListReloadBlocks(t *testing.T) {
	dir := t.TempDir()
	ca := newTestCANamed(t, "internal-ca")
	client := ca.signSerial(t, "operator", 0x4f8b)

	pool, bl := newInternalRevocation(t, dir, ca.pem, []byte(""))
	cf, kf := writeTestCert(t, dir, "internal.test")
	srvCert, err := tls.LoadX509KeyPair(cf, kf)
	if err != nil {
		t.Fatal(err)
	}
	addr := serveInternalTLSList(t, srvCert, ca.pem, pool, bl, nil, nil)

	if !callMTLS(addr, client) {
		t.Fatal("unlisted cert refused before the entry landed")
	}
	if err := writeFileAtomic(blocklistPath(dir), []byte(revokedEntryLine(t, client)+"\n")); err != nil {
		t.Fatal(err)
	}
	eventuallyCall(t, 5*time.Second, func() bool { return callMTLS(addr, client) }, false)
}

// TestRevokedResumedHandshakeRejected: VerifyConnection runs on resumed
// handshakes too — a client resuming a session minted before its serial
// was blocked is still refused.
func TestRevokedResumedHandshakeRejected(t *testing.T) {
	dir := t.TempDir()
	ca := newTestCANamed(t, "internal-ca")
	client := ca.signSerial(t, "operator", 0x4f8b)

	pool, bl := newInternalRevocation(t, dir, ca.pem, []byte(""))
	cf, kf := writeTestCert(t, dir, "internal.test")
	srvCert, err := tls.LoadX509KeyPair(cf, kf)
	if err != nil {
		t.Fatal(err)
	}
	addr := serveInternalTLSList(t, srvCert, ca.pem, pool, bl, nil, nil)

	cache := tls.NewLRUClientSessionCache(8)
	cfg := clientTLSCfg(client)
	cfg.ClientSessionCache = cache

	// First conn: full handshake; the response read stores the session
	// ticket in the cache.
	conn := dialMTLS(t, addr, cfg)
	if mtlsGet(t, conn, bufio.NewReader(conn)) != http.StatusOK {
		t.Fatal("first request failed")
	}
	_ = conn.Close()

	// Second conn proves the session actually resumes.
	conn = dialMTLS(t, addr, cfg)
	if !conn.ConnectionState().DidResume {
		t.Fatal("second conn did not resume — test setup broken")
	}
	if mtlsGet(t, conn, bufio.NewReader(conn)) != http.StatusOK {
		t.Fatal("resumed request failed")
	}
	_ = conn.Close()

	// Block the serial: a further resumed handshake must die too. TLS 1.3
	// completes the client side on the server's flight, so the
	// VerifyConnection refusal can land after tls.Dial returns — the conn
	// then dies on its first read/write.
	if err := writeFileAtomic(blocklistPath(dir), []byte(revokedEntryLine(t, client)+"\n")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := tls.Dial("tcp", addr, cfg)
		if err != nil {
			return // refusal surfaced inside the handshake
		}
		_ = conn.SetDeadline(time.Now().Add(300 * time.Millisecond))
		_, werr := conn.Write([]byte("GET / HTTP/1.1\r\nHost: internal.test\r\n\r\n"))
		_, rerr := conn.Read(make([]byte, 1))
		_ = conn.Close()
		if werr != nil || rerr != nil {
			return // resumed conn carrying a blocked serial was refused
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("resumed handshake with a blocked serial was accepted")
}

// TestRevokedLiveConnSevered: an established keep-alive conn whose serial
// becomes blocked is closed by the reload walk — IdleTimeout alone cannot
// bound it (ADR 0008). A conn on a still-trusted serial is untouched.
func TestRevokedLiveConnSevered(t *testing.T) {
	dir := t.TempDir()
	ca := newTestCANamed(t, "internal-ca")
	blocked := ca.signSerial(t, "operator", 0x4f8b)
	open := ca.signSerial(t, "operator", 0x99)

	pool, bl := newInternalRevocation(t, dir, ca.pem, []byte(""))
	cf, kf := writeTestCert(t, dir, "internal.test")
	srvCert, err := tls.LoadX509KeyPair(cf, kf)
	if err != nil {
		t.Fatal(err)
	}
	addr := serveInternalTLSList(t, srvCert, ca.pem, pool, bl, nil, nil)

	connBad := dialMTLS(t, addr, clientTLSCfg(blocked))
	brBad := bufio.NewReader(connBad)
	connGood := dialMTLS(t, addr, clientTLSCfg(open))
	brGood := bufio.NewReader(connGood)
	if mtlsGet(t, connBad, brBad) != http.StatusOK || mtlsGet(t, connGood, brGood) != http.StatusOK {
		t.Fatal("setup request failed")
	}
	eventuallyCall(t, 5*time.Second, func() bool { return bl.Tracked() == 2 }, true)

	if err := writeFileAtomic(blocklistPath(dir), []byte(revokedEntryLine(t, blocked)+"\n")); err != nil {
		t.Fatal(err)
	}
	eventuallyCall(t, 5*time.Second, func() bool {
		return bl.Blocked("CN=internal-ca", "4f8b")
	}, true)

	// The severed conn fails its next read — EOF or reset — while the
	// still-trusted conn keeps answering.
	deadline := time.Now().Add(5 * time.Second)
	for {
		_ = connBad.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
		if _, err := connBad.Read(make([]byte, 1)); err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("established conn on a newly blocked serial was not severed")
		}
	}
	eventuallyCall(t, 5*time.Second, func() bool { return bl.Tracked() == 1 }, true)
	if mtlsGet(t, connGood, brGood) != http.StatusOK {
		t.Fatal("conn on a still-trusted serial was severed")
	}
	_ = connGood.Close()
}

// TestRevokedConnStateDrop: closing a tracked conn drops its registry
// entry via the ConnState hook (no leak), and the hook drops on
// StateHijacked too.
func TestRevokedConnStateDrop(t *testing.T) {
	dir := t.TempDir()
	ca := newTestCANamed(t, "internal-ca")
	client := ca.signSerial(t, "operator", 0x99)

	pool, bl := newInternalRevocation(t, dir, ca.pem, []byte(""))
	cf, kf := writeTestCert(t, dir, "internal.test")
	srvCert, err := tls.LoadX509KeyPair(cf, kf)
	if err != nil {
		t.Fatal(err)
	}
	addr := serveInternalTLSList(t, srvCert, ca.pem, pool, bl, nil, nil)

	conn := dialMTLS(t, addr, clientTLSCfg(client))
	if mtlsGet(t, conn, bufio.NewReader(conn)) != http.StatusOK {
		t.Fatal("setup request failed")
	}
	eventuallyCall(t, 5*time.Second, func() bool { return bl.Tracked() == 1 }, true)
	_ = conn.Close()
	eventuallyCall(t, 5*time.Second, func() bool { return bl.Tracked() == 0 }, true)

	// The hook itself drops on both terminal states — a hijacked conn the
	// server no longer tracks must not keep its registration either.
	hook := connStateDrop(bl)
	raw1, raw2 := &fakeNetConn{}, &fakeNetConn{}
	leaf, _ := x509.ParseCertificate(client.Certificate[0])
	if err := bl.Track(raw1, leaf); err != nil {
		t.Fatal(err)
	}
	if err := bl.Track(raw2, leaf); err != nil {
		t.Fatal(err)
	}
	hook(raw1, http.StateHijacked)
	hook(raw2, http.StateClosed)
	hook(raw1, http.StateIdle) // non-terminal states are ignored
	if bl.Tracked() != 0 {
		t.Fatalf("registry leaked %d entries", bl.Tracked())
	}
}

// fakeNetConn is a bare net.Conn stand-in for ConnState-hook tests.
type fakeNetConn struct{ closed bool }

func (f *fakeNetConn) Read([]byte) (int, error)         { return 0, io.EOF }
func (f *fakeNetConn) Write([]byte) (int, error)        { return 0, io.EOF }
func (f *fakeNetConn) Close() error                     { f.closed = true; return nil }
func (f *fakeNetConn) LocalAddr() net.Addr              { return nil }
func (f *fakeNetConn) RemoteAddr() net.Addr             { return nil }
func (f *fakeNetConn) SetDeadline(time.Time) error      { return nil }
func (f *fakeNetConn) SetReadDeadline(time.Time) error  { return nil }
func (f *fakeNetConn) SetWriteDeadline(time.Time) error { return nil }

// TestRevocationOffByteIdentical: with no blocklist installed the clone
// the handshake gets carries NO VerifyConnection — the wire behaviour is
// byte-identical to a build without the feature (ADR 0008).
func TestRevocationOffByteIdentical(t *testing.T) {
	dir := t.TempDir()
	ca := newTestCA(t)
	caFile := filepath.Join(dir, "clients-ca.pem")
	if err := writeFileAtomic(caFile, ca.pem); err != nil {
		t.Fatal(err)
	}
	pool, err := tlsreload.NewCAPool(caFile)
	if err != nil {
		t.Fatal(err)
	}
	cf, kf := writeTestCert(t, dir, "internal.test")
	srvCert, err := tls.LoadX509KeyPair(cf, kf)
	if err != nil {
		t.Fatal(err)
	}
	tlsCfg, err := httpapi.ServerTLSConfig(srvCert, ca.pem)
	if err != nil {
		t.Fatal(err)
	}
	hotReloadClientCAs(tlsCfg, pool)

	if tlsCfg.VerifyConnection != nil {
		t.Fatal("VerifyConnection installed on the feature-off path")
	}
	clone, err := tlsCfg.GetConfigForClient(&tls.ClientHelloInfo{})
	if err != nil {
		t.Fatal(err)
	}
	if clone.VerifyConnection != nil {
		t.Fatal("per-handshake clone carries VerifyConnection with the feature off")
	}
}

// TestRevocationAbsentCertStill401: with the feature on, an absent client
// cert still reaches the handler's 401 — VerifyConnection no-ops on empty
// PeerCertificates (VerifyClientCertIfGiven lets the handshake through).
func TestRevocationAbsentCertStill401(t *testing.T) {
	dir := t.TempDir()
	ca := newTestCANamed(t, "internal-ca")
	client := ca.signSerial(t, "operator", 0x99)

	pool, bl := newInternalRevocation(t, dir, ca.pem,
		[]byte(revokedEntryLine(t, client)+"\n"))
	cf, kf := writeTestCert(t, dir, "internal.test")
	srvCert, err := tls.LoadX509KeyPair(cf, kf)
	if err != nil {
		t.Fatal(err)
	}
	addr := serveInternalTLSList(t, srvCert, ca.pem, pool, bl, nil, nil)

	hc := &http.Client{
		Timeout:   2 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, // test only
	}
	resp, err := hc.Get("https://" + addr + "/")
	if err != nil {
		t.Fatalf("absent-cert request errored at the transport layer: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("absent cert got %d, want 401", resp.StatusCode)
	}
}

// TestRevokedSerialWarnAndMetric: one rejected handshake emits the
// rate-limited internal_mtls_revoked_cert WARN (peer, serial, issuer) and
// increments tinycdi_internal_mtls_revoked_serial_total{listener="internal"}.
func TestRevokedSerialWarnAndMetric(t *testing.T) {
	dir := t.TempDir()
	ca := newTestCANamed(t, "internal-ca")
	blocked := ca.signSerial(t, "operator", 0x4f8b)

	var buf lockedBuffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	reg := prometheus.NewRegistry()
	metrics := observability.NewMetrics(reg, nil)

	pool, bl := newInternalRevocation(t, dir, ca.pem,
		[]byte(revokedEntryLine(t, blocked)+"\n"))
	cf, kf := writeTestCert(t, dir, "internal.test")
	srvCert, err := tls.LoadX509KeyPair(cf, kf)
	if err != nil {
		t.Fatal(err)
	}
	addr := serveInternalTLSList(t, srvCert, ca.pem, pool, bl, log, metrics)

	if callMTLS(addr, blocked) {
		t.Fatal("blocked cert accepted")
	}

	line := buf.String()
	for _, want := range []string{"internal_mtls_revoked_cert", `"serial":"4f8b"`, `"issuer":"CN=internal-ca"`} {
		if !strings.Contains(line, want) {
			t.Errorf("warn log missing %q: %s", want, line)
		}
	}

	var found *dto.Metric
	fams, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fams {
		if f.GetName() != "tinycdi_internal_mtls_revoked_serial_total" {
			continue
		}
		for _, m := range f.GetMetric() {
			found = m
		}
	}
	if found == nil || found.GetCounter().GetValue() != 1 {
		t.Fatalf("revoked-serial counter = %v, want 1", found)
	}
}

// TestRevokedListReloadErrorMetric: wired the way wire.go wires it, a
// corrupt list file keeps the last-good list, increments
// tinycdi_internal_mtls_revocation_reload_errors_total and leaves
// tinycdi_internal_mtls_revocation_entries at the loaded count.
func TestRevokedListReloadErrorMetric(t *testing.T) {
	dir := t.TempDir()
	reg := prometheus.NewRegistry()
	metrics := observability.NewMetrics(reg, nil)

	listFile := filepath.Join(dir, "revoked-serials.txt")
	if err := writeFileAtomic(listFile, []byte("4f8b CN=internal-ca\n")); err != nil {
		t.Fatal(err)
	}
	bl, err := tlsreload.NewBlocklist(listFile,
		tlsreload.WithInterval(10*time.Millisecond),
		tlsreload.WithReloadObserver(metrics.IncRevocationReloadError, metrics.SetRevocationEntries))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go bl.Run(ctx)

	if err := writeFileAtomic(listFile, []byte("not a list entry\n")); err != nil {
		t.Fatal(err)
	}
	get := func(name string) float64 {
		fams, err := reg.Gather()
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range fams {
			if f.GetName() != name {
				continue
			}
			for _, m := range f.GetMetric() {
				if c := m.GetCounter(); c != nil {
					return c.GetValue()
				}
				return m.GetGauge().GetValue()
			}
		}
		return -1
	}
	eventuallyCall(t, 5*time.Second, func() bool {
		return get("tinycdi_internal_mtls_revocation_reload_errors_total") >= 1
	}, true)
	if !bl.Blocked("CN=internal-ca", "4f8b") {
		t.Fatal("a corrupt reload dropped the last-good list")
	}
	if v := get("tinycdi_internal_mtls_revocation_entries"); v != 1 {
		t.Fatalf("entries gauge = %v, want 1", v)
	}
}

// TestParseFlags_RevokedList covers the flag/env surface and the
// fail-closed validation: a configured list with no internal listener is
// refused rather than silently ignored.
func TestParseFlags_RevokedList(t *testing.T) {
	c, err := ParseFlags(withArg(mergedArgs(), "-internal-revoked-list", "/etc/internal-revoked/revoked-serials.txt"), noEnv)
	if err != nil {
		t.Fatal(err)
	}
	if c.InternalRevokedList != "/etc/internal-revoked/revoked-serials.txt" {
		t.Fatalf("got %q", c.InternalRevokedList)
	}

	c, err = ParseFlags(mergedArgs(), envMap(map[string]string{
		"TCDI_INTERNAL_REVOKED_LIST": "/env/list.txt",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.InternalRevokedList != "/env/list.txt" {
		t.Fatalf("env fallback got %q", c.InternalRevokedList)
	}

	args := withArg(mergedArgs(), "-internal-listen", "")
	args = withArg(args, "-internal-revoked-list", "/etc/list.txt")
	if _, err := ParseFlags(args, noEnv); err == nil ||
		!strings.Contains(err.Error(), "-internal-revoked-list requires -internal-listen") {
		t.Fatalf("expected fail-closed refusal, got %v", err)
	}
}

// lockedBuffer is a bytes.Buffer safe for the server's log goroutines.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// callMTLSForced is callMTLS with the client certificate presented
// unconditionally. Go's TLS client withholds a certificate whose issuer is
// not in the server's acceptable-CA list (the request then reaches the
// handler without one), so a foreign-CA probe needs this to fail the
// handshake the way a non-Go client (curl, openssl s_client) does.
func callMTLSForced(addr string, cert tls.Certificate) bool {
	hc := &http.Client{
		Timeout: 2 * time.Second,
		Transport: &http.Transport{
			DisableKeepAlives: true,
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true, // test only: the assertion is the server-side client-CA check
				GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
					return &cert, nil
				},
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

// TestIdentify_HandshakeRejectionsCounted (R1-33, TM-40): on the real
// internal listener, a client cert from a foreign CA fails the TLS
// handshake — and each refusal is counted on
// tinycdi_internal_auth_failures_total{reason="handshake"}, while the
// structured internal_auth_rejected warn line is rate-limited (burst 10)
// and names the failure class and peer, never certificate material.
func TestIdentify_HandshakeRejectionsCounted(t *testing.T) {
	dir := t.TempDir()
	ca := newTestCANamed(t, "internal-ca")
	foreign := newTestCANamed(t, "foreign-ca")
	caFile := filepath.Join(dir, "clients-ca.pem")
	if err := writeFileAtomic(caFile, ca.pem); err != nil {
		t.Fatal(err)
	}
	pool, err := tlsreload.NewCAPool(caFile)
	if err != nil {
		t.Fatal(err)
	}
	cf, kf := writeTestCert(t, dir, "internal.test")
	srvCert, err := tls.LoadX509KeyPair(cf, kf)
	if err != nil {
		t.Fatal(err)
	}
	var logs lockedBuffer
	log := slog.New(slog.NewJSONHandler(&logs, nil))
	reg := prometheus.NewRegistry()
	metrics := observability.NewMetrics(reg, nil)
	addr := serveInternalTLSList(t, srvCert, ca.pem, pool, nil, log, metrics)

	const probes = 15
	probe := foreign.sign(t, "operator")
	for i := 0; i < probes; i++ {
		if callMTLSForced(addr, probe) {
			t.Fatal("foreign-CA cert accepted")
		}
	}
	want := map[string]string{"reason": "handshake"}
	deadline := time.Now().Add(5 * time.Second)
	for metricSum(t, reg, "tinycdi_internal_auth_failures_total", want) < probes && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := metricSum(t, reg, "tinycdi_internal_auth_failures_total", want); got != probes {
		t.Fatalf("handshake refusals counted = %v, want %d", got, probes)
	}
	out := logs.String()
	n := strings.Count(out, `"msg":"internal_auth_rejected"`)
	if n < 1 || n > 11 { // burst 10, plus at most one 1/s refill during the loop
		t.Errorf("internal_auth_rejected lines = %d, want 1..11 (rate-limited)", n)
	}
	for _, want := range []string{`"reason":"handshake"`, `"class":"unknown_ca"`, `"peer":"127.0.0.1:`} {
		if !strings.Contains(out, want) {
			t.Errorf("warn line missing %s: %s", want, out)
		}
	}
	if strings.Contains(out, "BEGIN CERTIFICATE") || strings.Contains(out, "foreign-ca") {
		t.Errorf("warn line carries certificate material: %s", out)
	}
}
