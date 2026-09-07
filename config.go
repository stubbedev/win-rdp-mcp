package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// ServerConfig is the [server] table of win-rdp-mcp.toml.
type ServerConfig struct {
	Host                string `toml:"host"`
	Port                int    `toml:"port"`
	AllowInsecureRemote bool   `toml:"allow_insecure_remote"`
	AuthKey             string `toml:"auth_key"`
	SSLCertFile         string `toml:"ssl_certfile"`
	SSLKeyFile          string `toml:"ssl_keyfile"`
}

// SecurityConfig is the [security] table.
type SecurityConfig struct {
	IPAllowlist       []string `toml:"ip_allowlist"`
	EnableTier3       bool     `toml:"enable_tier3"`
	DisableTier2      bool     `toml:"disable_tier2"`
	OAuthClientID     string   `toml:"oauth_client_id"`
	OAuthClientSecret string   `toml:"oauth_client_secret"`
}

// ToolsConfig is the [tools] table.
type ToolsConfig struct {
	Enable  []string `toml:"enable"`
	Exclude []string `toml:"exclude"`
}

// Config is the whole file. defined() reports whether a dotted key was actually
// present, which is what makes "CLI flag > config file > default" precedence
// work: a config value only wins over the default when the operator wrote it.
type Config struct {
	Server   ServerConfig   `toml:"server"`
	Security SecurityConfig `toml:"security"`
	Tools    ToolsConfig    `toml:"tools"`

	SourcePath string        `toml:"-"`
	meta       toml.MetaData `toml:"-"`
}

func (c *Config) defined(key ...string) bool { return c.meta.IsDefined(key...) }

const configFileName = "win-rdp-mcp.toml"

// discoverConfigPath resolves the config file: an explicit path wins, then
// ./win-rdp-mcp.toml, then ~/.config/win-rdp-mcp/win-rdp-mcp.toml. Empty means
// "no config file, use defaults".
func discoverConfigPath(explicit string) string {
	if explicit != "" {
		return expandUser(explicit)
	}
	if cwd, err := os.Getwd(); err == nil {
		if p := filepath.Join(cwd, configFileName); fileExists(p) {
			return p
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		if p := filepath.Join(home, ".config", "win-rdp-mcp", configFileName); fileExists(p) {
			return p
		}
	}
	return ""
}

func fileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}

func expandUser(path string) string {
	if path == "~" || len(path) > 1 && path[:2] == "~/" {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, path[1:])
		}
	}
	return path
}

// loadConfig reads and validates the TOML file. A nil path yields defaults; a
// path that does not exist is an error, because an operator who passed --config
// wants that file, not a silent fallback.
func loadConfig(path string) (*Config, error) {
	cfg := &Config{}
	if path == "" {
		return cfg, nil
	}
	if !fileExists(path) {
		return nil, fmt.Errorf("config file not found: %s", path)
	}
	meta, err := toml.DecodeFile(path, cfg)
	if err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	if undecoded := meta.Undecoded(); len(undecoded) > 0 {
		return nil, fmt.Errorf("config %s: unknown keys: %v", path, undecoded)
	}
	cfg.meta = meta
	cfg.SourcePath = path
	return cfg, nil
}
