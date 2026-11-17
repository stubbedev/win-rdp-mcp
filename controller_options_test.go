package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stubbedev/win-rdp-mcp/internal/grdp/core"
)

func TestControlOptionsComeFromTheConfigFile(t *testing.T) {
	t.Setenv(envTargetPass, "")
	path := writePrivateConfig(t, `
[target]
host = "192.168.1.50"
user = "administrator"
domain = "LAB"
password = "hunter2"
width = 1600
height = 900
no_agent = true

[security]
enable_tier3 = true
`)
	opts, err := parseControlOptions([]string{"--config", path})
	if err != nil {
		t.Fatal(err)
	}
	if opts.host != "192.168.1.50" || opts.user != "administrator" || opts.domain != "LAB" || opts.pass != "hunter2" {
		t.Errorf("connection = %s %s\\%s %q", opts.host, opts.domain, opts.user, opts.pass)
	}
	if opts.size != (sessionSize{1600, 900}) || !opts.noAgent {
		t.Errorf("size = %+v, noAgent = %v", opts.size, opts.noAgent)
	}
	if !opts.enabled["Shell"] {
		t.Error("enable_tier3 in the file did not enable Shell")
	}
}

func TestControlFlagsAndEnvironmentBeatTheConfigFile(t *testing.T) {
	path := writePrivateConfig(t, "[target]\nhost = \"file-host\"\nuser = \"file-user\"\npassword = \"file-pass\"\nwidth = 1600\n")

	t.Setenv(envTargetPass, "env-pass")
	opts, err := parseControlOptions([]string{"--config", path, "--user", "flag-user", "--width", "1024"})
	if err != nil {
		t.Fatal(err)
	}
	if opts.host != "file-host" || opts.user != "flag-user" || opts.size.width != 1024 || opts.pass != "env-pass" {
		t.Errorf("got host=%s user=%s width=%d pass=%q", opts.host, opts.user, opts.size.width, opts.pass)
	}

	passFile := filepath.Join(t.TempDir(), "pass")
	if err := os.WriteFile(passFile, []byte("flag-pass\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	opts, err = parseControlOptions([]string{"--config", path, "--pass-file", passFile})
	if err != nil {
		t.Fatal(err)
	}
	if opts.pass != "flag-pass" {
		t.Errorf("pass = %q, want the --pass-file value", opts.pass)
	}
}

func TestControlPassFileInTheConfigFile(t *testing.T) {
	t.Setenv(envTargetPass, "")
	passFile := filepath.Join(t.TempDir(), "pass")
	if err := os.WriteFile(passFile, []byte("from-file\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := writeConfig(t, "[target]\nhost = \"h\"\nuser = \"u\"\npass_file = '"+passFile+"'\n")
	opts, err := parseControlOptions([]string{"--config", path})
	if err != nil {
		t.Fatal(err)
	}
	if opts.pass != "from-file" {
		t.Errorf("pass = %q", opts.pass)
	}
}

func TestControlOptionsNeedATargetAndAPassword(t *testing.T) {
	t.Setenv(envTargetPass, "")
	empty := writeConfig(t, "")
	if _, err := parseControlOptions([]string{"--config", empty}); err == nil || !strings.Contains(err.Error(), "no target") {
		t.Errorf("err = %v, want a missing-target error", err)
	}
	if _, err := parseControlOptions([]string{"--config", empty, "--target", "h", "--user", "u"}); err == nil || !strings.Contains(err.Error(), "no password") {
		t.Errorf("err = %v, want a missing-password error", err)
	}
}

func TestControlServerTrustDefaultsToVerification(t *testing.T) {
	t.Setenv(envTargetPass, "p")
	base := "[target]\nhost = \"h\"\nuser = \"u\"\n"
	pin := strings.Repeat("ab", 32)

	opts, err := parseControlOptions([]string{"--config", writeConfig(t, base)})
	if err != nil {
		t.Fatal(err)
	}
	if opts.trust != core.TrustSystemRoots() {
		t.Errorf("trust = %+v, want system-root verification by default", opts.trust)
	}

	sum, err := core.ParseCertificateSHA256(pin)
	if err != nil {
		t.Fatal(err)
	}
	opts, err = parseControlOptions([]string{"--config", writeConfig(t, base+"tls_cert_sha256 = \""+pin+"\"\n")})
	if err != nil {
		t.Fatal(err)
	}
	if opts.trust != core.TrustPinnedCertificate(sum) {
		t.Errorf("trust = %+v, want the pinned certificate from the file", opts.trust)
	}

	opts, err = parseControlOptions([]string{"--config", writeConfig(t, base), "--tls-skip-verify"})
	if err != nil {
		t.Fatal(err)
	}
	if opts.trust != core.TrustAnyCertificate() {
		t.Errorf("trust = %+v, want verification skipped on --tls-skip-verify", opts.trust)
	}
}

func TestControlServerTrustRejectsConflictsAndBadPins(t *testing.T) {
	t.Setenv(envTargetPass, "p")
	base := "[target]\nhost = \"h\"\nuser = \"u\"\n"
	conflict := writeConfig(t, base+"tls_skip_verify = true\n")
	if _, err := parseControlOptions([]string{"--config", conflict, "--tls-cert-sha256", strings.Repeat("ab", 32)}); err == nil || !strings.Contains(err.Error(), "not both") {
		t.Errorf("err = %v, want a pin-versus-skip conflict", err)
	}
	if _, err := parseControlOptions([]string{"--config", writeConfig(t, base), "--tls-cert-sha256", "abcd"}); err == nil || !strings.Contains(err.Error(), "tls_cert_sha256") {
		t.Errorf("err = %v, want a malformed-pin error", err)
	}
}
