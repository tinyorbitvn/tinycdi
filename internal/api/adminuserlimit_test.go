// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/api/oidctest"
	"github.com/tinyorbitvn/tinycdi/internal/observability"
	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
	"github.com/tinyorbitvn/tinycdi/internal/store"
)

// fakeUserLimitSource serves the admin user-limits seam from memory:
// Report merges the stored overrides/default (with their versions) into
// the canned quota report the way the Postgres-backed source does, and the
// conditional writes enforce the version the way the store's do.
type fakeUserLimitSource struct {
	mu        sync.Mutex
	report    store.QuotaReport
	overrides map[string]int64
	versions  map[string]string
	def       *int64
	defVer    string
	seq       int
	setErr    error
	writes    []string // "set:ref" | "clear:ref" | "default" | "default:clear"
}

func newFakeUserLimitSource() *fakeUserLimitSource {
	return &fakeUserLimitSource{overrides: map[string]int64{}, versions: map[string]string{}}
}

func (f *fakeUserLimitSource) nextVersion() string {
	f.seq++
	return fmt.Sprintf("v%d", f.seq)
}

// seed stores an override directly (test setup) and returns its version.
func (f *fakeUserLimitSource) seed(owner string, max int64) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.overrides[owner] = max
	f.versions[owner] = f.nextVersion()
	return f.versions[owner]
}

func (f *fakeUserLimitSource) writeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.writes)
}

func (f *fakeUserLimitSource) Report(_ context.Context, _ string) (store.QuotaReport, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rep := f.report
	rep.DefaultUserLimit = f.def
	rep.DefaultUserLimitVersion = f.defVer
	rep.UserLimits = map[string]int64{}
	rep.UserLimitVersions = map[string]string{}
	for k, v := range f.overrides {
		rep.UserLimits[k] = v
		rep.UserLimitVersions[k] = f.versions[k]
	}
	return rep, nil
}

// precondition mirrors the store's conditional write: create matches only
// a missing row, an update only the row's current version.
func precondition(cur string, exists bool, expected string, create bool) error {
	if create == exists || (!create && cur != expected) {
		return store.ErrVersionMismatch
	}
	return nil
}

func (f *fakeUserLimitSource) SetOverride(_ context.Context, _ string, owner string, max int64, expected string, create bool) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.setErr != nil {
		return "", f.setErr
	}
	_, exists := f.overrides[owner]
	if err := precondition(f.versions[owner], exists, expected, create); err != nil {
		return "", err
	}
	f.overrides[owner] = max
	f.versions[owner] = f.nextVersion()
	f.writes = append(f.writes, "set:"+owner)
	return f.versions[owner], nil
}

func (f *fakeUserLimitSource) ClearOverride(_ context.Context, _ string, owner string, expected string, create bool) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.setErr != nil {
		return "", f.setErr
	}
	_, exists := f.overrides[owner]
	if create && !exists {
		return "", nil
	}
	if err := precondition(f.versions[owner], exists, expected, create); err != nil {
		return "", err
	}
	delete(f.overrides, owner)
	delete(f.versions, owner)
	f.writes = append(f.writes, "clear:"+owner)
	return "", nil
}

func (f *fakeUserLimitSource) SetDefault(_ context.Context, _ string, max int64, expected string, create bool) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.setErr != nil {
		return "", f.setErr
	}
	if err := precondition(f.defVer, f.def != nil, expected, create); err != nil {
		return "", err
	}
	v := max
	f.def = &v
	f.defVer = f.nextVersion()
	f.writes = append(f.writes, "default")
	return f.defVer, nil
}

func (f *fakeUserLimitSource) ClearDefault(_ context.Context, _ string, expected string, create bool) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.setErr != nil {
		return "", f.setErr
	}
	if create && f.def == nil {
		return "", nil
	}
	if err := precondition(f.defVer, f.def != nil, expected, create); err != nil {
		return "", err
	}
	f.def = nil
	f.defVer = ""
	f.writes = append(f.writes, "default:clear")
	return "", nil
}

// newUserLimitsEnv mounts the /v1/admin/tenants/{tenant}/user-limits
// routes backed by src; sink may be nil.
func newUserLimitsEnv(t *testing.T, src UserLimitSource, sink observability.AuditSink) *testEnv {
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
	mux := http.NewServeMux()
	mux.Handle("/auth/login", http.HandlerFunc(a.LoginHandler))
	mux.Handle("/auth/callback", http.HandlerFunc(a.CallbackHandler))
	h := NewAdminUserLimitsHandler(src, newFakeDirectory(), defaultTenants())
	if sink != nil {
		h.WithAuditSink(sink)
	}
	MountAdminUserLimitRoutes(mux, a, h)
	srv := httptest.NewServer(RequestID(Audit(logger)(mux)))
	env := &testEnv{issuer: iss, auth: a, store: sessions, server: srv, logs: logBuf}
	t.Cleanup(func() { srv.Close(); iss.Close() })
	return env
}

type userLimitEntryJSON struct {
	OwnerRef    string `json:"ownerRef"`
	Subject     string `json:"subject"`
	DisplayName string `json:"displayName"`
	Limit       *int64 `json:"limit"`
	Effective   *int64 `json:"effective"`
	Running     int64  `json:"running"`
	ETag        string `json:"etag"`
}

type userLimitPageJSON struct {
	Items      []userLimitEntryJSON `json:"items"`
	NextCursor string               `json:"nextCursor"`
}

type userLimitDefaultJSON struct {
	Tenant string `json:"tenant"`
	Limit  *int64 `json:"limit"`
}

const userLimitsPath = "/v1/admin/tenants/tenant-a/user-limits"
const userLimitDefaultPath = userLimitsPath + "/default"

// userLimitPath is the item path of ownerRef — escaped, since an issuer
// URL carries '/' and the reference itself a '|'.
func userLimitPath(ownerRef string) string {
	return userLimitsPath + "/" + url.PathEscape(ownerRef)
}

// ulFixture: two owners with usage (caller is user-a by default login).
func ulFixture(iss string) store.QuotaReport {
	return store.QuotaReport{
		HasLimits: true,
		Owners: []store.OwnerUsage{
			{OwnerRef: iss + "|user-a", Usage: store.QuotaAmounts{RunningSlots: 2}},
			{OwnerRef: iss + "|user-b", Usage: store.QuotaAmounts{RunningSlots: 1}},
		},
	}
}

// TestUserLimits_PagedAndPerItemETag: the list is a cursor-paged
// collection ordered by ownerRef; walking it with limit=2 visits every
// override exactly once, and every override row carries its own strong
// quoted etag.
func TestUserLimits_PagedAndPerItemETag(t *testing.T) {
	src := newFakeUserLimitSource()
	env := newUserLimitsEnv(t, src, nil)
	want := map[string]string{}
	for i := 0; i < 5; i++ {
		ref := fmt.Sprintf("%s|user-%d", env.issuer.URL(), i)
		want[ref] = src.seed(ref, int64(i+1))
	}
	sess, csrf := loginAdmin(t, env, "admin-a")

	var seen []string
	cursor := ""
	for pages := 0; ; pages++ {
		if pages > 5 {
			t.Fatal("paging did not terminate")
		}
		path := userLimitsPath + "?limit=2"
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		r := doReq(t, env, sess, csrf, http.MethodGet, path, "", nil)
		if r.StatusCode != http.StatusOK {
			r.Body.Close()
			t.Fatalf("GET %s: status=%d", path, r.StatusCode)
		}
		p := decodeBody[userLimitPageJSON](t, r)
		if len(p.Items) > 2 {
			t.Fatalf("page of %d items, want <= 2", len(p.Items))
		}
		for _, it := range p.Items {
			if !strings.HasPrefix(it.ETag, `"`) || !strings.HasSuffix(it.ETag, `"`) || len(it.ETag) < 3 {
				t.Fatalf("item %s etag=%q, want a non-empty quoted strong tag", it.OwnerRef, it.ETag)
			}
			if it.ETag != ETagOf(want[it.OwnerRef]) {
				t.Fatalf("item %s etag=%q, want the tag of its own version", it.OwnerRef, it.ETag)
			}
			seen = append(seen, it.OwnerRef)
		}
		if p.NextCursor == "" {
			break
		}
		cursor = p.NextCursor
	}
	if len(seen) != 5 {
		t.Fatalf("walk visited %d rows, want 5: %v", len(seen), seen)
	}
	for i := 1; i < len(seen); i++ {
		if seen[i-1] >= seen[i] {
			t.Fatalf("walk not strictly ordered by ownerRef: %v", seen)
		}
	}

	requireErr(t, doReq(t, env, sess, csrf, http.MethodGet, userLimitsPath+"?sort=running", "", nil),
		http.StatusBadRequest, CodeInvalidRequest, "unsupported sort")
	requireAdminGuards(t, env, http.MethodGet, userLimitsPath, "", nil, src.writeCount)
}

// TestAdminUserLimits_GetView: the collection unions the principals with
// usage and the ones with an override — an override on a principal with
// no usage yet included — with the stored and effective limits resolved.
func TestAdminUserLimits_GetView(t *testing.T) {
	src := newFakeUserLimitSource()
	env := newUserLimitsEnv(t, src, nil)
	src.report = ulFixture(env.issuer.URL())
	def := int64(4)
	src.def = &def
	src.seed(env.issuer.URL()+"|user-b", 2)
	src.seed(env.issuer.URL()+"|ghost", 7)
	sess, csrf := loginAdmin(t, env, "admin-a")

	r := doReq(t, env, sess, csrf, http.MethodGet, userLimitsPath, "", nil)
	p := decodeBody[userLimitPageJSON](t, r)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200", r.StatusCode)
	}
	if len(p.Items) != 3 || p.NextCursor != "" {
		t.Fatalf("items=%d next=%q, want 3 on one page (usage owners + override-only)", len(p.Items), p.NextCursor)
	}
	by := map[string]userLimitEntryJSON{}
	for _, u := range p.Items {
		by[u.Subject] = u
	}
	a := by["user-a"]
	if a.Running != 2 || a.Limit != nil || a.Effective == nil || *a.Effective != 4 || a.ETag != "" {
		t.Fatalf("user-a row=%+v, want running=2 limit=null effective=4 and no etag (no override row)", a)
	}
	b := by["user-b"]
	if b.Limit == nil || *b.Limit != 2 || b.Effective == nil || *b.Effective != 2 || b.Running != 1 {
		t.Fatalf("user-b row=%+v, want limit=2 effective=2 running=1", b)
	}
	g := by["ghost"]
	if g.Limit == nil || *g.Limit != 7 || g.Effective == nil || *g.Effective != 7 || g.Running != 0 {
		t.Fatalf("ghost row=%+v, want override 7, running 0", g)
	}
}

// TestUserLimit_PutRequiresIfMatch: PUT .../user-limits/{ownerRef} is a
// strong-ETag write — 428 without If-Match, "*" creates a missing override
// (and is 412 once it exists), a stale tag is 412, the current tag writes
// and answers the item with a new ETag.
func TestUserLimit_PutRequiresIfMatch(t *testing.T) {
	src := newFakeUserLimitSource()
	env := newUserLimitsEnv(t, src, nil)
	src.report = ulFixture(env.issuer.URL())
	sess, csrf := loginAdmin(t, env, "admin-a")
	me := env.issuer.URL() + "|user-a"
	put := func(body string, hdrs map[string]string) *http.Response {
		return doReq(t, env, sess, csrf, http.MethodPut, userLimitPath(me), body, hdrs)
	}

	requireErr(t, put(`{"limit":5}`, nil), http.StatusPreconditionRequired, CodePreconditionRequired, "no If-Match")
	requireErr(t, put(`{"limit":5}`, map[string]string{"If-Match": ETagOf("v1")}),
		http.StatusPreconditionFailed, CodePreconditionFailed, "tag on a missing override")
	if src.writeCount() != 0 {
		t.Fatalf("refused preconditions reached the store: %v", src.writes)
	}

	r := put(`{"limit":5}`, map[string]string{"If-Match": "*"})
	it := decodeBody[userLimitEntryJSON](t, r)
	tag := r.Header.Get("ETag")
	if r.StatusCode != http.StatusOK || tag == "" || it.ETag != tag {
		t.Fatalf("create: status=%d ETag=%q item=%+v", r.StatusCode, tag, it)
	}
	if it.OwnerRef != me || it.Limit == nil || *it.Limit != 5 || it.Effective == nil || *it.Effective != 5 || it.Running != 2 {
		t.Fatalf("created item=%+v, want override+effective 5, running 2", it)
	}

	requireErr(t, put(`{"limit":6}`, map[string]string{"If-Match": "*"}),
		http.StatusPreconditionFailed, CodePreconditionFailed, "* on an existing override")
	requireErr(t, put(`{"limit":6}`, map[string]string{"If-Match": ETagOf("v0")}),
		http.StatusPreconditionFailed, CodePreconditionFailed, "stale tag")
	r = put(`{"limit":6}`, map[string]string{"If-Match": tag})
	it = decodeBody[userLimitEntryJSON](t, r)
	next := r.Header.Get("ETag")
	if r.StatusCode != http.StatusOK || next == "" || next == tag || it.Limit == nil || *it.Limit != 6 {
		t.Fatalf("update: status=%d ETag=%q (was %q) item=%+v", r.StatusCode, next, tag, it)
	}

	requireAdminGuards(t, env, http.MethodPut, userLimitPath(me), `{"limit":7}`,
		map[string]string{"If-Match": next}, src.writeCount)
}

// TestUserLimit_DeleteRequiresIfMatch: DELETE .../user-limits/{ownerRef}
// clears the override only under the current tag — 428 without If-Match,
// 412 for a stale tag or "*", 204 on a match, 404 once the row is gone.
func TestUserLimit_DeleteRequiresIfMatch(t *testing.T) {
	src := newFakeUserLimitSource()
	env := newUserLimitsEnv(t, src, nil)
	me := env.issuer.URL() + "|user-a"
	ver := src.seed(me, 3)
	sess, csrf := loginAdmin(t, env, "admin-a")
	del := func(hdrs map[string]string) *http.Response {
		return doReq(t, env, sess, csrf, http.MethodDelete, userLimitPath(me), "", hdrs)
	}

	requireAdminGuards(t, env, http.MethodDelete, userLimitPath(me), "",
		map[string]string{"If-Match": ETagOf(ver)}, src.writeCount)

	requireErr(t, del(nil), http.StatusPreconditionRequired, CodePreconditionRequired, "no If-Match")
	requireErr(t, del(map[string]string{"If-Match": ETagOf("v0")}),
		http.StatusPreconditionFailed, CodePreconditionFailed, "stale tag")
	requireErr(t, del(map[string]string{"If-Match": "*"}),
		http.StatusPreconditionFailed, CodePreconditionFailed, "* on an existing override")
	if src.writeCount() != 0 {
		t.Fatalf("refused deletes reached the store: %v", src.writes)
	}
	r := del(map[string]string{"If-Match": ETagOf(ver)})
	r.Body.Close()
	if r.StatusCode != http.StatusNoContent {
		t.Fatalf("matching delete: status=%d, want 204", r.StatusCode)
	}
	if _, ok := src.overrides[me]; ok {
		t.Fatal("override survived its delete")
	}
	requireErr(t, del(map[string]string{"If-Match": ETagOf(ver)}),
		http.StatusNotFound, CodeNotFound, "delete of a missing override")
}

// TestUserLimitDefault_IfMatch: the tenant default is its own strong-ETag
// resource — GET carries the ETag while a row exists; PUT needs If-Match,
// "*" creates, a stale tag is 412, and limit null clears the row.
func TestUserLimitDefault_IfMatch(t *testing.T) {
	src := newFakeUserLimitSource()
	env := newUserLimitsEnv(t, src, nil)
	sess, csrf := loginAdmin(t, env, "admin-a")
	put := func(body string, hdrs map[string]string) *http.Response {
		return doReq(t, env, sess, csrf, http.MethodPut, userLimitDefaultPath, body, hdrs)
	}

	r := doReq(t, env, sess, csrf, http.MethodGet, userLimitDefaultPath, "", nil)
	d := decodeBody[userLimitDefaultJSON](t, r)
	if r.StatusCode != http.StatusOK || d.Limit != nil || d.Tenant != "tenant-a" || r.Header.Get("ETag") != "" {
		t.Fatalf("GET without a default: status=%d body=%+v ETag=%q", r.StatusCode, d, r.Header.Get("ETag"))
	}
	requireErr(t, put(`{"limit":3}`, nil), http.StatusPreconditionRequired, CodePreconditionRequired, "no If-Match")
	r = put(`{"limit":3}`, map[string]string{"If-Match": "*"})
	d = decodeBody[userLimitDefaultJSON](t, r)
	tag := r.Header.Get("ETag")
	if r.StatusCode != http.StatusOK || d.Limit == nil || *d.Limit != 3 || tag == "" {
		t.Fatalf("create: status=%d body=%+v ETag=%q", r.StatusCode, d, tag)
	}
	r = doReq(t, env, sess, csrf, http.MethodGet, userLimitDefaultPath, "", nil)
	r.Body.Close()
	if r.Header.Get("ETag") != tag {
		t.Fatalf("GET ETag=%q, want the write's %q", r.Header.Get("ETag"), tag)
	}
	requireErr(t, put(`{"limit":4}`, map[string]string{"If-Match": "*"}),
		http.StatusPreconditionFailed, CodePreconditionFailed, "* on an existing default")
	requireErr(t, put(`{"limit":4}`, map[string]string{"If-Match": ETagOf("v0")}),
		http.StatusPreconditionFailed, CodePreconditionFailed, "stale tag")
	r = put(`{"limit":null}`, map[string]string{"If-Match": tag})
	d = decodeBody[userLimitDefaultJSON](t, r)
	if r.StatusCode != http.StatusOK || d.Limit != nil || r.Header.Get("ETag") != "" {
		t.Fatalf("clear: status=%d body=%+v ETag=%q", r.StatusCode, d, r.Header.Get("ETag"))
	}
	if want := []string{"default", "default:clear"}; strings.Join(src.writes, ",") != strings.Join(want, ",") {
		t.Fatalf("writes=%v, want %v", src.writes, want)
	}

	requireAdminGuards(t, env, http.MethodPut, userLimitDefaultPath, `{"limit":3}`,
		map[string]string{"If-Match": "*"}, src.writeCount)
	requireAdminGuards(t, env, http.MethodGet, userLimitDefaultPath, "", nil, src.writeCount)
}

// TestAdminUserLimits_Validation: malformed bodies, bad owner refs and
// negative limits are 400s that never reach the store.
func TestAdminUserLimits_Validation(t *testing.T) {
	src := newFakeUserLimitSource()
	env := newUserLimitsEnv(t, src, nil)
	sess, csrf := loginAdmin(t, env, "admin-a")
	ok := userLimitPath("iss|sub")

	for name, tc := range map[string]struct {
		method, path, body string
	}{
		"not json":         {http.MethodPut, ok, `nope`},
		"missing limit":    {http.MethodPut, ok, `{}`},
		"null limit":       {http.MethodPut, ok, `{"limit":null}`},
		"no separator":     {http.MethodPut, userLimitPath("justsub"), `{"limit":3}`},
		"empty issuer":     {http.MethodPut, userLimitPath("|sub"), `{"limit":3}`},
		"empty subject":    {http.MethodPut, userLimitPath("iss|"), `{"limit":3}`},
		"negative limit":   {http.MethodPut, ok, `{"limit":-1}`},
		"unknown field":    {http.MethodPut, ok, `{"limit":3,"x":1}`},
		"bad delete ref":   {http.MethodDelete, userLimitPath("justsub"), ``},
		"negative default": {http.MethodPut, userLimitDefaultPath, `{"limit":-2}`},
		"fractional":       {http.MethodPut, userLimitDefaultPath, `{"limit":1.5}`},
		"trailing doc":     {http.MethodPut, userLimitDefaultPath, `{"limit":1} {}`},
	} {
		r := doReq(t, env, sess, csrf, tc.method, tc.path, tc.body, map[string]string{"If-Match": "*"})
		e := decodeErr(t, r)
		if r.StatusCode != http.StatusBadRequest || e.Code != CodeInvalidRequest {
			t.Fatalf("%s: status=%d code=%q, want 400 INVALID_REQUEST", name, r.StatusCode, e.Code)
		}
	}
	if len(src.writes) != 0 {
		t.Fatalf("invalid writes reached the store: %v", src.writes)
	}
}

// userLimitAuditEvents returns the dedicated user-limit events a sink
// captured, in emission order.
func userLimitAuditEvents(t *testing.T, buf *bytes.Buffer) []observability.AuditEvent {
	t.Helper()
	var events []observability.AuditEvent
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var e observability.AuditEvent
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("audit line %q: %v", line, err)
		}
		if strings.HasPrefix(e.Action, "admin.user_limit") {
			events = append(events, e)
		}
	}
	return events
}

// TestAdminUserLimits_Audit: every request emits a dedicated audit record
// through the route wrapper — the reads and each write, with the route's
// action, the pseudonymous actor and target owner, the tenant as target.
func TestAdminUserLimits_Audit(t *testing.T) {
	src := newFakeUserLimitSource()
	buf := &bytes.Buffer{}
	env := newUserLimitsEnv(t, src, observability.NewJSONSink(buf))
	sess, csrf := loginAdmin(t, env, "admin-a")
	me := env.issuer.URL() + "|user-a"

	doReq(t, env, sess, csrf, http.MethodGet, userLimitsPath, "", nil).Body.Close()
	r := doReq(t, env, sess, csrf, http.MethodPut, userLimitPath(me), `{"limit":2}`, map[string]string{"If-Match": "*"})
	r.Body.Close()
	tag := r.Header.Get("ETag")
	doReq(t, env, sess, csrf, http.MethodPut, userLimitDefaultPath, `{"limit":3}`, map[string]string{"If-Match": "*"}).Body.Close()
	doReq(t, env, sess, csrf, http.MethodGet, userLimitDefaultPath, "", nil).Body.Close()
	doReq(t, env, sess, csrf, http.MethodDelete, userLimitPath(me), "", map[string]string{"If-Match": tag}).Body.Close()

	events := userLimitAuditEvents(t, buf)
	want := []string{
		auditActionAdminUserLimitGet,
		auditActionAdminUserLimitSet,
		auditActionAdminUserLimitDefaultSet,
		auditActionAdminUserLimitDefaultGet,
		auditActionAdminUserLimitClear,
	}
	if len(events) != len(want) {
		t.Fatalf("audit events=%d, want %d: %v", len(events), len(want), events)
	}
	for i, e := range events {
		if e.Action != want[i] {
			t.Fatalf("event[%d].action=%q, want %q", i, e.Action, want[i])
		}
		if e.Outcome != observability.OutcomeSuccess || e.Tenant != "tenant-a" || e.RequestID == "" {
			t.Fatalf("event[%d]=%+v, want success on tenant-a with request id", i, e)
		}
		if e.TargetUID != "tenant-a" {
			t.Fatalf("event[%d].target=%q, want the tenant", i, e.TargetUID)
		}
		if !strings.HasPrefix(e.Actor, "oidc:") {
			t.Fatalf("event[%d].actor=%q, want pseudonymous ref", i, e.Actor)
		}
		if e.Details["role"] != TenantAdminGroup {
			t.Fatalf("event[%d] lacks role=%q: %+v", i, TenantAdminGroup, e)
		}
	}
	for _, i := range []int{1, 4} {
		if o := events[i].Details["owner"]; o == "" || o == me {
			t.Fatalf("event[%d] owner=%q, want the pseudonymized target owner", i, o)
		}
	}
	if events[1].Details["max_running"] != "2" || events[2].Details["max_running"] != "3" {
		t.Fatalf("details=%v %v, want max_running recorded", events[1].Details, events[2].Details)
	}
}

// TestAdminUserLimits_AuditDenied: non-admin and cross-tenant attempts
// land in the audit stream as denied events carrying the caller's actor
// and FORBIDDEN — like admin quota, not only the http.request record.
func TestAdminUserLimits_AuditDenied(t *testing.T) {
	src := newFakeUserLimitSource()
	buf := &bytes.Buffer{}
	env := newUserLimitsEnv(t, src, observability.NewJSONSink(buf))

	sess, csrf := login(t, env, "user-a")
	r := doReq(t, env, sess, csrf, http.MethodPut, userLimitPath("iss|sub"), `{"limit":3}`,
		map[string]string{"If-Match": "*"})
	r.Body.Close()
	if r.StatusCode != http.StatusForbidden {
		t.Fatalf("non-admin PUT: status=%d, want 403", r.StatusCode)
	}

	sessA, csrfA := loginAdmin(t, env, "admin-a")
	r = doReq(t, env, sessA, csrfA, http.MethodGet,
		"/v1/admin/tenants/tenant-b/user-limits", "", nil)
	r.Body.Close()
	if r.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-tenant GET: status=%d, want 403", r.StatusCode)
	}

	events := userLimitAuditEvents(t, buf)
	if len(events) != 2 {
		t.Fatalf("audit events=%d, want 2: %v", len(events), events)
	}
	if e := events[0]; e.Action != auditActionAdminUserLimitSet ||
		e.Outcome != observability.OutcomeDenied || e.ErrorCode != string(CodeForbidden) ||
		e.TargetUID != "tenant-a" {
		t.Fatalf("non-admin PUT audit=%+v, want denied admin.user_limit.set FORBIDDEN on tenant-a", e)
	}
	if e := events[1]; e.Action != auditActionAdminUserLimitGet ||
		e.Outcome != observability.OutcomeDenied || e.ErrorCode != string(CodeForbidden) ||
		e.TargetUID != "tenant-b" {
		t.Fatalf("cross-tenant GET audit=%+v, want denied admin.user_limit.get FORBIDDEN on tenant-b", e)
	}
	for _, e := range events {
		if !strings.HasPrefix(e.Actor, "oidc:") {
			t.Fatalf("denied event lacks the caller actor: %+v", e)
		}
	}
}

// TestAdminUserLimits_StoreErrors: store-level failures map to honest
// codes — a transport outage is retryable 503 UNAVAILABLE, a permanent
// failure 500 INTERNAL — and the audited event carries the real code.
func TestAdminUserLimits_StoreErrors(t *testing.T) {
	src := newFakeUserLimitSource()
	buf := &bytes.Buffer{}
	env := newUserLimitsEnv(t, src, observability.NewJSONSink(buf))
	sess, csrf := loginAdmin(t, env, "admin-a")
	me := env.issuer.URL() + "|user-a"

	src.setErr = fmt.Errorf("dial postgres: %w", context.DeadlineExceeded)
	requireErr(t, doReq(t, env, sess, csrf, http.MethodPut, userLimitPath(me), `{"limit":2}`,
		map[string]string{"If-Match": "*"}), http.StatusServiceUnavailable, CodeUnavailable, "transient store error")

	src.setErr = errors.New("pg: check constraint violated")
	requireErr(t, doReq(t, env, sess, csrf, http.MethodPut, userLimitDefaultPath, `{"limit":3}`,
		map[string]string{"If-Match": "*"}), http.StatusInternalServerError, CodeInternal, "permanent store error")

	events := userLimitAuditEvents(t, buf)
	if len(events) != 2 {
		t.Fatalf("audit events=%d, want 2: %v", len(events), events)
	}
	if events[0].ErrorCode != string(CodeUnavailable) || events[0].Outcome != observability.OutcomeFailure {
		t.Fatalf("transient audit=%+v, want failure/UNAVAILABLE", events[0])
	}
	if events[1].ErrorCode != string(CodeInternal) || events[1].Outcome != observability.OutcomeFailure {
		t.Fatalf("permanent audit=%+v, want failure/INTERNAL", events[1])
	}
}

// TestUserLimitReachedMapping: a per-principal refusal maps to 409
// QUOTA_EXHAUSTED + details.reason UserLimitReached + params limit/current
// — retryable only when teardown-pending holds cover the shortfall.
func TestUserLimitReachedMapping(t *testing.T) {
	be := newFakeBackend()
	env := newWorkspaceEnv(t, be, defaultCatalog(), defaultTenants(), func(h *WorkspaceHandler) {
		h.WithReleaseRetryAfter(func() int { return 7 })
	})
	sess, csrf := login(t, env, "user-a")
	body := `{"name":"x","templateRef":"tpl_linuxdesktop"}`

	// Hard refusal: not retryable, params carry the limit and current.
	be.createErr = &provisioning.UserLimitError{
		TenantID: "tenant-a", Owner: "iss|user-a", Limit: 1, Current: 1, Requested: 1,
	}
	r := doReq(t, env, sess, csrf, http.MethodPost, "/v1/workspaces", body,
		map[string]string{"Idempotency-Key": "key-ul-00001"})
	if r.StatusCode != http.StatusConflict {
		t.Fatalf("user-limit create: status=%d, want 409", r.StatusCode)
	}
	if got := r.Header.Get("Retry-After"); got != "" {
		t.Fatalf("hard refusal must not set Retry-After, got %q", got)
	}
	e := errBody(t, r)
	if e.Code != CodeQuotaExhausted || e.Retryable {
		t.Fatalf("body=%+v, want non-retryable QUOTA_EXHAUSTED", e)
	}
	if e.Details == nil || e.Details.Reason != ReasonUserLimitReached {
		t.Fatalf("details=%+v, want reason %q", e.Details, ReasonUserLimitReached)
	}
	if e.Details.Params[ParamKeyLimit] != "1" || e.Details.Params[ParamKeyCurrent] != "1" {
		t.Fatalf("params=%v, want limit=1 current=1", e.Details.Params)
	}

	// Teardown-pending variant: same code and reason, retryable with a
	// Retry-After estimate.
	be.createErr = &provisioning.UserLimitError{
		TenantID: "tenant-a", Owner: "iss|user-a", Limit: 1, Current: 1, Requested: 1,
		ReleasePending: true,
	}
	r = doReq(t, env, sess, csrf, http.MethodPost, "/v1/workspaces", body,
		map[string]string{"Idempotency-Key": "key-ul-00002"})
	if r.StatusCode != http.StatusConflict || r.Header.Get("Retry-After") != "7" {
		t.Fatalf("pending refusal: status=%d Retry-After=%q", r.StatusCode, r.Header.Get("Retry-After"))
	}
	e = errBody(t, r)
	if e.Code != CodeQuotaExhausted || !e.Retryable ||
		e.Details == nil || e.Details.Reason != ReasonUserLimitReached {
		t.Fatalf("pending body=%+v, want retryable QUOTA_EXHAUSTED UserLimitReached", e)
	}
}

// TestQuota_UserLimitFields: GET /v1/quota reports the caller's effective
// limit in userLimits and no per-user array.
func TestQuota_UserLimitFields(t *testing.T) {
	src := &fakeQuotaSource{}
	env := newQuotaEnv(t, src, newFakeDirectory(), defaultTenants())
	rep := quotaFixture(env.issuer.URL())
	def := int64(4)
	rep.DefaultUserLimit = &def
	rep.UserLimits = map[string]int64{env.issuer.URL() + "|user-a": 2}
	src.report = rep
	sess, csrf := login(t, env, "user-a")

	r := doReq(t, env, sess, csrf, http.MethodGet, "/v1/quota", "", nil)
	var v struct {
		Tenant     string `json:"tenant"`
		UserLimits *struct {
			RunningWorkspaces int64 `json:"runningWorkspaces"`
		} `json:"userLimits"`
		Users any `json:"users"`
	}
	defer r.Body.Close()
	if err := json.NewDecoder(r.Body).Decode(&v); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if v.UserLimits == nil || v.UserLimits.RunningWorkspaces != 2 {
		t.Fatalf("userLimits=%+v, want runningWorkspaces=2 (caller override)", v.UserLimits)
	}
	if v.Users != nil {
		t.Fatalf("users=%v, want no per-user array (the admin user-limits list owns it)", v.Users)
	}
}
