//go:build integration

// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package integration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/store"
)

// touchArgs builds EXPLAIN arguments for whichever shape the statement has:
// either one principal parameter or issuer + subject parameters, followed by
// the idle interval for the idle variant.
func touchArgs(sql string, idle bool) []any {
	args := []any{"https://idp.test"}
	if strings.Contains(sql, "$3") || (!idle && strings.Contains(sql, "$2")) {
		args = append(args, "user-1")
	} else {
		args[0] = "https://idp.test|user-1"
	}
	if idle {
		args = append(args, "60000ms")
	}
	return args
}

// TestTouchPrincipal_UsesIndex: the input hook's UPDATE must be able to use
// an index on sessions(issuer, subject). Matching on the concatenation
// issuer || '|' || subject cannot — every input event would then scan the
// whole sessions table. With sequential scans disabled the planner picks the
// index whenever the predicate allows it.
func TestTouchPrincipal_UsesIndex(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		sql   string
		idle  bool
		index string
	}{
		{"principal idle window", store.TouchPrincipalIdleSQL, true, "sessions_issuer_subject"},
		{"principal no idle window", store.TouchPrincipalSQL, false, "sessions_issuer_subject"},
		{"digest idle window", store.TouchSessionDigestIdleSQL, true, "sessions_pkey"},
		{"digest no idle window", store.TouchSessionDigestSQL, false, "sessions_pkey"},
	} {
		tx, err := db.Pool().Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `SET LOCAL enable_seqscan = off`); err != nil {
			t.Fatal(err)
		}
		rows, err := tx.Query(ctx, "EXPLAIN "+tc.sql, touchArgs(tc.sql, tc.idle)...)
		if err != nil {
			t.Fatalf("%s: explain: %v", tc.name, err)
		}
		var plan strings.Builder
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatal(err)
			}
			plan.WriteString(line + "\n")
		}
		rows.Close()
		_ = tx.Rollback(ctx)
		if !strings.Contains(plan.String(), tc.index) {
			t.Errorf("%s: plan does not use %s:\n%s", tc.name, tc.index, plan.String())
		}
	}
}

// TestTouchPrincipal_SplitsAtFirstSeparator: the principal "issuer|subject"
// is split at the FIRST '|' (an issuer URL never contains one), so a subject
// containing '|' still matches its own sessions — and only those.
func TestTouchPrincipal_SplitsAtFirstSeparator(t *testing.T) {
	db := newDB(t)
	ss := store.NewSessionStore(db, time.Hour, nil)
	ctx := context.Background()
	old := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Millisecond)
	save := func(id, iss, sub string) {
		t.Helper()
		if err := ss.Save(ctx, &store.Session{
			ID: id, Issuer: iss, Subject: sub, TenantID: "tenant-a",
			CreatedAt: old, LastSeenAt: old, ExpiresAt: time.Now().Add(time.Hour),
		}); err != nil {
			t.Fatalf("save %s: %v", id, err)
		}
	}
	save("s-plain", "https://idp.test", "user-1")
	save("s-pipe", "https://idp.test", "user|with|pipes")
	save("s-other", "https://idp.test", "user-2")

	n, err := ss.TouchPrincipal(ctx, "https://idp.test|user|with|pipes")
	if err != nil || n != 1 {
		t.Fatalf("pipe subject: touched %d err=%v, want 1", n, err)
	}
	n, err = ss.TouchPrincipal(ctx, "https://idp.test|user-1")
	if err != nil || n != 1 {
		t.Fatalf("plain subject: touched %d err=%v, want 1", n, err)
	}
	if n, err = ss.TouchPrincipal(ctx, "no-separator"); err != nil || n != 0 {
		t.Fatalf("malformed principal: touched %d err=%v, want 0", n, err)
	}
	var seen time.Time
	if err := db.Pool().QueryRow(ctx, `SELECT last_seen_at FROM sessions WHERE subject = 'user-2'`).Scan(&seen); err != nil {
		t.Fatal(err)
	}
	if !seen.Equal(old) {
		t.Fatal(fmt.Sprintf("untouched session slid: %v != %v", seen, old))
	}
}

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
