// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/tinyorbitvn/tinycdi/internal/observability"
	"github.com/tinyorbitvn/tinycdi/internal/store"
)

type fakeDurable struct {
	mu          sync.Mutex
	stdout      []observability.AuditEvent
	persisted   []observability.AuditEvent
	inTx        int
	amended     []observability.AuditEvent
	failPersist error
}

func (f *fakeDurable) WriteAudit(_ context.Context, e observability.AuditEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stdout = append(f.stdout, e)
	return nil
}
func (f *fakeDurable) Persists(action string) bool { return action != auditActionSessionTouch }
func (f *fakeDurable) PersistAudit(_ context.Context, tx store.Tx, e observability.AuditEvent) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failPersist != nil {
		return 0, f.failPersist
	}
	f.persisted = append(f.persisted, e)
	if tx != nil {
		f.inTx++
	}
	return int64(len(f.persisted)), nil
}
func (f *fakeDurable) AmendAudit(_ context.Context, _ int64, e observability.AuditEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.amended = append(f.amended, e)
	return nil
}

func TestAudited_PersistFailureFailsClosed(t *testing.T) {
	sink := &fakeDurable{failPersist: errors.New("db down")}
	h := audited(sink, routeAdminQuotaGet, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"x"`)
		respondJSON(w, map[string]string{"quota": "should-not-leak"})
	}))
	rec := httptest.NewRecorder()
	rec.Header().Set("X-Request-Id", "req-1") // set by an outer middleware before audited()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/admin/tenants/t/quota", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 when the audit row cannot be stored", rec.Code)
	}
	if rec.Header().Get("ETag") != "" || rec.Header().Get("X-Request-Id") != "req-1" {
		t.Fatalf("handler headers leaked or outer headers lost: %v", rec.Header())
	}
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil || env.Error.Code != "INTERNAL" {
		t.Fatalf("body is not the INTERNAL error envelope (no handler content): %s", rec.Body.String())
	}
	if len(sink.stdout) != 1 || sink.stdout[0].Outcome != observability.OutcomeFailure || sink.stdout[0].ErrorCode != "INTERNAL" {
		t.Fatalf("stdout records = %+v, want one failure/INTERNAL record (stdout survives an audit-store outage)", sink.stdout)
	}
}

func TestAudited_NoTxMutationPersistFailureFailsClosed(t *testing.T) {
	sink := &fakeDurable{failPersist: errors.New("db down")}
	h := audited(sink, routeAdminQuotaSet, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		respondJSON(w, map[string]string{"quota": "should-not-leak"}) // a K8s-only or single-statement mutation
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/v1/admin/tenants/t/quota", nil))
	if rec.Code != http.StatusInternalServerError || bytes.Contains(rec.Body.Bytes(), []byte("should-not-leak")) {
		t.Fatalf("code=%d body=%s; want 500 and no handler body", rec.Code, rec.Body.String())
	}
}

func TestAudited_TxHookPersistsOnceThenAmends(t *testing.T) {
	sink := &fakeDurable{}
	var fakeTx store.Tx = struct{ store.Tx }{}
	h := audited(sink, routeAdminQuotaSet, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hook := store.TxHookFromContext(r.Context())
		if hook == nil {
			t.Fatal("a mutation must carry the audit tx hook")
		}
		for i := 0; i < 2; i++ { // two committed transactions in one request
			if err := hook.PreCommit(r.Context(), fakeTx); err != nil {
				t.Fatal(err)
			}
			hook.Committed()
		}
		auditSetTarget(r.Context(), "ws_late") // learned after the transactions committed
		writeError(w, r, CodeQuotaExhausted, "late refusal")
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/v1/admin/tenants/t/quota", nil))
	if len(sink.persisted) != 1 || sink.inTx != 1 {
		t.Fatalf("persisted=%d inTx=%d, want exactly one in-transaction row", len(sink.persisted), sink.inTx)
	}
	if len(sink.amended) != 1 || sink.amended[0].Outcome != observability.OutcomeFailure ||
		sink.amended[0].ErrorCode != "QUOTA_EXHAUSTED" || sink.amended[0].TargetUID != "ws_late" {
		t.Fatalf("amend = %+v, want the final failure outcome and the late target", sink.amended)
	}
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want the handler's 409", rec.Code)
	}
}

func TestAudited_PanicAfterCommitAmendsFailure(t *testing.T) {
	sink := &fakeDurable{}
	var fakeTx store.Tx = struct{ store.Tx }{}
	h := audited(sink, routeAdminQuotaSet, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hook := store.TxHookFromContext(r.Context())
		if err := hook.PreCommit(r.Context(), fakeTx); err != nil {
			t.Fatal(err)
		}
		hook.Committed()
		panic("boom")
	}))
	func() {
		defer func() { _ = recover() }()
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPut, "/v1/admin/tenants/t/quota", nil))
	}()
	if len(sink.amended) != 1 || sink.amended[0].Outcome != observability.OutcomeFailure || sink.amended[0].ErrorCode != "panic" {
		t.Fatalf("amend = %+v, want failure/panic for the committed row", sink.amended)
	}
}

type plainSink struct{ n int }

func (p *plainSink) WriteAudit(context.Context, observability.AuditEvent) error { p.n++; return nil }

func TestAudited_PlainSinkDoesNotBuffer(t *testing.T) {
	sink := &plainSink{}
	rec := httptest.NewRecorder()
	h := audited(sink, routeAdminQuotaGet, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("x"))
		if rec.Body.Len() != 1 {
			t.Fatal("a sink without persistence must not hold the response back")
		}
	}))
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/admin/tenants/t/quota", nil))
	if sink.n != 1 {
		t.Fatalf("stdout records = %d, want 1", sink.n)
	}
}

func TestLateAuditSink_ForwardsPersistence(t *testing.T) {
	inner := &fakeDurable{}
	var late observability.AuditSink = lateAuditSink{func() observability.AuditSink { return inner }}
	d, ok := late.(durableAudit)
	if !ok || !d.Persists(auditActionSessionRevokeAll) || d.Persists(auditActionSessionTouch) {
		t.Fatalf("lateAuditSink must expose the inner sink's persistence (ok=%v)", ok)
	}
	if _, err := d.PersistAudit(context.Background(), nil, observability.AuditEvent{Action: auditActionSessionRevokeAll}); err != nil || len(inner.persisted) != 1 {
		t.Fatalf("PersistAudit not forwarded: err=%v persisted=%d", err, len(inner.persisted))
	}
	none := lateAuditSink{func() observability.AuditSink { return &plainSink{} }}
	if none.Persists(auditActionSessionRevokeAll) {
		t.Fatal("a plain inner sink persists nothing")
	}
}

func TestAudited_NoTxPathPersistsAfterHandler(t *testing.T) {
	sink := &fakeDurable{}
	h := audited(sink, routeAdminQuotaGet, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if store.TxHookFromContext(r.Context()) != nil {
			t.Fatal("reads carry no tx hook")
		}
		respondJSON(w, map[string]string{"ok": "1"})
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/admin/tenants/t/quota", nil))
	if rec.Code != http.StatusOK || len(sink.persisted) != 1 || sink.inTx != 0 {
		t.Fatalf("code=%d persisted=%d inTx=%d", rec.Code, len(sink.persisted), sink.inTx)
	}
}

func TestAudited_TouchStaysStdoutOnly(t *testing.T) {
	sink := &fakeDurable{}
	h := audited(sink, routeSessionTouch, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if store.TxHookFromContext(r.Context()) != nil {
			t.Fatal("session.touch is not persisted, so it carries no hook")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/session:touch", nil))
	if len(sink.persisted) != 0 || len(sink.stdout) != 1 {
		t.Fatalf("persisted=%d stdout=%d", len(sink.persisted), len(sink.stdout))
	}
}

func TestAuditTargetKind(t *testing.T) {
	for action, want := range map[string]string{
		"workspace.start": "workspace", "connection.create": "workspace", "file.transfer.upload": "workspace",
		"data.purge": "data", "admin.quota.set": "tenant", "session.revoke_all": "session", "other": "",
	} {
		if got := auditTargetKind(action); got != want {
			t.Errorf("auditTargetKind(%q) = %q, want %q", action, got, want)
		}
	}
}
