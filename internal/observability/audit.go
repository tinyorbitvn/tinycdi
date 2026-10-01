package observability

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"regexp"
	"sync"
	"time"
)

// AuditOutcome is the bounded outcome enum for audit events.
type AuditOutcome string

const (
	OutcomeSuccess AuditOutcome = "success"
	OutcomeFailure AuditOutcome = "failure"
	OutcomeDenied  AuditOutcome = "denied"
)

// AuditEvent is the structured audit record schema. Actor is ALWAYS the
// pseudonymous ActorRef (SHA-256 of issuer|subject, truncated) — raw subject,
// email or tokens must never be written. TargetUID may carry a workspace UID
// (audit trails need correlation; unlike metrics, audit logs are not
// cardinality-sensitive, but secrets remain forbidden).
type AuditEvent struct {
	Time      time.Time         `json:"time"`
	Actor     string            `json:"actor"`
	Action    string            `json:"action"`
	TargetUID string            `json:"targetUid,omitempty"`
	Tenant    string            `json:"tenant,omitempty"`
	RequestID string            `json:"requestId"`
	Outcome   AuditOutcome      `json:"outcome"`
	ErrorCode string            `json:"errorCode,omitempty"`
	Details   map[string]string `json:"details,omitempty"`
}

// ActorRef returns the pseudonymous actor identifier used in audit records:
// "oidc:" + first 8 bytes of SHA-256(issuer \x00 subject), hex-encoded.
// Stable for correlation across a user's actions, and does not expose the
// raw subject/email. The zero identity maps to "anonymous".
func ActorRef(issuer, subject string) string {
	if issuer == "" && subject == "" {
		return "anonymous"
	}
	sum := sha256.Sum256([]byte(issuer + "\x00" + subject))
	return "oidc:" + hex.EncodeToString(sum[:8])
}

// sensitiveFieldRe matches field/Detail keys that may carry credentials.
// It is the single redaction policy shared with the HTTP middleware audit
// path: any matching key's value is replaced with RedactedValue before the
// record leaves the process.
var sensitiveFieldRe = regexp.MustCompile(`(?i)(token|cookie|authorization|secret|password|credential|session|nonce|state|verifier|code|api[-_]?key|ticket|saml|assertion)`)

// RedactedValue replaces any sensitive value in emitted records.
const RedactedValue = "[REDACTED]"

// IsSensitiveField reports whether a field name may carry credential-like
// material and must be redacted.
func IsSensitiveField(name string) bool { return sensitiveFieldRe.MatchString(name) }

// RedactDetails returns a copy of d where every sensitive-keyed value is
// replaced with RedactedValue.
func RedactDetails(d map[string]string) map[string]string {
	if len(d) == 0 {
		return d
	}
	out := make(map[string]string, len(d))
	for k, v := range d {
		if IsSensitiveField(k) {
			out[k] = RedactedValue
		} else {
			out[k] = v
		}
	}
	return out
}

// AuditSink is the writer interface for audit events — implement it over
// Postgres/object storage in later tasks; JSONSink is the built-in
// line-delimited JSON sink.
type AuditSink interface {
	WriteAudit(ctx context.Context, e AuditEvent) error
}

// JSONSink writes one JSON-encoded AuditEvent per line to w.
type JSONSink struct {
	mu    sync.Mutex
	w     io.Writer
	clock func() time.Time
}

// NewJSONSink returns an AuditSink writing JSONL to w.
func NewJSONSink(w io.Writer) *JSONSink {
	return &JSONSink{w: w, clock: func() time.Time { return time.Now().UTC() }}
}

// WriteAudit fills Time when unset, redacts sensitive Detail keys, and emits
// the record as one JSON line.
func (s *JSONSink) WriteAudit(_ context.Context, e AuditEvent) error {
	if e.Time.IsZero() {
		e.Time = s.clock()
	}
	e.Details = RedactDetails(e.Details)
	s.mu.Lock()
	defer s.mu.Unlock()
	return json.NewEncoder(s.w).Encode(e)
}
