// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package sessionhost_test

import (
	"strings"
	"testing"

	"github.com/tinyorbitvn/tinycdi/internal/sessionhost"
)

// FuzzDomainMatch exercises the session-domain parser and the Host-header
// classifier with arbitrary strings (ports, IPv6 literals, IDNA/punycode,
// trailing dots, case, empty labels). Invariants: parsing and matching
// never panic and are deterministic; a successful match yields a
// well-formed workspace ID that maps back to a Host the same domain
// matches; Label/WorkspaceID round-trip exactly.
func FuzzDomainMatch(f *testing.F) {
	for _, c := range [][3]string{
		// Corpus seeded from sessionhost_test.go plus edge inputs.
		{"session.example.com", "ws-0123456789abcdef.session.example.com", "ws_0123456789abcdef"},
		{"session.example.com", "ws-0123456789abcdef.session.example.com:443", "ws_0123456789abcdef"},
		{"session.example.com", "WS-0123456789ABCDEF.SESSION.EXAMPLE.COM", "ws_0123456789abcdef"},
		{"session.example.com", "ws-0123456789abcdef.session.example.com.", "ws_0123456789abcdef"},
		{"session.example.com", "a.ws-0123456789abcdef.session.example.com", "ws_0123456789abcdef"},
		{"session.example.com", "ws-0123456789abcdef.session.example.com.evil.com", "ws_0123456789abcdef"},
		{"session.tcdi.localhost:4312", "ws-0123456789abcdef.session.tcdi.localhost:4312", "ws_0123456789abcdef"},
		{"session.tcdi.localhost:4312", "ws-0123456789abcdef.session.tcdi.localhost", "ws_0123456789abcdef"},
		{"session.example.com:443", "ws-0123456789abcdef.session.example.com:8444", "ws_0123456789abcdef"},
		{"session.example.com", "[::1]:8444", "ws_0123456789abcdef"},
		{"session.example.com", "ws-0123456789abcdef.[::1]", "ws_0123456789abcdef"},
		{"xn--ssion-qta.example.com", "ws-abcdefgh.xn--ssion-qta.example.com", "ws_abcdefgh"},
		{"session.example.com", "ws-xn--abcdefgh.session.example.com", "ws_abcdefgh"},
		{"session.example.com", "ws-0123456789abcdef.session.example.com:0", "ws_0123456789abcdef"},
		{"session.example.com", "ws-0123456789abcdef.session.example.com:65536", "ws_0123456789abcdef"},
		{"session.example.com", "ws-0123456789abcdef.session.example.com:", "ws_0123456789abcdef"},
		{"session.example.com", "ws-0123456789abcdef:443.session.example.com", "ws_0123456789abcdef"},
		{"session.example.com", "ws_0123456789abcdef.session.example.com", "ws_0123456789abcdef"},
		{"", "", ""},
		{"Session.Example.com", "ws-abcdefgh.session.example.com", "ws_abcdefgh"},
		{"session..example.com", "ws-abcdefgh..session.example.com", "ws_abcdefgh"},
		{"session.example.com", "", "ws_ABCDEFGH"},
		{"session.example.com", "ws-.session.example.com", "ws_abc.defgh"},
		{"session.example.com", "ws--abcdefgh.session.example.com", "ws_-abcdefg"},
		{"session.example.com", "ws-abcdefgh.session.example.com", "ws_abcdefghijklmnop"},
	} {
		f.Add(c[0], c[1], c[2])
	}
	f.Fuzz(func(t *testing.T, domain, hostport, wsID string) {
		// Wire-realistic bounds (net/http caps the whole header block at
		// 1 MiB; DNS names cap at 253 chars); mutator-grown giants can't
		// arrive and only stall the run's minimization rounds.
		if len(domain) > 64<<10 || len(hostport) > 64<<10 || len(wsID) > 64<<10 {
			return
		}
		d, err := sessionhost.ParseDomain(domain)
		if _, err2 := sessionhost.ParseDomain(domain); (err == nil) != (err2 == nil) {
			t.Fatalf("ParseDomain(%q) not deterministic", domain)
		}
		if err != nil {
			return
		}
		got1, ok1 := d.Match(hostport)
		got2, ok2 := d.Match(hostport)
		if got1 != got2 || ok1 != ok2 {
			t.Fatalf("Match(%q) not deterministic: %q,%v then %q,%v", hostport, got1, ok1, got2, ok2)
		}
		if ok1 {
			// A matched workspace ID must be well-formed and round-trip:
			// Host(id) re-parses and matches back to the same ID.
			l, err := sessionhost.Label(got1)
			if err != nil {
				t.Fatalf("Match(%q) = %q not a valid workspace ID", hostport, got1)
			}
			if id, ok := sessionhost.WorkspaceID(l); !ok || id != got1 {
				t.Fatalf("WorkspaceID(Label(%q)) = %q,%v", got1, id, ok)
			}
			h, err := d.Host(got1)
			if err != nil || !strings.HasPrefix(h, l+".") {
				t.Fatalf("Host(%q) = %q, %v", got1, h, err)
			}
			if id, ok := d.Match(h); !ok || id != got1 {
				t.Fatalf("Match(Host(%q)=%q) = %q,%v", got1, h, id, ok)
			}
		}

		// Label -> WorkspaceID -> Label round-trips exactly for accepted IDs.
		l, err := sessionhost.Label(wsID)
		if _, err2 := sessionhost.Label(wsID); (err == nil) != (err2 == nil) {
			t.Fatalf("Label(%q) not deterministic", wsID)
		}
		if err == nil {
			id, ok := sessionhost.WorkspaceID(l)
			if !ok || id != wsID {
				t.Fatalf("WorkspaceID(Label(%q)=%q) = %q,%v", wsID, l, id, ok)
			}
			l2, err := sessionhost.Label(id)
			if err != nil || l2 != l {
				t.Fatalf("Label(WorkspaceID(%q)) = %q, %v", l, l2, err)
			}
			// A generated Host always matches on this domain.
			h, err := d.Host(wsID)
			if err != nil {
				t.Fatalf("Host(%q) = %v after Label accepted", wsID, err)
			}
			if id, ok := d.Match(h); !ok || id != wsID {
				t.Fatalf("Match(Host(%q)=%q) = %q,%v", wsID, h, id, ok)
			}
		}
	})
}
