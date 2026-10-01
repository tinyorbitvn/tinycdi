package provisioning

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/tinyorbitvn/tinycdi/internal/store"
)

// ErrIdempotencyConflict is returned when an idempotency key is replayed
// with a different request body than the one originally stored.
var ErrIdempotencyConflict = errors.New("idempotency key replayed with a different request body")

// ErrBadCursor is returned when a page token fails to decode — a client
// error (400), never an internal one.
var ErrBadCursor = errors.New("bad page token")

// scopedIdemKey binds a caller-supplied idempotency key to the
// authenticated principal: the stored key is <sha256(actor)[:16]>|<key>,
// so the same client key under a different principal lands in an
// independent (tenant, actor) namespace and can never replay another
// principal's stored result (SEC-21). The fixed-width hex prefix keeps the
// composite unambiguous even though both fields may contain '|'.
func scopedIdemKey(actor, key string) string {
	sum := sha256.Sum256([]byte(actor))
	return hex.EncodeToString(sum[:16]) + "|" + key
}

// IsIdempotencyConflict reports whether err is a key/body conflict.
func IsIdempotencyConflict(err error) bool {
	return errors.Is(err, ErrIdempotencyConflict)
}

// RequestHash is the canonical fingerprint of a request body used for
// idempotency comparison.
func RequestHash(v any) []byte {
	canonical, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("request hash: marshal %v", err))
	}
	sum := sha256.Sum256(canonical)
	return sum[:]
}

// DeterministicRequestID derives the stable request ID for a
// (tenant, idempotency key) pair. The same pair always yields the same
// ID, so a replayed create maps back to the stored result instead of
// minting a new workspace.
func DeterministicRequestID(tenantID, key string) string {
	// UUIDv5 (SHA-1, namespace = URL namespace OID) over tenant|key.
	ns := []byte{0x6b, 0xa7, 0xb8, 0x11, 0x9d, 0xad, 0x11, 0xd1,
		0x80, 0xb4, 0x00, 0xc0, 0x4f, 0xd4, 0x30, 0xc8}
	h := sha1.New()
	h.Write(ns)
	h.Write([]byte(tenantID + "|" + key))
	sum := h.Sum(nil)
	sum[6] = (sum[6] & 0x0f) | 0x50 // version 5
	sum[8] = (sum[8] & 0x3f) | 0x80 // variant
	var b [36]byte
	hex.Encode(b[0:8], sum[0:4])
	b[8] = '-'
	hex.Encode(b[9:13], sum[4:6])
	b[13] = '-'
	hex.Encode(b[14:18], sum[6:8])
	b[18] = '-'
	hex.Encode(b[19:23], sum[8:10])
	b[23] = '-'
	hex.Encode(b[24:36], sum[10:16])
	return string(b[:])
}

// IdemRecord is the stored state for an idempotency key.
type IdemRecord struct {
	RequestID string
	Result    json.RawMessage // nil while the operation is unrecorded
}

// BeginIdempotent registers (tenantID, key) with the request hash inside
// tx. Callers pass an actor-scoped key (scopedIdemKey) so a key only ever
// resolves inside the (tenant, principal) namespace that created it. It
// returns the stored record and replayed=true when the key was completed
// before with the same operation and body; replayed=false means the
// caller must execute the operation and then call CompleteIdempotent.
//
// Same key + different op or different body -> ErrIdempotencyConflict.
// A key whose first attempt died before recording a result is resumed
// with its original deterministic request ID.
func BeginIdempotent(ctx context.Context, tx store.Tx, tenantID, key, op string, requestHash []byte) (IdemRecord, bool, error) {
	requestID := DeterministicRequestID(tenantID, key)
	_, err := tx.Exec(ctx, `
		INSERT INTO idempotency (tenant_id, key, op, request_hash, request_id)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (tenant_id, key) DO NOTHING`,
		tenantID, key, op, requestHash, requestID)
	if err != nil {
		return IdemRecord{}, false, fmt.Errorf("idempotency: begin %w", err)
	}

	var rec IdemRecord
	var storedHash []byte
	var storedOp string
	err = tx.QueryRow(ctx, `
		SELECT op, request_hash, request_id, result FROM idempotency
		WHERE tenant_id = $1 AND key = $2 FOR UPDATE`, tenantID, key).
		Scan(&storedOp, &storedHash, &rec.RequestID, &rec.Result)
	if err != nil {
		return IdemRecord{}, false, fmt.Errorf("idempotency: read %w", err)
	}
	// SEC-21: the operation (including the resource it names) is part of
	// the comparison — reusing a key for a different operation is a
	// conflict, never a replay of the other operation's result.
	if storedOp != op || !equalBytes(storedHash, requestHash) {
		return IdemRecord{}, false, ErrIdempotencyConflict
	}
	if rec.Result != nil {
		return rec, true, nil
	}
	return rec, false, nil
}

// CompleteIdempotent stores the operation result for the key inside tx so
// a later replay short-circuits with it.
func CompleteIdempotent(ctx context.Context, tx store.Tx, tenantID, key string, result any) error {
	body, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("idempotency: marshal result %w", err)
	}
	tag, err := tx.Exec(ctx, `
		UPDATE idempotency SET result = $3
		WHERE tenant_id = $1 AND key = $2`, tenantID, key, body)
	if err != nil {
		return fmt.Errorf("idempotency: complete %w", err)
	}
	if tag.RowsAffected() == 0 {
		return errors.New("idempotency: complete without begin")
	}
	return nil
}

// PruneIdempotency deletes idempotency rows older than the contract's
// 24 h key lifetime. Keys are single-scope and short-lived by design;
// without a sweep the table grows without bound. Intended to run on a
// periodic sweep (e.g. hourly) by the API host.
func (s *Service) PruneIdempotency(ctx context.Context) (int64, error) {
	tag, err := s.db.Pool().Exec(ctx, `
		DELETE FROM idempotency WHERE created_at < now() - interval '24 hours'`)
	if err != nil {
		return 0, fmt.Errorf("idempotency: prune %w", err)
	}
	return tag.RowsAffected(), nil
}

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := range a {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}
