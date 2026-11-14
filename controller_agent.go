package main

import (
	"context"
	"errors"
	"fmt"
	"image"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/stubbedev/win-rdp-mcp/internal/grdp/plugin/drdynvc"
)

const (
	agentUNCPath        = `\\tsclient\` + driveName
	agentExeName        = "win-rdp-mcp.exe"
	agentStartWait      = 30 * time.Second
	agentReattachWait   = 15 * time.Second
	agentAdoptWait      = 2 * time.Second
	driveAcceptWait     = 60 * time.Second
	runDialogSettle     = 1200 * time.Millisecond
	runDialogTries      = 3
	screenCheckWait     = 3 * time.Second
	screenCheckInterval = 100 * time.Millisecond
)

var errAgentChannelClosed = errors.New("the in-session agent's channel closed")

type agentLink struct {
	build   string
	enabled []string

	mu      sync.Mutex
	current *agentConn
	changed chan struct{}
}

func newAgentLink(build string, enabled []string) *agentLink {
	return &agentLink{build: build, enabled: enabled, changed: make(chan struct{})}
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

func (l *agentLink) dropAll() {
	if c := l.connection(); c != nil {
		c.OnChannelClosed()
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
			if msg.Build != c.link.build {
				logf("retiring an agent of another build (%s, version %s); this controller pushes %s", msg.Build, msg.Version, c.link.build)
				c.retire()
				continue
			}
			if err := c.write(rpcMessage{Kind: rpcConfigure, Enabled: c.link.enabled}); err != nil {
				slog.Error("agent channel: configuring the agent", "err", err)
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

func (c *agentConn) request(ctx context.Context, msg rpcMessage, what string) (rpcMessage, error) {
	id, err := newCallID()
	if err != nil {
		return rpcMessage{}, err
	}
	msg.ID = id
	reply := make(chan rpcMessage, 1)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return rpcMessage{}, errAgentChannelClosed
	}
	c.pending[id] = reply
	c.mu.Unlock()

	if err := c.write(msg); err != nil {
		c.forget(id)
		return rpcMessage{}, err
	}
	select {
	case res, ok := <-reply:
		if !ok {
			return rpcMessage{}, fmt.Errorf("%w while %s was running, so its result was lost", errAgentChannelClosed, what)
		}
		return res, nil
	case <-ctx.Done():
		c.forget(id)
		return rpcMessage{}, ctx.Err()
	}
}

func (c *agentConn) call(ctx context.Context, name string, args map[string]any) (toolResult, error) {
	res, err := c.request(ctx, rpcMessage{Kind: rpcCall, Name: name, Arguments: args}, name)
	if err != nil {
		return toolResult{}, err
	}
	return toolResult{Content: res.Content, IsError: res.IsError}, nil
}

func (c *agentConn) elementCenter(ctx context.Context, index int) (image.Point, error) {
	res, err := c.request(ctx, rpcMessage{Kind: rpcElement, Index: index}, "the element lookup")
	if err != nil {
		return image.Point{}, err
	}
	if res.IsError {
		return image.Point{}, errors.New(firstText(res.Content))
	}
	return image.Pt(res.X, res.Y), nil
}

func (c *agentConn) copyFile(ctx context.Context, tool, src, dst string) (int64, error) {
	res, err := c.request(ctx, rpcMessage{Kind: rpcCopyFile, Name: tool, Text: src, Path: dst}, tool)
	if err != nil {
		return 0, err
	}
	if res.IsError {
		return 0, errors.New(firstText(res.Content))
	}
	return res.Bytes, nil
}

func (c *agentConn) restoreClipboard(ctx context.Context, text string) error {
	res, err := c.request(ctx, rpcMessage{Kind: rpcRestoreClipboard, Text: text}, "the clipboard restore")
	if err != nil {
		return err
	}
	if res.IsError {
		return errors.New(firstText(res.Content))
	}
	return nil
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
	adoptChecked  bool
	failedLaunch  bool
}

func newAgentProxy(session *rdpSession, agentExe string, enabled map[string]bool) (*agentProxy, error) {
	f, err := os.Open(agentExe)
	if err != nil {
		return nil, fmt.Errorf("opening the agent binary: %w", err)
	}
	defer f.Close()
	build, err := buildIDOf(f)
	if err != nil {
		return nil, err
	}
	return &agentProxy{session: session, agentExe: agentExe, enabled: enabled, link: newAgentLink(build, toolNames(enabled))}, nil
}

func (p *agentProxy) ready() bool { return p.link.connection() != nil }

const runDialogMaxChars = 259

func (p *agentProxy) launchCommand() string {
	return `cmd /c ` + agentUNCPath + `\` + agentExeName + ` --transport rdpvc --build ` + p.link.build
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
	switch {
	case p.everConnected:
		if c, err := p.link.wait(ctx, agentReattachWait); err == nil {
			return c, nil
		}
		logf("the in-session agent did not come back within %s; launching it again", agentReattachWait)
	case !p.adoptChecked:
		p.adoptChecked = true
		if c, err := p.link.wait(ctx, agentAdoptWait); err == nil {
			p.everConnected = true
			return c, nil
		}
	}
	p.session.mu.Lock()
	userClipboard := p.session.conn.remoteClipboardText()
	p.session.mu.Unlock()
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
	if err := c.restoreClipboard(ctx, userClipboard); err != nil {
		logf("could not restore the session clipboard after launching the agent: %v", err)
	}
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
		return fmt.Errorf("staging the agent %s onto the redirected drive: %w", p.agentExe, err)
	}

	launch := p.launchCommand()
	logf("launching the agent in the session: %s", launch)
	_, err = p.session.withInput(ctx, func(in *desktopInput) (toolResult, error) {
		var err error
		for try := range runDialogTries {
			err = p.runInSession(ctx, in, launch, dismissLeftovers || try > 0)
			if !errors.Is(err, errRunDialogNotShown) {
				break
			}
		}
		return toolResult{}, err
	})
	if err != nil {
		return fmt.Errorf("launching the agent through the Run dialog: %w", err)
	}
	return nil
}

var (
	errRunDialogNotShown = errors.New("the Run dialog did not come up in front after Win+R (another window kept focus, or a Run dialog was already open), so nothing was pasted")
	errPasteMissedRunBox = errors.New("the pasted command did not appear in the Run dialog, so Enter was not pressed")
)

func (p *agentProxy) runInSession(ctx context.Context, in *desktopInput, command string, dismissLeftovers bool) error {
	conn := p.session.conn
	area := runDialogArea(p.session.size.width, p.session.size.height)
	settle := func() { conn.waitQuiet(ctx, captureQuiet, runDialogSettle, false) }
	if dismissLeftovers {
		painted := conn.lastPaint.Load()
		if err := in.press(keyEsc); err != nil {
			return err
		}
		conn.waitPaintAfter(ctx, painted, runDialogSettle)
		settle()
	}

	beforeRun := conn.capture()
	if err := in.press(keyWin, keyR); err != nil {
		return err
	}
	_, change := awaitScreen(ctx, conn, beforeRun, area, screenChange.dialogAppeared)
	if !change.dialogAppeared() {
		return fmt.Errorf("%w [%s]", errRunDialogNotShown, change)
	}
	settle()
	beforePaste := conn.capture()

	if err := in.paste(ctx, command); err != nil {
		return err
	}
	if _, change := awaitScreen(ctx, conn, beforePaste, area, screenChange.landedInside); !change.landedInside() {
		return fmt.Errorf("%w [%s]", errPasteMissedRunBox, change)
	}
	settle()
	if err := in.press(keyDel); err != nil {
		return err
	}
	return in.press(keyEnter)
}

func (p *agentProxy) transfer(ctx context.Context, tool string, args arguments) (toolResult, error) {
	local, err := filepath.Abs(args.stringOr("local_path", ""))
	if err != nil {
		return toolResult{}, fmt.Errorf("resolving local_path: %w", err)
	}
	target := args.stringOr("path", "")
	if tool == "FileUpload" && args.has("data_base64") {
		return toolResult{}, errors.New("give either local_path or data_base64, not both")
	}
	conn, err := p.connection(ctx)
	if err != nil {
		return toolResult{}, err
	}
	id, err := newCallID()
	if err != nil {
		return toolResult{}, err
	}
	staged := filepath.Join(p.session.workdir, "xfer-"+id)
	stagedUNC := agentUNCPath + `\xfer-` + id
	defer os.Remove(staged)
	callCtx, cancel := context.WithTimeout(ctx, toolCallTimeout)
	defer cancel()

	if tool == "FileUpload" {
		if _, err := copyFileSized(local, staged); err != nil {
			return toolResult{}, fmt.Errorf("reading %s: %w", local, err)
		}
		n, err := conn.copyFile(callCtx, tool, stagedUNC, target)
		if err != nil {
			return toolResult{}, err
		}
		return textResult("Uploaded %s from %s to %s", humanBytes(n), local, target), nil
	}
	n, err := conn.copyFile(callCtx, tool, target, stagedUNC)
	if err != nil {
		return toolResult{}, err
	}
	if _, err := copyFileCreatingDirs(staged, local); err != nil {
		return toolResult{}, fmt.Errorf("writing %s: %w", local, err)
	}
	return textResult("Downloaded %s from %s to %s", humanBytes(n), target, local), nil
}

func awaitScreen(ctx context.Context, conn *rdpConn, before *image.RGBA, area image.Rectangle, done func(screenChange) bool) (*image.RGBA, screenChange) {
	deadline := time.Now().Add(screenCheckWait)
	for {
		now := conn.capture()
		change := diffScreens(before, now, area)
		if done(change) || time.Now().After(deadline) || !sleepCtx(ctx, screenCheckInterval) {
			return now, change
		}
	}
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
	_, err := copyFileSized(src, dst)
	return err
}

func copyFileCreatingDirs(src, dst string) (int64, error) {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return 0, err
	}
	return copyFileSized(src, dst)
}

func copyFileSized(src, dst string) (int64, error) {
	in, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer in.Close()
	tmp := dst + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(out, in)
	if err != nil {
		out.Close()
		os.Remove(tmp)
		return n, err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return n, err
	}
	return n, os.Rename(tmp, dst)
}
