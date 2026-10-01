package api

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
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
	// SessionOriginCookieName defaults to "tcdi_session_origin" — the v0.1
	// cookie name. In v0.2 the cookie is never set; login and logout only
	// send a deletion (Max-Age=0) for it so upgraded browsers drop it (D17).
	SessionOriginCookieName string
	// SessionOrigin is retained for configuration compatibility only —
	// v0.2 derives the session origin per workspace from the session domain
	// and never publishes it in a cookie.
	SessionOrigin string
	// CSRFCookieName defaults to "tcdi_csrf" — the v0.1 cookie name. In v0.2
	// the CSRF token is derived from the session ID (P1) and returned by
	// GET /v1/me; this cookie is never set, only deleted on login/logout so
	// upgraded browsers drop it.
	CSRFCookieName string
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

	// IdleTimeout is the sliding inactivity lifetime of a session
	// (default 30m). AbsoluteTimeout is the hard cap from creation
	// (default 12h).
	IdleTimeout     time.Duration
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
	// AllowedTenants, when non-empty, restricts login to principals whose
	// TenantID is listed. TenantID empty is always rejected.
	AllowedTenants []string
	// RequiredGroups, when non-empty, additionally restricts login to
	// principals whose Groups claim contains at least one listed group.
	// The match is exact — the Keycloak full-path form "/platform-admins"
	// does not satisfy "platform-admins". Empty disables the gate.
	RequiredGroups []string

	// PostLoginRedirect is where the callback sends the browser after a
	// session is established. Default "/".
	PostLoginRedirect string
}

func (c *AuthConfig) withDefaults() {
	if c.SessionCookieName == "" {
		c.SessionCookieName = "__Host-tcdi_session"
	}
	if c.LoginCookieName == "" {
		c.LoginCookieName = "__Host-tcdi_login"
	}
	if c.SessionOriginCookieName == "" {
		c.SessionOriginCookieName = "tcdi_session_origin"
	}
	if c.CSRFCookieName == "" {
		c.CSRFCookieName = "tcdi_csrf"
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
	if c.IdleTimeout == 0 {
		c.IdleTimeout = 30 * time.Minute
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
// CSRFToken is written as "" — since v0.2 the synchronizer token is derived
// from the session ID (csrfTokenFor, P1) and never stored; the column stays
// until v0.3.
type Session struct {
	ID        string
	Principal Principal
	CSRFToken string
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
	// Persist the MAC form of the CSRF token like the Postgres store does:
	// a store read never yields a usable synchronizer token (SEC-27).
	cp.CSRFToken = csrfTokenMAC(sess.ID, sess.CSRFToken)
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

func (s *InMemorySessionStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, sessionKey(id))
	return nil
}

// Authenticator implements the OIDC login/logout endpoints and exposes the
// session accessors the middleware needs.
type Authenticator struct {
	cfg       *AuthConfig
	verifier  *oidc.IDTokenVerifier
	oauth2    oauth2.Config
	sessions  SessionStore
	directory Directory

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
	provider, err := oidc.NewProvider(ctx, cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("api: OIDC discovery: %w", err)
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
		sessions: sessions,
		log:      log,
		now:      time.Now,
	}
	return a, nil
}

// WithDirectory attaches the principal directory that logins upsert
// (display names for tenant views — best effort, never authz input).
func (a *Authenticator) WithDirectory(d Directory) *Authenticator {
	a.directory = d
	return a
}

func (a *Authenticator) SessionStore() SessionStore      { return a.sessions }
func (a *Authenticator) SessionCookieName() string       { return a.cfg.SessionCookieName }
func (a *Authenticator) LoginCookieName() string         { return a.cfg.LoginCookieName }
func (a *Authenticator) SessionOriginCookieName() string { return a.cfg.SessionOriginCookieName }
func (a *Authenticator) CSRFCookieName() string          { return a.cfg.CSRFCookieName }
func (a *Authenticator) CSRFHeader() string              { return a.cfg.CSRFHeader }

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

	url := a.oauth2.AuthCodeURL(state,
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
	// Any browser that still carries the v0.1 non-__Host- cookies drops them
	// on its next login (D17).
	a.expireLegacyCookies(w)
	http.Redirect(w, r, url, http.StatusFound)
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
	if principal.TenantID == "" || !a.tenantAllowed(principal.TenantID) {
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
		CSRFToken:  "", // derived from the session ID on demand (P1); column stays until v0.3
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
	// v0.1 published tcdi_csrf / tcdi_session_origin here; browsers upgraded
	// from v0.1 must drop them (D17).
	a.expireLegacyCookies(w)
	a.log.Info("oidc login succeeded",
		"request_id", RequestIDFromContext(ctx),
		"actor", observability.ActorRef(principal.Issuer, principal.Subject),
		"tenant", principal.TenantID,
	)
	http.Redirect(w, r, a.cfg.PostLoginRedirect, http.StatusFound)
}

// LogoutHandler destroys the server-side session and expires all login- and
// session-scoped cookies. Route it behind RequireAuth + RequireCSRF.
func (a *Authenticator) LogoutHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if c, err := r.Cookie(a.cfg.SessionCookieName); err == nil && c.Value != "" {
		_ = a.sessions.Delete(ctx, c.Value)
	}
	http.SetCookie(w, a.sessionCookie("", -1))
	a.expireLegacyCookies(w)
	w.WriteHeader(http.StatusNoContent)
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

// expireLegacyCookies deletes the two non-__Host- cookies v0.1 published on
// the portal origin — tcdi_csrf and tcdi_session_origin (D17). Login and
// logout both send them so browsers upgraded mid-session drop the legacy
// names regardless of which flow they take.
func (a *Authenticator) expireLegacyCookies(w http.ResponseWriter) {
	for _, name := range []string{a.cfg.CSRFCookieName, a.cfg.SessionOriginCookieName} {
		http.SetCookie(w, &http.Cookie{
			Name:     name,
			Value:    "",
			Path:     "/",
			Secure:   *a.cfg.SecureCookies,
			SameSite: a.cfg.SameSite,
			MaxAge:   -1,
		})
	}
}

// inputTouchMinInterval bounds how often desktop input writes to the session
// store: one slide per principal per minute is enough against a 30-minute
// idle window, and the bound keeps a busy stream from turning into a write
// flood.
const inputTouchMinInterval = time.Minute

// InputHook returns the broker input hook (broker.WithInputHook): each
// recorded "input" event slides the portal idle timer of the lease's
// principal — the Principal.Owner() string "issuer|subject" — throttled to
// one store write per principal per minute. Input never revives a session
// that already expired; the store's TouchPrincipal WHERE clause excludes
// sessions outside the idle window (D18).
func (a *Authenticator) InputHook() func(ctx context.Context, principal string) {
	var mu sync.Mutex
	last := map[string]time.Time{}
	return func(ctx context.Context, principal string) {
		now := a.now()
		mu.Lock()
		if t, ok := last[principal]; ok && now.Sub(t) < inputTouchMinInterval {
			mu.Unlock()
			return
		}
		last[principal] = now
		mu.Unlock()
		if _, err := a.sessions.TouchPrincipal(ctx, principal); err != nil {
			a.log.Warn("session idle touch failed", "err", err)
		}
	}
}

func (a *Authenticator) tenantAllowed(tenant string) bool {
	if len(a.cfg.AllowedTenants) == 0 {
		return true
	}
	for _, t := range a.cfg.AllowedTenants {
		if t == tenant {
			return true
		}
	}
	return false
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

// csrfTokenMAC is the stored credential form of a session's synchronizer
// CSRF token: hex of HMAC-SHA256 keyed by the raw session ID (which is
// itself never persisted — stores hold only its digest). A store/dump read
// therefore reveals neither the token nor a way to compute the MAC
// (SEC-27). Mirrored by internal/store.sessions.go; keep identical.
// Since v0.2 sessions persist CSRFToken = "" (P1), this MACs the empty
// token — the column remains until v0.3 and is no longer consulted.
func csrfTokenMAC(sessionID, token string) string {
	m := hmac.New(sha256.New, []byte(sessionID))
	m.Write([]byte("tcdi-csrf-token\x00"))
	m.Write([]byte(token))
	return hex.EncodeToString(m.Sum(nil))
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
