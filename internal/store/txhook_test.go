package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/tinyorbitvn/tinycdi/internal/store"
)

type insertHook struct {
	outcome   string
	pre       int
	committed bool
}

func (h *insertHook) PreCommit(ctx context.Context, tx store.Tx) error {
	h.pre++
	_, err := store.InsertAudit(ctx, tx, store.AuditRecord{Tenant: "t", ActorRef: "a", Action: "x", Outcome: h.outcome})
	return err
}

func (h *insertHook) Committed() { h.committed = true }

func TestWithTx_TxHookFailureRollsBackMutation(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	write := func(ctx context.Context, tenant string) error {
		return db.WithTx(ctx, func(tx store.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO tenant_user_limit_default (tenant_id, max_running) VALUES ($1, 1)`, tenant)
			return err
		})
	}
	ok := &insertHook{outcome: "success"}
	if err := write(store.ContextWithTxHook(ctx, ok), "ok"); err != nil {
		t.Fatal(err)
	}
	if !ok.committed {
		t.Fatal("Committed not called after COMMIT")
	}
	bad := &insertHook{outcome: "bogus"} // violates the outcome CHECK
	if err := write(store.ContextWithTxHook(ctx, bad), "bad"); err == nil {
		t.Fatal("a failing PreCommit must fail the transaction")
	}
	if bad.committed {
		t.Fatal("Committed called for a rolled-back transaction")
	}
	skipped := &insertHook{outcome: "success"}
	boom := errors.New("boom")
	if err := db.WithTx(store.ContextWithTxHook(ctx, skipped), func(store.Tx) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("body error = %v, want boom", err)
	}
	if skipped.pre != 0 || skipped.committed {
		t.Fatalf("hook ran for a failed body: pre=%d committed=%v", skipped.pre, skipped.committed)
	}
	var mut, audit int
	_ = db.Pool().QueryRow(ctx, `SELECT count(*) FROM tenant_user_limit_default WHERE tenant_id = 'bad'`).Scan(&mut)
	_ = db.Pool().QueryRow(ctx, `SELECT count(*) FROM audit_event`).Scan(&audit)
	if mut != 0 || audit != 1 {
		t.Fatalf("mutation rows=%d audit rows=%d; want 0 and 1 (fail closed)", mut, audit)
	}
	if err := write(ctx, "nohook"); err != nil {
		t.Fatalf("WithTx without a hook: %v", err)
	}
}
