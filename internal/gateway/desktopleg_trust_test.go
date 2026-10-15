package gateway_test

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestDesktopLeg_ReplacedHelperRelaysBytesWithoutDisposition characterizes
// the accepted in-pod trust residual (TM-37): a process that replaces the
// pod's :8443 server can serve arbitrary bytes on an allowlisted desktop
// path, and the gateway relays them with no transfer grant, cap or
// transfer audit — the documented limit of the in-pod policies. The
// gateway's own hardening is that it drops the upstream's
// Content-Disposition, so the desktop leg never offers a one-click
// download.
func TestDesktopLeg_ReplacedHelperRelaysBytesWithoutDisposition(t *testing.T) {
	secret := bytes.Repeat([]byte("TENANT-SECRET-ROW;"), 4<<20/18)
	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/app/exfil.bin" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", `attachment; filename="db-dump.bin"`)
		_, _ = w.Write(secret)
	}))
	t.Cleanup(up.Close)
	fb := newFakeBroker(t) // no transfer grants
	fb.scriptTicket("tk-inpod", testWSUID)
	fb.pointLeaseAt(t, "tk-inpod", up)
	srv := newGateway(t, fb, nil)
	cookie := launchOK(t, srv, testHost, "tk-inpod")

	resp := proxied(t, srv, testHost, "/app/exfil.bin", cookie, map[string]string{"Origin": testOrigin})
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	// The accepted residual: the bytes cross with no grant and no transfer
	// completion. If this ever stops holding, update TM-37 and the
	// release notes' "In-pod trust" limitation.
	if resp.StatusCode != http.StatusOK || !bytes.Equal(body, secret) {
		t.Fatalf("desktop leg: status=%d bytes=%d — the documented residual changed; update TM-37", resp.StatusCode, len(body))
	}
	if n := len(fb.completions()); n != 0 {
		t.Fatalf("desktop leg recorded %d transfer completions, want 0", n)
	}
	if cd := resp.Header.Get("Content-Disposition"); cd != "" {
		t.Fatalf("Content-Disposition = %q relayed from the runtime, want it dropped", cd)
	}
}
