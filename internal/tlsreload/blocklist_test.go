// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package tlsreload

// ADR 0008 tests: the internal-mTLS serial blocklist — parse bounds,
// fail-closed boot, keep-last-good reload, and the conn registry that
// severs live conns on a blocking swap, serialized against registration.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeConn is a net.Conn that only records Close.
type fakeConn struct{ closed atomic.Bool }

func (f *fakeConn) Read([]byte) (int, error)         { return 0, errors.New("fake") }
func (f *fakeConn) Write([]byte) (int, error)        { return 0, errors.New("fake") }
func (f *fakeConn) Close() error                     { f.closed.Store(true); return nil }
func (f *fakeConn) LocalAddr() net.Addr              { return nil }
func (f *fakeConn) RemoteAddr() net.Addr             { return nil }
func (f *fakeConn) SetDeadline(time.Time) error      { return nil }
func (f *fakeConn) SetReadDeadline(time.Time) error  { return nil }
func (f *fakeConn) SetWriteDeadline(time.Time) error { return nil }

// testLeaf issues a leaf cert with the given serial under a CA named cn.
func testLeaf(t *testing.T, issuerCN string, serial int64) *x509.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: issuerCN},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IsCA:         true,
		KeyUsage:     x509.KeyUsageCertSign,
	}
	leaf := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: "leaf"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, leaf, ca, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

// writeList writes a blocklist file atomically, mirroring the Secret-volume
// swap.
func writeList(t *testing.T, path, content string) {
	t.Helper()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

// TestParseBlocklistFormat covers the ADR 0008 grammar: comments and
// blanks ignored, serials normalized as big integers (colons, case,
// leading zeros), issuer compared byte-for-byte after a trim.
func TestParseBlocklistFormat(t *testing.T) {
	entries, err := parseBlocklist(`
# comment line
   # indented comment

4f8b2c1a09d3e4f5 CN=tinycdi-internal-ca
4F:8B:2C:1A:09:D3:E4:F5 CN=tinycdi-internal-ca
04ab CN=Second CA, O=Org
0 CN=zero
`)
	if err != nil {
		t.Fatalf("parseBlocklist: %v", err)
	}
	want := map[revokedEntry]struct{}{
		{issuer: "CN=tinycdi-internal-ca", serial: "4f8b2c1a09d3e4f5"}: {},
		{issuer: "CN=Second CA, O=Org", serial: "4ab"}:                 {},
		{issuer: "CN=zero", serial: "0"}:                               {},
	}
	if len(entries) != len(want) {
		t.Fatalf("got %d entries, want %d: %v", len(entries), len(want), entries)
	}
	for e := range want {
		if _, ok := entries[e]; !ok {
			t.Errorf("missing entry %+v", e)
		}
	}
}

// TestParseBlocklistRejectsWholeFile: any malformed line invalidates the
// whole file — a partially applied blocklist is worse than a stale one.
func TestParseBlocklistRejectsWholeFile(t *testing.T) {
	good := "4f8b CN=tinycdi-internal-ca\n"
	for _, bad := range []string{
		"4f8b",      // serial only, no issuer
		"4f8b ",     // empty issuer
		"zz CN=x",   // non-hex serial
		"0x4f CN=x", // 0x prefix not allowed
		"::: CN=x",  // colons only
		"-4f CN=x",  // sign not allowed
		"4f8b \t",   // whitespace-only issuer
	} {
		if _, err := parseBlocklist(good + bad + "\n"); err == nil {
			t.Errorf("parseBlocklist accepted bad line %q", bad)
		}
	}
}

// TestParseBlocklistLineIssuerSpaces: the issuer is the rest of the line,
// trimmed — spaces inside a DN survive.
func TestParseBlocklistLineIssuerSpaces(t *testing.T) {
	e, err := parseBlocklistLine("4f8b   CN=Some CA, O=Org Inc  ")
	if err != nil {
		t.Fatal(err)
	}
	if e.issuer != "CN=Some CA, O=Org Inc" || e.serial != "4f8b" {
		t.Fatalf("got %+v", e)
	}
}

// TestParseBlocklistBounds: > 8192 entries or > 1 MiB files are invalid.
func TestParseBlocklistBounds(t *testing.T) {
	var b strings.Builder
	for i := 0; i < maxBlocklistEntries; i++ {
		b.WriteString("4f8b CN=ca\n")
	}
	if _, err := parseBlocklist(b.String()); err != nil {
		t.Fatalf("%d entries should parse: %v", maxBlocklistEntries, err)
	}
	b.WriteString("4f8c CN=ca\n")
	if _, err := parseBlocklist(b.String()); err == nil {
		t.Fatalf("%d entries should be rejected", maxBlocklistEntries+1)
	}

	dir := t.TempDir()
	big := filepath.Join(dir, "big.txt")
	chunk := strings.Repeat("4f8b CN=ca\n", (maxBlocklistBytes/len("4f8b CN=ca\n"))+1)
	if err := os.WriteFile(big, []byte(chunk), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadBlocklist(big); err == nil {
		t.Fatal("over-1MiB list should be rejected")
	}
}

// TestNewBlocklistFailsClosed: flag set + unreadable or invalid file →
// boot fails (a configured revocation control never starts disabled).
func TestNewBlocklistFailsClosed(t *testing.T) {
	dir := t.TempDir()
	if _, err := NewBlocklist(filepath.Join(dir, "missing.txt")); err == nil {
		t.Fatal("missing file should fail boot")
	}
	bad := filepath.Join(dir, "bad.txt")
	writeList(t, bad, "garbage line\n")
	if _, err := NewBlocklist(bad); err == nil {
		t.Fatal("invalid file should fail boot")
	}
	// A valid file boots and reports its entries.
	good := filepath.Join(dir, "good.txt")
	writeList(t, good, "4f8b CN=ca\n4f8c CN=ca\n")
	bl, err := NewBlocklist(good)
	if err != nil {
		t.Fatal(err)
	}
	if bl.Current().Len() != 2 {
		t.Fatalf("got %d entries", bl.Current().Len())
	}
	// The empty file is a valid list revoking nothing — "no file" and
	// "empty file" are different states.
	empty := filepath.Join(dir, "empty.txt")
	writeList(t, empty, "")
	bl, err = NewBlocklist(empty)
	if err != nil {
		t.Fatalf("empty list should boot: %v", err)
	}
	if bl.Current().Len() != 0 {
		t.Fatal("empty list should hold no entries")
	}
}

// TestBlocklistReloadKeepsLastGood: an unreadable/invalid replacement
// keeps the previous list and fires the error observer.
func TestBlocklistReloadKeepsLastGood(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "list.txt")
	writeList(t, file, "4f8b CN=ca\n")
	var errCount, swapCount atomic.Int64
	bl, err := NewBlocklist(file,
		WithReloadObserver(func() { errCount.Add(1) }, func(int) { swapCount.Add(1) }),
		WithLogger(testLogger()))
	if err != nil {
		t.Fatal(err)
	}
	swapCount.Store(0) // boot load fires onSwap once; ignore it

	writeList(t, file, "not a list entry\n")
	bl.reload()
	if !bl.Blocked("CN=ca", "4f8b") {
		t.Fatal("bad reload dropped the last-good list")
	}
	if errCount.Load() != 1 {
		t.Fatalf("reload-error observer fired %d times", errCount.Load())
	}

	// A deleted file is also a failed reload, not an empty list.
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	bl.reload()
	if !bl.Blocked("CN=ca", "4f8b") {
		t.Fatal("deleted file dropped the last-good list")
	}
	if errCount.Load() != 2 {
		t.Fatalf("reload-error observer fired %d times", errCount.Load())
	}
}

// TestBlocklistReloadSwaps: a changed file swaps in on the next poll and
// fires the swap observer with the new count.
func TestBlocklistReloadSwaps(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "list.txt")
	writeList(t, file, "4f8b CN=ca\n")
	var lastEntries atomic.Int64
	bl, err := NewBlocklist(file,
		WithReloadObserver(nil, func(n int) { lastEntries.Store(int64(n)) }),
		WithLogger(testLogger()))
	if err != nil {
		t.Fatal(err)
	}
	writeList(t, file, "4f8b CN=ca\n4f8c CN=ca\n")
	bl.reload()
	if !bl.Blocked("CN=ca", "4f8c") || bl.Current().Len() != 2 {
		t.Fatal("reload did not pick up the appended entry")
	}
	if lastEntries.Load() != 2 {
		t.Fatalf("swap observer got %d", lastEntries.Load())
	}
	// Shrinking to the empty list is a valid deliberate state.
	writeList(t, file, "")
	bl.reload()
	if bl.Blocked("CN=ca", "4f8b") || bl.Current().Len() != 0 {
		t.Fatal("empty reload did not shrink the list")
	}
}

// TestBlocklistTrackAndSever: Track registers an admitted conn against
// its (issuer, serial); a reload blocking the pair severs the conn and
// drops the registration, while a sibling serial stays tracked.
func TestBlocklistTrackAndSever(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "list.txt")
	writeList(t, file, "")
	bl, err := NewBlocklist(file, WithLogger(testLogger()))
	if err != nil {
		t.Fatal(err)
	}
	leaf7 := testLeaf(t, "ca", 7)
	leaf9 := testLeaf(t, "ca", 9)
	c7, c9 := &fakeConn{}, &fakeConn{}
	if err := bl.Track(c7, leaf7); err != nil {
		t.Fatal(err)
	}
	if err := bl.Track(c9, leaf9); err != nil {
		t.Fatal(err)
	}
	if bl.Tracked() != 2 {
		t.Fatalf("tracked %d", bl.Tracked())
	}

	// Block serial 7 — issuer spelled as the leaf's Issuer.String() form.
	writeList(t, file, "7 CN=ca\n")
	bl.reload()
	if !c7.closed.Load() {
		t.Fatal("conn on a newly blocked serial was not severed")
	}
	if c9.closed.Load() {
		t.Fatal("conn on a still-trusted serial was severed")
	}
	if bl.Tracked() != 1 {
		t.Fatalf("registry leaked: %d entries", bl.Tracked())
	}

	// Track now refuses serial 7.
	if err := bl.Track(&fakeConn{}, leaf7); !errors.Is(err, ErrCertRevoked) {
		t.Fatalf("Track of blocked serial: %v", err)
	}
	// The same serial under a different issuer does not match — serials
	// are unique per issuer only.
	if err := bl.Track(&fakeConn{}, testLeaf(t, "other-ca", 7)); err != nil {
		t.Fatalf("same serial under another issuer must pass: %v", err)
	}
}

// TestBlocklistDrop removes a registration (ConnState close/hijack).
func TestBlocklistDrop(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "list.txt")
	writeList(t, file, "")
	bl, err := NewBlocklist(file, WithLogger(testLogger()))
	if err != nil {
		t.Fatal(err)
	}
	leaf := testLeaf(t, "ca", 7)
	c := &fakeConn{}
	if err := bl.Track(c, leaf); err != nil {
		t.Fatal(err)
	}
	bl.Drop(c)
	if bl.Tracked() != 0 {
		t.Fatal("Drop left a registration")
	}
	// A dropped conn is not walked on a later blocking swap.
	writeList(t, file, "7 CN=ca\n")
	bl.reload()
	if c.closed.Load() {
		t.Fatal("a dropped conn must not be severed")
	}
}

// TestBlocklistConcurrentTrackAndSwap hammers Track against reloads
// (-race) and asserts the serialization invariant: every conn registered
// before a blocking swap is severed by that swap — it cannot survive to
// the next reload.
func TestBlocklistConcurrentTrackAndSwap(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "list.txt")
	writeList(t, file, "")
	bl, err := NewBlocklist(file, WithInterval(time.Millisecond), WithLogger(testLogger()))
	if err != nil {
		t.Fatal(err)
	}
	leaf := testLeaf(t, "ca", 7)

	const n = 64
	conns := make([]*fakeConn, n)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var registered []*fakeConn
	for i := 0; i < n; i++ {
		conns[i] = &fakeConn{}
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Stagger so some Tracks land before and some after the swap.
			time.Sleep(time.Duration(i%8) * time.Millisecond / 4)
			if err := bl.Track(conns[i], leaf); err == nil {
				mu.Lock()
				registered = append(registered, conns[i])
				mu.Unlock()
			}
		}()
	}
	// Swap to a list blocking the serial mid-storm.
	time.Sleep(2 * time.Millisecond)
	writeList(t, file, "7 CN=ca\n")
	bl.reload()
	wg.Wait()

	for _, c := range registered {
		if !c.closed.Load() {
			t.Error("a conn registered before the swap survived it")
		}
	}
	if bl.Tracked() != 0 {
		t.Fatalf("registry leaked %d conns", bl.Tracked())
	}

	// Same storm through Run's poll loop for coverage of the ticker path.
	writeList(t, file, "")
	bl.reload()
	ctx, cancel := context.WithCancel(context.Background())
	go bl.Run(ctx)
	time.Sleep(5 * time.Millisecond)
	writeList(t, file, "7 CN=ca\n")
	c2 := &fakeConn{}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := bl.Track(c2, leaf); errors.Is(err, ErrCertRevoked) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !bl.Blocked("CN=ca", "7") {
		t.Fatal("poll loop never applied the new list")
	}
	cancel()
}

// testLogger returns a discard logger for tests.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
