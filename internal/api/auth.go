package api

import (
	"container/list"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/tinyorbitvn/tinycdi/internal/api/loginstate"
	"github.com/tinyorbitvn/tinycdi/internal/observability"
	"github.com/tinyorbitvn/tinycdi/internal/store"
)

// AuthConfig configures the OIDC authorization-code + PKCE login flow and the
// server-side session lifecycle.
type AuthConfig struct {
	// Issuer is the expected OIDC issuer URL. ID tokens whose iss claim does
	// not match exactly are rejected by go-oidc discovery verification.
	Issuer string
	// ClientID is the expected aud claim.
	ClientID string
	// ClientSecret is optional; the flow uses PKCE (S256) so public clients
	// work without a secret.
	ClientSecret string
	// RedirectURL is the callback URL registered at the IdP
	// (e.g. https://portal.example/auth/callback).
	RedirectURL string
	// Scopes requested at the IdP. Defaults to ["openid"].
	Scopes []string

	// SessionCookieName defaults to "__Host-tcdi_session" (host-only:
	// the cookie is set without a Domain attribute so the browser scopes it
	// to the API host exactly). Secure + HttpOnly + SameSite=Lax.
	SessionCookieName string
	// LoginCookieName defaults to "__Host-tcdi_login". It carries the
	// AEAD-sealed login state that binds an in-flight OIDC login to the
	// initiating browser (SEC-03): LoginHandler sets it, CallbackHandler
	// requires it. Secure + HttpOnly + SameSite=Lax, Path=/,
	// Max-Age=PendingTTL (600 s by default).
	LoginCookieName string
	// CSRFHeader is the header the CSRF middleware reads.
	// Defaults to "X-CSRF-Token".
	CSRFHeader string
	// SameSite for the session cookie. Defaults to SameSite=Lax: the portal
	// is a separate origin from the API and OIDC top-level GET navigations
	// must still carry the session; cross-site POSTs remain blocked and the
	// synchronizer CSRF token independently guards state changes.
	SameSite http.SameSite
	// SecureCookies controls the Secure flag. Defaults to true; tests may
	// set it false for httptest servers, release builds must not.
	SecureCookies *bool

	// AbsoluteTimeout is the hard cap on a session's lifetime from
	// creation (default 12h). The sliding idle timeout is not configured
	// here: it is owned by the session store (-session-idle /
	// TCDI_SESSION_IDLE -> store.NewSessionStore), which enforces it on
	// every read.
	AbsoluteTimeout time.Duration
	// PendingTTL bounds how long a login attempt (state/nonce/PKCE
	// verifier) stays valid (default 10m). It is sealed into the login
	// cookie as the State.Expires timestamp and caps the cookie Max-Age.
	PendingTTL time.Duration

	// LoginSealer seals in-flight login state into the login cookie so any
	// replica holding the keys can complete the login — there is no
	// server-side pending-login state to bound or lose (SEC-12). Required.
	LoginSealer *loginstate.Sealer

	// TenantClaim / GroupsClaim name the ID-token claims that carry tenant
	// membership and group membership. Defaults: "tenant_id", "groups".
	TenantClaim string
	GroupsClaim string
	// RequiredGroups, when non-empty, additionally restricts login to
	// principals whose Groups claim contains at least one listed group.
	// The match is exact — the Keycloak full-path form "/platform-admins"
	// does not satisfy "platform-admins". Empty disables the gate.
	RequiredGroups []string

	// PostLoginRedirect is where the callback sends the browser after a
	// session is established. Default "/".
	PostLoginRedirect string

	// EndSession turns on RP-initiated logout (chart oidc.endSession): when
	// the provider's discovery document advertises end_session_endpoint,
	// POST /v1/logout answers with the URL that ends the provider session
	// too. Off, sign-out only ends the portal session.
	EndSession bool
	// PostLogoutRedirect is sent as post_logout_redirect_uri on the
	// end-session URL (chart oidc.postLogoutRedirect). Empty omits it: the
	// URI must be registered at the provider, so it is opt-in and the
	// provider then shows its own logged-out page. Must be an absolute
	// https URL (http only for loopback dev hosts).
	PostLogoutRedirect string
}

func (c *AuthConfig) withDefaults() {
	if c.SessionCookieName == "" {
		c.SessionCookieName = "__Host-tcdi_session"
	}
	if c.LoginCookieName == "" {
		c.LoginCookieName = "__Host-tcdi_login"
	}
	if c.CSRFHeader == "" {
		c.CSRFHeader = "X-CSRF-Token"
	}
	if c.SameSite == 0 {
		c.SameSite = http.SameSiteLaxMode
	}
	if c.SecureCookies == nil {
		t := true
		c.SecureCookies = &t
	}
	if c.AbsoluteTimeout == 0 {
		c.AbsoluteTimeout = 12 * time.Hour
	}
	if c.PendingTTL == 0 {
		c.PendingTTL = 10 * time.Minute
	}
	if c.TenantClaim == "" {
		c.TenantClaim = "tenant_id"
	}
	if c.GroupsClaim == "" {
		c.GroupsClaim = "groups"
	}
	if c.PostLoginRedirect == "" {
		c.PostLoginRedirect = "/"
	}
	if len(c.Scopes) == 0 {
		c.Scopes = []string{oidc.ScopeOpenID}
	}
}

// Session is a server-side authenticated session. The browser only ever holds
// the opaque session ID in a host-only cookie; ID/access tokens are never sent
// to the client, never persisted here, and never logged.
//
// Credential forms (SEC-27): Session.ID is the raw session ID while the
// session is in flight, but stores persist it only as a SHA-256 digest.
// The synchronizer CSRF token is derived from the session ID (csrfTokenFor,
// P1) and never stored.
type Session struct {
	ID        string
	Principal Principal
	// IDToken is the raw OIDC id_token retained so logout can send
	// id_token_hint (the provider then skips its own confirmation page).
	// It is only ever persisted AEAD-sealed (store); it is never written to
	// a log line or an API response (V3.24).
	IDToken   string
	CreatedAt time.Time
	// LastSeenAt drives the sliding idle timeout; refreshed by store reads.
	LastSeenAt time.Time
	// ExpiresAt is the absolute expiry set at creation.
	ExpiresAt time.Time
}

// ErrSessionNotFound is returned by SessionStore.Get when the ID is unknown or
// the session has expired (idle or absolute).
var ErrSessionNotFound = errors.New("api: session not found")

// SessionStore persists sessions server-side. The in-memory implementation is
// suitable for tests and single-replica dev; production wiring (store)
// implements the same interface over Postgres.
type SessionStore interface {
	Save(ctx context.Context, s *Session) error
	// Get returns the session and slides its idle deadline. Expired or
	// unknown IDs return ErrSessionNotFound.
	Get(ctx context.Context, id string) (*Session, error)
	// Peek returns the session without writing last_seen_at — the read
	// passive endpoints use so polling never extends the idle window (P4).
	// Expired or unknown IDs return ErrSessionNotFound.
	Peek(ctx context.Context, id string) (*Session, error)
	// TouchPrincipal slides last_seen_at for the principal's sessions that
	// are still inside the idle window and before their absolute expiry
	// (D18). principal is the "issuer|subject" owner string. Returns the
	// number of sessions touched.
	TouchPrincipal(ctx context.Context, principal string) (int64, error)
	// TouchSessionDigest slides last_seen_at for exactly one session — the
	// row keyed by digestHex (hex of SHA-256(session id), the form a lease
	// records) — while it is still inside the idle window and before its
	// absolute expiry (SR-1-F3). Returns the number of sessions touched
	// (0 or 1).
	TouchSessionDigest(ctx context.Context, digestHex string) (int64, error)
	Delete(ctx context.Context, id string) error
}

// InMemorySessionStore is a SessionStore backed by a map.
type InMemorySessionStore struct {
	mu       sync.Mutex
	sessions map[string]*Session
	idle     time.Duration
	now      func() time.Time
}

func NewInMemorySessionStore(idle time.Duration) *InMemorySessionStore {
	return &InMemorySessionStore{
		sessions: make(map[string]*Session),
		idle:     idle,
		now:      time.Now,
	}
}

// WithClock overrides the clock; tests only.
func (s *InMemorySessionStore) WithClock(now func() time.Time) *InMemorySessionStore {
	s.now = now
	return s
}

func (s *InMemorySessionStore) Save(_ context.Context, sess *Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *sess
	s.sessions[sessionKey(sess.ID)] = &cp
	return nil
}

// peekLocked returns the live session or nil — no last_seen_at write. The
// caller holds s.mu; expired sessions are dropped opportunistically.
func (s *InMemorySessionStore) peekLocked(key string, now time.Time) *Session {
	sess, ok := s.sessions[key]
	if !ok {
		return nil
	}
	if !sess.ExpiresAt.IsZero() && !now.Before(sess.ExpiresAt) {
		delete(s.sessions, key)
		return nil
	}
	if s.idle > 0 && now.Sub(sess.LastSeenAt) >= s.idle {
		delete(s.sessions, key)
		return nil
	}
	return sess
}

func (s *InMemorySessionStore) Get(_ context.Context, id string) (*Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.peekLocked(sessionKey(id), s.now())
	if sess == nil {
		return nil, ErrSessionNotFound
	}
	sess.LastSeenAt = s.now()
	cp := *sess
	return &cp, nil
}

// Peek returns the session without writing last_seen_at (P4).
func (s *InMemorySessionStore) Peek(_ context.Context, id string) (*Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.peekLocked(sessionKey(id), s.now())
	if sess == nil {
		return nil, ErrSessionNotFound
	}
	cp := *sess
	return &cp, nil
}

// TouchPrincipal slides last_seen_at for the principal's live sessions —
// those still inside the idle window and before their absolute expiry
// (D18). principal is the "issuer|subject" owner string.
func (s *InMemorySessionStore) TouchPrincipal(_ context.Context, principal string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int64
	now := s.now()
	for key, sess := range s.sessions {
		if sess.Principal.Owner() != principal {
			continue
		}
		if s.peekLocked(key, now) == nil {
			continue // expired sessions stay dead — input never revives them
		}
		sess.LastSeenAt = now
		n++
	}
	return n, nil
}

// TouchSessionDigest slides last_seen_at for the single session the
// digestHex row key names (SR-1-F3) — same liveness guards as
// TouchPrincipal, so an expired session is never revived.
func (s *InMemorySessionStore) TouchSessionDigest(_ context.Context, digestHex string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.peekLocked(digestHex, s.now())
	if sess == nil {
		return 0, nil
	}
	sess.LastSeenAt = s.now()
	return 1, nil
}

func (s *InMemorySessionStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, sessionKey(id))
	return nil
}

// SessionRevoker destroys the session-layer material a portal session
// minted (threat-model S17): its active connection leases and its still
// outstanding launch tickets, revoked at the shared lease store so every
// gateway replica's renew loop observes the death within one renew cycle
// and a replayed workspace cookie resolves to a dead lease on any replica.
// The returned count is informational (audit); a nil revoker leaves
// sign-out as the portal-session-only destroy it was before (tests,
// single-purpose embeds).
type SessionRevoker interface {
	// RevokePortalSession revokes every active lease bound to the digest
	// of portalSessionID plus its unconsumed launch tickets, and returns
	// the number of leases revoked.
	RevokePortalSession(ctx context.Context, portalSessionID string) (int, error)
}

// Authenticator implements the OIDC login/logout endpoints and exposes the
// session accessors the middleware needs.
type Authenticator struct {
	cfg       *AuthConfig
	verifier  *oidc.IDTokenVerifier
	oauth2    oauth2.Config
	sessions  SessionStore
	directory Directory
	metrics   *observability.Metrics
	revoker   SessionRevoker
	// principalRevoker backs POST /v1/me/sessions:revoke-all (ADR 0007);
	// nil leaves that endpoint answering 503 — logout is unaffected.
	principalRevoker PrincipalRevoker
	auditSink        observability.AuditSink
	// endSessionEndpoint is the provider's discovered end_session_endpoint,
	// kept only when EndSession is on and the value is a safe absolute URL.
	// It is the only source of the sign-out navigation target.
	endSessionEndpoint string

	log *slog.Logger
	now func() time.Time
}

// NewAuthenticator runs OIDC discovery on cfg.Issuer and returns a ready
// Authenticator. Discovery happens once at startup; JWKS keys are fetched and
// cached by go-oidc's remote key set.
func NewAuthenticator(ctx context.Context, cfg AuthConfig, sessions SessionStore, log *slog.Logger) (*Authenticator, error) {
	cfg.withDefaults()
	if cfg.Issuer == "" || cfg.ClientID == "" || cfg.RedirectURL == "" {
		return nil, errors.New("api: AuthConfig requires Issuer, ClientID and RedirectURL")
	}
	for _, g := range cfg.RequiredGroups {
		if strings.TrimSpace(g) == "" {
			return nil, errors.New("api: AuthConfig RequiredGroups entries must be non-empty")
		}
	}
	if sessions == nil {
		return nil, errors.New("api: SessionStore is required")
	}
	if cfg.LoginSealer == nil {
		return nil, errors.New("api: AuthConfig requires LoginSealer")
	}
	if log == nil {
		log = slog.Default()
	}
	if cfg.PostLogoutRedirect != "" && !isHTTPSOrLoopbackURL(cfg.PostLogoutRedirect) {
		return nil, errors.New("api: AuthConfig PostLogoutRedirect must be an absolute https URL (http only for loopback hosts)")
	}
	provider, err := oidc.NewProvider(ctx, cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("api: OIDC discovery: %w", err)
	}
	var endSession string
	if cfg.EndSession {
		var disc struct {
			EndSessionEndpoint string `json:"end_session_endpoint"`
		}
		if err := provider.Claims(&disc); err != nil {
			return nil, fmt.Errorf("api: OIDC discovery claims: %w", err)
		}
		if isHTTPSOrLoopbackURL(disc.EndSessionEndpoint) {
			endSession = disc.EndSessionEndpoint
		} else if disc.EndSessionEndpoint != "" {
			log.Warn("oidc end_session_endpoint ignored: not an absolute https URL (http only for loopback hosts) without credentials")
		}
	}
	a := &Authenticator{
		cfg:      &cfg,
		verifier: provider.Verifier(&oidc.Config{ClientID: cfg.ClientID}),
		oauth2: oauth2.Config{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			RedirectURL:  cfg.RedirectURL,
			Endpoint:     provider.Endpoint(),
			Scopes:       cfg.Scopes,
		},
		sessions:           sessions,
		endSessionEndpoint: endSession,
		log:                log,
		now:                time.Now,
	}
	return a, nil
}

// WithDirectory attaches the principal directory that logins upsert
// (display names for tenant views — best effort, never authz input).
func (a *Authenticator) WithDirectory(d Directory) *Authenticator {
	a.directory = d
	return a
}

// WithMetrics attaches the platform metric set; completed login callbacks
// count toward tinycdi_logins_total (E8). Nil disables the count.
func (a *Authenticator) WithMetrics(m *observability.Metrics) *Authenticator {
	a.metrics = m
	return a
}

// WithSessionRevoker attaches the store-level revoker sign-out calls to end
// the session's live desktop material (S17) — broker.PublicRevoker in the
// wired backend. Nil disables the call.
func (a *Authenticator) WithSessionRevoker(r SessionRevoker) *Authenticator {
	a.revoker = r
	return a
}

// WithAuditSink attaches the sink the dedicated session.revoke audit record
// is written to at sign-out — the same action name the gateway's control
// revoke emits. The request-level audit trail stays in the middleware;
// this carries only the revocation outcome. Nil disables the event.
func (a *Authenticator) WithAuditSink(s observability.AuditSink) *Authenticator {
	a.auditSink = s
	return a
}

func (a *Authenticator) SessionStore() SessionStore { return a.sessions }
func (a *Authenticator) SessionCookieName() string  { return a.cfg.SessionCookieName }
func (a *Authenticator) LoginCookieName() string    { return a.cfg.LoginCookieName }
func (a *Authenticator) CSRFHeader() string         { return a.cfg.CSRFHeader }

// LoginHandler starts the authorization-code + PKCE flow: it generates state,
// nonce and a PKCE verifier, seals them together with the login's expiry into
// the __Host- login cookie, and redirects to the IdP authorization endpoint.
// Keeping the login state in a sealed cookie (instead of process memory)
// binds the login to the initiating browser (SEC-03) and lets any replica
// holding the keys complete the flow.
func (a *Authenticator) LoginHandler(w http.ResponseWriter, r *http.Request) {
	state, err := randToken(32)
	if err != nil {
		writeError(w, r, CodeInternal, "could not start login")
		return
	}
	nonce, err := randToken(32)
	if err != nil {
		writeError(w, r, CodeInternal, "could not start login")
		return
	}
	verifier, err := randToken(48) // 64-char base64url PKCE verifier (43..128 allowed)
	if err != nil {
		writeError(w, r, CodeInternal, "could not start login")
		return
	}
	token, err := a.cfg.LoginSealer.Seal(loginstate.State{
		OAuthState: state,
		Nonce:      nonce,
		Verifier:   verifier,
		Expires:    a.now().Add(a.cfg.PendingTTL).Unix(),
	})
	if err != nil {
		writeError(w, r, CodeInternal, "could not start login")
		return
	}

	authURL := a.oauth2.AuthCodeURL(state,
		oauth2.S256ChallengeOption(verifier),
		oauth2.SetAuthURLParam("nonce", nonce),
	)
	// Deliberately log only that a login was initiated — never the state,
	// nonce, verifier, or the authorization URL carrying them.
	a.log.Debug("oidc login started", "request_id", RequestIDFromContext(r.Context()))
	// Max-Age=600 per the SEC-03 spec, and never longer than the pending TTL.
	maxAge := int(a.cfg.PendingTTL.Seconds())
	if maxAge > 600 {
		maxAge = 600
	}
	http.SetCookie(w, a.loginCookie(token, maxAge))
	http.Redirect(w, r, authURL, http.StatusFound)
}

// CallbackHandler completes the flow. It opens the sealed login cookie
// (single use — a consumed code or a replayed/expired cookie is rejected),
// binds the callback's state parameter to the sealed state, exchanges the
// code with the sealed PKCE verifier, verifies the ID token (issuer,
// audience, expiry, signature via go-oidc) and its nonce, enforces tenant
// membership, deletes any pre-existing session cookie (fixation), and issues
// a fresh rotated session ID.
func (a *Authenticator) CallbackHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	if a.metrics != nil {
		// Every completed login attempt counts once under a bounded
		// outcome derived from the response class (E8): success for the
		// issue-and-redirect path, error for platform-side failures,
		// denied for every client-side refusal.
		rec := &statusRecorder{ResponseWriter: w}
		w = rec
		defer func() {
			a.metrics.IncLogin(loginOutcome(statusOrOK(rec.status)))
		}()
	}

	if e := r.URL.Query().Get("error"); e != "" {
		writeError(w, r, CodeUnauthenticated, "identity provider returned an error")
		return
	}
	state := r.URL.Query().Get("state")
	code := r.URL.Query().Get("code")
	if state == "" || code == "" {
		writeError(w, r, CodeInvalidRequest, "missing code or state")
		return
	}

	// SEC-03: the state alone must not suffice — the browser that started
	// this login holds the __Host-tcdi_login cookie carrying the sealed
	// state, nonce and PKCE verifier. A missing, tampered, expired or
	// foreign-key cookie fails closed, exactly like an unknown state.
	var st loginstate.State
	if c, err := r.Cookie(a.cfg.LoginCookieName); err == nil {
		st, err = a.cfg.LoginSealer.Open(c.Value, a.now())
		if err != nil {
			writeError(w, r, CodeUnauthenticated, "unknown or expired login state")
			return
		}
	} else {
		writeError(w, r, CodeUnauthenticated, "unknown or expired login state")
		return
	}
	if subtle.ConstantTimeCompare([]byte(st.OAuthState), []byte(state)) != 1 {
		writeError(w, r, CodeUnauthenticated, "unknown or expired login state")
		return
	}

	tok, err := a.oauth2.Exchange(ctx, code, oauth2.VerifierOption(st.Verifier))
	if err != nil {
		writeError(w, r, CodeUnauthenticated, "authorization code exchange failed")
		return
	}
	rawID, ok := tok.Extra("id_token").(string)
	if !ok || rawID == "" {
		writeError(w, r, CodeUnauthenticated, "token response had no id_token")
		return
	}
	idToken, err := a.verifier.Verify(ctx, rawID)
	if err != nil {
		// Covers wrong issuer, wrong audience, expired token and bad
		// signature. The error and the token are never logged.
		writeError(w, r, CodeUnauthenticated, "ID token verification failed")
		return
	}

	var claims map[string]interface{}
	if err := idToken.Claims(&claims); err != nil {
		writeError(w, r, CodeUnauthenticated, "could not parse ID token claims")
		return
	}
	if nonce, _ := claims["nonce"].(string); nonce == "" || nonce != st.Nonce {
		writeError(w, r, CodeUnauthenticated, "ID token nonce does not match login request")
		return
	}

	// The login state has been fully validated — consume the sealed cookie
	// so a stale value cannot linger in the jar.
	http.SetCookie(w, a.loginCookie("", -1))

	principal := Principal{
		Issuer:      idToken.Issuer,
		Subject:     idToken.Subject,
		TenantID:    stringClaim(claims, a.cfg.TenantClaim),
		Groups:      stringsClaim(claims, a.cfg.GroupsClaim),
		DisplayName: displayNameClaim(claims),
		Email:       displayClaim(stringClaim(claims, "email"), maxEmailLen),
	}
	if principal.TenantID == "" {
		writeError(w, r, CodeForbidden, "no tenant membership for this account")
		return
	}
	if !a.groupsAllowed(principal) {
		// Denials name only the pseudonymous actor — never the claimed
		// groups, the raw subject, or the token (audit redaction rules).
		a.log.Warn("oidc login denied: required group missing",
			"request_id", RequestIDFromContext(ctx),
			"actor", observability.ActorRef(principal.Issuer, principal.Subject),
		)
		writeError(w, r, CodeForbidden, "account is not a member of a required group")
		return
	}

	// Session fixation: if the browser already carries a session cookie,
	// destroy that session. The new session always gets a fresh random ID.
	if old, err := r.Cookie(a.cfg.SessionCookieName); err == nil && old.Value != "" {
		_ = a.sessions.Delete(ctx, old.Value)
	}

	sess := &Session{
		ID:         mustRandToken(32),
		Principal:  principal,
		IDToken:    rawID,
		CreatedAt:  a.now(),
		LastSeenAt: a.now(),
		ExpiresAt:  a.now().Add(a.cfg.AbsoluteTimeout),
	}
	if err := a.sessions.Save(ctx, sess); err != nil {
		writeError(w, r, CodeInternal, "could not create session")
		return
	}

	// Display directory (names for tenant views) is best effort: a failure
	// is logged and never blocks the login.
	if a.directory != nil {
		if err := a.directory.Remember(ctx, principal.TenantID, store.DirectoryEntry{
			OwnerRef:    principal.Owner(),
			Subject:     principal.Subject,
			DisplayName: principal.DisplayName,
			Email:       principal.Email,
		}); err != nil {
			a.log.Warn("principal directory update failed",
				"request_id", RequestIDFromContext(ctx), "err", err)
		}
	}

	http.SetCookie(w, a.sessionCookie(sess.ID, int(a.cfg.AbsoluteTimeout.Seconds())))
	a.log.Info("oidc login succeeded",
		"request_id", RequestIDFromContext(ctx),
		"actor", observability.ActorRef(principal.Issuer, principal.Subject),
		"tenant", principal.TenantID,
	)
	http.Redirect(w, r, a.cfg.PostLoginRedirect, http.StatusFound)
}

// loginOutcome maps a callback response status to the bounded
// tinycdi_logins_total outcome: <400 success, >=500 error, anything else
// denied.
func loginOutcome(status int) string {
	switch {
	case status >= 500:
		return "error"
	case status >= 400:
		return "denied"
	default:
		return "success"
	}
}

// LogoutResult is the body of POST /v1/logout when the provider session can
// be ended too (openapi LogoutResult).
type LogoutResult struct {
	// EndSessionURL is the provider's end-session endpoint with client_id
	// (and post_logout_redirect_uri when configured). The portal navigates
	// the browser there.
	EndSessionURL string `json:"endSessionUrl"`
}

// sessionRevokeTimeout bounds the lease-store call sign-out makes for the
// session's leases and tickets: a wedged store must not hang the logout
// response.
const sessionRevokeTimeout = 5 * time.Second

// logoutRetryAfter is the Retry-After hint on the 503 a sign-out answers
// when the session store could not delete the session row: a transient
// store failure, safe to retry once the store is back.
const logoutRetryAfter = "5"

// LogoutHandler destroys the server-side session and expires all login- and
// session-scoped cookies. Route it behind RequireAuth + RequireCSRF.
//
// Sign-out also ends the session's reach over the desktop layer (S17): the
// session row dies first — visible to ticket redemption's session check as
// early as possible — then the session-bound leases and outstanding tickets
// are revoked at the store (best effort, bounded). A revoked lease fails
// every replica's next renew, closing the live stream within one renew
// cycle, and its session_digest dies with it, so a copied workspace cookie
// can never rehydrate anywhere. A revoke failure is logged, counted and
// audited but never kept back the sign-out: the portal cookie is cleared
// and the session destroyed regardless.
//
// The session-row delete is the point of no return and is ordered first for
// exactly that reason: when it fails the handler answers a retryable 503
// (UNAVAILABLE + Retry-After) and tears NOTHING down — no cookie expiry, no
// lease or ticket revoke. The answer "not signed out, retry" then matches
// the world: the session still validates and its desktops are still alive.
// Revoking material first would leave a live session with dead streams — a
// half-revoked state a 503 would be lying about — and expiring the cookie
// would tell the browser it is signed out while a copied cookie still works.
// A retry re-runs the whole destroy; the material revoke is a no-op re-run
// when it already committed.
//
// RP-initiated logout: when EndSession is on and the provider advertises
// end_session_endpoint, it answers 200 {"endSessionUrl"} so the portal can
// continue there and end the provider session — otherwise the next visit
// signs the user straight back in. Otherwise 204. The URL is built only from
// discovery and configuration; nothing in the request reaches it (no open
// redirect). The session's retained ID token supplies id_token_hint, which
// is what makes the provider skip its own confirmation page; a session
// without one (pre-migration row, rotated key) falls back to client_id.
func (a *Authenticator) LogoutHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var idToken string
	if c, err := r.Cookie(a.cfg.SessionCookieName); err == nil && c.Value != "" {
		// Peek, not Get: the session is destroyed next, so the idle slide
		// would be a dead write. A missing or unopenable token never fails
		// the sign-out.
		if sess, err := a.sessions.Peek(ctx, c.Value); err == nil {
			idToken = sess.IDToken
		}
		if err := a.sessions.Delete(ctx, c.Value); err != nil {
			a.log.Warn("sign-out: session delete failed",
				"request_id", RequestIDFromContext(ctx), "err", err)
			w.Header().Set("Retry-After", logoutRetryAfter)
			writeError(w, r, CodeUnavailable, "could not sign out; retry")
			return
		}
		a.revokeSessionMaterial(r, c.Value)
	}
	http.SetCookie(w, a.sessionCookie("", -1))
	endSession := a.endSessionURL(idToken)
	if endSession == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(LogoutResult{EndSessionURL: endSession})
}

// revokeSessionMaterial revokes the portal session's session-layer material
// (S17) through the attached SessionRevoker — a no-op when none is wired.
// Best effort by contract: the local sign-out (session row + cookie) is
// already complete, so a store failure only produces the failure records —
// warn log, error metric, failure audit — never a failed response.
func (a *Authenticator) revokeSessionMaterial(r *http.Request, sessionID string) {
	if a.revoker == nil {
		return
	}
	// Detached from the request's cancellation: a client that disconnects
	// mid-logout must not abort the revocation transaction — sign-out was
	// already accepted, so the revoke still runs to its own 5 s bound.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), sessionRevokeTimeout)
	leases, err := a.revoker.RevokePortalSession(ctx, sessionID)
	cancel()
	var actor, tenant string
	if sess, ok := SessionFromContext(r.Context()); ok && sess != nil {
		actor = observability.ActorRef(sess.Principal.Issuer, sess.Principal.Subject)
		tenant = sess.Principal.TenantID
	}
	if err != nil {
		a.log.Warn("sign-out: session-bound revocation failed",
			"request_id", RequestIDFromContext(r.Context()),
			"actor", actor, "err", err)
		if a.metrics != nil {
			a.metrics.IncSessionRevocation("error")
		}
		a.writeRevokeAudit(r, actor, tenant, observability.OutcomeFailure, "revoke_failed", 0)
		return
	}
	if a.metrics != nil {
		a.metrics.IncSessionRevocation("ok")
	}
	a.writeRevokeAudit(r, actor, tenant, observability.OutcomeSuccess, "", leases)
}

// writeRevokeAudit emits the session.revoke audit record for a sign-out
// revocation — the same action name the gateway's control-surface revoke
// uses, so lease-death auditing reads uniformly. leases is the revoked
// count on success; session material never reaches the record (Detail
// keys still pass through observability.RedactDetails on write).
func (a *Authenticator) writeRevokeAudit(r *http.Request, actor, tenant string, outcome observability.AuditOutcome, errCode string, leases int) {
	if a.auditSink == nil {
		return
	}
	var details map[string]string
	if outcome == observability.OutcomeSuccess {
		details = map[string]string{"leases_revoked": strconv.Itoa(leases)}
	}
	_ = a.auditSink.WriteAudit(r.Context(), observability.AuditEvent{
		Actor:     actorOrAnonymous(actor),
		Action:    "session.revoke",
		Tenant:    tenant,
		RequestID: RequestIDFromContext(r.Context()),
		Outcome:   outcome,
		ErrorCode: errCode,
		Details:   details,
	})
}

// endSessionURL assembles the RP-initiated logout URL, or "" when sign-out
// should stay local. idToken is the session's retained ID token; "" emits
// no id_token_hint (the provider may then show its own confirmation page).
func (a *Authenticator) endSessionURL(idToken string) string {
	if a.endSessionEndpoint == "" {
		return ""
	}
	u, err := url.Parse(a.endSessionEndpoint)
	if err != nil {
		return ""
	}
	q := u.Query()
	if idToken != "" {
		q.Set("id_token_hint", idToken)
	}
	q.Set("client_id", a.cfg.ClientID)
	if a.cfg.PostLogoutRedirect != "" {
		q.Set("post_logout_redirect_uri", a.cfg.PostLogoutRedirect)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// isHTTPSOrLoopbackURL reports whether s is an absolute URL the sign-out
// flow may hand to the browser: https always; http only when the host is
// loopback (localhost / 127.0.0.0/8 / ::1) — the codebase's dev convention
// is a plain-http IdP on loopback (oidctest, a dev Keycloak), and plaintext
// to anywhere else is never a legitimate navigation target. Embedded
// credentials are rejected.
func isHTTPSOrLoopbackURL(s string) bool {
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || u.User != nil {
		return false
	}
	if u.Scheme == "https" {
		return true
	}
	return u.Scheme == "http" && isLoopbackHost(u.Hostname())
}

// isLoopbackHost reports whether host names a loopback address: the literal
// "localhost" or an IP in the loopback range. Strict parsing — non-canonical
// IP spellings (octal, hex, v4-in-v6 mapped) never count as loopback.
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.Unmap().IsLoopback()
}

// sessionCookie builds the host-only session cookie: no Domain attribute
// (browser scopes it to the API host exactly — required because portal and
// session origins live on separate registrable domains), Secure, HttpOnly,
// SameSite per config (default Lax).
func (a *Authenticator) sessionCookie(id string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name:     a.cfg.SessionCookieName,
		Value:    id,
		Path:     "/",
		Secure:   *a.cfg.SecureCookies,
		HttpOnly: true,
		SameSite: a.cfg.SameSite,
		MaxAge:   maxAge,
	}
}

// loginCookie carries the AEAD-sealed OIDC login state that binds the
// attempt to the initiating browser (SEC-03). __Host- shape: Secure,
// HttpOnly, Path=/, no Domain.
func (a *Authenticator) loginCookie(value string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name:     a.cfg.LoginCookieName,
		Value:    value,
		Path:     "/",
		Secure:   *a.cfg.SecureCookies,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   maxAge,
	}
}

// inputTouchMinInterval bounds how often desktop input writes to the session
// store: one slide per principal per minute is enough against a 30-minute
// idle window, and the bound keeps a busy stream from turning into a write
// flood.
const inputTouchMinInterval = time.Minute

// inputThrottleMaxEntries bounds the per-principal throttle map. Every
// distinct principal that produces desktop input adds an entry; the LRU keeps
// the backend's memory flat however many principals pass through. An evicted
// principal at worst gets one extra (idempotent) idle touch.
const inputThrottleMaxEntries = 10_000

// InputHook returns the broker input hook (broker.WithInputHook): each
// recorded "input" event slides the portal idle timer of the session the
// input arrived under — the lease's bound portal_session_digest — throttled
// to one store write per key per minute (SR-1-F3). A legacy lease carrying
// no digest falls back to the principal-wide touch. Input never revives a
// session that already expired; the store's WHERE clauses exclude sessions
// outside the idle window (D18).
func (a *Authenticator) InputHook() func(ctx context.Context, principal, sessionDigest string) {
	hook, _ := a.newInputHook(inputThrottleMaxEntries)
	return hook
}

// newInputHook builds the throttled hook over an LRU of at most max
// keys — a session digest when the lease names one, else the principal —
// so a multi-session principal throttles per session, not per identity;
// size reports the current entry count (tests).
func (a *Authenticator) newInputHook(max int) (hook func(ctx context.Context, principal, sessionDigest string), size func() int) {
	var mu sync.Mutex
	order := list.New() // front = most recently seen; elements are *throttleEntry
	byKey := map[string]*list.Element{}
	hook = func(ctx context.Context, principal, sessionDigest string) {
		key := "p:" + principal
		if sessionDigest != "" {
			key = "s:" + sessionDigest
		}
		now := a.now()
		mu.Lock()
		if el, ok := byKey[key]; ok {
			e := el.Value.(*throttleEntry)
			order.MoveToFront(el)
			if now.Sub(e.at) < inputTouchMinInterval {
				mu.Unlock()
				return
			}
			e.at = now
		} else {
			byKey[key] = order.PushFront(&throttleEntry{key: key, at: now})
			if order.Len() > max {
				oldest := order.Back()
				order.Remove(oldest)
				delete(byKey, oldest.Value.(*throttleEntry).key)
			}
		}
		mu.Unlock()
		var err error
		if sessionDigest != "" {
			_, err = a.sessions.TouchSessionDigest(ctx, sessionDigest)
		} else {
			_, err = a.sessions.TouchPrincipal(ctx, principal)
		}
		if err != nil {
			a.log.Warn("session idle touch failed", "err", err)
		}
	}
	size = func() int {
		mu.Lock()
		defer mu.Unlock()
		return order.Len()
	}
	return hook, size
}

// throttleEntry is one LRU element: when this key's session(s) were last
// touched — "s:"+digest keys a single bound session, "p:"+principal the
// legacy principal-wide touch.
type throttleEntry struct {
	key string
	at  time.Time
}

// groupsAllowed enforces RequiredGroups: with none configured the gate is
// off; otherwise at least one required group must appear verbatim in the
// verified claim. A missing or malformed claim parses to no groups and is
// denied — the gate never errors open.
func (a *Authenticator) groupsAllowed(p Principal) bool {
	if len(a.cfg.RequiredGroups) == 0 {
		return true
	}
	for _, g := range a.cfg.RequiredGroups {
		if p.InGroup(g) {
			return true
		}
	}
	return false
}

func stringClaim(claims map[string]interface{}, name string) string {
	s, _ := claims[name].(string)
	return s
}

func stringsClaim(claims map[string]interface{}, name string) []string {
	switch v := claims[name].(type) {
	case []interface{}:
		out := make([]string, 0, len(v))
		for _, it := range v {
			if s, ok := it.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case []string:
		return v
	case string:
		return []string{v}
	}
	return nil
}

// Bounds for display-only identity claims: they are rendered in the portal
// and stored in the principal directory, so they are trimmed and capped —
// a hostile or buggy IdP cannot smuggle unbounded strings.
const (
	maxDisplayNameLen = 128
	maxEmailLen       = 254
)

// displayClaim normalizes a display-only claim value: trimmed, rune-capped
// at max. The result carries no authorization meaning.
func displayClaim(s string, max int) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > max {
		s = string(r[:max])
	}
	return s
}

// displayNameClaim picks the caller's display name from the verified ID
// token: the `name` claim, falling back to `preferred_username`.
func displayNameClaim(claims map[string]interface{}) string {
	if n := stringClaim(claims, "name"); n != "" {
		return displayClaim(n, maxDisplayNameLen)
	}
	return displayClaim(stringClaim(claims, "preferred_username"), maxDisplayNameLen)
}

// sessionKey is the lookup key a store persists for a session ID: hex of
// SHA-256(id). A store (or dump) read then yields digests, never usable
// session IDs — same construction the broker uses for tickets (SEC-27).
// Mirrored by internal/store.sessions.go; keep the digest identical.
func sessionKey(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:])
}

// csrfTokenFor derives the session's synchronizer CSRF token:
// base64url(HMAC-SHA256(key = raw session ID, "tcdi-csrf-v2")). The token is
// never stored — /v1/me recomputes it for the response and RequireCSRF
// recomputes it for the compare (P1), so the credential exists only in the
// cookie's session ID.
func csrfTokenFor(sessionID string) string {
	m := hmac.New(sha256.New, []byte(sessionID))
	m.Write([]byte("tcdi-csrf-v2"))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func randToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func mustRandToken(n int) string {
	t, err := randToken(n)
	if err != nil {
		panic("api: crypto/rand unavailable: " + err.Error())
	}
	return t
}
