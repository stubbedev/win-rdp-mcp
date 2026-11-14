package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/stubbedev/win-rdp-mcp/internal/grdp/plugin/drdynvc"
)

const (
	agentUNCPath      = `\\tsclient\` + driveName
	agentExeName      = "win-rdp-mcp.exe"
	agentStartWait    = 30 * time.Second
	agentReattachWait = 15 * time.Second
	driveAcceptWait   = 60 * time.Second
	runDialogSettle   = 1200 * time.Millisecond
	runCommandSettle  = 400 * time.Millisecond
)

var errAgentChannelClosed = errors.New("the in-session agent's channel closed")

type agentLink struct {
	token string

	mu      sync.Mutex
	current *agentConn
	changed chan struct{}
}

func newAgentLink(token string) *agentLink {
	return &agentLink{token: token, changed: make(chan struct{})}
}

func (l *agentLink) Process([]byte) {}

func (l *agentLink) NewChannelHandler() drdynvc.DvcChannelHandler {
	return &agentConn{link: l, pending: map[string]chan rpcMessage{}}
}

func (l *agentLink) broadcastLocked() {
	close(l.changed)
	l.changed = make(chan struct{})
}

func (l *agentLink) connection() *agentConn {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.current
}

func (l *agentLink) attach(c *agentConn, version string) {
	l.mu.Lock()
	previous := l.current
	l.current = c
	l.broadcastLocked()
	l.mu.Unlock()
	logf("agent %s connected over the %s channel", version, agentChannelName)
	if previous != nil && previous != c {
		previous.retire()
	}
}

func (l *agentLink) detach(c *agentConn) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.current == c {
		l.current = nil
		l.broadcastLocked()
	}
}

func (l *agentLink) wait(ctx context.Context, timeout time.Duration) (*agentConn, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		l.mu.Lock()
		current, changed := l.current, l.changed
		l.mu.Unlock()
		if current != nil {
			return current, nil
		}
		select {
		case <-changed:
		case <-timer.C:
			return nil, fmt.Errorf("the in-session agent did not connect within %s", timeout)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

type agentConn struct {
	link *agentLink

	sendMu sync.Mutex
	send   func([]byte)

	mu      sync.Mutex
	in      frameReader
	pending map[string]chan rpcMessage
	closed  bool
}

func (c *agentConn) SetSendFunc(f func([]byte)) {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	c.send = f
}

func (c *agentConn) OnChannelCreated() {}

func (c *agentConn) OnChannelClosed() {
	c.mu.Lock()
	c.closed = true
	for id, ch := range c.pending {
		close(ch)
		delete(c.pending, id)
	}
	c.mu.Unlock()
	c.link.detach(c)
}

func (c *agentConn) Process(data []byte) {
	c.mu.Lock()
	msgs, err := c.in.push(data)
	c.mu.Unlock()
	if err != nil {
		slog.Error("agent channel: dropping undecodable data", "err", err)
	}
	for _, msg := range msgs {
		switch msg.Kind {
		case rpcHello:
			if msg.Token != c.link.token {
				logf("retiring a stale agent %s left by an earlier controller", msg.Version)
				c.retire()
				continue
			}
			c.link.attach(c, msg.Version)
		case rpcResult:
			c.deliver(msg)
		default:
			slog.Warn("agent channel: unexpected message", "kind", msg.Kind)
		}
	}
}

func (c *agentConn) deliver(msg rpcMessage) {
	c.mu.Lock()
	ch, ok := c.pending[msg.ID]
	delete(c.pending, msg.ID)
	c.mu.Unlock()
	if !ok {
		slog.Warn("agent channel: result for an unknown call", "id", msg.ID)
		return
	}
	ch <- msg
}

func (c *agentConn) write(msg rpcMessage) error {
	frame, err := encodeFrame(msg)
	if err != nil {
		return err
	}
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	if c.send == nil {
		return errAgentChannelClosed
	}
	c.send(frame)
	return nil
}

func (c *agentConn) retire() {
	if err := c.write(rpcMessage{Kind: rpcBye}); err != nil {
		slog.Warn("agent channel: retiring an agent", "err", err)
	}
}

func (c *agentConn) call(ctx context.Context, name string, args map[string]any) (toolResult, error) {
	id, err := newAgentToken()
	if err != nil {
		return toolResult{}, err
	}
	reply := make(chan rpcMessage, 1)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return toolResult{}, errAgentChannelClosed
	}
	c.pending[id] = reply
	c.mu.Unlock()

	if err := c.write(rpcMessage{Kind: rpcCall, ID: id, Name: name, Arguments: args}); err != nil {
		c.forget(id)
		return toolResult{}, err
	}
	select {
	case msg, ok := <-reply:
		if !ok {
			return toolResult{}, fmt.Errorf("%w while %s was running, so its result was lost", errAgentChannelClosed, name)
		}
		return toolResult{Content: msg.Content, IsError: msg.IsError}, nil
	case <-ctx.Done():
		c.forget(id)
		return toolResult{}, ctx.Err()
	}
}

func (c *agentConn) forget(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.pending, id)
}

type agentProxy struct {
	session  *rdpSession
	agentExe string
	enabled  map[string]bool
	link     *agentLink

	launchMu      sync.Mutex
	everConnected bool
	failedLaunch  bool
}

func newAgentProxy(session *rdpSession, agentExe string, enabled map[string]bool) (*agentProxy, error) {
	token, err := newAgentToken()
	if err != nil {
		return nil, err
	}
	return &agentProxy{session: session, agentExe: agentExe, enabled: enabled, link: newAgentLink(token)}, nil
}

func (p *agentProxy) ready() bool { return p.link.connection() != nil }

const runDialogMaxChars = 259

func (p *agentProxy) launchCommand() string {
	return `cmd /c ` + agentUNCPath + `\` + agentExeName + ` --transport rdpvc --token ` + p.link.token + p.tierFlags()
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

func (p *agentProxy) connection(ctx context.Context) (*agentConn, error) {
	p.session.mu.Lock()
	err := p.session.ensureConnected(ctx)
	p.session.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if c := p.link.connection(); c != nil {
		return c, nil
	}

	p.launchMu.Lock()
	defer p.launchMu.Unlock()
	if c := p.link.connection(); c != nil {
		return c, nil
	}
	if p.everConnected {
		if c, err := p.link.wait(ctx, agentReattachWait); err == nil {
			return c, nil
		}
		logf("the in-session agent did not come back within %s; launching it again", agentReattachWait)
	}
	if err := p.launch(ctx, p.failedLaunch); err != nil {
		p.failedLaunch = true
		return nil, err
	}
	c, err := p.link.wait(ctx, agentStartWait)
	if err != nil {
		p.failedLaunch = true
		return nil, fmt.Errorf("%w (the Run-dialog launch failed, or Defender blocked the agent)", err)
	}
	p.everConnected, p.failedLaunch = true, false
	return c, nil
}

func (p *agentProxy) launch(ctx context.Context, dismissLeftovers bool) error {
	acceptCtx, cancel := context.WithTimeout(ctx, driveAcceptWait)
	err := p.session.drive.WaitAccepted(acceptCtx)
	cancel()
	if err != nil {
		return fmt.Errorf("the session did not accept the redirected drive within %s (is drive redirection disabled on the target?): %w", driveAcceptWait, err)
	}
	dst := filepath.Join(p.session.workdir, agentExeName)
	if err := copyFile(p.agentExe, dst); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("agent binary not found at %s (build it with `just build-windows`, or pass --agent-exe)", p.agentExe)
		}
		return fmt.Errorf("staging the agent onto the redirected drive: %w", err)
	}

	launch := p.launchCommand()
	logf("launching the agent in the session: %s", launch)
	type step struct {
		settle time.Duration
		act    func(*desktopInput) error
	}
	var steps []step
	if dismissLeftovers {
		steps = append(steps, step{runCommandSettle, func(in *desktopInput) error { return in.press(keyEsc) }})
	}
	steps = append(steps,
		step{runDialogSettle, func(in *desktopInput) error { return in.press(keyWin, keyR) }},
		step{runCommandSettle, func(in *desktopInput) error { return in.paste(ctx, launch) }},
		step{0, func(in *desktopInput) error { return in.press(keyDel) }},
		step{0, func(in *desktopInput) error { return in.press(keyEnter) }},
	)
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
	return nil
}

func (p *agentProxy) call(ctx context.Context, name string, args map[string]any) (toolResult, error) {
	conn, err := p.connection(ctx)
	if err != nil {
		return toolResult{}, err
	}
	callCtx, cancel := context.WithTimeout(ctx, toolCallTimeout)
	defer cancel()
	result, err := conn.call(callCtx, name, args)
	if errors.Is(err, errAgentChannelClosed) {
		p.session.mu.Lock()
		if p.session.conn != nil && !p.session.conn.alive() {
			err = fmt.Errorf("%w: the RDP connection dropped (%s)", err, p.session.conn.exitReason())
		}
		p.session.mu.Unlock()
	}
	return result, err
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
