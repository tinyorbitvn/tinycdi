package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/broker"
	"github.com/tinyorbitvn/tinycdi/internal/ratelimit"
)

// TransferPathPrefix is the session-host route family for single-use file
// transfers (ADR 0009 §5). The id is an identifier, not a credential:
// the session cookie, the live lease and the broker's single-use claim
// authorize the request.
const TransferPathPrefix = "/v1/transfers/"

const (
	xferTokenHeader = "X-Tcdi-Xfer-Token"
	// transferStreamBudget bounds one transfer's socket time; it matches
	// store.FileTransferClaimDeadAfter so a dead claim and a dead stream
	// age out together.
	transferStreamBudget = 30 * time.Minute
)

var transferPathRE = regexp.MustCompile(`^/v1/transfers/(xf_[a-z2-7]{26})/content$`)

// transferOriginOK: a configured portal origin or the workspace's own
// origin. A missing Origin is refused: browsers send it on every CORS
// request and on same-origin PUTs, and requiring it is the CSRF fence for
// a same-site sibling.
func (g *Gateway) transferOriginOK(o, reqHost string) bool {
	return o != "" && g.launchOriginOK(o, reqHost)
}

func (g *Gateway) serveTransfer(w http.ResponseWriter, r *http.Request, wsID string) {
	origin := r.Header.Get("Origin")
	if !g.transferOriginOK(origin, r.Host) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "bad_origin"})
		return
	}
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", origin)
	h.Set("Access-Control-Allow-Credentials", "true")
	h.Add("Vary", "Origin")
	h.Set("Access-Control-Expose-Headers", "Content-Length, Content-Disposition")
	h.Set("Cache-Control", "no-store")
	clean, ok := cleanedProxyPath(r.URL)
	m := transferPathRE.FindStringSubmatch(clean)
	if !ok || m == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	transferID := m[1]
	if r.Method == http.MethodOptions {
		switch r.Header.Get("Access-Control-Request-Method") {
		case http.MethodGet, http.MethodPut:
		default:
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
			return
		}
		h.Set("Access-Control-Allow-Methods", "GET, PUT")
		h.Set("Access-Control-Allow-Headers", "Content-Type")
		h.Set("Access-Control-Max-Age", "600")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodPut {
		h.Set("Allow", "GET, PUT, OPTIONS")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
		return
	}
	s, err := g.lookupSession(r, wsID)
	if err != nil {
		var lim *lookupLimitedError
		if errors.As(err, &lim) {
			h.Set("Retry-After", strconv.Itoa(ratelimit.RetryAfterSeconds(lim.retryAfter)))
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "rate_limited"})
			return
		}
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "unavailable"})
		return
	}
	if s == nil || s.workspaceUID() != wsID {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if err := g.ensureTarget(r.Context(), s); err != nil {
		if terminalBrokerErr(err) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "unavailable"})
		return
	}
	t, up, rt := s.resolved()
	if t == nil || up == nil || rt == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "unavailable"})
		return
	}
	if r.Method == http.MethodPut && r.ContentLength < 0 {
		// Browsers send a Blob body with Content-Length; a chunked upload
		// cannot be checked against the cap before the claim spends it.
		writeJSON(w, http.StatusLengthRequired, map[string]string{"error": "length_required"})
		return
	}
	if !g.acquireUpstreamStream(wsID) {
		g.writeStreamCapRefused(w, r, wsID)
		return
	}
	defer g.releaseUpstreamStream(wsID)
	// A transfer in flight pins the session against the idle release: a
	// download keeps running after its tab closed. The closure stamps the
	// end time when the transfer finishes, after CompleteTransfer.
	s.beginTransfer()
	defer func() { s.endTransfer(g.now()) }()

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	grant, err := g.cfg.Broker.ClaimTransfer(ctx, g.cfg.Identity, s.leaseID(), transferID, r.Method)
	cancel()
	if err != nil {
		writeTransferClaimError(w, err)
		return
	}
	var n int64
	var done bool
	// Completion is recorded even when the client went away or the
	// download aborted (panic(http.ErrAbortHandler) still runs defers):
	// a fresh, bounded context.
	defer func() {
		cctx, ccancel := context.WithTimeout(context.WithoutCancel(r.Context()), 10*time.Second)
		defer ccancel()
		if err := g.cfg.Broker.CompleteTransfer(cctx, g.cfg.Identity, s.leaseID(), transferID, n, done); err != nil && g.cfg.Logger != nil {
			g.cfg.Logger.Warn("transfer completion not recorded", "err", err)
		}
	}()
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(transferStreamBudget))
	_ = rc.SetWriteDeadline(time.Now().Add(transferStreamBudget))
	if r.Method == http.MethodPut {
		n, done = g.transferUpload(w, r, grant, t, up, rt)
	} else {
		n, done = g.transferDownload(w, r, grant, t, up, rt)
	}
}

func writeTransferClaimError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, broker.ErrTransferInvalid):
		writeJSON(w, http.StatusGone, map[string]string{"error": "transfer_invalid"})
	case errors.Is(err, broker.ErrTransferDenied):
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "transfer_denied"})
	case errors.Is(err, broker.ErrTransferBusy):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "transfer_busy"})
	case terminalBrokerErr(err):
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
	default:
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "unavailable"})
	}
}

// helperURL builds the upstream /xfer/files/<name> URL from the resolved
// target authority (never client input) and re-checks it with the same
// parser-differential guard the desktop proxy uses.
func helperURL(up *url.URL, name string) (*url.URL, bool) {
	u := &url.URL{Scheme: up.Scheme, Host: up.Host, Path: "/xfer/files/" + name, RawPath: "/xfer/files/" + url.PathEscape(name)}
	clean, ok := cleanedProxyPath(u)
	if !ok || clean != u.Path || strings.Count(clean, "/") != 3 {
		return nil, false
	}
	return u, true
}

// countingReader counts bytes read and fails once more than max arrive.
type countingReader struct {
	r   io.Reader
	n   int64
	max int64
}

var errTransferTooLarge = errors.New("gateway: transfer exceeds its cap")

func (c *countingReader) Read(p []byte) (int, error) {
	k, err := c.r.Read(p)
	c.n += int64(k)
	if c.n > c.max {
		return k, errTransferTooLarge
	}
	return k, err
}

func (g *Gateway) transferUpload(w http.ResponseWriter, r *http.Request, grant broker.TransferGrant, t *broker.Target, up *url.URL, rt http.RoundTripper) (int64, bool) {
	if r.ContentLength > grant.MaxBytes {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "too_large"})
		return 0, false
	}
	u, ok := helperURL(up, grant.Path)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return 0, false
	}
	body := &countingReader{r: r.Body, max: grant.MaxBytes}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPut, u.String(), body)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "upstream_failed"})
		return 0, false
	}
	req.ContentLength = r.ContentLength
	req.Header.Set(xferTokenHeader, t.XferToken)
	req.Header.Set("Content-Type", "application/octet-stream")
	res, err := rt.RoundTrip(req)
	if err != nil {
		if errors.Is(err, errTransferTooLarge) || body.n > grant.MaxBytes {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "too_large"})
			return body.n, false
		}
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "upstream_failed"})
		return body.n, false
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 4<<10))
	switch res.StatusCode {
	case http.StatusOK:
		writeJSON(w, http.StatusOK, map[string]int64{"bytes": body.n})
		return body.n, true
	case http.StatusRequestEntityTooLarge:
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "too_large"})
	case http.StatusForbidden:
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "transfer_denied"})
	default:
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "upstream_failed"})
	}
	return body.n, false
}

func (g *Gateway) transferDownload(w http.ResponseWriter, r *http.Request, grant broker.TransferGrant, t *broker.Target, up *url.URL, rt http.RoundTripper) (int64, bool) {
	u, ok := helperURL(up, grant.Path)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return 0, false
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, u.String(), nil)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "upstream_failed"})
		return 0, false
	}
	req.Header.Set(xferTokenHeader, t.XferToken)
	res, err := rt.RoundTrip(req)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "upstream_failed"})
		return 0, false
	}
	defer res.Body.Close()
	switch {
	case res.StatusCode == http.StatusNotFound:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "file_not_found"})
		return 0, false
	case res.StatusCode == http.StatusRequestEntityTooLarge || res.ContentLength > grant.MaxBytes:
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "too_large"})
		return 0, false
	case res.StatusCode != http.StatusOK:
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "upstream_failed"})
		return 0, false
	}
	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(grant.Path))
	if res.ContentLength >= 0 {
		h.Set("Content-Length", strconv.FormatInt(res.ContentLength, 10))
	}
	w.WriteHeader(http.StatusOK)
	n, err := io.Copy(w, io.LimitReader(res.Body, grant.MaxBytes+1))
	if err != nil || n > grant.MaxBytes || (res.ContentLength >= 0 && n != res.ContentLength) {
		// Headers are gone; abort the connection so the browser sees a
		// failed download, not a truncated file.
		panic(http.ErrAbortHandler)
	}
	return n, true
}
