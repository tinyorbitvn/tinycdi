// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package gateway_test

// Contract tests for the stream-owner tab id (FX-R31): the portal mints a
// per-tab id and sends it on the launch POST's query and — via the KasmVNC
// `path` setting the redirect re-asserts — on every stream upgrade. The
// gateway forwards it into ClaimStream, where it lands on the lease in the
// same write as the stream epoch.

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

const testTabID = "0123456789abcdef0123456789abcdef"

// doLaunchQuery is doLaunch with a query string on the POST URL (the owner
// tab id rides there — the ticket itself stays in the body).
func doLaunchQuery(t *testing.T, srv *httptest.Server, host, ticket, query string, hdr map[string]string) *http.Response {
	t.Helper()
	form := url.Values{"ticket": {ticket}}
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/launch?"+query, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("build launch request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Host = host
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := srv.Client().Transport.(*http.Transport).RoundTrip(req)
	if err != nil {
		t.Fatalf("launch roundtrip: %v", err)
	}
	return resp
}

// TestLaunch_RedirectCarriesOwnerTabPath: a launch POST whose query names a
// valid tab id gets a redirect that re-asserts it as the KasmVNC `path`
// setting — path=websockify?tcdi_tab=<id> — so the frame's websocket URL
// carries the id on every connect and retry. An absent or malformed id
// leaves `path` at the client's default (a legacy, NULL claim).
func TestLaunch_RedirectCarriesOwnerTabPath(t *testing.T) {
	for _, tc := range []struct {
		name  string
		query string
		want  string // expected path= fragment in the Location, "" = absent
	}{
		{"valid id", "tcdi_tab=" + testTabID,
			"&path=" + url.QueryEscape("websockify?tcdi_tab="+testTabID)},
		{"absent", "", ""},
		{"malformed", "tcdi_tab=not-hex!", ""},
		{"too short", "tcdi_tab=0123456789abcdef", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fb := newFakeBroker(t)
			fb.scriptTicket("tk-ot", testWSUID)
			srv := newGateway(t, fb, nil)

			resp := doLaunchQuery(t, srv, testHost, "tk-ot", tc.query,
				map[string]string{"Origin": testOrigin, "Sec-Fetch-Site": "same-origin"})
			defer drain(resp)
			if resp.StatusCode != http.StatusSeeOther {
				t.Fatalf("launch = %d, want 303", resp.StatusCode)
			}
			loc := resp.Header.Get("Location")
			if tc.want == "" {
				if strings.Contains(loc, "path=") {
					t.Fatalf("redirect %q must not carry a client path setting", loc)
				}
				return
			}
			if !strings.Contains(loc, tc.want) {
				t.Fatalf("redirect %q, want it to contain %q", loc, tc.want)
			}
		})
	}
}

// TestStreamClaim_CarriesOwnerTab: the tab id on the upgrade's query rides
// the claim into the session directory — same-tab retries claim with the
// same id, a missing one claims as legacy ("").
func TestStreamClaim_CarriesOwnerTab(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
		want string
	}{
		{"tab id", "/websockify?tcdi_tab=" + testTabID, testTabID},
		{"no id", "/websockify", ""},
		{"malformed id passes through", "/websockify?tcdi_tab=zz", "zz"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fb := newFakeBroker(t)
			fb.scriptTicket("tk-oc", testWSUID)
			_, srv := newReplica(t, fb, "gw-1")
			cookie := launchOK(t, srv, testHost, "tk-oc")
			lease := fb.leaseOf(t, "tk-oc")

			resp := upgrade(t, srv, testHost, tc.path, cookie, map[string]string{"Origin": testOrigin})
			if resp.StatusCode != http.StatusSwitchingProtocols {
				drain(resp)
				t.Fatalf("upgrade = %d, want 101", resp.StatusCode)
			}
			resp.Body.Close()

			fb.mu.Lock()
			got, ok := fb.ownerTabs[lease.ID]
			fb.mu.Unlock()
			if !ok {
				t.Fatal("claim never reached the session directory")
			}
			if got != tc.want {
				t.Fatalf("ownerTab = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestStreamClaim_SameTabReconnectKeepsOwner (FX-R31 mechanism): a second
// upgrade from the same tab — what a backend restart's KasmVNC retry does —
// claims the SAME owner id at a new epoch, while a different tab's upgrade
// claims a different one. GET /connection can then tell "ours" from
// "elsewhere" without epoch arithmetic.
func TestStreamClaim_SameTabReconnectKeepsOwner(t *testing.T) {
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-same", testWSUID)
	_, srv := newReplica(t, fb, "gw-1")
	cookie := launchOK(t, srv, testHost, "tk-same")
	lease := fb.leaseOf(t, "tk-same")

	stream := func(path string) {
		resp := upgrade(t, srv, testHost, path, cookie, map[string]string{"Origin": testOrigin})
		if resp.StatusCode != http.StatusSwitchingProtocols {
			drain(resp)
			t.Fatalf("upgrade %s = %d, want 101", path, resp.StatusCode)
		}
		resp.Body.Close()
	}
	ownerOf := func() string {
		fb.mu.Lock()
		defer fb.mu.Unlock()
		return fb.ownerTabs[lease.ID]
	}

	stream("/websockify?tcdi_tab=" + testTabID)
	stream("/websockify?tcdi_tab=" + testTabID) // same tab reconnects: same id
	if got := ownerOf(); got != testTabID {
		t.Fatalf("owner after same-tab re-claim = %q, want %q", got, testTabID)
	}
	const other = "ffffffffffffffffffffffffffffffff"
	stream("/websockify?tcdi_tab=" + other) // another tab's claim replaces it
	if got := ownerOf(); got != other {
		t.Fatalf("owner after foreign claim = %q, want %q", got, other)
	}
}
