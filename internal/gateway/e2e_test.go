package gateway_test

// End-to-end websocket tests: a real upgrade through the gateway to the fake
// upstream's echo loop, then the two death paths — broker revoke and the
// fail-closed window driven by a fake clock.

import (
	"errors"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/broker"
	"github.com/tinyorbitvn/tinycdi/internal/gateway"
)

// fakeClock is a manually-advanced time source for fail-closed tests.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// wsWrite writes bytes onto the hijacked client side of an upgraded response.
func wsWrite(t *testing.T, resp *http.Response, b []byte) {
	t.Helper()
	w, ok := resp.Body.(io.Writer)
	if !ok {
		t.Fatal("upgraded response body is not writable")
	}
	if _, err := w.Write(b); err != nil {
		t.Fatalf("ws write: %v", err)
	}
}

func wsRead(t *testing.T, resp *http.Response, n int, timeout time.Duration) []byte {
	t.Helper()
	type res struct {
		b   []byte
		err error
	}
	done := make(chan res, 1)
	go func() {
		buf := make([]byte, n)
		m, err := io.ReadFull(resp.Body, buf)
		done <- res{buf[:m], err}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("ws read: %v", r.err)
		}
		return r.b
	case <-time.After(timeout):
		t.Fatal("ws read timed out")
		return nil
	}
}

// TestWS_EndToEnd_Echo: bytes written on the client side of the upgrade must
// round-trip through gateway -> upstream -> back.
func TestWS_EndToEnd_Echo(t *testing.T) {
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-e2e", "ws-1")
	srv := newGateway(t, fb, nil)
	cookie := launchOK(t, srv, "tk-e2e")

	resp := upgrade(t, srv, "/websockify", cookie, map[string]string{"Origin": testOrigin})
	if resp.StatusCode != http.StatusSwitchingProtocols {
		drain(resp)
		t.Fatalf("upgrade = %d, want 101", resp.StatusCode)
	}
	defer resp.Body.Close()

	payload := []byte("rfb-handshake-bytes")
	wsWrite(t, resp, payload)
	got := wsRead(t, resp, len(payload), 2*time.Second)
	if string(got) != string(payload) {
		t.Fatalf("echo mismatch: %q", got)
	}
}

// TestWS_RevokeClosesStream_EndToEnd: broker revoke -> open socket dies
// inside the revoke deadline, and the cookie no longer authorizes.
func TestWS_RevokeClosesStream_EndToEnd(t *testing.T) {
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-e2e-rev", "ws-1")
	srv := newGateway(t, fb, nil)
	cookie := launchOK(t, srv, "tk-e2e-rev")
	lease := fb.leaseOf(t, "tk-e2e-rev")

	resp := upgrade(t, srv, "/websockify", cookie, map[string]string{"Origin": testOrigin})
	if resp.StatusCode != http.StatusSwitchingProtocols {
		drain(resp)
		t.Fatalf("upgrade = %d, want 101", resp.StatusCode)
	}
	defer resp.Body.Close()

	// prove duplex before revoke
	wsWrite(t, resp, []byte("x"))
	wsRead(t, resp, 1, 2*time.Second)

	fb.failRenew(lease.ID, broker.ErrRevoked)

	done := make(chan error, 1)
	go func() {
		_, err := resp.Body.Read(make([]byte, 1))
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("stream stayed readable after revoke")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("open stream survived revoke past deadline")
	}

	post := proxied(t, srv, "/", cookie, map[string]string{"Origin": testOrigin})
	defer drain(post)
	if post.StatusCode != http.StatusUnauthorized {
		t.Fatalf("post-revoke GET = %d, want 401", post.StatusCode)
	}
}

// TestWS_FailClosed_FakeClock: with the broker unreachable (transient renew
// errors), the session must die — and the open socket with it — once no
// successful renew has landed inside RevokeDeadline. Driven by a fake clock
// so the 30 s budget is exercised without sleeping.
func TestWS_FailClosed_FakeClock(t *testing.T) {
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-e2e-fc", "ws-1")
	clock := &fakeClock{now: time.Now()}
	srv := newGateway(t, fb, func(c *gateway.Config) {
		c.Now = clock.Now
		c.RenewInterval = 10 * time.Millisecond
		c.RevokeDeadline = 100 * time.Millisecond // stands in for the 30 s budget
	})
	cookie := launchOK(t, srv, "tk-e2e-fc")
	lease := fb.leaseOf(t, "tk-e2e-fc")

	resp := upgrade(t, srv, "/websockify", cookie, map[string]string{"Origin": testOrigin})
	if resp.StatusCode != http.StatusSwitchingProtocols {
		drain(resp)
		t.Fatalf("upgrade = %d, want 101", resp.StatusCode)
	}
	defer resp.Body.Close()

	// Broker goes silent: renews fail transiently. Session must stay alive
	// while still inside the deadline.
	fb.failRenew(lease.ID, errors.New("broker unreachable"))

	// Still inside the window: stream survives transient failures.
	select {
	case <-time.After(30 * time.Millisecond):
	}
	if _, err := resp.Body.(io.Reader).Read(make([]byte, 0)); err != nil {
		t.Fatalf("stream died during transient window: %v", err)
	}

	// Advance the clock past the fail-closed deadline; the next renew tick
	// must kill the session and close the socket.
	clock.Advance(10 * time.Second)

	done := make(chan error, 1)
	go func() {
		_, err := resp.Body.Read(make([]byte, 1))
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("stream stayed readable past fail-closed deadline")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("fail-closed did not close the stream")
	}
}
