// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package ratelimit

import (
	"net/http"
	"strings"
	"testing"
)

// FuzzClientKey exercises the trusted-proxy client-address derivation
// (S18 / FX-R26) and the -trusted-proxies CSV parser with arbitrary
// RemoteAddr values, X-Forwarded-For lines and CIDR lists. Invariants:
// the key is deterministic, an untrusted peer always keys to itself, and
// a trusted peer keys to the right-most chain entry outside the trusted
// set — spoofed deeper claims can never claim the bucket. A selected
// entry that is not a plain IP collapses to the peer, so client bytes
// never mint keys; a non-IP socket peer still passes through verbatim —
// it is one fixed value per connection, not a sprayable claim.
// ParseTrustedProxies is deterministic and accepts only canonical CIDRs.
func FuzzClientKey(f *testing.F) {
	for _, c := range [][4]string{
		// Corpus seeded from ratelimit_test.go plus edge inputs.
		{"203.0.113.7:5150", "1.1.1.1, 2.2.2.2", "", ""},
		{"203.0.113.7:5150", "1.1.1.1, 2.2.2.2", "", "10.0.0.0/8"},
		{"10.1.2.3:8444", "6.6.6.6, 198.51.100.9, 10.9.9.9", "", "10.0.0.0/8"},
		{"10.1.2.3:8444", "10.4.4.4", "", "10.0.0.0/8"},
		{"10.1.2.3:8444", "", "", "10.0.0.0/8"},
		{"10.1.2.3:8444", "unknown, 198.51.100.9", "", "10.0.0.0/8"},
		{"[2001:db8::1]:443", "2001:db8::5, 10.1.2.3", "", "10.0.0.0/8"},
		{"2001:db8::1", "[2001:db8::5]", "", "2001:db8::/32"},
		{"10.1.2.3:8444", "::ffff:10.1.2.3", "", "10.0.0.0/8"},
		{"10.1.2.3:8444", "10.1.2.3, 10.1.2.3, 10.1.2.3", "", "10.0.0.0/8"},
		{"not-an-ip", "1.1.1.1", "", "10.0.0.0/8"},
		{"10.1.2.3:8444", "1.1.1.1,, 2.2.2.2", "  , ", "10.0.0.0/8"},
		{"10.1.2.3:8444", "1.1.1.1", "garbage", "10.0.0.0/8, 192.168.0.0/16"},
		{"10.1.2.3", "", "", "not-a-cidr"},
		{"", "", "", ""},
		{"  10.1.2.3  ", "  198.51.100.9  ", "", "10.0.0.0/8"},
		{"10.1.2.3:8444", "0.0.0.0, 255.255.255.255", "", "0.0.0.0/0"},
		{"10.1.2.3:8444", "999.999.999.999, 10.1.2.3x", "", "10.0.0.0/8"},
	} {
		f.Add(c[0], c[1], c[2], c[3])
	}
	f.Fuzz(func(t *testing.T, remoteAddr, xffA, xffB, csv string) {
		// Wire-realistic bounds (net/http caps the whole header block at
		// 1 MiB; real addresses and XFF lines are far smaller);
		// mutator-grown giants can't arrive and only stall the run.
		if len(remoteAddr) > 64<<10 || len(xffA) > 64<<10 || len(xffB) > 64<<10 || len(csv) > 64<<10 {
			return
		}
		trusted, err := ParseTrustedProxies(csv)
		trusted2, err2 := ParseTrustedProxies(csv)
		if (err == nil) != (err2 == nil) {
			t.Fatalf("ParseTrustedProxies(%q) not deterministic", csv)
		}
		if err == nil {
			if len(trusted) != len(trusted2) {
				t.Fatalf("ParseTrustedProxies(%q) length differs", csv)
			}
			for i, p := range trusted {
				if p != trusted2[i] {
					t.Fatalf("ParseTrustedProxies(%q) entry %d differs: %v vs %v", csv, i, p, trusted2[i])
				}
				if !p.IsValid() || p != p.Masked() {
					t.Fatalf("ParseTrustedProxies(%q) entry %v not a canonical CIDR", csv, p)
				}
			}
		}

		peer := PeerIP(remoteAddr)
		if peer2 := PeerIP(remoteAddr); peer != peer2 {
			t.Fatalf("PeerIP(%q) not deterministic", remoteAddr)
		}

		r := &http.Request{RemoteAddr: remoteAddr, Header: http.Header{}}
		if xffA != "" {
			r.Header.Add("X-Forwarded-For", xffA)
		}
		if xffB != "" {
			r.Header.Add("X-Forwarded-For", xffB)
		}
		got := ClientKey(r, trusted)
		if got2 := ClientKey(r, trusted); got != got2 {
			t.Fatalf("ClientKey not deterministic: %q then %q", got, got2)
		}

		// The contract, recomputed from the raw headers: an untrusted peer
		// IS the client (spoofed chain ignored); a trusted peer yields the
		// right-most entry outside the trusted set — unless that entry is
		// not a plain IP, which collapses to the peer — else the left-most
		// claim when the whole chain is trusted, else the peer itself.
		var entries []string
		for _, e := range strings.Split(xffA+","+xffB, ",") {
			if e = strings.TrimSpace(e); e != "" {
				entries = append(entries, e)
			}
		}
		want := canon(peer)
		if trustedPeer := inTrusted(peer, trusted); trustedPeer {
			leftmost := ""
			for i := len(entries) - 1; i >= 0; i-- {
				leftmost = entries[i]
				if !inTrusted(entries[i], trusted) {
					if isPlainAddr(entries[i]) {
						want = canon(entries[i])
					}
					leftmost = ""
					break
				}
			}
			if leftmost != "" {
				want = canon(leftmost)
			}
		}
		if got != want {
			t.Fatalf("ClientKey(peer=%q xff=%q,%q trusted=%v) = %q, want %q",
				remoteAddr, xffA, xffB, trusted, got, want)
		}
	})
}
