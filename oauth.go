package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A minimal OAuth 2.0 authorization server, enough for MCP clients that speak
// OAuth instead of a static key (Claude Desktop among them):
//
//	RFC 8414  GET  /.well-known/oauth-authorization-server
//	RFC 7591  POST /oauth/register            (deliberately disabled)
//	RFC 7636  GET  /oauth/authorize + POST /oauth/token  (code + PKCE)
//
// There is no browser login page here, so this is only safe for a
// pre-provisioned confidential client: the token exchange demands the client
// secret that the operator configured out of band. Dynamic registration stays
// off for the same reason, and redirect URIs must be pre-registered loopback URIs.

const (
	oauthCodeLifetime  = 5 * time.Minute
	oauthTokenLifetime = time.Hour
)

type authorizationCode struct {
	clientID      string
	redirectURI   string
	codeChallenge string
	expiresAt     time.Time
}

type accessToken struct {
	clientID  string
	expiresAt time.Time
}

// oauthStore holds the in-flight codes and live tokens. State is per-process and
// deliberately not persisted: a restart invalidates tokens, and clients re-auth.
type oauthStore struct {
	mu     sync.Mutex
	codes  map[string]authorizationCode
	tokens map[string]accessToken

	clientID     string
	clientSecret string
	redirectURIs []*url.URL
	issuer       string
}

func newOAuthStore(issuer, clientID, clientSecret string, redirectURIs []*url.URL) *oauthStore {
	return &oauthStore{
		codes:        map[string]authorizationCode{},
		tokens:       map[string]accessToken{},
		clientID:     clientID,
		clientSecret: clientSecret,
		redirectURIs: redirectURIs,
		issuer:       issuer,
	}
}

// validateToken reports whether tok is a live access token, dropping it if it
// has expired.
func (s *oauthStore) validateToken(tok string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	at, ok := s.tokens[tok]
	if !ok {
		return false
	}
	if time.Now().After(at.expiresAt) {
		delete(s.tokens, tok)
		return false
	}
	return true
}

// register wires the four OAuth endpoints onto mux.
func (s *oauthStore) register(mux *http.ServeMux) {
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", s.handleMetadata)
	mux.HandleFunc("GET /.well-known/oauth-protected-resource", s.handleProtectedResource)
	mux.HandleFunc("POST /oauth/register", s.handleRegister)
	mux.HandleFunc("GET /oauth/authorize", s.handleAuthorize)
	mux.HandleFunc("POST /oauth/token", s.handleToken)
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.MarshalWrite(w, payload)
}

func oauthError(w http.ResponseWriter, status int, code, description string) {
	payload := map[string]string{"error": code}
	if description != "" {
		payload["error_description"] = description
	}
	writeJSON(w, status, payload)
}

func (s *oauthStore) handleMetadata(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                s.issuer,
		"authorization_endpoint":                s.issuer + "/oauth/authorize",
		"token_endpoint":                        s.issuer + "/oauth/token",
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code"},
		"code_challenge_methods_supported":      []string{"S256"},
		"token_endpoint_auth_methods_supported": []string{"client_secret_post"},
	})
}

func (s *oauthStore) handleProtectedResource(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"resource":              s.issuer,
		"authorization_servers": []string{s.issuer},
	})
}

func (s *oauthStore) handleRegister(w http.ResponseWriter, _ *http.Request) {
	oauthError(w, http.StatusForbidden, "registration_disabled",
		"Dynamic OAuth client registration is disabled. Configure oauth_client_id and oauth_client_secret on the server and provision the secret out of band.")
}

func (s *oauthStore) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	clientID := q.Get("client_id")
	redirectURI := q.Get("redirect_uri")
	state := q.Get("state")
	challenge := q.Get("code_challenge")

	method := q.Get("code_challenge_method")
	if method == "" {
		method = "S256"
	}

	switch {
	case q.Get("response_type") != "code":
		oauthError(w, http.StatusBadRequest, "unsupported_response_type", "")
		return
	case challenge == "":
		oauthError(w, http.StatusBadRequest, "invalid_request", "code_challenge required (PKCE)")
		return
	case method != "S256":
		oauthError(w, http.StatusBadRequest, "invalid_request", "code_challenge_method must be S256")
		return
	case s.clientID == "" || s.clientSecret == "":
		oauthError(w, http.StatusInternalServerError, "server_error", "OAuth requires a configured client ID and secret")
		return
	case clientID != s.clientID:
		oauthError(w, http.StatusBadRequest, "invalid_client", "")
		return
	}

	target, err := s.registeredRedirect(redirectURI)
	if err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	code := randomToken(32)
	s.mu.Lock()
	s.pruneLocked()
	s.codes[code] = authorizationCode{
		clientID:      clientID,
		redirectURI:   redirectURI,
		codeChallenge: challenge,
		expiresAt:     time.Now().Add(oauthCodeLifetime),
	}
	s.mu.Unlock()

	params := target.Query()
	params.Set("code", code)
	if state != "" {
		params.Set("state", state)
	}
	target.RawQuery = params.Encode()
	http.Redirect(w, r, target.String(), http.StatusFound)
}

func (s *oauthStore) registeredRedirect(raw string) (*url.URL, error) {
	requested, err := parseLoopbackRedirectURI(raw)
	if err != nil {
		return nil, err
	}
	port, err := redirectPort(requested)
	if err != nil {
		return nil, err
	}
	for _, registered := range s.redirectURIs {
		if !sameRedirectIgnoringPort(registered, requested) {
			continue
		}
		target := *registered
		target.Host = joinHostPort(registered.Hostname(), port)
		return &target, nil
	}
	return nil, fmt.Errorf("redirect_uri %q is not registered for this client", raw)
}

func redirectPort(u *url.URL) (string, error) {
	raw := u.Port()
	if raw == "" {
		return "", nil
	}
	port, err := strconv.ParseUint(raw, 10, 16)
	if err != nil {
		return "", fmt.Errorf("redirect_uri port %q: %w", raw, err)
	}
	return strconv.FormatUint(port, 10), nil
}

func sameRedirectIgnoringPort(registered, requested *url.URL) bool {
	return registered.Scheme == requested.Scheme &&
		strings.EqualFold(registered.Hostname(), requested.Hostname()) &&
		registered.EscapedPath() == requested.EscapedPath() &&
		registered.RawQuery == requested.RawQuery
}

func joinHostPort(host, port string) string {
	if port != "" {
		return net.JoinHostPort(host, port)
	}
	if strings.Contains(host, ":") {
		return "[" + host + "]"
	}
	return host
}

func (s *oauthStore) handleToken(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_request", "")
		return
	}
	if r.PostForm.Get("grant_type") != "authorization_code" {
		oauthError(w, http.StatusBadRequest, "unsupported_grant_type", "")
		return
	}

	codeValue := r.PostForm.Get("code")
	verifier := r.PostForm.Get("code_verifier")
	clientID := r.PostForm.Get("client_id")
	redirectURI := r.PostForm.Get("redirect_uri")
	clientSecret := r.PostForm.Get("client_secret")

	s.mu.Lock()
	code, ok := s.codes[codeValue]
	// A code is one-time use: consume it now, so a replay after any failure
	// below cannot retry the PKCE or secret check.
	delete(s.codes, codeValue)
	s.mu.Unlock()

	if !ok {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "unknown code")
		return
	}
	switch {
	case code.clientID != clientID:
		oauthError(w, http.StatusBadRequest, "invalid_grant", "client_id mismatch")
		return
	case code.redirectURI != redirectURI:
		oauthError(w, http.StatusBadRequest, "invalid_grant", "redirect_uri mismatch")
		return
	case time.Now().After(code.expiresAt):
		oauthError(w, http.StatusBadRequest, "invalid_grant", "code expired")
		return
	case !verifyPKCE(verifier, code.codeChallenge):
		oauthError(w, http.StatusBadRequest, "invalid_grant", "PKCE verification failed")
		return
	case s.clientSecret == "" || subtle.ConstantTimeCompare([]byte(clientSecret), []byte(s.clientSecret)) != 1:
		oauthError(w, http.StatusUnauthorized, "invalid_client", "")
		return
	}

	token := randomToken(48)
	s.mu.Lock()
	s.pruneLocked()
	s.tokens[token] = accessToken{clientID: clientID, expiresAt: time.Now().Add(oauthTokenLifetime)}
	s.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"access_token": token,
		"token_type":   "Bearer",
		"expires_in":   int(oauthTokenLifetime.Seconds()),
	})
}

// pruneLocked drops expired codes and tokens so the maps cannot grow without
// bound on a long-lived server. Caller holds s.mu.
func (s *oauthStore) pruneLocked() {
	now := time.Now()
	for k, v := range s.codes {
		if now.After(v.expiresAt) {
			delete(s.codes, k)
		}
	}
	for k, v := range s.tokens {
		if now.After(v.expiresAt) {
			delete(s.tokens, k)
		}
	}
}

// verifyPKCE checks the S256 challenge. Only S256 is offered, so a "plain"
// verifier is never accepted.
func verifyPKCE(verifier, challenge string) bool {
	if verifier == "" {
		return false
	}
	sum := sha256.Sum256([]byte(verifier))
	expected := base64.RawURLEncoding.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare([]byte(expected), []byte(challenge)) == 1
}

// parseLoopbackRedirectURI restricts the callback to this machine, which is
// what makes a browserless authorize endpoint tolerable.
func parseLoopbackRedirectURI(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("redirect_uri %q: %w", raw, err)
	}
	switch {
	case u.Scheme != "http" && u.Scheme != "https":
		return nil, fmt.Errorf("redirect_uri %q: scheme must be http or https", raw)
	case strings.Contains(raw, "#"):
		return nil, fmt.Errorf("redirect_uri %q: must not carry a fragment", raw)
	case u.User != nil:
		return nil, fmt.Errorf("redirect_uri %q: must not carry user info", raw)
	case !isLoopbackHost(u.Hostname()):
		return nil, fmt.Errorf("redirect_uri %q: host must be loopback", raw)
	}
	return u, nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	addr, err := netip.ParseAddr(host)
	return err == nil && addr.IsLoopback()
}

func parseRedirectURIs(entries []string) ([]*url.URL, error) {
	out := make([]*url.URL, 0, len(entries))
	var errs []error
	for _, raw := range entries {
		entry := strings.TrimSpace(raw)
		if entry == "" {
			continue
		}
		u, err := parseLoopbackRedirectURI(entry)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		out = append(out, u)
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("invalid OAuth redirect URIs: %w", errors.Join(errs...))
	}
	return out, nil
}

func randomToken(n int) string {
	b := make([]byte, n)
	// rand.Read from crypto/rand cannot fail; it panics internally on entropy
	// failure rather than returning an error.
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
