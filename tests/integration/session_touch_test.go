//go:build integration

// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package integration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/store"
)

// TestTouchSessionDigest_ScopesToBoundSession (SR-1-F3): the digest-scoped
// touch slides exactly the one session the lease recorded — a second live
// session of the same principal keeps its own last_seen_at, and a session
// already past the idle window is never revived.
func TestTouchSessionDigest_ScopesToBoundSession(t *testing.T) {
	db := newDB(t)
	ss := store.NewSessionStore(db, time.Hour, nil)
	ctx := context.Background()
	old := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Millisecond)
	save := func(id string, lastSeen time.Time) {
		t.Helper()
		if err := ss.Save(ctx, &store.Session{
			ID: id, Issuer: "https://idp.test", Subject: "user-1", TenantID: "tenant-a",
			CreatedAt: old, LastSeenAt: lastSeen, ExpiresAt: time.Now().Add(time.Hour),
		}); err != nil {
			t.Fatalf("save %s: %v", id, err)
		}
	}
	save("s-bound", old)
	save("s-sibling", old)

	digestOf := func(id string) string { d := sha256.Sum256([]byte(id)); return hex.EncodeToString(d[:]) }
	digest := digestOf("s-bound")
	n, err := ss.TouchSessionDigest(ctx, digest)
	if err != nil || n != 1 {
		t.Fatalf("touch bound session: n=%d err=%v, want 1", n, err)
	}
	var seen time.Time
	if err := db.Pool().QueryRow(ctx,
		`SELECT last_seen_at FROM sessions WHERE subject = 'user-1' AND last_seen_at <> $1`, old).
		Scan(&seen); err != nil {
		t.Fatalf("bound session not touched: %v", err)
	}
	var sibling time.Time
	if err := db.Pool().QueryRow(ctx,
		`SELECT last_seen_at FROM sessions WHERE id = $1`,
		digestOf("s-sibling")).Scan(&sibling); err != nil {
		t.Fatal(err)
	}
	if !sibling.Equal(old) {
		t.Fatalf("sibling session slid: %v != %v", sibling, old)
	}

	// An idle-dead session is never revived: age the bound row past the
	// window, then touch — zero rows.
	if _, err := db.Pool().Exec(ctx,
		`UPDATE sessions SET last_seen_at = now() - interval '2 hours'
		 WHERE id = $1`, digest); err != nil {
		t.Fatal(err)
	}
	if n, err = ss.TouchSessionDigest(ctx, digest); err != nil || n != 0 {
		t.Fatalf("idle-dead session touched: n=%d err=%v, want 0", n, err)
	}
}
