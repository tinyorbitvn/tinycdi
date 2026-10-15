package gateway_test

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

	"github.com/tinyorbitvn/tinycdi/internal/gateway"
)

// controlFrom sends GET /v1/control/session on the control host as client
// (the right-most X-Forwarded-For entry behind a trusted loopback proxy).
func controlFrom(t *testing.T, srv *httptest.Server, client, bearer string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/v1/control/session", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = testControlHost
	req.Header.Set("X-Forwarded-For", client)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := srv.Client().Transport.(*http.Transport).RoundTrip(req)
	if err != nil {
		t.Fatalf("control request: %v", err)
	}
	drain(resp)
	return resp.StatusCode
}

// TestControlAuth_FailuresLimitedPerClient (R1-26): the bearer is the only
// gate on the control hosts, so failed guesses are budgeted per client.
// Once a client spends ControlFailureBurst failures, every control request
// from it answers 429 — the right token included — while another client
// is unaffected.
func TestControlAuth_FailuresLimitedPerClient(t *testing.T) {
	fb := newFakeBroker(t)
	srv := newGateway(t, fb, func(c *gateway.Config) {
		c.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}
	})
	const attacker, operator = "198.51.100.7", "198.51.100.8"
	for i := 0; i < gateway.ControlFailureBurst; i++ {
		if got := controlFrom(t, srv, attacker, "guess"); got != http.StatusUnauthorized {
			t.Fatalf("failed guess %d = %d, want 401", i, got)
		}
	}
	if got := controlFrom(t, srv, attacker, "guess"); got != http.StatusTooManyRequests {
		t.Fatalf("guess over budget = %d, want 429", got)
	}
	if got := controlFrom(t, srv, attacker, "control-test-token"); got != http.StatusTooManyRequests {
		t.Fatalf("right token from a locked-out client = %d, want 429", got)
	}
	if got := controlFrom(t, srv, operator, "control-test-token"); got != http.StatusOK {
		t.Fatalf("right token from another client = %d, want 200", got)
	}
}
