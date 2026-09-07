package main

import (
	"context"
	"fmt"
	"image"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// maxListedElements caps the interactive-element listing in a Snapshot. Past
// this the list stops helping a model choose and just burns context.
const maxListedElements = 50

// ── Snapshot ─────────────────────────────────────────────────────────────────

// snapshotTool captures the desktop plus the window list and the foreground
// window's controls. A capture failure is retried once behind a session
// reconnect, because the usual cause is an RDP client having disconnected and
// left the session with no console to draw to.
func snapshotTool(ctx context.Context, args arguments) (toolResult, error) {
	useVision := args.boolOr("use_vision", true)
	quality := args.intOr("quality", 75)
	maxWidth := args.intOr("max_width", 0)
	monitor := args.intOr("monitor", 0)

	var parts []contentBlock
	if useVision {
		img, _, err := captureWithReconnect(ctx, monitor)
		if err != nil {
			return toolResult{}, err
		}
		data, err := jpegBytes(img, quality, maxWidth)
		if err != nil {
			return toolResult{}, fmt.Errorf("encoding screenshot: %w", err)
		}
		parts = append(parts, contentBlock{Type: "image", Data: data, MIME: "image/jpeg"})
	}

	var b strings.Builder
	fmt.Fprintf(&b, "**System language:** %s\n\n**Windows:**\n", systemLanguage())
	windows, err := enumerateWindows()
	if err != nil {
		return toolResult{}, err
	}
	for _, w := range windows {
		fmt.Fprintf(&b, "  [%d] %s (%dx%d at %d,%d)\n",
			w.Handle, w.Title, w.Rect.Dx(), w.Rect.Dy(), w.Rect.Min.X, w.Rect.Min.Y)
	}

	// Element enumeration is best-effort: a foreground window that refuses to
	// enumerate should not cost the caller the screenshot and window list.
	if elements, err := interactiveElements(); err == nil && len(elements) > 0 {
		b.WriteString("\n**Interactive elements (foreground window):**\n")
		for _, el := range elements[:min(len(elements), maxListedElements)] {
			cx, cy := el.center()
			fmt.Fprintf(&b, "  [%d] %s — center (%d,%d)\n", el.Index, el.label(), cx, cy)
		}
	}

	parts = append(parts, contentBlock{Type: "text", Text: strings.TrimRight(b.String(), "\n")})
	return toolResult{Content: parts}, nil
}

// annotatedSnapshotTool is Snapshot with the controls boxed and numbered on the
// image itself.
func annotatedSnapshotTool(ctx context.Context, args arguments) (toolResult, error) {
	maxElements := args.intOr("max_elements", 30)
	quality := args.intOr("quality", 75)
	maxWidth := args.intOr("max_width", 0)

	img, region, err := captureWithReconnect(ctx, 0)
	if err != nil {
		return toolResult{}, err
	}

	elements, err := interactiveElements()
	if err != nil {
		return toolResult{}, err
	}
	if len(elements) == 0 {
		data, err := jpegBytes(img, quality, maxWidth)
		if err != nil {
			return toolResult{}, err
		}
		return imageResult(data, "image/jpeg", "No interactive elements found."), nil
	}
	elements = elements[:min(len(elements), maxElements)]

	// Boxes are drawn after scaling, so they stay aligned when max_width shrinks
	// the image.
	scaled := resizeToWidth(img, maxWidth)
	canvas, ok := scaled.(*image.RGBA)
	if !ok {
		canvas = image.NewRGBA(scaled.Bounds())
		copyInto(canvas, scaled)
	}
	scale := float64(canvas.Bounds().Dx()) / float64(img.Bounds().Dx())
	annotate(canvas, elements, region.Min, scale)

	data, err := jpegBytes(canvas, quality, 0)
	if err != nil {
		return toolResult{}, err
	}

	var b strings.Builder
	fmt.Fprintf(&b, "**Annotated %d elements:**\n", len(elements))
	for _, el := range elements {
		cx, cy := el.center()
		fmt.Fprintf(&b, "  [%d] %s — center (%d,%d)\n", el.Index, el.label(), cx, cy)
	}
	return imageResult(data, "image/jpeg", strings.TrimRight(b.String(), "\n")), nil
}

func copyInto(dst *image.RGBA, src image.Image) {
	b := src.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			dst.Set(x, y, src.At(x, y))
		}
	}
}

// captureWithReconnect grabs the screen, and on failure reconnects a
// disconnected session once before retrying.
func captureWithReconnect(ctx context.Context, monitor int) (*image.RGBA, image.Rectangle, error) {
	img, region, err := captureScreen(monitor)
	if err == nil {
		return img, region, nil
	}
	if reconnectErr := ensureSessionConnected(ctx, false); reconnectErr != nil {
		// The session was not the problem (or could not be fixed), so report
		// the original capture failure rather than the reconnect's.
		return nil, image.Rectangle{}, err
	}
	img, region, retryErr := captureScreen(monitor)
	if retryErr != nil {
		return nil, image.Rectangle{}, fmt.Errorf("after session reconnect: %w", retryErr)
	}
	return img, region, nil
}

// ── Input ────────────────────────────────────────────────────────────────────

func clickTool(_ context.Context, args arguments) (toolResult, error) {
	msg, err := mouseClick(args.intOr("x", 0), args.intOr("y", 0),
		args.stringOr("button", "left"), args.stringOr("action", "click"))
	if err != nil {
		return toolResult{}, err
	}
	return textResult("%s", msg), nil
}

func typeTool(_ context.Context, args arguments) (toolResult, error) {
	text := args.stringOr("text", "")
	if x, y := args.intOr("x", 0), args.intOr("y", 0); x != 0 || y != 0 {
		if _, err := mouseClick(x, y, "left", "click"); err != nil {
			return toolResult{}, err
		}
		time.Sleep(100 * time.Millisecond)
	}
	if args.boolOr("clear", false) {
		if err := pressKeys([]string{"ctrl", "a"}); err != nil {
			return toolResult{}, err
		}
		if err := pressKeys([]string{"delete"}); err != nil {
			return toolResult{}, err
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := typeText(text); err != nil {
		return toolResult{}, err
	}
	if args.boolOr("press_enter", false) {
		if err := pressKeys([]string{"enter"}); err != nil {
			return toolResult{}, err
		}
	}
	return textResult("Typed %d characters", len([]rune(text))), nil
}

func scrollTool(_ context.Context, args arguments) (toolResult, error) {
	amount := args.intOr("amount", 0)
	horizontal := args.boolOr("horizontal", false)
	if err := mouseScroll(amount, args.intOr("x", 0), args.intOr("y", 0), horizontal); err != nil {
		return toolResult{}, err
	}
	direction := "vertically"
	if horizontal {
		direction = "horizontally"
	}
	return textResult("Scrolled %d %s", amount, direction), nil
}

func moveTool(_ context.Context, args arguments) (toolResult, error) {
	x, y := args.intOr("x", 0), args.intOr("y", 0)
	duration := time.Duration(args.floatOr("duration", 0.3) * float64(time.Second))

	if args.boolOr("drag", false) {
		if err := mouseDrag(args.intOr("start_x", 0), args.intOr("start_y", 0), x, y, duration); err != nil {
			return toolResult{}, err
		}
		return textResult("Dragged to (%d,%d)", x, y), nil
	}
	if err := mouseMove(x, y, duration); err != nil {
		return toolResult{}, err
	}
	return textResult("Moved to (%d,%d)", x, y), nil
}

func shortcutTool(_ context.Context, args arguments) (toolResult, error) {
	raw := args.stringOr("keys", "")
	var keys []string
	for _, part := range strings.Split(raw, "+") {
		if k := strings.TrimSpace(part); k != "" {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return toolResult{}, fmt.Errorf("no keys given")
	}
	if err := pressKeys(keys); err != nil {
		return toolResult{}, err
	}
	return textResult("Executed shortcut: %s", raw), nil
}

// maxWait bounds the Wait tool. Longer sleeps belong to the client's own
// scheduling, not to a call holding the exclusive desktop lock.
const maxWait = 60 * time.Second

func waitTool(ctx context.Context, args arguments) (toolResult, error) {
	seconds := args.floatOr("seconds", 1.0)
	d := min(max(time.Duration(seconds*float64(time.Second)), 0), maxWait)

	select {
	case <-time.After(d):
		return textResult("Waited %s", d), nil
	case <-ctx.Done():
		return toolResult{}, ctx.Err()
	}
}

// ── Windows and apps ─────────────────────────────────────────────────────────

func focusWindowTool(_ context.Context, args arguments) (toolResult, error) {
	msg, err := focusWindow(args.stringOr("title", ""), args.int64Or("handle", 0))
	if err != nil {
		return toolResult{}, err
	}
	return textResult("%s", msg), nil
}

func minimizeAllTool(_ context.Context, _ arguments) (toolResult, error) {
	if err := pressKeys([]string{"win", "d"}); err != nil {
		return toolResult{}, err
	}
	return textResult("Minimized all windows"), nil
}

func appTool(ctx context.Context, args arguments) (toolResult, error) {
	name := args.stringOr("name", "")
	handle := args.int64Or("handle", 0)

	switch action := args.stringOr("action", "launch"); action {
	case "launch":
		if name == "" {
			return toolResult{}, fmt.Errorf("launch requires a name")
		}
		cmd := "Start-Process " + psQuote(name)
		if a := args.stringOr("args", ""); a != "" {
			cmd += " -ArgumentList " + psQuote(a)
		}
		if _, err := runPowerShell(ctx, cmd, 10*time.Second); err != nil {
			return toolResult{}, fmt.Errorf("launching %s: %w", name, err)
		}
		return textResult("Launched %s", name), nil

	case "switch":
		msg, err := focusWindow(name, handle)
		if err != nil {
			return toolResult{}, err
		}
		return textResult("%s", msg), nil

	case "resize":
		if handle == 0 {
			return toolResult{}, fmt.Errorf("resize requires a window handle")
		}
		msg, err := resizeWindow(handle, args.intOr("width", 0), args.intOr("height", 0))
		if err != nil {
			return toolResult{}, err
		}
		return textResult("%s", msg), nil

	default:
		return toolResult{}, fmt.Errorf("unknown action %q (use launch, switch or resize)", action)
	}
}

// ── Clipboard, lock, notification ────────────────────────────────────────────

func getClipboardTool(_ context.Context, _ arguments) (toolResult, error) {
	text, err := getClipboard()
	if err != nil {
		return toolResult{}, err
	}
	if text == "" {
		return textResult("(clipboard is empty, or holds no text)"), nil
	}
	return textResult("%s", text), nil
}

func setClipboardTool(_ context.Context, args arguments) (toolResult, error) {
	if err := setClipboard(args.stringOr("text", "")); err != nil {
		return toolResult{}, err
	}
	return textResult("Clipboard set"), nil
}

func lockScreenTool(_ context.Context, _ arguments) (toolResult, error) {
	if err := lockWorkstation(); err != nil {
		return toolResult{}, err
	}
	return textResult("Screen locked"), nil
}

func notificationTool(ctx context.Context, args arguments) (toolResult, error) {
	title := args.stringOr("title", "win-rdp-mcp")
	message := args.stringOr("message", "")

	// The toast XML is built in PowerShell from here, so both fields are XML
	// escaped before they reach it.
	script := fmt.Sprintf(`
[Windows.UI.Notifications.ToastNotificationManager, Windows.UI.Notifications, ContentType = WindowsRuntime] | Out-Null
[Windows.Data.Xml.Dom.XmlDocument, Windows.Data.Xml.Dom.XmlDocument, ContentType = WindowsRuntime] | Out-Null
$xml = New-Object Windows.Data.Xml.Dom.XmlDocument
$xml.LoadXml(%s)
$toast = [Windows.UI.Notifications.ToastNotification]::new($xml)
[Windows.UI.Notifications.ToastNotificationManager]::CreateToastNotifier("win-rdp-mcp").Show($toast)`,
		psQuote(fmt.Sprintf(
			`<toast><visual><binding template="ToastGeneric"><text>%s</text><text>%s</text></binding></visual></toast>`,
			xmlEscape(title), xmlEscape(message))))

	if _, err := runPowerShell(ctx, script, 15*time.Second); err != nil {
		return toolResult{}, err
	}
	return textResult("Notification shown"), nil
}

func xmlEscape(s string) string {
	return strings.NewReplacer(
		"&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;",
	).Replace(s)
}

// ── Session ──────────────────────────────────────────────────────────────────

func reconnectSessionTool(ctx context.Context, args arguments) (toolResult, error) {
	if err := ensureSessionConnected(ctx, args.boolOr("force", false)); err != nil {
		return toolResult{}, err
	}
	return textResult("Session connected to console"), nil
}

// ensureSessionConnected attaches the interactive session to the console when
// it is disconnected. Without a console the desktop has nothing to render to,
// so screenshots and UI automation fail — which is exactly the state an RDP
// client leaves behind when it closes.
func ensureSessionConnected(ctx context.Context, force bool) error {
	out, err := runCommand(ctx, 10*time.Second, "query", "session")
	if err != nil {
		return fmt.Errorf("querying sessions: %w", err)
	}

	sessionID, disconnected, ok := parseUserSession(out)
	if !ok {
		return fmt.Errorf("no user session found")
	}
	if !disconnected && !force {
		return nil
	}
	if _, err := runCommand(ctx, 10*time.Second, "tscon", sessionID, "/dest:console"); err != nil {
		return fmt.Errorf("tscon %s /dest:console: %w", sessionID, err)
	}
	// tscon returns before the session is drawable; a screenshot fired straight
	// after still comes back black.
	time.Sleep(time.Second)
	return nil
}

// disconnectedStates covers the localised spellings `query session` prints for
// a disconnected session — the tool has to work on a non-English Windows too.
var disconnectedStates = map[string]bool{
	"disc": true, "disconnected": true, "断开": true, "已断开": true,
}

// parseUserSession picks the interactive user's session out of `query session`
// output and reports whether it is disconnected.
//
// The listing is fixed-width, and the SESSIONNAME column is empty for a
// disconnected session — so a row can be "console alice 1 Active" or just
// "alice 2 Disc", and splitting on whitespace alone cannot tell a lone
// username from a lone session name. The indentation does: a session name
// starts at the left margin, a username never does.
func parseUserSession(out string) (id string, disconnected bool, found bool) {
	lines := strings.Split(out, "\n")
	if len(lines) < 2 {
		return "", false, false
	}

	for _, raw := range lines[1:] {
		// The leading '>' marks the current session and shifts nothing else.
		line := strings.TrimPrefix(strings.TrimRight(raw, " \t\r"), ">")
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}

		idIndex := slices.IndexFunc(fields, isDigits)
		if idIndex < 0 || idIndex+1 >= len(fields) {
			continue
		}

		var sessionName string
		switch idIndex {
		case 1:
			// One field before the ID: either a session name at the margin, or
			// an indented username on a row whose session name is blank.
			if indentOf(line, fields[0]) <= sessionNameColumn {
				sessionName = fields[0]
			}
		case 2:
			sessionName = fields[0]
		default:
			continue
		}

		// A row with no user is a listener or the services session, not
		// somebody's desktop.
		hasUser := idIndex == 2 || (idIndex == 1 && sessionName == "")
		if !hasUser {
			continue
		}
		switch strings.ToLower(sessionName) {
		case "services", "rdp-tcp":
			continue
		}
		return fields[idIndex], disconnectedStates[strings.ToLower(fields[idIndex+1])], true
	}
	return "", false, false
}

// sessionNameColumn is how far the SESSIONNAME column can be indented before a
// token must instead belong to USERNAME. `query session` prints one leading
// space; the username column sits far to the right of it.
const sessionNameColumn = 2

func indentOf(line, token string) int {
	return strings.Index(line, token)
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// ── OCR ──────────────────────────────────────────────────────────────────────

func ocrTool(ctx context.Context, args arguments) (toolResult, error) {
	img, err := captureRegionArgs(ctx, args)
	if err != nil {
		return toolResult{}, err
	}
	png, err := pngBytes(img)
	if err != nil {
		return toolResult{}, err
	}

	tmp, err := os.CreateTemp("", "win-rdp-mcp-ocr-*.png")
	if err != nil {
		return toolResult{}, err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(png); err != nil {
		tmp.Close()
		return toolResult{}, err
	}
	tmp.Close()

	text, err := runOCR(ctx, tmp.Name(), args.stringOr("lang", "eng"))
	if err != nil {
		return toolResult{}, err
	}
	if strings.TrimSpace(text) == "" {
		return textResult("(no text detected)"), nil
	}
	return textResult("%s", strings.TrimSpace(text)), nil
}

// runOCR prefers tesseract when it is on PATH, and otherwise drives the OCR
// engine that ships with Windows. Neither is a hard dependency of the server.
func runOCR(ctx context.Context, imagePath, lang string) (string, error) {
	if bin, err := exec.LookPath("tesseract"); err == nil {
		out, err := runCommand(ctx, 60*time.Second, bin, imagePath, "stdout", "-l", lang)
		if err == nil {
			return out, nil
		}
		// Fall through to the built-in engine: a missing language pack is the
		// common failure, and Windows OCR needs no packs.
	}
	return windowsBuiltinOCR(ctx, imagePath)
}

func windowsBuiltinOCR(ctx context.Context, imagePath string) (string, error) {
	script := fmt.Sprintf(`
Add-Type -AssemblyName System.Runtime.WindowsRuntime
$null = [Windows.Media.Ocr.OcrEngine, Windows.Foundation, ContentType = WindowsRuntime]
$null = [Windows.Graphics.Imaging.BitmapDecoder, Windows.Foundation, ContentType = WindowsRuntime]
Add-Type -TypeDefinition @'
using System;
using System.Threading.Tasks;
public static class AsyncHelper {
    public static T Await<T>(Windows.Foundation.IAsyncOperation<T> op) {
        return Task.Run(() => {
            while (op.Status == Windows.Foundation.AsyncStatus.Started) { System.Threading.Thread.Sleep(10); }
            return op.GetResults();
        }).Result;
    }
}
'@ -ReferencedAssemblies "C:\Windows\Microsoft.NET\Framework64\v4.0.30319\System.Runtime.WindowsRuntime.dll"
$stream = [System.IO.File]::OpenRead(%s)
try {
    $ras = [System.IO.WindowsRuntimeStreamExtensions]::AsRandomAccessStream($stream)
    $decoder = [AsyncHelper]::Await([Windows.Graphics.Imaging.BitmapDecoder]::CreateAsync($ras))
    $bitmap = [AsyncHelper]::Await($decoder.GetSoftwareBitmapAsync())
    $engine = [Windows.Media.Ocr.OcrEngine]::TryCreateFromUserProfileLanguages()
    if ($null -eq $engine) { throw "No OCR language pack is installed for this user profile." }
    Write-Output ([AsyncHelper]::Await($engine.RecognizeAsync($bitmap))).Text
} finally { $stream.Close() }`, psQuote(imagePath))

	out, err := runPowerShell(ctx, script, 60*time.Second)
	if err != nil {
		return "", fmt.Errorf("no usable OCR engine (install tesseract, or a Windows OCR language pack): %w", err)
	}
	return out, nil
}

// ── Screen recording ─────────────────────────────────────────────────────────

func screenRecordTool(ctx context.Context, args arguments) (toolResult, error) {
	// Bounded hard: this holds the exclusive desktop lock for its whole run and
	// every frame is a full screen grab.
	duration := min(max(args.floatOr("duration", 3.0), 0.5), 10.0)
	fps := min(max(args.intOr("fps", 5), 1), 10)
	maxWidth := args.intOr("max_width", 800)

	region, hasRegion := regionFromArgs(args)
	interval := time.Second / time.Duration(fps)
	frameCount := max(int(duration*float64(fps)), 1)

	frames := make([]image.Image, 0, frameCount)
	start := time.Now()
	for i := range frameCount {
		// Schedule against the start time, not the previous frame, so capture
		// cost does not make the GIF drift slower than the requested fps.
		if target := start.Add(time.Duration(i) * interval); time.Now().Before(target) {
			select {
			case <-time.After(time.Until(target)):
			case <-ctx.Done():
				return toolResult{}, ctx.Err()
			}
		}
		var (
			img *image.RGBA
			err error
		)
		if hasRegion {
			img, err = captureRect(region)
		} else {
			img, _, err = captureScreen(0)
		}
		if err != nil {
			return toolResult{}, err
		}
		frames = append(frames, img)
	}

	data, err := gifBytes(frames, interval, maxWidth)
	if err != nil {
		return toolResult{}, err
	}
	sizeKB := len(data) / 1024
	return imageResult(data, "image/gif",
		fmt.Sprintf("Recorded %.1fs at %dfps (%d frames, %dKB GIF)", duration, fps, len(frames), sizeKB)), nil
}

// regionFromArgs reads the left/top/right/bottom rectangle. All-zero means "no
// region", i.e. the whole screen.
func regionFromArgs(args arguments) (image.Rectangle, bool) {
	l, t := args.intOr("left", 0), args.intOr("top", 0)
	r, b := args.intOr("right", 0), args.intOr("bottom", 0)
	if l == 0 && t == 0 && r == 0 && b == 0 {
		return image.Rectangle{}, false
	}
	return image.Rect(l, t, r, b), true
}

func captureRegionArgs(ctx context.Context, args arguments) (*image.RGBA, error) {
	if region, ok := regionFromArgs(args); ok {
		return captureRect(region)
	}
	img, _, err := captureWithReconnect(ctx, 0)
	return img, err
}

// ── Shared process helpers ───────────────────────────────────────────────────

// psQuote wraps a value as a PowerShell single-quoted string. Single quotes
// suppress every form of expansion, so doubling the quote character is the
// whole escape — that is what keeps a filename or window title from becoming
// executable script.
func psQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// runPowerShell runs one command through PowerShell and returns its combined
// output. A non-zero exit becomes an error carrying that output, since
// PowerShell reports most failures on stderr with a zero-length stdout.
func runPowerShell(ctx context.Context, command string, timeout time.Duration) (string, error) {
	return runCommand(ctx, timeout, "powershell", "-NoProfile", "-NonInteractive", "-Command", command)
}

func runCommand(ctx context.Context, timeout time.Duration, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))

	if ctx.Err() == context.DeadlineExceeded {
		return text, fmt.Errorf("%s timed out after %s", filepath.Base(name), timeout)
	}
	if err != nil {
		if text != "" {
			return text, fmt.Errorf("%s: %w: %s", filepath.Base(name), err, text)
		}
		return text, fmt.Errorf("%s: %w", filepath.Base(name), err)
	}
	return text, nil
}
