// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

// Package loginstate carries in-flight OIDC login state (OAuth state, nonce
// and PKCE verifier) in an AEAD-sealed cookie instead of process memory, so
// a login started on one replica can complete on any replica holding the
// same keys.
package loginstate

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"
)

var (
	// ErrInvalid is returned by Open for tokens that fail authentication
	// or are malformed in any way.
	ErrInvalid = errors.New("loginstate: invalid")
	// ErrExpired is returned by Open for authentic tokens whose Expires
	// timestamp is in the past.
	ErrExpired = errors.New("loginstate: expired")
)

// additionalData binds the sealed blob to this exact use; a token minted for
// anything else cannot be replayed here.
const additionalData = "tcdi-login-v1"

// keyLen is AES-256.
const keyLen = 32

// nonceLen is the standard AES-GCM nonce size.
const nonceLen = 12

// State is one in-flight OIDC login.
type State struct {
	OAuthState string `json:"s"`
	Nonce      string `json:"n"`
	Verifier   string `json:"v"` // PKCE code verifier
	Expires    int64  `json:"e"` // unix seconds
}

// Sealer seals and opens login-state tokens. It is safe for concurrent use.
type Sealer struct {
	seal cipher.AEAD   // keys[0]
	open []cipher.AEAD // every key, newest first
}

// NewSealer needs at least one key; each key is exactly 32 bytes.
// keys[0] seals; every key opens.
func NewSealer(keys ...[]byte) (*Sealer, error) {
	if len(keys) == 0 {
		return nil, errors.New("loginstate: at least one key is required")
	}
	s := &Sealer{}
	for i, k := range keys {
		if len(k) != keyLen {
			return nil, fmt.Errorf("loginstate: key %d is %d bytes, want %d", i, len(k), keyLen)
		}
		block, err := aes.NewCipher(k)
		if err != nil {
			return nil, fmt.Errorf("loginstate: key %d: %w", i, err)
		}
		gcm, err := cipher.NewGCM(block)
		if err != nil {
			return nil, fmt.Errorf("loginstate: key %d: %w", i, err)
		}
		s.open = append(s.open, gcm)
	}
	s.seal = s.open[0]
	return s, nil
}

// LoadKeyFiles reads one key per file: 32 raw bytes, or base64 (std or URL,
// padded or not) decoding to 32 bytes; surrounding whitespace is ignored.
func LoadKeyFiles(paths []string) ([][]byte, error) {
	keys := make([][]byte, 0, len(paths))
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("loginstate: read %s: %w", p, err)
		}
		k, err := decodeKey(bytes.TrimSpace(raw))
		if err != nil {
			return nil, fmt.Errorf("loginstate: %s: %w", p, err)
		}
		keys = append(keys, k)
	}
	return keys, nil
}

// decodeKey interprets b as raw 32-byte key material or a base64 encoding of
// it (standard or URL alphabet, padded or not).
func decodeKey(b []byte) ([]byte, error) {
	if len(b) == keyLen {
		return b, nil
	}
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding,
		base64.URLEncoding,
		base64.RawStdEncoding,
		base64.RawURLEncoding,
	} {
		if dec, err := enc.DecodeString(string(b)); err == nil && len(dec) == keyLen {
			return dec, nil
		}
	}
	return nil, fmt.Errorf("key material must be %d raw bytes or base64 decoding to %d bytes", keyLen, keyLen)
}

// SealData is the generic purpose-bound seal used for values other than
// login state (the session's retained ID token): aad binds the blob to its
// exact use so a blob minted for anything else cannot be replayed here.
// Returns base64url(nonce || AES-256-GCM(pt)).
func (s *Sealer) SealData(aad string, pt []byte) (string, error) {
	nonce := make([]byte, nonceLen)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	out := s.seal.Seal(nonce, nonce, pt, []byte(aad))
	return base64.RawURLEncoding.EncodeToString(out), nil
}

// OpenData authenticates and decodes a SealData blob, trying every key
// (newest first — a rotated key still opens blobs it sealed). Tampered,
// malformed or wrong-purpose: ErrInvalid.
func (s *Sealer) OpenData(aad, token string) ([]byte, error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) < nonceLen+1+s.open[0].Overhead() {
		return nil, ErrInvalid
	}
	nonce, ct := raw[:nonceLen], raw[nonceLen:]
	for _, gcm := range s.open {
		if pt, err := gcm.Open(nil, nonce, ct, []byte(aad)); err == nil {
			return pt, nil
		}
	}
	return nil, ErrInvalid
}

// Seal returns base64url(nonce || AES-256-GCM(json(st))) with a random
// 12-byte nonce and additional data "tcdi-login-v1".
func (s *Sealer) Seal(st State) (string, error) {
	pt, err := json.Marshal(st)
	if err != nil {
		return "", err
	}
	return s.SealData(additionalData, pt)
}

// Open authenticates and decodes token. Tampered or malformed: ErrInvalid.
// Past Expires: ErrExpired.
func (s *Sealer) Open(token string, now time.Time) (State, error) {
	pt, err := s.OpenData(additionalData, token)
	if err != nil {
		return State{}, err
	}
	var st State
	if err := json.Unmarshal(pt, &st); err != nil {
		return State{}, ErrInvalid
	}
	if !now.Before(time.Unix(st.Expires, 0)) {
		return State{}, ErrExpired
	}
	return st, nil
}
