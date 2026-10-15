package gateway_test

import (
	"crypto/sha1"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/broker"
	"github.com/tinyorbitvn/tinycdi/internal/gateway"
)

// testReadBound stands in for the backend session listener's ReadTimeout
// (internal/backend/listeners.go sessionReadTimeout, 30 s in production).
const testReadBound = time.Second

// boundedServer serves h the way the backend session listener does: with a
// whole-request ReadTimeout.
func boundedServer(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(h)
	srv.Config.ReadTimeout = testReadBound
	srv.Config.ReadHeaderTimeout = testReadBound
	srv.Start()
	t.Cleanup(srv.Close)
	return srv
}

// TestSessionListener_AdmittedStreamOutlivesReadBound (R1-05, Review
// Focus 1): an admitted WebSocket stream lives past the listener's read
// bound — net/http clears the conn deadline when the upgrade is hijacked.
func TestSessionListener_AdmittedStreamOutlivesReadBound(t *testing.T) {
	echo := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer c.Close()
		sum := sha1.Sum([]byte(r.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: upgrade\r\n" +
			"Sec-WebSocket-Accept: " + base64.StdEncoding.EncodeToString(sum[:]) + "\r\n\r\n")
		_ = rw.Flush()
		_, _ = io.Copy(c, rw)
	}))
	t.Cleanup(echo.Close)
	fb := newFakeBroker(t)
	fb.scriptTicket("tk-rb-ws", testWSUID)
	fb.pointLeaseAt(t, "tk-rb-ws", echo)
	h, _ := newGatewayHandle(t, fb, nil)
	srv := boundedServer(t, h)
	cookie := launchOK(t, srv, testHost, "tk-rb-ws")

	up := upgrade(t, srv, testHost, "/websockify", cookie, map[string]string{"Origin": testOrigin})
	if up.StatusCode != http.StatusSwitchingProtocols {
		drain(up)
		t.Fatalf("upgrade = %d, want 101", up.StatusCode)
	}
	defer up.Body.Close()
	rwc := up.Body.(io.ReadWriteCloser)
	time.Sleep(3 * testReadBound)
	if _, err := rwc.Write([]byte("late")); err != nil {
		t.Fatalf("write after the read bound: %v", err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(rwc, buf); err != nil || string(buf) != "late" {
		t.Fatalf("echo after the read bound = %q, %v; want the stream alive", buf, err)
	}
}

// slowBody yields n bytes, one per tick, so the whole body takes n*tick.
type slowBody struct {
	n    int
	tick time.Duration
}

func (s *slowBody) Read(p []byte) (int, error) {
	if s.n == 0 {
		return 0, io.EOF
	}
	time.Sleep(s.tick)
	s.n--
	p[0] = 'x'
	return 1, nil
}

// TestSessionListener_TransferUploadOutlivesReadBound (R1-05, Review
// Focus 1): a granted upload whose body takes longer than the listener's
// read bound completes — the transfer leg extends its own deadline after
// the claim.
func TestSessionListener_TransferUploadOutlivesReadBound(t *testing.T) {
	fb := newFakeBroker(t)
	stub := newXferStub(t)
	fb.scriptTicket("tk-rb-xfer", testWSUID)
	lease := fb.leaseOf(t, "tk-rb-xfer")
	tgt := targetFor(stub.srv)
	tgt.XferToken = "xt"
	fb.mu.Lock()
	fb.resolveT[lease.ID] = tgt
	fb.transfers[xfID] = broker.TransferGrant{ID: xfID, WorkspaceUID: testWSUID, Direction: "upload", Path: "slow.bin", MaxBytes: 100}
	fb.mu.Unlock()
	h, _ := newGatewayHandle(t, fb, func(c *gateway.Config) { c.PortalOrigins = []string{portalOrigin} })
	srv := boundedServer(t, h)
	cookie := launchOK(t, srv, testHost, "tk-rb-xfer")

	const size = 10
	req, err := http.NewRequest(http.MethodPut, srv.URL+xfPath, &slowBody{n: size, tick: 3 * testReadBound / size})
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = size
	req.Host = testHost
	req.Header.Set("Cookie", gateway.SessionCookieName+"="+cookie)
	req.Header.Set("Origin", portalOrigin)
	res, err := srv.Client().Transport.(*http.Transport).RoundTrip(req)
	if err != nil {
		t.Fatalf("slow upload: %v", err)
	}
	defer drain(res)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("slow upload = %d, want 200", res.StatusCode)
	}
	if v, _ := stub.file("slow.bin"); len(v) != size {
		t.Fatalf("helper stored %d bytes, want %d", len(v), size)
	}
}
