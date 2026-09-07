//go:build linux

package main

import (
	"bytes"
	"context"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"image"
	_ "image/png" // register the PNG decoder for encodeSnapshot
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/pflag"
)

// The controller is the operator-side half of the system. It runs on Linux, is
// the MCP server the AI connects to over stdio, and drives the Windows target
// entirely over RDP:
//
//   - The desktop tier (screenshots, mouse, keyboard) is served locally by
//     holding a live xfreerdp session inside a headless Xvfb and wrapping it
//     with import (capture) and xdotool (input). No footprint on the target.
//   - The system tier (window tree, shell, registry, files, services, ...) is
//     served by the agent, which the controller pushes into the RDP session
//     over drive redirection and talks to through the dir transport — files on
//     that same redirected drive. Still nothing but RDP on the wire.
//
// The operator provides only RDP credentials; everything else is bootstrapped.

// desktopTools are answered locally via xdotool/import — they need no agent, so
// they work the moment the RDP session is up. Everything else is proxied to the
// in-session agent.
var desktopTools = map[string]bool{
	"Click": true, "Type": true, "Move": true, "Scroll": true,
	"Shortcut": true, "Wait": true, "Snapshot": true,
}

func controlMain(argv []string) error {
	fs := pflag.NewFlagSet("win-rdp-mcp control", pflag.ContinueOnError)
	fs.SortFlags = false
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "win-rdp-mcp control %s — drive a Windows target over RDP, expose it as MCP\n\n"+
			"Usage:\n  win-rdp-mcp control --target HOST --user USER [flags]\n\n"+
			"The password comes from --pass-file or $%s. Flags:\n", Version, envTargetPass)
		fmt.Fprint(fs.Output(), fs.FlagUsages())
	}

	var (
		target      = fs.StringP("target", "t", "", "target host or host:port (RDP)")
		user        = fs.StringP("user", "u", "", "RDP username")
		domain      = fs.StringP("domain", "D", "", "RDP domain (blank for a local account)")
		passFile    = fs.StringP("pass-file", "p", "", "file holding the RDP password (else $"+envTargetPass+")")
		width       = fs.IntP("width", "W", 1280, "session width in pixels")
		height      = fs.IntP("height", "H", 800, "session height in pixels")
		agentExe    = fs.String("agent-exe", "", "path to the Windows agent binary to push (default: ./win-rdp-mcp.exe)")
		noAgent     = fs.Bool("no-agent", false, "desktop tools only; do not push the in-session agent")
		debug       = fs.BoolP("debug", "d", false, "log RDP and bootstrap detail to stderr")
		enableAll   = fs.BoolP("enable-all", "a", false, "enable every tool, including destructive tier 3")
		enableTier3 = fs.BoolP("enable-tier3", "3", false, "enable the destructive tier 3 tools")
		disableT2   = fs.BoolP("disable-tier2", "2", false, "disable the interactive tier 2 tools")
	)
	if err := fs.Parse(argv); err != nil {
		if errors.Is(err, pflag.ErrHelp) {
			return nil
		}
		return err
	}

	pass, err := resolvePassword(*passFile)
	if err != nil {
		return err
	}
	if *target == "" || *user == "" {
		fs.Usage()
		return fmt.Errorf("both --target and --user are required")
	}

	enabled, err := resolveEnabledTools(toolSelection{
		enableTier3:  *enableTier3,
		disableTier2: *disableT2,
		enableAll:    *enableAll,
	})
	if err != nil {
		return err
	}

	sess := &rdpSession{
		host: *target, user: *user, domain: *domain, pass: pass,
		width: *width, height: *height, debug: *debug,
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := sess.start(ctx); err != nil {
		return fmt.Errorf("starting RDP session: %w", err)
	}
	defer sess.stop()
	logf("RDP session up: %s (%dx%d)", sess.host, sess.width, sess.height)
	if *debug {
		logf("exchange dir: %s", sess.workdir)
	}

	var proxy *agentProxy
	if !*noAgent {
		exe := *agentExe
		if exe == "" {
			exe = defaultAgentExe()
		}
		proxy = &agentProxy{session: sess, agentExe: exe, enabled: enabled}
		// Bootstrap runs in the background so the desktop tier is usable
		// immediately; system tools report "agent still starting" until it is
		// ready, rather than blocking startup on it.
		go func() {
			if err := proxy.bootstrap(ctx); err != nil {
				logf("agent bootstrap failed (desktop tools still work): %v", err)
			}
		}()
	}

	srv := mcp.NewServer(
		&mcp.Implementation{Name: "win-rdp-mcp-control", Version: Version},
		&mcp.ServerOptions{Instructions: controllerInstructions(*noAgent)},
	)
	if err := registerControllerTools(srv, enabled, sess, proxy); err != nil {
		return err
	}

	logf("controller ready — %d tools (%d desktop-local, rest via agent)", len(enabled), countLocal(enabled))
	if err := srv.Run(ctx, &mcp.StdioTransport{}); err != nil && ctx.Err() == nil {
		return err
	}
	return nil
}

const envTargetPass = "WIN_RDP_TARGET_PASS"

func resolvePassword(passFile string) (string, error) {
	if passFile != "" {
		data, err := os.ReadFile(passFile)
		if err != nil {
			return "", fmt.Errorf("reading --pass-file: %w", err)
		}
		return strings.TrimRight(string(data), "\r\n"), nil
	}
	if p := os.Getenv(envTargetPass); p != "" {
		return p, nil
	}
	return "", fmt.Errorf("no password: set $%s or pass --pass-file", envTargetPass)
}

func defaultAgentExe() string {
	// Next to the controller binary, then the working directory.
	if self, err := os.Executable(); err == nil {
		cand := filepath.Join(filepath.Dir(self), "win-rdp-mcp.exe")
		if fileExists(cand) {
			return cand
		}
	}
	return "win-rdp-mcp.exe"
}

func countLocal(enabled map[string]bool) int {
	n := 0
	for name := range enabled {
		if desktopTools[name] {
			n++
		}
	}
	return n
}

// ── RDP session ──────────────────────────────────────────────────────────────

// rdpSession holds one persistent headless RDP connection: an Xvfb display with
// xfreerdp drawing the remote desktop into it. Screenshots read that display;
// input is injected into it and FreeRDP relays it to the target. Coordinates
// are 1:1 because the Xvfb, the RDP session and the remote desktop are all the
// same size.
type rdpSession struct {
	host, user, domain, pass string
	width, height            int
	debug                    bool

	display string // e.g. ":140"
	workdir string // exposed to the session as \\tsclient\mcp
	xvfb    *exec.Cmd
	rdp     *exec.Cmd

	mu sync.Mutex // desktop input/capture is one physical device: serialise it
}

func (s *rdpSession) start(ctx context.Context) error {
	dir, err := os.MkdirTemp("", "win-rdp-mcp-ctl-")
	if err != nil {
		return err
	}
	s.workdir = dir

	display, err := allocDisplay()
	if err != nil {
		return err
	}
	s.display = display

	s.xvfb = exec.Command("Xvfb", s.display, "-screen", "0",
		fmt.Sprintf("%dx%dx24", s.width, s.height), "-nolisten", "tcp")
	if err := s.xvfb.Start(); err != nil {
		return fmt.Errorf("starting Xvfb: %w", err)
	}
	if err := s.waitForX(ctx); err != nil {
		s.stop()
		return err
	}

	args := []string{
		"/v:" + s.host,
		"/u:" + s.user,
		"/p:" + s.pass,
		"/cert:ignore", "/sec:nla",
		fmt.Sprintf("/size:%dx%d", s.width, s.height),
		"/drive:mcp," + s.workdir, // the file channel + agent delivery
		"+clipboard",
		"/log-level:" + logLevel(s.debug),
	}
	if s.domain != "" {
		args = append(args, "/d:"+s.domain)
	}
	s.rdp = exec.Command("xfreerdp", args...)
	s.rdp.Env = append(os.Environ(), "DISPLAY="+s.display, "KRB5_CONFIG=/dev/null")
	if s.debug {
		s.rdp.Stderr = os.Stderr
	}
	if err := s.rdp.Start(); err != nil {
		s.stop()
		return fmt.Errorf("starting xfreerdp: %w", err)
	}

	if err := s.waitForWindow(ctx); err != nil {
		s.stop()
		return err
	}
	// A fresh connection often lands on the lock screen; a click plus Enter
	// dismisses it and NLA completes the logon to a live desktop.
	s.wake(ctx)
	return nil
}

func (s *rdpSession) stop() {
	if s.rdp != nil && s.rdp.Process != nil {
		s.rdp.Process.Kill()
	}
	if s.xvfb != nil && s.xvfb.Process != nil {
		s.xvfb.Process.Kill()
	}
	if s.workdir != "" {
		os.RemoveAll(s.workdir)
	}
}

func logLevel(debug bool) string {
	if debug {
		return "INFO"
	}
	return "WARN"
}

// waitForX blocks until the Xvfb display accepts connections.
func (s *rdpSession) waitForX(ctx context.Context) error {
	return poll(ctx, 10*time.Second, func() bool {
		return exec.Command("xdpyinfo", "-display", s.display).Run() == nil
	}, "Xvfb display "+s.display+" did not come up")
}

// waitForWindow blocks until xfreerdp has mapped its window (i.e. the RDP
// connection succeeded and is drawing).
func (s *rdpSession) waitForWindow(ctx context.Context) error {
	return poll(ctx, 30*time.Second, func() bool {
		return s.windowID() != ""
	}, "xfreerdp window never appeared (RDP connection failed — check host/credentials)")
}

func (s *rdpSession) windowID() string {
	for _, sel := range [][]string{{"--name", "FreeRDP"}, {"--class", "xfreerdp"}} {
		out, err := s.xdo(append([]string{"search"}, sel...)...)
		if err == nil {
			if id := strings.TrimSpace(firstLine(out)); id != "" {
				return id
			}
		}
	}
	return ""
}

// wake nudges a possible lock screen: activate the window, click, press Enter,
// and give the desktop a moment to paint.
func (s *rdpSession) wake(ctx context.Context) {
	if id := s.windowID(); id != "" {
		s.xdo("windowactivate", id)
	}
	s.xdo("mousemove", strconv.Itoa(s.width/2), strconv.Itoa(s.height/2), "click", "1")
	sleepCtx(ctx, 700*time.Millisecond)
	s.xdo("key", "Return")
	sleepCtx(ctx, 3*time.Second)
}

// xdo runs an xdotool command against this session's display.
func (s *rdpSession) xdo(args ...string) (string, error) {
	cmd := exec.Command("xdotool", args...)
	cmd.Env = append(os.Environ(), "DISPLAY="+s.display)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("xdotool %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

// capture grabs the whole display as PNG bytes — the remote desktop at 1:1.
func (s *rdpSession) capture() ([]byte, error) {
	cmd := exec.Command("import", "-window", "root", "png:-")
	cmd.Env = append(os.Environ(), "DISPLAY="+s.display)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("screen capture failed: %w: %s", err, strings.TrimSpace(errb.String()))
	}
	return out.Bytes(), nil
}

// ── Desktop tools (local) ────────────────────────────────────────────────────

func (s *rdpSession) snapshot(ctx context.Context, args arguments, proxy *agentProxy) (toolResult, error) {
	s.mu.Lock()
	png, err := s.capture()
	s.mu.Unlock()
	if err != nil {
		return toolResult{}, err
	}

	var blocks []contentBlock
	if args.boolOr("use_vision", true) {
		data, mime := encodeSnapshot(png, args.intOr("max_width", 0), args.intOr("quality", 75))
		blocks = append(blocks, contentBlock{Type: "image", Data: data, MIME: mime})
	}

	// If the agent is up, borrow its window/element enumeration (screenshot
	// suppressed) so the snapshot carries clickable targets, not just pixels.
	text := fmt.Sprintf("Remote desktop %dx%d (via RDP).", s.width, s.height)
	if proxy != nil && proxy.ready() {
		sub := map[string]any{"use_vision": false}
		if r, err := proxy.call(ctx, "Snapshot", sub); err == nil {
			if t := firstText(r.Content); t != "" {
				text = t
			}
		}
	} else if proxy != nil {
		text += " (window/element list appears once the in-session agent finishes starting.)"
	}
	blocks = append(blocks, contentBlock{Type: "text", Text: text})
	return toolResult{Content: blocks}, nil
}

func (s *rdpSession) click(_ context.Context, args arguments) (toolResult, error) {
	x, y := args.intOr("x", 0), args.intOr("y", 0)
	button := map[string]string{"left": "1", "middle": "2", "right": "3"}[args.stringOr("button", "left")]
	if button == "" {
		button = "1"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch action := args.stringOr("action", "click"); action {
	case "hover":
		if _, err := s.xdo("mousemove", itoa(x), itoa(y)); err != nil {
			return toolResult{}, err
		}
		return textResult("Hovered at (%d,%d)", x, y), nil
	case "double":
		if _, err := s.xdo("mousemove", itoa(x), itoa(y), "click", "--repeat", "2", button); err != nil {
			return toolResult{}, err
		}
		return textResult("Double-clicked at (%d,%d)", x, y), nil
	default:
		if _, err := s.xdo("mousemove", itoa(x), itoa(y), "click", button); err != nil {
			return toolResult{}, err
		}
		return textResult("Clicked %s at (%d,%d)", args.stringOr("button", "left"), x, y), nil
	}
}

func (s *rdpSession) typeText(_ context.Context, args arguments) (toolResult, error) {
	text := args.stringOr("text", "")
	s.mu.Lock()
	defer s.mu.Unlock()
	if x, y := args.intOr("x", 0), args.intOr("y", 0); x != 0 || y != 0 {
		if _, err := s.xdo("mousemove", itoa(x), itoa(y), "click", "1"); err != nil {
			return toolResult{}, err
		}
	}
	if args.boolOr("clear", false) {
		s.xdo("key", "ctrl+a")
		s.xdo("key", "Delete")
	}
	if text != "" {
		if _, err := s.xdo("type", "--", text); err != nil {
			return toolResult{}, err
		}
	}
	if args.boolOr("press_enter", false) {
		s.xdo("key", "Return")
	}
	return textResult("Typed %d characters", len([]rune(text))), nil
}

func (s *rdpSession) move(_ context.Context, args arguments) (toolResult, error) {
	x, y := args.intOr("x", 0), args.intOr("y", 0)
	s.mu.Lock()
	defer s.mu.Unlock()
	if args.boolOr("drag", false) {
		sx, sy := args.intOr("start_x", 0), args.intOr("start_y", 0)
		if sx != 0 || sy != 0 {
			s.xdo("mousemove", itoa(sx), itoa(sy))
		}
		s.xdo("mousedown", "1")
		s.xdo("mousemove", itoa(x), itoa(y))
		s.xdo("mouseup", "1")
		return textResult("Dragged to (%d,%d)", x, y), nil
	}
	if _, err := s.xdo("mousemove", itoa(x), itoa(y)); err != nil {
		return toolResult{}, err
	}
	return textResult("Moved to (%d,%d)", x, y), nil
}

func (s *rdpSession) scroll(_ context.Context, args arguments) (toolResult, error) {
	amount := args.intOr("amount", 0)
	horizontal := args.boolOr("horizontal", false)
	s.mu.Lock()
	defer s.mu.Unlock()
	if x, y := args.intOr("x", 0), args.intOr("y", 0); x != 0 || y != 0 {
		s.xdo("mousemove", itoa(x), itoa(y))
	}
	// xdotool scrolls one "click" per button press: 4/5 vertical, 6/7 horizontal.
	button := "4" // up
	if amount < 0 {
		button = "5" // down
	}
	if horizontal {
		if amount < 0 {
			button = "6"
		} else {
			button = "7"
		}
	}
	n := amount
	if n < 0 {
		n = -n
	}
	for range n {
		s.xdo("click", button)
	}
	dir := "vertically"
	if horizontal {
		dir = "horizontally"
	}
	return textResult("Scrolled %d %s", amount, dir), nil
}

func (s *rdpSession) shortcut(_ context.Context, args arguments) (toolResult, error) {
	raw := args.stringOr("keys", "")
	combo := xdotoolCombo(raw)
	if combo == "" {
		return toolResult{}, fmt.Errorf("no keys given")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.xdo("key", combo); err != nil {
		return toolResult{}, err
	}
	return textResult("Executed shortcut: %s", raw), nil
}

func waitTool2(ctx context.Context, args arguments) (toolResult, error) {
	d := min(max(time.Duration(args.floatOr("seconds", 1.0)*float64(time.Second)), 0), maxWait)
	if !sleepCtx(ctx, d) {
		return toolResult{}, ctx.Err()
	}
	return textResult("Waited %s", d), nil
}

// xdotoolCombo maps a "ctrl+shift+esc" style string onto xdotool keysyms.
func xdotoolCombo(raw string) string {
	repl := map[string]string{
		"win": "super", "cmd": "super", "meta": "super",
		"esc": "Escape", "escape": "Escape", "enter": "Return", "return": "Return",
		"del": "Delete", "delete": "Delete", "ins": "Insert", "insert": "Insert",
		"pageup": "Prior", "pagedown": "Next", "space": "space", "tab": "Tab",
		"up": "Up", "down": "Down", "left": "Left", "right": "Right",
		"home": "Home", "end": "End", "backspace": "BackSpace", "printscreen": "Print",
	}
	var parts []string
	for p := range strings.SplitSeq(raw, "+") {
		k := strings.TrimSpace(strings.ToLower(p))
		if k == "" {
			continue
		}
		if mapped, ok := repl[k]; ok {
			parts = append(parts, mapped)
		} else {
			parts = append(parts, k)
		}
	}
	return strings.Join(parts, "+")
}

// ── MCP registration + routing ───────────────────────────────────────────────

func registerControllerTools(srv *mcp.Server, enabled map[string]bool, sess *rdpSession, proxy *agentProxy) error {
	specs, err := loadToolSpecs()
	if err != nil {
		return err
	}
	for _, spec := range specs {
		if !enabled[spec.Name] {
			continue
		}
		var schema jsonschema.Schema
		if err := json.Unmarshal(spec.InputSchema, &schema); err != nil {
			return fmt.Errorf("tool %s: %w", spec.Name, err)
		}
		resolved, err := schema.Resolve(nil)
		if err != nil {
			return fmt.Errorf("tool %s: %w", spec.Name, err)
		}
		tool := &mcp.Tool{
			Name: spec.Name, Description: spec.Description,
			InputSchema: &schema, Annotations: annotationsFor(spec.Name),
		}
		srv.AddTool(tool, routeTool(spec.Name, resolved, sess, proxy))
	}
	return nil
}

// routeTool sends desktop tools to the local xdotool/import handlers and every
// other tool to the in-session agent.
func routeTool(name string, schema *jsonschema.Resolved, sess *rdpSession, proxy *agentProxy) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args, err := decodeArguments(req.Params.Arguments)
		if err != nil {
			return errorResult("Invalid arguments: " + err.Error()), nil
		}
		if err := schema.Validate(map[string]any(args)); err != nil {
			return errorResult("Invalid arguments: " + err.Error()), nil
		}

		if desktopTools[name] {
			result, err := localDesktop(ctx, name, sess, proxy, args)
			if err != nil {
				return errorResult(name + " error: " + err.Error()), nil
			}
			return toCallResult(result), nil
		}

		if proxy == nil {
			return errorResult(name + " needs the in-session agent, which is disabled (-no-agent)."), nil
		}
		result, err := proxy.call(ctx, name, map[string]any(args))
		if err != nil {
			return errorResult(name + " error: " + err.Error()), nil
		}
		return toCallResult(result), nil
	}
}

func localDesktop(ctx context.Context, name string, sess *rdpSession, proxy *agentProxy, args arguments) (toolResult, error) {
	switch name {
	case "Snapshot":
		return sess.snapshot(ctx, args, proxy)
	case "Click":
		return sess.click(ctx, args)
	case "Type":
		return sess.typeText(ctx, args)
	case "Move":
		return sess.move(ctx, args)
	case "Scroll":
		return sess.scroll(ctx, args)
	case "Shortcut":
		return sess.shortcut(ctx, args)
	case "Wait":
		return waitTool2(ctx, args)
	}
	return toolResult{}, fmt.Errorf("no local handler for %s", name)
}

func controllerInstructions(noAgent bool) string {
	var b strings.Builder
	w := func(s string) { b.WriteString(s); b.WriteByte('\n') }
	w("# win-rdp-mcp (controller)")
	w("")
	w("Drives a Windows target over RDP. You are given only RDP credentials; the desktop is driven directly and the system agent is pushed into the session automatically.")
	w("")
	w("- Start with `Snapshot` to see the screen. Coordinates are screen pixels at the session resolution.")
	w("- `Click`, `Type`, `Move`, `Scroll`, `Shortcut` drive the desktop with no footprint on the target.")
	if !noAgent {
		w("- The window/element list, `Shell`, files, registry, services and the rest come from an agent pushed into the session over RDP. It may take a few seconds after startup before those answer.")
	} else {
		w("- Running with -no-agent: only the desktop tools above are available.")
	}
	return strings.TrimRight(b.String(), "\n")
}

// ── small helpers ────────────────────────────────────────────────────────────

func itoa(n int) string { return strconv.Itoa(n) }

// encodeSnapshot re-encodes the captured PNG as JPEG (smaller, fewer tokens),
// downscaling to maxWidth. If decoding or encoding fails it hands back the
// original PNG so a snapshot is never lost to a compression hiccup.
func encodeSnapshot(png []byte, maxWidth, quality int) ([]byte, string) {
	img, _, err := image.Decode(bytes.NewReader(png))
	if err != nil {
		return png, "image/png"
	}
	jpg, err := jpegBytes(img, quality, maxWidth)
	if err != nil {
		return png, "image/png"
	}
	return jpg, "image/jpeg"
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

func firstText(blocks []contentBlock) string {
	for _, b := range blocks {
		if b.Type == "text" {
			return b.Text
		}
	}
	return ""
}

// poll runs check every 250ms until it returns true or the deadline passes.
func poll(ctx context.Context, timeout time.Duration, check func() bool, failMsg string) error {
	deadline := time.Now().Add(timeout)
	for {
		if check() {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s", failMsg)
		}
		if !sleepCtx(ctx, 250*time.Millisecond) {
			return ctx.Err()
		}
	}
}

// sleepCtx sleeps for d, returning false if ctx is cancelled first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// allocDisplay finds an unused X display number.
func allocDisplay() (string, error) {
	for n := 140; n < 400; n++ {
		if !fileExists(fmt.Sprintf("/tmp/.X11-unix/X%d", n)) && !fileExists(fmt.Sprintf("/tmp/.X%d-lock", n)) {
			return ":" + strconv.Itoa(n), nil
		}
	}
	return "", fmt.Errorf("no free X display found")
}
