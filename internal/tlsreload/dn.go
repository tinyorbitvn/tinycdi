// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package tlsreload

import (
	"bytes"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// dnAttrOIDs maps the attribute type names a DN string may use — Go's
// pkix.Name.String() names and OpenSSL's RFC 2253 names — to their OIDs.
var dnAttrOIDs = map[string]string{
	"cn": "2.5.4.3", "c": "2.5.4.6", "l": "2.5.4.7", "st": "2.5.4.8",
	"street": "2.5.4.9", "o": "2.5.4.10", "ou": "2.5.4.11",
	"serialnumber": "2.5.4.5", "postalcode": "2.5.4.17",
	"dc": "0.9.2342.19200300.100.1.25", "uid": "0.9.2342.19200300.100.1.1",
	"emailaddress": "1.2.840.113549.1.9.1", "e": "1.2.840.113549.1.9.1",
}

// canonicalDN parses an RFC 2253 / RFC 4514 distinguished name — the
// form `openssl x509 -noout -issuer -nameopt RFC2253` prints and the
// form Go's pkix.Name.String() prints — into a canonical key: the sorted
// multiset of "OID=value" pairs, NUL-joined. Both renderings of one
// certificate name yield the same key whatever their attribute order,
// escaping (\XX hex bytes, \, …) or value encoding (#<DER hex>), and it
// equals canonicalRawName of the name's DER (ADR 0008, TM-38).
func canonicalDN(s string) (string, error) {
	var pairs []string
	i := 0
	for {
		for i < len(s) && s[i] == ' ' {
			i++
		}
		j := strings.IndexByte(s[i:], '=')
		if j <= 0 {
			return "", fmt.Errorf("bad DN %q: want type=value", s)
		}
		typ := strings.ToLower(strings.TrimSpace(s[i : i+j]))
		oid, ok := dnAttrOIDs[typ]
		if !ok {
			if !isDottedOID(typ) {
				return "", fmt.Errorf("bad DN %q: unknown attribute type %q", s, typ)
			}
			oid = typ
		}
		i += j + 1
		val, n, err := dnValue(s[i:])
		if err != nil {
			return "", fmt.Errorf("bad DN %q: %w", s, err)
		}
		pairs = append(pairs, oid+"="+val)
		i += n
		if i >= len(s) {
			break
		}
		i++ // the unescaped ',', ';' or '+' separator
	}
	sort.Strings(pairs)
	return strings.Join(pairs, "\x00"), nil
}

// dnValue decodes one attribute value at the start of s and reports how
// many bytes it consumed (up to, not including, the next unescaped
// separator). Unescaped leading and trailing spaces are not part of the
// value; an escaped space is.
func dnValue(s string) (string, int, error) {
	i := 0
	for i < len(s) && s[i] == ' ' {
		i++
	}
	if i < len(s) && s[i] == '#' {
		j := i + 1
		for j < len(s) && isHex(s[j]) {
			j++
		}
		der, err := hex.DecodeString(s[i+1 : j])
		if err != nil {
			return "", 0, fmt.Errorf("bad #hex value: %w", err)
		}
		var v string
		if rest, err := asn1.Unmarshal(der, &v); err != nil || len(rest) != 0 {
			return "", 0, errors.New("#hex value is not one DER string")
		}
		for j < len(s) && s[j] == ' ' {
			j++
		}
		if j < len(s) && !isDNSep(s[j]) {
			return "", 0, errors.New("junk after #hex value")
		}
		return v, j, nil
	}
	var b []byte
	trail := 0
	for i < len(s) && !isDNSep(s[i]) {
		c := s[i]
		if c == '\\' {
			switch {
			case i+2 < len(s) && isHex(s[i+1]) && isHex(s[i+2]):
				x, _ := strconv.ParseUint(s[i+1:i+3], 16, 8)
				b = append(b, byte(x))
				i += 3
			case i+1 < len(s):
				b = append(b, s[i+1])
				i += 2
			default:
				return "", 0, errors.New("dangling escape")
			}
			trail = 0
			continue
		}
		if c == ' ' {
			trail++
		} else {
			trail = 0
		}
		b = append(b, c)
		i++
	}
	b = b[:len(b)-trail]
	if len(b) == 0 || !utf8.Valid(b) || bytes.IndexByte(b, 0) >= 0 {
		return "", 0, errors.New("empty or invalid attribute value")
	}
	return string(b), i, nil
}

// canonicalRawName is canonicalDN for a DER-encoded certificate name
// (x509.Certificate.RawIssuer / RawSubject).
func canonicalRawName(der []byte) (string, error) {
	var rdns pkix.RDNSequence
	if rest, err := asn1.Unmarshal(der, &rdns); err != nil || len(rest) != 0 {
		return "", errors.New("tlsreload: unparsable certificate name")
	}
	var pairs []string
	for _, rdn := range rdns {
		for _, atv := range rdn {
			v, ok := atv.Value.(string)
			if !ok {
				return "", fmt.Errorf("tlsreload: name attribute %s is not a string", atv.Type)
			}
			pairs = append(pairs, atv.Type.String()+"="+v)
		}
	}
	if len(pairs) == 0 {
		return "", errors.New("tlsreload: empty certificate name")
	}
	sort.Strings(pairs)
	return strings.Join(pairs, "\x00"), nil
}

func isDNSep(c byte) bool { return c == ',' || c == ';' || c == '+' }

func isHex(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
}

func isDottedOID(s string) bool {
	if !strings.Contains(s, ".") {
		return false
	}
	for _, p := range strings.Split(s, ".") {
		if p == "" {
			return false
		}
		for _, r := range p {
			if r < '0' || r > '9' {
				return false
			}
		}
	}
	return true
}
