// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"github.com/tinyorbitvn/tinycdi/internal/observability"
	"github.com/tinyorbitvn/tinycdi/internal/store"
)

// AuditStore is the Postgres surface of DurableAuditSink
// (store.AuditStore). tx nil inserts through the pool.
type AuditStore interface {
	InsertAudit(ctx context.Context, tx store.Tx, r store.AuditRecord) (int64, error)
	AmendAudit(ctx context.Context, id int64, targetID, outcome, errorCode string, details map[string]string) error
}

// durableAudit is the extra surface audited() looks for on its sink.
type durableAudit interface {
	observability.AuditSink
	Persists(action string) bool
	PersistAudit(ctx context.Context, tx store.Tx, e observability.AuditEvent) (int64, error)
	AmendAudit(ctx context.Context, id int64, e observability.AuditEvent) error
	// reportPersistError signals an insert or amend ("insert"/"amend")
	// that failed for good, so no audit_event failure goes unnoticed.
	reportPersistError(op string, e observability.AuditEvent, err error)
}

// notPersisted are actions that stay stdout-only: session.touch is the
// portal's activity beat and changes no state an administrator audits.
var notPersisted = map[string]bool{auditActionSessionTouch: true}

// DurableAuditSink writes every event to the stdout sink and, through
// audited(), persists domain events to audit_event (ADR 0010). Plain
// WriteAudit callers (the authenticator's revoke records, config apply)
// stay stdout-only.
type DurableAuditSink struct {
	stdout  observability.AuditSink
	db      AuditStore
	log     *slog.Logger
	onError func(action string)
}

// NewDurableAuditSink wires the stdout sink (normally a GuardedSink) and
// the store. Every failed insert or amend is logged at error level with
// the request id and passed to onError (the
// tinycdi_audit_write_errors_total{event} metric in wiring). log defaults
// to slog.Default(); onError may be nil.
func NewDurableAuditSink(stdout observability.AuditSink, db AuditStore, log *slog.Logger, onError func(action string)) *DurableAuditSink {
	if log == nil {
		log = slog.Default()
	}
	return &DurableAuditSink{stdout: stdout, db: db, log: log, onError: onError}
}

func (s *DurableAuditSink) WriteAudit(ctx context.Context, e observability.AuditEvent) error {
	return s.stdout.WriteAudit(ctx, e)
}

func (s *DurableAuditSink) Persists(action string) bool { return !notPersisted[action] }

func (s *DurableAuditSink) PersistAudit(ctx context.Context, tx store.Tx, e observability.AuditEvent) (int64, error) {
	return s.db.InsertAudit(ctx, tx, auditRecordOf(e))
}

func (s *DurableAuditSink) AmendAudit(ctx context.Context, id int64, e observability.AuditEvent) error {
	r := auditRecordOf(e)
	return s.db.AmendAudit(ctx, id, r.TargetID, r.Outcome, r.ErrorCode, r.Details)
}

// reportPersistError is not rate-limited: during an audit-store outage
// every affected request already fails closed, one line per request. A
// failed amend leaves the committed row with its pre-commit snapshot, so
// the line carries the final outcome the row should have recorded.
func (s *DurableAuditSink) reportPersistError(op string, e observability.AuditEvent, err error) {
	if s.onError != nil {
		s.onError(e.Action)
	}
	s.log.Error("audit_event write failed", "op", op, "action", e.Action, "request_id", e.RequestID,
		"outcome", string(e.Outcome), "error_code", e.ErrorCode, "error", err)
}

func auditRecordOf(e observability.AuditEvent) store.AuditRecord {
	return store.AuditRecord{
		Tenant: e.Tenant, ActorRef: e.Actor, Action: e.Action,
		TargetKind: auditTargetKind(e.Action), TargetID: e.TargetUID,
		Outcome: string(e.Outcome), ErrorCode: e.ErrorCode, RequestID: e.RequestID,
		Details: observability.RedactDetails(e.Details),
	}
}

// auditTargetKind names what TargetUID identifies, derived from the
// action's namespace.
func auditTargetKind(action string) string {
	switch {
	case strings.HasPrefix(action, "workspace."), strings.HasPrefix(action, "connection."),
		strings.HasPrefix(action, "file.transfer."):
		return "workspace"
	case strings.HasPrefix(action, "data."):
		return "data"
	case strings.HasPrefix(action, "admin."):
		return "tenant"
	case strings.HasPrefix(action, "session."):
		return "session"
	}
	return ""
}

// auditTxHook persists the route's audit row inside the first
// transaction of the request that commits (store.TxHook).
type auditTxHook struct {
	sink  durableAudit
	build func() observability.AuditEvent

	mu        sync.Mutex
	pending   int64
	committed int64
}

func (h *auditTxHook) PreCommit(ctx context.Context, tx store.Tx) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.committed != 0 {
		return nil
	}
	id, err := h.sink.PersistAudit(ctx, tx, h.build())
	if err != nil {
		return fmt.Errorf("audit: %w", err)
	}
	h.pending = id
	return nil
}

func (h *auditTxHook) Committed() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.committed == 0 {
		h.committed = h.pending
	}
}

func (h *auditTxHook) committedID() int64 {
	if h == nil {
		return 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.committed
}

// bufferedResponse holds the handler's status and body until the audit
// row is stored. Headers go to the real writer's map; base is that map as
// it was before the handler ran, so a fail-closed answer can restore it.
// It deliberately implements neither http.Flusher nor http.Hijacker and has
// no Unwrap: a streaming or upgrading handler must not be mounted under a
// durable audited() route (a ResponseController call on it fails loudly
// instead of releasing bytes before the row is stored). No audited route
// streams today.
type bufferedResponse struct {
	w      http.ResponseWriter
	base   http.Header
	status int
	body   []byte
}

func newBufferedResponse(w http.ResponseWriter) *bufferedResponse {
	return &bufferedResponse{w: w, base: w.Header().Clone()}
}

func (b *bufferedResponse) Header() http.Header { return b.w.Header() }
func (b *bufferedResponse) WriteHeader(code int) {
	if b.status == 0 {
		b.status = code
	}
}
func (b *bufferedResponse) Write(p []byte) (int, error) {
	if b.status == 0 {
		b.status = http.StatusOK
	}
	b.body = append(b.body, p...)
	return len(p), nil
}
func (b *bufferedResponse) flush() {
	b.w.WriteHeader(statusOrOK(b.status))
	if len(b.body) > 0 {
		_, _ = b.w.Write(b.body)
	}
}

// discardForError drops everything the handler produced (body and every
// header it set) so the fail-closed error envelope is the whole response.
func (b *bufferedResponse) discardForError() {
	h := b.w.Header()
	for k := range h {
		delete(h, k)
	}
	for k, v := range b.base {
		h[k] = v
	}
	b.body = nil
}
