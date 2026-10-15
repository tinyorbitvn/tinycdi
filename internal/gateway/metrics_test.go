// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package gateway_test

// E8 gateway metrics tests: session-directory rehydration outcomes, the
// per-replica session gauge, launch refusals and the label-cardinality
// guarantee — no series may carry a workspace ID, subject, cookie, ticket
// or concrete path.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/tinyorbitvn/tinycdi/internal/broker"
	"github.com/tinyorbitvn/tinycdi/internal/gateway"
	"github.com/tinyorbitvn/tinycdi/internal/observability"
	"github.com/tinyorbitvn/tinycdi/internal/ratelimit"
)

// metricValue sums every series of name whose labels are a superset of
// want; counters and gauges both read through GetValue-able fields.
func metricValue(t *testing.T, reg *prometheus.Registry, name string, want map[string]string) float64 {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var sum float64
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, met := range mf.GetMetric() {
			got := map[string]string{}
			for _, l := range met.GetLabel() {
				got[l.GetName()] = l.GetValue()
			}
			match := true
			for k, v := range want {
				if got[k] != v {
					match = false
					break
				}
			}
			if match {
				sum += met.GetCounter().GetValue() + met.GetGauge().GetValue()
			}
		}
	}
	return sum
}

// newMetricsReplica builds a replica wired to the shared session directory
// AND a metric set on its own registry.
func newMetricsReplica(t *testing.T, fb *fakeBroker, id string, m *observability.Metrics) (*gateway.Gateway, *httptest.Server) {
	return newGatewayHandle(t, fb, func(c *gateway.Config) {
		c.Identity = broker.GatewayIdentity{ID: id, Audience: testDomain}
		c.Sessions = fb
		c.Metrics = m
	})
}

// TestMetrics_Rehydration (E8): a session this replica never saw is rebuilt
// from the directory and counted {result="ok"}; an unknown cookie is one
// definitive lookup counted {result="miss"}.
func TestMetrics_Rehydration(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := observability.NewMetrics(reg, nil)
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-met-rehy", testWSUID)
	_, srvA := newReplica(t, fb, "gw-A")
	_, srvB := newMetricsReplica(t, fb, "gw-B", m)
	cookie := launchOK(t, srvA, testHost, "tk-met-rehy")

	resp := proxied(t, srvB, testHost, "/", cookie, nil)
	drain(resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("rehydrated request on B = %d, want 200", resp.StatusCode)
	}
	if got := metricValue(t, reg, "tinycdi_gateway_rehydrations_total", map[string]string{"result": "ok"}); got != 1 {
		t.Fatalf("rehydrations{result=ok} = %v, want 1", got)
	}
	if got := metricValue(t, reg, "tinycdi_sessions_active", nil); got != 1 {
		t.Fatalf("sessions_active = %v, want 1", got)
	}

	// An unknown cookie is a miss: one lookup, no session allocated.
	resp = proxied(t, srvB, testHost, "/", "never-seen-cookie", nil)
	drain(resp)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unknown cookie on B = %d, want 401", resp.StatusCode)
	}
	if got := metricValue(t, reg, "tinycdi_gateway_rehydrations_total", map[string]string{"result": "miss"}); got != 1 {
		t.Fatalf("rehydrations{result=miss} = %v, want 1", got)
	}
	if got := metricValue(t, reg, "tinycdi_sessions_active", nil); got != 1 {
		t.Fatalf("sessions_active after miss = %v, want still 1", got)
	}
}

// TestMetrics_NoHighCardinalityLabels (E8): after a launch, a rehydration,
// a cookie miss and a rate-limited launch, no exported series carries a
// workspace ID, subject, cookie, ticket or concrete path in a label — and
// no label name from the forbidden set appears at all.
func TestMetrics_NoHighCardinalityLabels(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := observability.NewMetrics(reg, nil)
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-met-hc", testWSUID)
	_, srvA := newReplica(t, fb, "gw-A")
	_, srvB := newGatewayHandle(t, fb, func(c *gateway.Config) {
		c.Identity = broker.GatewayIdentity{ID: "gw-B", Audience: testDomain}
		c.Sessions = fb
		c.Metrics = m
		// burst 1: the second launch attempt inside the window is refused.
		c.LaunchLimiter = ratelimit.New(1, 1, 100, nil)
	})

	cookie := launchOK(t, srvA, testHost, "tk-met-hc")
	resp := proxied(t, srvB, testHost, "/", cookie, nil)
	drain(resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("rehydrated request = %d, want 200", resp.StatusCode)
	}
	resp = proxied(t, srvB, testHost, "/", "unknown-cookie", nil)
	drain(resp)
	// Two rapid launches: the first consumes the bucket, the second is
	// refused 429 and counted under route="/v1/launch".
	resp = doLaunch(t, srvB, testHost, "tk-unscripted", map[string]string{"Origin": testOrigin})
	drain(resp)
	resp = doLaunch(t, srvB, testHost, "tk-unscripted", map[string]string{"Origin": testOrigin})
	defer drain(resp)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second launch = %d, want 429 (rate limiter must see a refusal for this test)", resp.StatusCode)
	}
	if got := metricValue(t, reg, "tinycdi_rate_limited_total", map[string]string{"route": "/v1/launch"}); got != 1 {
		t.Fatalf("rate_limited_total{route=/v1/launch} = %v, want 1", got)
	}

	// Values that must never appear as a label value: workspace ID,
	// principal subject, cookie value, ticket, digest material.
	deny := []string{testWSUID, cookie, "tk-met-hc", "tk-unscripted", "iss|alice", testHost}
	forbidden := map[string]bool{}
	for _, n := range observability.ForbiddenLabelNames {
		forbidden[n] = true
	}
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		for _, met := range mf.GetMetric() {
			for _, l := range met.GetLabel() {
				if forbidden[l.GetName()] {
					t.Fatalf("metric %s uses forbidden label %q", mf.GetName(), l.GetName())
				}
				for _, bad := range deny {
					if strings.Contains(l.GetValue(), bad) {
						t.Fatalf("metric %s label %q carries identity material %q (value %q)",
							mf.GetName(), l.GetName(), bad, l.GetValue())
					}
				}
			}
		}
	}
}
