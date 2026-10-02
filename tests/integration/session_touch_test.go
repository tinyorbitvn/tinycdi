//go:build integration

// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package integration

import (
	"context"
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
		name string
		sql  string
		idle bool
	}{
		{"idle window", store.TouchPrincipalIdleSQL, true},
		{"no idle window", store.TouchPrincipalSQL, false},
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
		if !strings.Contains(plan.String(), "sessions_issuer_subject") {
			t.Errorf("%s: plan does not use the (issuer, subject) index:\n%s", tc.name, plan.String())
		}
	}
}

// TestTouchPrincipal_SplitsAtFirstSeparator: the principal "issuer|subject"
// is split at the FIRST '|' (an issuer URL never contains one), so a subject
// containing '|' still matches its own sessions — and only those.
func TestTouchPrincipal_SplitsAtFirstSeparator(t *testing.T) {
	db := newDB(t)
	ss := store.NewSessionStore(db, time.Hour)
	ctx := context.Background()
	old := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Millisecond)
	save := func(id, iss, sub string) {
		t.Helper()
		if err := ss.Save(ctx, &store.Session{
			ID: id, Issuer: iss, Subject: sub, TenantID: "tenant-a", CSRFToken: "c",
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
