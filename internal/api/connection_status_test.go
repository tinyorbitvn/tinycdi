package api

// Contract tests for GET /v1/workspaces/{id}/connection (P4): a passive
// endpoint that reports the workspace's session connection state without
// sliding the portal idle timer (D18).

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/api/oidctest"
	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
)

// fakeStater scripts the broker-facing connection-state surface.
type fakeStater struct {
	mu    sync.Mutex
	state ConnectionStatus
	err   *Error
	gotWS string
	calls int
}

func (f *fakeStater) ConnectionState(_ context.Context, wsUID string) (ConnectionStatus, *Error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.gotWS = wsUID
	return f.state, f.err
}

// fakeWorkspaceGet scripts the ownership check the endpoint shares with
// GET /v1/workspaces/{id}.
type fakeWorkspaceGet struct {
	rec      provisioning.WorkspaceRecord
	err      error
	gotScope string
	gotID    string
}

func (f *fakeWorkspaceGet) GetWorkspace(_ context.Context, _, ownerScope, id string) (provisioning.WorkspaceRecord, error) {
	f.gotScope, f.gotID = ownerScope, id
	return f.rec, f.err
}

func newConnStatusEnv(t *testing.T, stater ConnectionStater, ws workspaceGetter) *testEnv {
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
	}, sessions, logger)
	if err != nil {
		t.Fatalf("NewAuthenticator: %v", err)
	}
	mux := http.NewServeMux()
	mux.Handle("/auth/login", http.HandlerFunc(a.LoginHandler))
	mux.Handle("/auth/callback", http.HandlerFunc(a.CallbackHandler))
	MountMeRoutes(mux, a, NewMeHandler(testSessionDomain))
	MountConnectionStatusRoutes(mux, a, NewConnectionStatusHandler(stater, ws, defaultTenants()))
	srv := httptest.NewServer(RequestID(Audit(logger)(mux)))
	env := &testEnv{issuer: iss, auth: a, store: sessions, server: srv, logs: logBuf}
	t.Cleanup(func() { srv.Close(); iss.Close() })
	return env
}

// TestConnectionStatus_Handler: the handler passes the broker state through
// under the documented response shape.
func TestConnectionStatus_Handler(t *testing.T) {
	renewed := time.Now().Add(-5 * time.Second).UTC().Truncate(time.Second)
	st := &fakeStater{state: ConnectionStatus{
		State: "connected", LeaseActive: true, LastRenewedAt: &renewed,
	}}
	ws := &fakeWorkspaceGet{rec: provisioning.WorkspaceRecord{ID: "ws_00000000000000000000000001"}}
	env := newConnStatusEnv(t, st, ws)
	sess, _ := login(t, env, "alice")

	r := env.authedGet(t, sess, "/v1/workspaces/ws_00000000000000000000000001/connection")
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200", r.StatusCode)
	}
	var view struct {
		State         string     `json:"state"`
		LeaseActive   bool       `json:"leaseActive"`
		LastRenewedAt *time.Time `json:"lastRenewedAt"`
	}
	if err := json.NewDecoder(r.Body).Decode(&view); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if view.State != "connected" || !view.LeaseActive || view.LastRenewedAt == nil {
		t.Fatalf("view = %+v", view)
	}
	if st.gotWS != "ws_00000000000000000000000001" {
		t.Fatalf("stater got workspace %q", st.gotWS)
	}
	if ws.gotID != "ws_00000000000000000000000001" {
		t.Fatalf("ownership check skipped workspace %q", ws.gotID)
	}
}

// TestConnection_NotOwner: the ownership rule of GET /v1/workspaces/{id} —
// another user's workspace is indistinguishable from absent (404).
func TestConnection_NotOwner(t *testing.T) {
	st := &fakeStater{state: ConnectionStatus{State: "none"}}
	ws := &fakeWorkspaceGet{err: provisioning.ErrWorkspaceNotFound}
	env := newConnStatusEnv(t, st, ws)
	sess, _ := login(t, env, "alice")

	r := env.authedGet(t, sess, "/v1/workspaces/ws_00000000000000000000000009/connection")
	defer r.Body.Close()
	if r.StatusCode != http.StatusNotFound {
		t.Fatalf("status=%d, want 404", r.StatusCode)
	}
	if st.calls != 0 {
		t.Fatal("stater must not run for a workspace the caller cannot see")
	}
}

// TestConnectionEndpoint_DoesNotSlideIdle: polling the passive endpoint for a
// full idle window never extends the session — the next RequireAuth request
// is 401 (P4, D18).
func TestConnectionEndpoint_DoesNotSlideIdle(t *testing.T) {
	st := &fakeStater{state: ConnectionStatus{State: "connected", LeaseActive: true}}
	ws := &fakeWorkspaceGet{rec: provisioning.WorkspaceRecord{ID: "ws_00000000000000000000000001"}}
	env := newConnStatusEnv(t, st, ws)
	fc := &fakeClock{now: time.Now()}
	env.store.WithClock(fc.Now)
	env.auth.now = fc.Now
	sess, _ := login(t, env, "alice")

	const path = "/v1/workspaces/ws_00000000000000000000000001/connection"
	for min := 1; min <= 31; min++ {
		fc.Advance(time.Minute)
		r := env.authedGet(t, sess, path)
		r.Body.Close()
		if min < 30 && r.StatusCode != http.StatusOK {
			t.Fatalf("poll at minute %d: status=%d, want 200 while the session lives", min, r.StatusCode)
		}
		if min >= 30 && r.StatusCode != http.StatusUnauthorized {
			t.Fatalf("poll at minute %d: status=%d, want 401 past idle expiry", min, r.StatusCode)
		}
	}
	if st.calls == 0 {
		t.Fatal("endpoint never reached the stater")
	}

	// The passive polls must not have kept the session alive.
	r := env.authedGet(t, sess, "/v1/me")
	defer r.Body.Close()
	if r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("RequireAuth after 31 min of passive polling: %d, want 401", r.StatusCode)
	}
}
