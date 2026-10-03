package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/api/oidctest"
	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
)

// fakeWorkspaceBackend implements the handler-facing provisioning contract
// without Postgres: records are keyed by ID, scoped by tenant + owner.
type fakeWorkspaceBackend struct {
	mu        sync.Mutex
	recs      map[string]provisioning.WorkspaceRecord
	seq       int
	createErr error
	signalErr error
	listErr   error
	attachErr error
	gotCreate []provisioning.CreateRequest
	gotAttach []fakeAttachCall
}

// fakeAttachCall records one AttachRetained invocation so tests can pin
// the SEC-01 routing contract (caller, owner scope, claimed record).
type fakeAttachCall struct {
	caller     string
	ownerScope string
	dataID     string
	idemKey    string
	req        provisioning.AttachRequest
}

func newFakeBackend() *fakeWorkspaceBackend {
	return &fakeWorkspaceBackend{recs: map[string]provisioning.WorkspaceRecord{}}
}

func (f *fakeWorkspaceBackend) CreateWorkspace(_ context.Context, tenantID, key string, req provisioning.CreateRequest, _ []byte) (provisioning.WorkspaceRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gotCreate = append(f.gotCreate, req)
	if f.createErr != nil {
		return provisioning.WorkspaceRecord{}, f.createErr
	}
	f.seq++
	rec := provisioning.WorkspaceRecord{
		ID:           fmt.Sprintf("ws_%026d", f.seq),
		TenantID:     tenantID,
		Owner:        req.OwnerIssuer + "|" + req.OwnerSubject,
		OwnerIssuer:  req.OwnerIssuer,
		OwnerSub:     req.OwnerSubject,
		Name:         req.Name,
		Template:     req.Template,
		Vector:       req.Vector,
		DesiredState: req.DesiredState,
		DataPolicy:   req.DataPolicy,
		Phase:        "Pending",
		RequestID:    provisioning.DeterministicRequestID(tenantID, key),
		Revision:     1,
		CreatedAt:    time.Now().UTC(),
		UpdatedAt:    time.Now().UTC(),
	}
	f.recs[rec.ID] = rec
	return rec, nil
}

// AttachRetained emulates the claimed attach path: the record's owner is
// the caller for a plain user scope; the workspace is created with
// dataPolicy Retain and the ref set.
func (f *fakeWorkspaceBackend) AttachRetained(_ context.Context, tenantID, caller, ownerScope, dataID, idemKey string, req provisioning.AttachRequest, _ []byte) (provisioning.WorkspaceRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gotAttach = append(f.gotAttach, fakeAttachCall{caller, ownerScope, dataID, idemKey, req})
	if f.attachErr != nil {
		return provisioning.WorkspaceRecord{}, f.attachErr
	}
	f.seq++
	iss, sub, _ := strings.Cut(caller, "|")
	rec := provisioning.WorkspaceRecord{
		ID:              fmt.Sprintf("ws_%026d", f.seq),
		TenantID:        tenantID,
		Owner:           caller,
		OwnerIssuer:     iss,
		OwnerSub:        sub,
		Name:            req.Name,
		Template:        req.Template,
		Vector:          req.Vector,
		DesiredState:    req.DesiredState,
		DataPolicy:      "Retain",
		Phase:           "Pending",
		RetainedDataRef: dataID,
		Revision:        1,
		CreatedAt:       time.Now().UTC(),
		UpdatedAt:       time.Now().UTC(),
	}
	f.recs[rec.ID] = rec
	return rec, nil
}

func (f *fakeWorkspaceBackend) GetWorkspace(_ context.Context, tenantID, ownerScope, id string) (provisioning.WorkspaceRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rec, ok := f.recs[id]
	if !ok || rec.TenantID != tenantID || (ownerScope != "" && rec.Owner != ownerScope) {
		return provisioning.WorkspaceRecord{}, provisioning.ErrWorkspaceNotFound
	}
	return rec, nil
}

func (f *fakeWorkspaceBackend) ListWorkspaces(_ context.Context, tenantID, ownerScope, phase, _ string, _ int) ([]provisioning.WorkspaceRecord, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, "", f.listErr
	}
	var out []provisioning.WorkspaceRecord
	for _, r := range f.recs {
		if r.TenantID != tenantID {
			continue
		}
		if ownerScope != "" && r.Owner != ownerScope {
			continue
		}
		if phase != "" && r.Phase != phase {
			continue
		}
		out = append(out, r)
	}
	return out, "", nil
}

func (f *fakeWorkspaceBackend) SignalWorkspace(_ context.Context, tenantID, _, ownerScope, id, _ string, kind provisioning.IntentKind, _ []byte) (provisioning.WorkspaceRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.signalErr != nil {
		return provisioning.WorkspaceRecord{}, f.signalErr
	}
	rec, ok := f.recs[id]
	if !ok || rec.TenantID != tenantID || (ownerScope != "" && rec.Owner != ownerScope) {
		return provisioning.WorkspaceRecord{}, provisioning.ErrWorkspaceNotFound
	}
	switch kind {
	case provisioning.IntentStart:
		rec.DesiredState = "Running"
		rec.Phase = "Provisioning"
	case provisioning.IntentStop:
		rec.DesiredState = "Stopped"
		rec.Phase = "Stopping"
	case provisioning.IntentDelete:
		rec.Phase = "Terminating"
	}
	f.recs[id] = rec
	return rec, nil
}

// fakeCatalog resolves tpl_x template IDs per tenant.
type fakeCatalog struct {
	entries map[string][]provisioning.TemplateInfo
}

func (f *fakeCatalog) Resolve(_ context.Context, tenantID, id string) (TemplateEntry, error) {
	for _, e := range f.entries[tenantID] {
		if "tpl_"+e.Name == id {
			return TemplateEntry{
				ID: id, Name: e.Name, Revision: e.Revision,
				Runtime: e.Runtime, Experience: e.Experience,
				CPUMillis: 2000, MemoryMiB: 4096, StorageGiB: 20,
				IdleTimeoutSeconds: 1800, DisconnectGraceSeconds: 600, MaxRunningSeconds: 28800,
				DataPolicyDefault: "Ephemeral", ClipboardPolicy: "Disabled",
				PublishedAt: time.Now().UTC(),
			}, nil
		}
	}
	return TemplateEntry{}, ErrTemplateNotFound
}

func (f *fakeCatalog) List(_ context.Context, tenantID, _ string, _ string, _ int) ([]TemplateEntry, string, error) {
	var out []TemplateEntry
	for _, e := range f.entries[tenantID] {
		e2, _ := f.Resolve(context.Background(), tenantID, "tpl_"+e.Name)
		out = append(out, e2)
	}
	return out, "", nil
}

func newWorkspaceEnv(t *testing.T, be workspaceBackend, cat TemplateCatalog, tenants TenantResolver, opts ...func(*WorkspaceHandler)) *testEnv {
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
	h := NewWorkspaceHandler(be, cat, tenants)
	for _, o := range opts {
		o(h)
	}
	th := NewTemplateHandler(cat, tenants)
	mux := http.NewServeMux()
	mux.Handle("/auth/login", http.HandlerFunc(a.LoginHandler))
	mux.Handle("/auth/callback", http.HandlerFunc(a.CallbackHandler))
	MountWorkspaceRoutes(mux, a, h, th)
	srv := httptest.NewServer(RequestID(Audit(logger)(mux)))
	env := &testEnv{issuer: iss, auth: a, store: sessions, server: srv, logs: logBuf}
	t.Cleanup(func() { srv.Close(); iss.Close() })
	return env
}

func defaultCatalog() *fakeCatalog {
	return &fakeCatalog{entries: map[string][]provisioning.TemplateInfo{
		"tenant-a": {{ID: "tpl_linuxdesktop", Name: "linuxdesktop", Revision: 1, Runtime: "LinuxContainer", Experience: "Desktop"}},
	}}
}

func defaultTenants() TenantResolver {
	return StaticTenantResolver{"tenant-a": "ns-a", "tenant-b": "ns-b"}
}

// doReq issues an authenticated request with session + CSRF cookies.
func doReq(t *testing.T, env *testEnv, sess, csrf *http.Cookie, method, path, body string, hdrs map[string]string) *http.Response {
	t.Helper()
	var rdr *strings.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	} else {
		rdr = strings.NewReader("")
	}
	req, _ := http.NewRequest(method, env.server.URL+path, rdr)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.AddCookie(sess)
	req.Header.Set(env.auth.CSRFHeader(), csrf.Value)
	for k, v := range hdrs {
		req.Header.Set(k, v)
	}
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

// login drives a full OIDC round trip for subject and returns the session
// cookie plus a cookie-shaped carrier for the session's CSRF token. Since
// v0.2 the token is derived from the session ID (P1) — portal JS reads it
// from GET /v1/me — so the helper derives the same value without a request.
func login(t *testing.T, env *testEnv, subject string) (*http.Cookie, *http.Cookie) {
	t.Helper()
	env.issuer.Subject = subject
	resp, cookies := env.login(t)
	resp.Body.Close()
	sess := findCookie(cookies, env.auth.SessionCookieName())
	if sess == nil {
		t.Fatal("login set no session cookie")
	}
	return sess, &http.Cookie{Name: env.auth.CSRFHeader(), Value: csrfTokenFor(sess.Value)}
}

func decodeBody[T any](t *testing.T, r *http.Response) T {
	t.Helper()
	defer r.Body.Close()
	var v T
	if err := json.NewDecoder(r.Body).Decode(&v); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return v
}

// TestCreateWorkspaceOwnerFromPrincipal: the owner is always the verified
// principal; an "owner" field in the body is rejected as an unknown field.
func TestCreateWorkspaceOwnerFromPrincipal(t *testing.T) {
	be := newFakeBackend()
	env := newWorkspaceEnv(t, be, defaultCatalog(), defaultTenants())
	sess, csrf := login(t, env, "user-a")

	r := doReq(t, env, sess, csrf, http.MethodPost, "/v1/workspaces",
		`{"name":"research-desktop","templateRef":"tpl_linuxdesktop","desiredState":"Running","owner":"mallory"}`,
		map[string]string{"Idempotency-Key": "key-owner-1000"})
	// OpenAPI additionalProperties:false — a body-level owner is rejected.
	if r.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400 (owner field must be rejected)", r.StatusCode)
	}
	r.Body.Close()

	r = doReq(t, env, sess, csrf, http.MethodPost, "/v1/workspaces",
		`{"name":"research-desktop","templateRef":"tpl_linuxdesktop","desiredState":"Running"}`,
		map[string]string{"Idempotency-Key": "key-owner-2000"})
	view := decodeBody[WorkspaceView](t, r)
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("status=%d, want 201", r.StatusCode)
	}
	if len(be.gotCreate) != 1 {
		t.Fatalf("backend got %d creates", len(be.gotCreate))
	}
	got := be.gotCreate[0]
	if got.OwnerSubject != "user-a" || got.OwnerIssuer != env.issuer.URL() {
		t.Fatalf("owner not derived from principal: %+v", got)
	}
	if got.Name != "research-desktop" || got.Template.Name != "linuxdesktop" {
		t.Fatalf("bad resolved request: %+v", got)
	}
	if view.Name != "research-desktop" || !strings.HasPrefix(view.ID, "ws_") {
		t.Fatalf("bad view: %+v", view)
	}
}

// TestWorkspaceOwnership: user B cannot read or act on user A's workspace.
func TestWorkspaceOwnership(t *testing.T) {
	be := newFakeBackend()
	env := newWorkspaceEnv(t, be, defaultCatalog(), defaultTenants())
	sessA, csrfA := login(t, env, "user-a")

	r := doReq(t, env, sessA, csrfA, http.MethodPost, "/v1/workspaces",
		`{"name":"a-desktop","templateRef":"tpl_linuxdesktop"}`,
		map[string]string{"Idempotency-Key": "key-a-10000"})
	created := decodeBody[WorkspaceView](t, r)
	r.Body.Close()
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("create status=%d", r.StatusCode)
	}

	sessB, csrfB := login(t, env, "user-b")
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/v1/workspaces/" + created.ID},
		{http.MethodPost, "/v1/workspaces/" + created.ID + "/start"},
		{http.MethodPost, "/v1/workspaces/" + created.ID + "/stop"},
		{http.MethodDelete, "/v1/workspaces/" + created.ID},
	} {
		r := doReq(t, env, sessB, csrfB, tc.method, tc.path, "",
			map[string]string{"Idempotency-Key": "key-b-10000"})
		if r.StatusCode != http.StatusNotFound {
			t.Fatalf("%s %s: status=%d, want 404 (foreign workspace must be invisible)", tc.method, tc.path, r.StatusCode)
		}
		r.Body.Close()
	}
	// B's list must not contain A's workspace.
	r = doReq(t, env, sessB, csrfB, http.MethodGet, "/v1/workspaces", "", nil)
	list := decodeBody[WorkspaceList](t, r)
	r.Body.Close()
	if len(list.Items) != 0 {
		t.Fatalf("B sees %d workspaces, want 0", len(list.Items))
	}
	// A sees exactly their own.
	r = doReq(t, env, sessA, csrfA, http.MethodGet, "/v1/workspaces", "", nil)
	list = decodeBody[WorkspaceList](t, r)
	r.Body.Close()
	if len(list.Items) != 1 || list.Items[0].ID != created.ID {
		t.Fatalf("A list = %+v", list.Items)
	}
}

func TestCreateWorkspaceErrorMapping(t *testing.T) {
	be := newFakeBackend()
	env := newWorkspaceEnv(t, be, defaultCatalog(), defaultTenants())
	sess, csrf := login(t, env, "user-a")
	body := `{"name":"x","templateRef":"tpl_linuxdesktop"}`

	// missing Idempotency-Key -> 400
	r := doReq(t, env, sess, csrf, http.MethodPost, "/v1/workspaces", body, nil)
	if r.StatusCode != http.StatusBadRequest || decodeError(t, r) != string(CodeInvalidRequest) {
		t.Fatalf("missing key: status=%d", r.StatusCode)
	}
	r.Body.Close()

	// quota exhaustion -> 409 QUOTA_EXHAUSTED
	be.createErr = &provisioning.QuotaExceededError{TenantID: "tenant-a", Dimension: "runningSlots", Limit: 10, Used: 10, Requested: 1}
	r = doReq(t, env, sess, csrf, http.MethodPost, "/v1/workspaces", body,
		map[string]string{"Idempotency-Key": "key-quota-00"})
	if r.StatusCode != http.StatusConflict || decodeError(t, r) != string(CodeQuotaExhausted) {
		t.Fatalf("quota: status=%d", r.StatusCode)
	}
	r.Body.Close()

	// idempotency conflict -> 409 IDEMPOTENCY_CONFLICT
	be.createErr = provisioning.ErrIdempotencyConflict
	r = doReq(t, env, sess, csrf, http.MethodPost, "/v1/workspaces", body,
		map[string]string{"Idempotency-Key": "key-idem-000"})
	if r.StatusCode != http.StatusConflict || decodeError(t, r) != string(CodeIdempotencyConflict) {
		t.Fatalf("idem: status=%d", r.StatusCode)
	}
	r.Body.Close()
	be.createErr = nil

	// unknown template -> 422 INVALID_TEMPLATE
	r = doReq(t, env, sess, csrf, http.MethodPost, "/v1/workspaces",
		`{"name":"x","templateRef":"tpl_unknowntpl"}`,
		map[string]string{"Idempotency-Key": "key-tpl-0000"})
	if r.StatusCode != http.StatusUnprocessableEntity || decodeError(t, r) != string(CodeInvalidTemplate) {
		t.Fatalf("template: status=%d", r.StatusCode)
	}
	r.Body.Close()

	// malformed body -> 400
	r = doReq(t, env, sess, csrf, http.MethodPost, "/v1/workspaces", `{"name":`,
		map[string]string{"Idempotency-Key": "key-bad-0000"})
	if r.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad json: status=%d", r.StatusCode)
	}
	r.Body.Close()
}

// TestCreateQuotaNotConfigured: a tenant with no quota row (admission fails
// closed with ErrNoQuota) is 409 QUOTA_NOT_CONFIGURED with the actionable
// message — distinct from QUOTA_EXHAUSTED, which stays for a real over-limit
// (FX-R17). The start path shares the mapping.
func TestCreateQuotaNotConfigured(t *testing.T) {
	be := newFakeBackend()
	env := newWorkspaceEnv(t, be, defaultCatalog(), defaultTenants())
	sess, csrf := login(t, env, "user-a")
	body := `{"name":"x","templateRef":"tpl_linuxdesktop"}`

	be.createErr = fmt.Errorf("reserve: %w", provisioning.ErrNoQuota)
	r := doReq(t, env, sess, csrf, http.MethodPost, "/v1/workspaces", body,
		map[string]string{"Idempotency-Key": "key-noquota-0"})
	if r.StatusCode != http.StatusConflict {
		t.Fatalf("status=%d, want 409", r.StatusCode)
	}
	e := errBody(t, r)
	if e.Code != CodeQuotaNotConfigured {
		t.Fatalf("code=%s, want QUOTA_NOT_CONFIGURED", e.Code)
	}
	if want := "No quota is configured for your tenant. Ask an administrator to set one."; e.Message != want {
		t.Fatalf("message=%q, want %q", e.Message, want)
	}
	if e.Retryable {
		t.Fatal("QUOTA_NOT_CONFIGURED must not be retryable")
	}

	// A real over-limit stays QUOTA_EXHAUSTED.
	be.createErr = &provisioning.QuotaExceededError{TenantID: "tenant-a", Dimension: "runningSlots", Limit: 1, Used: 1, Requested: 1}
	r = doReq(t, env, sess, csrf, http.MethodPost, "/v1/workspaces", body,
		map[string]string{"Idempotency-Key": "key-noquota-1"})
	if r.StatusCode != http.StatusConflict || decodeError(t, r) != string(CodeQuotaExhausted) {
		t.Fatalf("over limit: status=%d, want 409 QUOTA_EXHAUSTED", r.StatusCode)
	}
	r.Body.Close()
}

// TestStartQuotaExhaustedMapping covers defect F7: a start that cannot
// re-acquire its running-quota reservation surfaces as
// 409 QUOTA_EXHAUSTED, not a generic error.
func TestStartQuotaExhaustedMapping(t *testing.T) {
	be := newFakeBackend()
	env := newWorkspaceEnv(t, be, defaultCatalog(), defaultTenants())
	sess, csrf := login(t, env, "user-a")

	r := doReq(t, env, sess, csrf, http.MethodPost, "/v1/workspaces",
		`{"name":"a-desktop","templateRef":"tpl_linuxdesktop"}`,
		map[string]string{"Idempotency-Key": "key-q-create"})
	created := decodeBody[WorkspaceView](t, r)
	r.Body.Close()
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("create status=%d", r.StatusCode)
	}

	be.signalErr = &provisioning.QuotaExceededError{
		TenantID: "tenant-a", Dimension: "runningSlots", Limit: 1, Used: 1, Requested: 1,
	}
	r = doReq(t, env, sess, csrf, http.MethodPost, "/v1/workspaces/"+created.ID+"/start", "",
		map[string]string{"Idempotency-Key": "key-q-start-0"})
	if r.StatusCode != http.StatusConflict || decodeError(t, r) != string(CodeQuotaExhausted) {
		t.Fatalf("start over quota: status=%d, want 409 QUOTA_EXHAUSTED", r.StatusCode)
	}
	r.Body.Close()
}

// TestQuotaReleasePending: a quota refusal whose shortfall is only
// teardown-held quota keeps the QUOTA_EXHAUSTED code for compatibility but
// is retryable with details.reason=release_pending and a Retry-After
// header — a real exhaustion stays plain and non-retryable.
func TestQuotaReleasePending(t *testing.T) {
	be := newFakeBackend()
	env := newWorkspaceEnv(t, be, defaultCatalog(), defaultTenants(), func(h *WorkspaceHandler) {
		h.WithReleaseRetryAfter(func() int { return 7 })
	})
	sess, csrf := login(t, env, "user-a")
	body := `{"name":"x","templateRef":"tpl_linuxdesktop"}`

	// Release-pending create: 409 QUOTA_EXHAUSTED + retryable + reason +
	// Retry-After from the wired estimate.
	be.createErr = &provisioning.QuotaExceededError{
		TenantID: "tenant-a", Dimension: "runningSlots", Limit: 2, Used: 2, Requested: 1,
		ReleasePending: true,
	}
	r := doReq(t, env, sess, csrf, http.MethodPost, "/v1/workspaces", body,
		map[string]string{"Idempotency-Key": "key-rel-0001"})
	if r.StatusCode != http.StatusConflict {
		t.Fatalf("release_pending create: status=%d, want 409", r.StatusCode)
	}
	if got := r.Header.Get("Retry-After"); got != "7" {
		t.Fatalf("Retry-After=%q, want 7", got)
	}
	e := errBody(t, r)
	if e.Code != CodeQuotaExhausted {
		t.Fatalf("code=%s, want QUOTA_EXHAUSTED", e.Code)
	}
	if !e.Retryable {
		t.Fatal("release_pending must be retryable")
	}
	if e.Details == nil || e.Details.Reason != ReasonReleasePending {
		t.Fatalf("details=%+v, want reason release_pending", e.Details)
	}
	be.createErr = nil

	// The same signal on the start path.
	r = doReq(t, env, sess, csrf, http.MethodPost, "/v1/workspaces",
		`{"name":"started","templateRef":"tpl_linuxdesktop"}`,
		map[string]string{"Idempotency-Key": "key-rel-0002"})
	created := decodeBody[WorkspaceView](t, r)
	r.Body.Close()
	be.signalErr = &provisioning.QuotaExceededError{
		TenantID: "tenant-a", Dimension: "runningSlots", Limit: 2, Used: 2, Requested: 1,
		ReleasePending: true,
	}
	r = doReq(t, env, sess, csrf, http.MethodPost, "/v1/workspaces/"+created.ID+"/start", "",
		map[string]string{"Idempotency-Key": "key-rel-0003"})
	if r.StatusCode != http.StatusConflict || r.Header.Get("Retry-After") != "7" {
		t.Fatalf("release_pending start: status=%d Retry-After=%q", r.StatusCode, r.Header.Get("Retry-After"))
	}
	e = errBody(t, r)
	if e.Code != CodeQuotaExhausted || !e.Retryable || e.Details == nil ||
		e.Details.Reason != ReasonReleasePending {
		t.Fatalf("start body=%+v, want retryable QUOTA_EXHAUSTED release_pending", e)
	}
	be.signalErr = nil

	// A genuine over-limit stays the plain non-retryable refusal.
	be.createErr = &provisioning.QuotaExceededError{
		TenantID: "tenant-a", Dimension: "runningSlots", Limit: 2, Used: 2, Requested: 1,
	}
	r = doReq(t, env, sess, csrf, http.MethodPost, "/v1/workspaces", body,
		map[string]string{"Idempotency-Key": "key-rel-0004"})
	if r.StatusCode != http.StatusConflict {
		t.Fatalf("pure exhaustion: status=%d, want 409", r.StatusCode)
	}
	if got := r.Header.Get("Retry-After"); got != "" {
		t.Fatalf("pure exhaustion must not set Retry-After, got %q", got)
	}
	e = errBody(t, r)
	if e.Code != CodeQuotaExhausted || e.Retryable || e.Details != nil {
		t.Fatalf("pure exhaustion body=%+v, want non-retryable QUOTA_EXHAUSTED without details", e)
	}
}

func TestUnknownTenantForbidden(t *testing.T) {
	be := newFakeBackend()
	env := newWorkspaceEnv(t, be, defaultCatalog(), defaultTenants())
	env.issuer.TenantID = "tenant-unknown"
	sess, csrf := login(t, env, "user-a")
	env.issuer.TenantID = "tenant-a"

	r := doReq(t, env, sess, csrf, http.MethodPost, "/v1/workspaces",
		`{"name":"x","templateRef":"tpl_linuxdesktop"}`,
		map[string]string{"Idempotency-Key": "key-t-000000"})
	if r.StatusCode != http.StatusForbidden || decodeError(t, r) != string(CodeForbidden) {
		t.Fatalf("unknown tenant create: status=%d", r.StatusCode)
	}
	r.Body.Close()
	r = doReq(t, env, sess, csrf, http.MethodGet, "/v1/workspaces", "", nil)
	if r.StatusCode != http.StatusForbidden {
		t.Fatalf("unknown tenant list: status=%d", r.StatusCode)
	}
	r.Body.Close()
}

// TestWorkspaceHandlerNoSecretsInLogs: session IDs, CSRF tokens and ID
// tokens must never appear in the audit/log stream (same rule as the
// middleware redaction test).
func TestWorkspaceHandlerNoSecretsInLogs(t *testing.T) {
	be := newFakeBackend()
	env := newWorkspaceEnv(t, be, defaultCatalog(), defaultTenants())
	sess, csrf := login(t, env, "user-a")

	for _, r := range []*http.Response{
		doReq(t, env, sess, csrf, http.MethodPost, "/v1/workspaces",
			`{"name":"x","templateRef":"tpl_linuxdesktop"}`, map[string]string{"Idempotency-Key": "key-s-000000"}),
		doReq(t, env, sess, csrf, http.MethodGet, "/v1/workspaces", "", nil),
	} {
		r.Body.Close()
	}
	logs := env.logs.String()
	for _, leak := range []string{sess.Value, csrf.Value, env.issuer.LastIDToken()} {
		if leak != "" && strings.Contains(logs, leak) {
			t.Fatalf("log leak of sensitive value")
		}
	}
}

// ---------------------------------------------------------------------------
// SEC-01 / SEC-I7 regression tests
// ---------------------------------------------------------------------------

// TestCreateRetainedDataRef_RoutesThroughAttach: a create carrying
// retainedDataRef goes through the claimed attach path — the same
// owner-scoped, state-checked transaction as POST /v1/data/{id}/attach —
// never the plain CreateWorkspace. The caller is the verified principal
// and the owner scope follows admin membership.
func TestCreateRetainedDataRef_RoutesThroughAttach(t *testing.T) {
	be := newFakeBackend()
	env := newWorkspaceEnv(t, be, defaultCatalog(), defaultTenants())

	// Regular user: owner scope is their own iss|sub.
	sess, csrf := login(t, env, "user-a")
	r := doReq(t, env, sess, csrf, http.MethodPost, "/v1/workspaces",
		`{"name":"restored","templateRef":"tpl_linuxdesktop","desiredState":"Running","retainedDataRef":"rd_aaaaaaaa01"}`,
		map[string]string{"Idempotency-Key": "key-ret-0001"})
	view := decodeBody[WorkspaceView](t, r)
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("create-with-ref: status=%d", r.StatusCode)
	}
	if len(be.gotCreate) != 0 {
		t.Fatal("retainedDataRef create reached plain CreateWorkspace")
	}
	if len(be.gotAttach) != 1 {
		t.Fatalf("AttachRetained calls = %d, want 1", len(be.gotAttach))
	}
	call := be.gotAttach[0]
	wantOwner := env.issuer.URL() + "|user-a"
	if call.dataID != "rd_aaaaaaaa01" || call.caller != wantOwner || call.ownerScope != wantOwner {
		t.Fatalf("attach call = %+v, want dataID rd_aaaaaaaa01 caller/scope %s", call, wantOwner)
	}
	if call.idemKey != "key-ret-0001" {
		t.Fatalf("idempotency key not forwarded: %q", call.idemKey)
	}
	if call.req.Name != "restored" || call.req.Template.Runtime != "LinuxContainer" ||
		call.req.DesiredState != "Running" {
		t.Fatalf("attach request = %+v", call.req)
	}
	if view.RetainedDataRef != "rd_aaaaaaaa01" || view.DataPolicy != "Retain" {
		t.Fatalf("view = %+v, want Retain + ref", view)
	}

	// Tenant admin: caller is the admin principal, scope is tenant-wide.
	env.issuer.Groups = []string{TenantAdminGroup}
	sessAdm, csrfAdm := login(t, env, "admin-1")
	r = doReq(t, env, sessAdm, csrfAdm, http.MethodPost, "/v1/workspaces",
		`{"name":"admin-restore","templateRef":"tpl_linuxdesktop","retainedDataRef":"rd_bbbbbbbb02"}`,
		map[string]string{"Idempotency-Key": "key-ret-0002"})
	r.Body.Close()
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("admin create-with-ref: status=%d", r.StatusCode)
	}
	call = be.gotAttach[1]
	if call.caller != env.issuer.URL()+"|admin-1" || call.ownerScope != "" {
		t.Fatalf("admin attach call = %+v, want caller=admin ownerScope=\"\"", call)
	}
}

// TestCreateRetainedDataRef_ErrorMapping: the attach path's domain errors
// map to the contract codes — a foreign record is 404 (existence never
// leaks), a non-Retained record is 409, a runtime mismatch is 422.
func TestCreateRetainedDataRef_ErrorMapping(t *testing.T) {
	be := newFakeBackend()
	env := newWorkspaceEnv(t, be, defaultCatalog(), defaultTenants())
	sess, csrf := login(t, env, "user-a")
	body := `{"name":"x","templateRef":"tpl_linuxdesktop","retainedDataRef":"rd_aaaaaaaa01"}`

	for i, tc := range []struct {
		err    error
		status int
		code   ErrorCode
	}{
		{provisioning.ErrRetainedNotFound, http.StatusNotFound, CodeNotFound},
		{provisioning.ErrRetainedState, http.StatusConflict, CodeInvalidState},
		{provisioning.ErrRuntimeMismatch, http.StatusUnprocessableEntity, CodeInvalidTemplate},
		{provisioning.ErrIdempotencyConflict, http.StatusConflict, CodeIdempotencyConflict},
		{&provisioning.QuotaExceededError{TenantID: "tenant-a", Dimension: "runningSlots"}, http.StatusConflict, CodeQuotaExhausted},
		{provisioning.ErrNoQuota, http.StatusConflict, CodeQuotaNotConfigured},
	} {
		be.attachErr = tc.err
		r := doReq(t, env, sess, csrf, http.MethodPost, "/v1/workspaces", body,
			map[string]string{"Idempotency-Key": fmt.Sprintf("key-errmap-%04d", i)})
		if r.StatusCode != tc.status {
			t.Fatalf("%v: status=%d, want %d", tc.err, r.StatusCode, tc.status)
		}
		if e := errBody(t, r); e.Code != tc.code {
			t.Fatalf("%v: code=%s, want %s", tc.err, e.Code, tc.code)
		}
	}
	be.attachErr = nil
}

// TestCreateRetainedDataRef_EphemeralRejected: dataPolicy Ephemeral
// contradicts a retainedDataRef — an attached disk is Retain by
// definition.
func TestCreateRetainedDataRef_EphemeralRejected(t *testing.T) {
	be := newFakeBackend()
	env := newWorkspaceEnv(t, be, defaultCatalog(), defaultTenants())
	sess, csrf := login(t, env, "user-a")
	r := doReq(t, env, sess, csrf, http.MethodPost, "/v1/workspaces",
		`{"name":"x","templateRef":"tpl_linuxdesktop","dataPolicy":"Ephemeral","retainedDataRef":"rd_aaaaaaaa01"}`,
		map[string]string{"Idempotency-Key": "key-ret-eph0"})
	if r.StatusCode != http.StatusBadRequest {
		t.Fatalf("ephemeral+ref: status=%d, want 400", r.StatusCode)
	}
	r.Body.Close()
	if len(be.gotCreate)+len(be.gotAttach) != 0 {
		t.Fatal("rejected request reached the backend")
	}
}

// TestCreateDecoderErrorNotEchoed: decoder internals are never reflected
// in the client-facing message (SEC-I7).
func TestCreateDecoderErrorNotEchoed(t *testing.T) {
	be := newFakeBackend()
	env := newWorkspaceEnv(t, be, defaultCatalog(), defaultTenants())
	sess, csrf := login(t, env, "user-a")

	for _, body := range []string{
		`{"name":`, // syntax error
		`{"name":"x","templateRef":"tpl_linuxdesktop"} {}`, // trailing document
		`{"name":"x","templateRef":"tpl_linuxdesktop"}x`,   // trailing garbage
		`{"name":"x","templateRef":"tpl_linuxdesktop",}`,   // trailing comma
	} {
		r := doReq(t, env, sess, csrf, http.MethodPost, "/v1/workspaces", body,
			map[string]string{"Idempotency-Key": "key-decode-0001"})
		if r.StatusCode != http.StatusBadRequest {
			t.Fatalf("body %q: status=%d, want 400", body, r.StatusCode)
		}
		if e := errBody(t, r); e.Code != CodeInvalidRequest || e.Message != "invalid request body" {
			t.Fatalf("body %q: error=%+v, want generic INVALID_REQUEST", body, e)
		}
	}
}

// TestSignalBodyValidated: signal endpoints take no request body — junk or
// multi-document bodies are rejected, a single JSON value is tolerated.
func TestSignalBodyValidated(t *testing.T) {
	be := newFakeBackend()
	env := newWorkspaceEnv(t, be, defaultCatalog(), defaultTenants())
	sess, csrf := login(t, env, "user-a")

	r := doReq(t, env, sess, csrf, http.MethodPost, "/v1/workspaces",
		`{"name":"a-desktop","templateRef":"tpl_linuxdesktop"}`,
		map[string]string{"Idempotency-Key": "key-sig-00001"})
	created := decodeBody[WorkspaceView](t, r)
	r.Body.Close()
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("create status=%d", r.StatusCode)
	}

	for _, body := range []string{"not json", `{"a":1} {"b":2}`} {
		r := doReq(t, env, sess, csrf, http.MethodPost, "/v1/workspaces/"+created.ID+"/start", body,
			map[string]string{"Idempotency-Key": "key-sig-00002"})
		if r.StatusCode != http.StatusBadRequest {
			t.Fatalf("start body %q: status=%d, want 400", body, r.StatusCode)
		}
		r.Body.Close()
	}
	r = doReq(t, env, sess, csrf, http.MethodPost, "/v1/workspaces/"+created.ID+"/start", `{}`,
		map[string]string{"Idempotency-Key": "key-sig-00003"})
	if r.StatusCode != http.StatusOK {
		t.Fatalf("start with {} body: status=%d, want 200", r.StatusCode)
	}
	r.Body.Close()
}

// TestListWorkspaces_BadPageToken: a malformed pageToken is a client error
// (400 INVALID_REQUEST), not an internal one (SEC-I7).
func TestListWorkspaces_BadPageToken(t *testing.T) {
	be := newFakeBackend()
	be.listErr = fmt.Errorf("decode: %w", provisioning.ErrBadCursor)
	env := newWorkspaceEnv(t, be, defaultCatalog(), defaultTenants())
	sess, csrf := login(t, env, "user-a")

	r := doReq(t, env, sess, csrf, http.MethodGet, "/v1/workspaces?pageToken=not-a-token", "", nil)
	if r.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad pageToken: status=%d, want 400", r.StatusCode)
	}
	if e := errBody(t, r); e.Code != CodeInvalidRequest {
		t.Fatalf("code=%s, want INVALID_REQUEST", e.Code)
	}
}
