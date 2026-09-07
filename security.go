package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
)

// ── Bind-address safety ──────────────────────────────────────────────────────

// isLoopbackBindHost reports whether binding to host reaches only this machine.
func isLoopbackBindHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	addr, err := netip.ParseAddr(host)
	return err == nil && addr.IsLoopback()
}

// parseIPAllowlist turns bare IPs and CIDRs into prefixes. A bare IP becomes a
// single-host prefix. Every bad entry is reported at once so one typo does not
// hide the next.
func parseIPAllowlist(entries []string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	var errs []error

	for _, raw := range entries {
		entry := strings.TrimSpace(raw)
		if entry == "" {
			continue
		}
		if strings.Contains(entry, "/") {
			prefix, err := netip.ParsePrefix(entry)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", entry, err))
				continue
			}
			out = append(out, prefix.Masked())
			continue
		}
		addr, err := netip.ParseAddr(entry)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", entry, err))
			continue
		}
		out = append(out, netip.PrefixFrom(addr, addr.BitLen()))
	}

	if len(errs) > 0 {
		return nil, fmt.Errorf("invalid IP allowlist entries: %w", errors.Join(errs...))
	}
	return out, nil
}

func allowlistContains(allow []netip.Prefix, addr netip.Addr) bool {
	// An IPv4-mapped IPv6 client address (::ffff:10.0.0.1) must match an IPv4
	// rule, so compare on the unmapped form.
	addr = addr.Unmap()
	for _, p := range allow {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// ── SSRF guard for server-side fetches ───────────────────────────────────────

// validateFetchURL vets a user-supplied URL before the server fetches it. Only
// http(s), no embedded credentials, and every resolved address must be public:
// otherwise a tool call becomes a probe of the Windows host's own network.
func validateFetchURL(ctx context.Context, raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("only http and https URL schemes are allowed")
	}
	if u.Hostname() == "" {
		return errors.New("URL must include a hostname")
	}
	if u.User != nil {
		return errors.New("credentials in URLs are not allowed")
	}

	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", u.Hostname())
	if err != nil {
		return fmt.Errorf("could not resolve hostname: %w", err)
	}
	for _, addr := range addrs {
		if !isPublicAddr(addr) {
			return fmt.Errorf("private, loopback, link-local, multicast and reserved addresses are blocked: %s", addr)
		}
	}
	return nil
}

func isPublicAddr(addr netip.Addr) bool {
	a := addr.Unmap()
	switch {
	case a.IsPrivate(), a.IsLoopback(), a.IsLinkLocalUnicast(), a.IsLinkLocalMulticast(),
		a.IsMulticast(), a.IsInterfaceLocalMulticast(), a.IsUnspecified():
		return false
	}
	// 100.64.0.0/10 (CGNAT) and 0.0.0.0/8 are not covered by the stdlib
	// predicates but are just as much "not the public internet".
	if a.Is4() {
		b := a.As4()
		if b[0] == 0 || (b[0] == 100 && b[1] >= 64 && b[1] <= 127) {
			return false
		}
	}
	return true
}

// fetchValidated GETs a validated URL. Redirects are refused rather than
// followed: a public URL that 302s to 169.254.169.254 would otherwise walk
// straight past the check above. The body is capped at maxBytes.
func fetchValidated(ctx context.Context, rawURL string, header http.Header, maxBytes int64) ([]byte, error) {
	if err := validateFetchURL(ctx, rawURL); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}

	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return nil, fmt.Errorf("redirects are not allowed (HTTP %d to %q)", resp.StatusCode, resp.Header.Get("Location"))
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	// Read one byte past the cap so an oversized body is detected rather than
	// silently truncated.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > maxBytes {
		return nil, fmt.Errorf("response exceeds %d byte limit", maxBytes)
	}
	return body, nil
}

// ── HTTP middleware ──────────────────────────────────────────────────────────

// publicPaths never require authentication: the health probe, and the OAuth
// endpoints a client must reach before it holds a token.
var publicPaths = map[string]bool{
	"/health": true,
	"/.well-known/oauth-authorization-server": true,
	"/.well-known/oauth-protected-resource":   true,
	"/oauth/register":                         true,
	"/oauth/authorize":                        true,
	"/oauth/token":                            true,
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"error":%q}`+"\n", msg)
}

// ipAllowlistMiddleware rejects clients outside the configured networks. /health
// stays reachable so a load balancer probe does not need an allowlist entry.
func ipAllowlistMiddleware(allow []netip.Prefix, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			next.ServeHTTP(w, r)
			return
		}
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = r.RemoteAddr
		}
		addr, err := netip.ParseAddr(host)
		if err != nil {
			writeJSONError(w, http.StatusForbidden, "Forbidden: invalid client address "+host)
			return
		}
		if !allowlistContains(allow, addr) {
			writeJSONError(w, http.StatusForbidden, "Forbidden: client IP "+addr.String()+" is not in allowlist")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// authMiddleware requires a Bearer token on every non-public path. Either the
// static API key or a live OAuth access token is accepted; when only one of the
// two is configured, the other check is simply never satisfied.
func authMiddleware(authKey string, validateOAuth func(string) bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if publicPaths[r.URL.Path] {
			next.ServeHTTP(w, r)
			return
		}
		token, ok := bearerToken(r)
		if ok {
			// Constant-time compare so the key cannot be recovered a byte at a
			// time from response timing.
			if authKey != "" && subtle.ConstantTimeCompare([]byte(token), []byte(authKey)) == 1 {
				next.ServeHTTP(w, r)
				return
			}
			if validateOAuth != nil && validateOAuth(token) {
				next.ServeHTTP(w, r)
				return
			}
		}
		w.Header().Set("WWW-Authenticate", `Bearer realm="win-rdp-mcp"`)
		writeJSONError(w, http.StatusUnauthorized, "Unauthorized")
	})
}

func bearerToken(r *http.Request) (string, bool) {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", false
	}
	return h[len(prefix):], true
}
