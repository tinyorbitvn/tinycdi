// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package backend

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"
)

func TestAuditRetentionLoop_PrunesOldRowsAtStart(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	for _, age := range []string{"100 days", "1 day"} {
		if _, err := db.Pool().Exec(ctx, `
			INSERT INTO audit_event (tenant, actor_ref, action, outcome, created_at)
			VALUES ('t', 'a', 'x', 'success', now() - $1::interval)`, age); err != nil {
			t.Fatal(err)
		}
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		auditRetentionLoop(db, 90, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))(runCtx)
		close(done)
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var n int
		_ = db.Pool().QueryRow(ctx, `SELECT count(*) FROM audit_event`).Scan(&n)
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("audit_event rows = %d after 10s, want 1 (the 100-day-old row pruned, the 1-day-old kept)", n)
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	<-done
}
