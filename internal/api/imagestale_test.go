// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package api

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/api/oidctest"
	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
)

// staleCatalog resolves catalog entries verbatim so tests can drive the
// raw workspaces.cdi.tinyorbit.vn/image-built-at annotation value.
type staleCatalog struct {
	entries map[string]TemplateEntry // key: tenantID + "/" + template ID
}

func (c staleCatalog) Resolve(_ context.Context, tenantID, id string) (TemplateEntry, error) {
	e, ok := c.entries[tenantID+"/"+id]
	if !ok {
		return TemplateEntry{}, ErrTemplateNotFound
	}
	return e, nil
}

func (c staleCatalog) List(_ context.Context, tenantID, _, _ string, _ int) ([]TemplateEntry, string, error) {
	var out []TemplateEntry
	for k, e := range c.entries {
		if strings.HasPrefix(k, tenantID+"/") {
			out = append(out, e)
		}
	}
	return out, "", nil
}

// newStaleEnv mounts the workspace/template routes with handlers the test
// keeps handles on (logger injection for the warn-only contract).
func newStaleEnv(t *testing.T, be workspaceBackend, cat TemplateCatalog) (*testEnv, *WorkspaceHandler, *TemplateHandler) {
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
	h := NewWorkspaceHandler(be, cat, defaultTenants()).WithImageStaleAfter(336 * time.Hour)
	th := NewTemplateHandler(cat, defaultTenants()).WithImageStaleAfter(336 * time.Hour)
	h.log = logger
	th.log = logger
	mux := http.NewServeMux()
	mux.Handle("/auth/login", http.HandlerFunc(a.LoginHandler))
	mux.Handle("/auth/callback", http.HandlerFunc(a.CallbackHandler))
	MountWorkspaceRoutes(mux, a, h, th)
	srv := httptest.NewServer(RequestID(Audit(logger)(mux)))
	env := &testEnv{issuer: iss, auth: a, store: sessions, server: srv, logs: logBuf}
	t.Cleanup(func() { srv.Close(); iss.Close() })
	return env, h, th
}

func warnLines(logs string) int {
	n := 0
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, `"level":"WARN"`) {
			n++
		}
	}
	return n
}

// TestImageStale_Threshold: an image-built-at annotation older than
// -image-stale-after reports imageStale true; fresher reports false; no
// annotation leaves both fields absent.
func TestImageStale_Threshold(t *testing.T) {
	now := time.Now().UTC()
	cat := staleCatalog{entries: map[string]TemplateEntry{
		"tenant-a/tpl_fresh": {
			ID: "tpl_fresh", Name: "fresh", Revision: 1,
			Runtime: "LinuxContainer", Experience: "Desktop",
			ImageBuiltAt: now.Add(-13 * 24 * time.Hour).Format(time.RFC3339),
		},
		"tenant-a/tpl_stale": {
			ID: "tpl_stale", Name: "stale", Revision: 1,
			Runtime: "LinuxContainer", Experience: "Desktop",
			ImageBuiltAt: now.Add(-15 * 24 * time.Hour).Format(time.RFC3339),
		},
		"tenant-a/tpl_plain": {
			ID: "tpl_plain", Name: "plain", Revision: 1,
			Runtime: "LinuxContainer", Experience: "Desktop",
		},
	}}
	env, _, _ := newStaleEnv(t, newFakeBackend(), cat)
	sess, csrf := login(t, env, "user-a")

	r := doReq(t, env, sess, csrf, http.MethodGet, "/v1/templates", "", nil)
	list := decodeBody[templateList](t, r)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200", r.StatusCode)
	}
	byID := map[string]templateView{}
	for _, v := range list.Items {
		byID[v.ID] = v
	}
	fresh := byID["tpl_fresh"]
	if fresh.ImageStale == nil || *fresh.ImageStale {
		t.Fatalf("13-day-old image: imageStale=%v, want false present", fresh.ImageStale)
	}
	if fresh.ImageBuiltAt == nil {
		t.Fatal("13-day-old image: imageBuiltAt absent, want the parsed timestamp")
	}
	stale := byID["tpl_stale"]
	if stale.ImageStale == nil || !*stale.ImageStale {
		t.Fatalf("15-day-old image: imageStale=%v, want true", stale.ImageStale)
	}
	plain := byID["tpl_plain"]
	if plain.ImageBuiltAt != nil || plain.ImageStale != nil {
		t.Fatalf("no annotation: imageBuiltAt=%v imageStale=%v, want both absent",
			plain.ImageBuiltAt, plain.ImageStale)
	}
}

// TestImageStale_MalformedAnnotation: a non-RFC 3339 annotation value leaves
// both fields absent, logs exactly one warning, and the request succeeds.
func TestImageStale_MalformedAnnotation(t *testing.T) {
	cat := staleCatalog{entries: map[string]TemplateEntry{
		"tenant-a/tpl_broken": {
			ID: "tpl_broken", Name: "broken", Revision: 1,
			Runtime: "LinuxContainer", Experience: "Desktop",
			ImageBuiltAt: "last thursday-ish",
		},
	}}
	env, _, _ := newStaleEnv(t, newFakeBackend(), cat)
	sess, csrf := login(t, env, "user-a")

	r := doReq(t, env, sess, csrf, http.MethodGet, "/v1/templates", "", nil)
	list := decodeBody[templateList](t, r)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200 (malformed annotation must not fail the request)", r.StatusCode)
	}
	if len(list.Items) != 1 {
		t.Fatalf("got %d items, want 1", len(list.Items))
	}
	v := list.Items[0]
	if v.ImageBuiltAt != nil || v.ImageStale != nil {
		t.Fatalf("malformed annotation: imageBuiltAt=%v imageStale=%v, want both absent",
			v.ImageBuiltAt, v.ImageStale)
	}
	if n := warnLines(env.logs.String()); n != 1 {
		t.Fatalf("got %d warning log lines, want exactly 1\n%s", n, env.logs.String())
	}
}

// TestWorkspaceView_ImageStaleFromTemplate: the workspace view resolves
// image freshness through the workspace's template — a stale template
// reports imageStale: true; a deleted template leaves both fields absent.
func TestWorkspaceView_ImageStaleFromTemplate(t *testing.T) {
	now := time.Now().UTC()
	cat := staleCatalog{entries: map[string]TemplateEntry{
		"tenant-a/tpl_staleimg": {
			ID: "tpl_staleimg", Name: "staleimg", Revision: 2,
			Runtime: "LinuxContainer", Experience: "Desktop",
			ImageBuiltAt: now.Add(-15 * 24 * time.Hour).Format(time.RFC3339),
		},
	}}
	be := newFakeBackend()
	env, _, _ := newStaleEnv(t, be, cat)
	sess, csrf := login(t, env, "user-a")
	owner := env.issuer.URL() + "|" + env.issuer.Subject

	mkRec := func(id, tplID string) provisioning.WorkspaceRecord {
		return provisioning.WorkspaceRecord{
			ID: id, TenantID: "tenant-a", Owner: owner,
			OwnerIssuer: env.issuer.URL(), OwnerSub: env.issuer.Subject,
			Name:    "w-" + id[3:],
			Template: provisioning.TemplateInfo{
				ID: tplID, Name: strings.TrimPrefix(tplID, "tpl_"), Revision: 2,
				Runtime: "LinuxContainer", Experience: "Desktop",
			},
			Phase: "Running", DesiredState: "Running", DataPolicy: "Ephemeral",
			Revision:  1,
			CreatedAt: now.Add(-time.Hour), UpdatedAt: now,
		}
	}
	be.recs["ws_stale0001"] = mkRec("ws_stale0001", "tpl_staleimg")
	be.recs["ws_orphan001"] = mkRec("ws_orphan001", "tpl_gone")

	r := doReq(t, env, sess, csrf, http.MethodGet, "/v1/workspaces/ws_stale0001", "", nil)
	v := decodeBody[WorkspaceView](t, r)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200", r.StatusCode)
	}
	if v.ImageStale == nil || !*v.ImageStale {
		t.Fatalf("stale template: imageStale=%v, want true", v.ImageStale)
	}
	if v.ImageBuiltAt == nil {
		t.Fatal("stale template: imageBuiltAt absent, want the parsed timestamp")
	}

	r = doReq(t, env, sess, csrf, http.MethodGet, "/v1/workspaces/ws_orphan001", "", nil)
	v = decodeBody[WorkspaceView](t, r)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200 (deleted template must not fail the request)", r.StatusCode)
	}
	if v.ImageBuiltAt != nil || v.ImageStale != nil {
		t.Fatalf("deleted template: imageBuiltAt=%v imageStale=%v, want both absent",
			v.ImageBuiltAt, v.ImageStale)
	}
}
