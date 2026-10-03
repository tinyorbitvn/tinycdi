// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package backend

// E8 metrics wiring tests: the app listener is instrumented under
// listener="app", and /metrics exists only on the dedicated metrics
// listener — never on the public app or session listeners.

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/tinyorbitvn/tinycdi/internal/broker"
	"github.com/tinyorbitvn/tinycdi/internal/gateway"
	"github.com/tinyorbitvn/tinycdi/internal/observability"
	"github.com/tinyorbitvn/tinycdi/internal/provisioning"
	"github.com/tinyorbitvn/tinycdi/internal/sessionhost"
)

// metricSum sums every series of name whose labels are a superset of want.
func metricSum(t *testing.T, reg *prometheus.Registry, name string, want map[string]string) float64 {
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
			match := true
			got := map[string]string{}
			for _, l := range met.GetLabel() {
				got[l.GetName()] = l.GetValue()
			}
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

// TestMetrics_AppListenerInstrumented (E8): one request on the production
// app-listener handler increments tinycdi_http_requests_total under
// listener="app" with the mux route template — never a concrete path.
func TestMetrics_AppListenerInstrumented(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := observability.NewMetrics(reg, nil)
	srv := httptest.NewServer(testAppHandlerWithMetrics(t, m))
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/v1/session")
	if err != nil {
		t.Fatalf("GET /v1/session: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/session = %d, want 200", resp.StatusCode)
	}

	got := metricSum(t, reg, "tinycdi_http_requests_total",
		map[string]string{"listener": "app", "route": "/v1/session", "code_class": "2xx"})
	if got != 1 {
		t.Fatalf("tinycdi_http_requests_total{listener=app,route=/v1/session,code_class=2xx} = %v, want 1", got)
	}
}

// fakeTemplateLister is a static templateLister for the image-age sync
// tests: tenant ID -> catalog entries.
type fakeTemplateLister map[string][]provisioning.TemplateCatalogEntry

func (f fakeTemplateLister) List(_ context.Context, tenantID, _ string) ([]provisioning.TemplateCatalogEntry, error) {
	return f[tenantID], nil
}

// TestImageAgeSync_PublishesAndPrunes: the gauge reports each family's
// newest-revision image age, skips absent/malformed timestamps and drops
// series whose family left the catalog.
func TestImageAgeSync_PublishesAndPrunes(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := observability.NewMetrics(reg, nil)
	tenants := provisioning.TenantNamespaces{"tenant-a": "ns-a"}
	built := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	cat := fakeTemplateLister{"tenant-a": {
		{Name: "browser", ImageBuiltAt: built},
		{Name: "desktop", ImageBuiltAt: ""},
		{Name: "broken", ImageBuiltAt: "not-a-timestamp"},
	}}
	known := map[string]struct{}{}
	syncImageAges(context.Background(), cat, tenants, m, testLog(), known)

	age := metricSum(t, reg, "tinycdi_runtime_image_age_seconds", map[string]string{"family": "browser"})
	if age < 7100 || age > 7300 {
		t.Fatalf("runtime_image_age_seconds{family=browser} = %v, want ~7200", age)
	}
	if got := metricSum(t, reg, "tinycdi_runtime_image_age_seconds", map[string]string{"family": "desktop"}); got != 0 {
		t.Fatalf("family without imageBuiltAt produced a series: %v", got)
	}
	if got := metricSum(t, reg, "tinycdi_runtime_image_age_seconds", map[string]string{"family": "broken"}); got != 0 {
		t.Fatalf("malformed imageBuiltAt produced a series: %v", got)
	}

	// A family that leaves the catalog loses its series.
	cat["tenant-a"] = cat["tenant-a"][1:]
	syncImageAges(context.Background(), cat, tenants, m, testLog(), known)
	if got := metricSum(t, reg, "tinycdi_runtime_image_age_seconds", map[string]string{"family": "browser"}); got != 0 {
		t.Fatalf("departed family kept its series: %v", got)
	}
}

// stubBrokerClient is a dead broker.BrokerClient: the session listener only
// needs the interface satisfied to serve host-level 404s.
type stubBrokerClient struct{}

func (stubBrokerClient) RedeemTicket(context.Context, broker.GatewayIdentity, string) (broker.Lease, error) {
	return broker.Lease{}, errors.New("stub")
}
func (stubBrokerClient) RenewLease(context.Context, broker.GatewayIdentity, string, broker.Fence) (broker.Lease, error) {
	return broker.Lease{}, errors.New("stub")
}
func (stubBrokerClient) ResolveTarget(context.Context, broker.GatewayIdentity, string) (broker.Target, error) {
	return broker.Target{}, errors.New("stub")
}
func (stubBrokerClient) RevokeLease(context.Context, string) error { return errors.New("stub") }
func (stubBrokerClient) RevokeLeaseChanged(context.Context, string) (bool, error) {
	return false, errors.New("stub")
}
func (stubBrokerClient) ReportActivity(context.Context, broker.GatewayIdentity, string, broker.Fence, broker.ActivityEvent) error {
	return errors.New("stub")
}

// TestMetrics_NotOnPublicListeners (E8): /metrics is served only on the
// dedicated metrics listener. On the app listener the route table simply
// has no such route; on the session listener the path answers 404 for
// every host class — workspace, control and foreign alike.
func TestMetrics_NotOnPublicListeners(t *testing.T) {
	// App listener.
	appSrv := httptest.NewServer(testAppHandler(t))
	t.Cleanup(appSrv.Close)
	resp, err := http.Get(appSrv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET app /metrics: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /metrics on app listener = %d, want 404", resp.StatusCode)
	}

	// Session listener.
	dom, err := sessionhost.ParseDomain("session.example.test")
	if err != nil {
		t.Fatal(err)
	}
	gw, err := gateway.New(gateway.Config{
		Identity:      broker.GatewayIdentity{ID: "gw-test", Audience: "session.example.test"},
		SessionDomain: dom,
		ControlHosts:  []string{"backend.tinycdi.svc"},
		Broker:        stubBrokerClient{},
	})
	if err != nil {
		t.Fatalf("gateway.New: %v", err)
	}
	sessSrv := httptest.NewServer(gw)
	t.Cleanup(sessSrv.Close)
	t.Cleanup(gw.Close)

	for _, host := range []string{
		"ws-deadbeef.session.example.test", // workspace host
		"backend.tinycdi.svc",              // control host
		"unrelated.example.net",            // foreign host
	} {
		req, err := http.NewRequest(http.MethodGet, sessSrv.URL+"/metrics", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = host
		resp, err := sessSrv.Client().Transport.(*http.Transport).RoundTrip(req)
		if err != nil {
			t.Fatalf("GET /metrics host %q: %v", host, err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("GET /metrics on session listener host %q = %d, want 404", host, resp.StatusCode)
		}
	}
}
