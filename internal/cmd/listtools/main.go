package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "listtools: %v\n", err)
		os.Exit(1)
	}
}

func run(argv []string) error {
	if len(argv) == 0 {
		return fmt.Errorf("usage: listtools <server binary> [server flags...]")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.Command(argv[0], append([]string{"--transport", "stdio"}, argv[1:]...)...)
	client := mcp.NewClient(&mcp.Implementation{Name: "listtools", Version: "0"}, nil)
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		return fmt.Errorf("starting %s: %w", argv[0], err)
	}
	defer session.Close()

	list, err := session.ListTools(ctx, nil)
	if err != nil {
		return fmt.Errorf("tools/list: %w", err)
	}
	fmt.Printf("%d tools:\n", len(list.Tools))
	for _, tool := range list.Tools {
		fmt.Printf("  %s\n", tool.Name)
	}
	return nil
}
