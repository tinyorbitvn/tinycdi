// Package oidctest provides an in-process fake OIDC issuer for tests:
// discovery (/.well-known/openid-configuration), JWKS, authorization and
// token endpoints. It performs real RS256 signing so the Authenticator under
// test exercises the same verification path as with a production IdP — no
// external IdP is required.
package oidctest

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"time"
)

// Issuer is a fake OIDC provider backed by an httptest server.
type Issuer struct {
	server *httptest.Server
	key    *rsa.PrivateKey
	kid    string

	// ClientID is the aud the issuer mints and the client_id it accepts.
	ClientID string
	// Subject, TenantID and Groups are the default claims put into ID
	// tokens; tests may mutate them or use MutateTokenClaims.
	Subject  string
	TenantID string
	Groups   []string
	// TokenTTL controls the exp claim. Default 1h.
	TokenTTL time.Duration
	// EndSessionEndpoint, when set before the Authenticator runs discovery,
	// is advertised as the discovery document's end_session_endpoint
	// (RP-initiated logout). Empty omits the field, like a provider
	// without RP-initiated logout.
	EndSessionEndpoint string

	mu       sync.Mutex
	authReqs map[string]*authRequest // code -> request
	mutate   func(map[string]any)
	now      func() time.Time

	lastIDToken     string
	lastAccessToken string
}

type authRequest struct {
	nonce     string
	challenge string
	clientID  string
}

// NewIssuer starts a fake issuer on a random local port.
func NewIssuer() (*Issuer, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("oidctest: generate key: %w", err)
	}
	i := &Issuer{
		key:      key,
		kid:      "oidctest-key-1",
		ClientID: "tinycdi-portal",
		Subject:  "user-1",
		TenantID: "tenant-a",
		Groups:   []string{"devs"},
		TokenTTL: time.Hour,
		authReqs: make(map[string]*authRequest),
		now:      time.Now,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", i.discovery)
	mux.HandleFunc("/authorize", i.authorize)
	mux.HandleFunc("/token", i.token)
	mux.HandleFunc("/jwks", i.jwks)
	i.server = httptest.NewServer(mux)
	return i, nil
}

// URL is the issuer identifier (iss) — the httptest server base URL.
func (i *Issuer) URL() string { return i.server.URL }

// Close shuts the issuer down.
func (i *Issuer) Close() { i.server.Close() }

// SetClock overrides the issuer clock; tests only.
func (i *Issuer) SetClock(now func() time.Time) { i.now = now }

// LastIDToken / LastAccessToken return the most recently issued tokens, so
// tests can assert those values never appear in logs.
func (i *Issuer) LastIDToken() string {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.lastIDToken
}

func (i *Issuer) LastAccessToken() string {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.lastAccessToken
}

// MutateTokenClaims registers a hook applied to the ID-token claims map
// before signing on every token exchange. Use it to mint wrong issuer,
// audience, expiry or nonce for negative tests.
func (i *Issuer) MutateTokenClaims(fn func(map[string]any)) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.mutate = fn
}

func (i *Issuer) discovery(w http.ResponseWriter, r *http.Request) {
	doc := map[string]any{
		"issuer":                                i.URL(),
		"authorization_endpoint":                i.URL() + "/authorize",
		"token_endpoint":                        i.URL() + "/token",
		"jwks_uri":                              i.URL() + "/jwks",
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"code_challenge_methods_supported":      []string{"S256"},
	}
	if i.EndSessionEndpoint != "" {
		doc["end_session_endpoint"] = i.EndSessionEndpoint
	}
	writeJSON(w, doc)
}

// authorize validates the request and redirects back with code+state, like a
// real IdP would after user consent.
func (i *Issuer) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("response_type") != "code" || q.Get("client_id") == "" ||
		q.Get("redirect_uri") == "" || q.Get("state") == "" {
		http.Error(w, "invalid authorize request", http.StatusBadRequest)
		return
	}
	code := randomString(24)
	i.mu.Lock()
	i.authReqs[code] = &authRequest{
		nonce:     q.Get("nonce"),
		challenge: q.Get("code_challenge"),
		clientID:  q.Get("client_id"),
	}
	i.mu.Unlock()

	redirect, err := url.Parse(q.Get("redirect_uri"))
	if err != nil {
		http.Error(w, "bad redirect_uri", http.StatusBadRequest)
		return
	}
	rq := redirect.Query()
	rq.Set("code", code)
	rq.Set("state", q.Get("state"))
	redirect.RawQuery = rq.Encode()
	http.Redirect(w, r, redirect.String(), http.StatusFound)
}

// token implements the authorization_code grant including PKCE S256
// verification. A code is single-use.
func (i *Issuer) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	if r.Form.Get("grant_type") != "authorization_code" {
		writeJSONError(w, "unsupported_grant_type")
		return
	}
	code := r.Form.Get("code")
	i.mu.Lock()
	pend, ok := i.authReqs[code]
	delete(i.authReqs, code) // single-use
	mutate := i.mutate
	i.mu.Unlock()
	if !ok {
		writeJSONError(w, "invalid_grant")
		return
	}
	if sum := sha256.Sum256([]byte(r.Form.Get("code_verifier"))); pend.challenge != "" &&
		pend.challenge != base64.RawURLEncoding.EncodeToString(sum[:]) {
		writeJSONError(w, "invalid_grant")
		return
	}

	now := i.now()
	claims := map[string]any{
		"iss": i.URL(),
		"sub": i.Subject,
		"aud": pend.clientID,
		"exp": now.Add(i.TokenTTL).Unix(),
		"iat": now.Unix(),
	}
	if pend.nonce != "" {
		claims["nonce"] = pend.nonce
	}
	if i.TenantID != "" {
		claims["tenant_id"] = i.TenantID
	}
	if len(i.Groups) > 0 {
		claims["groups"] = i.Groups
	}
	if mutate != nil {
		mutate(claims)
	}
	idToken, err := i.sign(claims)
	if err != nil {
		http.Error(w, "signing failed", http.StatusInternalServerError)
		return
	}
	accessToken := randomString(32)
	i.mu.Lock()
	i.lastIDToken = idToken
	i.lastAccessToken = accessToken
	i.mu.Unlock()
	writeJSON(w, map[string]any{
		"access_token": accessToken,
		"token_type":   "Bearer",
		"expires_in":   3600,
		"id_token":     idToken,
	})
}

func (i *Issuer) jwks(w http.ResponseWriter, r *http.Request) {
	pub := &i.key.PublicKey
	writeJSON(w, map[string]any{
		"keys": []map[string]any{{
			"kty": "RSA",
			"kid": i.kid,
			"use": "sig",
			"alg": "RS256",
			"n":   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
			"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
		}},
	})
}

func (i *Issuer) sign(claims map[string]any) (string, error) {
	header, err := json.Marshal(map[string]any{"alg": "RS256", "typ": "JWT", "kid": i.kid})
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	b64 := base64.RawURLEncoding
	signingInput := b64.EncodeToString(header) + "." + b64.EncodeToString(payload)
	sum := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, i.key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signingInput + "." + b64.EncodeToString(sig), nil
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONError(w http.ResponseWriter, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": code})
}

func randomString(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
