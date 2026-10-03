// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package backend

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
)

const goodTenantQuotas = `[{"tenant":"tenant-a","runningWorkspaces":3,"cpu":"4","memory":"8Gi","storage":"100Gi"}]`

// TestParseFlags_TenantQuotas (FX-R17): -tenant-quotas / TCDI_TENANT_QUOTAS
// carry the chart-rendered JSON; unset means "declare nothing".
func TestParseFlags_TenantQuotas(t *testing.T) {
	c, err := ParseFlags(mergedArgs(), noEnv)
	if err != nil || c.TenantQuotas != "" {
		t.Fatalf("default: TenantQuotas=%q err=%v, want empty, nil", c.TenantQuotas, err)
	}
	c, err = ParseFlags(append(mergedArgs(), "-tenant-quotas", goodTenantQuotas), noEnv)
	if err != nil || c.TenantQuotas != goodTenantQuotas {
		t.Fatalf("flag: TenantQuotas=%q err=%v", c.TenantQuotas, err)
	}
	c, err = ParseFlags(mergedArgs(), envMap(map[string]string{"TCDI_TENANT_QUOTAS": goodTenantQuotas}))
	if err != nil || c.TenantQuotas != goodTenantQuotas {
		t.Fatalf("env: TenantQuotas=%q err=%v", c.TenantQuotas, err)
	}
}

// TestParseFlags_MalformedTenantQuotasFailsStartup: a malformed value is a
// startup error that names the flag — never a silently ignored quota.
func TestParseFlags_MalformedTenantQuotasFailsStartup(t *testing.T) {
	for name, bad := range map[string]string{
		"not json":      `tenant-a=3`,
		"negative":      `[{"tenant":"tenant-a","runningWorkspaces":-1,"cpu":"1","memory":"1Gi","storage":"1Gi"}]`,
		"bad quantity":  `[{"tenant":"tenant-a","runningWorkspaces":1,"cpu":"1","memory":"8GB","storage":"1Gi"}]`,
		"missing field": `[{"tenant":"tenant-a","runningWorkspaces":1,"cpu":"1","memory":"1Gi"}]`,
		"duplicate":     strings.Replace(goodTenantQuotas, "]", ",", 1) + goodTenantQuotas[1:],
	} {
		_, err := ParseFlags(append(mergedArgs(), "-tenant-quotas", bad), noEnv)
		if err == nil || !strings.Contains(err.Error(), "-tenant-quotas") {
			t.Errorf("%s: err=%v, want an error naming -tenant-quotas", name, err)
		}
	}
}

// quotaPasses records every pass of the tenant-quota startup loop.
type quotaPasses struct {
	mu      sync.Mutex
	passes  int
	changed int
}

func (q *quotaPasses) observe(changed int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.passes++
	q.changed += changed
}

func (q *quotaPasses) snapshot() (passes, changed int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.passes, q.changed
}

// TestTenantQuotaStartup_TwoReplicasWriteOnce (FX-R17): the upsert is a
// singleton loop, so two replicas starting together run it in exactly one of
// them — one pass, one row written — and a failover only adds an idempotent
// pass that changes nothing.
func TestTenantQuotaStartup_TwoReplicasWriteOnce(t *testing.T) {
	db := newDB(t)
	quotas, err := provisioning.ParseTenantQuotas(goodTenantQuotas)
	if err != nil {
		t.Fatal(err)
	}
	const retry = 200 * time.Millisecond
	seen := &quotaPasses{}
	loop := func() func(context.Context) {
		return tenantQuotaSingleton(testLog(), db, quotas, retry, seen.observe)
	}
	a := startReplica(t, db, "a", retry, nil, loop())
	b := startReplica(t, db, "b", retry, nil, loop())

	waitFor(t, 15*time.Second, "a leader", func() bool { return a.isLeader() || b.isLeader() })
	waitFor(t, 10*time.Second, "the quota pass", func() bool { p, _ := seen.snapshot(); return p >= 1 })
	time.Sleep(time.Second) // give a second (wrong) replica time to write too

	if p, c := seen.snapshot(); p != 1 || c != 1 {
		t.Fatalf("passes=%d changed=%d, want exactly one pass writing one row", p, c)
	}
	var slots, cpu, mem, disk int64
	var updated time.Time
	if err := db.Pool().QueryRow(context.Background(), `
		SELECT max_running_slots, max_cpu_millis, max_memory_bytes, max_disk_bytes, updated_at
		FROM tenant_quota WHERE tenant_id = 'tenant-a'`).Scan(&slots, &cpu, &mem, &disk, &updated); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if slots != 3 || cpu != 4000 || mem != 8<<30 || disk != 100<<30 {
		t.Fatalf("row = %d/%d/%d/%d, want 3/4000/%d/%d", slots, cpu, mem, disk, int64(8<<30), int64(100<<30))
	}

	// Failover: the successor re-runs the loop; the row is already right.
	holder, other := a, b
	if b.isLeader() {
		holder, other = b, a
	}
	holder.cancel()
	<-holder.done
	waitFor(t, 15*time.Second, "the other replica to lead", other.isLeader)
	waitFor(t, 10*time.Second, "the successor pass", func() bool { p, _ := seen.snapshot(); return p >= 2 })
	if p, c := seen.snapshot(); p != 2 || c != 1 {
		t.Fatalf("after failover passes=%d changed=%d, want 2 passes and still 1 row written", p, c)
	}
	var updated2 time.Time
	if err := db.Pool().QueryRow(context.Background(),
		`SELECT updated_at FROM tenant_quota WHERE tenant_id = 'tenant-a'`).Scan(&updated2); err != nil {
		t.Fatal(err)
	}
	if !updated2.Equal(updated) {
		t.Fatalf("failover pass rewrote the row: updated_at %v -> %v", updated, updated2)
	}
}

// TestTenantQuotaStartup_RetriesUntilDatabaseAnswers: a failing pass is
// logged and retried, not fatal — creates are refused (QUOTA_NOT_CONFIGURED)
// only until the pass lands.
func TestTenantQuotaStartup_RetriesUntilDatabaseAnswers(t *testing.T) {
	db := newDB(t)
	// Make the first pass fail: the table is renamed away, then restored.
	ctx := context.Background()
	if _, err := db.Pool().Exec(ctx, `ALTER TABLE tenant_quota RENAME TO tenant_quota_away`); err != nil {
		t.Fatal(err)
	}
	quotas, _ := provisioning.ParseTenantQuotas(goodTenantQuotas)
	seen := &quotaPasses{}
	loopCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		tenantQuotaSingleton(testLog(), db, quotas, 50*time.Millisecond, seen.observe)(loopCtx)
	}()
	t.Cleanup(func() { cancel(); <-done })

	time.Sleep(300 * time.Millisecond)
	if p, _ := seen.snapshot(); p != 0 {
		t.Fatalf("a failed pass was reported as done (passes=%d)", p)
	}
	if _, err := db.Pool().Exec(ctx, `ALTER TABLE tenant_quota_away RENAME TO tenant_quota`); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, "the retried pass", func() bool { _, c := seen.snapshot(); return c == 1 })
}
