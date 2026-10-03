// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

//go:build integration

package integration

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/store"
)

// TestMigration017_Idempotent: sessions.csrf_token stays but is NULLable
// after Migrate — the expand half of the expand/contract (a rolling
// upgrade's surviving v0.2 replicas still SELECT/INSERT it; the column
// itself drops in v0.4). The migration file applies twice and the runner
// re-runs cleanly.
func TestMigration017_Idempotent(t *testing.T) {
	db := newDB(t)
	defer db.Close()
	ctx := context.Background()

	var nullable string
	if err := db.Pool().QueryRow(ctx, `
		SELECT is_nullable FROM information_schema.columns
		WHERE table_name = 'sessions' AND column_name = 'csrf_token'`).Scan(&nullable); err != nil {
		t.Fatalf("column probe: %v", err)
	}
	if nullable != "YES" {
		t.Fatalf("sessions.csrf_token is_nullable = %q, want YES (column kept, NOT NULL dropped)", nullable)
	}

	sql, err := os.ReadFile(repoPath("internal/store/migrations/017_sessions_drop_csrf.sql"))
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	for i := 0; i < 2; i++ {
		if _, err := db.Pool().Exec(ctx, string(sql)); err != nil {
			t.Fatalf("apply %d: %v", i+1, err)
		}
	}
	// The full runner is idempotent too.
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
}

// TestMigration017_OldAndNewReplicasCoexist: during a rolling upgrade a
// v0.3 replica INSERTs sessions without naming csrf_token while a
// surviving v0.2 replica still INSERTs and SELECTs it — the nullable
// column must accept both write shapes until it drops in v0.4 (E14).
func TestMigration017_OldAndNewReplicasCoexist(t *testing.T) {
	db := newDB(t)
	defer db.Close()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)

	// v0.3 write path: Save names every column except csrf_token.
	ss := store.NewSessionStore(db, time.Minute, nil)
	if err := ss.Save(ctx, &store.Session{
		ID: "sess-v03", Issuer: "iss", Subject: "sub-v03", TenantID: "tenant-a",
		CreatedAt: now, LastSeenAt: now, ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatalf("v0.3 save with column present: %v", err)
	}

	// v0.2 write path: the same column list plus csrf_token (the HMAC of
	// the empty derived token it used to persist).
	if _, err := db.Pool().Exec(ctx, `
		INSERT INTO sessions (id, issuer, subject, tenant_id, groups,
			csrf_token, display_name, email, created_at, last_seen_at, expires_at, epoch, id_token)
		VALUES ('sess-v02-digest', 'iss', 'sub-v02', 'tenant-a', '[]'::jsonb,
			'legacy-mac', '', '', $1, $1, $2, '', NULL)`, now, now.Add(time.Hour)); err != nil {
		t.Fatalf("v0.2-style insert naming csrf_token: %v", err)
	}

	// The old replica's SELECT of csrf_token still resolves: NULL for the
	// v0.3 row, the stored value for its own.
	var newTok, oldTok *string
	if err := db.Pool().QueryRow(ctx,
		`SELECT csrf_token FROM sessions WHERE subject='sub-v03'`).Scan(&newTok); err != nil {
		t.Fatalf("select v0.3 row: %v", err)
	}
	if err := db.Pool().QueryRow(ctx,
		`SELECT csrf_token FROM sessions WHERE subject='sub-v02'`).Scan(&oldTok); err != nil {
		t.Fatalf("select v0.2 row: %v", err)
	}
	if newTok != nil {
		t.Fatalf("v0.3 row csrf_token = %q, want NULL", *newTok)
	}
	if oldTok == nil || *oldTok != "legacy-mac" {
		t.Fatalf("v0.2 row csrf_token = %v, want 'legacy-mac'", oldTok)
	}
}
