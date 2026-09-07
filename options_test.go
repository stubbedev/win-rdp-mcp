package main

import (
	"strings"
	"testing"
)

func TestParseOptionsDefaults(t *testing.T) {
	opts, err := parseOptions(nil)
	if err != nil {
		t.Fatal(err)
	}
	if opts.host != "127.0.0.1" || opts.port != 8090 || opts.transport != "streamable-http" {
		t.Errorf("defaults = %s %s:%d", opts.transport, opts.host, opts.port)
	}
	if opts.authenticated() {
		t.Error("no auth should be configured by default")
	}
	if len(opts.enabled) != len(tier1)+len(tier2) {
		t.Errorf("default tools = %d", len(opts.enabled))
	}
}

func TestParseOptionsEnvAndFlagPrecedence(t *testing.T) {
	t.Setenv(envAuthKey, "from-env")

	opts, err := parseOptions(nil)
	if err != nil {
		t.Fatal(err)
	}
	if opts.authKey != "from-env" {
		t.Errorf("authKey = %q, want the environment value", opts.authKey)
	}

	// An explicit flag beats the environment.
	opts, err = parseOptions([]string{"--auth-key", "from-flag"})
	if err != nil {
		t.Fatal(err)
	}
	if opts.authKey != "from-flag" {
		t.Errorf("authKey = %q, want the flag value", opts.authKey)
	}
}

// These gates are the reason the server is safe to run on a LAN at all.
func TestValidateOptionsNetworkGates(t *testing.T) {
	tier12, err := resolveEnabledTools(toolSelection{})
	if err != nil {
		t.Fatal(err)
	}
	all, err := resolveEnabledTools(toolSelection{enableAll: true})
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		opts    options
		wantErr string
	}{
		{
			name: "loopback with no auth is fine",
			opts: options{transport: "streamable-http", host: "127.0.0.1", enabled: all},
		},
		{
			name: "stdio ignores the bind rules",
			opts: options{transport: "stdio", host: "0.0.0.0", enabled: all},
		},
		{
			name:    "remote with no auth is refused",
			opts:    options{transport: "streamable-http", host: "0.0.0.0", enabled: tier12},
			wantErr: "without authentication",
		},
		{
			name: "remote with no auth is allowed once acknowledged",
			opts: options{transport: "streamable-http", host: "0.0.0.0", enabled: tier12, allowInsecureRemote: true},
		},
		{
			name:    "tier 3 remote is refused even when acknowledged",
			opts:    options{transport: "streamable-http", host: "0.0.0.0", enabled: all, allowInsecureRemote: true},
			wantErr: "tier 3",
		},
		{
			name: "tier 3 remote is fine behind an auth key",
			opts: options{transport: "streamable-http", host: "0.0.0.0", enabled: all, authKey: "k"},
		},
		{
			name: "tier 3 remote is fine behind oauth",
			opts: options{transport: "streamable-http", host: "0.0.0.0", enabled: all,
				oauthClientID: "id", oauthClientSecret: "secret"},
		},
		{
			name:    "half an oauth client is refused",
			opts:    options{transport: "streamable-http", host: "127.0.0.1", enabled: tier12, oauthClientID: "id"},
			wantErr: "OAuth requires both",
		},
		{
			name:    "half a TLS pair is refused",
			opts:    options{transport: "streamable-http", host: "127.0.0.1", enabled: tier12, sslCertFile: "cert.pem"},
			wantErr: "HTTPS requires both",
		},
		{
			name:    "an unknown transport is refused",
			opts:    options{transport: "carrier-pigeon", host: "127.0.0.1", enabled: tier12},
			wantErr: "unknown transport",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateOptions(tc.opts)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("expected an error mentioning %q", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Fatalf("error = %v, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

func TestInstructionsReflectTheEnabledTools(t *testing.T) {
	tier12, _ := resolveEnabledTools(toolSelection{})
	if got := instructions(options{enabled: tier12}); !strings.Contains(got, "Destructive tools are disabled") {
		t.Error("instructions should say destructive tools are off when they are")
	}

	all, _ := resolveEnabledTools(toolSelection{enableAll: true})
	got := instructions(options{enabled: all})
	if !strings.Contains(got, "Destructive tools are enabled") || !strings.Contains(got, "Shell") {
		t.Error("instructions should warn about the destructive tools when they are on")
	}
}
