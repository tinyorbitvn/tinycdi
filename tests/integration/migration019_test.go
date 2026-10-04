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

// csrfColumnExists reports whether sessions.csrf_token is still there.
func csrfColumnExists(t *testing.T, db *store.DB) bool {
	t.Helper()
	var exists bool
	if err := db.Pool().QueryRow(context.Background(), `
		SELECT EXISTS (SELECT 1 FROM information_schema.columns
		WHERE table_name = 'sessions' AND column_name = 'csrf_token')`).Scan(&exists); err != nil {
		t.Fatalf("column probe: %v", err)
	}
	return exists
}

// TestMigration019_UpFromV031: a database standing at the v0.3.1 shape —
// all migrations through 018 applied, sessions.csrf_token present and
// nullable, real session rows in both write shapes — upgrades to v0.4:
// the runner applies 019, drops the column, keeps every row, and the
// session store keeps working on the migrated schema. This is the path
// every supported v0.3.x -> v0.4 deploy takes.
func TestMigration019_UpFromV031(t *testing.T) {
	db := newDB(t) // fully migrated: 019 already applied
	defer db.Close()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)

	// Rewind to the v0.3.1 shape (csrf_token back, nullable; 019
	// unrecorded) and seed it the way a live v0.3.1 database looks.
	rewindToV031Shape(t, db, false)
	if !csrfColumnExists(t, db) {
		t.Fatal("rewound schema lacks csrf_token")
	}

	ss := store.NewSessionStore(db, time.Minute, nil)
	if err := ss.Save(ctx, &store.Session{
		ID: "sess-live", Issuer: "iss", Subject: "sub-live", TenantID: "tenant-a",
		DisplayName: "Live User", Email: "live@example.com",
		CreatedAt:   now, LastSeenAt: now, ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatalf("seed v0.3 session: %v", err)
	}
	// A row the nullable column still carried data on: written by a
	// legacy replica during the v0.3 rolling window.
	if _, err := db.Pool().Exec(ctx, `
		INSERT INTO sessions (id, issuer, subject, tenant_id, groups,
			csrf_token, display_name, email, created_at, last_seen_at, expires_at, epoch, id_token)
		VALUES ('sess-legacy-digest', 'iss', 'sub-legacy', 'tenant-a', '[]'::jsonb,
			'legacy-mac', 'Legacy', '', $1, $1, $2, '', NULL)`, now, now.Add(time.Hour)); err != nil {
		t.Fatalf("seed legacy session: %v", err)
	}

	// The upgrade: the runner applies 019 and nothing else.
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate v0.3.1 -> v0.4: %v", err)
	}
	if csrfColumnExists(t, db) {
		t.Fatal("sessions.csrf_token still present after 019")
	}
	var applied bool
	if err := db.Pool().QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = 19)`).Scan(&applied); err != nil || !applied {
		t.Fatalf("019 not recorded after migrate: applied=%v err=%v", applied, err)
	}

	// Every row survived the drop.
	var n int
	if err := db.Pool().QueryRow(ctx, `SELECT count(*) FROM sessions`).Scan(&n); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if n != 2 {
		t.Fatalf("sessions row count = %d, want 2", n)
	}
	var subject string
	if err := db.Pool().QueryRow(ctx,
		`SELECT subject FROM sessions WHERE id = 'sess-legacy-digest'`).Scan(&subject); err != nil || subject != "sub-legacy" {
		t.Fatalf("legacy row lost: subject=%q err=%v", subject, err)
	}

	// Sessions keep working on the contracted schema: the row written
	// before the drop reads back, and new writes succeed.
	got, err := ss.Get(ctx, "sess-live")
	if err != nil {
		t.Fatalf("get pre-drop session: %v", err)
	}
	if got.Subject != "sub-live" || got.DisplayName != "Live User" {
		t.Fatalf("pre-drop session corrupt: %+v", got)
	}
	if err := ss.Save(ctx, &store.Session{
		ID: "sess-post", Issuer: "iss", Subject: "sub-post", TenantID: "tenant-a",
		CreatedAt: now, LastSeenAt: now, ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatalf("save post-drop session: %v", err)
	}
	if _, err := ss.Get(ctx, "sess-post"); err != nil {
		t.Fatalf("get post-drop session: %v", err)
	}
}

// TestMigration019_Idempotent: the DROP COLUMN IF EXISTS file applies
// twice and the runner re-runs cleanly on the contracted schema.
func TestMigration019_Idempotent(t *testing.T) {
	db := newDB(t)
	defer db.Close()
	ctx := context.Background()

	if csrfColumnExists(t, db) {
		t.Fatal("csrf_token present on a fully-migrated schema")
	}
	sql, err := os.ReadFile(repoPath("internal/store/migrations/019_sessions_drop_csrf_column.sql"))
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	for i := 0; i < 2; i++ {
		if _, err := db.Pool().Exec(ctx, string(sql)); err != nil {
			t.Fatalf("apply %d: %v", i+1, err)
		}
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	if csrfColumnExists(t, db) {
		t.Fatal("csrf_token reappeared")
	}
}
