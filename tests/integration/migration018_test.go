// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

//go:build integration

package integration

import (
	"context"
	"os"
	"testing"
)

// TestMigration018_UpFrom017: a database standing at 017 upgrades to 018 —
// the nullable connection_lease.stream_owner_tab / .stream_owner_epoch /
// .portal_session_digest and launch_ticket.portal_session_digest columns
// land, existing rows stay NULL (no backfill), and replicas predating the
// columns can coexist with them (expand-only, down-compatible).
func TestMigration018_UpFrom017(t *testing.T) {
	db := newDB(t) // fully migrated: 018 already applied
	defer db.Close()
	ctx := context.Background()

	for _, tc := range []struct{ table, column string }{
		{"connection_lease", "stream_owner_tab"},
		{"connection_lease", "stream_owner_epoch"},
		{"connection_lease", "portal_session_digest"},
		{"launch_ticket", "portal_session_digest"},
	} {
		var nullable string
		if err := db.Pool().QueryRow(ctx, `
			SELECT is_nullable FROM information_schema.columns
			WHERE table_name = $1 AND column_name = $2`,
			tc.table, tc.column).Scan(&nullable); err != nil {
			t.Fatalf("column probe %s.%s: %v", tc.table, tc.column, err)
		}
		if nullable != "YES" {
			t.Fatalf("%s.%s is_nullable = %q, want YES", tc.table, tc.column, nullable)
		}
	}

	// Rewind to the 017 shape (columns gone, version unrecorded), then let
	// the runner upgrade — the path a rolling deploy takes.
	if _, err := db.Pool().Exec(ctx, `
		ALTER TABLE connection_lease
			DROP COLUMN stream_owner_tab,
			DROP COLUMN stream_owner_epoch,
			DROP COLUMN portal_session_digest;
		ALTER TABLE launch_ticket DROP COLUMN portal_session_digest;
		DELETE FROM schema_migrations WHERE version = 18`); err != nil {
		t.Fatalf("rewind to 017: %v", err)
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate from 017: %v", err)
	}
	var applied bool
	if err := db.Pool().QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = 18)`).Scan(&applied); err != nil || !applied {
		t.Fatalf("018 not recorded after migrate: applied=%v err=%v", applied, err)
	}

	// The migration file applies twice and the runner re-runs cleanly.
	sql, err := os.ReadFile(repoPath("internal/store/migrations/018_lease_stream_owner.sql"))
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
}

// TestMigration018_OldReplicaCoexists: a lease row written by rc.3 code
// (owner id + owner epoch + issuing session digest set by ticket/claim)
// keeps every column a pre-018 replica reads/writes — the columns are
// additive, so the old INSERT and SELECT lists are untouched. The rc.2
// claim write (epoch bump without naming the columns) neither breaks on
// nor corrupts them; the stored owner just goes stale — readers gate on
// stream_owner_epoch = stream_epoch, so it can never masquerade as the
// current claim's owner.
func TestMigration018_OldReplicaCoexists(t *testing.T) {
	db := newDB(t)
	defer db.Close()
	ctx := context.Background()

	if _, err := db.Pool().Exec(ctx,
		`INSERT INTO workspaces (id, tenant_id, owner_subject, request_id)
		 VALUES ('ws_018', 'tenant-a', 'iss|alice', 'req-018')`); err != nil {
		t.Fatalf("seed workspace row: %v", err)
	}
	// A row the new code wrote: owner id, its epoch and the session digest.
	if _, err := db.Pool().Exec(ctx, `
		INSERT INTO connection_lease
			(id, workspace_id, tenant_id, principal_subject, runtime_generation,
			 runtime_uid, fencing_version, gateway_id, state, expires_at,
			 stream_epoch, stream_owner_tab, stream_owner_epoch, portal_session_digest)
		VALUES ('lease-018', 'ws_018', 'tenant-a', 'iss|alice', 1,
			'rt-018', 1, 'gw-1', 'active', now() + interval '30 seconds',
			3, '0123456789abcdef0123456789abcdef', 3, '\xdeadbeef'::bytea)`); err != nil {
		t.Fatalf("rc.3-shaped insert: %v", err)
	}
	// The rc.2 read path: the pre-018 column list still resolves.
	var epoch int64
	if err := db.Pool().QueryRow(ctx, `
		SELECT stream_epoch FROM connection_lease
		WHERE id = 'lease-018' AND state = 'active'`).Scan(&epoch); err != nil || epoch != 3 {
		t.Fatalf("rc.2 select: epoch=%d err=%v", epoch, err)
	}
	// The rc.2 claim write (epoch bump without naming the columns).
	if _, err := db.Pool().Exec(ctx, `
		UPDATE connection_lease SET stream_epoch = stream_epoch + 1
		WHERE id = 'lease-018' AND state = 'active'`); err != nil {
		t.Fatalf("rc.2 claim update: %v", err)
	}
	var ownerTab *string
	var ownerEpoch *int64
	if err := db.Pool().QueryRow(ctx,
		`SELECT stream_epoch, stream_owner_tab, stream_owner_epoch FROM connection_lease WHERE id = 'lease-018'`).
		Scan(&epoch, &ownerTab, &ownerEpoch); err != nil {
		t.Fatalf("re-read: %v", err)
	}
	// The id survives in storage but is provably stale: owner_epoch 3 !=
	// stream_epoch 4 — the read path reports it as absent.
	if epoch != 4 || ownerTab == nil || *ownerTab != "0123456789abcdef0123456789abcdef" ||
		ownerEpoch == nil || *ownerEpoch != 3 || *ownerEpoch == epoch {
		t.Fatalf("epoch=%d ownerTab=%v ownerEpoch=%v — want 4, stored id, stale 3",
			epoch, ownerTab, ownerEpoch)
	}
}
