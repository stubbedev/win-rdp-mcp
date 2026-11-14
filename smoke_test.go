package main

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const smokeServerEnv = "WIN_RDP_MCP_SMOKE_SERVER"

func TestMain(m *testing.M) {
	if os.Getenv(smokeServerEnv) == "1" {
		if err := run(os.Args[1:]); err != nil {
			logf("%v", err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestStdioSmoke(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.Command(os.Args[0], "--transport", "stdio", "--enable-all")
	cmd.Env = append(os.Environ(), smokeServerEnv+"=1")
	client := mcp.NewClient(&mcp.Implementation{Name: "smoke", Version: "0"}, nil)
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatalf("connecting to the stdio server: %v", err)
	}
	defer session.Close()

	list, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	specs, err := loadToolSpecs()
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Tools) != len(specs) {
		t.Fatalf("tools/list returned %d tools, want all %d with --enable-all", len(list.Tools), len(specs))
	}
	for _, tool := range list.Tools {
		if tool.Name == "" || tool.InputSchema == nil {
			t.Errorf("malformed tool %+v", tool)
		}
	}

	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "Wait", Arguments: map[string]any{"seconds": 0.05}})
	if err != nil {
		t.Fatalf("tools/call Wait: %v", err)
	}
	if res.IsError || len(res.Content) == 0 {
		t.Fatalf("Wait failed: %+v", res)
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok || !strings.HasPrefix(text.Text, "[task:") {
		t.Fatalf("Wait result is missing its task id: %+v", res.Content[0])
	}
}
