// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package provisioning_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
	"github.com/tinyorbitvn/tinycdi/internal/store"
)

func TestParseTenantQuotas(t *testing.T) {
	got, err := provisioning.ParseTenantQuotas(`[
		{"tenant":"b","runningWorkspaces":3,"cpu":"4","memory":"8Gi","storage":"100Gi"},
		{"tenant":"a","runningWorkspaces":0,"cpu":"500m","memory":"512Mi","storage":"1Ti"},
		{"tenant":"c","runningWorkspaces":1,"cpu":1.5,"memory":1024,"storage":"10G"}
	]`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := []provisioning.TenantQuota{
		{TenantID: "a", Limits: provisioning.ResourceVector{RunningSlots: 0, CPUMillis: 500, MemoryBytes: 512 << 20, DiskBytes: 1 << 40}},
		{TenantID: "b", Limits: provisioning.ResourceVector{RunningSlots: 3, CPUMillis: 4000, MemoryBytes: 8 << 30, DiskBytes: 100 << 30}},
		{TenantID: "c", Limits: provisioning.ResourceVector{RunningSlots: 1, CPUMillis: 1500, MemoryBytes: 1024, DiskBytes: 10_000_000_000}},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d quotas, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] { // sorted by tenant
			t.Errorf("quota[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}

	for _, empty := range []string{"", "  ", "[]", "null"} {
		q, err := provisioning.ParseTenantQuotas(empty)
		if err != nil || len(q) != 0 {
			t.Errorf("ParseTenantQuotas(%q) = %v, %v; want no quotas", empty, q, err)
		}
	}
}

func TestParseTenantQuotasRejects(t *testing.T) {
	ok := `"runningWorkspaces":1,"cpu":"1","memory":"1Gi","storage":"1Gi"`
	for name, in := range map[string]string{
		"not json":           `tenant=a`,
		"not an array":       `{"tenant":"a"}`,
		"missing tenant":     `[{` + ok + `}]`,
		"empty tenant":       `[{"tenant":"",` + ok + `}]`,
		"duplicate tenant":   `[{"tenant":"a",` + ok + `},{"tenant":"a",` + ok + `}]`,
		"unknown field":      `[{"tenant":"a",` + ok + `,"gpu":1}]`,
		"missing workspaces": `[{"tenant":"a","cpu":"1","memory":"1Gi","storage":"1Gi"}]`,
		"missing cpu":        `[{"tenant":"a","runningWorkspaces":1,"memory":"1Gi","storage":"1Gi"}]`,
		"missing memory":     `[{"tenant":"a","runningWorkspaces":1,"cpu":"1","storage":"1Gi"}]`,
		"missing storage":    `[{"tenant":"a","runningWorkspaces":1,"cpu":"1","memory":"1Gi"}]`,
		"negative slots":     `[{"tenant":"a","runningWorkspaces":-1,"cpu":"1","memory":"1Gi","storage":"1Gi"}]`,
		"fractional slots":   `[{"tenant":"a","runningWorkspaces":1.5,"cpu":"1","memory":"1Gi","storage":"1Gi"}]`,
		"negative cpu":       `[{"tenant":"a","runningWorkspaces":1,"cpu":"-1","memory":"1Gi","storage":"1Gi"}]`,
		"unparsable cpu":     `[{"tenant":"a","runningWorkspaces":1,"cpu":"lots","memory":"1Gi","storage":"1Gi"}]`,
		"binary cpu":         `[{"tenant":"a","runningWorkspaces":1,"cpu":"1Gi","memory":"1Gi","storage":"1Gi"}]`,
		"negative memory":    `[{"tenant":"a","runningWorkspaces":1,"cpu":"1","memory":"-1Gi","storage":"1Gi"}]`,
		"unparsable memory":  `[{"tenant":"a","runningWorkspaces":1,"cpu":"1","memory":"8GB","storage":"1Gi"}]`,
		"unparsable storage": `[{"tenant":"a","runningWorkspaces":1,"cpu":"1","memory":"1Gi","storage":"big"}]`,
		"empty storage":      `[{"tenant":"a","runningWorkspaces":1,"cpu":"1","memory":"1Gi","storage":""}]`,
		"trailing data":      `[] []`,
	} {
		if _, err := provisioning.ParseTenantQuotas(in); err == nil {
			t.Errorf("%s: ParseTenantQuotas(%s) succeeded, want an error", name, in)
		}
	}
}

func readQuota(t *testing.T, db *store.DB, tenant string) (v provisioning.ResourceVector, updated time.Time, found bool) {
	t.Helper()
	rows, err := db.Pool().Query(context.Background(), `
		SELECT max_running_slots, max_cpu_millis, max_memory_bytes, max_disk_bytes, updated_at
		FROM tenant_quota WHERE tenant_id = $1`, tenant)
	if err != nil {
		t.Fatalf("read quota: %v", err)
	}
	defer rows.Close()
	if rows.Next() {
		if err := rows.Scan(&v.RunningSlots, &v.CPUMillis, &v.MemoryBytes, &v.DiskBytes, &updated); err != nil {
			t.Fatalf("scan quota: %v", err)
		}
		found = true
	}
	return v, updated, found
}

// TestApplyTenantQuotas_CreateUpdateIdempotent (FX-R17): the first pass
// creates the row, an identical pass changes nothing (the row is not even
// touched), and a changed value updates it in place.
func TestApplyTenantQuotas_CreateUpdateIdempotent(t *testing.T) {
	db := recoveryDB(t)
	ctx := context.Background()
	q := provisioning.TenantQuota{TenantID: "tenant-a", Limits: provisioning.ResourceVector{
		RunningSlots: 3, CPUMillis: 4000, MemoryBytes: 8 << 30, DiskBytes: 100 << 30}}

	n, err := provisioning.ApplyTenantQuotas(ctx, db, []provisioning.TenantQuota{q})
	if err != nil || n != 1 {
		t.Fatalf("create: changed=%d err=%v, want 1, nil", n, err)
	}
	got, updated1, found := readQuota(t, db, "tenant-a")
	if !found || got != q.Limits {
		t.Fatalf("after create: %+v found=%v, want %+v", got, found, q.Limits)
	}

	time.Sleep(20 * time.Millisecond) // updated_at would visibly advance on a rewrite
	n, err = provisioning.ApplyTenantQuotas(ctx, db, []provisioning.TenantQuota{q})
	if err != nil || n != 0 {
		t.Fatalf("idempotent re-apply: changed=%d err=%v, want 0, nil", n, err)
	}
	if _, updated2, _ := readQuota(t, db, "tenant-a"); !updated2.Equal(updated1) {
		t.Fatalf("idempotent re-apply rewrote the row: updated_at %v -> %v", updated1, updated2)
	}

	q.Limits.RunningSlots, q.Limits.CPUMillis = 5, 6000
	n, err = provisioning.ApplyTenantQuotas(ctx, db, []provisioning.TenantQuota{q})
	if err != nil || n != 1 {
		t.Fatalf("update: changed=%d err=%v, want 1, nil", n, err)
	}
	if got, _, _ := readQuota(t, db, "tenant-a"); got != q.Limits {
		t.Fatalf("after update: %+v, want %+v", got, q.Limits)
	}
}

// TestApplyTenantQuotas_UnlistedTenantUntouched: only the listed tenants are
// written; a tenant hand-seeded (or seeded by an earlier release) keeps its
// row exactly, and an empty list writes nothing.
func TestApplyTenantQuotas_UnlistedTenantUntouched(t *testing.T) {
	db := recoveryDB(t)
	ctx := context.Background()
	other := provisioning.ResourceVector{RunningSlots: 9, CPUMillis: 9000, MemoryBytes: 9 << 30, DiskBytes: 9 << 40}
	if err := db.WithTx(ctx, func(tx store.Tx) error {
		return provisioning.SetQuota(ctx, tx, "tenant-other", other)
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, seeded, _ := readQuota(t, db, "tenant-other")

	n, err := provisioning.ApplyTenantQuotas(ctx, db, []provisioning.TenantQuota{
		{TenantID: "tenant-a", Limits: provisioning.ResourceVector{RunningSlots: 1}},
	})
	if err != nil || n != 1 {
		t.Fatalf("apply: changed=%d err=%v", n, err)
	}
	got, updated, found := readQuota(t, db, "tenant-other")
	if !found || got != other || !updated.Equal(seeded) {
		t.Fatalf("unlisted tenant changed: %+v found=%v updated=%v (seeded %v)", got, found, updated, seeded)
	}
	if n, err := provisioning.ApplyTenantQuotas(ctx, db, nil); err != nil || n != 0 {
		t.Fatalf("empty list: changed=%d err=%v, want 0, nil", n, err)
	}
	var rows int
	if err := db.Pool().QueryRow(ctx, `SELECT count(*) FROM tenant_quota`).Scan(&rows); err != nil || rows != 2 {
		t.Fatalf("tenant_quota rows = %d (err %v), want 2", rows, err)
	}
}

// TestApplyTenantQuotas_BelowUsageAccepted: lowering a limit under the
// tenant's current usage is accepted; the held reservation survives and new
// reservations are refused as QUOTA_EXHAUSTED-class errors.
func TestApplyTenantQuotas_BelowUsageAccepted(t *testing.T) {
	db := recoveryDB(t)
	ctx := context.Background()
	big := provisioning.ResourceVector{RunningSlots: 5, CPUMillis: 8000, MemoryBytes: 1 << 34, DiskBytes: 1 << 40}
	one := provisioning.ResourceVector{RunningSlots: 1, CPUMillis: 1000, MemoryBytes: 1 << 30, DiskBytes: 1 << 33}
	if _, err := provisioning.ApplyTenantQuotas(ctx, db, []provisioning.TenantQuota{{TenantID: "tenant-a", Limits: big}}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	reserve := func(ws string) error {
		return db.WithTx(ctx, func(tx store.Tx) error {
			if _, err := tx.Exec(ctx, `
				INSERT INTO workspaces (id, tenant_id, owner_subject, request_id)
				VALUES ($1, 'tenant-a', 'iss|sub', $2) ON CONFLICT DO NOTHING`, ws, "req-"+ws); err != nil {
				return err
			}
			return provisioning.Reserve(ctx, tx, "tenant-a", ws, one)
		})
	}
	if err := reserve("ws_below0001"); err != nil {
		t.Fatalf("first reserve: %v", err)
	}

	zero := provisioning.ResourceVector{}
	if n, err := provisioning.ApplyTenantQuotas(ctx, db, []provisioning.TenantQuota{{TenantID: "tenant-a", Limits: zero}}); err != nil || n != 1 {
		t.Fatalf("lower below usage: changed=%d err=%v, want 1, nil", n, err)
	}
	held, err := provisioning.HeldUsage(ctx, db.Pool(), "tenant-a")
	if err != nil || held != one {
		t.Fatalf("existing reservation after lowering = %+v err=%v, want %+v kept", held, err, one)
	}
	if err := reserve("ws_below0002"); !provisioning.IsQuotaExceeded(err) {
		t.Fatalf("reserve after lowering: err=%v, want a quota-exceeded error", err)
	}
}

// TestReserve_NoQuotaRowVsOverLimit (FX-R17): a tenant without a row fails
// closed with ErrNoQuota (API: QUOTA_NOT_CONFIGURED); a configured tenant
// over its limit gets QuotaExceededError (API: QUOTA_EXHAUSTED). The two
// must never be confused.
func TestReserve_NoQuotaRowVsOverLimit(t *testing.T) {
	db := recoveryDB(t)
	ctx := context.Background()
	v := provisioning.ResourceVector{RunningSlots: 1, CPUMillis: 1000, MemoryBytes: 1 << 30, DiskBytes: 1 << 33}
	seedWS := func(tenant, ws string) {
		if _, err := db.Pool().Exec(ctx, `
			INSERT INTO workspaces (id, tenant_id, owner_subject, request_id) VALUES ($1, $2, 'iss|sub', $3)`,
			ws, tenant, "req-"+ws); err != nil {
			t.Fatalf("seed workspace: %v", err)
		}
	}
	seedWS("tenant-none", "ws_noquota01")
	err := db.WithTx(ctx, func(tx store.Tx) error { return provisioning.Reserve(ctx, tx, "tenant-none", "ws_noquota01", v) })
	if !errors.Is(err, provisioning.ErrNoQuota) || provisioning.IsQuotaExceeded(err) {
		t.Fatalf("no row: err=%v, want ErrNoQuota and not a quota-exceeded error", err)
	}

	if _, err := provisioning.ApplyTenantQuotas(ctx, db, []provisioning.TenantQuota{
		{TenantID: "tenant-cfg", Limits: provisioning.ResourceVector{RunningSlots: 1, CPUMillis: 1000, MemoryBytes: 1 << 30, DiskBytes: 1 << 33}},
	}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	seedWS("tenant-cfg", "ws_overlim001")
	seedWS("tenant-cfg", "ws_overlim002")
	if err := db.WithTx(ctx, func(tx store.Tx) error { return provisioning.Reserve(ctx, tx, "tenant-cfg", "ws_overlim001", v) }); err != nil {
		t.Fatalf("within limit: %v", err)
	}
	err = db.WithTx(ctx, func(tx store.Tx) error { return provisioning.Reserve(ctx, tx, "tenant-cfg", "ws_overlim002", v) })
	if !provisioning.IsQuotaExceeded(err) || errors.Is(err, provisioning.ErrNoQuota) {
		t.Fatalf("over limit: err=%v, want a quota-exceeded error and not ErrNoQuota", err)
	}
	if !strings.Contains(err.Error(), "tenant-cfg") {
		t.Fatalf("over-limit error does not name the tenant: %v", err)
	}
}
