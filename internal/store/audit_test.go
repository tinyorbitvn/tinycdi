package store_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/tinyorbitvn/tinycdi/internal/store"
)

func TestAudit_InsertListFilterCursor(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if _, err := store.InsertAudit(ctx, db.Pool(), store.AuditRecord{
			Tenant: "t1", ActorRef: "oidc:aa", Action: "workspace.start", TargetKind: "workspace",
			TargetID: fmt.Sprintf("ws_%d", i), Outcome: "success", RequestID: "r", Details: map[string]string{"k": "v"},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.InsertAudit(ctx, db.Pool(), store.AuditRecord{Tenant: "t2", ActorRef: "oidc:bb", Action: "workspace.stop", Outcome: "denied"}); err != nil {
		t.Fatal(err)
	}
	first, err := db.ListAudit(ctx, store.AuditFilter{Tenant: "t1"}, 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 3 || first[0].TargetID != "ws_4" || first[1].TargetID != "ws_3" {
		t.Fatalf("first page = %+v, want ws_4, ws_3 (+1 lookahead)", first)
	}
	if first[0].Details["k"] != "v" || first[0].Outcome != "success" {
		t.Fatalf("round trip lost fields: %+v", first[0])
	}
	second, err := db.ListAudit(ctx, store.AuditFilter{Tenant: "t1"}, 2, &store.AuditCursor{CreatedAt: first[1].CreatedAt, ID: first[1].ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 3 || second[0].TargetID != "ws_2" || second[2].TargetID != "ws_0" {
		t.Fatalf("second page = %+v, want ws_2, ws_1, ws_0", second)
	}
	only, _ := db.ListAudit(ctx, store.AuditFilter{Tenant: "t1", TargetID: "ws_3"}, 10, nil)
	if len(only) != 1 {
		t.Fatalf("target filter = %d rows, want 1", len(only))
	}
	byActor, _ := db.ListAudit(ctx, store.AuditFilter{Tenant: "t2", ActorRef: "oidc:bb", Action: "workspace.stop"}, 10, nil)
	if len(byActor) != 1 || byActor[0].Outcome != "denied" {
		t.Fatalf("tenant isolation / actor+action filter: %+v", byActor)
	}
	future := time.Now().Add(time.Hour)
	none, _ := db.ListAudit(ctx, store.AuditFilter{Tenant: "t1", Since: &future}, 10, nil)
	if len(none) != 0 {
		t.Fatalf("since filter leaked %d rows", len(none))
	}
}

func TestAudit_OutcomeVocabulary(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	if _, err := store.InsertAudit(ctx, db.Pool(), store.AuditRecord{Tenant: "t", ActorRef: "a", Action: "x", Outcome: "failure"}); err != nil {
		t.Fatalf("failure outcome refused: %v", err)
	}
	if _, err := store.InsertAudit(ctx, db.Pool(), store.AuditRecord{Tenant: "t", ActorRef: "a", Action: "x", Outcome: "error"}); err == nil {
		t.Fatal("outcome outside the observability vocabulary accepted")
	}
}

func TestAudit_AmendAndPrune(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	var ids []int64
	for _, target := range []string{"old-a", "old-b", "new"} {
		id, err := store.InsertAudit(ctx, db.Pool(), store.AuditRecord{Tenant: "t", ActorRef: "a", Action: "x", TargetID: target, Outcome: "success"})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := db.AmendAudit(ctx, ids[2], "ws_late", "failure", "QUOTA_EXHAUSTED", map[string]string{"counts": "1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool().Exec(ctx, `UPDATE audit_event SET created_at = now() - interval '100 days' WHERE target_id LIKE 'old-%'`); err != nil {
		t.Fatal(err)
	}
	n, err := db.PruneAudit(ctx, time.Now().Add(-90*24*time.Hour), 1)
	if err != nil || n != 2 {
		t.Fatalf("PruneAudit = %d, %v; want 2 rows over batch size 1", n, err)
	}
	left, _ := db.ListAudit(ctx, store.AuditFilter{Tenant: "t"}, 10, nil)
	if len(left) != 1 || left[0].Outcome != "failure" || left[0].ErrorCode != "QUOTA_EXHAUSTED" || left[0].Details["counts"] != "1" || left[0].TargetID != "ws_late" {
		t.Fatalf("after amend+prune: %+v", left)
	}
}

// TestAudit_InsertSanitizedTarget (R1-22): request-controlled audit text
// with NUL, invalid UTF-8 or 60 KB inserts and amends cleanly, valid and
// bounded, and the target filter finds the long row by its raw value.
func TestAudit_InsertSanitizedTarget(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	long := strings.Repeat("A", 60000)
	for name, raw := range map[string]string{"nul": "a\x00b", "badutf8": "a\xffb", "long": long} {
		id, err := store.InsertAudit(ctx, db.Pool(), store.AuditRecord{
			Tenant: "t-san", ActorRef: "oidc:aa", Action: "workspace.delete", TargetKind: "workspace",
			TargetID: raw, Outcome: "failure", RequestID: raw, Details: map[string]string{"path": raw},
		})
		if err != nil {
			t.Fatalf("%s: insert %v", name, err)
		}
		if err := db.AmendAudit(ctx, id, raw, "failure", "INVALID_REQUEST", map[string]string{"path": raw}); err != nil {
			t.Fatalf("%s: amend %v", name, err)
		}
	}
	rows, err := db.ListAudit(ctx, store.AuditFilter{Tenant: "t-san"}, 10, nil)
	if err != nil || len(rows) != 3 {
		t.Fatalf("rows=%d err=%v, want 3", len(rows), err)
	}
	for _, r := range rows {
		for _, v := range []string{r.TargetID, r.RequestID, r.Details["path"]} {
			if len(v) > 256 || !utf8.ValidString(v) || strings.ContainsRune(v, 0) {
				t.Fatalf("stored value %q (len %d) not bounded valid UTF-8", v[:min(len(v), 40)], len(v))
			}
		}
	}
	hit, err := db.ListAudit(ctx, store.AuditFilter{Tenant: "t-san", TargetID: long}, 10, nil)
	if err != nil || len(hit) != 1 {
		t.Fatalf("filter by the raw long target: rows=%d err=%v, want 1", len(hit), err)
	}
}
