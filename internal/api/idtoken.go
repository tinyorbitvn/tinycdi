// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package api

import "github.com/tinyorbitvn/tinycdi/internal/api/loginstate"

// idTokenAAD purpose-binds the sealed blob: a login-state token or any
// other blob sealed under the same keys cannot be replayed as an ID token.
const idTokenAAD = "tcdi-idtoken-v1"

// IDTokenSealer seals the session's retained OIDC ID token for at-rest
// storage with the same keys the login-state cookie uses (D20-style
// rotation: keys[0] seals, every key opens). It satisfies
// store.IDTokenSeal; the raw token is never persisted and never logged —
// a key that no longer opens the blob yields "" on read, so logout falls
// back to a client_id-only end-session URL instead of failing.
type IDTokenSealer struct{ s *loginstate.Sealer }

// NewIDTokenSealer wraps the login-state sealer. s must not be nil.
func NewIDTokenSealer(s *loginstate.Sealer) *IDTokenSealer {
	return &IDTokenSealer{s: s}
}

// SealIDToken returns the AEAD-sealed form of the raw ID token.
func (s *IDTokenSealer) SealIDToken(raw string) (string, error) {
	return s.s.SealData(idTokenAAD, []byte(raw))
}

// OpenIDToken returns the raw ID token, or an error when no configured
// key opens the blob (rotation past retention, tamper, wrong purpose).
func (s *IDTokenSealer) OpenIDToken(blob string) (string, error) {
	pt, err := s.s.OpenData(idTokenAAD, blob)
	if err != nil {
		return "", err
	}
	return string(pt), nil
}
