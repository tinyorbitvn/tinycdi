//go:build integration

package integration

import (
	"context"
	"os"
	"testing"
)

// TestMigration017_Idempotent: sessions.csrf_token is gone after Migrate and
// applying the migration file's SQL again still succeeds (E14; the v0.3
// rename of the spec's 013_drop_session_csrf — B5.3 assigns the next free
// number).
func TestMigration017_Idempotent(t *testing.T) {
	db := newDB(t)
	defer db.Close()
	ctx := context.Background()

	var exists bool
	if err := db.Pool().QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_name = 'sessions' AND column_name = 'csrf_token')`).Scan(&exists); err != nil {
		t.Fatalf("column probe: %v", err)
	}
	if exists {
		t.Fatal("sessions.csrf_token still present after migrate")
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
