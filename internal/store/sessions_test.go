package store

// SEC-27 unit tests for the credential-digest helpers. The end-to-end
// proof that rows persist digests (never raw IDs/tokens) lives in
// tests/integration TestPGSessionStore.

import (
	"regexp"
	"testing"
)

var hexDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)

func TestSessionKeyIsDigest(t *testing.T) {
	k := sessionKey("sess-abc")
	if !hexDigest.MatchString(k) {
		t.Fatalf("sessionKey %q is not a 64-hex SHA-256 digest", k)
	}
	if k == sessionKey("sess-other") {
		t.Fatal("distinct session IDs collide")
	}
	if k == "sess-abc" {
		t.Fatal("raw session ID used as key")
	}
}

func TestCSRFTokenMACIsKeyedBySessionID(t *testing.T) {
	m := csrfTokenMAC("sess-abc", "csrf-xyz")
	if !hexDigest.MatchString(m) {
		t.Fatalf("csrfTokenMAC %q is not a 64-hex digest", m)
	}
	if m == csrfTokenMAC("sess-other", "csrf-xyz") {
		t.Fatal("MAC not bound to the session ID")
	}
	if m == csrfTokenMAC("sess-abc", "csrf-other") {
		t.Fatal("MAC not bound to the token")
	}
	if m == "csrf-xyz" {
		t.Fatal("raw CSRF token returned")
	}
}
