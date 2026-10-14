package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/store"
)

func seedBoot(t *testing.T, db *store.DB, ws string, gen int64, tenant string, requested time.Time) {
	t.Helper()
	if _, err := db.Pool().Exec(context.Background(),
		`INSERT INTO workspace_boot (workspace_id, runtime_generation, tenant_id, requested_at) VALUES ($1, $2, $3, $4)`,
		ws, gen, tenant, requested); err != nil {
		t.Fatal(err)
	}
}

func TestOverview_LiveSessionsAndP95(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	now := time.Now()
	for i, secs := range []int{10, 20, 30, 40, 100} {
		ws := "ws-p" + string(rune('a'+i))
		seedBoot(t, db, ws, 1, "t1", now.Add(-time.Hour))
		if _, ok, err := db.MarkBootReady(ctx, ws, 1, now.Add(-time.Hour).Add(time.Duration(secs)*time.Second)); err != nil || !ok {
			t.Fatalf("MarkBootReady(%s) = %v %v", ws, ok, err)
		}
	}
	seedBoot(t, db, "ws-old", 1, "t1", now.Add(-48*time.Hour))
	_, _, _ = db.MarkBootReady(ctx, "ws-old", 1, now.Add(-47*time.Hour)) // outside the 24 h window
	seedBoot(t, db, "ws-pending", 1, "t1", now)                          // never Ready: no sample

	seedWorkspace(t, db, "t1", "https://idp|alice", "ws-live")
	seedLiveLease(t, db, "lease-live", "ws-live", "t1", "https://idp|alice", "active", now, now.Add(time.Minute))
	seedWorkspace(t, db, "t1", "https://idp|alice", "ws-dead")
	seedLiveLease(t, db, "lease-dead", "ws-dead", "t1", "https://idp|alice", "active", now.Add(-time.Hour), now.Add(-time.Minute))

	o, err := db.TenantOverview(ctx, "t1", now)
	if err != nil {
		t.Fatal(err)
	}
	if o.LiveSessions != 1 || o.ProvisioningSamples != 5 || o.ProvisioningP95Seconds == nil {
		t.Fatalf("overview = %+v", o)
	}
	if p := *o.ProvisioningP95Seconds; p < 87 || p > 89 { // percentile_cont(0.95) over 10,20,30,40,100 = 88
		t.Fatalf("p95 = %v, want 88", p)
	}
	empty, _ := db.TenantOverview(ctx, "t-none", now)
	if empty.ProvisioningP95Seconds != nil || empty.LiveSessions != 0 {
		t.Fatalf("empty tenant = %+v", empty)
	}
}

func TestMarkBootReady_FirstWinsAndPrune(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	now := time.Now()
	seedBoot(t, db, "ws", 3, "t1", now.Add(-30*time.Second))
	d, ok, err := db.MarkBootReady(ctx, "ws", 3, now)
	if err != nil || !ok || d < 29*time.Second || d > 31*time.Second {
		t.Fatalf("first = %v %v %v", d, ok, err)
	}
	if _, ok, _ := db.MarkBootReady(ctx, "ws", 3, now.Add(time.Minute)); ok {
		t.Fatal("a second replica must not re-mark the same incarnation")
	}
	if _, ok, _ := db.MarkBootReady(ctx, "ws", 4, now); ok {
		t.Fatal("unknown generation marked")
	}
	if n, err := db.PruneWorkspaceBoot(ctx, now.Add(time.Hour)); err != nil || n != 1 {
		t.Fatalf("prune = %d %v", n, err)
	}
}
