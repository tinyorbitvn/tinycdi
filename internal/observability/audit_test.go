package observability

import (
	"bytes"
	"context"
	"encoding/json"
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
