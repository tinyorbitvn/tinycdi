package gateway

// Launch endpoint contract (design §6.3, ADR 0001, origin policy ADR 0004):
//
//   POST /v1/launch carries the opaque ticket in the request BODY — never in
//   the query string. The handler validates Host (allowlist), Origin and
//   Sec-Fetch-Site, redeems the ticket atomically through the broker, sets
//   the host-only Secure/HttpOnly/SameSite=Lax session cookie and returns
//   303 to a clean URL. A launch that fails validation must NOT consume
//   the ticket.
//
//   Origin policy: the portal lives on a different registrable domain, so
//   the designed launch POST arrives CROSS-SITE with Origin=<portal
//   origin>. Origin must match (scheme+host+port) a configured portal
//   origin or this gateway's own public origin; when Sec-Fetch-Site is
//   present an absent/"null" Origin is rejected. Requests carrying neither
//   header are non-browser clients and stay allowed — the one-use 60s
//   ticket is still required.

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
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
	// CleanPath is the path the browser is redirected to after redemption —
	// no ticket or session material may appear in it.
	CleanPath = "/"

	maxLaunchBody = 4096
)

// launchRequest is the parsed POST body; Ticket is never read from the URL.
type launchRequest struct {
	Ticket string
}

// launchOriginOK reports whether the request's Origin may redeem a launch
// ticket: an exact (scheme+host+port, default ports normalized) match of a
// configured portal origin, or the gateway's own public origin seen on this
// request's authority. The portal allowlist applies ONLY to the launch
// POST — WebSocket upgrades and desktop routes keep the strict
// Origin == public origin rule (originOK).
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
	// Public-origin leg keeps the old triple-match shape: the Origin's
	// authority must equal this request's authority (hostOK already
	// allowlisted it) AND the configured public origin.
	h, _, err := net.SplitHostPort(reqHost)
	if err != nil {
		h = reqHost
	}
	return u.Hostname() == h && originsEqual(u, g.pubOrigin)
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

// handleLaunch redeems a launch ticket: POST body ticket=<opaque> ->
// broker.RedeemTicket -> __Host- cookie -> 303 clean URL. Validation order is
// security-significant: every check runs BEFORE redemption so a rejected
// launch never consumes the ticket.
func (g *Gateway) handleLaunch(w http.ResponseWriter, r *http.Request) {
	if !g.hostOK(r) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "bad_host"})
		return
	}
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

	// Takeover: a new lease for a workspace this gateway already serves
	// fences the old session's sockets BEFORE the new cookie is written.
	s := newSession(randToken(32), lease, g.now())
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

	// SameSite=Lax, never Strict: the launch POST is cross-site by design
	// (portal and session are different sites), and a Strict cookie is not
	// sent on the 303 top-level redirect that follows it — the desktop
	// would load "unauthorized" (launch regression).
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    s.id,
		Path:     "/",
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	w.Header().Set("Location", CleanPath)
	w.WriteHeader(http.StatusSeeOther)
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
