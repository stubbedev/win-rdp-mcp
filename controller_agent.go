package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

const (
	agentUNCPath     = `\\tsclient\` + driveName
	bootstrapWait    = 90 * time.Second
	agentReadyWait   = 45 * time.Second
	runDialogSettle  = 1200 * time.Millisecond
	runCommandSettle = 400 * time.Millisecond
)

type agentProxy struct {
	session  *rdpSession
	agentExe string
	enabled  map[string]bool

	up      atomic.Bool
	once    sync.Once
	bootErr atomic.Pointer[error]
}

func (p *agentProxy) rpcDir() string { return filepath.Join(p.session.workdir, "rpc") }
func (p *agentProxy) ready() bool    { return p.up.Load() }

func (p *agentProxy) bootstrap(ctx context.Context) error {
	p.once.Do(func() {
		if err := p.doBootstrap(ctx); err != nil {
			p.bootErr.Store(&err)
		}
	})
	if err := p.bootErr.Load(); err != nil {
		return *err
	}
	return nil
}

func (p *agentProxy) launchCommand() string {
	return agentUNCPath + `\win-rdp-mcp.exe --transport dir --dir ` + agentUNCPath + `\rpc` + p.tierFlags()
}

func (p *agentProxy) doBootstrap(ctx context.Context) error {
	dst := filepath.Join(p.session.workdir, "win-rdp-mcp.exe")
	if err := copyFile(p.agentExe, dst); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("agent binary not found at %s (build it with `just build-windows`, or pass --agent-exe)", p.agentExe)
		}
		return fmt.Errorf("staging agent onto the redirected drive: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(p.rpcDir(), "req"), 0o755); err != nil {
		return fmt.Errorf("creating the agent request directory: %w", err)
	}

	launch := p.launchCommand()
	logf("bootstrapping agent in session: %s", launch)
	steps := []struct {
		settle time.Duration
		act    func(*desktopInput) error
	}{
		{runDialogSettle, func(in *desktopInput) error { return in.press(keyWin, keyR) }},
		{runCommandSettle, func(in *desktopInput) error { return in.typeString(launch) }},
		{0, func(in *desktopInput) error { return in.press(keyEnter) }},
	}
	for _, step := range steps {
		if _, err := p.session.withInput(ctx, func(in *desktopInput) (toolResult, error) {
			return toolResult{}, step.act(in)
		}); err != nil {
			return fmt.Errorf("launching the agent through the Run dialog: %w", err)
		}
		if !sleepCtx(ctx, step.settle) {
			return ctx.Err()
		}
	}

	readyFile := filepath.Join(p.rpcDir(), "ready")
	if err := poll(ctx, agentReadyWait, func() bool { return fileExists(readyFile) },
		"agent did not report ready (Run-dialog launch or Defender may have blocked it)"); err != nil {
		return err
	}
	p.up.Store(true)
	logf("agent ready")
	return nil
}

func (p *agentProxy) tierFlags() string {
	hasTier3 := slices.ContainsFunc(tier3, func(n string) bool { return p.enabled[n] })
	hasTier2 := slices.ContainsFunc(tier2, func(n string) bool { return p.enabled[n] })
	flags := ""
	if hasTier3 {
		flags += " --enable-tier3"
	}
	if !hasTier2 {
		flags += " --disable-tier2"
	}
	return flags
}

func (p *agentProxy) call(ctx context.Context, name string, args map[string]any) (toolResult, error) {
	p.session.mu.Lock()
	err := p.session.ensureConnected(ctx)
	p.session.mu.Unlock()
	if err != nil {
		return toolResult{}, err
	}

	if !p.ready() {
		waitCtx, cancel := context.WithTimeout(ctx, bootstrapWait)
		defer cancel()
		if err := poll(waitCtx, bootstrapWait, func() bool { return p.ready() || p.failed() },
			"the in-session agent did not finish starting"); err != nil {
			return toolResult{}, err
		}
		if err := p.bootErr.Load(); err != nil {
			return toolResult{}, fmt.Errorf("the in-session agent failed to start: %w", *err)
		}
	}

	id, err := newRPCID()
	if err != nil {
		return toolResult{}, err
	}
	data, err := json.Marshal(dirRequest{ID: id, Name: name, Arguments: args})
	if err != nil {
		return toolResult{}, fmt.Errorf("encoding the %s request: %w", name, err)
	}
	if err := writeAtomic(filepath.Join(p.rpcDir(), "req", id+".json"), data); err != nil {
		return toolResult{}, fmt.Errorf("writing request to the redirected drive: %w", err)
	}

	respPath := filepath.Join(p.rpcDir(), "resp", id+".json")
	callCtx, cancel := context.WithTimeout(ctx, toolCallTimeout)
	defer cancel()
	if err := poll(callCtx, toolCallTimeout, func() bool { return fileExists(respPath) },
		"no response from the agent (it may have exited)"); err != nil {
		return toolResult{}, err
	}

	raw, err := os.ReadFile(respPath)
	if err != nil {
		return toolResult{}, fmt.Errorf("reading the agent response: %w", err)
	}
	if err := os.Remove(respPath); err != nil {
		return toolResult{}, fmt.Errorf("removing the agent response: %w", err)
	}
	var resp dirResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return toolResult{}, fmt.Errorf("decoding agent response: %w", err)
	}
	return toolResult{Content: resp.Content, IsError: resp.IsError}, nil
}

func (p *agentProxy) failed() bool { return p.bootErr.Load() != nil }

func newRPCID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generating a request id: %w", err)
	}
	return fmt.Sprintf("%013d-%s", time.Now().UnixMilli(), hex.EncodeToString(b[:])), nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

func writeAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
