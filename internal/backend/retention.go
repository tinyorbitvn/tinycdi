// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package backend

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/observability"
	"github.com/tinyorbitvn/tinycdi/internal/store"
)

// terminalRetentionInterval is the sweep cadence. Rows become eligible a
// full retention window (days) after going terminal, so an hourly pass
// is far finer than the signal.
const terminalRetentionInterval = time.Hour

// terminalRetentionBatch bounds one DELETE statement: small enough that
// no batch meaningfully contends with the redeem/sign-out paths sharing
// the tables; the pass loops batches until the window is clear.
const terminalRetentionBatch = 500

// terminalRetentionSweepLoop is the leader-gated singleton that prunes
// terminal connection_lease and launch_ticket rows past
// -terminal-retention-days (SR-4-F5). Both tables are append-mostly:
// every revocation index and the dead-cookie lookup exist because
// nothing ever removed dead rows — the sweep bounds the growth so those
// indexes stay cheap and any future index build stops scaling with
// deployment age. Registered on b.singletons, it runs only on the
// replica holding the Postgres advisory lock — the deletes are
// idempotent, so the gating keeps every replica from paying the scan,
// not the correctness.
//
// One INFO line per pass carries the per-table counts; pruning is
// housekeeping, not a security event, so it is not audited. The counter
// and last-success gauge only move when the metrics listener is on.
func terminalRetentionSweepLoop(db *store.DB, days int, m *observability.Metrics, log *slog.Logger) func(ctx context.Context) {
	return func(ctx context.Context) {
		retention := time.Duration(days) * 24 * time.Hour
		sweep := func() {
			res, err := db.SweepTerminalConnectionRows(ctx, retention, terminalRetentionBatch)
			switch {
			case err != nil && errors.Is(err, context.Canceled):
			case err != nil:
				log.Error("terminal row retention sweep failed", "err", err)
			default:
				log.Info("terminal row retention sweep",
					"leases_pruned", res.Leases, "tickets_pruned", res.Tickets)
				if m != nil {
					m.AddRetentionRowsPruned("connection_lease", float64(res.Leases))
					m.AddRetentionRowsPruned("launch_ticket", float64(res.Tickets))
					m.SetRetentionLastSuccess(time.Now())
				}
			}
		}
		sweep()
		ticker := time.NewTicker(terminalRetentionInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				sweep()
			}
		}
	}
}

const (
	auditRetentionInterval = 24 * time.Hour
	auditRetentionBatch    = 1000
)

// auditRetentionLoop is the leader singleton that prunes audit_event rows
// older than days, once at election and then daily (ADR 0011).
func auditRetentionLoop(db *store.DB, days int, m *observability.Metrics, log *slog.Logger) func(ctx context.Context) {
	return func(ctx context.Context) {
		prune := func() {
			n, err := db.PruneAudit(ctx, time.Now().Add(-time.Duration(days)*24*time.Hour), auditRetentionBatch)
			switch {
			case err != nil && errors.Is(err, context.Canceled):
			case err != nil:
				log.Error("audit retention prune failed", "err", err)
			default:
				log.Info("audit retention prune", "rows_pruned", n)
				if m != nil {
					m.AddRetentionRowsPruned("audit_event", float64(n))
				}
			}
		}
		prune()
		ticker := time.NewTicker(auditRetentionInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				prune()
			}
		}
	}
}
