package main

import (
	"os"
	"path/filepath"
	"testing"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), configFileName)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadConfig(t *testing.T) {
	path := writeConfig(t, `
[server]
host = "0.0.0.0"
port = 9000
auth_key = "s3cret"

[security]
ip_allowlist = ["192.168.1.0/24"]
enable_tier3 = true

[tools]
exclude = ["ScreenRecord"]
`)
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Server.Host != "0.0.0.0" || cfg.Server.Port != 9000 || cfg.Server.AuthKey != "s3cret" {
		t.Errorf("server config = %+v", cfg.Server)
	}
	if !cfg.Security.EnableTier3 || len(cfg.Security.IPAllowlist) != 1 {
		t.Errorf("security config = %+v", cfg.Security)
	}
	if len(cfg.Tools.Exclude) != 1 || cfg.Tools.Exclude[0] != "ScreenRecord" {
		t.Errorf("tools config = %+v", cfg.Tools)
	}

	// defined() is what separates "the operator wrote false" from "absent", and
	// that distinction drives the whole precedence chain.
	if !cfg.defined("server", "host") {
		t.Error("server.host should be reported as defined")
	}
	if cfg.defined("server", "ssl_certfile") {
		t.Error("an absent key must not be reported as defined")
	}
}

func TestLoadConfigNoPathReturnsDefaults(t *testing.T) {
	cfg, err := loadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Host != "" || cfg.defined("server", "host") {
		t.Errorf("an empty path should yield a blank config, got %+v", cfg.Server)
	}
}

// A --config path the operator typed must not silently fall back to defaults.
func TestLoadConfigMissingFileIsAnError(t *testing.T) {
	if _, err := loadConfig(filepath.Join(t.TempDir(), "nope.toml")); err == nil {
		t.Fatal("expected an error for a missing config file")
	}
}

func TestLoadConfigRejectsUnknownKeys(t *testing.T) {
	path := writeConfig(t, "[server]\nhsot = \"typo\"\n")
	if _, err := loadConfig(path); err == nil {
		t.Fatal("expected an error for an unknown key")
	}
}
