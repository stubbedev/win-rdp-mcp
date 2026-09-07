package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

func TestIsLoopbackBindHost(t *testing.T) {
	for host, want := range map[string]bool{
		"127.0.0.1": true, "127.0.0.53": true, "::1": true, "localhost": true, "LOCALHOST": true,
		"0.0.0.0": false, "192.168.1.10": false, "": false, "example.com": false,
	} {
		if got := isLoopbackBindHost(host); got != want {
			t.Errorf("isLoopbackBindHost(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestParseIPAllowlist(t *testing.T) {
	allow, err := parseIPAllowlist([]string{"192.168.1.0/24", "10.0.0.7", " ", "::1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(allow) != 3 {
		t.Fatalf("parsed %d prefixes, want 3", len(allow))
	}

	for addr, want := range map[string]bool{
		"192.168.1.1": true, "192.168.1.255": true, "192.168.2.1": false,
		"10.0.0.7": true, "10.0.0.8": false, "::1": true,
		// An IPv4-mapped client address must still match an IPv4 rule.
		"::ffff:10.0.0.7": true,
	} {
		if got := allowlistContains(allow, netip.MustParseAddr(addr)); got != want {
			t.Errorf("allowlistContains(%s) = %v, want %v", addr, got, want)
		}
	}
}

func TestParseIPAllowlistReportsEveryBadEntry(t *testing.T) {
	_, err := parseIPAllowlist([]string{"not-an-ip", "192.168.1.0/99"})
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"not-an-ip", "192.168.1.0/99"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q, got %v", want, err)
		}
	}
}

// The SSRF guard is what stops a Scrape or PlaySound call becoming a probe of
// the Windows host's own network.
func TestValidateFetchURLBlocksNonPublicTargets(t *testing.T) {
	ctx := context.Background()
	for _, raw := range []string{
		"file:///etc/passwd",
		"ftp://example.com/x",
		"http://user:pass@example.com/",
		"http://127.0.0.1:8090/mcp",
		"http://localhost/admin",
		"http://169.254.169.254/latest/meta-data/",
		"http://[::1]/",
		"http://0.0.0.0/",
		"not a url at all",
	} {
		if err := validateFetchURL(ctx, raw); err == nil {
			t.Errorf("validateFetchURL(%q) allowed a target it should block", raw)
		}
	}
}

func TestIsPublicAddr(t *testing.T) {
	for addr, want := range map[string]bool{
		"8.8.8.8": true, "1.1.1.1": true, "2606:4700:4700::1111": true,
		"10.0.0.1": false, "172.16.0.1": false, "192.168.0.1": false,
		"127.0.0.1": false, "169.254.169.254": false, "0.0.0.0": false,
		"224.0.0.1": false, "100.64.0.1": false, "::1": false,
	} {
		if got := isPublicAddr(netip.MustParseAddr(addr)); got != want {
			t.Errorf("isPublicAddr(%s) = %v, want %v", addr, got, want)
		}
	}
}

func TestAuthMiddleware(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	handler := authMiddleware("secret", func(tok string) bool { return tok == "oauth-token" }, ok)

	tests := []struct {
		name   string
		path   string
		header string
		want   int
	}{
		{"health needs no token", "/health", "", http.StatusTeapot},
		{"metadata needs no token", "/.well-known/oauth-authorization-server", "", http.StatusTeapot},
		{"mcp without a token", "/mcp", "", http.StatusUnauthorized},
		{"mcp with a wrong token", "/mcp", "Bearer nope", http.StatusUnauthorized},
		{"mcp with the api key", "/mcp", "Bearer secret", http.StatusTeapot},
		{"mcp with an oauth token", "/mcp", "Bearer oauth-token", http.StatusTeapot},
		{"scheme is case-insensitive", "/mcp", "bearer secret", http.StatusTeapot},
		{"a bare key is not a bearer token", "/mcp", "secret", http.StatusUnauthorized},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, tc.path, nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}

func TestIPAllowlistMiddleware(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	allow, err := parseIPAllowlist([]string{"192.168.1.0/24"})
	if err != nil {
		t.Fatal(err)
	}
	handler := ipAllowlistMiddleware(allow, ok)

	for _, tc := range []struct {
		remote string
		path   string
		want   int
	}{
		{"192.168.1.5:1234", "/mcp", http.StatusTeapot},
		{"10.1.1.1:1234", "/mcp", http.StatusForbidden},
		// The probe stays reachable so a load balancer needs no allowlist entry.
		{"10.1.1.1:1234", "/health", http.StatusTeapot},
	} {
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		req.RemoteAddr = tc.remote
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Errorf("%s %s: status = %d, want %d", tc.remote, tc.path, rec.Code, tc.want)
		}
	}
}
