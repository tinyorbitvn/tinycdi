package xferd_test

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tinyorbitvn/tinycdi/internal/xferd"
)

const token = "tok-123"

type rig struct {
	srv      *httptest.Server
	root     string
	outside  string
	upstream *httptest.Server
	gotPath  string
	gotToken string
	gotXFF   string
}

func newRig(t *testing.T, policy string, max int64) *rig {
	t.Helper()
	r := &rig{}
	base := t.TempDir()
	r.root = filepath.Join(base, "transfers")
	r.outside = filepath.Join(base, "secret.txt")
	if err := os.WriteFile(r.outside, []byte("do not touch"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.upstream = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.gotPath, r.gotToken, r.gotXFF = req.URL.Path, req.Header.Get(xferd.TokenHeader), req.Header.Get("X-Forwarded-For")
		_, _ = io.WriteString(w, "desktop")
	}))
	t.Cleanup(r.upstream.Close)
	up, _ := url.Parse(r.upstream.URL)
	h, err := xferd.New(xferd.Config{
		Root: r.root, Token: []byte(token), MaxBytes: max, Policy: policy,
		Upstream: up, UpstreamServerName: "example.com",
		UpstreamCAPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: r.upstream.Certificate().Raw}),
	})
	if err != nil {
		t.Fatal(err)
	}
	r.srv = httptest.NewTLSServer(h)
	t.Cleanup(r.srv.Close)
	return r
}

func (r *rig) do(t *testing.T, method, rawPath, tok, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, r.srv.URL+rawPath, strings.NewReader(body))
	if tok != "" {
		req.Header.Set(xferd.TokenHeader, tok)
	}
	req.SetBasicAuth("kasm_user", "pw")
	res, err := r.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { res.Body.Close() })
	return res
}

func TestXferd_JailRefusesEscapes(t *testing.T) {
	r := newRig(t, "Bidirectional", 1<<20)
	if err := os.MkdirAll(r.root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(r.outside, filepath.Join(r.root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("..", filepath.Join(r.root, "up")); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{
		"/xfer/files/..%2Fsecret.txt", "/xfer/files/%2E%2E", "/xfer/files/link", "/xfer/files/up%2Fsecret.txt",
		"/xfer/files/a%2Fb", "/xfer/files/%00", "/xfer/files/", "/xfer/files/a%5Cb", "/xfer/files/x/../../secret.txt",
	} {
		for _, m := range []string{http.MethodGet, http.MethodPut} {
			if res := r.do(t, m, p, token, "pwned"); res.StatusCode == http.StatusOK {
				t.Errorf("%s %s = 200, want refusal", m, p)
			}
		}
	}
	if b, _ := os.ReadFile(r.outside); string(b) != "do not touch" {
		t.Fatalf("file outside the jail changed: %q", b)
	}
	if err := os.RemoveAll(r.root); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Dir(r.outside), r.root); err != nil {
		t.Fatal(err)
	}
	if res := r.do(t, http.MethodGet, "/xfer/files/secret.txt", token, ""); res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("transfer root replaced by a symlink: %d, want 503", res.StatusCode)
	}
}

func TestXferd_TokenPolicyAndCap(t *testing.T) {
	r := newRig(t, "Upload", 8)
	if res := r.do(t, http.MethodPut, "/xfer/files/a.txt", "", "12345"); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token: %d", res.StatusCode)
	}
	if res := r.do(t, http.MethodPut, "/xfer/files/a.txt", "tok-124", "12345"); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong token: %d", res.StatusCode)
	}
	if res := r.do(t, http.MethodPut, "/xfer/files/a.txt", token, "12345"); res.StatusCode != http.StatusOK {
		t.Fatalf("upload: %d", res.StatusCode)
	}
	if b, _ := os.ReadFile(filepath.Join(r.root, "a.txt")); string(b) != "12345" {
		t.Fatalf("uploaded content %q", b)
	}
	if res := r.do(t, http.MethodPut, "/xfer/files/big.bin", token, "123456789"); res.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("over cap: %d", res.StatusCode)
	}
	entries, _ := os.ReadDir(r.root)
	for _, e := range entries {
		if e.Name() != "a.txt" {
			t.Fatalf("leftover after refused upload: %s", e.Name())
		}
	}
	if res := r.do(t, http.MethodGet, "/xfer/files/a.txt", token, ""); res.StatusCode != http.StatusForbidden {
		t.Fatalf("download under an Upload floor: %d", res.StatusCode)
	}
}

func TestXferd_DownloadAndUnknownPolicyFailsClosed(t *testing.T) {
	r := newRig(t, "Download", 1<<20)
	_ = os.MkdirAll(r.root, 0o700)
	_ = os.WriteFile(filepath.Join(r.root, "Ảnh.png"), []byte("img"), 0o600)
	res := r.do(t, http.MethodGet, "/xfer/files/"+url.PathEscape("Ảnh.png"), token, "")
	if b, _ := io.ReadAll(res.Body); res.StatusCode != http.StatusOK || string(b) != "img" || res.ContentLength != 3 {
		t.Fatalf("download: %d %q len=%d", res.StatusCode, b, res.ContentLength)
	}
	if res := r.do(t, http.MethodGet, "/xfer/files/missing", token, ""); res.StatusCode != http.StatusNotFound {
		t.Fatalf("missing: %d", res.StatusCode)
	}
	z := newRig(t, "Sideways", 1<<20)
	if res := z.do(t, http.MethodPut, "/xfer/files/a", token, "x"); res.StatusCode != http.StatusForbidden {
		t.Fatalf("unknown policy: %d, want 403 (fail closed)", res.StatusCode)
	}
}

func TestXferd_ProxiesDesktopAndStripsToken(t *testing.T) {
	r := newRig(t, "Disabled", 0)
	req, _ := http.NewRequest(http.MethodGet, r.srv.URL+"/websockify?x=1", nil)
	req.SetBasicAuth("kasm_user", "pw")
	req.Header.Set(xferd.TokenHeader, token)
	req.Header.Set("X-Forwarded-For", "203.0.113.9")
	res, err := r.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if b, _ := io.ReadAll(res.Body); res.StatusCode != http.StatusOK || string(b) != "desktop" {
		t.Fatalf("proxy: %d %q", res.StatusCode, b)
	}
	if r.gotPath != "/websockify" || r.gotToken != "" || r.gotXFF != "203.0.113.9" {
		t.Fatalf("upstream saw path %q token %q xff %q; the token must never reach KasmVNC and the client chain must", r.gotPath, r.gotToken, r.gotXFF)
	}
	anonReq, _ := http.NewRequest(http.MethodGet, r.srv.URL+"/", nil)
	anon, err := r.srv.Client().Do(anonReq)
	if err != nil {
		t.Fatal(err)
	}
	anon.Body.Close()
	if anon.StatusCode != http.StatusUnauthorized || r.gotPath != "/websockify" {
		t.Fatalf("anonymous request: %d (upstream path %q); it must be answered locally", anon.StatusCode, r.gotPath)
	}
}

func TestXferd_ProxiesWebSocketUpgrade(t *testing.T) {
	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if !strings.EqualFold(req.Header.Get("Upgrade"), "websocket") {
			http.Error(w, "no upgrade", http.StatusBadRequest)
			return
		}
		c, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
		_ = rw.Flush()
		buf := make([]byte, 4)
		if _, err := io.ReadFull(rw, buf); err == nil {
			_, _ = c.Write(buf)
		}
	}))
	t.Cleanup(up.Close)
	upURL, _ := url.Parse(up.URL)
	h, err := xferd.New(xferd.Config{
		Root: filepath.Join(t.TempDir(), "transfers"), Token: []byte(token), MaxBytes: 1, Policy: "Disabled",
		Upstream: upURL, UpstreamServerName: "example.com",
		UpstreamCAPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: up.Certificate().Raw}),
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	conn, err := tls.Dial("tcp", srv.Listener.Addr().String(), &tls.Config{RootCAs: pool, ServerName: "example.com", MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, _ = io.WriteString(conn, "GET /websockify HTTP/1.1\r\nHost: example.com\r\nAuthorization: Basic a2FzbV91c2VyOnB3\r\n"+
		"Connection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n")
	br := bufio.NewReader(conn)
	res, err := http.ReadResponse(br, nil)
	if err != nil || res.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade through the helper: %v %v", res, err)
	}
	_, _ = io.WriteString(conn, "ping")
	got := make([]byte, 4)
	if _, err := io.ReadFull(br, got); err != nil || string(got) != "ping" {
		t.Fatalf("websocket bytes did not round-trip through the helper: %q %v", got, err)
	}
}
