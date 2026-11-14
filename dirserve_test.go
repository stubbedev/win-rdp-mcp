package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestExchangeAbandoned(t *testing.T) {
	drive := t.TempDir()
	rpc := filepath.Join(drive, "rpc-1")
	if err := os.Mkdir(rpc, 0o755); err != nil {
		t.Fatal(err)
	}
	if exchangeAbandoned(rpc) {
		t.Fatal("a present exchange directory was reported abandoned")
	}
	if err := os.Remove(rpc); err != nil {
		t.Fatal(err)
	}
	if !exchangeAbandoned(rpc) {
		t.Fatal("a missing exchange directory on a reachable drive was not reported abandoned")
	}
	if exchangeAbandoned(filepath.Join(drive, "unplugged", "rpc-1")) {
		t.Fatal("an unreachable drive (a reconnect in progress) was reported abandoned")
	}
}

func TestServeDirExitsWhenItsExchangeDirectoryIsRemoved(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "rpc-1")
	done := make(chan error, 1)
	go func() { done <- serveDir(context.Background(), dir, map[string]bool{}) }()

	ready := filepath.Join(dir, "ready")
	deadline := time.Now().Add(10 * time.Second)
	for !fileExists(ready) {
		if time.Now().After(deadline) {
			t.Fatal("serveDir never wrote its ready marker")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serveDir returned %v, want a clean exit", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serveDir kept running after its exchange directory was removed")
	}
}
