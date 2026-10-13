// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package api

import (
	"fmt"
	"net/http"
	"net/url"
	"testing"

	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
)

// TestTemplatesList_Paged: limit=1 walks the whole catalog via nextCursor,
// name ascending, each template exactly once.
func TestTemplatesList_Paged(t *testing.T) {
	cat := &fakeCatalog{entries: map[string][]provisioning.TemplateInfo{
		"tenant-a": {
			{Name: "charlie", Revision: 1, Runtime: "LinuxContainer", Experience: "Desktop"},
			{Name: "alpha", Revision: 1, Runtime: "LinuxContainer", Experience: "Desktop"},
			{Name: "bravo", Revision: 1, Runtime: "LinuxContainer", Experience: "Browser"},
		},
	}}
	env := newWorkspaceEnv(t, newFakeBackend(), cat, defaultTenants())
	sess, csrf := login(t, env, "user-a")

	var got []string
	q := "limit=1"
	for pages := 0; ; pages++ {
		if pages > 4 {
			t.Fatal("paging did not terminate")
		}
		r := doReq(t, env, sess, csrf, http.MethodGet, "/v1/templates?"+q, "", nil)
		if r.StatusCode != http.StatusOK {
			t.Fatalf("status=%d body=%+v", r.StatusCode, decodeErr(t, r))
		}
		p := decodeBody[Page[templateView]](t, r)
		if len(p.Items) > 1 {
			t.Fatalf("limit=1 returned %d items", len(p.Items))
		}
		for _, it := range p.Items {
			got = append(got, it.Name)
		}
		if p.NextCursor == "" {
			break
		}
		q = "limit=1&cursor=" + url.QueryEscape(p.NextCursor)
	}
	if fmt.Sprint(got) != "[alpha bravo charlie]" {
		t.Fatalf("walk=%v, want [alpha bravo charlie]", got)
	}

	r := doReq(t, env, sess, csrf, http.MethodGet, "/v1/templates?sort=createdAt", "", nil)
	if e := decodeErr(t, r); r.StatusCode != http.StatusBadRequest || e.Code != CodeInvalidRequest {
		t.Fatalf("unsupported sort: status=%d code=%s, want 400 INVALID_REQUEST", r.StatusCode, e.Code)
	}
}
