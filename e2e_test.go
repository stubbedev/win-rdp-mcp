//go:build e2e

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type e2eTarget struct {
	host, user, passFile string
}

func e2eTargetFromEnv(t *testing.T) e2eTarget {
	t.Helper()
	target := e2eTarget{
		host:     os.Getenv("WIN_RDP_E2E_TARGET"),
		user:     os.Getenv("WIN_RDP_E2E_USER"),
		passFile: os.Getenv("WIN_RDP_E2E_PASS_FILE"),
	}
	if target.host == "" || target.user == "" || target.passFile == "" {
		t.Skip("set WIN_RDP_E2E_TARGET, WIN_RDP_E2E_USER and WIN_RDP_E2E_PASS_FILE to run against a Windows machine")
	}
	return target
}

func buildAgent(t *testing.T, dir string) string {
	t.Helper()
	exe := filepath.Join(dir, "win-rdp-mcp.exe")
	cmd := exec.Command("go", "build", "-trimpath", "-ldflags", "-s -w", "-o", exe, ".")
	cmd.Env = append(os.Environ(), "GOOS=windows", "GOARCH=amd64", "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building the Windows agent: %v\n%s", err, out)
	}
	return exe
}

type e2eSession struct {
	t       *testing.T
	session *mcp.ClientSession
}

func (s e2eSession) call(name string, args map[string]any) *mcp.CallToolResult {
	s.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	res, err := s.session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		s.t.Fatalf("%s: %v", name, err)
	}
	return res
}

func (s e2eSession) ok(name string, args map[string]any) string {
	s.t.Helper()
	res := s.call(name, args)
	text := e2eText(res)
	if res.IsError {
		s.t.Fatalf("%s failed: %s", name, text)
	}
	return text
}

func e2eText(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

func e2eHasImage(res *mcp.CallToolResult) bool {
	for _, c := range res.Content {
		if img, ok := c.(*mcp.ImageContent); ok && len(img.Data) > 0 {
			return true
		}
	}
	return false
}

func TestEndToEnd(t *testing.T) {
	target := e2eTargetFromEnv(t)
	dir := t.TempDir()
	agent := buildAgent(t, dir)

	cmd := exec.Command(os.Args[0], "control", "--target", target.host, "--user", target.user,
		"--pass-file", target.passFile, "--agent-exe", agent, "--enable-all")
	cmd.Env = append(os.Environ(), smokeServerEnv+"=1")
	client := mcp.NewClient(&mcp.Implementation{Name: "e2e", Version: "0"}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatalf("starting the controller: %v", err)
	}
	defer session.Close()
	s := e2eSession{t: t, session: session}

	if res := s.call("Snapshot", nil); res.IsError || !e2eHasImage(res) {
		t.Fatalf("Snapshot: %s", e2eText(res))
	}
	if info := s.ok("GetSystemInfo", nil); !strings.Contains(info, "**Controller:**") || !strings.Contains(info, "**win-rdp-mcp:**") {
		t.Fatalf("GetSystemInfo lacks the version lines:\n%s", info)
	}
	if out := s.ok("Shell", map[string]any{"command": "'e2e ' + (6*7)"}); !strings.Contains(out, "e2e 42") {
		t.Fatalf("Shell: %s", out)
	}
	if out := s.ok("Shell", map[string]any{"command": "'45°F ünïcode ✓'"}); !strings.Contains(out, "45°F ünïcode ✓") {
		t.Fatalf("non-ASCII Shell output was mangled: %s", out)
	}

	s.ok("App", map[string]any{"name": "notepad.exe"})
	s.ok("Wait", map[string]any{"seconds": 3})
	s.ok("Shortcut", map[string]any{"keys": "ctrl+n"})
	listing := s.ok("Snapshot", map[string]any{"use_vision": false})
	if !strings.Contains(listing, "Click takes element=N") {
		t.Fatalf("Snapshot listed no elements:\n%s", listing)
	}
	long := strings.Repeat("pasted text ", 20) + "END"
	if out := s.ok("Type", map[string]any{"text": long}); !strings.HasPrefix(out, "Pasted") {
		t.Fatalf("long single-line text was not pasted: %s", out)
	}
	if out := s.ok("Type", map[string]any{"text": " typed !@#"}); !strings.HasPrefix(out, "Typed") {
		t.Fatalf("short text was not typed: %s", out)
	}
	if res := s.call("Click", map[string]any{"element": 999}); !res.IsError {
		t.Fatal("clicking a missing element succeeded")
	}

	payload := make([]byte, 5<<20)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	up := filepath.Join(dir, "up.bin")
	down := filepath.Join(dir, "down.bin")
	if err := os.WriteFile(up, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	remote := `C:\Users\` + target.user + `\win-rdp-mcp-e2e.bin`
	s.ok("FileUpload", map[string]any{"path": remote, "local_path": up})
	s.ok("FileDownload", map[string]any{"path": remote, "local_path": down})
	if got, err := os.ReadFile(down); err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("the file round trip differs (%v)", err)
	}

	s.ok("ReconnectSession", map[string]any{"force": true})
	if out := s.ok("Shell", map[string]any{"command": "'after reconnect'"}); !strings.Contains(out, "after reconnect") {
		t.Fatalf("the agent did not recover after a reconnect: %s", out)
	}
}
