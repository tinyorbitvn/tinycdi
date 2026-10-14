package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// AuditRecord is one row of audit_event: a domain audit event persisted
// next to its stdout record (ADR 0010). ActorRef is already pseudonymous.
type AuditRecord struct {
	ID         int64
	Tenant     string
	ActorRef   string
	Action     string
	TargetKind string
	TargetID   string
	Outcome    string
	ErrorCode  string
	RequestID  string
	Details    map[string]string
	CreatedAt  time.Time
}

// RowQuerier is satisfied by *pgxpool.Pool and pgx.Tx.
type RowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func detailsJSON(d map[string]string) (string, error) {
	if d == nil {
		d = map[string]string{}
	}
	b, err := json.Marshal(d)
	if err != nil {
		return "", fmt.Errorf("store: audit details %w", err)
	}
	return string(b), nil
}

// InsertAudit writes r through q (the pool, or the audited mutation's
// transaction) and returns the row id.
func InsertAudit(ctx context.Context, q RowQuerier, r AuditRecord) (int64, error) {
	details, err := detailsJSON(r.Details)
	if err != nil {
		return 0, err
	}
	var id int64
	if err := q.QueryRow(ctx, `
		INSERT INTO audit_event (tenant, actor_ref, action, target_kind, target_id, outcome, error_code, request_id, details)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::jsonb)
		RETURNING id`,
		r.Tenant, r.ActorRef, r.Action, r.TargetKind, r.TargetID, r.Outcome, r.ErrorCode, r.RequestID, details,
	).Scan(&id); err != nil {
		return 0, fmt.Errorf("store: insert audit %w", err)
	}
	return id, nil
}

// AmendAudit rewrites the target, outcome, error code and details of a row
// inserted inside the audited mutation's transaction: the handler may refuse
// after COMMIT, and a create handler learns its target id only afterwards.
func (d *DB) AmendAudit(ctx context.Context, id int64, targetID, outcome, errorCode string, details map[string]string) error {
	raw, err := detailsJSON(details)
	if err != nil {
		return err
	}
	if _, err := d.pool.Exec(ctx,
		`UPDATE audit_event SET target_id = $2, outcome = $3, error_code = $4, details = $5::jsonb WHERE id = $1`,
		id, targetID, outcome, errorCode, raw); err != nil {
		return fmt.Errorf("store: amend audit %w", err)
	}
	return nil
}

// AuditFilter narrows ListAudit; empty fields do not filter. Since is
// inclusive, Until exclusive.
type AuditFilter struct {
	Tenant   string
	Action   string
	ActorRef string
	TargetID string
	Since    *time.Time
	Until    *time.Time
}

// AuditCursor is the keyset position (created_at, id) of the last row of
// the previous page; rows strictly older follow.
type AuditCursor struct {
	CreatedAt time.Time
	ID        int64
}

// ListAudit returns up to limit+1 rows of f.Tenant, newest first; the
// extra row tells the caller another page exists.
func (d *DB) ListAudit(ctx context.Context, f AuditFilter, limit int, after *AuditCursor) ([]AuditRecord, error) {
	args := []any{f.Tenant}
	where := "tenant = $1"
	add := func(cond string, v any) {
		args = append(args, v)
		where += fmt.Sprintf(" AND "+cond, len(args))
	}
	if f.Action != "" {
		add("action = $%d", f.Action)
	}
	if f.ActorRef != "" {
		add("actor_ref = $%d", f.ActorRef)
	}
	if f.TargetID != "" {
		add("target_id = $%d", f.TargetID)
	}
	if f.Since != nil {
		add("created_at >= $%d", *f.Since)
	}
	if f.Until != nil {
		add("created_at < $%d", *f.Until)
	}
	if after != nil {
		args = append(args, after.CreatedAt, after.ID)
		where += fmt.Sprintf(" AND (created_at, id) < ($%d, $%d)", len(args)-1, len(args))
	}
	args = append(args, limit+1)
	rows, err := d.pool.Query(ctx, `
		SELECT id, tenant, actor_ref, action, target_kind, target_id, outcome, error_code, request_id, details, created_at
		FROM audit_event WHERE `+where+fmt.Sprintf(` ORDER BY created_at DESC, id DESC LIMIT $%d`, len(args)), args...)
	if err != nil {
		return nil, fmt.Errorf("store: list audit %w", err)
	}
	defer rows.Close()
	var out []AuditRecord
	for rows.Next() {
		var r AuditRecord
		var raw []byte
		if err := rows.Scan(&r.ID, &r.Tenant, &r.ActorRef, &r.Action, &r.TargetKind, &r.TargetID,
			&r.Outcome, &r.ErrorCode, &r.RequestID, &raw, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: scan audit %w", err)
		}
		if err := json.Unmarshal(raw, &r.Details); err != nil {
			return nil, fmt.Errorf("store: audit details %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// PruneAudit deletes rows created before `before`, batch rows per
// statement so one prune never holds a long lock.
func (d *DB) PruneAudit(ctx context.Context, before time.Time, batch int64) (int64, error) {
	var total int64
	for {
		tag, err := d.pool.Exec(ctx, `
			DELETE FROM audit_event WHERE id IN (
				SELECT id FROM audit_event WHERE created_at < $1 ORDER BY created_at LIMIT $2)`, before, batch)
		if err != nil {
			return total, fmt.Errorf("store: prune audit %w", err)
		}
		total += tag.RowsAffected()
		if tag.RowsAffected() < batch {
			return total, nil
		}
		if err := ctx.Err(); err != nil {
			return total, err
		}
	}
}

// AuditStore adapts the DB to the API's durable audit sink.
type AuditStore struct{ db *DB }

// NewAuditStore wraps db.
func NewAuditStore(db *DB) *AuditStore { return &AuditStore{db: db} }

// InsertAudit inserts through tx, or through the pool when tx is nil.
func (s *AuditStore) InsertAudit(ctx context.Context, tx Tx, r AuditRecord) (int64, error) {
	if tx != nil {
		return InsertAudit(ctx, tx, r)
	}
	return InsertAudit(ctx, s.db.pool, r)
}

// AmendAudit forwards to DB.AmendAudit.
func (s *AuditStore) AmendAudit(ctx context.Context, id int64, targetID, outcome, errorCode string, details map[string]string) error {
	return s.db.AmendAudit(ctx, id, targetID, outcome, errorCode, details)
}
