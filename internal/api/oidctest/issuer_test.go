package oidctest

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func noRedirect() *http.Client {
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
}

func TestDiscoveryAndJWKS(t *testing.T) {
	iss, err := NewIssuer()
	if err != nil {
		t.Fatal(err)
	}
	defer iss.Close()

	resp, err := http.Get(iss.URL() + "/.well-known/openid-configuration")
	if err != nil {
		t.Fatal(err)
	}
	var disc map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&disc)
	resp.Body.Close()
	if disc["issuer"] != iss.URL() {
		t.Fatalf("discovery issuer = %v", disc["issuer"])
	}

	resp, err = http.Get(iss.URL() + "/jwks")
	if err != nil {
		t.Fatal(err)
	}
	var jwks struct {
		Keys []map[string]any `json:"keys"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&jwks)
	resp.Body.Close()
	if len(jwks.Keys) != 1 || jwks.Keys[0]["kty"] != "RSA" {
		t.Fatalf("jwks malformed: %v", jwks)
	}
}

// exchange runs authorize -> token and returns the token endpoint response.
func exchange(t *testing.T, iss *Issuer, verifier, overrideVerifier string) (int, map[string]any) {
	t.Helper()
	client := noRedirect()
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	authURL := iss.URL() + "/authorize?response_type=code&client_id=" + iss.ClientID +
		"&redirect_uri=" + url.QueryEscape("https://portal.test/cb") +
		"&state=s1&nonce=n1&code_challenge=" + challenge + "&code_challenge_method=S256"
	resp, err := client.Get(authURL)
	if err != nil {
		t.Fatal(err)
	}
	loc := resp.Header.Get("Location")
	resp.Body.Close()
	cb, _ := url.Parse(loc)
	code := cb.Query().Get("code")
	if code == "" {
		t.Fatalf("authorize returned no code: %q", loc)
	}
	v := verifier
	if overrideVerifier != "" {
		v = overrideVerifier
	}
	resp2, err := client.PostForm(iss.URL()+"/token", url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {"https://portal.test/cb"},
		"client_id":     {iss.ClientID},
		"code_verifier": {v},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp2.Body).Decode(&out)
	return resp2.StatusCode, out
}

func TestTokenExchangeIssuesIDToken(t *testing.T) {
	iss, err := NewIssuer()
	if err != nil {
		t.Fatal(err)
	}
	defer iss.Close()

	status, body := exchange(t, iss, strings.Repeat("a", 64), "")
	if status != http.StatusOK {
		t.Fatalf("token status=%d body=%v", status, body)
	}
	if body["id_token"] == "" || body["access_token"] == "" {
		t.Fatalf("token response missing tokens: %v", body)
	}
	if iss.LastIDToken() != body["id_token"] {
		t.Fatal("LastIDToken does not match issued token")
	}
}

func TestTokenExchangeRejectsBadVerifier(t *testing.T) {
	iss, err := NewIssuer()
	if err != nil {
		t.Fatal(err)
	}
	defer iss.Close()

	status, body := exchange(t, iss, strings.Repeat("a", 64), strings.Repeat("b", 64))
	if status != http.StatusBadRequest || body["error"] != "invalid_grant" {
		t.Fatalf("status=%d body=%v", status, body)
	}
}

func TestAuthorizationCodeSingleUse(t *testing.T) {
	iss, err := NewIssuer()
	if err != nil {
		t.Fatal(err)
	}
	defer iss.Close()

	verifier := strings.Repeat("c", 64)
	sum := sha256.Sum256([]byte(verifier))
	client := noRedirect()
	resp, err := client.Get(iss.URL() + "/authorize?response_type=code&client_id=" + iss.ClientID +
		"&redirect_uri=" + url.QueryEscape("https://portal.test/cb") +
		"&state=s1&nonce=n1&code_challenge=" + base64.RawURLEncoding.EncodeToString(sum[:]) +
		"&code_challenge_method=S256")
	if err != nil {
		t.Fatal(err)
	}
	cb, _ := url.Parse(resp.Header.Get("Location"))
	resp.Body.Close()
	code := cb.Query().Get("code")

	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {"https://portal.test/cb"},
		"client_id":     {iss.ClientID},
		"code_verifier": {verifier},
	}
	r1, err := client.PostForm(iss.URL()+"/token", form)
	if err != nil {
		t.Fatal(err)
	}
	r1.Body.Close()
	if r1.StatusCode != http.StatusOK {
		t.Fatalf("first redeem status=%d", r1.StatusCode)
	}
	r2, err := client.PostForm(iss.URL()+"/token", form)
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Body.Close()
	if r2.StatusCode != http.StatusBadRequest {
		t.Fatalf("replayed code accepted: status=%d", r2.StatusCode)
	}
}
