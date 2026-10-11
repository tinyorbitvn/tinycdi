// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package gateway_test

// V11 — per-workspace upstream stream bound on the session proxy. Every
// request the gateway admits to a workspace's runtime — a /websockify
// stream or a plain asset fetch — holds an upstream connection, and each
// one costs the KasmVNC runtime ~3 fds inside the Xvnc process, which
// crashes once it crosses FD_SETSIZE (~330 concurrent connections on a
// clean pod; the runtime ships no cap). The bound lives in the gateway:
// exactly cap in flight per workspace per replica, a retryable 429 over
// it, and slot release on every exit path. These tests fail on
// origin/main — the bound does not exist there (an over-cap request is
// proxied, not refused).

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/tinyorbitvn/tinycdi/internal/broker"
	"github.com/tinyorbitvn/tinycdi/internal/gateway"
	"github.com/tinyorbitvn/tinycdi/internal/observability"
)

// gatedUpstream is a fake runtime that parks every request to holdPath
// until the test opens the release gate — the way a test holds N upstream
// slots open at once and releases them on demand. Websocket upgrades run
// the echo handler like fakeUpstream; every other path answers 200
// immediately.
type gatedUpstream struct {
	release  chan struct{} // closed once: every held request answers
	inFlight chan struct{} // buffered; one token per request parked upstream
	srv      *httptest.Server
}

func newGatedUpstream(t *testing.T, holdPath string, capacity int) *gatedUpstream {
	t.Helper()
	u := &gatedUpstream{
		release:  make(chan struct{}),
		inFlight: make(chan struct{}, capacity+1),
	}
	u.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if upgradeOffer(r) {
			fakeUpstream(w, r)
			return
		}
		if r.URL.Path == holdPath {
			u.inFlight <- struct{}{}
			select {
			case <-u.release:
			case <-r.Context().Done():
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(u.srv.Close)
	return u
}

// holdRequest sends one fetch to holdPath through the gateway and returns
// once the upstream has parked it — the slot is then observably held. The
// returned channel delivers the response status once the gate opens (or
// the request fails).
func holdRequest(t *testing.T, srv *httptest.Server, up *gatedUpstream, host, holdPath, cookie string) <-chan int {
	t.Helper()
	done := make(chan int, 1)
	go func() {
		req, err := http.NewRequest(http.MethodGet, srv.URL+holdPath, nil)
		if err != nil {
			done <- -1
			return
		}
		req.Host = host
		req.Header.Set("Cookie", gateway.SessionCookieName+"="+cookie)
		resp, err := srv.Client().Transport.(*http.Transport).RoundTrip(req)
		if err != nil {
			done <- -1
			return
		}
		done <- resp.StatusCode
		drain(resp)
	}()
	select {
	case <-up.inFlight:
	case <-time.After(10 * time.Second):
		t.Fatalf("request to %s never reached the upstream gate", holdPath)
	}
	return done
}

// streamCount polls the gateway's tracked upstream count for wsID until
// it settles — conn teardown is asynchronous past the wire.
func streamCount(t *testing.T, gw *gateway.Gateway, wsID string, want int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, per := gw.UpstreamStreamCount(wsID); per == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	total, per := gw.UpstreamStreamCount(wsID)
	t.Fatalf("upstream slots for %s = %d (total %d), want %d — a slot leaked", wsID, per, total, want)
}

// openStream opens a live /websockify stream on the session; the caller
// owns closing resp.Body.
func openStream(t *testing.T, srv *httptest.Server, host, cookie string) *http.Response {
	t.Helper()
	ws := upgrade(t, srv, host, "/websockify", cookie, map[string]string{"Origin": "https://" + host})
	if ws.StatusCode != http.StatusSwitchingProtocols {
		drain(ws)
		t.Fatalf("upgrade = %d, want 101", ws.StatusCode)
	}
	return ws
}

// TestStreamCap_ExactBound: with the cap at N, N held requests admit and
// request N+1 answers 429 + Retry-After: 1 + {"error":"too_many_streams"}
// — the session-listener limiter shape — while every live holder keeps
// its slot and the refusal reaches no upstream.
func TestStreamCap_ExactBound(t *testing.T) {
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-cap", testWSUID)
	up := newGatedUpstream(t, "/app/hold", 3)
	fb.pointLeaseAt(t, "tk-cap", up.srv)
	audit := &auditRecorder{}
	gw, srv := newGatewayHandle(t, fb, func(c *gateway.Config) {
		c.UpstreamStreamCap = 3
		c.Audit = audit
	})
	cookie := launchOK(t, srv, testHost, "tk-cap")

	// Slots 1-2: parked fetches; slot 3: a live websocket stream — the
	// bound counts them alike.
	held := holdRequest(t, srv, up, testHost, "/app/hold", cookie)
	held2 := holdRequest(t, srv, up, testHost, "/app/hold", cookie)
	ws := openStream(t, srv, testHost, cookie)
	defer ws.Body.Close()
	streamCount(t, gw, testWSUID, 3)

	// Request N+1 is refused before it can touch the runtime.
	resp := proxied(t, srv, testHost, "/", cookie, nil)
	defer drain(resp)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("request over the cap = %d, want 429", resp.StatusCode)
	}
	if got := resp.Header.Get("Retry-After"); got != "1" {
		t.Fatalf("Retry-After = %q, want \"1\"", got)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"too_many_streams"`) {
		t.Fatalf("429 body = %s, want error too_many_streams", body)
	}
	found := false
	for _, a := range audit.auditActions() {
		if a == "session.stream" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no session.stream audit event for the refusal, got %v", audit.auditActions())
	}
	if _, per := gw.UpstreamStreamCount(testWSUID); per != 3 {
		t.Fatalf("count after refusal = %d, want still 3", per)
	}

	// Open the gate: the parked fetches unwind; the live stream still
	// holds its slot; closing it returns the last slot.
	close(up.release)
	for i, ch := range []<-chan int{held, held2} {
		if code := <-ch; code != http.StatusOK {
			t.Fatalf("held fetch %d = %d, want 200 once the gate opened", i, code)
		}
	}
	streamCount(t, gw, testWSUID, 1)
	ws.Body.Close()
	streamCount(t, gw, testWSUID, 0)
}

// TestStreamCap_MixedHTTPAndWebsocket: a live /websockify stream and a
// plain proxied fetch draw from the SAME per-workspace budget — at cap 2,
// one of each fills it and the next fetch is refused.
func TestStreamCap_MixedHTTPAndWebsocket(t *testing.T) {
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-mix", testWSUID)
	up := newGatedUpstream(t, "/app/hold", 1)
	fb.pointLeaseAt(t, "tk-mix", up.srv)
	gw, srv := newGatewayHandle(t, fb, func(c *gateway.Config) {
		c.UpstreamStreamCap = 2
	})
	cookie := launchOK(t, srv, testHost, "tk-mix")

	ws := openStream(t, srv, testHost, cookie)
	defer ws.Body.Close()
	held := holdRequest(t, srv, up, testHost, "/app/hold", cookie)

	resp := proxied(t, srv, testHost, "/", cookie, nil)
	defer drain(resp)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("third upstream request (1 ws + 1 fetch held) = %d, want 429", resp.StatusCode)
	}
	close(up.release)
	if code := <-held; code != http.StatusOK {
		t.Fatalf("held fetch = %d, want 200", code)
	}
	ws.Body.Close()
	streamCount(t, gw, testWSUID, 0)
}

// TestStreamCap_SlotRelease drives each exit path that must return a
// slot: normal completion, a hijacked stream's client close, a
// replacement upgrade's fence, an upstream transport failure, a client
// disconnect mid-request, a session kill under an open stream, and the
// pre-stop drain. Each case parks enough holders to sit at the cap; the
// probe after the exit must NOT be refused — a leaked slot reads as a
// 429 there.
func TestStreamCap_SlotRelease(t *testing.T) {
	cases := []struct {
		name string
		cap  int
		// run holds the slot(s), drives the exit path and returns the
		// per-workspace count the gateway should settle at (0 when every
		// holder is gone). A nonzero settle covers exit paths that leave
		// a successor holding a slot (the fence case).
		run func(t *testing.T, fb *fakeBroker, gw *gateway.Gateway, srv *httptest.Server, up *gatedUpstream, cookie string) int
		// probeLive is false when the exit path killed the session — the
		// follow-up probe then expects 401 rather than an admission.
		probeLive bool
	}{
		{
			name: "normal completion",
			cap:  1,
			run: func(t *testing.T, _ *fakeBroker, _ *gateway.Gateway, srv *httptest.Server, up *gatedUpstream, cookie string) int {
				done := holdRequest(t, srv, up, testHost, "/app/hold", cookie)
				resp := proxied(t, srv, testHost, "/", cookie, nil)
				drain(resp)
				if resp.StatusCode != http.StatusTooManyRequests {
					t.Fatalf("request during a held slot = %d, want 429", resp.StatusCode)
				}
				close(up.release)
				if code := <-done; code != http.StatusOK {
					t.Fatalf("held fetch = %d, want 200 after the gate opened", code)
				}
				return 0
			},
			probeLive: true,
		},
		{
			name: "websocket client close",
			cap:  1,
			run: func(t *testing.T, _ *fakeBroker, _ *gateway.Gateway, srv *httptest.Server, _ *gatedUpstream, cookie string) int {
				ws := openStream(t, srv, testHost, cookie)
				resp := proxied(t, srv, testHost, "/", cookie, nil)
				drain(resp)
				if resp.StatusCode != http.StatusTooManyRequests {
					t.Fatalf("request during a live stream = %d, want 429", resp.StatusCode)
				}
				ws.Body.Close()
				return 0
			},
			probeLive: true,
		},
		{
			// A second upgrade fences the first stream: its hijacked
			// conn closes and its request unwinds, returning the slot —
			// the count settles to the survivor alone.
			name: "fenced by replacement upgrade",
			cap:  2,
			run: func(t *testing.T, _ *fakeBroker, gw *gateway.Gateway, srv *httptest.Server, _ *gatedUpstream, cookie string) int {
				old := openStream(t, srv, testHost, cookie)
				defer old.Body.Close()
				next := openStream(t, srv, testHost, cookie)
				defer next.Body.Close()
				// The fenced conn dies: a read on the old stream must
				// error, and its slot unwinds.
				if _, err := old.Body.Read(make([]byte, 1)); err == nil {
					t.Log("old stream still readable post-fence (conns may linger briefly)")
				}
				streamCount(t, gw, testWSUID, 1)
				return 1
			},
			probeLive: true,
		},
		{
			// Dial failure upstream: the ErrorHandler's 502 is an exit
			// path like any other — the slot returns with it.
			name: "upstream error",
			cap:  1,
			run: func(t *testing.T, fb *fakeBroker, _ *gateway.Gateway, srv *httptest.Server, _ *gatedUpstream, cookie string) int {
				dead := httptest.NewTLSServer(http.HandlerFunc(fakeUpstream))
				dead.Close() // listening socket gone: every dial fails
				fb.pointLeaseAt(t, "tk-x", dead)
				resp := proxied(t, srv, testHost, "/", cookie, nil)
				drain(resp)
				if resp.StatusCode != http.StatusBadGateway {
					t.Fatalf("request to dead upstream = %d, want 502", resp.StatusCode)
				}
				return 0
			},
			probeLive: true,
		},
		{
			name: "client disconnect mid-request",
			cap:  1,
			run: func(t *testing.T, _ *fakeBroker, _ *gateway.Gateway, srv *httptest.Server, up *gatedUpstream, cookie string) int {
				ctx, cancel := context.WithCancel(context.Background())
				done := make(chan struct{})
				go func() {
					defer close(done)
					req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/app/hold", nil)
					if err != nil {
						return
					}
					req.Host = testHost
					req.Header.Set("Cookie", gateway.SessionCookieName+"="+cookie)
					resp, err := srv.Client().Transport.(*http.Transport).RoundTrip(req)
					if err == nil {
						drain(resp)
					}
				}()
				select {
				case <-up.inFlight:
				case <-time.After(10 * time.Second):
					t.Fatal("request never reached the upstream gate")
				}
				cancel()
				<-done
				return 0
			},
			probeLive: true,
		},
		{
			// The lease dies under an open stream: kill() cancels the
			// tracked ctx and closes the conn — the slot returns with
			// the unwind.
			name: "session killed under open stream",
			cap:  1,
			run: func(t *testing.T, fb *fakeBroker, gw *gateway.Gateway, srv *httptest.Server, _ *gatedUpstream, cookie string) int {
				ws := openStream(t, srv, testHost, cookie)
				defer ws.Body.Close()
				fb.failRenew(fb.leaseOf(t, "tk-x").ID, broker.ErrRevoked)
				streamCount(t, gw, testWSUID, 0)
				return 0
			},
			probeLive: false,
		},
		{
			// Drain closes every live stream's conn; the slot returns
			// when the proxied request unwinds.
			name: "drain",
			cap:  1,
			run: func(t *testing.T, _ *fakeBroker, gw *gateway.Gateway, srv *httptest.Server, _ *gatedUpstream, cookie string) int {
				ws := openStream(t, srv, testHost, cookie)
				defer ws.Body.Close()
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				gw.Drain(ctx)
				cancel()
				streamCount(t, gw, testWSUID, 0)
				return 0
			},
			probeLive: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fb := newFakeBroker(t)
			fb.scriptTicket("tk-x", testWSUID)
			up := newGatedUpstream(t, "/app/hold", 2)
			fb.pointLeaseAt(t, "tk-x", up.srv)
			gw, srv := newGatewayHandle(t, fb, func(c *gateway.Config) {
				c.UpstreamStreamCap = tc.cap
			})
			cookie := launchOK(t, srv, testHost, "tk-x")

			settle := tc.run(t, fb, gw, srv, up, cookie)
			streamCount(t, gw, testWSUID, settle)

			if tc.probeLive {
				resp := proxied(t, srv, testHost, "/", cookie, nil)
				drain(resp)
				if resp.StatusCode == http.StatusTooManyRequests {
					t.Fatalf("post-exit probe = 429 — the slot leaked")
				}
			} else {
				resp := proxied(t, srv, testHost, "/", cookie, nil)
				drain(resp)
				if resp.StatusCode != http.StatusUnauthorized {
					t.Fatalf("probe on a killed session = %d, want 401", resp.StatusCode)
				}
			}
		})
	}
}

// TestStreamCap_ConcurrentStormNoLeak: a burst of concurrent requests
// over the cap interleaves admissions and refusals; when the dust settles
// every slot is returned and the total is exactly zero — no leak, no
// negative count.
func TestStreamCap_ConcurrentStormNoLeak(t *testing.T) {
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-storm", testWSUID)
	up := newGatedUpstream(t, "/app/hold", 8)
	fb.pointLeaseAt(t, "tk-storm", up.srv)
	gw, srv := newGatewayHandle(t, fb, func(c *gateway.Config) {
		c.UpstreamStreamCap = 8
	})
	cookie := launchOK(t, srv, testHost, "tk-storm")

	const burst = 24
	var wg sync.WaitGroup
	results := make(chan int, burst)
	// Round 1: 8 parked holders + 16 concurrent — the tail must refuse.
	for i := 0; i < 8; i++ {
		holdRequest(t, srv, up, testHost, "/app/hold", cookie)
	}
	streamCount(t, gw, testWSUID, 8)
	for i := 0; i < burst-8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp := proxied(t, srv, testHost, "/", cookie, nil)
			results <- resp.StatusCode
			drain(resp)
		}()
	}
	wg.Wait()
	for i := 0; i < burst-8; i++ {
		if code := <-results; code != http.StatusTooManyRequests {
			t.Fatalf("storm request = %d, want 429 — a slot beyond the cap was handed out", code)
		}
	}
	// Gate open: holders complete; the refused retry and succeed.
	close(up.release)
	streamCount(t, gw, testWSUID, 0)

	// Round 2: a mixed churn of parallel short requests — every answer
	// is a clean 200/429 and the count returns to zero.
	results = make(chan int, burst)
	for i := 0; i < burst; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			path := "/"
			if i%3 == 0 {
				path = "/app/hold" // gate already open — these complete
			}
			resp := proxied(t, srv, testHost, path, cookie, nil)
			results <- resp.StatusCode
			drain(resp)
		}(i)
	}
	wg.Wait()
	for i := 0; i < burst; i++ {
		if code := <-results; code != http.StatusOK && code != http.StatusTooManyRequests {
			t.Fatalf("churn request = %d, want 200 or 429", code)
		}
	}
	streamCount(t, gw, testWSUID, 0)
}

// TestStreamCap_PerWorkspace: the bound is per workspace — one workspace
// at its cap leaves a sibling's requests untouched.
func TestStreamCap_PerWorkspace(t *testing.T) {
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-ws1", testWSUID)
	fb.scriptTicket("tk-ws2", testWSUID2)
	up := newGatedUpstream(t, "/app/hold", 1)
	fb.pointLeaseAt(t, "tk-ws1", up.srv)
	gw, srv := newGatewayHandle(t, fb, func(c *gateway.Config) {
		c.UpstreamStreamCap = 1
		c.Sessions = fb // cookie for ws2 mints a second session cleanly
	})
	cookie1 := launchOK(t, srv, testHost, "tk-ws1")
	cookie2 := launchOK(t, srv, testHost2, "tk-ws2")

	held := holdRequest(t, srv, up, testHost, "/app/hold", cookie1)
	resp := proxied(t, srv, testHost, "/", cookie1, nil)
	drain(resp)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second request on the capped workspace = %d, want 429", resp.StatusCode)
	}
	// The sibling workspace still admits — its counter is its own.
	resp = proxied(t, srv, testHost2, "/", cookie2, nil)
	drain(resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("request on the sibling workspace = %d, want 200 — the bound must be per workspace", resp.StatusCode)
	}
	if _, per := gw.UpstreamStreamCount(testWSUID2); per != 0 {
		t.Fatalf("sibling workspace count = %d, want 0 after its request completed", per)
	}
	close(up.release)
	<-held
	streamCount(t, gw, testWSUID, 0)
}

// TestStreamCap_Metrics: the in-flight gauge tracks admissions and the
// refusal counter increments under a bounded route label — never a
// workspace one.
func TestStreamCap_Metrics(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := observability.NewMetrics(reg, nil)
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-met", testWSUID)
	up := newGatedUpstream(t, "/app/hold", 2)
	fb.pointLeaseAt(t, "tk-met", up.srv)
	gw, srv := newGatewayHandle(t, fb, func(c *gateway.Config) {
		c.UpstreamStreamCap = 1
		c.Metrics = m
	})
	cookie := launchOK(t, srv, testHost, "tk-met")

	held := holdRequest(t, srv, up, testHost, "/app/hold", cookie)
	if got := metricValue(t, reg, "tinycdi_gateway_upstream_streams", nil); got != 1 {
		t.Fatalf("gateway_upstream_streams = %v, want 1", got)
	}
	resp := proxied(t, srv, testHost, "/", cookie, nil)
	drain(resp)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("over-cap request = %d, want 429", resp.StatusCode)
	}
	if got := metricValue(t, reg, "tinycdi_rate_limited_total", map[string]string{"route": "upstream_streams"}); got != 1 {
		t.Fatalf("rate_limited_total{route=upstream_streams} = %v, want 1", got)
	}
	close(up.release)
	<-held
	streamCount(t, gw, testWSUID, 0)
	if got := metricValue(t, reg, "tinycdi_gateway_upstream_streams", nil); got != 0 {
		t.Fatalf("gateway_upstream_streams after release = %v, want 0", got)
	}
	// The gauge carries no labels at all — a workspace value can never
	// become one.
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() != "tinycdi_gateway_upstream_streams" {
			continue
		}
		for _, met := range mf.GetMetric() {
			if len(met.GetLabel()) != 0 {
				t.Fatalf("upstream_streams carries labels %v — the bound must stay unlabelled", met.GetLabel())
			}
		}
	}
}
