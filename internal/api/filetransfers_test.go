// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package api

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/observability"
	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
	"github.com/tinyorbitvn/tinycdi/internal/sessionhost"
	"github.com/tinyorbitvn/tinycdi/internal/store"
)

func TestMintFileTransfer_RejectsBadNames(t *testing.T) {
	for _, name := range []string{"", ".", "..", "../x", "a/b", "/etc/passwd", `a\b`, "a\x00b", "a\nb",
		strings.Repeat("x", 256), "\xff\xfe", "a;b", "a..b", "50%2e.txt", "100%.txt"} {
		if msg := validTransferName(name); msg == "" {
			t.Errorf("validTransferName(%q) accepted", name)
		}
	}
	for _, name := range []string{"report.pdf", "Ảnh chụp.png", ".hidden", "a b c.txt", "x-y_z.tar.gz"} {
		if msg := validTransferName(name); msg != "" {
			t.Errorf("validTransferName(%q) refused: %s", name, msg)
		}
	}
}

func TestMintFileTransfer_PolicyOwnerAndReadiness(t *testing.T) {
	env, deps, domain, _ := newFileTransferEnv(t)
	sess, csrf := login(t, env, "user-a")
	post := func(sess, csrf *http.Cookie, body string) *http.Response {
		return doReq(t, env, sess, csrf, http.MethodPost, "/v1/workspaces/ws_xfer00001/file-transfers", body, nil)
	}
	deps.policy = provisioning.SessionPolicy{FileTransfer: "Upload", MaxFileBytes: 1000}

	res := post(sess, csrf, `{"direction":"upload","path":"a.txt","bytes":10}`)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("upload: %d %+v", res.StatusCode, decodeErr(t, res))
	}
	out := decodeBody[fileTransferView](t, res)
	origin, _ := domain.Origin("ws_xfer00001")
	if !regexp.MustCompile(`^xf_[a-z2-7]{26}$`).MatchString(out.ID) || out.MaxBytes != 1000 ||
		out.ContentURL != origin+"/v1/transfers/"+out.ID+"/content" {
		t.Fatalf("view = %+v", out)
	}
	if got := deps.inserted[0]; got.RuntimeUID != "uid-7" || got.RuntimeGeneration != 3 || got.DeclaredBytes != 10 {
		t.Fatalf("row not bound to the observed incarnation: %+v", got)
	}
	cases := []struct {
		name string
		body string
		want int
	}{
		{"download under Upload policy", `{"direction":"download","path":"a.txt"}`, http.StatusForbidden},
		{"over the cap", `{"direction":"upload","path":"a.txt","bytes":1001}`, http.StatusBadRequest},
		{"upload without bytes", `{"direction":"upload","path":"a.txt"}`, http.StatusBadRequest},
		{"bad name", `{"direction":"upload","path":"../a.txt","bytes":1}`, http.StatusBadRequest},
		{"bad direction", `{"direction":"sideways","path":"a.txt"}`, http.StatusBadRequest},
	}
	for _, c := range cases {
		if res := post(sess, csrf, c.body); res.StatusCode != c.want {
			t.Errorf("%s: %d, want %d", c.name, res.StatusCode, c.want)
		}
	}
	if e := decodeErr(t, post(sess, csrf, `{"direction":"download","path":"a.txt"}`)); e.Details == nil || e.Details.Reason != "transfer_denied" {
		t.Errorf("denied download details = %+v, want reason transfer_denied", e.Details)
	}
	bsess, bcsrf := login(t, env, "user-b")
	if res := post(bsess, bcsrf, `{"direction":"upload","path":"a.txt","bytes":1}`); res.StatusCode != http.StatusNotFound {
		t.Fatalf("another user minting on user-a's workspace: %d, want 404", res.StatusCode)
	}
	deps.status.Phase = "Provisioning"
	if res := post(sess, csrf, `{"direction":"upload","path":"a.txt","bytes":1}`); res.StatusCode != http.StatusConflict {
		t.Fatalf("not Ready: %d", res.StatusCode)
	}
	if len(deps.inserted) != 1 {
		t.Fatalf("refused mints inserted rows: %d", len(deps.inserted))
	}
}

func TestMintFileTransfer_AuditsMintAndRefusal(t *testing.T) {
	env, deps, _, sink := newFileTransferEnv(t)
	sess, csrf := login(t, env, "user-a")
	deps.policy = provisioning.SessionPolicy{FileTransfer: "Upload", MaxFileBytes: 1000}
	doReq(t, env, sess, csrf, http.MethodPost, "/v1/workspaces/ws_xfer00001/file-transfers",
		`{"direction":"upload","path":"a.txt","bytes":10}`, nil).Body.Close()
	doReq(t, env, sess, csrf, http.MethodPost, "/v1/workspaces/ws_xfer00001/file-transfers",
		`{"direction":"download","path":"a.txt"}`, nil).Body.Close() // refused: the policy is Upload
	var mint, refused *observability.AuditEvent
	for _, e := range sink.get() {
		e := e
		switch e.Action {
		case auditActionFileTransferUpload:
			mint = &e
		case auditActionFileTransferDownload:
			refused = &e
		}
	}
	if mint == nil || mint.Outcome != observability.OutcomeSuccess || mint.TargetUID != "ws_xfer00001" ||
		mint.Details["phase"] != "mint" || mint.Details["direction"] != "upload" || mint.Details["path"] != "a.txt" ||
		mint.Details["bytes"] != "10" || mint.Details["policy"] != "Upload" {
		t.Fatalf("mint event = %+v", mint)
	}
	if refused == nil || refused.Outcome != observability.OutcomeDenied || refused.ErrorCode != string(CodeForbidden) ||
		refused.TargetUID != "ws_xfer00001" || refused.Details["direction"] != "download" || refused.Details["policy"] != "Upload" {
		t.Fatalf("refusal event = %+v; a refused transfer must be audited like a completed one", refused)
	}
}

type fakeTransferDeps struct {
	mu       sync.Mutex
	policy   provisioning.SessionPolicy
	status   ObservedStatus
	inserted []store.FileTransfer
}

func (d *fakeTransferDeps) ResolveSessionPolicy(context.Context, string, provisioning.TemplateCatalogEntry) (provisioning.SessionPolicy, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.policy, nil
}

func (d *fakeTransferDeps) InsertFileTransfer(_ context.Context, t store.FileTransfer) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.inserted = append(d.inserted, t)
	return nil
}

func (d *fakeTransferDeps) WorkspaceStatus(context.Context, string, string) (ObservedStatus, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.status, nil
}

func newFileTransferEnv(t *testing.T) (*testEnv, *fakeTransferDeps, sessionhost.Domain, *captureSink) {
	t.Helper()
	be := newFakeBackend()
	deps := &fakeTransferDeps{status: ObservedStatus{
		Fresh: true, Found: true, Phase: "Ready", RuntimeUID: "uid-7", ObservedRuntimeGeneration: 3,
	}}
	domain, err := sessionhost.ParseDomain("session.example.test")
	if err != nil {
		t.Fatal(err)
	}
	sink := &captureSink{}
	h := NewFileTransferHandler(be, deps, deps, domain).WithAuditSink(sink)
	env := newEnvWithRoutes(t, func(mux *http.ServeMux, a *Authenticator) {
		MountFileTransferRoutes(mux, a, h, func(next http.Handler) http.Handler { return next })
	})
	seedWorkspace(be, env, "user-a", "ws_xfer00001", "w", time.Now())
	return env, deps, domain, sink
}
