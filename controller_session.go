package main

import (
	"context"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/stubbedev/win-rdp-mcp/internal/grdp"
	"github.com/stubbedev/win-rdp-mcp/internal/grdp/plugin/cliprdr"
	"github.com/stubbedev/win-rdp-mcp/internal/grdp/plugin/drdynvc"
	"github.com/stubbedev/win-rdp-mcp/internal/rdpdr"
)

const (
	defaultRDPPort      = "3389"
	dialTimeout         = 15 * time.Second
	connectQuiet        = 500 * time.Millisecond
	connectPaintWait    = 5 * time.Second
	captureQuiet        = 150 * time.Millisecond
	inputReaction       = 500 * time.Millisecond
	capturePaintWait    = time.Second
	paintPollInterval   = 20 * time.Millisecond
	clipboardAckWait    = 5 * time.Second
	clipboardOfferTries = 5
	clipboardOfferPause = time.Second
	driveName           = "mcp"
	minSessionSide      = 200
	maxSessionSide      = 8192
)

type sessionSize struct{ width, height int }

func newSessionSize(width, height int) (sessionSize, error) {
	for _, side := range []struct {
		name string
		v    int
	}{{"width", width}, {"height", height}} {
		if side.v < minSessionSide || side.v > maxSessionSide {
			return sessionSize{}, fmt.Errorf("session %s %d is outside %d..%d", side.name, side.v, minSessionSide, maxSessionSide)
		}
	}
	return sessionSize{width: width, height: height}, nil
}

func withDefaultPort(target string) string {
	if _, _, err := net.SplitHostPort(target); err == nil {
		return target
	}
	return net.JoinHostPort(target, defaultRDPPort)
}

type rdpSession struct {
	host, user, domain, pass string
	size                     sessionSize
	debug                    bool

	life           context.Context
	onFirstConnect func(context.Context)
	onDisconnect   func()
	agentChannel   drdynvc.DvcChannelHandler

	workdir   string
	drive     *rdpdr.Drive
	conn      *rdpConn
	connected bool
	pointer   image.Point
	lastInput time.Time

	mu sync.Mutex
}

type rdpConn struct {
	client *grdp.RdpClient

	fbMu sync.Mutex
	fb   *image.RGBA
	tile *image.RGBA

	done     chan struct{}
	doneOnce sync.Once
	cause    error

	clipboard       atomic.Pointer[string]
	remoteClipboard atomic.Pointer[string]
	lastPaint       atomic.Int64
	fetched         atomic.Pointer[chan struct{}]
}

func (c *rdpConn) remoteClipboardText() string {
	if p := c.remoteClipboard.Load(); p != nil {
		return *p
	}
	return ""
}

func (c *rdpConn) rememberRemoteClipboard(text string) {
	c.remoteClipboard.Store(&text)
}

func (c *rdpConn) clipboardText() string {
	if fetched := c.fetched.Swap(nil); fetched != nil {
		close(*fetched)
	}
	if p := c.clipboard.Load(); p != nil {
		return *p
	}
	return ""
}

var errClipboardRefused = errors.New("the session refused the clipboard offer")

func (c *rdpConn) offerClipboard(ctx context.Context, text string) (<-chan struct{}, error) {
	fetched := make(chan struct{})
	c.fetched.Store(&fetched)
	var err error
	for try := range clipboardOfferTries {
		if try > 0 && !sleepCtx(ctx, clipboardOfferPause) {
			return nil, ctx.Err()
		}
		err = c.offerClipboardOnce(ctx, text)
		if err == nil {
			return fetched, nil
		}
		if !errors.Is(err, errClipboardRefused) && !errors.Is(err, cliprdr.ErrClipboardNotReady) {
			return nil, err
		}
	}
	return nil, fmt.Errorf("%w %d times (is clipboard redirection disabled on the target?)", err, clipboardOfferTries)
}

func (c *rdpConn) offerClipboardOnce(ctx context.Context, text string) error {
	c.clipboard.Store(&text)
	ack, err := c.client.AnnounceClipboard()
	if err != nil {
		return fmt.Errorf("offering text on the session clipboard: %w", err)
	}
	timer := time.NewTimer(clipboardAckWait)
	defer timer.Stop()
	select {
	case accepted := <-ack:
		if !accepted {
			return errClipboardRefused
		}
		return nil
	case <-timer.C:
		return fmt.Errorf("the session did not answer the clipboard offer within %s", clipboardAckWait)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *rdpConn) alive() bool {
	select {
	case <-c.done:
		return false
	default:
		return true
	}
}

func (c *rdpConn) end(cause error) {
	c.doneOnce.Do(func() {
		c.cause = cause
		close(c.done)
	})
}

func (c *rdpConn) exitReason() string {
	reason := "the connection closed"
	if c.cause != nil {
		reason = c.cause.Error()
	}
	if code := c.client.LastErrorInfo(); code != 0 {
		reason += " (" + disconnectReason(code) + ")"
	}
	return reason
}

func (c *rdpConn) waitQuiet(ctx context.Context, quiet, limit time.Duration, needPaint bool) {
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		last := c.lastPaint.Load()
		if (last != 0 || !needPaint) && time.Since(time.Unix(0, last)) >= quiet {
			return
		}
		if !sleepCtx(ctx, paintPollInterval) {
			return
		}
	}
}

func (c *rdpConn) waitPaintAfter(ctx context.Context, since int64, limit time.Duration) bool {
	deadline := time.Now().Add(limit)
	for c.lastPaint.Load() <= since {
		if time.Now().After(deadline) || !sleepCtx(ctx, paintPollInterval) {
			return false
		}
	}
	return true
}

func (c *rdpConn) paint(bitmaps []grdp.Bitmap) {
	defer c.lastPaint.Store(time.Now().UnixNano())
	c.fbMu.Lock()
	defer c.fbMu.Unlock()
	for i := range bitmaps {
		bm := &bitmaps[i]
		c.tile = bm.FillRGBA(c.tile)
		dst := image.Rect(bm.DestLeft, bm.DestTop, bm.DestRight+1, bm.DestBottom+1)
		draw.Draw(c.fb, dst, c.tile, image.Point{}, draw.Src)
	}
}

func (c *rdpConn) capture() *image.RGBA {
	c.fbMu.Lock()
	defer c.fbMu.Unlock()
	out := image.NewRGBA(c.fb.Rect)
	copy(out.Pix, c.fb.Pix)
	return out
}

var disconnectReasons = map[uint32]string{
	0x01: "disconnected by an administrative tool",
	0x02: "logged off by an administrative tool",
	0x03: "idle timeout",
	0x04: "logon timeout",
	0x05: "another connection took the session",
	0x06: "the server ran out of memory",
	0x07: "connection denied",
	0x09: "the account may not log in over RDP",
	0x0A: "fresh credentials are required",
	0x0B: "disconnected by the user",
	0x0C: "logged off by the user",
}

func disconnectReason(code uint32) string {
	if r, ok := disconnectReasons[code]; ok {
		return r
	}
	return fmt.Sprintf("server error info 0x%08x", code)
}

func (s *rdpSession) ensureConnected(ctx context.Context) error {
	if s.conn != nil && s.conn.alive() {
		return nil
	}
	if err := s.prepareDrive(); err != nil {
		return err
	}
	reconnect := s.connected
	if reconnect {
		logf("RDP connection lost (%s); reconnecting", s.conn.exitReason())
		s.conn.client.Close()
	}
	conn, err := s.connect()
	if err != nil {
		return err
	}
	s.conn = conn
	if reconnect {
		conn.waitQuiet(ctx, connectQuiet, connectPaintWait, true)
		logf("RDP session reconnected: %s", s.host)
		return nil
	}
	if err := s.wake(ctx); err != nil {
		return err
	}
	s.connected = true
	logf("RDP session up: %s (%dx%d)", s.host, s.size.width, s.size.height)
	if s.debug {
		logf("exchange dir: %s", s.workdir)
	}
	if s.onFirstConnect != nil {
		go s.onFirstConnect(s.life)
	}
	return nil
}

func (s *rdpSession) prepareDrive() error {
	if s.drive != nil {
		return nil
	}
	dir, err := os.MkdirTemp("", "win-rdp-mcp-ctl-")
	if err != nil {
		return fmt.Errorf("creating the redirected drive directory: %w", err)
	}
	drive, err := rdpdr.NewDrive(driveName, dir)
	if err != nil {
		os.RemoveAll(dir)
		return err
	}
	s.workdir, s.drive = dir, drive
	return nil
}

func (s *rdpSession) connect() (*rdpConn, error) {
	dialer := &net.Dialer{Timeout: dialTimeout}
	client := grdp.NewRdpClient(s.host, s.size.width, s.size.height, func(addr string) (net.Conn, error) {
		return dialer.Dial("tcp", addr)
	})
	conn := &rdpConn{
		client: client,
		fb:     image.NewRGBA(image.Rect(0, 0, s.size.width, s.size.height)),
		done:   make(chan struct{}),
	}
	client.SetDeviceRedirection(s.drive)
	if s.agentChannel != nil {
		client.RegisterDynamicChannel(agentChannelName, s.agentChannel)
	}
	client.OnBitmap(conn.paint)
	client.OnClipboard(conn.rememberRemoteClipboard, conn.clipboardText)
	client.OnError(func(err error) { s.lost(conn, err) })
	client.OnClose(func() { s.lost(conn, nil) })
	if err := client.Login(s.domain, s.user, s.pass); err != nil {
		client.Close()
		reason := err.Error()
		if code := client.LastErrorInfo(); code != 0 {
			reason += " (" + disconnectReason(code) + ")"
		}
		return nil, fmt.Errorf("RDP connection to %s as %s failed: %s", s.host, s.user, reason)
	}
	return conn, nil
}

func (s *rdpSession) lost(conn *rdpConn, cause error) {
	conn.end(cause)
	if s.onDisconnect != nil {
		s.onDisconnect()
	}
}

func (s *rdpSession) wake(ctx context.Context) error {
	if err := s.input().press(keyShift); err != nil {
		return fmt.Errorf("waking the session: %w", err)
	}
	s.conn.waitQuiet(ctx, connectQuiet, connectPaintWait, true)
	return nil
}

var errReconnectRequested = errors.New("a reconnect was requested")

func (s *rdpSession) reconnect(ctx context.Context, force bool) (toolResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn != nil && s.conn.alive() {
		if !force {
			return textResult("The RDP session to %s is connected; pass force to reconnect anyway", s.host), nil
		}
		s.lost(s.conn, errReconnectRequested)
		s.conn.client.Close()
	}
	if err := s.ensureConnected(ctx); err != nil {
		return toolResult{}, err
	}
	return textResult("Reconnected the RDP session to %s", s.host), nil
}

func (s *rdpSession) input() *desktopInput {
	return &desktopInput{sink: s.conn.client, clipboard: s.conn, size: s.size, pointer: &s.pointer}
}

func (s *rdpSession) withInput(ctx context.Context, f func(*desktopInput) (toolResult, error)) (toolResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureConnected(ctx); err != nil {
		return toolResult{}, err
	}
	defer func() { s.lastInput = time.Now() }()
	return f(s.input())
}

func (s *rdpSession) capture(ctx context.Context) (*image.RGBA, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureConnected(ctx); err != nil {
		return nil, err
	}
	if wait := inputReaction - time.Since(s.lastInput); wait > 0 {
		sleepCtx(ctx, wait)
	}
	s.conn.waitQuiet(ctx, captureQuiet, capturePaintWait, false)
	return s.conn.capture(), nil
}

func (s *rdpSession) stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	var errs []error
	if s.conn != nil {
		s.conn.client.Close()
	}
	if s.drive != nil {
		errs = append(errs, s.drive.Close())
	}
	if s.workdir != "" {
		errs = append(errs, os.RemoveAll(s.workdir))
	}
	if err := errors.Join(errs...); err != nil {
		logf("cleaning up the RDP session: %v", err)
	}
}

func (s *rdpSession) snapshot(ctx context.Context, args arguments, proxy *agentProxy) (toolResult, error) {
	img, err := s.capture(ctx)
	if err != nil {
		return toolResult{}, err
	}

	type listing struct {
		text string
		err  error
	}
	var elements chan listing
	if proxy != nil && proxy.ready() {
		elements = make(chan listing, 1)
		go func() {
			r, err := proxy.call(ctx, "Snapshot", map[string]any{"use_vision": false})
			elements <- listing{firstText(r.Content), err}
		}()
	}

	var shot image.Image = img
	if region, ok := regionFromArgs(args); ok {
		region = region.Intersect(img.Bounds())
		if region.Empty() {
			return toolResult{}, fmt.Errorf("the region lies outside the %dx%d session", s.size.width, s.size.height)
		}
		shot = img.SubImage(region)
	}
	var blocks []contentBlock
	if args.boolOr("use_vision", true) {
		data, err := jpegBytes(shot, args.intOr("quality", 75), args.intOr("max_width", 0))
		if err != nil {
			return toolResult{}, fmt.Errorf("encoding the screenshot: %w", err)
		}
		blocks = append(blocks, contentBlock{Type: "image", Data: data, MIME: "image/jpeg"})
	}

	text := fmt.Sprintf("Remote desktop %dx%d (via RDP).", s.size.width, s.size.height)
	switch {
	case elements != nil:
		got := <-elements
		if got.err != nil {
			text += " (window/element list unavailable: " + got.err.Error() + ")"
		} else if got.text != "" {
			text = got.text
		}
	case proxy != nil:
		text += " (window/element list appears once the in-session agent finishes starting.)"
	}
	blocks = append(blocks, contentBlock{Type: "text", Text: text})
	return toolResult{Content: blocks}, nil
}
