package gateway

// Launch endpoint contract (design §6.3, ADR 0001, origin policy ADR 0004):
//
//   POST /v1/launch carries the opaque ticket in the request BODY — never in
//   the query string. The handler runs on a per-workspace session host
//   (ServeHTTP matched Host under the session domain), validates Origin and
//   Sec-Fetch-Site, redeems the ticket atomically through the broker,
//   verifies the lease's workspace matches the host's, sets the host-only
//   Secure/HttpOnly session cookie (SameSite=Lax, or SameSite=None;
//   Partitioned in partitioned cookie mode) and returns 303 to a clean URL.
//   A launch that fails validation must NOT consume the ticket; a redeemed
//   lease on the wrong host is revoked instead.
//
//   Origin policy: the portal lives on a different registrable domain, so
//   the designed launch POST arrives CROSS-SITE with Origin=<portal
//   origin>. Origin must match (scheme+host+port) a configured portal
//   origin or this gateway's own public origin; when Sec-Fetch-Site is
//   present an absent/"null" Origin is rejected. Requests carrying neither
//   header are non-browser clients and stay allowed — the one-use 60s
//   ticket is still required.

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/api"
	"github.com/tinyorbitvn/tinycdi/internal/broker"
	"github.com/tinyorbitvn/tinycdi/internal/observability"
)

const (
	// LaunchPath is the session-origin launch endpoint.
	LaunchPath = "/v1/launch"
	// TicketField is the form field carrying the ticket in the POST body.
	TicketField = "ticket"
	// DesktopPath is where the browser is redirected after redemption — no
	// ticket or session material may appear in it. resize=remote is a static
	// client setting: the KasmVNC web client otherwise treats a page inside
	// an iframe as an embedded widget and forces resize=off, which keeps the
	// remote screen at its old size and leaves large dark regions in a
	// larger portal frame (FX-R18). It is the default in a top-level tab.
	DesktopPath = "/?resize=remote"

	maxLaunchBody = 4096
)

// launchRequest is the parsed POST body; Ticket is never read from the URL.
type launchRequest struct {
	Ticket string
}

// launchOriginOK reports whether the request's Origin may redeem a launch
// ticket: an exact (scheme+host+port, default ports normalized) match of a
// configured portal origin, or this request's own workspace origin —
// https:// + the request Host, which ServeHTTP already matched against the
// session domain (ADR 0004). The portal allowlist applies ONLY to the
// launch POST — WebSocket upgrades and desktop routes keep the strict
// Origin == own workspace origin rule (originOK).
func (g *Gateway) launchOriginOK(o, reqHost string) bool {
	u, ok := parseOrigin(o)
	if !ok {
		return false
	}
	for _, p := range g.portalOrigins {
		if originsEqual(u, p) {
			return true
		}
	}
	return originsEqual(u, &url.URL{Scheme: "https", Host: reqHost})
}

// parseOrigin parses a serialized Origin header value: scheme://host[:port]
// with no userinfo, path, query or fragment ("null" never parses).
func parseOrigin(o string) (*url.URL, bool) {
	u, err := url.Parse(o)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") ||
		u.Host == "" || u.User != nil ||
		u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, false
	}
	return u, true
}

// effectivePort returns the URL's port, filling in the scheme default when
// absent — browsers omit :443/:80 in Origin, so compare normalized.
func effectivePort(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	switch u.Scheme {
	case "https":
		return "443"
	case "http":
		return "80"
	}
	return ""
}

// originsEqual compares two origins exactly on scheme+host+port, with
// default ports normalized and hostnames case-insensitive.
func originsEqual(a, b *url.URL) bool {
	return a.Scheme == b.Scheme &&
		strings.EqualFold(a.Hostname(), b.Hostname()) &&
		effectivePort(a) == effectivePort(b)
}

func randToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// handleLaunch redeems a launch ticket on a workspace host (wsID is the
// workspace the request Host names): POST body ticket=<opaque> ->
// broker.RedeemTicket -> host binding -> __Host- cookie -> 303 clean URL.
// Validation order is security-significant: every check runs BEFORE
// redemption so a rejected launch never consumes the ticket.
func (g *Gateway) handleLaunch(w http.ResponseWriter, r *http.Request, wsID string) {
	// Launch origin policy (ADR 0004): the portal↔session POST is
	// cross-site by design, so CSRF/session-fixation resistance comes from
	// the one-use ticket bound to the requesting user plus the configured
	// portal-origin allowlist — not from Origin == session origin.
	// Fetch metadata stays a hard gate: when a browser labels the request
	// (Sec-Fetch-Site present), an absent or "null" Origin can never
	// redeem, and a recognized site value requires an allowlisted Origin.
	// Requests with neither header are non-browser clients and remain
	// allowed (ticket still required); a present Origin is always checked.
	origin := r.Header.Get("Origin")
	switch sfs := r.Header.Get("Sec-Fetch-Site"); {
	case sfs != "":
		switch sfs {
		case "same-origin", "same-site", "cross-site":
		default:
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "bad_fetch_site"})
			return
		}
		if origin == "" || origin == "null" || !g.launchOriginOK(origin, r.Host) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "bad_origin"})
			return
		}
	case origin != "" && !g.launchOriginOK(origin, r.Host):
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "bad_origin"})
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method"})
		return
	}
	if r.URL.Query().Get(TicketField) != "" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "ticket_in_query"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxLaunchBody)
	if err := r.ParseForm(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_form"})
		return
	}
	ticket := r.PostForm.Get(TicketField)
	if ticket == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_ticket"})
		return
	}

	lease, err := g.cfg.Broker.RedeemTicket(r.Context(), g.cfg.Identity, ticket)
	if err != nil {
		if g.cfg.Metrics != nil {
			g.cfg.Metrics.IncLeaseFailure(leaseFailureReason(err))
		}
		code := http.StatusUnauthorized
		if errors.Is(err, broker.ErrConnectionInUse) {
			code = http.StatusConflict
		}
		g.audit(r, "launch.redeem", "", observability.OutcomeDenied, "invalid_ticket")
		writeJSON(w, code, map[string]string{"error": "invalid_ticket"})
		return
	}

	// Host binding (D11): the lease must belong to the workspace this host
	// names. A mismatch redeems-then-revokes — the minted lease is burned
	// so it cannot be replayed on the right host later — but never sets a
	// cookie. The host gate in ServeHTTP already ran, so this can only
	// fire when the ticket was minted for a different workspace.
	if lease.WorkspaceUID != wsID {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		_ = g.cfg.Broker.RevokeLease(ctx, lease.ID)
		cancel()
		g.audit(r, "launch.host_mismatch", lease.WorkspaceUID, observability.OutcomeDenied, "host_mismatch")
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "host_mismatch"})
		return
	}

	s := newSession(randToken(32), lease, g.now())
	// With a session directory the digest must be bound before the cookie
	// leaves the process — an unbound cookie would die with this replica.
	// On failure the lease is revoked and no cookie is set: the launch
	// failed cleanly and the user retries (D19).
	if g.cfg.Sessions != nil {
		bctx, bcancel := context.WithTimeout(r.Context(), 10*time.Second)
		err := g.cfg.Sessions.BindSession(bctx, g.cfg.Identity, lease.ID, sessionDigest(s.id))
		bcancel()
		if err != nil {
			if g.cfg.Metrics != nil {
				g.cfg.Metrics.IncLeaseFailure(leaseFailureReason(err))
			}
			rctx, rcancel := context.WithTimeout(r.Context(), 5*time.Second)
			_ = g.cfg.Broker.RevokeLease(rctx, lease.ID)
			rcancel()
			g.audit(r, "launch.redeem", lease.WorkspaceUID, observability.OutcomeDenied, "bind_failed")
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "unavailable"})
			return
		}
	}

	// Takeover: a new lease for a workspace this gateway already serves
	// fences the old session's sockets BEFORE the new cookie is written.
	g.mu.Lock()
	old := g.byWorkspace[lease.WorkspaceUID]
	g.sessions[s.id] = s
	g.byLease[lease.ID] = s
	g.byWorkspace[lease.WorkspaceUID] = s
	g.mu.Unlock()
	if old != nil {
		g.killSession(old, "takeover")
	}
	go g.renewLoop(s)
	go g.activitySender(s)
	g.audit(r, "launch.redeem", lease.WorkspaceUID, observability.OutcomeSuccess, "")

	http.SetCookie(w, g.sessionCookie(s.id))
	w.Header().Set("Location", DesktopPath)
	w.WriteHeader(http.StatusSeeOther)
}

// sessionCookie builds the host-only (__Host-, no Domain, Path=/) Secure
// HttpOnly session cookie for the configured CookieMode (D10, D16).
//
// Lax, never Strict: the launch POST may arrive cross-site, and a Strict
// cookie is not sent on the 303 redirect that follows it — the desktop
// would load "unauthorized" (launch regression). Lax reaches the session
// origin inside the portal's iframe only when both are the same site.
//
// Partitioned: SameSite=None so the cookie is sent inside a cross-site
// portal iframe, and Partitioned (CHIPS) so the browser keys it to the
// embedding top-level site — another site framing the session origin never
// sees it.
func (g *Gateway) sessionCookie(id string) *http.Cookie {
	c := &http.Cookie{
		Name:     SessionCookieName,
		Value:    id,
		Path:     "/",
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	}
	if g.cfg.CookieMode == CookieModePartitioned {
		c.SameSite = http.SameSiteNoneMode
		c.Partitioned = true
	}
	return c
}

// audit emits one structured audit event. Tickets, cookie values and
// upstream credentials are never written — Details keys pass through
// observability redaction regardless.
func (g *Gateway) audit(r *http.Request, action, targetUID string, outcome observability.AuditOutcome, errCode string) {
	if g.cfg.Audit == nil {
		return
	}
	reqID := api.RequestIDFromContext(r.Context())
	if reqID == "" {
		reqID = r.Header.Get("X-Request-Id")
	}
	_ = g.cfg.Audit.WriteAudit(r.Context(), observability.AuditEvent{
		Time:      time.Now().UTC(),
		Actor:     "gateway:" + g.cfg.Identity.ID,
		Action:    action,
		TargetUID: targetUID,
		RequestID: reqID,
		Outcome:   outcome,
		ErrorCode: errCode,
	})
}
