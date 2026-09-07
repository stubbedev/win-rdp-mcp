//go:build windows

package main

import (
	"errors"
	"fmt"
	"image"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	gdi32    = windows.NewLazySystemDLL("gdi32.dll")
	shcore   = windows.NewLazySystemDLL("shcore.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")

	procGetDC                     = user32.NewProc("GetDC")
	procReleaseDC                 = user32.NewProc("ReleaseDC")
	procGetSystemMetrics          = user32.NewProc("GetSystemMetrics")
	procEnumDisplayMonitors       = user32.NewProc("EnumDisplayMonitors")
	procGetMonitorInfoW           = user32.NewProc("GetMonitorInfoW")
	procEnumWindows               = user32.NewProc("EnumWindows")
	procEnumChildWindows          = user32.NewProc("EnumChildWindows")
	procIsWindowVisible           = user32.NewProc("IsWindowVisible")
	procGetWindowTextW            = user32.NewProc("GetWindowTextW")
	procGetWindowTextLengthW      = user32.NewProc("GetWindowTextLengthW")
	procGetClassNameW             = user32.NewProc("GetClassNameW")
	procGetWindowRect             = user32.NewProc("GetWindowRect")
	procGetWindowThreadProcessId  = user32.NewProc("GetWindowThreadProcessId")
	procGetForegroundWindow       = user32.NewProc("GetForegroundWindow")
	procSetForegroundWindow       = user32.NewProc("SetForegroundWindow")
	procShowWindow                = user32.NewProc("ShowWindow")
	procIsIconic                  = user32.NewProc("IsIconic")
	procMoveWindow                = user32.NewProc("MoveWindow")
	procSendInput                 = user32.NewProc("SendInput")
	procGetCursorPos              = user32.NewProc("GetCursorPos")
	procOpenClipboard             = user32.NewProc("OpenClipboard")
	procCloseClipboard            = user32.NewProc("CloseClipboard")
	procEmptyClipboard            = user32.NewProc("EmptyClipboard")
	procGetClipboardData          = user32.NewProc("GetClipboardData")
	procSetClipboardData          = user32.NewProc("SetClipboardData")
	procLockWorkStation           = user32.NewProc("LockWorkStation")
	procSetProcessDPIAware        = user32.NewProc("SetProcessDPIAware")
	procSetProcessDpiAwarenessCtx = user32.NewProc("SetProcessDpiAwarenessContext")
	procSetProcessDpiAwareness    = shcore.NewProc("SetProcessDpiAwareness")
	procGetUserDefaultLocaleName  = kernel32.NewProc("GetUserDefaultLocaleName")
	procGlobalAlloc               = kernel32.NewProc("GlobalAlloc")
	procGlobalFree                = kernel32.NewProc("GlobalFree")
	procGlobalLock                = kernel32.NewProc("GlobalLock")
	procGlobalUnlock              = kernel32.NewProc("GlobalUnlock")

	procCreateCompatibleDC     = gdi32.NewProc("CreateCompatibleDC")
	procCreateCompatibleBitmap = gdi32.NewProc("CreateCompatibleBitmap")
	procSelectObject           = gdi32.NewProc("SelectObject")
	procBitBlt                 = gdi32.NewProc("BitBlt")
	procGetDIBits              = gdi32.NewProc("GetDIBits")
	procDeleteObject           = gdi32.NewProc("DeleteObject")
	procDeleteDC               = gdi32.NewProc("DeleteDC")
)

const (
	smXVirtualScreen  = 76
	smYVirtualScreen  = 77
	smCXVirtualScreen = 78
	smCYVirtualScreen = 79

	srcCopy      = 0x00CC0020
	captureBlt   = 0x40000000
	dibRGBColors = 0

	cfUnicodeText = 13
	gmemMoveable  = 0x0002

	swRestore = 9

	inputMouse    = 0
	inputKeyboard = 1

	mouseEventMove        = 0x0001
	mouseEventLeftDown    = 0x0002
	mouseEventLeftUp      = 0x0004
	mouseEventRightDown   = 0x0008
	mouseEventRightUp     = 0x0010
	mouseEventMiddleDown  = 0x0020
	mouseEventMiddleUp    = 0x0040
	mouseEventWheel       = 0x0800
	mouseEventHWheel      = 0x1000
	mouseEventVirtualDesk = 0x4000
	mouseEventAbsolute    = 0x8000

	keyEventExtended = 0x0001
	keyEventKeyUp    = 0x0002
	keyEventUnicode  = 0x0004

	wheelDelta = 120
)

// ── DPI ──────────────────────────────────────────────────────────────────────

var initPlatform = sync.OnceFunc(func() {
	// Per-monitor v2 keeps screenshots at the panel's real pixel count on a
	// scaled 4K display, and keeps click coordinates in that same space. The
	// two older calls are the fallbacks for pre-1703 and pre-8.1 hosts.
	const dpiAwarePerMonitorV2 = ^uintptr(3) // (DPI_AWARENESS_CONTEXT)-4
	if r, _, _ := procSetProcessDpiAwarenessCtx.Call(dpiAwarePerMonitorV2); r != 0 {
		return
	}
	if r, _, _ := procSetProcessDpiAwareness.Call(2); r == 0 {
		return
	}
	procSetProcessDPIAware.Call()
})

// ── Screen capture ───────────────────────────────────────────────────────────

type rect struct{ Left, Top, Right, Bottom int32 }

func (r rect) toImage() image.Rectangle {
	return image.Rect(int(r.Left), int(r.Top), int(r.Right), int(r.Bottom))
}

type bitmapInfoHeader struct {
	Size          uint32
	Width, Height int32
	Planes        uint16
	BitCount      uint16
	Compression   uint32
	SizeImage     uint32
	XPelsPerMeter int32
	YPelsPerMeter int32
	ClrUsed       uint32
	ClrImportant  uint32
}

type monitorInfo struct {
	Size    uint32
	Monitor rect
	Work    rect
	Flags   uint32
}

func getSystemMetric(index int) int {
	r, _, _ := procGetSystemMetrics.Call(uintptr(index))
	return int(int32(r))
}

// virtualScreenRect is the bounding box of every monitor, in the coordinate
// space that mouse and window positions use.
func virtualScreenRect() image.Rectangle {
	x := getSystemMetric(smXVirtualScreen)
	y := getSystemMetric(smYVirtualScreen)
	return image.Rect(x, y, x+getSystemMetric(smCXVirtualScreen), y+getSystemMetric(smCYVirtualScreen))
}

// listMonitors returns each monitor's rectangle in virtual-screen coordinates,
// in the order Windows enumerates them (so "monitor 1" is stable per boot).
func listMonitors() []image.Rectangle {
	var out []image.Rectangle
	cb := syscall.NewCallback(func(hMonitor, _ uintptr, lprc *rect, _ uintptr) uintptr {
		mi := monitorInfo{Size: uint32(unsafe.Sizeof(monitorInfo{}))}
		if r, _, _ := procGetMonitorInfoW.Call(hMonitor, uintptr(unsafe.Pointer(&mi))); r != 0 {
			out = append(out, mi.Monitor.toImage())
		} else {
			out = append(out, lprc.toImage())
		}
		return 1
	})
	procEnumDisplayMonitors.Call(0, 0, cb, 0)
	return out
}

// captureRect grabs a region of the virtual screen as RGBA. monitor 0 in
// captureScreen means "everything"; a region is given in the same coordinates.
func captureRect(r image.Rectangle) (*image.RGBA, error) {
	initPlatform()
	w, h := r.Dx(), r.Dy()
	if w <= 0 || h <= 0 {
		return nil, fmt.Errorf("capture region is empty: %v", r)
	}

	screenDC, _, err := procGetDC.Call(0)
	if screenDC == 0 {
		return nil, fmt.Errorf("GetDC failed: %w", err)
	}
	defer procReleaseDC.Call(0, screenDC)

	memDC, _, err := procCreateCompatibleDC.Call(screenDC)
	if memDC == 0 {
		return nil, fmt.Errorf("CreateCompatibleDC failed: %w", err)
	}
	defer procDeleteDC.Call(memDC)

	bitmap, _, err := procCreateCompatibleBitmap.Call(screenDC, uintptr(w), uintptr(h))
	if bitmap == 0 {
		return nil, fmt.Errorf("CreateCompatibleBitmap failed: %w", err)
	}
	defer procDeleteObject.Call(bitmap)

	old, _, _ := procSelectObject.Call(memDC, bitmap)
	defer procSelectObject.Call(memDC, old)

	// CAPTUREBLT includes layered windows (tooltips, some menus) that a plain
	// SRCCOPY silently drops.
	ok, _, err := procBitBlt.Call(memDC, 0, 0, uintptr(w), uintptr(h),
		screenDC, uintptr(int32(r.Min.X)), uintptr(int32(r.Min.Y)), srcCopy|captureBlt)
	if ok == 0 {
		return nil, fmt.Errorf("BitBlt failed: %w", err)
	}

	// A negative height asks GDI for a top-down buffer, matching image.RGBA's
	// row order.
	bi := bitmapInfoHeader{
		Size: uint32(unsafe.Sizeof(bitmapInfoHeader{})), Width: int32(w), Height: int32(-h),
		Planes: 1, BitCount: 32, Compression: 0,
	}
	buf := make([]byte, w*h*4)
	lines, _, err := procGetDIBits.Call(memDC, bitmap, 0, uintptr(h),
		uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&bi)), dibRGBColors)
	if lines == 0 {
		return nil, fmt.Errorf("GetDIBits failed: %w", err)
	}

	img := image.NewRGBA(image.Rect(0, 0, w, h))
	// GDI hands back BGRA with an unused alpha byte; swap to RGBA and force the
	// pixels opaque so JPEG/GIF encoding does not see a fully transparent image.
	for i := 0; i < len(buf); i += 4 {
		img.Pix[i+0] = buf[i+2]
		img.Pix[i+1] = buf[i+1]
		img.Pix[i+2] = buf[i+0]
		img.Pix[i+3] = 0xFF
	}
	return img, nil
}

// captureScreen grabs monitor n (1-based), or the whole virtual screen for 0.
// The returned rectangle is the captured region in virtual-screen coordinates,
// which is what maps a window or control rect onto a pixel in the image.
func captureScreen(monitor int) (*image.RGBA, image.Rectangle, error) {
	initPlatform()
	region := virtualScreenRect()
	if monitor > 0 {
		monitors := listMonitors()
		if monitor > len(monitors) {
			return nil, image.Rectangle{}, fmt.Errorf("monitor %d not found (have %d)", monitor, len(monitors))
		}
		region = monitors[monitor-1]
	}
	img, err := captureRect(region)
	return img, region, err
}

// ── Window enumeration ───────────────────────────────────────────────────────

func windowText(hwnd uintptr) string {
	n, _, _ := procGetWindowTextLengthW.Call(hwnd)
	if n == 0 {
		return ""
	}
	buf := make([]uint16, n+1)
	got, _, _ := procGetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	return windows.UTF16ToString(buf[:got])
}

func windowClass(hwnd uintptr) string {
	buf := make([]uint16, 256)
	got, _, _ := procGetClassNameW.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	return windows.UTF16ToString(buf[:got])
}

func windowRect(hwnd uintptr) (image.Rectangle, bool) {
	var r rect
	ok, _, _ := procGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&r)))
	return r.toImage(), ok != 0
}

func isVisible(hwnd uintptr) bool {
	r, _, _ := procIsWindowVisible.Call(hwnd)
	return r != 0
}

func enumerateWindows() ([]windowInfo, error) {
	initPlatform()
	var out []windowInfo
	cb := syscall.NewCallback(func(hwnd, _ uintptr) uintptr {
		// Untitled and hidden windows are noise for an agent choosing a target.
		if !isVisible(hwnd) {
			return 1
		}
		title := windowText(hwnd)
		if title == "" {
			return 1
		}
		r, ok := windowRect(hwnd)
		if !ok {
			return 1
		}
		var pid uint32
		procGetWindowThreadProcessId.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
		out = append(out, windowInfo{Handle: int64(hwnd), Title: title, Rect: r, PID: pid})
		return 1
	})
	if r, _, err := procEnumWindows.Call(cb, 0); r == 0 && len(out) == 0 {
		return nil, fmt.Errorf("EnumWindows failed: %w", err)
	}
	return out, nil
}

func interactiveElements() ([]uiElement, error) {
	initPlatform()
	fg, _, _ := procGetForegroundWindow.Call()
	if fg == 0 {
		return nil, nil
	}
	var out []uiElement
	cb := syscall.NewCallback(func(hwnd, _ uintptr) uintptr {
		if !isVisible(hwnd) {
			return 1
		}
		r, ok := windowRect(hwnd)
		if !ok {
			return 1
		}
		out = append(out, uiElement{
			Index: len(out) + 1,
			Class: windowClass(hwnd),
			Text:  windowText(hwnd),
			Rect:  r,
		})
		return 1
	})
	procEnumChildWindows.Call(fg, cb, 0)
	return out, nil
}

func focusWindow(title string, handle int64) (string, error) {
	initPlatform()
	hwnd := uintptr(handle)
	if hwnd == 0 {
		if title == "" {
			return "", errors.New("provide a window title or handle")
		}
		windows, err := enumerateWindows()
		if err != nil {
			return "", err
		}
		best := 0
		for _, w := range windows {
			if score := partialRatio(strings.ToLower(title), strings.ToLower(w.Title)); score > best {
				best, hwnd = score, uintptr(w.Handle)
			}
		}
		if best < 50 {
			return "", fmt.Errorf("no window matching %q (best score %d)", title, best)
		}
	}

	if iconic, _, _ := procIsIconic.Call(hwnd); iconic != 0 {
		procShowWindow.Call(hwnd, swRestore)
	}
	if ok, _, err := procSetForegroundWindow.Call(hwnd); ok == 0 {
		return "", fmt.Errorf("SetForegroundWindow failed: %w", err)
	}
	return fmt.Sprintf("Focused window handle=%d title=%q", hwnd, windowText(hwnd)), nil
}

func resizeWindow(handle int64, width, height int) (string, error) {
	initPlatform()
	hwnd := uintptr(handle)
	r, ok := windowRect(hwnd)
	if !ok {
		return "", fmt.Errorf("no window with handle %d", handle)
	}
	if ok, _, err := procMoveWindow.Call(hwnd, uintptr(int32(r.Min.X)), uintptr(int32(r.Min.Y)),
		uintptr(int32(width)), uintptr(int32(height)), 1); ok == 0 {
		return "", fmt.Errorf("MoveWindow failed: %w", err)
	}
	return fmt.Sprintf("Resized %d to %dx%d", handle, width, height), nil
}

// ── Synthetic input ──────────────────────────────────────────────────────────

type mouseInput struct {
	dx, dy      int32
	mouseData   uint32
	dwFlags     uint32
	time        uint32
	dwExtraInfo uintptr
}

type keybdInput struct {
	wVk, wScan  uint16
	dwFlags     uint32
	time        uint32
	dwExtraInfo uintptr
}

// winInput mirrors the Win32 INPUT union. keybdInput is smaller than
// mouseInput, so the mouse member doubles as the union's storage.
type winInput struct {
	typ uint32
	_   uint32
	mi  mouseInput
}

func (in *winInput) keyboard() *keybdInput {
	return (*keybdInput)(unsafe.Pointer(&in.mi))
}

func sendInputs(inputs []winInput) error {
	if len(inputs) == 0 {
		return nil
	}
	sent, _, err := procSendInput.Call(uintptr(len(inputs)),
		uintptr(unsafe.Pointer(&inputs[0])), unsafe.Sizeof(winInput{}))
	if int(sent) != len(inputs) {
		return fmt.Errorf("SendInput sent %d of %d events: %w", sent, len(inputs), err)
	}
	return nil
}

// absoluteMouseMove builds a move event in the 0..65535 normalized space
// SendInput expects, mapped over the whole virtual desktop so multi-monitor
// coordinates land on the right screen.
func absoluteMouseMove(x, y int) winInput {
	v := virtualScreenRect()
	nx := int32(0)
	ny := int32(0)
	if v.Dx() > 1 {
		nx = int32(float64(x-v.Min.X) * 65535 / float64(v.Dx()-1))
	}
	if v.Dy() > 1 {
		ny = int32(float64(y-v.Min.Y) * 65535 / float64(v.Dy()-1))
	}
	return winInput{typ: inputMouse, mi: mouseInput{
		dx: nx, dy: ny, dwFlags: mouseEventMove | mouseEventAbsolute | mouseEventVirtualDesk,
	}}
}

func buttonFlags(button string) (down, up uint32, err error) {
	switch strings.ToLower(button) {
	case "", "left":
		return mouseEventLeftDown, mouseEventLeftUp, nil
	case "right":
		return mouseEventRightDown, mouseEventRightUp, nil
	case "middle":
		return mouseEventMiddleDown, mouseEventMiddleUp, nil
	}
	return 0, 0, fmt.Errorf("unknown mouse button %q (use left, right or middle)", button)
}

func cursorPos() (int, int) {
	var pt struct{ X, Y int32 }
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	return int(pt.X), int(pt.Y)
}

func mouseClick(x, y int, button, action string) (string, error) {
	initPlatform()
	down, up, err := buttonFlags(button)
	if err != nil {
		return "", err
	}
	press := func(flag uint32) winInput {
		return winInput{typ: inputMouse, mi: mouseInput{dwFlags: flag}}
	}

	switch strings.ToLower(action) {
	case "hover":
		if err := sendInputs([]winInput{absoluteMouseMove(x, y)}); err != nil {
			return "", err
		}
		return fmt.Sprintf("Hovered at (%d,%d)", x, y), nil
	case "double":
		if err := sendInputs([]winInput{
			absoluteMouseMove(x, y), press(down), press(up), press(down), press(up),
		}); err != nil {
			return "", err
		}
		return fmt.Sprintf("Double-clicked %s at (%d,%d)", button, x, y), nil
	case "", "click":
		if err := sendInputs([]winInput{absoluteMouseMove(x, y), press(down), press(up)}); err != nil {
			return "", err
		}
		return fmt.Sprintf("Clicked %s at (%d,%d)", button, x, y), nil
	}
	return "", fmt.Errorf("unknown action %q (use click, double or hover)", action)
}

// mouseMove walks the pointer to the target over duration. Apps that track
// hover or drag gestures need the intermediate positions, not a teleport.
func mouseMove(x, y int, duration time.Duration) error {
	initPlatform()
	const stepInterval = 10 * time.Millisecond
	steps := int(duration / stepInterval)
	if steps < 1 {
		return sendInputs([]winInput{absoluteMouseMove(x, y)})
	}

	startX, startY := cursorPos()
	for step := 1; step <= steps; step++ {
		frac := float64(step) / float64(steps)
		ix := startX + int(float64(x-startX)*frac)
		iy := startY + int(float64(y-startY)*frac)
		if err := sendInputs([]winInput{absoluteMouseMove(ix, iy)}); err != nil {
			return err
		}
		time.Sleep(stepInterval)
	}
	return nil
}

func mouseDrag(startX, startY, x, y int, duration time.Duration) error {
	initPlatform()
	if startX != 0 || startY != 0 {
		if err := sendInputs([]winInput{absoluteMouseMove(startX, startY)}); err != nil {
			return err
		}
	}
	if err := sendInputs([]winInput{{typ: inputMouse, mi: mouseInput{dwFlags: mouseEventLeftDown}}}); err != nil {
		return err
	}
	// Release the button even if the move fails part-way, so a failed drag does
	// not leave the desktop stuck in a selection.
	moveErr := mouseMove(x, y, duration)
	upErr := sendInputs([]winInput{{typ: inputMouse, mi: mouseInput{dwFlags: mouseEventLeftUp}}})
	return errors.Join(moveErr, upErr)
}

func mouseScroll(amount, x, y int, horizontal bool) error {
	initPlatform()
	var inputs []winInput
	if x != 0 || y != 0 {
		inputs = append(inputs, absoluteMouseMove(x, y))
	}
	flag := uint32(mouseEventWheel)
	if horizontal {
		flag = mouseEventHWheel
	}
	inputs = append(inputs, winInput{typ: inputMouse, mi: mouseInput{
		mouseData: uint32(int32(amount * wheelDelta)), dwFlags: flag,
	}})
	return sendInputs(inputs)
}

// typeText sends the string as Unicode key events, so it does not depend on the
// active keyboard layout and handles non-ASCII directly.
func typeText(text string) error {
	initPlatform()
	var inputs []winInput
	for _, r := range text {
		for _, unit := range utf16Units(r) {
			for _, flags := range [...]uint32{keyEventUnicode, keyEventUnicode | keyEventKeyUp} {
				in := winInput{typ: inputKeyboard}
				*in.keyboard() = keybdInput{wScan: unit, dwFlags: flags}
				inputs = append(inputs, in)
			}
		}
	}
	return sendInputs(inputs)
}

func utf16Units(r rune) []uint16 {
	if r > 0xFFFF {
		r -= 0x10000
		return []uint16{uint16(0xD800 + (r >> 10)), uint16(0xDC00 + (r & 0x3FF))}
	}
	return []uint16{uint16(r)}
}

// pressKeys presses every named key in order and releases them in reverse, so
// "ctrl+shift+esc" is a real chord rather than three taps.
func pressKeys(keys []string) error {
	initPlatform()
	codes := make([]uint16, 0, len(keys))
	for _, k := range keys {
		vk, ok := virtualKey(k)
		if !ok {
			return fmt.Errorf("unknown key %q", k)
		}
		codes = append(codes, vk)
	}

	key := func(vk uint16, flags uint32) winInput {
		in := winInput{typ: inputKeyboard}
		*in.keyboard() = keybdInput{wVk: vk, dwFlags: flags}
		return in
	}
	var inputs []winInput
	for _, vk := range codes {
		inputs = append(inputs, key(vk, 0))
	}
	for i := len(codes) - 1; i >= 0; i-- {
		inputs = append(inputs, key(codes[i], keyEventKeyUp))
	}
	return sendInputs(inputs)
}

var namedKeys = map[string]uint16{
	"ctrl": 0x11, "control": 0x11, "alt": 0x12, "shift": 0x10,
	"win": 0x5B, "super": 0x5B, "cmd": 0x5B, "meta": 0x5B,
	"tab": 0x09, "enter": 0x0D, "return": 0x0D, "esc": 0x1B, "escape": 0x1B,
	"space": 0x20, "backspace": 0x08, "delete": 0x2E, "del": 0x2E,
	"insert": 0x2D, "home": 0x24, "end": 0x23, "pageup": 0x21, "pagedown": 0x22,
	"up": 0x26, "down": 0x28, "left": 0x25, "right": 0x27,
	"capslock": 0x14, "printscreen": 0x2C, "numlock": 0x90,
	"volumemute": 0xAD, "volumedown": 0xAE, "volumeup": 0xAF,
}

func virtualKey(name string) (uint16, bool) {
	k := strings.ToLower(strings.TrimSpace(name))
	if vk, ok := namedKeys[k]; ok {
		return vk, true
	}
	if len(k) > 1 && k[0] == 'f' {
		var n int
		if _, err := fmt.Sscanf(k[1:], "%d", &n); err == nil && n >= 1 && n <= 24 {
			return uint16(0x6F + n), true
		}
	}
	if len(k) == 1 {
		switch c := k[0]; {
		case c >= 'a' && c <= 'z':
			return uint16(c - 'a' + 0x41), true
		case c >= '0' && c <= '9':
			return uint16(c - '0' + 0x30), true
		}
	}
	return 0, false
}

// ── Clipboard ────────────────────────────────────────────────────────────────

// openClipboard pins the goroutine to its thread: the clipboard is owned by the
// opening thread, so a mid-sequence goroutine migration would break the close.
func openClipboard() error {
	runtime.LockOSThread()
	// The clipboard is a single global lock another process may briefly hold;
	// a few retries beat failing a screenshot-and-copy sequence outright.
	var lastErr error
	for range 10 {
		if ok, _, err := procOpenClipboard.Call(0); ok != 0 {
			return nil
		} else {
			lastErr = err
		}
		time.Sleep(20 * time.Millisecond)
	}
	runtime.UnlockOSThread()
	return fmt.Errorf("OpenClipboard failed: %w", lastErr)
}

func closeClipboard() {
	procCloseClipboard.Call()
	runtime.UnlockOSThread()
}

func getClipboard() (string, error) {
	if err := openClipboard(); err != nil {
		return "", err
	}
	defer closeClipboard()

	h, _, err := procGetClipboardData.Call(cfUnicodeText)
	if h == 0 {
		// An image or file list on the clipboard is not an error worth failing
		// the call over — there is simply no text.
		if err != nil && !errors.Is(err, syscall.Errno(0)) {
			return "", nil
		}
		return "", nil
	}
	ptr, err := globalLock(h)
	if err != nil {
		return "", err
	}
	defer procGlobalUnlock.Call(h)
	return windows.UTF16PtrToString(ptr), nil
}

// globalLock pins a moveable global memory block and returns a pointer into it.
//
// The block is allocated by GlobalAlloc, so it lives outside the Go heap and
// does not move while locked: the uintptr the call returns is a stable address,
// and converting it is sound. go vet's unsafeptr analyzer cannot see that
// provenance through a LazyProc call, so this is the single place in the server
// that trips it — which is why the Windows cross-vet runs with
// -unsafeptr=false while the host vet stays fully armed.
func globalLock(h uintptr) (*uint16, error) {
	ptr, _, err := procGlobalLock.Call(h)
	if ptr == 0 {
		return nil, fmt.Errorf("GlobalLock failed: %w", err)
	}
	return (*uint16)(unsafe.Pointer(ptr)), nil
}

func setClipboard(text string) error {
	utf16, err := windows.UTF16FromString(text)
	if err != nil {
		return fmt.Errorf("text is not valid for the clipboard: %w", err)
	}
	size := uintptr(len(utf16) * 2)

	if err := openClipboard(); err != nil {
		return err
	}
	defer closeClipboard()

	if ok, _, err := procEmptyClipboard.Call(); ok == 0 {
		return fmt.Errorf("EmptyClipboard failed: %w", err)
	}
	mem, _, err := procGlobalAlloc.Call(gmemMoveable, size)
	if mem == 0 {
		return fmt.Errorf("GlobalAlloc failed: %w", err)
	}
	ptr, err := globalLock(mem)
	if err != nil {
		procGlobalFree.Call(mem)
		return err
	}
	copy(unsafe.Slice(ptr, len(utf16)), utf16)
	procGlobalUnlock.Call(mem)

	if ok, _, err := procSetClipboardData.Call(cfUnicodeText, mem); ok == 0 {
		// Ownership only transfers on success, so on failure the block is ours
		// to free.
		procGlobalFree.Call(mem)
		return fmt.Errorf("SetClipboardData failed: %w", err)
	}
	return nil
}

// ── Misc ─────────────────────────────────────────────────────────────────────

func lockWorkstation() error {
	if ok, _, err := procLockWorkStation.Call(); ok == 0 {
		return fmt.Errorf("LockWorkStation failed: %w", err)
	}
	return nil
}

func systemLanguage() string {
	buf := make([]uint16, 85) // LOCALE_NAME_MAX_LENGTH
	n, _, _ := procGetUserDefaultLocaleName.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if n == 0 {
		return "unknown"
	}
	return windows.UTF16ToString(buf[:n])
}
