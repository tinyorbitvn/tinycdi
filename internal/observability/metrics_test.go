package observability

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

func gatherFamilies(t *testing.T, reg *prometheus.Registry) map[string][]string {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]string{}
	for _, mf := range mfs {
		var labels []string
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				labels = append(labels, l.GetName())
			}
		}
		out[mf.GetName()] = labels
	}
	return out
}

func TestMetricCatalogueRegistered(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg, []string{"tenant-a", "tenant-b"})

	m.ObserveHTTP("app", "/v1/me", "GET", "2xx", 25*time.Millisecond)
	m.ObserveProvisioningLatency("success", 30*time.Second)
	m.SetRunningWorkspaces("tenant-a", 3)
	m.SetReservedWorkspaces("tenant-b", 1)
	m.IncLeaseFailure("expired")
	m.SetStuckFinalizers(2)
	m.SetQuotaDrift("tenant-a", 0)
	m.SetPVCLeaks(1)
	m.IncBootDeadlineExceeded()
	m.AddSessionsActive(2)
	m.IncRehydration("ok")
	m.IncStreamsFenced()
	m.IncLogin("success")
	m.SetRuntimeImageAge("browser", 3600)
	m.IncRateLimited("/v1/login")

	want := []string{
		"tinycdi_http_requests_total",
		"tinycdi_http_request_duration_seconds",
		"tinycdi_workspace_provisioning_seconds",
		"tinycdi_workspaces_running",
		"tinycdi_workspaces_reserved",
		"tinycdi_lease_failures_total",
		"tinycdi_finalizers_stuck",
		"tinycdi_quota_drift",
		"tinycdi_pvc_leaks",
		"tinycdi_boot_deadline_exceeded_total",
		"tinycdi_sessions_active",
		"tinycdi_gateway_rehydrations_total",
		"tinycdi_gateway_streams_fenced_total",
		"tinycdi_logins_total",
		"tinycdi_runtime_image_age_seconds",
		"tinycdi_rate_limited_total",
	}
	fams := gatherFamilies(t, reg)
	for _, name := range want {
		if _, ok := fams[name]; !ok {
			t.Fatalf("metric %s not registered/emitted", name)
		}
	}
}

// TestNoForbiddenLabelNames is the label-cardinality guard: it fails if any
// registered metric carries a label that would leak identity or explode
// cardinality (user email/subject, workspace name/ID, request IDs, ...).
func TestNoForbiddenLabelNames(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg, []string{"tenant-a"})
	m.ObserveHTTP("app", "/v1/x", "GET", "2xx", time.Millisecond)
	m.SetRunningWorkspaces("tenant-a", 1)
	m.SetQuotaDrift("tenant-a", 1)
	m.IncLeaseFailure("denied")
	m.ObserveProvisioningLatency("failure", time.Second)

	forbidden := map[string]bool{}
	for _, n := range ForbiddenLabelNames {
		forbidden[n] = true
	}
	for fam, labels := range gatherFamilies(t, reg) {
		for _, l := range labels {
			if forbidden[l] {
				t.Fatalf("metric %s uses forbidden label %q", fam, l)
			}
		}
	}
}

func TestTenantLabelBounded(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg, []string{"tenant-a", "tenant-b"})

	m.SetRunningWorkspaces("tenant-a", 1)
	m.SetRunningWorkspaces("tenant-b", 1)
	// Arbitrary/user-controlled values must collapse into "other" so tenant
	// label cardinality stays <= len(allowlist)+1.
	for _, rogue := range []string{"mallory", "eve", "tenant-c", "admin@corp.com"} {
		m.SetRunningWorkspaces(rogue, 9)
	}

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var vals []string
	for _, mf := range mfs {
		if mf.GetName() != "tinycdi_workspaces_running" {
			continue
		}
		for _, met := range mf.GetMetric() {
			for _, l := range met.GetLabel() {
				if l.GetName() == "tenant" {
					vals = append(vals, l.GetValue())
				}
			}
		}
	}
	if len(vals) > 3 {
		t.Fatalf("tenant label cardinality exceeded bound: %v", vals)
	}
	for _, v := range vals {
		switch v {
		case "tenant-a", "tenant-b", "other":
		default:
			t.Fatalf("unbounded tenant label value leaked: %q", v)
		}
	}
}

// TestMethodLabelBounded: a client-sent method must never mint a new
// label value — anything outside the known verbs collapses to "other".
func TestMethodLabelBounded(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg, nil)

	m.ObserveHTTP("app", "/v1/x", "GET", "2xx", time.Millisecond)
	m.ObserveHTTP("app", "/v1/x", "FOO", "4xx", time.Millisecond)
	m.ObserveHTTP("app", "/v1/x", "descriptors", "4xx", time.Millisecond)

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() != "tinycdi_http_requests_total" {
			continue
		}
		for _, met := range mf.GetMetric() {
			for _, l := range met.GetLabel() {
				if l.GetName() != "method" {
					continue
				}
				switch l.GetValue() {
				case "GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS", "other":
				default:
					t.Fatalf("unbounded method label value: %q", l.GetValue())
				}
			}
		}
	}
	// FOO/descriptors must have collapsed into one "other" series.
	var other float64
	for _, mf := range mfs {
		if mf.GetName() != "tinycdi_http_requests_total" {
			continue
		}
		for _, met := range mf.GetMetric() {
			for _, l := range met.GetLabel() {
				if l.GetName() == "method" && l.GetValue() == "other" {
					other += met.GetCounter().GetValue()
				}
			}
		}
	}
	if other != 2 {
		t.Fatalf("method=other count = %v, want 2 (FOO + descriptors)", other)
	}
}

func TestReasonAndResultLabelsBounded(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg, nil)

	m.IncLeaseFailure("expired")
	m.IncLeaseFailure("stale-generation")
	m.IncLeaseFailure("some-novel-failure-mode-xyz")
	m.ObserveProvisioningLatency("success", time.Second)
	m.ObserveProvisioningLatency("weird-outcome", time.Second)

	// Check label *values*, not names, stay inside the bounded sets.
	mfs, _ := reg.Gather()
	for _, mf := range mfs {
		for _, met := range mf.GetMetric() {
			for _, l := range met.GetLabel() {
				switch mf.GetName() {
				case "tinycdi_lease_failures_total":
					switch l.GetValue() {
					case "expired", "conflict", "denied", "unavailable", "other":
					default:
						t.Fatalf("unbounded reason label value: %q", l.GetValue())
					}
				case "tinycdi_workspace_provisioning_seconds":
					switch l.GetValue() {
					case "success", "failure", "other":
					default:
						t.Fatalf("unbounded result label value: %q", l.GetValue())
					}
				}
			}
		}
	}
}
