package gateway_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/tinyorbitvn/tinycdi/internal/broker"
	"github.com/tinyorbitvn/tinycdi/internal/gateway"
)

const (
	portalOrigin = "https://portal.test"
	xfID         = "xf_abcdefghijklmnopqrstuvwxyz"
	xfPath       = "/v1/transfers/" + xfID + "/content"
)

// xferStub stands in for tcdi-xferd: it checks the injected credential
// and stores/serves files by name.
type xferStub struct {
	mu    sync.Mutex
	files map[string]string
	paths []string
	srv   *httptest.Server
}

func newXferStub(t *testing.T) *xferStub {
	s := &xferStub{files: map[string]string{"report.pdf": "PDFDATA"}}
	s.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.paths = append(s.paths, r.URL.EscapedPath())
		if r.Header.Get("X-Tcdi-Xfer-Token") != "xt" || r.Header.Get("Cookie") != "" {
			http.Error(w, "bad credential", http.StatusUnauthorized)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/xfer/files/")
		switch r.Method {
		case http.MethodPut:
			b, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, "short body", http.StatusBadRequest)
				return
			}
			s.files[name] = string(b)
			_ = json.NewEncoder(w).Encode(map[string]int{"bytes": len(b)})
		case http.MethodGet:
			v, ok := s.files[name]
			if !ok {
				http.NotFound(w, r)
				return
			}
			_, _ = io.WriteString(w, v)
		}
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *xferStub) file(name string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.files[name]
	return v, ok
}

func (s *xferStub) seen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.paths...)
}

func transferRig(t *testing.T, grant broker.TransferGrant, xferToken string) (*fakeBroker, *httptest.Server, *xferStub, string) {
	t.Helper()
	fb := newFakeBroker(t)
	stub := newXferStub(t)
	fb.scriptTicket("tk-xfer", testWSUID)
	lease := fb.leaseOf(t, "tk-xfer")
	tgt := targetFor(stub.srv)
	tgt.XferToken = xferToken
	fb.mu.Lock()
	fb.resolveT[lease.ID] = tgt
	if grant.ID != "" {
		fb.transfers[grant.ID] = grant
	}
	fb.mu.Unlock()
	srv := newGateway(t, fb, func(c *gateway.Config) { c.PortalOrigins = []string{portalOrigin} })
	cookie := launchOK(t, srv, testHost, "tk-xfer")
	return fb, srv, stub, cookie
}

func transferReq(t *testing.T, srv *httptest.Server, method, path, cookie, origin, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	req.Host = testHost
	if cookie != "" {
		req.Header.Set("Cookie", gateway.SessionCookieName+"="+cookie)
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	res, err := srv.Client().Transport.(*http.Transport).RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { res.Body.Close() })
	return res
}

func TestTransfer_UploadRoundTrip(t *testing.T) {
	fb, srv, stub, cookie := transferRig(t, broker.TransferGrant{ID: xfID, WorkspaceUID: testWSUID, Direction: "upload", Path: "Ảnh 1.png", MaxBytes: 100}, "xt")
	res := transferReq(t, srv, http.MethodPut, xfPath, cookie, portalOrigin, "hello")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("upload: %d", res.StatusCode)
	}
	if res.Header.Get("Access-Control-Allow-Origin") != portalOrigin || res.Header.Get("Access-Control-Allow-Credentials") != "true" {
		t.Fatalf("CORS headers: %v", res.Header)
	}
	if v, _ := stub.file("Ảnh 1.png"); v != "hello" || stub.seen()[0] != "/xfer/files/%E1%BA%A2nh%201.png" {
		t.Fatalf("helper saw %q at %v", v, stub.seen())
	}
	if got := fb.completions(); len(got) != 1 || got[0] != (transferDone{xfID, 5, true}) {
		t.Fatalf("completions = %+v", got)
	}
	if res := transferReq(t, srv, http.MethodPut, xfPath, cookie, portalOrigin, "again"); res.StatusCode != http.StatusGone {
		t.Fatalf("replayed id: %d, want 410", res.StatusCode)
	}
}

func TestTransfer_DownloadRoundTrip(t *testing.T) {
	fb, srv, _, cookie := transferRig(t, broker.TransferGrant{ID: xfID, WorkspaceUID: testWSUID, Direction: "download", Path: "report.pdf", MaxBytes: 100}, "xt")
	res := transferReq(t, srv, http.MethodGet, xfPath, cookie, portalOrigin, "")
	b, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK || string(b) != "PDFDATA" ||
		res.Header.Get("Content-Disposition") != `attachment; filename*=UTF-8''report.pdf` {
		t.Fatalf("download: %d %q %v", res.StatusCode, b, res.Header)
	}
	if got := fb.completions(); len(got) != 1 || got[0] != (transferDone{xfID, 7, true}) {
		t.Fatalf("completions = %+v", got)
	}
}

func TestTransfer_DownloadOverCapAborts(t *testing.T) {
	fb, srv, _, cookie := transferRig(t, broker.TransferGrant{ID: xfID, WorkspaceUID: testWSUID, Direction: "download", Path: "report.pdf", MaxBytes: 3}, "xt")
	res := transferReq(t, srv, http.MethodGet, xfPath, cookie, portalOrigin, "")
	b, _ := io.ReadAll(res.Body)
	if res.StatusCode == http.StatusOK && len(b) > 3 {
		t.Fatalf("over-cap download delivered %d bytes", len(b))
	}
	if got := fb.completions(); len(got) != 1 || got[0].ok {
		t.Fatalf("completions = %+v, want one failed", got)
	}
}

func TestTransfer_UploadOverCapRefused(t *testing.T) {
	fb, srv, stub, cookie := transferRig(t, broker.TransferGrant{ID: xfID, WorkspaceUID: testWSUID, Direction: "upload", Path: "a.bin", MaxBytes: 4}, "xt")
	if res := transferReq(t, srv, http.MethodPut, xfPath, cookie, portalOrigin, "0123456789"); res.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("over cap: %d", res.StatusCode)
	}
	if _, ok := stub.file("a.bin"); ok {
		t.Fatal("an over-cap upload reached the helper as a complete file")
	}
	if got := fb.completions(); len(got) != 1 || got[0].ok {
		t.Fatalf("completions = %+v, want one failed", got)
	}
}

func TestTransfer_Refusals(t *testing.T) {
	grant := broker.TransferGrant{ID: xfID, WorkspaceUID: testWSUID, Direction: "upload", Path: "a.txt", MaxBytes: 100}
	_, srv, _, cookie := transferRig(t, grant, "xt")
	cases := []struct {
		name, method, path, cookie, origin string
		want                               int
	}{
		{"no session", http.MethodPut, xfPath, "", portalOrigin, http.StatusUnauthorized},
		{"foreign origin", http.MethodPut, xfPath, cookie, "https://evil.test", http.StatusForbidden},
		{"no origin", http.MethodPut, xfPath, cookie, "", http.StatusForbidden},
		{"malformed id", http.MethodPut, "/v1/transfers/xf_BAD/content", cookie, portalOrigin, http.StatusNotFound},
		{"traversal", http.MethodPut, "/v1/transfers/" + xfID + "/../content", cookie, portalOrigin, http.StatusNotFound},
		{"wrong method", http.MethodPost, xfPath, cookie, portalOrigin, http.StatusMethodNotAllowed},
	}
	for _, c := range cases {
		if res := transferReq(t, srv, c.method, c.path, c.cookie, c.origin, "x"); res.StatusCode != c.want {
			t.Errorf("%s: %d, want %d", c.name, res.StatusCode, c.want)
		}
	}
	pre, _ := http.NewRequest(http.MethodOptions, srv.URL+xfPath, nil)
	pre.Host = testHost
	pre.Header.Set("Origin", portalOrigin)
	pre.Header.Set("Access-Control-Request-Method", "PUT")
	res, err := srv.Client().Transport.(*http.Transport).RoundTrip(pre)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusNoContent || res.Header.Get("Access-Control-Allow-Methods") != "GET, PUT" ||
		res.Header.Get("Access-Control-Allow-Origin") != portalOrigin {
		t.Fatalf("preflight: %d %v", res.StatusCode, res.Header)
	}
}

func TestTransfer_BrokerRefusalsMapped(t *testing.T) {
	for _, c := range []struct {
		err  error
		want int
	}{
		{broker.ErrTransferInvalid, http.StatusGone},
		{broker.ErrTransferDenied, http.StatusForbidden},
		{broker.ErrTransferBusy, http.StatusConflict},
	} {
		fb, srv, stub, cookie := transferRig(t, broker.TransferGrant{}, "xt")
		fb.mu.Lock()
		fb.claimErr = c.err
		fb.mu.Unlock()
		if res := transferReq(t, srv, http.MethodPut, xfPath, cookie, portalOrigin, "x"); res.StatusCode != c.want {
			t.Errorf("%v: %d, want %d", c.err, res.StatusCode, c.want)
		}
		if len(stub.seen()) != 0 || len(fb.completions()) != 0 {
			t.Errorf("%v: a refused claim reached the helper or completed", c.err)
		}
	}
}

func TestTransfer_DesktopProxyStillRefusesXfer(t *testing.T) {
	_, srv, stub, cookie := transferRig(t, broker.TransferGrant{}, "xt")
	res := proxied(t, srv, testHost, "/xfer/files/report.pdf", cookie, nil)
	drain(res)
	if res.StatusCode != http.StatusNotFound || len(stub.seen()) != 0 {
		t.Fatalf("generic proxy reached /xfer/: %d %v", res.StatusCode, stub.seen())
	}
	// KasmVNC's own upload/download surface stays off the allowlist too
	// (ADR 0010 §5: the helper is the only transfer path).
	for _, p := range []string{"/xfer", "/api/upload", "/api/download", "/upload", "/downloads/a", "/Downloads/a"} {
		r := proxied(t, srv, testHost, p, cookie, nil)
		drain(r)
		if r.StatusCode != http.StatusNotFound {
			t.Errorf("generic proxy served %s: %d, want 404", p, r.StatusCode)
		}
	}
	if len(stub.seen()) != 0 {
		t.Fatalf("a refused desktop path reached the helper: %v", stub.seen())
	}
}
