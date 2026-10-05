package observability

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestJSONSinkWritesSchema(t *testing.T) {
	var buf bytes.Buffer
	sink := NewJSONSink(&buf)

	err := sink.WriteAudit(context.Background(), AuditEvent{
		Actor:     ActorRef("https://idp.example", "user-1"),
		Action:    "workspace.create",
		TargetUID: "uid-123",
		Tenant:    "tenant-a",
		RequestID: "req-1",
		Outcome:   OutcomeSuccess,
	})
	if err != nil {
		t.Fatal(err)
	}
	var e map[string]any
	if err := json.Unmarshal(buf.Bytes(), &e); err != nil {
		t.Fatalf("audit record not JSON: %v", err)
	}
	for _, k := range []string{"time", "actor", "action", "targetUid", "tenant", "requestId", "outcome"} {
		if _, ok := e[k]; !ok {
			t.Fatalf("audit record missing field %q: %v", k, e)
		}
	}
	if e["outcome"] != "success" || e["action"] != "workspace.create" {
		t.Fatalf("bad record: %v", e)
	}
}

func TestActorRefPseudonymousAndStable(t *testing.T) {
	a1 := ActorRef("https://idp.example", "user-1")
	a2 := ActorRef("https://idp.example", "user-1")
	if a1 != a2 {
		t.Fatal("ActorRef not stable")
	}
	if strings.Contains(a1, "user-1") || strings.Contains(a1, "idp.example") {
		t.Fatalf("ActorRef leaks raw identity: %q", a1)
	}
	if ActorRef("https://idp.example", "user-2") == a1 {
		t.Fatal("ActorRef collides across subjects")
	}
	if got := ActorRef("", ""); got != "anonymous" {
		t.Fatalf("empty identity should map to anonymous, got %q", got)
	}
}

func TestAuditRedactsSecretLikeFields(t *testing.T) {
	var buf bytes.Buffer
	sink := NewJSONSink(&buf)

	secrets := []string{
		"Bearer ultra-secret-token-456",
		"session=abc123def456",
		"oidc-state-value-789",
	}
	err := sink.WriteAudit(context.Background(), AuditEvent{
		Actor:     ActorRef("i", "s"),
		Action:    "auth.callback",
		RequestID: "req-9",
		Outcome:   OutcomeFailure,
		ErrorCode: "UNAUTHENTICATED",
		Details: map[string]string{
			"authorization": secrets[0],
			"cookie":        secrets[1],
			"state":         secrets[2],
			"note":          "plain-ok",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, s := range secrets {
		if strings.Contains(out, s) {
			t.Fatalf("audit output contains secret value %q", s)
		}
	}
	if !strings.Contains(out, "plain-ok") {
		t.Fatal("non-sensitive detail was lost")
	}
	if !strings.Contains(out, "[REDACTED]") {
		t.Fatal("redaction marker missing")
	}
}

func TestJSONSinkFillsTime(t *testing.T) {
	var buf bytes.Buffer
	sink := NewJSONSink(&buf)
	sink.clock = func() time.Time { return time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC) }
	if err := sink.WriteAudit(context.Background(), AuditEvent{
		Actor: "anonymous", Action: "x", RequestID: "r", Outcome: OutcomeDenied,
	}); err != nil {
		t.Fatal(err)
	}
	var e map[string]any
	_ = json.Unmarshal(buf.Bytes(), &e)
	if !strings.Contains(e["time"].(string), "2026-09-30T12:00:00") {
		t.Fatalf("time not filled: %v", e["time"])
	}
}

// failSink is an AuditSink whose every write fails.
type failSink struct{ err error }

func (f failSink) WriteAudit(context.Context, AuditEvent) error { return f.err }

// TestGuardedSink_ReportsFailureNeverPropagates: a failing inner sink must
// still satisfy the "audit never fails the request" contract — WriteAudit
// returns nil — while every failure is counted via onError and the warn
// log is rate-limited to one line per warnEvery so a permanently broken
// sink cannot flood the stream; the next emitted line reports how many
// failures were suppressed in between.
func TestGuardedSink_ReportsFailureNeverPropagates(t *testing.T) {
	var logBuf bytes.Buffer
	var counted []string
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s := NewGuardedSink(failSink{err: errors.New("sink down")},
		slog.New(slog.NewTextHandler(&logBuf, nil)),
		func(action string) { counted = append(counted, action) })
	s.now = func() time.Time { return now }
	s.warnEvery = time.Minute

	e := AuditEvent{Action: "workspace.create", Outcome: OutcomeSuccess}
	if err := s.WriteAudit(context.Background(), e); err != nil {
		t.Fatalf("guarded write returned error: %v", err)
	}
	if len(counted) != 1 || counted[0] != "workspace.create" {
		t.Fatalf("onError calls = %v, want [workspace.create]", counted)
	}
	if n := strings.Count(logBuf.String(), "audit sink write failed"); n != 1 {
		t.Fatalf("warn lines = %d, want 1: %q", n, logBuf.String())
	}

	// Failures inside warnEvery: counted, not re-logged.
	_ = s.WriteAudit(context.Background(), e)
	_ = s.WriteAudit(context.Background(), e)
	if len(counted) != 3 {
		t.Fatalf("onError calls = %d, want 3 (every failure counts)", len(counted))
	}
	if n := strings.Count(logBuf.String(), "audit sink write failed"); n != 1 {
		t.Fatalf("warn lines = %d, want 1 (rate-limited): %q", n, logBuf.String())
	}

	// Past warnEvery the next failure logs again, carrying the suppressed
	// count — and a healthy inner write passes through untouched.
	now = now.Add(2 * time.Minute)
	_ = s.WriteAudit(context.Background(), e)
	if n := strings.Count(logBuf.String(), "audit sink write failed"); n != 2 {
		t.Fatalf("warn lines = %d, want 2: %q", n, logBuf.String())
	}
	if !strings.Contains(logBuf.String(), "suppressed_since_last=2") {
		t.Fatalf("warn lacks the suppressed count: %q", logBuf.String())
	}
}

// TestGuardedSink_InnerSuccessPassesThrough: successful writes are
// delivered verbatim and report nothing.
func TestGuardedSink_InnerSuccessPassesThrough(t *testing.T) {
	var buf, logBuf bytes.Buffer
	var counted int
	s := NewGuardedSink(NewJSONSink(&buf),
		slog.New(slog.NewTextHandler(&logBuf, nil)),
		func(string) { counted++ })
	if err := s.WriteAudit(context.Background(), AuditEvent{
		Actor: "oidc:abc", Action: "session.revoke", Outcome: OutcomeSuccess,
	}); err != nil {
		t.Fatal(err)
	}
	if buf.Len() == 0 || counted != 0 || logBuf.Len() != 0 {
		t.Fatalf("success path polluted: out=%q counted=%d log=%q", buf.String(), counted, logBuf.String())
	}
}
