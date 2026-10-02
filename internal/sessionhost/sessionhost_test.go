// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package sessionhost_test

import (
	"strings"
	"testing"

	"github.com/tinyorbitvn/tinycdi/internal/sessionhost"
)

func TestLabel(t *testing.T) {
	cases := []struct {
		id, want string
		ok       bool
	}{
		{"ws_0123456789abcdef0123456789abcdef", "ws-0123456789abcdef0123456789abcdef", true},
		{"ws_abcdefgh", "ws-abcdefgh", true},
		{"ws_ABCDEFGH", "", false},                   // upper case is not reversible through DNS
		{"ws_short", "", false},                      // 5 chars: below the 8-char minimum
		{"ws_" + strings.Repeat("a", 61), "", false}, // label would exceed 63 chars
		{"ws-abcdefgh", "", false},
		{"xx_abcdefgh", "", false},
		{"ws_abc.defgh", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		got, err := sessionhost.Label(c.id)
		if (err == nil) != c.ok || got != c.want {
			t.Errorf("Label(%q) = %q, %v; want %q, ok=%v", c.id, got, err, c.want, c.ok)
		}
	}
}

func TestMatch(t *testing.T) {
	d, err := sessionhost.ParseDomain("session.example.com")
	if err != nil {
		t.Fatal(err)
	}
	const id = "ws_0123456789abcdef"
	cases := []struct {
		host string
		want string
		ok   bool
	}{
		{"ws-0123456789abcdef.session.example.com", id, true},
		{"ws-0123456789abcdef.session.example.com:443", id, true},
		{"WS-0123456789ABCDEF.SESSION.EXAMPLE.COM", "", false},
		{"ws-0123456789abcdef.session.example.com.", "", false},
		{"ws-0123456789abcdef.session.example.com:8443", "", false},
		{"a.ws-0123456789abcdef.session.example.com", "", false},
		{"session.example.com", "", false},
		{"ws-0123456789abcdef.evil.com", "", false},
		{"ws-0123456789abcdef.session.example.com.evil.com", "", false},
		{"xsession.example.com", "", false},
		{"ws-0123456789abcdefsession.example.com", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		got, ok := d.Match(c.host)
		if ok != c.ok || got != c.want {
			t.Errorf("Match(%q) = %q, %v; want %q, %v", c.host, got, ok, c.want, c.ok)
		}
	}
}

func TestMatch_WithPort(t *testing.T) {
	d, _ := sessionhost.ParseDomain("session.tcdi.localhost:4312")
	if _, ok := d.Match("ws-0123456789abcdef.session.tcdi.localhost:4312"); !ok {
		t.Error("configured port must match")
	}
	if _, ok := d.Match("ws-0123456789abcdef.session.tcdi.localhost"); ok {
		t.Error("missing port must not match when one is configured")
	}
}

func TestParseDomain_Rejects(t *testing.T) {
	for _, s := range []string{"", "https://session.example.com", "session.example.com/x",
		"*.session.example.com", "Session.Example.com", "session..example.com",
		"session.example.com.", "10.0.0.1", "[::1]:8444", "session.example.com:0", "session.example.com:99999"} {
		if _, err := sessionhost.ParseDomain(s); err == nil {
			t.Errorf("ParseDomain(%q): want error", s)
		}
	}
}

func TestRoundTrip(t *testing.T) {
	d, _ := sessionhost.ParseDomain("session.example.com")
	const id = "ws_0123456789abcdef0123456789abcdef"
	h, err := d.Host(id)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := d.Match(h); !ok || got != id {
		t.Fatalf("Match(Host(id)) = %q, %v", got, ok)
	}
	if o, _ := d.Origin(id); o != "https://"+h {
		t.Fatalf("Origin = %q", o)
	}
	if d.Wildcard() != "*.session.example.com" {
		t.Fatalf("Wildcard = %q", d.Wildcard())
	}
}

// TestParseDomain_ExplicitHTTPSPort: ':443' is the scheme's default port and
// is normalised away — browsers send no port for https://…:443, so keeping it
// would 421 every request.
func TestParseDomain_ExplicitHTTPSPort(t *testing.T) {
	d, err := sessionhost.ParseDomain("session.example.com:443")
	if err != nil {
		t.Fatal(err)
	}
	const id = "ws_0123456789abcdef"
	if got, ok := d.Match("ws-0123456789abcdef.session.example.com"); !ok || got != id {
		t.Fatalf("Match(host without port) = %q, %v; want %q, true", got, ok, id)
	}
	if got, ok := d.Match("ws-0123456789abcdef.session.example.com:443"); !ok || got != id {
		t.Fatalf("Match(host with :443) = %q, %v; want %q, true", got, ok, id)
	}
	if _, ok := d.Match("ws-0123456789abcdef.session.example.com:8444"); ok {
		t.Fatal("a different port must not match")
	}
	if h, _ := d.Host(id); h != "ws-0123456789abcdef.session.example.com" {
		t.Fatalf("Host = %q, want no port", h)
	}
	if o, _ := d.Origin(id); o != "https://ws-0123456789abcdef.session.example.com" {
		t.Fatalf("Origin = %q, want no port", o)
	}
	if w := d.Wildcard(); w != "*.session.example.com" {
		t.Fatalf("Wildcard = %q, want no port", w)
	}
	if s := d.String(); s != "session.example.com" {
		t.Fatalf("String = %q, want the canonical domain without :443", s)
	}
}
