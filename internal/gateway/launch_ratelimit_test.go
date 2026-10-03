package gateway_test

// E7: POST /v1/launch sits behind a per-client token bucket; a refused
// attempt is denied before ticket redemption so the ticket stays usable.

import (
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/gateway"
	"github.com/tinyorbitvn/tinycdi/internal/ratelimit"
)

// TestLaunch_RateLimitedDoesNotConsumeTicket: a launch refused by the
// limiter answers 429 with Retry-After and never reaches RedeemTicket —
// the same ticket still redeems once the bucket refills.
func TestLaunch_RateLimitedDoesNotConsumeTicket(t *testing.T) {
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-rl-1", testWSUID)
	fb.scriptTicket("tk-rl-2", testWSUID)

	now := time.Now()
	lim := ratelimit.New(60, 1, 100, func() time.Time { return now }) // 1/s, burst 1
	srv := newGateway(t, fb, func(c *gateway.Config) {
		c.LaunchLimiter = lim
	})

	// First launch drains the one-token bucket and redeems normally.
	resp := doLaunch(t, srv, testHost, "tk-rl-1", map[string]string{"Origin": testOrigin})
	drain(resp)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("first launch = %d, want 303", resp.StatusCode)
	}

	// Second launch inside the window: refused before redemption.
	resp = doLaunch(t, srv, testHost, "tk-rl-2", map[string]string{"Origin": testOrigin})
	drain(resp)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("refused launch = %d, want 429", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Fatal("429 launch carries no Retry-After")
	}
	if fb.wasRedeemed("tk-rl-2") {
		t.Fatal("rate-limited launch consumed the ticket")
	}

	// After the bucket refills the untouched ticket still redeems.
	now = now.Add(2 * time.Second)
	resp = doLaunch(t, srv, testHost, "tk-rl-2", map[string]string{"Origin": testOrigin})
	defer drain(resp)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("retried launch = %d, want 303 — the refused attempt must not burn the ticket", resp.StatusCode)
	}
}

// TestLaunch_RateLimitDisabledByDefault: without a configured limiter the
// launch surface is unchanged (contract test that nil means unlimited).
func TestLaunch_RateLimitDisabledByDefault(t *testing.T) {
	fb := newFakeBroker(t)
	srv := newGateway(t, fb, nil)
	for i := 0; i < 25; i++ {
		resp := doLaunch(t, srv, testHost, "tk-none", map[string]string{"Origin": testOrigin})
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests {
			t.Fatalf("request %d rate-limited with no limiter configured", i+1)
		}
	}
}
