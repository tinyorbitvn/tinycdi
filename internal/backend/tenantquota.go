// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package backend

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/observability"
	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
	"github.com/tinyorbitvn/tinycdi/internal/store"
)

// tenantQuotaRetry is how long a failed quota pass waits before trying again.
const tenantQuotaRetry = 5 * time.Second

// tenantQuotaSingleton returns the startup loop that upserts the declared
// tenant quotas (-tenant-quotas). It is registered as a singleton, so it runs
// only in the replica holding the leader lock and re-runs on a leader
// hand-over; the upsert leaves an already-correct row untouched, so a repeat
// pass changes nothing. A failing pass is logged and retried until it lands
// or ctx ends — until then creates are refused with QUOTA_NOT_CONFIGURED.
// observe, when set, is told how many rows each completed pass changed.
// sink, when set, receives one admin.quota.config_apply audit event for the
// completed pass — and for every failed pass, so a persistently failing
// apply is auditable too (outcome failure/"apply_failed", changed=0: the
// transaction rolls back, so nothing was applied). The platform-level
// quota write must be reconstructable from the audit stream like the
// admin API's own writes (the apply is the operator's action — actor
// "config:tenant-quotas", no request id exists, so the correlation field
// carries the fixed "startup" marker).
func tenantQuotaSingleton(log *slog.Logger, db *store.DB, quotas []provisioning.TenantQuota,
	retry time.Duration, observe func(changed int), sink observability.AuditSink) func(context.Context) {
	return func(ctx context.Context) {
		names := make([]string, 0, len(quotas))
		for _, q := range quotas {
			names = append(names, q.TenantID)
		}
		for {
			changed, err := provisioning.ApplyTenantQuotas(ctx, db, quotas)
			if err == nil {
				log.Info("tenant quotas applied", "declared", len(quotas), "changed", changed)
				if observe != nil {
					observe(changed)
				}
				writeQuotaApplyAudit(ctx, sink, names, observability.OutcomeSuccess, "", changed)
				return
			}
			if errors.Is(err, context.Canceled) || ctx.Err() != nil {
				return
			}
			log.Error("tenant quota pass failed; retrying", "err", err, "retry_in", retry)
			writeQuotaApplyAudit(ctx, sink, names, observability.OutcomeFailure, "apply_failed", 0)
			select {
			case <-ctx.Done():
				return
			case <-time.After(retry):
			}
		}
	}
}

// writeQuotaApplyAudit emits the admin.quota.config_apply audit record for
// one pass of the startup apply — success carries the changed-row count,
// failure carries the stable "apply_failed" code and changed=0 (the
// transaction rolled back). A nil sink is a no-op.
func writeQuotaApplyAudit(ctx context.Context, sink observability.AuditSink, names []string,
	outcome observability.AuditOutcome, errCode string, changed int) {
	if sink == nil {
		return
	}
	_ = sink.WriteAudit(ctx, observability.AuditEvent{
		Actor:     "config:tenant-quotas",
		Action:    "admin.quota.config_apply",
		RequestID: "startup",
		Outcome:   outcome,
		ErrorCode: errCode,
		Details: map[string]string{
			"tenants": strings.Join(names, ","),
			"changed": strconv.Itoa(changed),
		},
	})
}
