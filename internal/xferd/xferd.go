// Package xferd is the in-pod file-transfer helper (ADR 0010 §5 (i)). It
// owns the runtime's single admitted port, serves /xfer/files/{name}
// against a jailed transfer root, and reverse-proxies everything else to
// KasmVNC on loopback.
package xferd

import (
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// TokenHeader carries the per-workspace transfer credential.
const TokenHeader = "X-Tcdi-Xfer-Token"

const filesPrefix = "/xfer/files/"

// Config is the helper's whole input; main fills it from the pod.
type Config struct {
	Root               string // $HOME/transfers
	Token              []byte // Secret xfer-token; empty refuses every transfer (fail closed)
	MaxBytes           int64  // TCDI_XFER_MAX_BYTES (local floor)
	Policy             string // TCDI_XFER_POLICY: Disabled|Upload|Download|Bidirectional
	Upstream           *url.URL
	UpstreamServerName string // a SAN of the staged cert ("localhost")
	UpstreamCAPEM      []byte // the staged tls.crt, pinned as the only root
}

type helper struct {
	cfg   Config
	proxy *httputil.ReverseProxy
}

// New returns the helper's handler.
func New(cfg Config) (http.Handler, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(cfg.UpstreamCAPEM) {
		return nil, errors.New("xferd: upstream certificate is not PEM")
	}
	if cfg.Upstream == nil {
		return nil, errors.New("xferd: no upstream")
	}
	tr := &http.Transport{
		TLSClientConfig:   &tls.Config{RootCAs: pool, ServerName: cfg.UpstreamServerName, MinVersion: tls.VersionTLS12},
		ForceAttemptHTTP2: false,
	}
	up := cfg.Upstream
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(up)
			pr.Out.Host = pr.In.Host
			pr.Out.Header.Del(TokenHeader)
			// Rewrite drops the forwarding headers. The gateway already
			// rebuilt them from its trusted chain, and KasmVNC keys its
			// brute-force blacklist on them (gateway rewriteForwarded), so
			// they pass through unchanged; only the gateway can reach :8443.
			for _, k := range []string{"X-Forwarded-For", "X-Forwarded-Proto", "X-Forwarded-Host", "Forwarded", "X-Real-Ip"} {
				if v, ok := pr.In.Header[k]; ok {
					pr.Out.Header[k] = v
				}
			}
		},
		Transport:     tr,
		FlushInterval: -1,
	}
	return &helper{cfg: cfg, proxy: proxy}, nil
}

// ValidName is the same single-file-name rule the backend applies at mint.
func ValidName(name string) bool {
	if name == "" || len(name) > 255 || !utf8.ValidString(name) || name == "." || name == ".." ||
		strings.ContainsAny(name, "/\\;%") || strings.Contains(name, "..") {
		return false
	}
	for _, r := range name {
		if r == 0 || unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func (h *helper) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/xfer" || strings.HasPrefix(r.URL.Path, "/xfer/") {
		h.transfer(w, r)
		return
	}
	// All upstream traffic now comes from 127.0.0.1, so KasmVNC's
	// brute-force blacklist would lock every client out after a few
	// anonymous probes. The gateway injects Basic credentials on every
	// request; answer anything without them here.
	if _, _, ok := r.BasicAuth(); !ok {
		w.Header().Set("WWW-Authenticate", `Basic realm="tcdi"`)
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	h.proxy.ServeHTTP(w, r)
}

func (h *helper) allows(direction string) bool {
	switch h.cfg.Policy {
	case "Bidirectional":
		return true
	case "Upload":
		return direction == "upload"
	case "Download":
		return direction == "download"
	}
	return false // Disabled, unset or unknown: fail closed
}

// openRoot refuses a transfer root that is not a real directory: the home
// volume is user-writable, so a symlink planted at $HOME/transfers must
// never move the jail.
func (h *helper) openRoot() (*os.Root, error) {
	if err := os.Mkdir(h.cfg.Root, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return nil, err
	}
	fi, err := os.Lstat(h.cfg.Root)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() || fi.Mode()&fs.ModeSymlink != 0 {
		return nil, errors.New("xferd: transfer root is not a directory")
	}
	return os.OpenRoot(h.cfg.Root)
}

func (h *helper) transfer(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	got := []byte(r.Header.Get(TokenHeader))
	if len(h.cfg.Token) == 0 || subtle.ConstantTimeCompare(got, h.cfg.Token) != 1 {
		http.Error(w, "transfer credential required", http.StatusUnauthorized)
		return
	}
	raw := r.URL.EscapedPath()
	if !strings.HasPrefix(raw, filesPrefix) {
		http.NotFound(w, r)
		return
	}
	name, err := url.PathUnescape(strings.TrimPrefix(raw, filesPrefix))
	if err != nil || !ValidName(name) {
		http.Error(w, "bad file name", http.StatusBadRequest)
		return
	}
	var direction string
	switch r.Method {
	case http.MethodGet:
		direction = "download"
	case http.MethodPut:
		direction = "upload"
	default:
		w.Header().Set("Allow", "GET, PUT")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !h.allows(direction) || h.cfg.MaxBytes <= 0 {
		http.Error(w, "file "+direction+" is not allowed", http.StatusForbidden)
		return
	}
	root, err := h.openRoot()
	if err != nil {
		http.Error(w, "transfer folder unavailable", http.StatusServiceUnavailable)
		return
	}
	defer root.Close()
	if direction == "download" {
		h.download(w, root, name)
		return
	}
	h.upload(w, r, root, name)
}

func (h *helper) download(w http.ResponseWriter, root *os.Root, name string) {
	fi, err := root.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		http.Error(w, "no such file", http.StatusNotFound)
		return
	}
	if err != nil || !fi.Mode().IsRegular() {
		http.Error(w, "not a regular file", http.StatusConflict)
		return
	}
	if fi.Size() > h.cfg.MaxBytes {
		http.Error(w, "file is larger than the transfer limit", http.StatusRequestEntityTooLarge)
		return
	}
	f, err := root.Open(name)
	if err != nil {
		http.Error(w, "cannot open file", http.StatusConflict)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(fi.Size(), 10))
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, io.LimitReader(f, fi.Size()))
}

func (h *helper) upload(w http.ResponseWriter, r *http.Request, root *os.Root, name string) {
	if r.ContentLength > h.cfg.MaxBytes {
		http.Error(w, "file is larger than the transfer limit", http.StatusRequestEntityTooLarge)
		return
	}
	if fi, err := root.Lstat(name); err == nil && !fi.Mode().IsRegular() {
		http.Error(w, "the name is taken by a folder or link", http.StatusConflict)
		return
	}
	var rnd [8]byte
	_, _ = rand.Read(rnd[:])
	tmp := ".tcdi-xfer-" + hex.EncodeToString(rnd[:]) + ".part"
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		http.Error(w, "cannot write file", http.StatusServiceUnavailable)
		return
	}
	n, copyErr := io.Copy(f, io.LimitReader(r.Body, h.cfg.MaxBytes+1))
	closeErr := f.Close()
	if n > h.cfg.MaxBytes {
		_ = root.Remove(tmp)
		http.Error(w, "file is larger than the transfer limit", http.StatusRequestEntityTooLarge)
		return
	}
	if copyErr != nil || closeErr != nil {
		_ = root.Remove(tmp)
		http.Error(w, "upload interrupted", http.StatusBadRequest)
		return
	}
	if err := root.Rename(tmp, name); err != nil {
		_ = root.Remove(tmp)
		http.Error(w, "cannot place file", http.StatusConflict)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]int64{"bytes": n})
}
