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

// rewindToV031Shape returns a fully-migrated database to the v0.3.1
// shape: sessions.csrf_token back as a nullable column (as 017 left it)
// and the 019 row of schema_migrations removed, so a following
// db.Migrate re-applies 019 and restores the true v0.4 shape. Use
// notNull=true to also restore the pre-017 NOT NULL (the v0.2 shape the
// expand step ran against).
func rewindToV031Shape(t *testing.T, db *store.DB, notNull bool) {
	t.Helper()
	ctx := context.Background()
	ddl := `ALTER TABLE sessions ADD COLUMN csrf_token text`
	if notNull {
		ddl += ` NOT NULL`
	}
	if _, err := db.Pool().Exec(ctx, ddl); err != nil {
		t.Fatalf("rewind: re-add csrf_token: %v", err)
	}
	if _, err := db.Pool().Exec(ctx,
		`DELETE FROM schema_migrations WHERE version = 19`); err != nil {
		t.Fatalf("rewind: unrecord 019: %v", err)
	}
}

// TestMigration017_Idempotent: the expand file still applies cleanly in
// the chain — on a pre-017 shape it drops NOT NULL, applying it twice is
// a no-op, and the runner then contracts the column via 019.
func TestMigration017_Idempotent(t *testing.T) {
	db := newDB(t)
	defer db.Close()
	ctx := context.Background()

	// Stand at the v0.2 shape 017 ran against: column present, NOT NULL.
	rewindToV031Shape(t, db, true)

	sql, err := os.ReadFile(repoPath("internal/store/migrations/017_sessions_drop_csrf.sql"))
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	for i := 0; i < 2; i++ {
		if _, err := db.Pool().Exec(ctx, string(sql)); err != nil {
			t.Fatalf("apply %d: %v", i+1, err)
		}
	}
	var nullable string
	if err := db.Pool().QueryRow(ctx, `
		SELECT is_nullable FROM information_schema.columns
		WHERE table_name = 'sessions' AND column_name = 'csrf_token'`).Scan(&nullable); err != nil {
		t.Fatalf("column probe: %v", err)
	}
	if nullable != "YES" {
		t.Fatalf("sessions.csrf_token is_nullable = %q, want YES (NOT NULL dropped)", nullable)
	}

	// The runner contracts the column (019) and re-runs cleanly.
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate after rewind: %v", err)
	}
	var exists bool
	if err := db.Pool().QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM information_schema.columns
		WHERE table_name = 'sessions' AND column_name = 'csrf_token')`).Scan(&exists); err != nil {
		t.Fatalf("column probe after contract: %v", err)
	}
	if exists {
		t.Fatal("sessions.csrf_token still present after re-migrate, want dropped by 019")
	}
}

// TestMigration017_OldAndNewReplicasCoexist: on the expand-step schema
// (nullable csrf_token, as every v0.3.x release leaves it) a replica
// INSERTing sessions without naming the column coexists with a legacy
// replica that still INSERTs and SELECTs it — the property that carried
// the v0.2 -> v0.3 rolling window (E14). v0.4 then contracts the column:
// once 019 applies, a legacy-shaped write naming csrf_token fails, which
// is why upgrades must pass through v0.3.x.
func TestMigration017_OldAndNewReplicasCoexist(t *testing.T) {
	db := newDB(t)
	defer db.Close()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)

	// The v0.3.1 schema shape: csrf_token present, nullable.
	rewindToV031Shape(t, db, false)

	// New-replica write path: Save names every column except csrf_token.
	ss := store.NewSessionStore(db, time.Minute, nil)
	if err := ss.Save(ctx, &store.Session{
		ID: "sess-v03", Issuer: "iss", Subject: "sub-v03", TenantID: "tenant-a",
		CreatedAt: now, LastSeenAt: now, ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatalf("v0.3 save with column present: %v", err)
	}

	// Legacy-replica write path: the same column list plus csrf_token
	// (the HMAC of the empty derived token it used to persist).
	if _, err := db.Pool().Exec(ctx, `
		INSERT INTO sessions (id, issuer, subject, tenant_id, groups,
			csrf_token, display_name, email, created_at, last_seen_at, expires_at, epoch, id_token)
		VALUES ('sess-v02-digest', 'iss', 'sub-v02', 'tenant-a', '[]'::jsonb,
			'legacy-mac', '', '', $1, $1, $2, '', NULL)`, now, now.Add(time.Hour)); err != nil {
		t.Fatalf("legacy-style insert naming csrf_token: %v", err)
	}

	// The legacy replica's SELECT of csrf_token still resolves: NULL for
	// the new-shaped row, the stored value for its own.
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

	// Contract: 019 drops the column. New-shaped sessions keep working;
	// a legacy-shaped write naming csrf_token now fails — the reason a
	// v0.2 replica cannot survive past the v0.3.x window.
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate after rewind: %v", err)
	}
	if err := ss.Save(ctx, &store.Session{
		ID: "sess-v04", Issuer: "iss", Subject: "sub-v04", TenantID: "tenant-a",
		CreatedAt: now, LastSeenAt: now, ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatalf("v0.4 save after drop: %v", err)
	}
	if _, err := db.Pool().Exec(ctx,
		`UPDATE sessions SET csrf_token = 'x' WHERE subject = 'sub-v02'`); err == nil {
		t.Fatal("legacy-shaped write naming csrf_token succeeded after drop")
	}
}
