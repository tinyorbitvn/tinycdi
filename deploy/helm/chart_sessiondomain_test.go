// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package chart_test

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// TestSessionDomainRoutes (D9): the session edge is ONE wildcard rule —
// the HTTPRoute hostname and the Ingress host are *.<sessionDomain>, and
// the Ingress TLS stanza (the certificate coverage request: the named
// Secret's certificate must cover these hosts) asks for the same
// wildcard, never the bare domain or a per-workspace host.
func TestSessionDomainRoutes(t *testing.T) {
	want := "*.session.lab.example.net"

	t.Run("HTTPRoute", func(t *testing.T) {
		docs := renderArgs(t,
			"-f", filepath.Join("tinycdi", "ci", "gateway-api-values.yaml"),
			"--set", "sessionDomain=session.lab.example.net")
		var session doc
		for _, d := range selectDocs(docs, "HTTPRoute") {
			if n, _ := meta(d); n == "session" {
				session = d
			}
		}
		if session == nil {
			t.Fatal("no session HTTPRoute rendered")
		}
		spec, _ := session["spec"].(map[string]any)
		var hosts []string
		for _, h := range toSlice(spec["hostnames"]) {
			hosts = append(hosts, fmt.Sprintf("%v", h))
		}
		if len(hosts) != 1 || hosts[0] != want {
			t.Errorf("session HTTPRoute hostnames = %v, want exactly [%s]", hosts, want)
		}
	})

	t.Run("Ingress", func(t *testing.T) {
		docs := renderArgs(t,
			"-f", filepath.Join("tinycdi", "ci", "ingress-certmanager-values.yaml"),
			"--set", "sessionDomain=session.lab.example.net")
		var session doc
		for _, d := range selectDocs(docs, "Ingress") {
			if n, _ := meta(d); n == "session" {
				session = d
			}
		}
		if session == nil {
			t.Fatal("no session Ingress rendered")
		}
		spec, _ := session["spec"].(map[string]any)
		var ruleHost string
		for _, r := range toSlice(spec["rules"]) {
			rm, _ := r.(map[string]any)
			ruleHost, _ = rm["host"].(string)
		}
		if ruleHost != want {
			t.Errorf("session Ingress rule host = %q, want %q", ruleHost, want)
		}
		// The certificate request: spec.tls[].hosts declares which names
		// the referenced Secret's certificate must cover — it must be the
		// wildcard, or the edge would serve a non-matching cert.
		var tlsHosts []string
		for _, tl := range toSlice(spec["tls"]) {
			tm, _ := tl.(map[string]any)
			for _, h := range toSlice(tm["hosts"]) {
				tlsHosts = append(tlsHosts, fmt.Sprintf("%v", h))
			}
		}
		found := false
		for _, h := range tlsHosts {
			if h == want {
				found = true
			}
		}
		if !found {
			t.Errorf("session Ingress tls hosts = %v, want %s covered", tlsHosts, want)
		}
	})
}

// TestSessionHostValueFails (D9): the 0.1.x value sessionHost is gone —
// setting it must fail the render with a migration hint that names
// sessionDomain, not a bare schema rejection.
func TestSessionHostValueFails(t *testing.T) {
	out := renderErrArgs(t,
		"-f", filepath.Join("tinycdi", "ci", "minimal-values.yaml"),
		"--set", "sessionHost=session.lab.example.net")
	if !strings.Contains(out, "sessionDomain") {
		t.Errorf("setting sessionHost must fail naming sessionDomain, got: %s", out)
	}
}

// TestSessionDomainMustDifferFromPortal: portalHost equal to the session
// domain — or INSIDE it, where the *. wildcard route would capture portal
// traffic — must fail the render.
func TestSessionDomainMustDifferFromPortal(t *testing.T) {
	for _, tc := range []struct {
		label, sessionDomain string
	}{
		{"equal", "portal.lab.example.net"},
		{"inside", "lab.example.net"},
		{"inside apex", "example.net"},
	} {
		out := renderErrArgs(t,
			"-f", filepath.Join("tinycdi", "ci", "minimal-values.yaml"),
			"--set", "sessionDomain="+tc.sessionDomain)
		if !strings.Contains(out, "sessionDomain") || !strings.Contains(out, "portalHost") {
			t.Errorf("%s: portalHost inside sessionDomain must fail naming both values, got: %s", tc.label, out)
		}
	}
}

// TestBackendArgsSessionDomain: the backend session listener is told the
// session domain (workspaces live on <label>.<sessionDomain>) and the
// in-cluster Service names whose Host may reach the /healthz and
// /v1/control/* control surface; the frontend gets the same domain for
// its CSP. The 0.1.x flags -session-origin and -session-allowed-hosts
// are gone.
func TestBackendArgsSessionDomain(t *testing.T) {
	for _, vf := range []string{"example-values.yaml", "minimal-values.yaml"} {
		docs := renderArgs(t,
			"-f", filepath.Join("tinycdi", "ci", vf),
			"--set", "sessionDomain=session.lab.example.net")
		dep := deployment(docs, "backend")
		if dep == nil {
			t.Fatalf("%s: no backend Deployment rendered", vf)
		}
		args := strings.Join(firstContainerArgs(dep), "\n")
		if !strings.Contains(args, "-session-domain=session.lab.example.net") {
			t.Errorf("%s: backend args missing -session-domain=session.lab.example.net\nargs:\n%s", vf, args)
		}
		var controlHosts string
		for _, a := range firstContainerArgs(dep) {
			if strings.HasPrefix(a, "-session-control-hosts=") {
				controlHosts = strings.TrimPrefix(a, "-session-control-hosts=")
			}
		}
		for _, svc := range []string{"backend", "backend.tcdi-system", "backend.tcdi-system.svc"} {
			if !strings.Contains(","+controlHosts+",", ","+svc+",") &&
				!strings.HasPrefix(controlHosts, svc+",") &&
				!strings.HasSuffix(controlHosts, ","+svc) && controlHosts != svc {
				t.Errorf("%s: -session-control-hosts %q must contain the Service name %q\nargs:\n%s", vf, controlHosts, svc, args)
			}
		}
		for _, gone := range []string{"-session-origin", "-session-allowed-hosts"} {
			if strings.Contains(args, gone) {
				t.Errorf("%s: backend args still carry removed flag %s\nargs:\n%s", vf, gone, args)
			}
		}
		// -gateway-audience defaults to the session domain string.
		if !strings.Contains(args, "-gateway-audience=session.lab.example.net") {
			t.Errorf("%s: backend args missing -gateway-audience=session.lab.example.net\nargs:\n%s", vf, args)
		}

		fe := deployment(docs, "frontend")
		if fe == nil {
			t.Fatalf("%s: no frontend Deployment rendered", vf)
		}
		fargs := strings.Join(firstContainerArgs(fe), "\n")
		if !strings.Contains(fargs, "-session-domain=session.lab.example.net") {
			t.Errorf("%s: frontend args missing -session-domain=session.lab.example.net\nargs:\n%s", vf, fargs)
		}
		if strings.Contains(fargs, "-session-origin") {
			t.Errorf("%s: frontend args still carry removed flag -session-origin\nargs:\n%s", vf, fargs)
		}
	}
}
