package gateway_test

// S18: the client-address chain toward the runtime is rebuilt from the
// trusted chain only — a client-sent X-Forwarded-For/Forwarded/X-Real-IP
// never reaches the pod (the runtime keys its brute-force blacklist on the
// forwarded address; FX-R26 showed a spoofed value colliding with the
// healthcheck's 127.0.0.1 blacklist entry).

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"

	"github.com/tinyorbitvn/tinycdi/internal/gateway"
)

// fwdRecorder is the fake runtime for these tests: it captures the
// client-address headers the gateway actually forwarded.
type fwdRecorder struct {
	mu        sync.Mutex
	xff       string
	forwarded string
	realIP    string
	srv       *httptest.Server
}

func newFwdRecorder(t *testing.T) *fwdRecorder {
	u := &fwdRecorder{}
	u.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.xff = r.Header.Get("X-Forwarded-For")
		u.forwarded = r.Header.Get("Forwarded")
		u.realIP = r.Header.Get("X-Real-Ip")
		u.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *fwdRecorder) seen() (xff, forwarded, realIP string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.xff, u.forwarded, u.realIP
}

// launchOn spins a gateway whose broker resolves the scripted ticket's
// lease to rec, redeems it and returns the session cookie.
func launchOn(t *testing.T, fb *fakeBroker, rec *fwdRecorder, mutate func(*gateway.Config)) (*httptest.Server, string) {
	t.Helper()
	fb.scriptTicket("tk-fwd", testWSUID)
	l := fb.leaseOf(t, "tk-fwd")
	fb.mu.Lock()
	fb.resolveT[l.ID] = targetFor(rec.srv)
	fb.mu.Unlock()
	srv := newGateway(t, fb, mutate)
	return srv, launchOK(t, srv, testHost, "tk-fwd")
}

// TestProxy_ClientXFFNeverReachesRuntime: with no trusted proxies the
// client IS the socket peer; its self-declared addresses are stripped and
// the runtime sees the verified peer only.
func TestProxy_ClientXFFNeverReachesRuntime(t *testing.T) {
	fb := newFakeBroker(t)
	rec := newFwdRecorder(t)
	srv, cookie := launchOn(t, fb, rec, nil)

	resp := proxied(t, srv, testHost, "/", cookie, map[string]string{
		"X-Forwarded-For": "198.51.100.77, 127.0.0.1",
		"Forwarded":       `for=198.51.100.77`,
		"X-Real-Ip":       "198.51.100.77",
	})
	defer drain(resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("proxied GET = %d, want 200", resp.StatusCode)
	}
	xff, fwd, real := rec.seen()
	if strings.Contains(xff, "198.51.100.77") || strings.Contains(fwd, "198.51.100.77") || strings.Contains(real, "198.51.100.77") {
		t.Fatalf("client-supplied address reached the runtime: xff=%q forwarded=%q x-real-ip=%q", xff, fwd, real)
	}
	if xff != "127.0.0.1" {
		t.Fatalf("xff = %q, want the verified socket peer only", xff)
	}
}

// TestProxy_XFFFromTrustedChainOnly: with the peer inside trusted-proxies
// the runtime learns the right-most untrusted chain entry plus the peer —
// deeper (spoofable) claims are dropped.
func TestProxy_XFFFromTrustedChainOnly(t *testing.T) {
	fb := newFakeBroker(t)
	rec := newFwdRecorder(t)
	srv, cookie := launchOn(t, fb, rec, func(c *gateway.Config) {
		c.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}
	})

	resp := proxied(t, srv, testHost, "/", cookie, map[string]string{
		// As an ingress would send it: spoofable client claims on the
		// left, the verified client as the last appended hop.
		"X-Forwarded-For": "6.6.6.6, 198.51.100.9",
		"Forwarded":       `for=6.6.6.6`,
	})
	defer drain(resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("proxied GET = %d, want 200", resp.StatusCode)
	}
	xff, fwd, _ := rec.seen()
	if strings.Contains(xff, "6.6.6.6") || strings.Contains(fwd, "6.6.6.6") {
		t.Fatalf("spoofed deep chain entry reached the runtime: xff=%q forwarded=%q", xff, fwd)
	}
	if xff != "198.51.100.9, 127.0.0.1" {
		t.Fatalf("xff = %q, want \"198.51.100.9, 127.0.0.1\" (client + trusted peer)", xff)
	}
	if fwd != "for=198.51.100.9, for=127.0.0.1" {
		t.Fatalf("forwarded = %q, want derived chain only", fwd)
	}
}

// TestProxy_NoXFFAtAll: a request with no forwarding headers still lets the
// runtime identify the client — the socket peer.
func TestProxy_NoXFFAtAll(t *testing.T) {
	fb := newFakeBroker(t)
	rec := newFwdRecorder(t)
	srv, cookie := launchOn(t, fb, rec, nil)

	resp := proxied(t, srv, testHost, "/", cookie, nil)
	defer drain(resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("proxied GET = %d, want 200", resp.StatusCode)
	}
	xff, fwd, real := rec.seen()
	if xff != "127.0.0.1" || real != "127.0.0.1" {
		t.Fatalf("xff=%q x-real-ip=%q, want the socket peer", xff, real)
	}
	if fwd != "for=127.0.0.1" {
		t.Fatalf("forwarded = %q, want for=127.0.0.1", fwd)
	}
}
