package gateway_test

// Fuzz targets for the session listener's parsing boundaries (B6-FUZZ):
// the Host-header classification that routes every request, and the
// /v1/launch ticket-redemption parse. Both run ServeHTTP on an in-process
// gateway backed by an in-memory digest store — no sockets, no upstream.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/broker"
	"github.com/tinyorbitvn/tinycdi/internal/gateway"
)

// newFuzzBroker builds the in-memory broker/directory without the TLS
// upstream helper (fuzz paths never resolve a target).
func newFuzzBroker() *fakeBroker {
	return &fakeBroker{
		leases:    map[string]broker.Lease{},
		redeemed:  map[string]bool{},
		renewErr:  map[string]error{},
		resolveT:  map[string]broker.Target{},
		revokedAt: map[string]bool{},
		revokes:   map[string]int{},
		digests:   map[broker.SessionDigest]string{},
		epochs:    map[string]uint64{},
		ownerTabs: map[string]string{},
		renewBy:   map[string]int{},
	}
}

// ticketDigest mirrors the broker's ticketHash lookup: the store key is
// the SHA-256 of the opaque ticket, never the ticket itself.
func ticketDigest(opaque string) string {
	sum := sha256.Sum256([]byte(opaque))
	return hex.EncodeToString(sum[:])
}

// digestBroker hashes the presented ticket before the in-memory lookup so
// the redemption path exercises the same digest-keyed read the real
// broker runs.
type digestBroker struct{ *fakeBroker }

func (d *digestBroker) RedeemTicket(ctx context.Context, gw broker.GatewayIdentity, opaque string) (broker.Lease, error) {
	return d.fakeBroker.RedeemTicket(ctx, gw, ticketDigest(opaque))
}

// scriptTicketHash scripts a ticket redeemable into a lease for wsUID,
// keyed by digest exactly like the broker's launch_ticket.ticket_hash.
func (f *fakeBroker) scriptTicketHash(ticket, wsUID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.leases[ticketDigest(ticket)] = broker.Lease{
		ID:                "lease-" + ticketDigest(ticket)[:12],
		WorkspaceUID:      wsUID,
		TenantID:          "tenant-a",
		PrincipalSubject:  "iss|alice",
		RuntimeGeneration: 1,
		RuntimeUID:        "rt-1",
		FencingVersion:    1,
		GatewayID:         "gw-test",
		ExpiresAt:         time.Now().Add(broker.LeaseTTL),
	}
}

func (f *fakeBroker) redeemedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.redeemed)
}

// newFuzzGateway builds a Gateway handler on the test session domain with
// the fake broker as both Broker and session directory. RenewInterval is
// stretched so sessions minted by fuzz hits park their loops.
func newFuzzGateway(f *testing.F, fb *fakeBroker) *gateway.Gateway {
	f.Helper()
	g, err := gateway.New(gateway.Config{
		Identity:       broker.GatewayIdentity{ID: "gw-test", Audience: testDomain},
		SessionDomain:  testSessionDomain,
		ControlHosts:   []string{testControlHost},
		ControlToken:   "control-test-token",
		Broker:         &digestBroker{fb},
		Sessions:       fb,
		RenewInterval:  time.Hour,
		RevokeDeadline: time.Hour,
		Now:            time.Now,
	})
	if err != nil {
		f.Fatalf("gateway.New: %v", err)
	}
	f.Cleanup(g.Close)
	return g
}

// fuzzRequest builds a request for ServeHTTP without a socket, applying
// the same url.ParseRequestURI net/http applies on the wire. A target
// the wire parser would reject is reported so the fuzz body skips it —
// such a request can never reach ServeHTTP in production either.
func fuzzRequest(method, host, requestURI, body string) (*http.Request, bool) {
	u, err := url.ParseRequestURI(requestURI)
	if err != nil {
		return nil, false
	}
	return &http.Request{
		Method:        method,
		URL:           u,
		Host:          host,
		RequestURI:    requestURI,
		Header:        http.Header{},
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
		RemoteAddr:    "198.51.100.9:51000",
	}, true
}

// controlHostExpect replicates the control-host check so the fuzz body
// asserts the OBSERVABLE contract instead of internals.
func controlHostExpect(host string) bool {
	h, _, err := net.SplitHostPort(host)
	if err != nil {
		h = host
	}
	return h == testControlHost
}

// FuzzHostClassify feeds arbitrary Host headers and request paths through
// ServeHTTP. Invariants: classification is total (every input produces a
// bounded status) and deterministic; host classes behave per contract —
// control paths exist only on the control host, /v1/launch and proxied
// routes require a host under the session domain, an unmatched host
// always answers 421, and /metrics never exists on this listener.
func FuzzHostClassify(f *testing.F) {
	fb := newFuzzBroker()
	g := newFuzzGateway(f, fb)
	for _, c := range [][2]string{
		{testHost, "/"},
		{testHost, "/v1/launch"},
		{testHost, "/websockify"},
		{testHost + ":443", "/vnc.html"},
		{"WS-0000000A." + strings.ToUpper(testDomain), "/"},
		{testHost + ".", "/"},
		{testControlHost, "/healthz"},
		{testControlHost, "/v1/control/session"},
		{testControlHost + ":8444", "/healthz"},
		{testHost, "/healthz"},
		{"evil.com", "/healthz"},
		{"evil.com", "/"},
		{testHost, "/metrics"},
		{testControlHost, "/metrics"},
		{"[::1]", "/healthz"},
		{"[::1]:8444", "/"},
		{"ws-0000000a.xn--ssion-qta.example", "/"},
		{"", "/"},
		{"host with spaces", "/"},
		{testHost, "/v1/control/../x"},
		{testHost, "/app/../../etc"},
		{testHost, "/%2e%2e/"},
		{testHost, "/a;b"},
		{testHost, "/a\\b"},
		{"a:", "/"},
		{testHost + ":0", "/"},
		{testHost + ":99999", "/"},
		{testHost + ":443:443", "/"},
	} {
		f.Add(c[0], c[1])
	}
	f.Fuzz(func(t *testing.T, host, path string) {
		// Wire-realistic bounds (net/http caps the whole header block at
		// 1 MiB; real Host headers and request paths are far smaller);
		// mutator-grown giants can't arrive and only stall the run.
		if len(host) > 64<<10 || len(path) > 64<<10 {
			return
		}
		req, ok := fuzzRequest(http.MethodGet, host, path, "")
		if !ok {
			return
		}
		rec := httptest.NewRecorder()
		g.ServeHTTP(rec, req)
		code := rec.Code

		_, wsOK := testSessionDomain.Match(host)
		controlOK := controlHostExpect(host)

		var wantMin, wantMax int // expected status, as an inclusive range
		switch {
		case req.URL.Path == "/metrics":
			wantMin, wantMax = 404, 404
		case req.URL.Path == gateway.LaunchPath:
			if wsOK {
				wantMin, wantMax = 405, 405 // GET: launch validation ran
			} else {
				wantMin, wantMax = 421, 421
			}
		case req.URL.Path == "/healthz":
			if controlOK {
				wantMin, wantMax = 200, 200
			} else {
				wantMin, wantMax = 404, 404
			}
		case strings.HasPrefix(req.URL.Path, "/v1/control/"):
			if controlOK {
				wantMin, wantMax = 401, 401 // control surface closed w/o bearer
			} else {
				wantMin, wantMax = 404, 404
			}
		default:
			if wsOK {
				// Reached serveProxy: path allowlist, upgrade and
				// session-cookie validation — never a redirect or proxy
				// result without a live session.
				wantMin, wantMax = 400, 404
			} else {
				wantMin, wantMax = 421, 421
			}
		}
		if code < wantMin || code > wantMax {
			t.Fatalf("Host=%q path=%q status=%d, want %d..%d (wsOK=%v controlOK=%v)",
				host, path, code, wantMin, wantMax, wsOK, controlOK)
		}

		// Determinism: an identical request classifies identically. The
		// only state a GET can mutate is a session map — nothing can
		// mint a session without a launch POST.
		req2, _ := fuzzRequest(http.MethodGet, host, path, "")
		rec2 := httptest.NewRecorder()
		g.ServeHTTP(rec2, req2)
		if rec2.Code != code {
			t.Fatalf("Host=%q path=%q not deterministic: %d then %d", host, path, code, rec2.Code)
		}
		if _, _, _, inflight := g.SessionMapCounts(); inflight != 0 {
			t.Fatalf("inflight lookups leaked: %d", inflight)
		}
	})
}

// FuzzLaunchRedeem feeds arbitrary launch requests through ServeHTTP on a
// session host: method, query string (ticket-in-query must 403), body
// (form parse edges, oversized tickets, dup fields), Origin and
// Sec-Fetch-Site. Invariants: status is bounded to the contract set; a
// 303 implies a scripted ticket for THIS host's workspace plus a
// __Host-session cookie and a Location that never leaks the ticket; a
// redeemed ticket never redeems twice; nothing mints a session without a
// 303.
func FuzzLaunchRedeem(f *testing.F) {
	fb := newFuzzBroker()
	g := newFuzzGateway(f, fb)
	const (
		validTicket   = "tk-fuzz-valid"   // scripted for testWSUID
		foreignTicket = "tk-fuzz-foreign" // scripted for testWSUID2 (host mismatch)
		expiredTicket = "tk-fuzz-expired" // pre-consumed in the store
	)
	fb.scriptTicketHash(validTicket, testWSUID)
	fb.scriptTicketHash(foreignTicket, testWSUID2)
	fb.redeemed[ticketDigest(expiredTicket)] = true

	originOK := "https://" + testHost
	for _, c := range [][5]string{
		// host, method, query, body, origin
		{testHost, "POST", "", "ticket=" + validTicket, originOK},
		{testHost, "POST", "", "ticket=" + foreignTicket, originOK},
		{testHost, "POST", "", "ticket=" + expiredTicket, originOK},
		{testHost, "POST", "", "ticket=unknown-ticket", originOK},
		{testHost, "POST", "", "", originOK},
		{testHost, "POST", "ticket=" + validTicket, "ticket=" + validTicket, originOK},
		{testHost, "GET", "", "ticket=" + validTicket, originOK},
		{testHost, "POST", "", "ticket=%zz", originOK},
		{testHost, "POST", "", "ticket=a&ticket=b", originOK},
		{testHost, "POST", "", "ticket=" + validTicket + "&x=1", ""},
		{testHost, "POST", "", "ticket=" + validTicket, "null"},
		{testHost, "POST", "", "ticket=" + validTicket, "https://evil.example"},
		{testHost, "POST", "", "ticket=" + validTicket + strings.Repeat("A", 9000), originOK},
		{testHost, "POST", "", "ticket", originOK},
		{testHost, "POST", "", "notaform%%%%", originOK},
		{"evil.com", "POST", "", "ticket=" + validTicket, originOK},
		{testHost2, "POST", "", "ticket=" + validTicket, "https://" + testHost2},
		{testHost, "PUT", "", "ticket=" + validTicket, originOK},
		{testHost, "POST", "", strings.Repeat("ticket=x&", 500), originOK},
	} {
		f.Add(c[0], c[1], c[2], c[3], c[4])
	}
	f.Fuzz(func(t *testing.T, host, method, rawQuery, body, origin string) {
		// Wire-realistic bounds: the header block is capped at 1 MiB by
		// net/http and the handler itself reads at most maxLaunchBody
		// bytes — mutator-grown giants can't arrive and only stall the
		// run's minimization rounds.
		if len(host) > 64<<10 || len(method) > 1<<10 || len(rawQuery) > 64<<10 ||
			len(body) > 64<<10 || len(origin) > 64<<10 {
			return
		}
		target := gateway.LaunchPath
		if rawQuery != "" {
			target += "?" + rawQuery
		}
		req, ok := fuzzRequest(method, host, target, body)
		if !ok {
			return
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if origin != "" {
			req.Header.Set("Origin", origin)
		}

		redeemedBefore := fb.redeemedCount()
		mintsBefore := gateway.SessionMints()
		rec := httptest.NewRecorder()
		g.ServeHTTP(rec, req)
		code := rec.Code

		switch code {
		case 303, 400, 401, 403, 405, 421:
		default:
			t.Fatalf("launch status = %d outside the contract set", code)
		}

		_, wsOK := testSessionDomain.Match(host)
		if !wsOK && code != 421 {
			t.Fatalf("launch on unmatched host %q = %d, want 421", host, code)
		}
		if code == 303 {
			// Accept implies well-formed: POST, session cookie set, clean
			// redirect carrying no ticket material.
			if method != http.MethodPost {
				t.Fatalf("non-POST method %q redeemed", method)
			}
			var cookie *http.Cookie
			for _, c := range rec.Result().Cookies() {
				if c.Name == gateway.SessionCookieName {
					cookie = c
				}
			}
			if cookie == nil || cookie.Value == "" {
				t.Fatalf("303 without %s cookie", gateway.SessionCookieName)
			}
			loc := rec.Header().Get("Location")
			if !strings.HasPrefix(loc, "/") || strings.Contains(loc, validTicket) ||
				strings.Contains(loc, foreignTicket) {
				t.Fatalf("303 Location %q not a clean path", loc)
			}
			if got := gateway.SessionMints() - mintsBefore; got != 1 {
				t.Fatalf("303 minted %d sessions, want 1", got)
			}
		} else if got := gateway.SessionMints() - mintsBefore; got != 0 {
			t.Fatalf("status %d minted %d sessions", code, got)
		}
		if _, _, _, inflight := g.SessionMapCounts(); inflight != 0 {
			t.Fatalf("inflight lookups leaked: %d", inflight)
		}

		// Replay the identical request: if redemption consumed a ticket
		// the second attempt must fail as invalid_ticket (single use);
		// otherwise the outcome must be identical.
		req2, _ := fuzzRequest(method, host, target, body)
		req2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if origin != "" {
			req2.Header.Set("Origin", origin)
		}
		rec2 := httptest.NewRecorder()
		g.ServeHTTP(rec2, req2)
		if fb.redeemedCount() > redeemedBefore {
			if rec2.Code != http.StatusUnauthorized {
				t.Fatalf("replayed redeemed ticket = %d, want 401", rec2.Code)
			}
		} else if rec2.Code != code {
			t.Fatalf("launch not deterministic: %d then %d", code, rec2.Code)
		}
	})
}
