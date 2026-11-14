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
	reconnectSettle     = 2 * time.Second
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

func (s sessionSize) center() (int, int) { return s.width / 2, s.height / 2 }

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
	agentChannel   drdynvc.DvcChannelHandler

	workdir   string
	drive     *rdpdr.Drive
	conn      *rdpConn
	connected bool
	pointer   image.Point

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

	clipboard atomic.Pointer[string]
}

func (c *rdpConn) clipboardText() string {
	if p := c.clipboard.Load(); p != nil {
		return *p
	}
	return ""
}

var errClipboardRefused = errors.New("the session refused the clipboard offer")

func (c *rdpConn) offerClipboard(ctx context.Context, text string) error {
	var err error
	for try := range clipboardOfferTries {
		if try > 0 && !sleepCtx(ctx, clipboardOfferPause) {
			return ctx.Err()
		}
		err = c.offerClipboardOnce(ctx, text)
		if !errors.Is(err, errClipboardRefused) && !errors.Is(err, cliprdr.ErrClipboardNotReady) {
			return err
		}
	}
	return fmt.Errorf("%w %d times (is clipboard redirection disabled on the target?)", err, clipboardOfferTries)
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

func (c *rdpConn) paint(bitmaps []grdp.Bitmap) {
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
		sleepCtx(ctx, reconnectSettle)
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
	client.OnClipboard(nil, conn.clipboardText)
	client.OnError(func(err error) { conn.end(err) })
	client.OnClose(func() { conn.end(nil) })
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

func (s *rdpSession) wake(ctx context.Context) error {
	in := s.input()
	x, y := s.size.center()
	if _, err := in.clickAt(x, y, pointerLeft, 1); err != nil {
		return fmt.Errorf("waking the session: %w", err)
	}
	sleepCtx(ctx, 700*time.Millisecond)
	if err := in.press(keyEnter); err != nil {
		return fmt.Errorf("waking the session: %w", err)
	}
	sleepCtx(ctx, 3*time.Second)
	return nil
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
	return f(s.input())
}

func (s *rdpSession) capture(ctx context.Context) (*image.RGBA, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureConnected(ctx); err != nil {
		return nil, err
	}
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

	var blocks []contentBlock
	if args.boolOr("use_vision", true) {
		data, err := jpegBytes(img, args.intOr("quality", 75), args.intOr("max_width", 0))
		if err != nil {
			return toolResult{}, fmt.Errorf("encoding the screenshot: %w", err)
		}
		blocks = append(blocks, contentBlock{Type: "image", Data: data, MIME: "image/jpeg"})
	}

	text := fmt.Sprintf("Remote desktop %dx%d (via RDP).", s.size.width, s.size.height)
	switch {
	case proxy != nil && proxy.ready():
		r, err := proxy.call(ctx, "Snapshot", map[string]any{"use_vision": false})
		if err != nil {
			text += " (window/element list unavailable: " + err.Error() + ")"
		} else if t := firstText(r.Content); t != "" {
			text = t
		}
	case proxy != nil:
		text += " (window/element list appears once the in-session agent finishes starting.)"
	}
	blocks = append(blocks, contentBlock{Type: "text", Text: text})
	return toolResult{Content: blocks}, nil
}
