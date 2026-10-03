package gateway_test

// E7: POST /v1/launch sits behind a per-client token bucket; a refused
// attempt is denied before ticket redemption so the ticket stays usable.

import (
	"fmt"
	"io"
	"net/http"
	"strconv"
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

// TestLaunch_SessionKeyedBehindNAT (FX-R30 test 1): twenty users behind
// ONE client address each hold a live session — re-launches carrying the
// session cookie key on the session digest, so the shared per-IP budget
// never applies to them.
func TestLaunch_SessionKeyedBehindNAT(t *testing.T) {
	fb := newFakeBroker(t)
	now := time.Now()
	// Burst exactly 20: twenty anonymous first launches spend the whole
	// per-IP budget — the re-launch fleet then passes only if it keys on
	// something other than the client address.
	lim := ratelimit.New(60, 20, 1000, func() time.Time { return now })
	srv := newGateway(t, fb, func(c *gateway.Config) { c.LaunchLimiter = lim })

	type user struct{ host, cookie string }
	users := make([]user, 20)
	for i := range users {
		wsUID := fmt.Sprintf("ws_%08x", 0x100+i)
		host := fmt.Sprintf("ws-%08x.%s", 0x100+i, testDomain)
		fb.scriptTicket("tk-nat-open-"+strconv.Itoa(i), wsUID)
		fb.scriptTicket("tk-nat-again-"+strconv.Itoa(i), wsUID)
		// First launch: anonymous — it spends one per-IP token and sets
		// the host-only session cookie.
		resp := doLaunch(t, srv, host, "tk-nat-open-"+strconv.Itoa(i), map[string]string{
			"Origin":         "https://" + host,
			"Sec-Fetch-Site": "same-origin",
		})
		var cookie string
		for _, c := range resp.Cookies() {
			if c.Name == gateway.SessionCookieName {
				cookie = c.Value
			}
		}
		drain(resp)
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("user %d first launch = %d, want 303", i, resp.StatusCode)
		}
		if cookie == "" {
			t.Fatalf("user %d first launch set no session cookie", i)
		}
		users[i] = user{host: host, cookie: cookie}
	}

	// Every re-launch carries the live session cookie, so each keys on its
	// own session digest — the empty IP bucket is never consulted.
	for i, u := range users {
		resp := doLaunch(t, srv, u.host, "tk-nat-again-"+strconv.Itoa(i), map[string]string{
			"Origin":         "https://" + u.host,
			"Sec-Fetch-Site": "same-origin",
			"Cookie":         gateway.SessionCookieName + "=" + u.cookie,
		})
		drain(resp)
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("user %d re-launch = %d, want 303 — a live session must key the launch bucket", i, resp.StatusCode)
		}
	}

	// The per-IP budget really was spent: one more anonymous launch on a
	// fresh workspace is refused — proof the re-launches did not ride the
	// client-IP key.
	fb.scriptTicket("tk-nat-anon", "ws_00000200")
	resp := doLaunch(t, srv, "ws-00000200."+testDomain, "tk-nat-anon", map[string]string{
		"Origin":         "https://ws-00000200." + testDomain,
		"Sec-Fetch-Site": "same-origin",
	})
	defer drain(resp)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("anonymous launch after the IP burst = %d, want 429", resp.StatusCode)
	}
}

// TestLaunch_AbusiveSessionKeyed (FX-R30 test 2): one session flooding
// launches that fail redemption keeps its cookie valid and stays keyed on
// the session — it starves only its own bucket while a neighbour session
// and the shared anonymous budget behave exactly as before.
func TestLaunch_AbusiveSessionKeyed(t *testing.T) {
	fb := newFakeBroker(t)
	now := time.Now()
	lim := ratelimit.New(60, 2, 1000, func() time.Time { return now }) // burst 2, frozen
	srv := newGateway(t, fb, func(c *gateway.Config) { c.LaunchLimiter = lim })

	// Two sessions on one client address already spend the whole per-IP
	// burst.
	fb.scriptTicket("tk-a", testWSUID)
	cookieA := launchOK(t, srv, testHost, "tk-a")
	fb.scriptTicket("tk-b", testWSUID2)
	cookieB := launchOK(t, srv, testHost2, "tk-b")

	hdr := func(host, cookie string) map[string]string {
		return map[string]string{
			"Origin":         "https://" + host,
			"Sec-Fetch-Site": "same-origin",
			"Cookie":         gateway.SessionCookieName + "=" + cookie,
		}
	}
	// The abuser fires launches with a bogus ticket: each passes the
	// session-keyed bucket, fails redemption, leaves the session live —
	// and from the third on hits the abuser's own empty bucket.
	for i, want := range []int{
		http.StatusUnauthorized, http.StatusUnauthorized,
		http.StatusTooManyRequests, http.StatusTooManyRequests, http.StatusTooManyRequests,
	} {
		resp := doLaunch(t, srv, testHost, "tk-bogus", hdr(testHost, cookieA))
		drain(resp)
		if resp.StatusCode != want {
			t.Fatalf("abuser launch %d = %d, want %d", i, resp.StatusCode, want)
		}
	}
	// The neighbour's own session bucket is untouched.
	fb.scriptTicket("tk-b2", testWSUID2)
	resp := doLaunch(t, srv, testHost2, "tk-b2", hdr(testHost2, cookieB))
	drain(resp)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("neighbour re-launch after the abuser flood = %d, want 303", resp.StatusCode)
	}
	// And the anonymous budget: the two first launches spent it, so an
	// anonymous launch is still refused — the forged/unknown-cookie case
	// rides the same key.
	fb.scriptTicket("tk-c", "ws_00000200")
	resp = doLaunch(t, srv, "ws-00000200."+testDomain, "tk-c", map[string]string{
		"Origin":         "https://ws-00000200." + testDomain,
		"Sec-Fetch-Site": "same-origin",
		"Cookie":         gateway.SessionCookieName + "=forged-not-a-session",
	})
	defer drain(resp)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("forged-cookie anonymous launch = %d, want 429 — an unverified cookie must fall back to the client-IP key", resp.StatusCode)
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
