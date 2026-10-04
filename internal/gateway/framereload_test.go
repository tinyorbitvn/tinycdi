// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package gateway_test

// NAVTEL-1 frame re-navigation counter (tinycdi_session_frame_reloads_total):
// a session-frame document load on a session whose lease already had a
// stream is a re-navigation the per-tab owner id cannot see — the portal
// watch's FX-R32 fallback reload and user reloads alike. First loads,
// websockify upgrades, asset fetches and unauthenticated requests must
// never count.

import (
	"net/http"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/tinyorbitvn/tinycdi/internal/gateway"
	"github.com/tinyorbitvn/tinycdi/internal/observability"
)

// navMeta carries the fetch-metadata headers a browser sends on a frame or
// top-level navigation (Sec-Fetch-Mode: navigate + the context's dest).
func navMeta(dest string) map[string]string {
	return map[string]string{
		"Sec-Fetch-Mode": "navigate",
		"Sec-Fetch-Dest": dest,
	}
}

func frameReloads(t *testing.T, reg *prometheus.Registry, dest string) float64 {
	t.Helper()
	return metricValue(t, reg, "tinycdi_session_frame_reloads_total", map[string]string{"dest": dest})
}

// A document load before the lease ever streamed is a FIRST load — the
// counter only moves once a stream existed to navigate away from.
func TestFrameReload_FirstLoadNotCounted(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := observability.NewMetrics(reg, nil)
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-fr-first", testWSUID)
	_, srv := newMetricsReplica(t, fb, "gw-A", m)

	// An unknown cookie gets 401 and counts nothing.
	resp := proxied(t, srv, testHost, "/", "bogus-cookie", navMeta("iframe"))
	drain(resp)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bogus cookie = %d, want 401", resp.StatusCode)
	}

	cookie := launchOK(t, srv, testHost, "tk-fr-first")
	resp = proxied(t, srv, testHost, "/", cookie, navMeta("iframe"))
	drain(resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first document load = %d, want 200", resp.StatusCode)
	}
	if got := frameReloads(t, reg, "iframe"); got != 0 {
		t.Fatalf("frame_reloads{dest=iframe} = %v after first load, want 0", got)
	}
}

// Once the stream was live, a document navigation counts; asset fetches,
// subresource fetches of document paths and websockify upgrades do not.
func TestFrameReload_CountedAfterStream(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := observability.NewMetrics(reg, nil)
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-fr-live", testWSUID)
	_, srv := newMetricsReplica(t, fb, "gw-A", m)
	cookie := launchOK(t, srv, testHost, "tk-fr-live")

	up := upgrade(t, srv, testHost, "/websockify", cookie, map[string]string{"Origin": testOrigin})
	if up.StatusCode != http.StatusSwitchingProtocols {
		drain(up)
		t.Fatalf("upgrade = %d, want 101", up.StatusCode)
	}
	defer up.Body.Close()

	// The portal watch's lease-active frame reload: iframe navigation.
	resp := proxied(t, srv, testHost, "/", cookie, navMeta("iframe"))
	drain(resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("frame reload = %d, want 200", resp.StatusCode)
	}
	if got := frameReloads(t, reg, "iframe"); got != 1 {
		t.Fatalf("frame_reloads{dest=iframe} = %v, want 1", got)
	}

	// An asset fetch is not a document load.
	resp = proxied(t, srv, testHost, "/app/main.js", cookie, map[string]string{
		"Sec-Fetch-Mode": "no-cors",
		"Sec-Fetch-Dest": "script",
	})
	drain(resp)
	// A subresource fetch of a document path is not either — fetch
	// metadata beats the path heuristic when present.
	resp = proxied(t, srv, testHost, "/", cookie, map[string]string{
		"Sec-Fetch-Mode": "cors",
		"Sec-Fetch-Dest": "empty",
	})
	drain(resp)
	if got := frameReloads(t, reg, "iframe"); got != 1 {
		t.Fatalf("frame_reloads{dest=iframe} = %v after non-navigations, want still 1", got)
	}
	if got := metricValue(t, reg, "tinycdi_session_frame_reloads_total", nil); got != 1 {
		t.Fatalf("frame_reloads total = %v, want 1", got)
	}
}

// Destinations are bounded: a top-level navigation labels "document"; a
// browser without fetch metadata (or a non-browser client) still counts
// on the document-path heuristic under "other".
func TestFrameReload_DestLabel(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := observability.NewMetrics(reg, nil)
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-fr-dest", testWSUID)
	_, srv := newMetricsReplica(t, fb, "gw-A", m)
	cookie := launchOK(t, srv, testHost, "tk-fr-dest")

	up := upgrade(t, srv, testHost, "/websockify", cookie, map[string]string{"Origin": testOrigin})
	if up.StatusCode != http.StatusSwitchingProtocols {
		drain(up)
		t.Fatalf("upgrade = %d, want 101", up.StatusCode)
	}
	defer up.Body.Close()

	resp := proxied(t, srv, testHost, "/vnc.html", cookie, navMeta("document"))
	drain(resp)
	if got := frameReloads(t, reg, "document"); got != 1 {
		t.Fatalf("frame_reloads{dest=document} = %v, want 1", got)
	}
	// No Sec-Fetch-* headers at all: the document path still counts.
	resp = proxied(t, srv, testHost, "/", cookie, nil)
	drain(resp)
	if got := frameReloads(t, reg, "other"); got != 1 {
		t.Fatalf("frame_reloads{dest=other} = %v, want 1", got)
	}
}

// The gate is the LEASE's stream history, not the replica's: a session
// rehydrated on a sibling replica (rollout) still knows a stream was
// claimed, so its first document load here counts as a re-navigation.
func TestFrameReload_RehydratedSessionCounts(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := observability.NewMetrics(reg, nil)
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-fr-rehy", testWSUID)
	_, srvA := newReplica(t, fb, "gw-A")
	_, srvB := newMetricsReplica(t, fb, "gw-B", m)
	cookie := launchOK(t, srvA, testHost, "tk-fr-rehy")

	up := upgrade(t, srvA, testHost, "/websockify", cookie, map[string]string{"Origin": testOrigin})
	if up.StatusCode != http.StatusSwitchingProtocols {
		drain(up)
		t.Fatalf("upgrade on A = %d, want 101", up.StatusCode)
	}
	defer up.Body.Close()

	// B has never seen the cookie: the document load rehydrates the
	// session AND counts, because the lease's stream epoch is already >0.
	resp := proxied(t, srvB, testHost, "/", cookie, navMeta("iframe"))
	drain(resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("rehydrated frame load on B = %d, want 200", resp.StatusCode)
	}
	if got := frameReloads(t, reg, "iframe"); got != 1 {
		t.Fatalf("frame_reloads{dest=iframe} = %v on B, want 1", got)
	}
}

// Without a session directory there is no stream epoch — the local
// "a conn was hijacked" record still arms the counter.
func TestFrameReload_NoDirectory(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := observability.NewMetrics(reg, nil)
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-fr-nodir", testWSUID)
	srv := newGateway(t, fb, func(c *gateway.Config) { c.Metrics = m })
	cookie := launchOK(t, srv, testHost, "tk-fr-nodir")

	resp := proxied(t, srv, testHost, "/", cookie, navMeta("iframe"))
	drain(resp)
	up := upgrade(t, srv, testHost, "/websockify", cookie, map[string]string{"Origin": testOrigin})
	if up.StatusCode != http.StatusSwitchingProtocols {
		drain(up)
		t.Fatalf("upgrade = %d, want 101", up.StatusCode)
	}
	defer up.Body.Close()
	resp = proxied(t, srv, testHost, "/", cookie, navMeta("iframe"))
	drain(resp)

	if got := frameReloads(t, reg, "iframe"); got != 1 {
		t.Fatalf("frame_reloads{dest=iframe} = %v, want 1 (local stream record)", got)
	}
}

// A POST to a document path is never a navigation.
func TestFrameReload_PostNotCounted(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := observability.NewMetrics(reg, nil)
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-fr-post", testWSUID)
	_, srv := newMetricsReplica(t, fb, "gw-A", m)
	cookie := launchOK(t, srv, testHost, "tk-fr-post")

	up := upgrade(t, srv, testHost, "/websockify", cookie, map[string]string{"Origin": testOrigin})
	if up.StatusCode != http.StatusSwitchingProtocols {
		drain(up)
		t.Fatalf("upgrade = %d, want 101", up.StatusCode)
	}
	defer up.Body.Close()

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = testHost
	req.Header.Set("Cookie", gateway.SessionCookieName+"="+cookie)
	resp, err := srv.Client().Transport.(*http.Transport).RoundTrip(req)
	if err != nil {
		t.Fatalf("POST roundtrip: %v", err)
	}
	drain(resp)
	if got := metricValue(t, reg, "tinycdi_session_frame_reloads_total", nil); got != 0 {
		t.Fatalf("frame_reloads = %v after POST, want 0", got)
	}
}
