package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Session is a server-side OIDC session record. It mirrors the API-layer
// session shape; internal/backend adapts between the two (this package must
// not import internal/api — that would create an import cycle through
// internal/provisioning).
//
// Credential forms (SEC-27): Session.ID is the raw session ID at the Go
// boundary; the row is keyed by its SHA-256 digest, so a database or backup
// read never yields a usable session ID. The synchronizer CSRF token is
// derived from the session ID on demand (P1) and never persisted.
// IDTokenSeal seals the raw OIDC ID token for at-rest storage and opens it
// on read (api.IDTokenSealer over the login-state keys). A nil sealer
// stores NULL; an open failure yields an empty token rather than a failed
// read — logout then falls back to a client_id-only end-session URL
// (V3.24).
type IDTokenSeal interface {
	SealIDToken(raw string) (string, error)
	OpenIDToken(blob string) (string, error)
}

type Session struct {
	ID       string
	Issuer   string
	Subject  string
	TenantID string
	Groups   []string
	// DisplayName and Email are display-only identity copied from the
	// verified ID token at login (migration 012); they carry no
	// authorization meaning.
	DisplayName string
	Email       string
	// IDToken is the raw OIDC id_token retained for RP-initiated logout
	// (id_token_hint); the row holds only its AEAD-sealed form, never the
	// raw token (migration 015).
	IDToken    string
	CreatedAt  time.Time
	LastSeenAt time.Time
	ExpiresAt  time.Time // zero = no absolute expiry
}

// ErrSessionNotFound is returned for unknown or expired session IDs.
var ErrSessionNotFound = errors.New("store: session not found")

// sessionKey is the persisted primary key for a session ID: hex of
// SHA-256(id), the same construction tickets use (SEC-27). Mirrored by
// internal/api.auth.go; keep the digest identical.
func sessionKey(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:])
}

// SessionStore is the Postgres-backed session store. Get slides the idle
// deadline by writing last_seen_at; idle or absolute expiry makes the
// session invisible and removes it.
type SessionStore struct {
	db   *DB
	idle time.Duration
	seal IDTokenSeal // nil = id_token column stays NULL
	now  func() time.Time
}

// NewSessionStore returns a SessionStore with the sliding idle timeout.
// seal persists the retained OIDC ID token AEAD-sealed; nil keeps the
// column NULL (logout then falls back to client_id only).
func NewSessionStore(db *DB, idle time.Duration, seal IDTokenSeal) *SessionStore {
	return &SessionStore{db: db, idle: idle, seal: seal, now: time.Now}
}

// WithClock overrides the clock; tests only.
func (s *SessionStore) WithClock(now func() time.Time) *SessionStore {
	s.now = now
	return s
}

// Save inserts or replaces the session record, binding it to the current
// session epoch. If the epoch row is missing the insert fails — a session
// that would outlive a restore is never written.
func (s *SessionStore) Save(ctx context.Context, sess *Session) error {
	groups, err := json.Marshal(sess.Groups)
	if err != nil {
		return err
	}
	var expires *time.Time
	if !sess.ExpiresAt.IsZero() {
		expires = &sess.ExpiresAt
	}
	// Only digest/sealed forms are persisted (SEC-27): id holds SHA-256 of
	// the session ID and id_token holds the login-state AEAD seal of the
	// OIDC ID token — the raw token never reaches the row.
	var idToken *string
	if sess.IDToken != "" && s.seal != nil {
		sealed, err := s.seal.SealIDToken(sess.IDToken)
		if err != nil {
			return fmt.Errorf("session id_token seal: %w", err)
		}
		idToken = &sealed
	}
	_, err = s.db.Pool().Exec(ctx, `
		INSERT INTO sessions (id, issuer, subject, tenant_id, groups,
			display_name, email, created_at, last_seen_at, expires_at, epoch, id_token)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10,
			(SELECT value FROM platform_meta WHERE key = 'session_epoch'), $11)
		ON CONFLICT (id) DO UPDATE SET
			issuer = EXCLUDED.issuer, subject = EXCLUDED.subject,
			tenant_id = EXCLUDED.tenant_id, groups = EXCLUDED.groups,
			display_name = EXCLUDED.display_name, email = EXCLUDED.email,
			created_at = EXCLUDED.created_at,
			last_seen_at = EXCLUDED.last_seen_at, expires_at = EXCLUDED.expires_at,
			epoch = EXCLUDED.epoch, id_token = EXCLUDED.id_token`,
		sessionKey(sess.ID), sess.Issuer, sess.Subject, sess.TenantID, groups,
		sess.DisplayName, sess.Email,
		sess.CreatedAt, sess.LastSeenAt, expires, idToken)
	return err
}

// sessionReturning intentionally does NOT return id: the column holds the
// session-ID digest, and Session.ID must keep carrying the raw ID the
// caller looked up.
const sessionReturning = `
	RETURNING ` + sessionColumns

const sessionColumns = `issuer, subject, tenant_id, groups,
	display_name, email, created_at, last_seen_at, expires_at, id_token`

// currentEpochSQL resolves the session epoch in the same statement so a
// rotation takes effect on the very next read — no cached copy can go stale.
const currentEpochSQL = `(SELECT value FROM platform_meta WHERE key = 'session_epoch')`

// Get returns the session and slides last_seen_at. Unknown, expired, or
// stale-epoch sessions are deleted and reported as ErrSessionNotFound:
// the epoch comparison is what makes a restored database dump unable to
// resurrect a session revoked after the backup (F5). Expiry is evaluated
// server-side against the PRE-update last_seen_at; an interval string
// keeps the comparison unambiguously typed.
func (s *SessionStore) Get(ctx context.Context, id string) (*Session, error) {
	key := sessionKey(id)
	var row pgx.Row
	if s.idle > 0 {
		row = s.db.Pool().QueryRow(ctx, `
			UPDATE sessions SET last_seen_at = now()
			WHERE id = $1
			  AND epoch = `+currentEpochSQL+`
			  AND (expires_at IS NULL OR expires_at > now())
			  AND last_seen_at > now() - $2::interval`+sessionReturning,
			key, fmt.Sprintf("%dms", s.idle.Milliseconds()))
	} else {
		row = s.db.Pool().QueryRow(ctx, `
			UPDATE sessions SET last_seen_at = now()
			WHERE id = $1
			  AND epoch = `+currentEpochSQL+`
			  AND (expires_at IS NULL OR expires_at > now())`+sessionReturning, key)
	}
	return s.scanSession(ctx, row, key, id)
}

// scanSession materializes the sessionReturning/sessionColumns projection.
// A no-row read drops dead sessions opportunistically — including ones a
// rotated epoch orphaned — and reports ErrSessionNotFound.
func (s *SessionStore) scanSession(ctx context.Context, row pgx.Row, key, id string) (*Session, error) {
	var sess Session
	var groups []byte
	var expires *time.Time
	var idToken *string
	err := row.Scan(&sess.Issuer, &sess.Subject, &sess.TenantID, &groups,
		&sess.DisplayName, &sess.Email,
		&sess.CreatedAt, &sess.LastSeenAt, &expires, &idToken)
	if errors.Is(err, pgx.ErrNoRows) {
		if s.idle > 0 {
			_, _ = s.db.Pool().Exec(ctx, `
				DELETE FROM sessions WHERE id = $1
				  AND (epoch <> `+currentEpochSQL+`
				    OR (expires_at IS NOT NULL AND expires_at <= now())
				    OR last_seen_at <= now() - $2::interval)`,
				key, fmt.Sprintf("%dms", s.idle.Milliseconds()))
		} else {
			_, _ = s.db.Pool().Exec(ctx, `
				DELETE FROM sessions WHERE id = $1
				  AND (epoch <> `+currentEpochSQL+`
				    OR (expires_at IS NOT NULL AND expires_at <= now()))`, key)
		}
		return nil, ErrSessionNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("session get: %w", err)
	}
	sess.ID = id // raw ID at the Go boundary; the row key is its digest
	if err := json.Unmarshal(groups, &sess.Groups); err != nil {
		return nil, fmt.Errorf("session groups: %w", err)
	}
	if expires != nil {
		sess.ExpiresAt = *expires
	}
	// The sealed ID token opens to the raw token; a key that no longer
	// opens it yields "" — logout then falls back rather than failing.
	if idToken != nil && s.seal != nil {
		if raw, err := s.seal.OpenIDToken(*idToken); err == nil {
			sess.IDToken = raw
		}
	}
	return &sess, nil
}

// Peek returns the session without writing last_seen_at — the read passive
// endpoints use so polling them never extends the idle window (P4, D18).
// Validity rules are identical to Get: unknown, idle-expired,
// absolute-expired or stale-epoch sessions report ErrSessionNotFound.
func (s *SessionStore) Peek(ctx context.Context, id string) (*Session, error) {
	key := sessionKey(id)
	var row pgx.Row
	if s.idle > 0 {
		row = s.db.Pool().QueryRow(ctx, `
			SELECT `+sessionColumns+` FROM sessions
			WHERE id = $1
			  AND epoch = `+currentEpochSQL+`
			  AND (expires_at IS NULL OR expires_at > now())
			  AND last_seen_at > now() - $2::interval`,
			key, fmt.Sprintf("%dms", s.idle.Milliseconds()))
	} else {
		row = s.db.Pool().QueryRow(ctx, `
			SELECT `+sessionColumns+` FROM sessions
			WHERE id = $1
			  AND epoch = `+currentEpochSQL+`
			  AND (expires_at IS NULL OR expires_at > now())`, key)
	}
	return s.scanSession(ctx, row, key, id)
}

// TouchPrincipalSQL and TouchPrincipalIdleSQL are the statements behind
// TouchPrincipal (without / with an idle window), exported so the integration
// suite can EXPLAIN them: they match on the issuer and subject columns
// ($1, $2) so migration 013's (issuer, subject) index applies; the idle
// variant takes the window as $3.
const (
	TouchPrincipalSQL = `
		UPDATE sessions SET last_seen_at = now()
		WHERE issuer = $1 AND subject = $2
		  AND epoch = ` + currentEpochSQL + `
		  AND (expires_at IS NULL OR expires_at > now())`
	TouchPrincipalIdleSQL = TouchPrincipalSQL + `
		  AND last_seen_at > now() - $3::interval`
)

// TouchPrincipal slides last_seen_at for the principal's sessions that are
// still inside the idle window and before their absolute expiry (D18):
// desktop input keeps the owning user's portal session alive but can never
// revive an expired one. principal is the lease's principal_subject —
// "issuer|subject", the Principal.Owner() string — split at the first '|'
// (an issuer URL never contains one; a subject may). A principal without a
// separator matches nothing. Returns the row count actually updated.
func (s *SessionStore) TouchPrincipal(ctx context.Context, principal string) (int64, error) {
	issuer, subject, ok := strings.Cut(principal, "|")
	if !ok {
		return 0, nil
	}
	var (
		tag pgconn.CommandTag
		err error
	)
	if s.idle > 0 {
		tag, err = s.db.Pool().Exec(ctx, TouchPrincipalIdleSQL,
			issuer, subject, fmt.Sprintf("%dms", s.idle.Milliseconds()))
	} else {
		tag, err = s.db.Pool().Exec(ctx, TouchPrincipalSQL, issuer, subject)
	}
	if err != nil {
		return 0, fmt.Errorf("session touch: %w", err)
	}
	return tag.RowsAffected(), nil
}

// TouchSessionDigestSQL and TouchSessionDigestIdleSQL are the statements
// behind TouchSessionDigest (without / with an idle window): they address
// exactly one session row — $1 is the sessions.id key form (hex of the
// SHA-256 digest a lease recorded as portal_session_digest); the idle
// variant takes the window as $2.
const (
	TouchSessionDigestSQL = `
		UPDATE sessions SET last_seen_at = now()
		WHERE id = $1
		  AND epoch = ` + currentEpochSQL + `
		  AND (expires_at IS NULL OR expires_at > now())`
	TouchSessionDigestIdleSQL = TouchSessionDigestSQL + `
		  AND last_seen_at > now() - $2::interval`
)

// TouchSessionDigest slides last_seen_at for exactly the session row the
// digest names — the session the desktop stream's input arrived under
// (SR-1-F3): input under one session's lease no longer refreshes the
// principal's other sessions. The guards are identical to TouchPrincipal —
// live epoch, inside absolute expiry, inside the idle window — so input
// still can never revive an expired session. Returns the row count
// actually updated (0 for an unknown or dead row).
func (s *SessionStore) TouchSessionDigest(ctx context.Context, digestHex string) (int64, error) {
	var (
		tag pgconn.CommandTag
		err error
	)
	if s.idle > 0 {
		tag, err = s.db.Pool().Exec(ctx, TouchSessionDigestIdleSQL,
			digestHex, fmt.Sprintf("%dms", s.idle.Milliseconds()))
	} else {
		tag, err = s.db.Pool().Exec(ctx, TouchSessionDigestSQL, digestHex)
	}
	if err != nil {
		return 0, fmt.Errorf("session touch: %w", err)
	}
	return tag.RowsAffected(), nil
}

// Delete removes the session; deleting a missing ID is a no-op.
func (s *SessionStore) Delete(ctx context.Context, id string) error {
	_, err := s.db.Pool().Exec(ctx, `DELETE FROM sessions WHERE id = $1`, sessionKey(id))
	return err
}

// RotateSessionEpoch installs a fresh session epoch. Every session bound
// to the previous epoch is rejected on next read — the restore procedure
// runs this after loading a dump so pre-restore sessions (revoked or not)
// are unrecoverable. It also creates the epoch infrastructure defensively
// so it works against dumps taken before migration 009.
func (d *DB) RotateSessionEpoch(ctx context.Context) error {
	_, err := d.pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS platform_meta (
			key   text PRIMARY KEY,
			value text NOT NULL
		);
		ALTER TABLE sessions ADD COLUMN IF NOT EXISTS epoch text NOT NULL DEFAULT '';
		INSERT INTO platform_meta (key, value)
		VALUES ('session_epoch', gen_random_uuid()::text)
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`)
	return err
}
