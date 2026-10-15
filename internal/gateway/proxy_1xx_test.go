package gateway_test

import (
	"crypto/sha1"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/textproto"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/tinyorbitvn/tinycdi/internal/gateway"
)

// hostile1xxUpstream answers every request with an informational response
// carrying hostile headers, then a 200 HTML page — the runtime pod is
// tenant-controlled and may do exactly this.
func hostile1xxUpstream(t *testing.T, code int) *httptest.Server {
	t.Helper()
	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		h := w.Header()
		h.Set("Link", "</app/x.js>; rel=preload; as=script")
		h.Set("Set-Cookie", "planted=1; Path=/")
		h.Set("Content-Security-Policy", "default-src *")
		w.WriteHeader(code)
		h.Del("Link")
		h.Del("Set-Cookie")
		h.Del("Content-Security-Policy")
		h.Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<script>/* runtime page */</script>"))
	}))
	t.Cleanup(up.Close)
	return up
}

// TestResponsePolicy_Upstream1xxCannotStripPinnedHeaders (R1-04): an
// upstream 1xx must neither reach the browser nor erase the security
// headers ServeHTTP pins on every session-origin response.
func TestResponsePolicy_Upstream1xxCannotStripPinnedHeaders(t *testing.T) {
	for _, tc := range []struct {
		name string
		code int
	}{{"103 early hints", http.StatusEarlyHints}, {"100 continue", http.StatusContinue}} {
		t.Run(tc.name, func(t *testing.T) {
			fb := newFakeBroker(t)
			fb.scriptTicket("tk-1xx", testWSUID)
			fb.pointLeaseAt(t, "tk-1xx", hostile1xxUpstream(t, tc.code))
			srv := newGateway(t, fb, nil)
			cookie := launchOK(t, srv, testHost, "tk-1xx")

			req, err := http.NewRequest(http.MethodGet, srv.URL+"/vnc.html", nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Host = testHost
			req.Header.Set("Cookie", gateway.SessionCookieName+"="+cookie)
			var infos atomic.Int32
			req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
				Got1xxResponse: func(int, textproto.MIMEHeader) error { infos.Add(1); return nil },
			}))
			resp, err := srv.Client().Transport.(*http.Transport).RoundTrip(req)
			if err != nil {
				t.Fatalf("roundtrip: %v", err)
			}
			defer drain(resp)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			if n := infos.Load(); n != 0 {
				t.Errorf("client saw %d informational responses, want 0", n)
			}
			csp := resp.Header.Get("Content-Security-Policy")
			if !strings.Contains(csp, "frame-ancestors") || !strings.Contains(csp, "connect-src 'self' wss://"+testHost) {
				t.Errorf("CSP = %q, want the pinned session CSP", csp)
			}
			if got := resp.Header.Values("Content-Security-Policy"); len(got) != 1 {
				t.Errorf("CSP values = %q, want exactly one", got)
			}
			for name, want := range map[string]string{
				"Cross-Origin-Opener-Policy":   "same-origin",
				"Cross-Origin-Resource-Policy": "same-origin",
				"X-Content-Type-Options":       "nosniff",
				"Origin-Agent-Cluster":         "?1",
				"Strict-Transport-Security":    "max-age=63072000; includeSubDomains",
			} {
				if got := resp.Header.Values(name); len(got) != 1 || got[0] != want {
					t.Errorf("%s = %q, want exactly [%q]", name, got, want)
				}
			}
			if pp := resp.Header.Get("Permissions-Policy"); !strings.Contains(pp, "camera=()") {
				t.Errorf("Permissions-Policy = %q, want camera=()", pp)
			}
			for _, h := range []string{"Link", "Set-Cookie"} {
				if v := resp.Header.Get(h); v != "" {
					t.Errorf("%s = %q reached the client", h, v)
				}
			}
		})
	}
}

// TestProxy_WebSocketUpgradeSurvives1xxGuard (Review Focus 2): the 1xx
// guard drops 100 and 102–199 only; a 101 upgrade still switches and
// frames flow both ways.
func TestProxy_WebSocketUpgradeSurvives1xxGuard(t *testing.T) {
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
	fb.scriptTicket("tk-1xx-ws", testWSUID)
	fb.pointLeaseAt(t, "tk-1xx-ws", echo)
	srv := newGateway(t, fb, nil)
	cookie := launchOK(t, srv, testHost, "tk-1xx-ws")

	up := upgrade(t, srv, testHost, "/websockify", cookie, map[string]string{"Origin": testOrigin})
	if up.StatusCode != http.StatusSwitchingProtocols {
		drain(up)
		t.Fatalf("upgrade = %d, want 101", up.StatusCode)
	}
	defer up.Body.Close()
	rwc, ok := up.Body.(io.ReadWriteCloser)
	if !ok {
		t.Fatalf("101 body is %T, want a read-write stream", up.Body)
	}
	if _, err := rwc.Write([]byte("ping")); err != nil {
		t.Fatalf("write through stream: %v", err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(rwc, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("echo = %q, %v; want ping", buf, err)
	}
}
