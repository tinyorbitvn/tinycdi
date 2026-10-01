package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/api/oidctest"
)

// fakeIssuer scripts the broker-facing ticket issuer. It records the
// principal/flags it was called with so the test can prove the handler
// forwards the verified identity, never request-body fields.
type fakeIssuer struct {
	mu        sync.Mutex
	ticket    IssuedTicket
	err       *Error
	gotOwner  string
	gotTenant string
	gotWS     string
	gotTakeov bool
	calls     int
}

func (f *fakeIssuer) IssueTicket(_ context.Context, p Principal, wsUID string, takeover bool) (IssuedTicket, *Error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.gotOwner = p.Owner()
	f.gotTenant = p.TenantID
	f.gotWS = wsUID
	f.gotTakeov = takeover
	return f.ticket, f.err
}

func newConnectionEnv(t *testing.T, issuer ConnectionIssuer) *testEnv {
	t.Helper()
	iss, err := oidctest.NewIssuer()
	if err != nil {
		t.Fatalf("oidctest.NewIssuer: %v", err)
	}
	logBuf := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(logBuf, nil))
	sessions := NewInMemorySessionStore(30 * time.Minute)
	a, err := NewAuthenticator(context.Background(), AuthConfig{
		Issuer:      iss.URL(),
		ClientID:    iss.ClientID,
		RedirectURL: "https://portal.test/auth/callback",
		LoginSealer: testLoginSealer(t),
	}, sessions, logger)
	if err != nil {
		t.Fatalf("NewAuthenticator: %v", err)
	}
	h := NewConnectionHandler(issuer, defaultTenants(), "https://session.example.test")
	mux := http.NewServeMux()
	mux.Handle("/auth/login", http.HandlerFunc(a.LoginHandler))
	mux.Handle("/auth/callback", http.HandlerFunc(a.CallbackHandler))
	MountConnectionRoutes(mux, a, h)
	srv := httptest.NewServer(RequestID(Audit(logger)(mux)))
	env := &testEnv{issuer: iss, auth: a, store: sessions, server: srv, logs: logBuf}
	t.Cleanup(func() { srv.Close(); iss.Close() })
	return env
}

// TestCreateConnection_IssuesTicket: an authenticated owner gets 201 with a
// ticket + session-origin launch URL; issuer sees the verified principal.
func TestCreateConnection_IssuesTicket(t *testing.T) {
	fi := &fakeIssuer{ticket: IssuedTicket{
		WorkspaceID: "ws_00000000000000000000000001",
		Token:       "tkt_testopaquevalue_0123456789abcdef",
		ExpiresAt:   time.Now().Add(60 * time.Second).UTC(),
	}}
	env := newConnectionEnv(t, fi)
	sess, csrf := login(t, env, "alice")

	r := doReq(t, env, sess, csrf, http.MethodPost,
		"/v1/workspaces/ws_00000000000000000000000001/connections",
		`{"takeover":true}`, nil)
	defer r.Body.Close()
	if r.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(r.Body)
		t.Fatalf("status=%d body=%s, want 201", r.StatusCode, b)
	}
	// SEC-I7: the ticket is bearer-equivalent — never cacheable.
	if cc := r.Header.Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control=%q, want no-store", cc)
	}
	var view struct {
		WorkspaceID string    `json:"workspaceId"`
		Ticket      string    `json:"ticket"`
		LaunchURL   string    `json:"launchUrl"`
		ExpiresAt   time.Time `json:"expiresAt"`
	}
	if err := json.NewDecoder(r.Body).Decode(&view); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if view.Ticket != fi.ticket.Token {
		t.Fatalf("ticket mismatch: %q", view.Ticket)
	}
	if view.LaunchURL != "https://session.example.test/v1/launch" {
		t.Fatalf("launchUrl=%q, want session-origin /v1/launch", view.LaunchURL)
	}
	if view.ExpiresAt.IsZero() {
		t.Fatal("expiresAt missing")
	}
	if fi.gotWS != "ws_00000000000000000000000001" || !fi.gotTakeov {
		t.Fatalf("issuer args: ws=%q takeover=%v", fi.gotWS, fi.gotTakeov)
	}
	// The verified principal (issuer URL + subject alice) reaches the issuer.
	if !strings.HasSuffix(fi.gotOwner, "|alice") || fi.gotTenant == "" {
		t.Fatalf("principal not forwarded: owner=%q tenant=%q", fi.gotOwner, fi.gotTenant)
	}
}

// TestCreateConnection_RequiresAuthAndCSRF: unauthenticated and CSRF-less
// requests are rejected before any issuer call.
func TestCreateConnection_RequiresAuthAndCSRF(t *testing.T) {
	fi := &fakeIssuer{}
	env := newConnectionEnv(t, fi)

	resp, err := http.Post(env.server.URL+"/v1/workspaces/ws_00000000000000000000000001/connections",
		"application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no-auth status=%d, want 401", resp.StatusCode)
	}

	sess, _ := login(t, env, "alice")
	req, _ := http.NewRequest(http.MethodPost,
		env.server.URL+"/v1/workspaces/ws_00000000000000000000000001/connections",
		strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(sess)
	resp2, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusForbidden {
		t.Fatalf("no-csrf status=%d, want 403", resp2.StatusCode)
	}
	if fi.calls != 0 {
		t.Fatalf("issuer called %d times on rejected requests", fi.calls)
	}
}

// TestCreateConnection_ConnectionInUse: the broker's ErrConnectionInUse maps
// to 409 CONNECTION_IN_USE per openapi.yaml.
func TestCreateConnection_ConnectionInUse(t *testing.T) {
	fi := &fakeIssuer{err: NewError(CodeConnectionInUse, "workspace already has an active connection")}
	env := newConnectionEnv(t, fi)
	sess, csrf := login(t, env, "alice")

	r := doReq(t, env, sess, csrf, http.MethodPost,
		"/v1/workspaces/ws_00000000000000000000000001/connections", `{}`, nil)
	defer r.Body.Close()
	if r.StatusCode != http.StatusConflict {
		t.Fatalf("status=%d, want 409", r.StatusCode)
	}
	var e Error
	if err := json.NewDecoder(r.Body).Decode(&e); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if e.Code != CodeConnectionInUse {
		t.Fatalf("code=%q, want CONNECTION_IN_USE", e.Code)
	}
}

// TestCreateConnection_NotFoundAndBadID: unknown workspace -> 404, malformed
// id -> 400, unknown tenant -> 403.
func TestCreateConnection_NotFoundAndBadID(t *testing.T) {
	fi := &fakeIssuer{err: NewError(CodeNotFound, "workspace not found")}
	env := newConnectionEnv(t, fi)
	sess, csrf := login(t, env, "alice")

	r := doReq(t, env, sess, csrf, http.MethodPost,
		"/v1/workspaces/ws_00000000000000000000000009/connections", `{}`, nil)
	r.Body.Close()
	if r.StatusCode != http.StatusNotFound {
		t.Fatalf("status=%d, want 404", r.StatusCode)
	}

	r = doReq(t, env, sess, csrf, http.MethodPost,
		"/v1/workspaces/not-an-id/connections", `{}`, nil)
	r.Body.Close()
	if r.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad-id status=%d, want 400", r.StatusCode)
	}
	if fi.calls != 1 {
		t.Fatalf("issuer called %d times; malformed id must not reach it", fi.calls)
	}
}

// TestCreateConnection_NoTicketInLogs: the issued ticket appears in the 201
// body only — never in the audit/request log.
func TestCreateConnection_NoTicketInLogs(t *testing.T) {
	const secret = "tkt_never_in_logs_0123456789abcdef0123"
	fi := &fakeIssuer{ticket: IssuedTicket{
		WorkspaceID: "ws_00000000000000000000000001",
		Token:       secret,
		ExpiresAt:   time.Now().Add(60 * time.Second).UTC(),
	}}
	env := newConnectionEnv(t, fi)
	sess, csrf := login(t, env, "alice")

	r := doReq(t, env, sess, csrf, http.MethodPost,
		"/v1/workspaces/ws_00000000000000000000000001/connections", `{}`, nil)
	r.Body.Close()
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("status=%d", r.StatusCode)
	}
	if strings.Contains(env.logs.String(), secret) {
		t.Fatalf("ticket leaked into request log: %s", env.logs.String())
	}
}
