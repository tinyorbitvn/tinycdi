// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package opclient

import (
	"crypto/x509"
	"testing"
)

// verifyServerChain must fail closed on an empty server name — x509 reads
// an empty DNSName as "skip the hostname check", which would accept any
// certificate signed by the internal CA.
func TestVerifyServerChain_EmptyServerNameFailsClosed(t *testing.T) {
	if err := verifyServerChain([]*x509.Certificate{{}}, "", x509.NewCertPool()); err == nil {
		t.Fatal("empty serverName accepted")
	}
}

func TestVerifyServerChain_NoPeerCertsFailsClosed(t *testing.T) {
	if err := verifyServerChain(nil, "api-internal", x509.NewCertPool()); err == nil {
		t.Fatal("empty peer chain accepted")
	}
}
