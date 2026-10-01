// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

// Package sessionhost maps platform workspace IDs to the per-workspace
// hosts of the session domain (design §3.2, D9). Each workspace is served
// on "<label>.<sessionDomain>" where the label is a deterministic
// lower-case DNS label derived from the workspace ID, so the edge needs
// only one wildcard route and one wildcard certificate.
package sessionhost

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

// labelPrefix/labelSep tie the host label to the platform workspace ID
// form "ws_<suffix>" — the same mapping provisioning.WorkspaceCRName uses
// ('_' is not valid in a DNS label).
const (
	idPrefix    = "ws_"
	labelPrefix = "ws-"
)

// labelSuffixOK reports whether s is a valid workspace-ID suffix:
// [a-z0-9]{8,60}. The 60-char ceiling keeps "ws-"+suffix inside the 63-byte
// DNS label limit.
func labelSuffixOK(s string) bool {
	if len(s) < 8 || len(s) > 60 {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

// Label maps a platform workspace ID ("ws_<suffix>") to its DNS label
// ("ws-<suffix>"). The suffix must match [a-z0-9]{8,60}.
func Label(workspaceID string) (string, error) {
	suffix, ok := strings.CutPrefix(workspaceID, idPrefix)
	if !ok || !labelSuffixOK(suffix) {
		return "", fmt.Errorf("sessionhost: workspace ID %q is not ws_ + [a-z0-9]{8,60}", workspaceID)
	}
	return labelPrefix + suffix, nil
}

// WorkspaceID is the inverse of Label.
func WorkspaceID(label string) (string, bool) {
	suffix, ok := strings.CutPrefix(label, labelPrefix)
	if !ok || !labelSuffixOK(suffix) {
		return "", false
	}
	return idPrefix + suffix, true
}

// Domain is the session domain, optionally with a port.
type Domain struct {
	host string // lower-case DNS name, no port
	port string // "" or a canonical decimal port 1-65535
}

// dnsLabelOK reports whether l is a valid lower-case DNS label.
func dnsLabelOK(l string) bool {
	if len(l) == 0 || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
		return false
	}
	for _, r := range l {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return false
		}
	}
	return true
}

// splitHostPort splits "host[:port]"; the port, when present, must be a
// non-empty decimal number. A bare host returns port == "".
func splitHostPort(s string) (host, port string, ok bool) {
	host = s
	if i := strings.LastIndex(s, ":"); i >= 0 {
		host, port = s[:i], s[i+1:]
		p, err := strconv.Atoi(port)
		if err != nil || p < 1 || p > 65535 || port != strconv.Itoa(p) {
			return "", "", false
		}
	}
	return host, port, true
}

// ParseDomain accepts "session.example.com" or "session.example.com:8444".
// It rejects schemes, paths, wildcards, upper case, empty labels, a
// trailing dot and IP literals.
func ParseDomain(s string) (Domain, error) {
	fail := func() (Domain, error) {
		return Domain{}, fmt.Errorf("sessionhost: bad session domain %q (want host[:port])", s)
	}
	if s == "" || strings.Contains(s, "://") || strings.ContainsAny(s, "/@") ||
		strings.HasPrefix(s, "*") || s != strings.ToLower(s) {
		return fail()
	}
	host, port, ok := splitHostPort(s)
	if !ok || host == "" || len(host) > 253 ||
		strings.HasSuffix(host, ".") || strings.HasPrefix(host, ".") ||
		net.ParseIP(host) != nil {
		return fail()
	}
	for _, l := range strings.Split(host, ".") {
		if !dnsLabelOK(l) {
			return fail()
		}
	}
	return Domain{host: host, port: port}, nil
}

// String returns the domain as configured: "host" or "host:port".
func (d Domain) String() string {
	if d.host == "" {
		return ""
	}
	if d.port == "" {
		return d.host
	}
	return d.host + ":" + d.port
}

// Host returns "<label>.<domain>[:port]" for the workspace.
func (d Domain) Host(workspaceID string) (string, error) {
	l, err := Label(workspaceID)
	if err != nil {
		return "", err
	}
	return l + "." + d.String(), nil
}

// Origin returns "https://" + Host(workspaceID).
func (d Domain) Origin(workspaceID string) (string, error) {
	h, err := d.Host(workspaceID)
	if err != nil {
		return "", err
	}
	return "https://" + h, nil
}

// Wildcard returns the wildcard form "*.<domain>[:port]".
func (d Domain) Wildcard() string {
	return "*." + d.String()
}

// Match reports the workspace ID addressed by a request Host header.
// Exactly one label must precede the domain; the port must equal the
// configured port (absent or 443 when none is configured).
func (d Domain) Match(hostport string) (string, bool) {
	if d.host == "" {
		return "", false
	}
	host, port, ok := splitHostPort(hostport)
	if !ok {
		return "", false
	}
	switch {
	case d.port != "":
		if port != d.port {
			return "", false
		}
	case port != "" && port != "443":
		return "", false
	}
	label, ok := strings.CutSuffix(host, "."+d.host)
	if !ok || label == "" || strings.Contains(label, ".") {
		return "", false
	}
	return WorkspaceID(label)
}
