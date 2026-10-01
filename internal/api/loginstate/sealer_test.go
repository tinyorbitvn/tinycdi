package loginstate_test

import (
	"bytes"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/api/loginstate"
)

func key(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }

func TestSealOpen_RoundTrip(t *testing.T) {
	s, err := loginstate.NewSealer(key(1))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0)
	in := loginstate.State{OAuthState: "st", Nonce: "no", Verifier: "ve", Expires: now.Add(time.Minute).Unix()}
	tok, err := s.Seal(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := s.Open(tok, now)
	if err != nil || out != in {
		t.Fatalf("Open = %+v, %v", out, err)
	}
}

func TestOpen_Expired(t *testing.T) {
	s, _ := loginstate.NewSealer(key(1))
	now := time.Unix(1_800_000_000, 0)
	tok, _ := s.Seal(loginstate.State{OAuthState: "st", Expires: now.Add(-time.Second).Unix()})
	if _, err := s.Open(tok, now); !errors.Is(err, loginstate.ErrExpired) {
		t.Fatalf("err = %v, want ErrExpired", err)
	}
}

func TestOpen_Tampered(t *testing.T) {
	s, _ := loginstate.NewSealer(key(1))
	now := time.Unix(1_800_000_000, 0)
	tok, _ := s.Seal(loginstate.State{OAuthState: "st", Expires: now.Add(time.Minute).Unix()})
	raw, _ := base64.RawURLEncoding.DecodeString(tok)
	raw[len(raw)-1] ^= 0x01
	if _, err := s.Open(base64.RawURLEncoding.EncodeToString(raw), now); !errors.Is(err, loginstate.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
}

func TestOpen_PreviousKeyStillOpens(t *testing.T) {
	old, _ := loginstate.NewSealer(key(1))
	now := time.Unix(1_800_000_000, 0)
	tok, _ := old.Seal(loginstate.State{OAuthState: "st", Expires: now.Add(time.Minute).Unix()})
	rotated, _ := loginstate.NewSealer(key(2), key(1))
	if _, err := rotated.Open(tok, now); err != nil {
		t.Fatalf("rotated sealer rejected a token sealed with the previous key: %v", err)
	}
}

func TestOpen_UnknownKey(t *testing.T) {
	a, _ := loginstate.NewSealer(key(1))
	b, _ := loginstate.NewSealer(key(2))
	now := time.Unix(1_800_000_000, 0)
	tok, _ := a.Seal(loginstate.State{OAuthState: "st", Expires: now.Add(time.Minute).Unix()})
	if _, err := b.Open(tok, now); !errors.Is(err, loginstate.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
}

func TestNewSealer_RejectsBadKeys(t *testing.T) {
	if _, err := loginstate.NewSealer(); err == nil {
		t.Fatal("no keys: want error")
	}
	if _, err := loginstate.NewSealer(make([]byte, 16)); err == nil {
		t.Fatal("16-byte key: want error")
	}
}

func TestOpen_Garbage(t *testing.T) {
	s, _ := loginstate.NewSealer(key(1))
	for _, tok := range []string{"", "!!!", "AAAA"} {
		if _, err := s.Open(tok, time.Now()); !errors.Is(err, loginstate.ErrInvalid) {
			t.Fatalf("Open(%q) err = %v, want ErrInvalid", tok, err)
		}
	}
}

// --- LoadKeyFiles -----------------------------------------------------------

func writeKeyFile(t *testing.T, contents []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "login.key")
	if err := os.WriteFile(p, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadKeyFiles_RawAndBase64(t *testing.T) {
	raw := key(0x42) // printable: a key that is all whitespace bytes is degenerate
	for name, contents := range map[string][]byte{
		"raw 32 bytes":           raw,
		"base64 std padded":      []byte(base64.StdEncoding.EncodeToString(raw)),
		"base64url unpadded":     []byte(base64.RawURLEncoding.EncodeToString(raw)),
		"surrounding whitespace": []byte("  \n" + base64.StdEncoding.EncodeToString(raw) + "\n\t"),
	} {
		t.Run(name, func(t *testing.T) {
			keys, err := loginstate.LoadKeyFiles([]string{writeKeyFile(t, contents)})
			if err != nil {
				t.Fatalf("LoadKeyFiles: %v", err)
			}
			if len(keys) != 1 || !bytes.Equal(keys[0], raw) {
				t.Fatalf("keys = %x, want %x", keys, raw)
			}
		})
	}
}

func TestLoadKeyFiles_MultipleFiles(t *testing.T) {
	keys, err := loginstate.LoadKeyFiles([]string{
		writeKeyFile(t, key(1)),
		writeKeyFile(t, []byte(base64.RawURLEncoding.EncodeToString(key(2)))),
	})
	if err != nil {
		t.Fatalf("LoadKeyFiles: %v", err)
	}
	if len(keys) != 2 || !bytes.Equal(keys[0], key(1)) || !bytes.Equal(keys[1], key(2)) {
		t.Fatalf("keys = %x", keys)
	}
}

func TestLoadKeyFiles_RejectsBadContents(t *testing.T) {
	for name, contents := range map[string][]byte{
		"too short raw":       key(1)[:16],
		"base64 wrong length": []byte(base64.StdEncoding.EncodeToString(key(1)[:16])),
		"not base64":          []byte("%%%not-a-key%%%"),
		"empty":               {},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := loginstate.LoadKeyFiles([]string{writeKeyFile(t, contents)}); err == nil {
				t.Fatal("LoadKeyFiles accepted bad key material")
			}
		})
	}
}

func TestLoadKeyFiles_MissingFile(t *testing.T) {
	if _, err := loginstate.LoadKeyFiles([]string{filepath.Join(t.TempDir(), "nope")}); err == nil {
		t.Fatal("LoadKeyFiles on a missing file: want error")
	}
}
