// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

// Package sessionhost maps platform workspace IDs ("ws_<suffix>") to the
// per-workspace hosts ("ws-<suffix>.<sessionDomain>") the session listener
// serves, and matches incoming Host headers back to workspace IDs.
package sessionhost

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
)

const (
	idPrefix    = "ws_"
	labelPrefix = "ws-"
)

// validSuffix reports whether s matches [a-z0-9]{8,60}. The 60-char cap keeps
// "ws-"+suffix within the 63-char DNS label limit; only lower case round-trips
// through DNS.
func validSuffix(s string) bool {
	if len(s) < 8 || len(s) > 60 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}

// Label maps a platform workspace ID ("ws_<suffix>") to its DNS label
// ("ws-<suffix>"). The suffix must match [a-z0-9]{8,60}.
func Label(workspaceID string) (string, error) {
	suffix, ok := strings.CutPrefix(workspaceID, idPrefix)
	if !ok || !validSuffix(suffix) {
		return "", fmt.Errorf("sessionhost: invalid workspace ID %q", workspaceID)
	}
	return labelPrefix + suffix, nil
}

// WorkspaceID is the inverse of Label.
func WorkspaceID(label string) (string, bool) {
	suffix, ok := strings.CutPrefix(label, labelPrefix)
	if !ok || !validSuffix(suffix) {
		return "", false
	}
	return idPrefix + suffix, true
}

// Domain is the session domain, optionally with a port.
type Domain struct {
	raw  string // as configured
	host string // lower-case DNS domain, without port
	port int    // 0 when none is configured
}

// ParseDomain accepts "session.example.com" or "session.example.com:8444".
// It rejects schemes, paths, wildcards, upper case, empty labels, a
// trailing dot and IP literals.
func ParseDomain(s string) (Domain, error) {
	hostport := s
	d := Domain{raw: s}
	if i := strings.LastIndexByte(hostport, ':'); i >= 0 {
		p, err := parsePort(hostport[i+1:])
		if err != nil {
			return Domain{}, fmt.Errorf("sessionhost: invalid domain %q: %w", s, err)
		}
		d.port = p
		hostport = hostport[:i]
	}
	if err := validDomain(hostport); err != nil {
		return Domain{}, fmt.Errorf("sessionhost: invalid domain %q: %w", s, err)
	}
	d.host = hostport
	return d, nil
}

func parsePort(s string) (int, error) {
	if s == "" {
		return 0, errors.New("empty port")
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, fmt.Errorf("port %q is not numeric", s)
		}
	}
	p, err := strconv.Atoi(s)
	if err != nil || p < 1 || p > 65535 {
		return 0, fmt.Errorf("port %q out of range", s)
	}
	return p, nil
}

func validDomain(host string) error {
	if host == "" {
		return errors.New("empty host")
	}
	if host != strings.ToLower(host) {
		return errors.New("host must be lower case")
	}
	if net.ParseIP(host) != nil {
		return errors.New("IP literal not allowed")
	}
	if len(host) > 253 {
		return errors.New("host too long")
	}
	for _, label := range strings.Split(host, ".") {
		if !validDNSLabel(label) {
			return fmt.Errorf("bad DNS label %q", label)
		}
	}
	return nil
}

// validDNSLabel reports whether l is a DNS-1123 label: 1-63 chars of
// [a-z0-9-] that neither starts nor ends with a hyphen.
func validDNSLabel(l string) bool {
	if len(l) == 0 || len(l) > 63 {
		return false
	}
	for i := 0; i < len(l); i++ {
		c := l[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	return l[0] != '-' && l[len(l)-1] != '-'
}

// String returns the domain as configured.
func (d Domain) String() string {
	return d.raw
}

func (d Domain) hostport() string {
	if d.port == 0 {
		return d.host
	}
	return d.host + ":" + strconv.Itoa(d.port)
}

// Host returns "ws-<suffix>.<domain>[:port]" for the workspace ID.
func (d Domain) Host(workspaceID string) (string, error) {
	l, err := Label(workspaceID)
	if err != nil {
		return "", err
	}
	return l + "." + d.hostport(), nil
}

// Origin returns "https://" + Host for the workspace ID.
func (d Domain) Origin(workspaceID string) (string, error) {
	h, err := d.Host(workspaceID)
	if err != nil {
		return "", err
	}
	return "https://" + h, nil
}

// Wildcard returns "*.<domain>[:port]".
func (d Domain) Wildcard() string {
	return "*." + d.hostport()
}

// Match reports the workspace ID addressed by a request Host header.
// Exactly one label must precede the domain; the port must equal the
// configured port (absent or 443 when none is configured).
func (d Domain) Match(hostport string) (workspaceID string, ok bool) {
	host := hostport
	port := 0
	hasPort := false
	if i := strings.LastIndexByte(hostport, ':'); i >= 0 {
		p, err := parsePort(hostport[i+1:])
		if err != nil {
			return "", false
		}
		host, port, hasPort = hostport[:i], p, true
		if strings.IndexByte(host, ':') >= 0 {
			return "", false
		}
	}
	if d.port != 0 {
		if !hasPort || port != d.port {
			return "", false
		}
	} else if hasPort && port != 443 {
		return "", false
	}
	label, found := strings.CutSuffix(host, "."+d.host)
	if !found || strings.Contains(label, ".") {
		return "", false
	}
	return WorkspaceID(label)
}
