package main

import (
	"crypto/sha256"
	"encoding/base64"
	json "encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func newTestOAuth(t *testing.T) (*oauthStore, *http.ServeMux) {
	t.Helper()
	store := newOAuthStore("https://win.example:8090", "client-id", "client-secret")
	mux := http.NewServeMux()
	store.register(mux)
	return store, mux
}

func challengeFor(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// authorize drives the authorization endpoint and returns the issued code.
func authorize(t *testing.T, mux *http.ServeMux, params url.Values) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/oauth/authorize?"+params.Encode(), nil))
	return rec
}

func validAuthorizeParams(challenge string) url.Values {
	return url.Values{
		"response_type":         {"code"},
		"client_id":             {"client-id"},
		"redirect_uri":          {"http://127.0.0.1:33418/callback"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"state":                 {"xyz"},
	}
}

func TestOAuthAuthorizationCodeFlow(t *testing.T) {
	store, mux := newTestOAuth(t)
	const verifier = "a-sufficiently-long-pkce-code-verifier-value"

	rec := authorize(t, mux, validAuthorizeParams(challengeFor(verifier)))
	if rec.Code != http.StatusFound {
		t.Fatalf("authorize status = %d, want 302: %s", rec.Code, rec.Body)
	}
	location, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	code := location.Query().Get("code")
	if code == "" {
		t.Fatal("no code in the redirect")
	}
	if got := location.Query().Get("state"); got != "xyz" {
		t.Errorf("state = %q, want xyz", got)
	}

	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"code_verifier": {verifier},
		"client_id":     {"client-id"},
		"client_secret": {"client-secret"},
		"redirect_uri":  {"http://127.0.0.1:33418/callback"},
	}
	token := postToken(t, mux, form)
	if token.Code != http.StatusOK {
		t.Fatalf("token status = %d, want 200: %s", token.Code, token.Body)
	}

	var body struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(token.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.TokenType != "Bearer" || body.ExpiresIn <= 0 {
		t.Errorf("unexpected token response: %+v", body)
	}
	if !store.validateToken(body.AccessToken) {
		t.Error("the issued token should validate")
	}
	if store.validateToken("some-other-token") {
		t.Error("an unknown token must not validate")
	}

	// Codes are single-use: a replay must not mint a second token.
	if replay := postToken(t, mux, form); replay.Code == http.StatusOK {
		t.Error("replaying an authorization code should fail")
	}
}

func postToken(t *testing.T, mux *http.ServeMux, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/oauth/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestOAuthAuthorizeRejectsBadRequests(t *testing.T) {
	_, mux := newTestOAuth(t)
	challenge := challengeFor("verifier-verifier-verifier-verifier")

	tests := map[string]func(url.Values){
		"non-loopback redirect": func(v url.Values) { v.Set("redirect_uri", "https://evil.example/callback") },
		"missing PKCE":          func(v url.Values) { v.Del("code_challenge") },
		"plain PKCE":            func(v url.Values) { v.Set("code_challenge_method", "plain") },
		"wrong response type":   func(v url.Values) { v.Set("response_type", "token") },
		"unknown client":        func(v url.Values) { v.Set("client_id", "someone-else") },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			params := validAuthorizeParams(challenge)
			mutate(params)
			if rec := authorize(t, mux, params); rec.Code == http.StatusFound {
				t.Errorf("authorize should have refused, got a redirect to %s", rec.Header().Get("Location"))
			}
		})
	}
}

func TestOAuthTokenRejectsBadGrants(t *testing.T) {
	const verifier = "another-long-enough-pkce-code-verifier"
	base := url.Values{
		"grant_type":    {"authorization_code"},
		"code_verifier": {verifier},
		"client_id":     {"client-id"},
		"client_secret": {"client-secret"},
		"redirect_uri":  {"http://127.0.0.1:33418/callback"},
	}

	tests := map[string]func(url.Values){
		"wrong PKCE verifier":   func(v url.Values) { v.Set("code_verifier", "not-the-verifier") },
		"wrong client secret":   func(v url.Values) { v.Set("client_secret", "guess") },
		"missing client secret": func(v url.Values) { v.Del("client_secret") },
		"redirect uri mismatch": func(v url.Values) { v.Set("redirect_uri", "http://127.0.0.1:1/other") },
		"wrong grant type":      func(v url.Values) { v.Set("grant_type", "password") },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			// Each case gets a fresh code, since a failed exchange consumes it.
			_, mux := newTestOAuth(t)
			rec := authorize(t, mux, validAuthorizeParams(challengeFor(verifier)))
			location, _ := url.Parse(rec.Header().Get("Location"))

			form := url.Values{}
			for k, v := range base {
				form[k] = v
			}
			form.Set("code", location.Query().Get("code"))
			mutate(form)

			if got := postToken(t, mux, form); got.Code == http.StatusOK {
				t.Errorf("token exchange should have failed, got 200: %s", got.Body)
			}
		})
	}
}

// Dynamic registration would let anyone who can reach the port mint a client,
// which is exactly what the pre-provisioned model exists to prevent.
func TestOAuthDynamicRegistrationIsDisabled(t *testing.T) {
	_, mux := newTestOAuth(t)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/oauth/register", strings.NewReader("{}")))
	if rec.Code != http.StatusForbidden {
		t.Errorf("register status = %d, want 403", rec.Code)
	}
}

func TestOAuthExpiredTokenIsRejected(t *testing.T) {
	store := newOAuthStore("https://win.example", "id", "secret")
	store.tokens["stale"] = accessToken{clientID: "id", expiresAt: time.Now().Add(-time.Minute)}
	if store.validateToken("stale") {
		t.Error("an expired token must not validate")
	}
	if _, ok := store.tokens["stale"]; ok {
		t.Error("an expired token should be dropped on validation")
	}
}

func TestVerifyPKCE(t *testing.T) {
	const verifier = "the-verifier-the-client-kept-to-itself"
	if !verifyPKCE(verifier, challengeFor(verifier)) {
		t.Error("a matching verifier should pass")
	}
	if verifyPKCE("", challengeFor(verifier)) {
		t.Error("an empty verifier must never pass")
	}
	// "plain" is not offered, so a verifier that equals the challenge must fail.
	if verifyPKCE(verifier, verifier) {
		t.Error("plain PKCE must not be accepted")
	}
}

func TestIsLoopbackRedirectURI(t *testing.T) {
	for raw, want := range map[string]bool{
		"http://127.0.0.1:1234/cb": true, "https://localhost/cb": true, "http://[::1]:9/cb": true,
		"https://example.com/cb": false, "myapp://cb": false, "http://127.0.0.1/cb#frag": false,
		"": false,
	} {
		if got := isLoopbackRedirectURI(raw); got != want {
			t.Errorf("isLoopbackRedirectURI(%q) = %v, want %v", raw, got, want)
		}
	}
}
