package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	cdiv1 "github.com/tinyorbitvn/tinycdi/api/v1"
	"github.com/tinyorbitvn/tinycdi/internal/api/oidctest"
	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
	"github.com/tinyorbitvn/tinycdi/internal/sessionhost"
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
	gotPolicy provisioning.SessionPolicy
	calls     int
}

func (f *fakeIssuer) IssueTicket(_ context.Context, p Principal, wsUID string, takeover bool, policy provisioning.SessionPolicy, _ string) (IssuedTicket, *Error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.gotOwner = p.Owner()
	f.gotTenant = p.TenantID
	f.gotWS = wsUID
	f.gotTakeov = takeover
	f.gotPolicy = policy
	return f.ticket, f.err
}

func newConnectionEnv(t *testing.T, issuer ConnectionIssuer, opts ...func(*ConnectionHandler)) *testEnv {
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
	d, err := sessionhost.ParseDomain("session.example.test")
	if err != nil {
		t.Fatalf("sessionhost.ParseDomain: %v", err)
	}
	h := NewConnectionHandler(issuer, defaultTenants(), d).WithLogger(logger)
	for _, o := range opts {
		o(h)
	}
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
	if view.LaunchURL != "https://ws-00000000000000000000000001.session.example.test/v1/launch" {
		t.Fatalf("launchUrl=%q, want per-workspace host /v1/launch", view.LaunchURL)
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

// TestCreateConnection_ResolvedPolicyRidesTicket (ADR 0009 §2): the
// session policy the WithClipboardSource resolver returns — template
// policy ∧ tenant ceiling — is what reaches the ticket issuer and what
// the response mirrors back, so the portal's frame flags always match
// what the ticket recorded.
func TestCreateConnection_ResolvedPolicyRidesTicket(t *testing.T) {
	fi := &fakeIssuer{ticket: IssuedTicket{
		WorkspaceID: "ws_00000000000000000000000001",
		Token:       "tkt_testopaquevalue_0123456789abcdef",
		ExpiresAt:   time.Now().Add(60 * time.Second).UTC(),
	}}
	env := newConnectionEnv(t, fi, func(h *ConnectionHandler) {
		h.WithClipboardSource(func(_ context.Context, _ Principal, _ string) (provisioning.SessionPolicy, error) {
			return provisioning.SessionPolicy{
				Clipboard:    provisioning.PolicyDisabled,
				FileTransfer: cdiv1.FileTransferDisabled,
			}, nil
		})
	})
	sess, csrf := login(t, env, "alice")

	r := doReq(t, env, sess, csrf, http.MethodPost,
		"/v1/workspaces/ws_00000000000000000000000001/connections", "", nil)
	defer r.Body.Close()
	if r.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(r.Body)
		t.Fatalf("status=%d body=%s, want 201", r.StatusCode, b)
	}
	if fi.gotPolicy.Clipboard != "Disabled" {
		t.Fatalf("issuer policy = %+v, want clipboard Disabled", fi.gotPolicy)
	}
	var view struct {
		SessionPolicy struct {
			Clipboard    string `json:"clipboard"`
			FileTransfer string `json:"fileTransfer"`
		} `json:"sessionPolicy"`
	}
	if err := json.NewDecoder(r.Body).Decode(&view); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if view.SessionPolicy.Clipboard != "Disabled" || view.SessionPolicy.FileTransfer != "Disabled" {
		t.Fatalf("response sessionPolicy = %+v, want Disabled/Disabled", view.SessionPolicy)
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

// TestCreateConnection_InvalidBodyGenericMessage: a malformed body answers
// 400 INVALID_REQUEST with the generic message every other handler uses —
// the decoder detail is logged server-side, never echoed (SEC-I7).
func TestCreateConnection_InvalidBodyGenericMessage(t *testing.T) {
	fi := &fakeIssuer{}
	env := newConnectionEnv(t, fi)
	sess, csrf := login(t, env, "alice")

	r := doReq(t, env, sess, csrf, http.MethodPost,
		"/v1/workspaces/ws_00000000000000000000000001/connections",
		`{"takeover":true,"attacker_field":1}`, nil)
	defer r.Body.Close()
	if r.StatusCode != http.StatusBadRequest {
		b, _ := io.ReadAll(r.Body)
		t.Fatalf("status=%d body=%s, want 400", r.StatusCode, b)
	}
	b, _ := io.ReadAll(r.Body)
	var wrap struct {
		Error Error `json:"error"`
	}
	if err := json.Unmarshal(b, &wrap); err != nil {
		t.Fatalf("error body not JSON: %q", b)
	}
	e := wrap.Error
	if e.Code != CodeInvalidRequest || e.Message != "invalid request body" {
		t.Fatalf("code=%q message=%q, want INVALID_REQUEST + generic message", e.Code, e.Message)
	}
	if bytes.Contains(b, []byte("attacker_field")) {
		t.Fatalf("decoder detail echoed to client: %s", b)
	}
	if !strings.Contains(env.logs.String(), "attacker_field") {
		t.Fatalf("decoder detail missing from server log: %s", env.logs.String())
	}
	if fi.calls != 0 {
		t.Fatal("issuer called on an invalid body")
	}
}

// TestCreateConnection_TrailingJSONRejected: the shared decoder accepts
// exactly one document — a second JSON value after the request object is a
// 400 like everywhere else.
func TestCreateConnection_TrailingJSONRejected(t *testing.T) {
	fi := &fakeIssuer{}
	env := newConnectionEnv(t, fi)
	sess, csrf := login(t, env, "alice")

	r := doReq(t, env, sess, csrf, http.MethodPost,
		"/v1/workspaces/ws_00000000000000000000000001/connections",
		`{"takeover":true} {"extra":true}`, nil)
	defer r.Body.Close()
	if r.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", r.StatusCode)
	}
	if fi.calls != 0 {
		t.Fatal("issuer called on trailing data")
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
	e := decodeErr(t, r)
	if e.Code != CodeConnectionInUse {
		t.Fatalf("code=%q, want CONNECTION_IN_USE", e.Code)
	}
}

// TestCreateConnection_NotFoundAndBadID: unknown workspace -> 404, an id
// that cannot map to a session host -> 409 INVALID_STATE, unknown tenant
// -> 403.
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
	defer r.Body.Close()
	if r.StatusCode != http.StatusConflict {
		t.Fatalf("bad-id status=%d, want 409", r.StatusCode)
	}
	e := decodeErr(t, r)
	if e.Code != CodeInvalidState {
		t.Fatalf("bad-id code=%q, want INVALID_STATE", e.Code)
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

// TestLaunchURL_PerWorkspaceHost: the launch URL points at the workspace's
// own session host — ws-<suffix>.<session domain> — and two workspaces get
// two different hosts (D9).
func TestLaunchURL_PerWorkspaceHost(t *testing.T) {
	fi := &fakeIssuer{ticket: IssuedTicket{
		Token:     "tkt_launch_url_0123456789abcdef",
		ExpiresAt: time.Now().Add(60 * time.Second).UTC(),
	}}
	env := newConnectionEnv(t, fi)
	sess, csrf := login(t, env, "alice")

	hosts := map[string]bool{}
	for _, wsID := range []string{"ws_abc12345", "ws_zzz999yy"} {
		fi.ticket.WorkspaceID = wsID
		r := doReq(t, env, sess, csrf, http.MethodPost,
			"/v1/workspaces/"+wsID+"/connections", `{}`, nil)
		var view struct {
			LaunchURL string `json:"launchUrl"`
		}
		if err := json.NewDecoder(r.Body).Decode(&view); err != nil {
			t.Fatalf("decode: %v", err)
		}
		r.Body.Close()
		if r.StatusCode != http.StatusCreated {
			t.Fatalf("%s: status=%d, want 201", wsID, r.StatusCode)
		}
		u, err := url.Parse(view.LaunchURL)
		if err != nil {
			t.Fatalf("%s: launchUrl %q does not parse: %v", wsID, view.LaunchURL, err)
		}
		label, err := sessionhost.Label(wsID)
		if err != nil {
			t.Fatalf("Label(%q): %v", wsID, err)
		}
		if u.Host != label+".session.example.test" {
			t.Fatalf("%s: launchUrl host = %q, want %s.session.example.test", wsID, u.Host, label)
		}
		if u.Scheme != "https" || u.Path != LaunchPath {
			t.Fatalf("%s: launchUrl = %q, want https://<host>/v1/launch", wsID, view.LaunchURL)
		}
		hosts[u.Host] = true
	}
	if len(hosts) != 2 {
		t.Fatalf("two workspaces share a launch host: %v", hosts)
	}
}

// TestConnectionCreate_RequiresPortalSession: a principal without a portal
// session (no session in the request context) is refused with 401 before the
// issuer runs — every ticket and lease is bound to a portal session.
func TestConnectionCreate_RequiresPortalSession(t *testing.T) {
	issuer := &fakeIssuer{}
	d, err := sessionhost.ParseDomain("session.example.test")
	if err != nil {
		t.Fatalf("sessionhost.ParseDomain: %v", err)
	}
	h := NewConnectionHandler(issuer, defaultTenants(), d)
	req := httptest.NewRequest(http.MethodPost, "/v1/workspaces/ws_00000000000000000000000001/connections", strings.NewReader(`{}`))
	req.SetPathValue("id", "ws_00000000000000000000000001")
	req = req.WithContext(withPrincipalNoSession(req.Context()))
	rec := httptest.NewRecorder()
	h.Create(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d want 401; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), string(CodeUnauthenticated)) {
		t.Fatalf("body %s lacks %s", rec.Body.String(), CodeUnauthenticated)
	}
	if issuer.calls != 0 {
		t.Fatalf("IssueTicket called %d times, want 0", issuer.calls)
	}
}

// withPrincipalNoSession sets a verified principal but no portal session, so
// SessionFromContext reports (nil, false).
func withPrincipalNoSession(ctx context.Context) context.Context {
	return WithPrincipal(ctx, Principal{Issuer: "https://idp.test", Subject: "alice", TenantID: "tenant-a"})
}
