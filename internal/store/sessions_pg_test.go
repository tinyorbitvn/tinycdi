// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/store"
)

func countSessions(t *testing.T, db *store.DB) int {
	t.Helper()
	var n int
	if err := db.Pool().QueryRow(context.Background(), `SELECT count(*) FROM sessions`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestSessionStore_MissNeverWrites (R1-03): a read that finds no live
// session writes nothing — an expired row stays until the sweep, so a
// forged-cookie spray never turns into DELETE traffic.
func TestSessionStore_MissNeverWrites(t *testing.T) {
	db := newDB(t)
	ss := store.NewSessionStore(db, time.Minute, nil)
	ctx := context.Background()
	now := time.Now().UTC()
	expired := &store.Session{ID: "sess-expired", Issuer: "iss", Subject: "sub", TenantID: "tenant-a",
		CreatedAt: now.Add(-2 * time.Hour), LastSeenAt: now.Add(-2 * time.Hour), ExpiresAt: now.Add(-time.Hour)}
	if err := ss.Save(ctx, expired); err != nil {
		t.Fatal(err)
	}
	for _, read := range []func(context.Context, string) (*store.Session, error){ss.Get, ss.Peek} {
		if _, err := read(ctx, "sess-expired"); !errors.Is(err, store.ErrSessionNotFound) {
			t.Fatalf("read expired = %v, want ErrSessionNotFound", err)
		}
	}
	if n := countSessions(t, db); n != 1 {
		t.Fatalf("sessions rows = %d after missed reads, want 1 (the read path must not delete)", n)
	}
}

// TestPruneDeadSessions_RemovesOnlyDeadRows (R1-03): the leader sweep
// deletes absolute-expired, idle-expired and stale-epoch rows and keeps a
// live session.
func TestPruneDeadSessions_RemovesOnlyDeadRows(t *testing.T) {
	db := newDB(t)
	idle := 30 * time.Minute
	ss := store.NewSessionStore(db, idle, nil)
	ctx := context.Background()
	now := time.Now().UTC()
	mk := func(id string, lastSeen, expires time.Time) {
		t.Helper()
		if err := ss.Save(ctx, &store.Session{ID: id, Issuer: "iss", Subject: id, TenantID: "tenant-a",
			CreatedAt: lastSeen, LastSeenAt: lastSeen, ExpiresAt: expires}); err != nil {
			t.Fatal(err)
		}
	}
	mk("sess-live", now, now.Add(time.Hour))
	mk("sess-absolute", now, now.Add(-time.Minute))
	mk("sess-idle", now.Add(-2*time.Hour), now.Add(time.Hour))
	mk("sess-stale", now, now.Add(time.Hour))
	if _, err := db.Pool().Exec(ctx,
		`UPDATE sessions SET epoch = 'stale-epoch' WHERE id = encode(sha256($1::bytea), 'hex')`, "sess-stale"); err != nil {
		t.Fatal(err)
	}
	n, err := db.PruneDeadSessions(ctx, idle, 500)
	if err != nil || n != 3 {
		t.Fatalf("PruneDeadSessions = %d, %v; want 3, nil", n, err)
	}
	if got := countSessions(t, db); got != 1 {
		t.Fatalf("rows left = %d, want 1", got)
	}
	if _, err := ss.Peek(ctx, "sess-live"); err != nil {
		t.Fatalf("live session gone: %v", err)
	}
}
