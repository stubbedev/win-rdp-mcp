package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"

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
	OAuthRedirectURIs []string `toml:"oauth_redirect_uris"`
}

// ToolsConfig is the [tools] table.
type ToolsConfig struct {
	Enable  []string `toml:"enable"`
	Exclude []string `toml:"exclude"`
}

type TargetConfig struct {
	Host     string `toml:"host"`
	User     string `toml:"user"`
	Domain   string `toml:"domain"`
	Password string `toml:"password"`
	PassFile string `toml:"pass_file"`
	Width    int    `toml:"width"`
	Height   int    `toml:"height"`
	AgentExe string `toml:"agent_exe"`
	NoAgent  bool   `toml:"no_agent"`
}

func (t TargetConfig) validate(path string, mode fs.FileMode) error {
	if t.Password != "" && t.PassFile != "" {
		return fmt.Errorf("config %s: set password or pass_file under [target], not both", path)
	}
	if t.Password != "" && runtime.GOOS != "windows" && mode.Perm()&0o077 != 0 {
		return fmt.Errorf("config %s holds a password but other users can read it (mode %v); run chmod 600 %s", path, mode.Perm(), path)
	}
	return nil
}

// Config is the whole file. defined() reports whether a dotted key was actually
// present, which is what makes "CLI flag > config file > default" precedence
// work: a config value only wins over the default when the operator wrote it.
type Config struct {
	Server   ServerConfig   `toml:"server"`
	Security SecurityConfig `toml:"security"`
	Tools    ToolsConfig    `toml:"tools"`
	Target   TargetConfig   `toml:"target"`

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
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file: %w", err)
	}
	meta, err := toml.DecodeFile(path, cfg)
	if err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	if undecoded := meta.Undecoded(); len(undecoded) > 0 {
		return nil, fmt.Errorf("config %s: unknown keys: %v", path, undecoded)
	}
	if err := cfg.Target.validate(path, info.Mode()); err != nil {
		return nil, err
	}
	cfg.meta = meta
	cfg.SourcePath = path
	return cfg, nil
}
