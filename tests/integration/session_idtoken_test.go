//go:build integration

// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package integration

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/api"
	"github.com/tinyorbitvn/tinycdi/internal/api/loginstate"
	"github.com/tinyorbitvn/tinycdi/internal/store"
)

func testKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return k
}

func testIDTokenSealer(t *testing.T, keys ...[]byte) *api.IDTokenSealer {
	t.Helper()
	s, err := loginstate.NewSealer(keys...)
	if err != nil {
		t.Fatal(err)
	}
	return api.NewIDTokenSealer(s)
}

// V3.24: the session's retained OIDC ID token persists only AEAD-sealed
// (login-state keys, purpose-bound additional data) — never plaintext —
// while Get returns the raw token for logout's id_token_hint.
func TestPGSessionStore_IDTokenSealedRoundTrip(t *testing.T) {
	db := newDB(t)
	key := testKey(t)
	ss := store.NewSessionStore(db, time.Hour, testIDTokenSealer(t, key))
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)

	raw := "header.payload.signature-id-token"
	sess := &store.Session{
		ID: "sess-idtok", Issuer: "iss", Subject: "sub", TenantID: "tenant-a",
		CSRFToken: "csrf", IDToken: raw,
		CreatedAt: now, LastSeenAt: now, ExpiresAt: now.Add(time.Hour),
	}
	if err := ss.Save(ctx, sess); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := ss.Get(ctx, "sess-idtok")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.IDToken != raw {
		t.Fatalf("IDToken = %q, want the raw token", got.IDToken)
	}

	// The row carries only the sealed blob: not NULL, not the raw token,
	// and not containing it.
	sum := sha256Hex("sess-idtok")
	var rowToken *string
	if err := db.Pool().QueryRow(ctx,
		`SELECT id_token FROM sessions WHERE id = $1`, sum).Scan(&rowToken); err != nil {
		t.Fatalf("id_token read: %v", err)
	}
	if rowToken == nil {
		t.Fatal("id_token column NULL for a session that carried a token")
	}
	if *rowToken == raw || strings.Contains(*rowToken, raw) {
		t.Fatalf("id_token persisted raw: %q", *rowToken)
	}
	if strings.Contains(*rowToken, "header.payload") {
		t.Fatalf("id_token looks partially plaintext: %q", *rowToken)
	}
}

// A key rotation that removes the sealing key must not break the session
// read or the sign-out: Get returns the session with an empty IDToken, so
// logout falls back to a client_id-only end-session URL.
func TestPGSessionStore_IDTokenRotationFallback(t *testing.T) {
	db := newDB(t)
	oldKey, newKey := testKey(t), testKey(t)
	sealOld := store.NewSessionStore(db, time.Hour, testIDTokenSealer(t, oldKey))
	sealNew := store.NewSessionStore(db, time.Hour, testIDTokenSealer(t, newKey))
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)

	if err := sealOld.Save(ctx, &store.Session{
		ID: "sess-rot", Issuer: "iss", Subject: "sub", TenantID: "tenant-a",
		CSRFToken: "csrf", IDToken: "raw-id-token",
		CreatedAt: now, LastSeenAt: now, ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := sealNew.Get(ctx, "sess-rot")
	if err != nil {
		t.Fatalf("get under rotated keys must not fail: %v", err)
	}
	if got.IDToken != "" {
		t.Fatalf("IDToken = %q, want empty after rotation", got.IDToken)
	}
	if got.Subject != "sub" {
		t.Fatalf("session lost on rotation: %+v", got)
	}

	// A store with both keys (rotation window) still opens the old blob.
	both := store.NewSessionStore(db, time.Hour, testIDTokenSealer(t, newKey, oldKey))
	got, err = both.Get(ctx, "sess-rot")
	if err != nil {
		t.Fatalf("get during rotation: %v", err)
	}
	if got.IDToken != "raw-id-token" {
		t.Fatalf("IDToken = %q during rotation", got.IDToken)
	}
}

// A session row that predates the id_token column (or was saved without
// one) reads back with an empty IDToken and a NULL column.
func TestPGSessionStore_IDTokenLegacyNull(t *testing.T) {
	db := newDB(t)
	ss := store.NewSessionStore(db, time.Hour, testIDTokenSealer(t, testKey(t)))
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)

	if err := ss.Save(ctx, &store.Session{
		ID: "sess-legacy", Issuer: "iss", Subject: "sub", TenantID: "tenant-a",
		CSRFToken: "csrf",
		CreatedAt: now, LastSeenAt: now, ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	var rowToken *string
	if err := db.Pool().QueryRow(ctx,
		`SELECT id_token FROM sessions WHERE id = $1`, sha256Hex("sess-legacy")).Scan(&rowToken); err != nil {
		t.Fatal(err)
	}
	if rowToken != nil {
		t.Fatalf("id_token = %q, want NULL for a session without one", *rowToken)
	}
	got, err := ss.Get(ctx, "sess-legacy")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.IDToken != "" {
		t.Fatalf("IDToken = %q, want empty", got.IDToken)
	}
}

// sha256Hex is the store's session-key digest (hex SHA-256 of the raw ID).
func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}
