//go:build integration

package integration

import (
	"context"
	"os"
	"testing"

	"github.com/tinyorbitvn/tinycdi/internal/store"
)

// TestMigration012_Idempotent: the principal-directory migration applies
// cleanly on top of itself (fresh DB already ran it via Migrate; running
// the file's SQL again must succeed).
func TestMigration012_Idempotent(t *testing.T) {
	db := newDB(t)
	defer db.Close()
	sql, err := os.ReadFile(repoPath("internal/store/migrations/012_principal_directory.sql"))
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	for i := 0; i < 2; i++ {
		if _, err := db.Pool().Exec(context.Background(), string(sql)); err != nil {
			t.Fatalf("apply %d: %v", i+1, err)
		}
	}
	// The full runner is idempotent too.
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
}

// TestDirectory_UpsertOnLogin: Remember upserts on (tenant, owner_ref) —
// a second login with a changed display name updates the row in place.
func TestDirectory_UpsertOnLogin(t *testing.T) {
	db := newDB(t)
	defer db.Close()
	ctx := context.Background()
	dir := store.NewPrincipalDirectory(db)

	ownerRef := "https://issuer.example|user-a"
	e := store.DirectoryEntry{OwnerRef: ownerRef, Subject: "user-a",
		DisplayName: "Alice", Email: "alice@example.com"}
	if err := dir.Remember(ctx, "tenant-a", e); err != nil {
		t.Fatalf("remember 1: %v", err)
	}

	e.DisplayName = "Alice Cooper"
	e.Email = "alice.cooper@example.com"
	if err := dir.Remember(ctx, "tenant-a", e); err != nil {
		t.Fatalf("remember 2: %v", err)
	}

	got, err := dir.Lookup(ctx, "tenant-a", []string{ownerRef})
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	row, ok := got[ownerRef]
	if !ok {
		t.Fatal("directory row missing after upserts")
	}
	if row.DisplayName != "Alice Cooper" || row.Email != "alice.cooper@example.com" {
		t.Fatalf("row not updated: %+v", row)
	}

	var n int
	if err := db.Pool().QueryRow(ctx,
		`SELECT count(*) FROM principal_directory WHERE tenant_id='tenant-a' AND owner_ref=$1`,
		ownerRef).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("rows=%d, want 1 (upsert, not insert)", n)
	}
}

// TestQuotaReport_UnitsAndOwners (store level): Report sums held
// reservations per owner in storage units (bytes), which the API maps to
// millicores/MiB/GiB.
func TestQuotaReport_UnitsAndOwners(t *testing.T) {
	db := newDB(t)
	defer db.Close()
	ctx := context.Background()

	// Seed: tenant-a quota row + two active workspaces with held
	// reservations and one released reservation that must not count.
	if _, err := db.Pool().Exec(ctx, `
		INSERT INTO tenant_quota (tenant_id, max_running_slots, max_cpu_millis, max_memory_bytes, max_disk_bytes)
		VALUES ('tenant-a', 8, 16000, 34359738368, 107374182400)`); err != nil {
		t.Fatalf("seed quota: %v", err)
	}
	if _, err := db.Pool().Exec(ctx, `
		INSERT INTO workspaces (id, tenant_id, owner_subject, request_id, state) VALUES
		('ws_0000000000000000000000000a', 'tenant-a', 'iss|user-a', 'req-a1', 'active'),
		('ws_0000000000000000000000000b', 'tenant-a', 'iss|user-b', 'req-b1', 'active'),
		('ws_0000000000000000000000000c', 'tenant-a', 'iss|user-a', 'req-a2', 'deleted')`); err != nil {
		t.Fatalf("seed workspaces: %v", err)
	}
	if _, err := db.Pool().Exec(ctx, `
		INSERT INTO quota_reservation (workspace_id, tenant_id, running_slots, cpu_millis, memory_bytes, disk_bytes, state) VALUES
		('ws_0000000000000000000000000a', 'tenant-a', 1, 2000, 4294967296, 21474836480, 'held'),
		('ws_0000000000000000000000000b', 'tenant-a', 1, 4000, 8589934592, 42949672960, 'held'),
		('ws_0000000000000000000000000c', 'tenant-a', 1, 1000, 1073741824, 10737418240, 'held')`); err != nil {
		t.Fatalf("seed reservations: %v", err)
	}

	rep, err := store.NewQuotaReader(db).Report(ctx, "tenant-a")
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if !rep.HasLimits {
		t.Fatal("expected limits")
	}
	if rep.Limits.CPUMillis != 16000 || rep.Limits.MemoryBytes != 34359738368 || rep.Limits.DiskBytes != 107374182400 {
		t.Fatalf("limits: %+v", rep.Limits)
	}
	// Usage counts active workspaces and held reservations (incl. the
	// still-held reservation of the deleted workspace).
	if rep.Usage.Workspaces != 2 || rep.Usage.RunningSlots != 3 ||
		rep.Usage.CPUMillis != 7000 ||
		rep.Usage.MemoryBytes != 4294967296+8589934592+1073741824 ||
		rep.Usage.DiskBytes != 21474836480+42949672960+10737418240 {
		t.Fatalf("usage: %+v", rep.Usage)
	}
	if len(rep.Owners) != 2 {
		t.Fatalf("owners=%d, want 2", len(rep.Owners))
	}
	if rep.Owners[0].OwnerRef != "iss|user-a" || rep.Owners[0].Usage.Workspaces != 1 {
		t.Fatalf("owner[0]: %+v", rep.Owners[0])
	}
}
