// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/store"
)

func transferRow(id, ws string, expires time.Time) store.FileTransfer {
	return store.FileTransfer{
		ID: id, WorkspaceID: ws, Tenant: "t1", PrincipalIssuer: "https://idp", PrincipalSubject: "alice",
		RuntimeGeneration: 1, RuntimeUID: "uid-1", Direction: "upload", Path: "report.pdf",
		DeclaredBytes: 10, ResolvedPolicy: "Bidirectional", ExpiresAt: expires,
	}
}

func transferStatus(t *testing.T, db *store.DB, id string) string {
	t.Helper()
	var s string
	if err := db.Pool().QueryRow(context.Background(), `SELECT status FROM session_file_transfer WHERE id = $1`, id).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestFileTransfer_InsertAndSweep(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	now := time.Now()
	seedWorkspace(t, db, "t1", "https://idp|alice", "ws-x")
	for _, r := range []store.FileTransfer{
		transferRow("xf_live", "ws-x", now.Add(time.Minute)),
		transferRow("xf_stale", "ws-x", now.Add(-time.Second)),
		transferRow("xf_dead", "ws-x", now.Add(time.Minute)),
		transferRow("xf_old", "ws-x", now.Add(-8*24*time.Hour)),
	} {
		if err := store.InsertFileTransfer(ctx, db, r); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Pool().Exec(ctx, `UPDATE session_file_transfer SET status = 'claimed', claimed_at = $1 WHERE id = 'xf_dead'`, now.Add(-31*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool().Exec(ctx, `UPDATE session_file_transfer SET status = 'completed', created_at = $1 WHERE id = 'xf_old'`, now.Add(-8*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	expired, failed, pruned, err := db.SweepFileTransfers(ctx, now)
	if err != nil || expired != 1 || failed != 1 || pruned != 1 {
		t.Fatalf("sweep = %d/%d/%d %v, want 1/1/1", expired, failed, pruned, err)
	}
	if transferStatus(t, db, "xf_live") != "pending" || transferStatus(t, db, "xf_stale") != "expired" || transferStatus(t, db, "xf_dead") != "failed" {
		t.Fatal("wrong states after sweep")
	}
	if err := store.InsertFileTransfer(ctx, db, transferRow("xf_live", "ws-x", now)); err == nil {
		t.Fatal("duplicate id accepted")
	}
}
