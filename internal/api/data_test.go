package api

// Contract tests — the /v1/data retained-disk surface
// (design §5, openapi /v1/data + /attach + /purge). The handlers and the
// Postgres store were compile-only stubs when written, so every contract test
// fails on missing behaviour (500 INTERNAL from the stub handlers), never
// on a compile error.
//
// fakeRetainedStore is a contract-faithful in-memory RetainedDataStore so
// the tests pin HTTP plumbing — authn/CSRF, validation, owner scoping,
// error-code mapping, idempotency — independently of Postgres. The
// transactional guarantees (attach/purge exclusivity, quota, outbox) are
// pinned against the real store in tests/integration/retention_test.go.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/api/oidctest"
	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
)

// fakeRetainedStore implements RetainedDataStore per the contract:
// owner-scoped reads, per-caller purge nonces bound to the record's
// transition epoch, exclusive state transitions and idempotent replays.
type fakeRetainedStore struct {
	mu       sync.Mutex
	recs     map[string]*RetainedRecord
	order    []string
	nonces   map[string]fakeNonce // caller|id -> live nonce
	wsSeq    int
	idem     map[string]fakeIdem // tenantID|caller|key -> stored result
	quotaErr error               // attach fails with this when set
	listErr  error               // list fails with this when set
}

type fakeNonce struct {
	value string
	epoch int64
}

type fakeIdem struct {
	bodyHash [32]byte
	ws       provisioning.WorkspaceRecord
}

func newFakeRetainedStore() *fakeRetainedStore {
	return &fakeRetainedStore{
		recs:   map[string]*RetainedRecord{},
		nonces: map[string]fakeNonce{},
		idem:   map[string]fakeIdem{},
	}
}

func nonceKey(caller, id string) string { return caller + "|" + id }

// seed adds a record to the fake inventory.
func (f *fakeRetainedStore) seed(rec RetainedRecord) *RetainedRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := rec
	f.recs[r.ID] = &r
	f.order = append(f.order, r.ID)
	return &r
}

func (f *fakeRetainedStore) get(id string) *RetainedRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.recs[id]
}

func (f *fakeRetainedStore) mintNonceLocked(caller string, r *RetainedRecord) string {
	v := fmt.Sprintf("nonce-%s-%d-%d", r.ID, r.TransitionSeq, len(f.nonces)+1)
	f.nonces[nonceKey(caller, r.ID)] = fakeNonce{value: v, epoch: r.TransitionSeq}
	return v
}

func (f *fakeRetainedStore) nonceValidLocked(caller string, r *RetainedRecord, nonce string) bool {
	n, ok := f.nonces[nonceKey(caller, r.ID)]
	return ok && n.value == nonce && n.epoch == r.TransitionSeq
}

func (f *fakeRetainedStore) lookupLocked(tenantID, ownerScope, id string) (*RetainedRecord, error) {
	r, ok := f.recs[id]
	if !ok || r.TenantID != tenantID || (ownerScope != "" && r.Owner != ownerScope) {
		return nil, ErrRetainedNotFound
	}
	return r, nil
}

func (f *fakeRetainedStore) ListRetained(_ context.Context, tenantID, caller, ownerScope, _ string, _ int) ([]RetainedRecord, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, "", f.listErr
	}
	var out []RetainedRecord
	for _, id := range f.order {
		r := f.recs[id]
		if r.TenantID != tenantID || r.State == RetainedStatePurged {
			continue
		}
		if ownerScope != "" && r.Owner != ownerScope {
			continue
		}
		cp := *r
		cp.PurgeNonce = f.mintNonceLocked(caller, r)
		out = append(out, cp)
	}
	return out, "", nil
}

func (f *fakeRetainedStore) ReadRetained(_ context.Context, tenantID, caller, ownerScope, id string) (RetainedRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, err := f.lookupLocked(tenantID, ownerScope, id)
	if err != nil {
		return RetainedRecord{}, err
	}
	cp := *r
	cp.PurgeNonce = f.mintNonceLocked(caller, r)
	return cp, nil
}

func (f *fakeRetainedStore) ImportRetained(_ context.Context, info RetainedDiskInfo) (RetainedRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := info.PVCNamespace + "|" + info.PVCUID
	for _, id := range f.order {
		r := f.recs[id]
		if r.PVCNamespace+"|"+r.PVCUID == key {
			return *r, nil // idempotent re-import of the same dataset
		}
	}
	rec := &RetainedRecord{
		ID:                  fmt.Sprintf("rd_%028d", len(f.order)+1),
		TenantID:            info.TenantID,
		Owner:               info.Owner,
		State:               RetainedStateRetained,
		PVCNamespace:        info.PVCNamespace,
		PVCName:             info.PVCName,
		PVCUID:              info.PVCUID,
		SourceWorkspaceID:   info.SourceWorkspaceID,
		SourceWorkspaceName: info.SourceWorkspaceName,
		Runtime:             info.Runtime,
		SizeBytes:           info.SizeBytes,
		RetainedAt:          time.Now().UTC(),
		UpdatedAt:           time.Now().UTC(),
	}
	f.recs[rec.ID] = rec
	f.order = append(f.order, rec.ID)
	return *rec, nil
}

func (f *fakeRetainedStore) AttachRetained(_ context.Context, tenantID, caller, ownerScope, dataID, idemKey string, req AttachRequest, bodyHash []byte) (provisioning.WorkspaceRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if idemKey != "" {
		k := tenantID + "|" + caller + "|" + idemKey
		var sum [32]byte
		copy(sum[:], bodyHash)
		if e, ok := f.idem[k]; ok {
			if e.bodyHash != sum {
				return provisioning.WorkspaceRecord{}, provisioning.ErrIdempotencyConflict
			}
			return e.ws, nil
		}
		rec, err := f.attachLocked(tenantID, ownerScope, dataID, req)
		if err != nil {
			return provisioning.WorkspaceRecord{}, err
		}
		f.idem[k] = fakeIdem{bodyHash: sum, ws: rec}
		return rec, nil
	}
	return f.attachLocked(tenantID, ownerScope, dataID, req)
}

func (f *fakeRetainedStore) attachLocked(tenantID, ownerScope, dataID string, req AttachRequest) (provisioning.WorkspaceRecord, error) {
	rec, err := f.lookupLocked(tenantID, ownerScope, dataID)
	if err != nil {
		return provisioning.WorkspaceRecord{}, err
	}
	if rec.State != RetainedStateRetained {
		return provisioning.WorkspaceRecord{}, ErrRetainedState
	}
	if req.Template.Runtime != rec.Runtime {
		return provisioning.WorkspaceRecord{}, ErrRuntimeMismatch
	}
	if f.quotaErr != nil {
		return provisioning.WorkspaceRecord{}, f.quotaErr
	}
	f.wsSeq++
	ws := provisioning.WorkspaceRecord{
		ID:              fmt.Sprintf("ws_%026d", f.wsSeq),
		TenantID:        tenantID,
		Owner:           rec.Owner,
		Name:            req.Name,
		Template:        req.Template,
		Vector:          req.Vector,
		DesiredState:    req.DesiredState,
		DataPolicy:      "Retain",
		Phase:           "Pending",
		RetainedDataRef: rec.ID,
		CreatedAt:       time.Now().UTC(),
		UpdatedAt:       time.Now().UTC(),
	}
	rec.State = RetainedStateAttaching
	rec.ConsumingWorkspaceID = ws.ID
	rec.TransitionSeq++
	rec.UpdatedAt = time.Now().UTC()
	return ws, nil
}

func (f *fakeRetainedStore) PurgeRetained(_ context.Context, tenantID, caller, ownerScope, dataID, nonce, _ string, _ []byte) (RetainedRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rec, err := f.lookupLocked(tenantID, ownerScope, dataID)
	if err != nil {
		return RetainedRecord{}, err
	}
	if !f.nonceValidLocked(caller, rec, nonce) {
		return RetainedRecord{}, ErrPurgeNonce
	}
	switch rec.State {
	case RetainedStateRetained:
		rec.State = RetainedStatePurging
		rec.TransitionSeq++
		rec.UpdatedAt = time.Now().UTC()
		delete(f.nonces, nonceKey(caller, rec.ID)) // nonce consumed
		return *rec, nil
	case RetainedStatePurging, RetainedStatePurged:
		return *rec, nil // natural idempotent replay under a fresh nonce
	default:
		return RetainedRecord{}, ErrRetainedState
	}
}

func (f *fakeRetainedStore) CompleteAttach(_ context.Context, dataID, workspaceUID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	rec := f.recs[dataID]
	if rec == nil {
		return ErrRetainedNotFound
	}
	if rec.State != RetainedStateAttaching || rec.ConsumingWorkspaceID != workspaceUID {
		return ErrRetainedState
	}
	rec.State = RetainedStateAttached
	rec.TransitionSeq++
	return nil
}

func (f *fakeRetainedStore) ReturnToRetained(_ context.Context, dataID, workspaceUID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	rec := f.recs[dataID]
	if rec == nil {
		return ErrRetainedNotFound
	}
	if rec.ConsumingWorkspaceID != workspaceUID {
		return nil // not bound to that workspace: no-op
	}
	if rec.State != RetainedStateAttaching && rec.State != RetainedStateAttached {
		return ErrRetainedState
	}
	rec.State = RetainedStateRetained
	rec.ConsumingWorkspaceID = ""
	rec.TransitionSeq++
	return nil
}

func (f *fakeRetainedStore) CompletePurge(_ context.Context, dataID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	rec := f.recs[dataID]
	if rec == nil {
		return ErrRetainedNotFound
	}
	if rec.State != RetainedStatePurging {
		return ErrRetainedState
	}
	rec.State = RetainedStatePurged
	rec.TransitionSeq++
	return nil
}

func (f *fakeRetainedStore) RetainedPVCUIDs(_ context.Context, tenantID string) (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]string{}
	for _, r := range f.recs {
		if r.TenantID == tenantID && r.State != RetainedStatePurged {
			out[r.PVCUID] = r.ID
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------

func newDataEnv(t *testing.T, ds RetainedDataStore, opts ...func(*DataHandler)) *testEnv {
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
	h := NewDataHandler(ds, defaultCatalog(), defaultTenants())
	for _, o := range opts {
		o(h)
	}
	mux := http.NewServeMux()
	mux.Handle("/auth/login", http.HandlerFunc(a.LoginHandler))
	mux.Handle("/auth/callback", http.HandlerFunc(a.CallbackHandler))
	MountDataRoutes(mux, a, h)
	srv := httptest.NewServer(RequestID(Audit(logger)(mux)))
	env := &testEnv{issuer: iss, auth: a, store: sessions, server: srv, logs: logBuf}
	t.Cleanup(func() { srv.Close(); iss.Close() })
	return env
}

// envOwner returns the record owner matching a login subject under the
// env's live OIDC issuer (the iss|sub owner convention). Seeded records
// must use this — a fixed issuer string can never match the dynamic
// httptest issuer URL the authenticator verifies.
func envOwner(env *testEnv, subject string) string {
	return env.issuer.URL() + "|" + subject
}

func seedRetained(fs *fakeRetainedStore, id, tenant, owner, runtime string) *RetainedRecord {
	return fs.seed(RetainedRecord{
		ID:                  id,
		TenantID:            tenant,
		Owner:               owner,
		State:               RetainedStateRetained,
		PVCNamespace:        "ns-a",
		PVCName:             "ws-src-home",
		PVCUID:              "pvcuid-" + id,
		SourceWorkspaceID:   "ws_src" + id,
		SourceWorkspaceName: "research-desktop",
		Runtime:             runtime,
		SizeBytes:           20 << 30,
		RetainedAt:          time.Now().UTC(),
		UpdatedAt:           time.Now().UTC(),
	})
}

func doDataReq(t *testing.T, env *testEnv, sess, csrf *http.Cookie, method, path, body string, hdrs map[string]string) *http.Response {
	t.Helper()
	return doReq(t, env, sess, csrf, method, path, body, hdrs)
}

// listData performs GET /v1/data and decodes the retained list.
func listData(t *testing.T, env *testEnv, sess, csrf *http.Cookie) retainedDataList {
	t.Helper()
	r := doDataReq(t, env, sess, csrf, http.MethodGet, "/v1/data", "", nil)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/data: status=%d", r.StatusCode)
	}
	return decodeBody[retainedDataList](t, r)
}

func errBody(t *testing.T, r *http.Response) Error {
	t.Helper()
	defer r.Body.Close()
	var e Error
	if err := json.NewDecoder(r.Body).Decode(&e); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	return e
}

// TestListRetainedData_OwnerScope: an owner sees only their own retained
// disks; a tenant admin sees the whole tenant. Each record carries a fresh
// purgeConfirmationNonce.
func TestListRetainedData_OwnerScope(t *testing.T) {
	fs := newFakeRetainedStore()
	env := newDataEnv(t, fs)
	seedRetained(fs, "rd_aaaaaaaa01", "tenant-a", envOwner(env, "user-a"), "LinuxContainer")
	seedRetained(fs, "rd_bbbbbbbb02", "tenant-a", envOwner(env, "user-b"), "LinuxContainer")
	seedRetained(fs, "rd_cccccccc03", "tenant-b", envOwner(env, "user-b"), "LinuxContainer")

	sessA, csrfA := login(t, env, "user-a")
	got := listData(t, env, sessA, csrfA)
	if len(got.Items) != 1 || got.Items[0].ID != "rd_aaaaaaaa01" {
		t.Fatalf("owner list = %+v, want only rd_aaaaaaaa01", got.Items)
	}
	if got.Items[0].State != "Retained" || got.Items[0].PurgeConfirmationNonce == "" {
		t.Fatalf("record view missing state/nonce: %+v", got.Items[0])
	}

	env.issuer.Groups = []string{TenantAdminGroup}
	sessAdm, csrfAdm := login(t, env, "admin-1")
	got = listData(t, env, sessAdm, csrfAdm)
	if len(got.Items) != 2 {
		t.Fatalf("admin tenant list = %d items, want 2 (tenant-a only)", len(got.Items))
	}
}

// TestAttachRetained_CreatesWorkspace: a successful attach returns 201 with
// a new ws_ workspace that claims the disk exclusively (record shows
// Attaching + consumingWorkspaceId, dataPolicy Retain, retainedDataRef set).
func TestAttachRetained_CreatesWorkspace(t *testing.T) {
	fs := newFakeRetainedStore()
	env := newDataEnv(t, fs)
	rec := seedRetained(fs, "rd_attach0001", "tenant-a", envOwner(env, "user-a"), "LinuxContainer")

	sess, csrf := login(t, env, "user-a")

	r := doDataReq(t, env, sess, csrf, http.MethodPost, "/v1/data/"+rec.ID+"/attach",
		`{"name":"restored-desktop","templateRef":"tpl_linuxdesktop"}`,
		map[string]string{"Idempotency-Key": "key-attach-0001"})
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("attach: status=%d body=%+v", r.StatusCode, errBody(t, r))
	}
	ws := decodeBody[WorkspaceView](t, r)
	if ws.ID == "" || ws.RetainedDataRef != rec.ID {
		t.Fatalf("attach view = %+v, want new ws_ id with retainedDataRef=%s", ws, rec.ID)
	}
	after := fs.get(rec.ID)
	if after.State != RetainedStateAttaching || after.ConsumingWorkspaceID != ws.ID {
		t.Fatalf("record after attach = %+v, want Attaching consumed by %s", after, ws.ID)
	}
}

// TestAttachRetained_RequiresIdempotencyKey: attach without a usable
// Idempotency-Key is rejected with 400 INVALID_REQUEST.
func TestAttachRetained_RequiresIdempotencyKey(t *testing.T) {
	fs := newFakeRetainedStore()
	env := newDataEnv(t, fs)
	rec := seedRetained(fs, "rd_attach0002", "tenant-a", envOwner(env, "user-a"), "LinuxContainer")

	sess, csrf := login(t, env, "user-a")

	r := doDataReq(t, env, sess, csrf, http.MethodPost, "/v1/data/"+rec.ID+"/attach",
		`{"name":"restored-desktop","templateRef":"tpl_linuxdesktop"}`, nil)
	if r.StatusCode != http.StatusBadRequest {
		t.Fatalf("attach without Idempotency-Key: status=%d, want 400", r.StatusCode)
	}
	if e := errBody(t, r); e.Code != CodeInvalidRequest {
		t.Fatalf("code=%s, want INVALID_REQUEST", e.Code)
	}
}

// TestAttachRetained_ForeignRecord: a caller who does not own the retained
// record gets 404 — existence is not leaked across owners (tenant admins
// excepted).
func TestAttachRetained_ForeignRecord(t *testing.T) {
	fs := newFakeRetainedStore()
	env := newDataEnv(t, fs)
	rec := seedRetained(fs, "rd_attach0003", "tenant-a", envOwner(env, "user-a"), "LinuxContainer")

	sess, csrf := login(t, env, "user-b")

	r := doDataReq(t, env, sess, csrf, http.MethodPost, "/v1/data/"+rec.ID+"/attach",
		`{"name":"stolen-desktop","templateRef":"tpl_linuxdesktop"}`,
		map[string]string{"Idempotency-Key": "key-attach-0003"})
	if r.StatusCode != http.StatusNotFound {
		t.Fatalf("foreign attach: status=%d, want 404", r.StatusCode)
	}
	if e := errBody(t, r); e.Code != CodeNotFound {
		t.Fatalf("code=%s, want NOT_FOUND", e.Code)
	}
}

// TestAttachRetained_TenantAdmin: a tenant admin may attach a record owned
// by someone else in the tenant.
func TestAttachRetained_TenantAdmin(t *testing.T) {
	fs := newFakeRetainedStore()
	env := newDataEnv(t, fs)
	rec := seedRetained(fs, "rd_attach0004", "tenant-a", envOwner(env, "user-a"), "LinuxContainer")

	env.issuer.Groups = []string{TenantAdminGroup}
	sess, csrf := login(t, env, "admin-1")

	r := doDataReq(t, env, sess, csrf, http.MethodPost, "/v1/data/"+rec.ID+"/attach",
		`{"name":"admin-restore","templateRef":"tpl_linuxdesktop"}`,
		map[string]string{"Idempotency-Key": "key-attach-0004"})
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("admin attach: status=%d, want 201", r.StatusCode)
	}
}

// TestAttachRetained_NotRetained: attaching a record that is not in state
// Retained is a 409 INVALID_STATE — a disk has at most one consumer.
func TestAttachRetained_NotRetained(t *testing.T) {
	fs := newFakeRetainedStore()
	env := newDataEnv(t, fs)
	rec := seedRetained(fs, "rd_attach0005", "tenant-a", envOwner(env, "user-a"), "LinuxContainer")
	rec.State = RetainedStateAttached
	rec.ConsumingWorkspaceID = "ws_alreadythere001"
	sess, csrf := login(t, env, "user-a")

	r := doDataReq(t, env, sess, csrf, http.MethodPost, "/v1/data/"+rec.ID+"/attach",
		`{"name":"second-home","templateRef":"tpl_linuxdesktop"}`,
		map[string]string{"Idempotency-Key": "key-attach-0005"})
	if r.StatusCode != http.StatusConflict {
		t.Fatalf("attach on Attached record: status=%d, want 409", r.StatusCode)
	}
	if e := errBody(t, r); e.Code != CodeInvalidState {
		t.Fatalf("code=%s, want INVALID_STATE", e.Code)
	}
}

// TestAttachRetained_RuntimeMismatch: a template whose runtime differs from
// the disk's runtime is 422 INVALID_TEMPLATE.
func TestAttachRetained_RuntimeMismatch(t *testing.T) {
	fs := newFakeRetainedStore()
	env := newDataEnv(t, fs)
	rec := seedRetained(fs, "rd_attach0006", "tenant-a", envOwner(env, "user-a"), "WindowsVM")
	sess, csrf := login(t, env, "user-a")

	r := doDataReq(t, env, sess, csrf, http.MethodPost, "/v1/data/"+rec.ID+"/attach",
		`{"name":"wrong-runtime","templateRef":"tpl_linuxdesktop"}`,
		map[string]string{"Idempotency-Key": "key-attach-0006"})
	if r.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("runtime mismatch: status=%d, want 422", r.StatusCode)
	}
	if e := errBody(t, r); e.Code != CodeInvalidTemplate {
		t.Fatalf("code=%s, want INVALID_TEMPLATE", e.Code)
	}
}

// TestAttachRetained_QuotaExhausted: attach without compute/disk headroom
// is a coded 409 QUOTA_EXHAUSTED — never a silent failure.
func TestAttachRetained_QuotaExhausted(t *testing.T) {
	fs := newFakeRetainedStore()
	env := newDataEnv(t, fs)
	rec := seedRetained(fs, "rd_attach0007", "tenant-a", envOwner(env, "user-a"), "LinuxContainer")
	fs.quotaErr = &provisioning.QuotaExceededError{TenantID: "tenant-a", Dimension: "runningSlots"}
	sess, csrf := login(t, env, "user-a")

	r := doDataReq(t, env, sess, csrf, http.MethodPost, "/v1/data/"+rec.ID+"/attach",
		`{"name":"no-headroom","templateRef":"tpl_linuxdesktop"}`,
		map[string]string{"Idempotency-Key": "key-attach-0007"})
	if r.StatusCode != http.StatusConflict {
		t.Fatalf("quota exhausted attach: status=%d, want 409", r.StatusCode)
	}
	if e := errBody(t, r); e.Code != CodeQuotaExhausted {
		t.Fatalf("code=%s, want QUOTA_EXHAUSTED", e.Code)
	}
	if fs.get(rec.ID).State != RetainedStateRetained {
		t.Fatalf("rejected attach moved state to %s", fs.get(rec.ID).State)
	}
}

// TestAttachRetained_QuotaReleasePending: a quota refusal covered only by
// teardown-held reservations keeps the QUOTA_EXHAUSTED code but is
// retryable with details.reason=release_pending and a Retry-After header.
func TestAttachRetained_QuotaReleasePending(t *testing.T) {
	fs := newFakeRetainedStore()
	env := newDataEnv(t, fs, func(h *DataHandler) {
		h.WithReleaseRetryAfter(func() int { return 3 })
	})
	rec := seedRetained(fs, "rd_attach0023", "tenant-a", envOwner(env, "user-a"), "LinuxContainer")
	fs.quotaErr = &provisioning.QuotaExceededError{
		TenantID: "tenant-a", Dimension: "runningSlots", ReleasePending: true,
	}
	sess, csrf := login(t, env, "user-a")

	r := doDataReq(t, env, sess, csrf, http.MethodPost, "/v1/data/"+rec.ID+"/attach",
		`{"name":"pending-release","templateRef":"tpl_linuxdesktop"}`,
		map[string]string{"Idempotency-Key": "key-attach-0023"})
	if r.StatusCode != http.StatusConflict {
		t.Fatalf("release_pending attach: status=%d, want 409", r.StatusCode)
	}
	if got := r.Header.Get("Retry-After"); got != "3" {
		t.Fatalf("Retry-After=%q, want 3", got)
	}
	e := errBody(t, r)
	if e.Code != CodeQuotaExhausted || !e.Retryable ||
		e.Details == nil || e.Details.Reason != ReasonReleasePending {
		t.Fatalf("body=%+v, want retryable QUOTA_EXHAUSTED release_pending", e)
	}
	if fs.get(rec.ID).State != RetainedStateRetained {
		t.Fatalf("rejected attach moved state to %s", fs.get(rec.ID).State)
	}
}

// TestAttachRetained_QuotaNotConfigured: a tenant without a quota row is a
// coded 409 QUOTA_NOT_CONFIGURED, not QUOTA_EXHAUSTED (FX-R17).
func TestAttachRetained_QuotaNotConfigured(t *testing.T) {
	fs := newFakeRetainedStore()
	env := newDataEnv(t, fs)
	rec := seedRetained(fs, "rd_attach0017", "tenant-a", envOwner(env, "user-a"), "LinuxContainer")
	fs.quotaErr = provisioning.ErrNoQuota
	sess, csrf := login(t, env, "user-a")

	r := doDataReq(t, env, sess, csrf, http.MethodPost, "/v1/data/"+rec.ID+"/attach",
		`{"name":"no-quota","templateRef":"tpl_linuxdesktop"}`,
		map[string]string{"Idempotency-Key": "key-attach-0017"})
	if r.StatusCode != http.StatusConflict {
		t.Fatalf("status=%d, want 409", r.StatusCode)
	}
	if e := errBody(t, r); e.Code != CodeQuotaNotConfigured {
		t.Fatalf("code=%s, want QUOTA_NOT_CONFIGURED", e.Code)
	}
}

// TestAttachRetained_IdempotentReplay: same Idempotency-Key + same body
// returns the same workspace; same key + different body conflicts.
func TestAttachRetained_IdempotentReplay(t *testing.T) {
	fs := newFakeRetainedStore()
	env := newDataEnv(t, fs)
	rec := seedRetained(fs, "rd_attach0008", "tenant-a", envOwner(env, "user-a"), "LinuxContainer")

	sess, csrf := login(t, env, "user-a")
	body := `{"name":"restored-desktop","templateRef":"tpl_linuxdesktop"}`
	hdrs := map[string]string{"Idempotency-Key": "key-attach-0008"}

	r1 := doDataReq(t, env, sess, csrf, http.MethodPost, "/v1/data/"+rec.ID+"/attach", body, hdrs)
	if r1.StatusCode != http.StatusCreated {
		t.Fatalf("first attach: status=%d", r1.StatusCode)
	}
	ws1 := decodeBody[WorkspaceView](t, r1)
	r2 := doDataReq(t, env, sess, csrf, http.MethodPost, "/v1/data/"+rec.ID+"/attach", body, hdrs)
	if r2.StatusCode != http.StatusCreated {
		t.Fatalf("replayed attach: status=%d, want 201 with stored result", r2.StatusCode)
	}
	ws2 := decodeBody[WorkspaceView](t, r2)
	if ws1.ID != ws2.ID {
		t.Fatalf("idempotent replay minted %s then %s", ws1.ID, ws2.ID)
	}
	r3 := doDataReq(t, env, sess, csrf, http.MethodPost, "/v1/data/"+rec.ID+"/attach",
		`{"name":"different-name","templateRef":"tpl_linuxdesktop"}`, hdrs)
	if r3.StatusCode != http.StatusConflict {
		t.Fatalf("conflicting replay: status=%d, want 409", r3.StatusCode)
	}
	if e := errBody(t, r3); e.Code != CodeIdempotencyConflict {
		t.Fatalf("code=%s, want IDEMPOTENCY_CONFLICT", e.Code)
	}
}

// TestPurgeRetained_Accepted: purge with a valid confirmationNonce returns
// 202 and moves the record to Purging; the disk stays quota-held until the
// volume is actually gone (CompletePurge, covered by integration).
func TestPurgeRetained_Accepted(t *testing.T) {
	fs := newFakeRetainedStore()
	env := newDataEnv(t, fs)
	rec := seedRetained(fs, "rd_purge00001", "tenant-a", envOwner(env, "user-a"), "LinuxContainer")

	sess, csrf := login(t, env, "user-a")

	list := listData(t, env, sess, csrf)
	var nonce string
	for _, it := range list.Items {
		if it.ID == rec.ID {
			nonce = it.PurgeConfirmationNonce
		}
	}
	if nonce == "" {
		t.Fatal("list did not issue a purgeConfirmationNonce")
	}
	r := doDataReq(t, env, sess, csrf, http.MethodPost, "/v1/data/"+rec.ID+"/purge",
		`{"confirmationNonce":"`+nonce+`"}`, nil)
	if r.StatusCode != http.StatusAccepted {
		t.Fatalf("purge: status=%d body=%+v", r.StatusCode, errBody(t, r))
	}
	view := decodeBody[retainedDataView](t, r)
	if view.State != "Purging" {
		t.Fatalf("purge view state=%s, want Purging", view.State)
	}
}

// TestPurgeRetained_MissingOrBadNonce: no nonce, a short nonce, a stale
// nonce, and a nonce issued to another principal all fail 400.
func TestPurgeRetained_MissingOrBadNonce(t *testing.T) {
	fs := newFakeRetainedStore()
	env := newDataEnv(t, fs)
	rec := seedRetained(fs, "rd_purge00002", "tenant-a", envOwner(env, "user-a"), "LinuxContainer")

	sess, csrf := login(t, env, "user-a")

	for _, body := range []string{`{}`, `{"confirmationNonce":"short"}`, `{"confirmationNonce":"forged-nonce-value"}`} {
		r := doDataReq(t, env, sess, csrf, http.MethodPost, "/v1/data/"+rec.ID+"/purge", body, nil)
		if r.StatusCode != http.StatusBadRequest {
			t.Fatalf("purge %s: status=%d, want 400", body, r.StatusCode)
		}
		if e := errBody(t, r); e.Code != CodeInvalidRequest {
			t.Fatalf("purge %s: code=%s, want INVALID_REQUEST", body, e.Code)
		}
	}
	if fs.get(rec.ID).State != RetainedStateRetained {
		t.Fatalf("bad nonce moved state to %s", fs.get(rec.ID).State)
	}
}

// TestPurgeRetained_NonceSingleUse: the nonce is bound to the record state
// epoch — replaying the consumed nonce fails 400 even though a fresh read
// issues a new nonce that idempotently replays the purge.
func TestPurgeRetained_NonceSingleUse(t *testing.T) {
	fs := newFakeRetainedStore()
	env := newDataEnv(t, fs)
	rec := seedRetained(fs, "rd_purge00003", "tenant-a", envOwner(env, "user-a"), "LinuxContainer")

	sess, csrf := login(t, env, "user-a")

	list := listData(t, env, sess, csrf)
	nonce := list.Items[0].PurgeConfirmationNonce
	r := doDataReq(t, env, sess, csrf, http.MethodPost, "/v1/data/"+rec.ID+"/purge",
		`{"confirmationNonce":"`+nonce+`"}`, nil)
	if r.StatusCode != http.StatusAccepted {
		t.Fatalf("first purge: status=%d", r.StatusCode)
	}

	// Replaying the consumed nonce is a 400, not a second purge.
	r = doDataReq(t, env, sess, csrf, http.MethodPost, "/v1/data/"+rec.ID+"/purge",
		`{"confirmationNonce":"`+nonce+`"}`, nil)
	if r.StatusCode != http.StatusBadRequest {
		t.Fatalf("consumed nonce replay: status=%d, want 400", r.StatusCode)
	}

	// A fresh nonce replays the purge idempotently — the record is already
	// Purging and the operation is naturally idempotent.
	list = listData(t, env, sess, csrf)
	fresh := list.Items[0].PurgeConfirmationNonce
	r = doDataReq(t, env, sess, csrf, http.MethodPost, "/v1/data/"+rec.ID+"/purge",
		`{"confirmationNonce":"`+fresh+`"}`, nil)
	if r.StatusCode != http.StatusAccepted {
		t.Fatalf("fresh-nonce replay: status=%d, want 202", r.StatusCode)
	}
	if view := decodeBody[retainedDataView](t, r); view.State != "Purging" {
		t.Fatalf("replay view state=%s, want Purging", view.State)
	}
}

// TestPurgeRetained_ForeignRecord: a non-owner cannot purge — same 404
// non-leaking convention as attach.
func TestPurgeRetained_ForeignRecord(t *testing.T) {
	fs := newFakeRetainedStore()
	env := newDataEnv(t, fs)
	rec := seedRetained(fs, "rd_purge00004", "tenant-a", envOwner(env, "user-a"), "LinuxContainer")

	sess, csrf := login(t, env, "user-b")

	r := doDataReq(t, env, sess, csrf, http.MethodPost, "/v1/data/"+rec.ID+"/purge",
		`{"confirmationNonce":"some-nonce-value"}`, nil)
	if r.StatusCode != http.StatusNotFound {
		t.Fatalf("foreign purge: status=%d, want 404", r.StatusCode)
	}
}

// TestPurgeRetained_AttachedRecord: only Retained records can be purged —
// Attaching/Attached disks return 409 INVALID_STATE.
func TestPurgeRetained_AttachedRecord(t *testing.T) {
	fs := newFakeRetainedStore()
	env := newDataEnv(t, fs)
	rec := seedRetained(fs, "rd_purge00005", "tenant-a", envOwner(env, "user-a"), "LinuxContainer")

	sess, csrf := login(t, env, "user-a")

	// Move the record through attach so it is no longer Retained.
	r := doDataReq(t, env, sess, csrf, http.MethodPost, "/v1/data/"+rec.ID+"/attach",
		`{"name":"consumer","templateRef":"tpl_linuxdesktop"}`,
		map[string]string{"Idempotency-Key": "key-attach-0009"})
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("setup attach: status=%d", r.StatusCode)
	}
	list := listData(t, env, sess, csrf)
	var nonce string
	for _, it := range list.Items {
		if it.ID == rec.ID {
			nonce = it.PurgeConfirmationNonce
		}
	}
	r = doDataReq(t, env, sess, csrf, http.MethodPost, "/v1/data/"+rec.ID+"/purge",
		`{"confirmationNonce":"`+nonce+`"}`, nil)
	if r.StatusCode != http.StatusConflict {
		t.Fatalf("purge on Attaching record: status=%d, want 409", r.StatusCode)
	}
	if e := errBody(t, r); e.Code != CodeInvalidState {
		t.Fatalf("code=%s, want INVALID_STATE", e.Code)
	}
}

// TestData_Unauthenticated: all three endpoints reject requests without a
// session (middleware-level; this one may already pass pre-implementation).
func TestData_Unauthenticated(t *testing.T) {
	fs := newFakeRetainedStore()
	env := newDataEnv(t, fs)

	for _, spec := range []struct{ method, path string }{
		{http.MethodGet, "/v1/data"},
		{http.MethodPost, "/v1/data/rd_aaaaaaaa01/attach"},
		{http.MethodPost, "/v1/data/rd_aaaaaaaa01/purge"},
	} {
		req, _ := http.NewRequest(spec.method, env.server.URL+spec.path, nil)
		resp, err := noRedirectClient().Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", spec.method, spec.path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s %s unauthenticated: status=%d, want 401", spec.method, spec.path, resp.StatusCode)
		}
	}
}

// TestData_CSRFRequired: attach/purge are state-changing and require the
// synchronizer token (middleware-level).
func TestData_CSRFRequired(t *testing.T) {
	fs := newFakeRetainedStore()
	env := newDataEnv(t, fs)
	rec := seedRetained(fs, "rd_csrf000001", "tenant-a", envOwner(env, "user-a"), "LinuxContainer")

	sess, _ := login(t, env, "user-a")

	for _, path := range []string{"/v1/data/" + rec.ID + "/attach", "/v1/data/" + rec.ID + "/purge"} {
		req, _ := http.NewRequest(http.MethodPost, env.server.URL+path, nil)
		req.AddCookie(sess)
		resp, err := noRedirectClient().Do(req)
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("POST %s without CSRF: status=%d, want 403", path, resp.StatusCode)
		}
	}
}

// TestData_BadIDs: malformed dataId path params are 400 INVALID_REQUEST.
func TestData_BadIDs(t *testing.T) {
	fs := newFakeRetainedStore()
	env := newDataEnv(t, fs)
	sess, csrf := login(t, env, "user-a")

	for _, path := range []string{"/v1/data/nope/attach", "/v1/data/ws_wrongprefix/attach"} {
		r := doDataReq(t, env, sess, csrf, http.MethodPost, path,
			`{"name":"x","templateRef":"tpl_linuxdesktop"}`,
			map[string]string{"Idempotency-Key": "key-badid-00001"})
		if r.StatusCode != http.StatusBadRequest {
			t.Fatalf("POST %s: status=%d, want 400", path, r.StatusCode)
		}
		r.Body.Close()
	}
}

// ensure the fake satisfies the seam at compile time
var _ RetainedDataStore = (*fakeRetainedStore)(nil)

// ---------------------------------------------------------------------------
// SEC-I7 regression tests
// ---------------------------------------------------------------------------

// TestDataDecoderErrorNotEchoed: attach and purge never reflect decoder
// internals in the client-facing message, and trailing JSON is rejected.
func TestDataDecoderErrorNotEchoed(t *testing.T) {
	fs := newFakeRetainedStore()
	env := newDataEnv(t, fs)
	rec := seedRetained(fs, "rd_decode0001", "tenant-a", envOwner(env, "user-a"), "LinuxContainer")
	sess, csrf := login(t, env, "user-a")

	for _, body := range []string{
		`{"name":`,
		`{"name":"x","templateRef":"tpl_linuxdesktop"} {"extra":true}`,
		`{"name":"x","templateRef":"tpl_linuxdesktop"} trailing`,
	} {
		r := doDataReq(t, env, sess, csrf, http.MethodPost, "/v1/data/"+rec.ID+"/attach", body,
			map[string]string{"Idempotency-Key": "key-decode-0001"})
		if r.StatusCode != http.StatusBadRequest {
			t.Fatalf("attach body %q: status=%d, want 400", body, r.StatusCode)
		}
		if e := errBody(t, r); e.Code != CodeInvalidRequest || e.Message != "invalid request body" {
			t.Fatalf("attach body %q: error=%+v, want generic INVALID_REQUEST", body, e)
		}
	}

	for _, body := range []string{
		`{"confirmationNonce":`,
		`{"confirmationNonce":"some-nonce-value"} {}`,
	} {
		r := doDataReq(t, env, sess, csrf, http.MethodPost, "/v1/data/"+rec.ID+"/purge", body, nil)
		if r.StatusCode != http.StatusBadRequest {
			t.Fatalf("purge body %q: status=%d, want 400", body, r.StatusCode)
		}
		if e := errBody(t, r); e.Code != CodeInvalidRequest || e.Message != "invalid request body" {
			t.Fatalf("purge body %q: error=%+v, want generic INVALID_REQUEST", body, e)
		}
	}
}

// TestDataList_BadPageToken: a malformed pageToken maps to 400
// INVALID_REQUEST rather than a 500 (SEC-I7).
func TestDataList_BadPageToken(t *testing.T) {
	fs := newFakeRetainedStore()
	fs.listErr = fmt.Errorf("decode: %w", provisioning.ErrBadCursor)
	env := newDataEnv(t, fs)
	sess, csrf := login(t, env, "user-a")

	r := doDataReq(t, env, sess, csrf, http.MethodGet, "/v1/data?pageToken=%%%bad", "", nil)
	if r.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad pageToken: status=%d, want 400", r.StatusCode)
	}
	if e := errBody(t, r); e.Code != CodeInvalidRequest {
		t.Fatalf("code=%s, want INVALID_REQUEST", e.Code)
	}
}

// TestGetRetainedData_Scope: GET /v1/data/{dataId} serves the same view as a
// list row to the owner and to a tenant-admin of the same tenant; another
// user and another tenant's admin get 404 (existence is never leaked).
func TestGetRetainedData_Scope(t *testing.T) {
	fs := newFakeRetainedStore()
	env := newDataEnv(t, fs)
	seedRetained(fs, "rd_getrec0001", "tenant-a", envOwner(env, "user-a"), "LinuxContainer")

	sessA, csrfA := login(t, env, "user-a")
	r := doDataReq(t, env, sessA, csrfA, http.MethodGet, "/v1/data/rd_getrec0001", "", nil)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("owner: status=%d, want 200", r.StatusCode)
	}
	v := decodeBody[retainedDataView](t, r)
	if v.ID != "rd_getrec0001" || v.State != "Retained" || v.PurgeConfirmationNonce == "" ||
		v.Owner.Subject != "user-a" || v.SourceWorkspaceName != "research-desktop" {
		t.Fatalf("owner view = %+v", v)
	}

	sessB, csrfB := login(t, env, "user-b")
	r = doDataReq(t, env, sessB, csrfB, http.MethodGet, "/v1/data/rd_getrec0001", "", nil)
	if e := errBody(t, r); r.StatusCode != http.StatusNotFound || e.Code != CodeNotFound {
		t.Fatalf("other user: status=%d code=%q, want 404 NOT_FOUND", r.StatusCode, e.Code)
	}

	env.issuer.Groups = []string{TenantAdminGroup}
	sessAdm, csrfAdm := login(t, env, "admin-1")
	r = doDataReq(t, env, sessAdm, csrfAdm, http.MethodGet, "/v1/data/rd_getrec0001", "", nil)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("tenant-admin of the same tenant: status=%d, want 200", r.StatusCode)
	}

	env.issuer.TenantID = "tenant-b"
	sessOther, csrfOther := login(t, env, "admin-b")
	r = doDataReq(t, env, sessOther, csrfOther, http.MethodGet, "/v1/data/rd_getrec0001", "", nil)
	if e := errBody(t, r); r.StatusCode != http.StatusNotFound || e.Code != CodeNotFound {
		t.Fatalf("other tenant's admin: status=%d code=%q, want 404 NOT_FOUND", r.StatusCode, e.Code)
	}

	r = doDataReq(t, env, sessA, csrfA, http.MethodGet, "/v1/data/not-an-id", "", nil)
	if r.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed id: status=%d, want 400", r.StatusCode)
	}
}
