package gateway_test

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/gateway"
)

// TestProxy_DialRetriesUntilUpstreamListens: the first request after Ready
// can race the Service route; the gateway retries the connect inside its
// budget instead of answering 502.
func TestProxy_DialRetriesUntilUpstreamListens(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("up")) })
	first := httptest.NewTLSServer(ok)
	addr, tlsCfg := first.Listener.Addr().String(), first.TLS
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-cold", testWSUID)
	fb.pointLeaseAt(t, "tk-cold", first)
	srv := newGateway(t, fb, func(c *gateway.Config) { c.UpstreamDialBudget = 5 * time.Second })
	cookie := launchOK(t, srv, testHost, "tk-cold")
	first.Close() // nothing listens now

	go func() {
		time.Sleep(700 * time.Millisecond)
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return
		}
		second := &httptest.Server{Listener: ln, TLS: tlsCfg, Config: &http.Server{Handler: ok}}
		second.StartTLS()
		t.Cleanup(second.Close)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/", nil)
	req.Host = testHost
	req.Header.Set("Cookie", gateway.SessionCookieName+"="+cookie)
	resp, err := srv.Client().Transport.(*http.Transport).RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	drain(resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200 after the upstream came up inside the budget", resp.StatusCode)
	}
}

func TestProxy_DialBudgetExhaustedIs502(t *testing.T) {
	dead := httptest.NewTLSServer(http.NotFoundHandler())
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-dead", testWSUID)
	fb.pointLeaseAt(t, "tk-dead", dead)
	srv := newGateway(t, fb, func(c *gateway.Config) { c.UpstreamDialBudget = 300 * time.Millisecond })
	cookie := launchOK(t, srv, testHost, "tk-dead")
	dead.Close()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/", nil)
	req.Host = testHost
	req.Header.Set("Cookie", gateway.SessionCookieName+"="+cookie)
	start := time.Now()
	resp, err := srv.Client().Transport.(*http.Transport).RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	drain(resp)
	if resp.StatusCode != http.StatusBadGateway || time.Since(start) > 3*time.Second {
		t.Fatalf("status %d after %v, want 502 within the budget", resp.StatusCode, time.Since(start))
	}
}
