// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package tlsreload

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	// maxBlocklistBytes bounds the revocation list file (ADR 0008): a
	// blocklist is small by nature — the bound stops a bad push from
	// stalling the handshake path.
	maxBlocklistBytes = 1 << 20
	// maxBlocklistEntries bounds the entry lines one list may carry.
	maxBlocklistEntries = 8192
)

// ErrCertRevoked is Track's refusal: the presented leaf's (issuer, serial)
// is on the revocation list. The TLS handshake aborts on it — the peer
// sees a bad-certificate alert, not the listener's 401.
var ErrCertRevoked = errors.New("tlsreload: certificate on the revocation list")

// WithReloadObserver registers Blocklist reload hooks: onError runs on
// every failed reload that keeps the previous list, onSwap after every
// successful load or swap with the new entry count (including the boot
// load in NewBlocklist). Both are optional. Reloader and CAPool ignore
// the hooks.
func WithReloadObserver(onError func(), onSwap func(entries int)) Option {
	return func(o *options) {
		o.onError = onError
		o.onSwap = onSwap
	}
}

// WithKnownIssuers gives a Blocklist the current client-CA bundle: each
// load and swap then warns about every entry whose issuer names no
// certificate in it — an entry that can never match a verified leaf
// issued directly by a bundle CA (TM-38).
func WithKnownIssuers(certs func() []*x509.Certificate) Option {
	return func(o *options) { o.knownIssuers = certs }
}

// revokedEntry is one (issuer, serial) pair. issuer is the canonical name
// key (canonicalDN of the file's DN, canonicalRawName of a leaf's
// RawIssuer), so every spelling of one DN — OpenSSL's RFC 2253 output,
// Go's pkix.Name.String(), any attribute order or escaping — compares
// equal; serial is the serial number normalized to lowercase hex
// (big.Int.Text(16)), so file spellings differing in case, colons or
// leading-zero octets compare equal.
type revokedEntry struct {
	issuer string
	serial string
}

// Revocations is an immutable snapshot of one loaded blocklist — the set
// of (issuer, serial) pairs the handshake path matches the verified peer
// leaf against, each mapped to its issuer as written in the file (for
// warnings).
type Revocations struct {
	entries map[revokedEntry]string
}

// Blocked reports whether the (issuer, serial) pair is listed. issuer is
// any DN spelling canonicalDN accepts; an unparsable one is never listed.
func (s *Revocations) Blocked(issuer, serial string) bool {
	key, err := canonicalDN(issuer)
	if err != nil {
		return false
	}
	return s.blocked(key, serial)
}

func (s *Revocations) blocked(issuerKey, serial string) bool {
	_, ok := s.entries[revokedEntry{issuer: issuerKey, serial: serial}]
	return ok
}

// Len reports the entry count.
func (s *Revocations) Len() int { return len(s.entries) }

// Blocklist serves the newest revocation list found at file and severs
// tracked connections whose serial becomes blocked. It mirrors CAPool:
// reload is a poll on the content hash (Kubernetes Secret volumes update
// by symlink swap, which defeats fsnotify), a failed reload keeps the
// last-good list, and boot is fail-closed — an unreadable or invalid file
// fails NewBlocklist, so a configured revocation control never starts
// silently disabled (ADR 0008, SR-3-F5).
type Blocklist struct {
	file string
	options

	// mu serializes the swap+sever walk in reload against registration in
	// Track: a conn registered just before a swap still lands in the walk,
	// so it cannot survive until the next reload.
	mu    sync.Mutex
	cur   *Revocations
	conns map[net.Conn]revokedEntry
	hash  [32]byte
}

// NewBlocklist loads the list once and fails if it is unusable.
func NewBlocklist(file string, opts ...Option) (*Blocklist, error) {
	b := &Blocklist{
		file:    file,
		options: options{interval: 30 * time.Second, log: slog.Default()},
		conns:   map[net.Conn]revokedEntry{},
	}
	for _, o := range opts {
		o(&b.options)
	}
	set, hash, err := loadBlocklist(file)
	if err != nil {
		return nil, err
	}
	b.cur = set
	b.hash = hash
	b.warnUnknownIssuers(set)
	if b.onSwap != nil {
		b.onSwap(set.Len())
	}
	return b, nil
}

// Current returns the snapshot the handshake path consults (ADR 0008).
// Reloads replace it wholesale; the returned set is immutable.
func (b *Blocklist) Current() *Revocations {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.cur
}

// Blocked reports whether the (issuer, serial) pair is on the current
// list.
func (b *Blocklist) Blocked(issuer, serial string) bool {
	return b.Current().Blocked(issuer, serial)
}

// Track checks leaf's (issuer, serial) against the current list and, when
// it is not blocked, records conn → the pair so a later reload that
// blocks it severs the conn. The check and the registration run under the
// same lock as the reload swap+sever walk, so a conn verified just before
// a swap lands in the walk — it never survives to the next reload. A
// blocked leaf reports ErrCertRevoked and registers nothing. A nil conn
// checks without registering.
func (b *Blocklist) Track(conn net.Conn, leaf *x509.Certificate) error {
	// A leaf whose issuer name does not decode gets the empty key, which
	// no entry carries (canonicalDN never returns "").
	issuer, _ := canonicalRawName(leaf.RawIssuer)
	key := revokedEntry{issuer: issuer}
	if leaf.SerialNumber != nil {
		key.serial = leaf.SerialNumber.Text(16)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.cur.blocked(key.issuer, key.serial) {
		return ErrCertRevoked
	}
	if conn != nil {
		b.conns[conn] = key
	}
	return nil
}

// Drop removes conn's registration — the internal server's ConnState hook
// calls it on StateClosed/StateHijacked so the registry cannot leak.
func (b *Blocklist) Drop(conn net.Conn) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.conns, conn)
}

// Tracked reports the live-conn registry size.
func (b *Blocklist) Tracked() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.conns)
}

// warnUnknownIssuers logs one warning per entry whose issuer names no
// certificate in the WithKnownIssuers bundle: such an entry can never
// match a leaf a bundle CA issued directly. Loading still
// succeeds — the bundle may be mid-rotation.
func (b *Blocklist) warnUnknownIssuers(set *Revocations) {
	if b.knownIssuers == nil {
		return
	}
	known := map[string]bool{}
	for _, c := range b.knownIssuers() {
		if k, err := canonicalRawName(c.RawSubject); err == nil {
			known[k] = true
		}
	}
	for e, written := range set.entries {
		if !known[e.issuer] {
			b.log.Warn("tlsreload: revocation entry names an issuer outside the client-CA bundle; it matches only leaves whose clients present that issuer as an intermediate",
				"file", b.file, "serial", e.serial, "issuer", written)
		}
	}
}

// Run re-reads the file every interval until ctx is done.
func (b *Blocklist) Run(ctx context.Context) {
	t := time.NewTicker(b.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			b.reload()
		}
	}
}

// reload re-reads the file. A failed read or parse keeps the last-good
// list — never fail open silently, never crash-loop — and increments the
// reload-error observer. A changed list swaps in under mu and severs
// every tracked conn whose (issuer, serial) is now blocked.
func (b *Blocklist) reload() {
	set, hash, err := loadBlocklist(b.file)
	if err != nil {
		b.log.Error("tlsreload: revocation list reload failed, keeping previous list",
			"file", b.file, "err", err)
		if b.onError != nil {
			b.onError()
		}
		return
	}
	b.mu.Lock()
	if hash == b.hash {
		b.mu.Unlock()
		return
	}
	b.cur = set
	b.hash = hash
	for c, key := range b.conns {
		if set.blocked(key.issuer, key.serial) {
			_ = c.Close()
			delete(b.conns, c)
		}
	}
	b.mu.Unlock()
	b.warnUnknownIssuers(set)
	if b.onSwap != nil {
		b.onSwap(set.Len())
	}
	b.log.Info("tlsreload: reloaded revocation list", "file", b.file, "entries", set.Len())
}

// loadBlocklist reads and parses the list file. Files over
// maxBlocklistBytes or carrying more than maxBlocklistEntries entry lines
// are invalid (ADR 0008); parsing is atomic — one malformed line rejects
// the whole file, because a partially applied blocklist is worse than a
// stale one.
func loadBlocklist(file string) (*Revocations, [32]byte, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, [32]byte{}, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxBlocklistBytes+1))
	if err != nil {
		return nil, [32]byte{}, err
	}
	if len(data) > maxBlocklistBytes {
		return nil, [32]byte{}, fmt.Errorf("tlsreload: revocation list exceeds %d bytes", maxBlocklistBytes)
	}
	entries, err := parseBlocklist(string(data))
	if err != nil {
		return nil, [32]byte{}, err
	}
	return &Revocations{entries: entries}, sha256.Sum256(data), nil
}

// parseBlocklist parses the ADR 0008 format: "<serial-hex> <issuer-dn>"
// per line; blank lines and lines whose first non-whitespace character is
// '#' are ignored; there are no trailing comments — the issuer is the
// rest of the line, trimmed (issuer: an RFC 2253 DN in any spelling,
// matched canonically). Any malformed line fails the whole file.
func parseBlocklist(data string) (map[revokedEntry]string, error) {
	entries := map[revokedEntry]string{}
	count := 0
	for i, raw := range strings.Split(data, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || line[0] == '#' {
			continue
		}
		count++
		if count > maxBlocklistEntries {
			return nil, fmt.Errorf("tlsreload: revocation list exceeds %d entries", maxBlocklistEntries)
		}
		e, written, err := parseBlocklistLine(line)
		if err != nil {
			return nil, fmt.Errorf("tlsreload: revocation list line %d: %w", i+1, err)
		}
		entries[e] = written
	}
	return entries, nil
}

// parseBlocklistLine parses one "<serial-hex> <issuer-dn>" line. The
// serial is hex (colons stripped, case-insensitive, no 0x prefix,
// leading-zero octets optional) and normalizes via big.Int.Text(16); the
// issuer is the rest of the line, trimmed — a DN may carry spaces — and
// must parse as a DN (canonicalDN): an issuer that cannot match any
// certificate fails the line. It returns the entry and the issuer as
// written.
func parseBlocklistLine(line string) (revokedEntry, string, error) {
	i := strings.IndexAny(line, " \t")
	if i < 0 {
		return revokedEntry{}, "", errors.New("want \"<serial-hex> <issuer-dn>\"")
	}
	serialTok, issuer := line[:i], strings.TrimSpace(line[i:])
	if issuer == "" {
		return revokedEntry{}, "", errors.New("missing issuer DN")
	}
	hexStr := strings.ReplaceAll(serialTok, ":", "")
	if hexStr == "" {
		return revokedEntry{}, "", fmt.Errorf("bad serial %q", serialTok)
	}
	for _, r := range hexStr {
		if !('0' <= r && r <= '9') && !('a' <= r && r <= 'f') && !('A' <= r && r <= 'F') {
			return revokedEntry{}, "", fmt.Errorf("bad serial %q", serialTok)
		}
	}
	key, err := canonicalDN(issuer)
	if err != nil {
		return revokedEntry{}, "", err
	}
	n, _ := new(big.Int).SetString(hexStr, 16) // hex-only input cannot fail
	return revokedEntry{issuer: key, serial: n.Text(16)}, issuer, nil
}
