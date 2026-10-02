package gateway_test

// Activity reporting through the gateway (design §8):
// connected/disconnect ride the interactive stream lifecycle; "input" is
// emitted only when a client->upstream WebSocket binary frame carries an
// RFB KeyEvent/PointerEvent — rate-limited to InputReportInterval.
//
// Synchronisation: these tests never poll a wall-clock
// deadline for broker-visible state — that was the flake. Two barriers
// replace sleeps:
//
//   - activityBroker.sent is a FIFO channel signalled for every event the
//     sender delivered to the broker. Per-session ordering means an event
//     that should never have been reported cannot hide behind a later one:
//     it FIFO-precedes it and inflates the recorded count.
//   - the fake upstream echoes client bytes verbatim, so reading the echo
//     of a written frame proves the gateway already read it off the client
//     conn — the sniffer's feed and noteInput's rate-limit decision for
//     that frame ran inside that Read. A frame whose echo returned has a
//     FINAL limiter decision; the test then only has to observe delivery.
//
// Time itself is driven only by the injected fake clock.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/broker"
	"github.com/tinyorbitvn/tinycdi/internal/gateway"
)

// activityBroker wraps the scripted fake broker and reports every delivered
// activity event on sent — the deterministic "sender delivered" signal.
type activityBroker struct {
	*fakeBroker
	sent chan broker.ActivityEventType
}

func newActivityBroker(t *testing.T) *activityBroker {
	t.Helper()
	return &activityBroker{
		fakeBroker: newFakeBroker(t),
		sent:       make(chan broker.ActivityEventType, 16),
	}
}

func (b *activityBroker) ReportActivity(ctx context.Context, gw broker.GatewayIdentity, leaseID string, fence broker.Fence, ev broker.ActivityEvent) error {
	err := b.fakeBroker.ReportActivity(ctx, gw, leaseID, fence, ev)
	b.sent <- ev.Type
	return err
}

// syncGateway is newGateway over any BrokerClient — the activity tests wrap
// the fake broker with delivery signalling, and newGateway is typed on the
// concrete fake.
func syncGateway(t *testing.T, brk gateway.BrokerClient, mutate func(*gateway.Config)) *httptest.Server {
	t.Helper()
	cfg := gateway.Config{
		Identity:       broker.GatewayIdentity{ID: "gw-test", Audience: testDomain},
		SessionDomain:  testSessionDomain,
		ControlHosts:   []string{testControlHost},
		Broker:         brk,
		ControlToken:   "control-test-token",
		RenewInterval:  25 * time.Millisecond, // fast cadence so revoke tests don't sleep
		RevokeDeadline: 150 * time.Millisecond,
		Now:            time.Now,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	h, err := gateway.New(cfg)
	if err != nil {
		t.Fatalf("gateway.New: %v", err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

// awaitSent consumes exactly len(want) delivered events in order. The
// ceiling only bounds a genuinely broken pipeline, never timing jitter.
func awaitSent(t *testing.T, b *activityBroker, want ...broker.ActivityEventType) {
	t.Helper()
	for i, w := range want {
		select {
		case got := <-b.sent:
			if got != w {
				t.Fatalf("event %d = %q, want sequence %v", i, got, want)
			}
		case <-time.After(30 * time.Second):
			t.Fatalf("waiting for event %d (%q): only %d of %v delivered", i, w, i, want)
		}
	}
}

// awaitNoSent asserts the delivered-event channel is empty right now —
// everything the sender had queued has already been consumed by awaitSent.
func awaitNoSent(t *testing.T, b *activityBroker) {
	t.Helper()
	select {
	case ev := <-b.sent:
		t.Fatalf("unexpected extra activity report %q", ev)
	default:
	}
}

// awaitEcho reads back the echo of written bytes: proof the gateway read
// them off the client conn, so the sniffer evaluated them already.
func awaitEcho(t *testing.T, resp *http.Response, n int) {
	t.Helper()
	wsRead(t, resp, n, 30*time.Second)
}

// wsFrame builds a masked client->server WebSocket frame (real clients
// always mask).
func wsFrame(opcode byte, payload []byte) []byte {
	var b []byte
	b = append(b, 0x80|opcode)
	switch n := len(payload); {
	case n < 126:
		b = append(b, 0x80|byte(n))
	case n < 1<<16:
		b = append(b, 0x80|126, byte(n>>8), byte(n))
	default:
		b = append(b, 0x80|127, 0, 0, 0, 0, byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
	}
	key := []byte{0x5a, 0xa5, 0x5a, 0xa5}
	b = append(b, key...)
	for i, p := range payload {
		b = append(b, p^key[i%4])
	}
	return b
}

// inputs counts recorded input reports so far.
func inputs(fb *fakeBroker) int {
	n := 0
	for _, ty := range fb.activityTypes() {
		if ty == broker.ActivityInput {
			n++
		}
	}
	return n
}

// TestActivity_ConnectDisconnectAndReconnect: stream admission reports
// "connected", stream close reports "disconnect", and a reconnect reports
// a new "connected" — the sequence the broker uses to arm/cancel the
// disconnect grace timer.
func TestActivity_ConnectDisconnectAndReconnect(t *testing.T) {
	fb := newActivityBroker(t)
	fb.scriptTicket("tk-act", testWSUID)
	srv := syncGateway(t, fb, nil)
	cookie := launchOK(t, srv, testHost, "tk-act")

	resp := upgrade(t, srv, testHost, "/websockify", cookie, map[string]string{"Origin": testOrigin})
	if resp.StatusCode != http.StatusSwitchingProtocols {
		drain(resp)
		t.Fatalf("upgrade = %d, want 101", resp.StatusCode)
	}
	awaitSent(t, fb, broker.ActivityConnected)

	resp.Body.Close()
	awaitSent(t, fb, broker.ActivityDisconnect)

	// Reconnect: a fresh upgrade reports connected again — broker-side this
	// cancels the pending disconnect window.
	resp2 := upgrade(t, srv, testHost, "/websockify", cookie, map[string]string{"Origin": testOrigin})
	if resp2.StatusCode != http.StatusSwitchingProtocols {
		drain(resp2)
		t.Fatalf("reconnect upgrade = %d, want 101", resp2.StatusCode)
	}
	defer resp2.Body.Close()
	awaitSent(t, fb, broker.ActivityConnected)
	awaitNoSent(t, fb)
}

// TestActivity_InputReported_RateLimited: RFB KeyEvent/PointerEvent frames
// report "input", at most once per InputReportInterval — the fake clock
// advances the window so a later input reports again.
func TestActivity_InputReported_RateLimited(t *testing.T) {
	fb := newActivityBroker(t)
	fb.scriptTicket("tk-inp", testWSUID)
	clock := &fakeClock{now: time.Now()}
	srv := syncGateway(t, fb, func(c *gateway.Config) {
		c.Now = clock.Now
		// Advancing the fake clock past the input window must not trip the
		// fail-closed deadline (it measures real liveness in production) —
		// pin it well above the advance so only the limiter sees the jump.
		c.RevokeDeadline = time.Hour
	})
	cookie := launchOK(t, srv, testHost, "tk-inp")

	resp := upgrade(t, srv, testHost, "/websockify", cookie, map[string]string{"Origin": testOrigin})
	if resp.StatusCode != http.StatusSwitchingProtocols {
		drain(resp)
		t.Fatalf("upgrade = %d, want 101", resp.StatusCode)
	}
	defer resp.Body.Close()
	awaitSent(t, fb, broker.ActivityConnected)

	key := wsFrame(0x2, []byte{4, 1, 0, 0, 0, 0, 0x61})
	wsWrite(t, resp, key)
	awaitEcho(t, resp, len(key)) // frame evaluated by the limiter
	awaitSent(t, fb, broker.ActivityInput)
	if inputs(fb.fakeBroker) != 1 {
		t.Fatalf("key event -> %d input reports, want 1", inputs(fb.fakeBroker))
	}

	// A second key event inside the window is collapsed, pointer too. The
	// echoes prove both decisions were made; FIFO on sent means a wrongly
	// reported one would surface ahead of any later report.
	key2 := wsFrame(0x2, []byte{4, 1, 0, 0, 0, 0, 0x62})
	ptr := wsFrame(0x2, []byte{5, 1, 0, 5, 0, 5})
	wsWrite(t, resp, key2)
	wsWrite(t, resp, ptr)
	awaitEcho(t, resp, len(key2)+len(ptr))
	if inputs(fb.fakeBroker) != 1 {
		t.Fatalf("in-window inputs -> %d reports, want 1 (rate-limited)", inputs(fb.fakeBroker))
	}

	// Past the interval a new input reports again. Advancing the fake clock
	// past the window makes this frame the sentinel: any wrongly-reported
	// suppressed input would have been delivered before it (FIFO), so the
	// final count is exact.
	clock.Advance(gateway.InputReportInterval + time.Second)
	ptr2 := wsFrame(0x2, []byte{5, 1, 0, 7, 0, 7})
	wsWrite(t, resp, ptr2)
	awaitEcho(t, resp, len(ptr2))
	awaitSent(t, fb, broker.ActivityInput)
	if inputs(fb.fakeBroker) != 2 {
		t.Fatalf("post-window input -> %d reports, want 2", inputs(fb.fakeBroker))
	}
	awaitNoSent(t, fb)
}

// TestActivity_PingsTextVideoIgnored: WS ping/pong frames, text frames and
// non-input binary messages (framebuffer requests, clipboard) never report
// input — and upstream->client video never enters the sniffed direction.
func TestActivity_PingsTextVideoIgnored(t *testing.T) {
	fb := newActivityBroker(t)
	fb.scriptTicket("tk-noninput", testWSUID)
	clock := &fakeClock{now: time.Now()}
	srv := syncGateway(t, fb, func(c *gateway.Config) {
		c.Now = clock.Now
		// Advancing the fake clock past the input window must not trip the
		// fail-closed deadline (it measures real liveness in production) —
		// pin it well above the advance so only the limiter sees the jump.
		c.RevokeDeadline = time.Hour
	})
	cookie := launchOK(t, srv, testHost, "tk-noninput")

	resp := upgrade(t, srv, testHost, "/websockify", cookie, map[string]string{"Origin": testOrigin})
	if resp.StatusCode != http.StatusSwitchingProtocols {
		drain(resp)
		t.Fatalf("upgrade = %d, want 101", resp.StatusCode)
	}
	defer resp.Body.Close()
	awaitSent(t, fb, broker.ActivityConnected)

	var batch []byte
	batch = append(batch, wsFrame(0x9, []byte("ping"))...)                 // ping
	batch = append(batch, wsFrame(0xA, []byte("pong"))...)                 // pong
	batch = append(batch, wsFrame(0x1, []byte{4, 0, 0, 0})...)             // text, not RFB
	batch = append(batch, wsFrame(0x2, []byte{3, 0, 0, 0, 0, 0})...)       // FramebufferUpdateRequest
	batch = append(batch, wsFrame(0x2, []byte{6, 0, 0, 0, 0, 0, 0, 0})...) // ClientCutText
	// upstream->client "video": the echo loop bounces these back; frames on
	// the return leg are never sniffed regardless.
	wsWrite(t, resp, batch)
	awaitEcho(t, resp, len(batch)) // every frame evaluated by the limiter
	if inputs(fb.fakeBroker) != 0 {
		t.Fatalf("non-input traffic -> %d input reports, want 0", inputs(fb.fakeBroker))
	}

	// Sentinel: past the rate window a real input reports exactly once.
	// A wrongly-reported batch frame would both have set the limiter's
	// lastInputReport (it cannot — the clock advanced past the window) and
	// FIFO-precede this report — either way the count must stay exact.
	clock.Advance(gateway.InputReportInterval + time.Second)
	key := wsFrame(0x2, []byte{4, 1, 0, 0, 0, 0, 0x61})
	wsWrite(t, resp, key)
	awaitEcho(t, resp, len(key))
	awaitSent(t, fb, broker.ActivityInput)
	if inputs(fb.fakeBroker) != 1 {
		t.Fatalf("non-input traffic produced a report: got %d inputs, want exactly 1 sentinel",
			inputs(fb.fakeBroker))
	}
	awaitNoSent(t, fb)
}

// TestActivity_KillSessionFlushesDisconnect: when the renew loop
// kills a session (lease revoked server-side), the killed conn's deferred
// "disconnect" report is enqueued AFTER s.done closed. The sender must
// still deliver it inside a bounded flush window instead of dropping it:
// for kills whose lease is still live server-side (renew_deadline,
// broker_unreachable), this report is the broker's only stream-close
// signal — dropping it leaks open_streams and burns the 45 s drain budget.
func TestActivity_KillSessionFlushesDisconnect(t *testing.T) {
	fb := newActivityBroker(t)
	fb.scriptTicket("tk-kill", testWSUID)
	srv := syncGateway(t, fb, nil)
	cookie := launchOK(t, srv, testHost, "tk-kill")

	resp := upgrade(t, srv, testHost, "/websockify", cookie, map[string]string{"Origin": testOrigin})
	if resp.StatusCode != http.StatusSwitchingProtocols {
		drain(resp)
		t.Fatalf("upgrade = %d, want 101", resp.StatusCode)
	}
	defer resp.Body.Close()
	awaitSent(t, fb, broker.ActivityConnected)

	// The next renew tick fails terminally -> killSession closes done and
	// the hijacked conn; the proxy defer then enqueues disconnect.
	fb.failRenew(fb.leaseOf(t, "tk-kill").ID, broker.ErrRevoked)
	awaitSent(t, fb, broker.ActivityDisconnect)
	awaitNoSent(t, fb)
}
