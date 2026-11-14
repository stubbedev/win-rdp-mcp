package main

import (
	"context"
	"errors"
	"fmt"
	"image"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stubbedev/win-rdp-mcp/internal/grdp"
	"github.com/stubbedev/win-rdp-mcp/internal/grdp/protocol/pdu"
)

type inputEvent struct {
	kind    string
	code    uint16
	ext     bool
	release bool
	x, y    int
}

type recordingSink struct {
	events []inputEvent
	failAt int
}

func (r *recordingSink) record(e inputEvent) error {
	r.events = append(r.events, e)
	if r.failAt > 0 && len(r.events) == r.failAt {
		return errors.New("sink failed")
	}
	return nil
}

func (r *recordingSink) Key(sc uint8, ext, release bool) error {
	return r.record(inputEvent{kind: "key", code: uint16(sc), ext: ext, release: release})
}

func (r *recordingSink) Unicode(u uint16, release bool) error {
	return r.record(inputEvent{kind: "unicode", code: u, release: release})
}

func (r *recordingSink) Pointer(flags uint16, x, y int) error {
	return r.record(inputEvent{kind: "pointer", code: flags, x: x, y: y})
}

func (r *recordingSink) Synchronize(toggles uint32) error {
	return r.record(inputEvent{kind: "sync", code: uint16(toggles)})
}

func newTestInput() (*desktopInput, *recordingSink) {
	sink := &recordingSink{}
	return &desktopInput{sink: sink, size: sessionSize{width: 1280, height: 800}, pointer: &image.Point{}}, sink
}

func TestParseKeyCombo(t *testing.T) {
	cases := map[string][]keyDef{
		"ctrl+c":         {keyCtrl, mustLookupKey("c")},
		"CTRL+Shift+Esc": {keyCtrl, keyShift, mustLookupKey("escape")},
		"win+r":          {keyWin, keyR},
		" alt + f4 ":     {keyAlt, {vk: 0x73, scan: 0x3E}},
		"F24":            {{vk: 0x87, scan: 0x76}},
		"ctrl+/":         {keyCtrl, {vk: 0xBF, scan: 0x35}},
		"ctrl++":         {keyCtrl},
	}
	for in, want := range cases {
		got, err := parseKeyCombo(in)
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("parseKeyCombo(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "+", "ctrl+f25", "ctrl+f04", "KP_Enter", "ctrl+é"} {
		if _, err := parseKeyCombo(bad); err == nil {
			t.Errorf("parseKeyCombo(%q) accepted an unknown key", bad)
		}
	}
}

func TestKeyTableIsConsistent(t *testing.T) {
	seen := map[uint16]keyDef{}
	check := func(name string, k keyDef) {
		if prev, ok := seen[k.vk]; ok && prev != k {
			t.Errorf("%s: virtual key 0x%02x maps to two scancodes", name, k.vk)
		}
		seen[k.vk] = k
		if k.scan == 0 {
			t.Errorf("%s has no scancode", name)
		}
	}
	for name, k := range namedKeys {
		check(name, k)
	}
	for r, k := range characterKeys {
		check(string(r), k)
	}
}

func TestPressReleasesInReverse(t *testing.T) {
	in, sink := newTestInput()
	if err := in.press(keyCtrl, keyShift, mustLookupKey("escape")); err != nil {
		t.Fatal(err)
	}
	want := []inputEvent{
		{kind: "key", code: 0x1D}, {kind: "key", code: 0x2A}, {kind: "key", code: 0x01},
		{kind: "key", code: 0x01, release: true}, {kind: "key", code: 0x2A, release: true}, {kind: "key", code: 0x1D, release: true},
	}
	if !slices.Equal(sink.events, want) {
		t.Fatalf("events = %+v, want %+v", sink.events, want)
	}
}

func TestPressReleasesHeldKeysWhenAKeyFails(t *testing.T) {
	in, sink := newTestInput()
	sink.failAt = 2
	if err := in.press(keyCtrl, keyShift, keyTab); err == nil {
		t.Fatal("no error from a failing sink")
	}
	want := []inputEvent{
		{kind: "key", code: 0x1D}, {kind: "key", code: 0x2A}, {kind: "key", code: 0x1D, release: true},
	}
	if !slices.Equal(sink.events, want) {
		t.Fatalf("events = %+v, want %+v", sink.events, want)
	}
}

func TestExtendedKeysCarryTheFlag(t *testing.T) {
	in, sink := newTestInput()
	if err := in.press(keyWin); err != nil {
		t.Fatal(err)
	}
	if !sink.events[0].ext || sink.events[0].code != 0x5B {
		t.Fatalf("win key sent as %+v, want extended scancode 0x5B", sink.events[0])
	}
}

func TestTypingStrokes(t *testing.T) {
	strokes := typingStrokes("aA!\r\nb\té😀")
	var got []string
	for _, s := range strokes {
		switch {
		case s.isKey && s.key == keyEnter:
			got = append(got, "ENTER")
		case s.isKey && s.key == keyTab:
			got = append(got, "TAB")
		case s.isKey && s.shift:
			got = append(got, fmt.Sprintf("shift+%#x", s.key.scan))
		case s.isKey:
			got = append(got, fmt.Sprintf("%#x", s.key.scan))
		default:
			got = append(got, fmt.Sprintf("u+%04x", s.unit))
		}
	}
	want := []string{"0x1e", "shift+0x1e", "shift+0x2", "ENTER", "0x30", "TAB", "u+00e9", "u+d83d", "u+de00"}
	if !slices.Equal(got, want) {
		t.Fatalf("strokes = %q, want %q", got, want)
	}
}

func TestEveryPrintableASCIICharacterHasAKey(t *testing.T) {
	for r := rune(0x20); r < 0x7F; r++ {
		if _, _, ok := typingKey(r); !ok {
			t.Errorf("%q has no US-layout key", r)
		}
	}
}

func TestTypeStringSynchronizesThenSendsKeys(t *testing.T) {
	in, sink := newTestInput()
	if err := in.typeString("Hé\n"); err != nil {
		t.Fatal(err)
	}
	want := []inputEvent{
		{kind: "sync", code: syncNumLockOnly},
		{kind: "key", code: 0x2A}, {kind: "key", code: 0x23}, {kind: "key", code: 0x23, release: true}, {kind: "key", code: 0x2A, release: true},
		{kind: "unicode", code: 'é'}, {kind: "unicode", code: 'é', release: true},
		{kind: "key", code: 0x1C}, {kind: "key", code: 0x1C, release: true},
	}
	if !slices.Equal(sink.events, want) {
		t.Fatalf("events = %+v, want %+v", sink.events, want)
	}
}

func TestClickMovesThenPressesTheButton(t *testing.T) {
	in, sink := newTestInput()
	res, err := in.click(context.Background(), arguments{"x": 10.0, "y": 20.0, "button": "right", "action": "double"})
	if err != nil {
		t.Fatal(err)
	}
	want := []inputEvent{
		{kind: "pointer", code: pdu.PTRFLAGS_MOVE, x: 10, y: 20},
		{kind: "pointer", code: pdu.PTRFLAGS_BUTTON2 | pdu.PTRFLAGS_DOWN, x: 10, y: 20},
		{kind: "pointer", code: pdu.PTRFLAGS_BUTTON2, x: 10, y: 20},
		{kind: "pointer", code: pdu.PTRFLAGS_BUTTON2 | pdu.PTRFLAGS_DOWN, x: 10, y: 20},
		{kind: "pointer", code: pdu.PTRFLAGS_BUTTON2, x: 10, y: 20},
	}
	if !slices.Equal(sink.events, want) {
		t.Fatalf("events = %+v, want %+v", sink.events, want)
	}
	if !strings.Contains(firstText(res.Content), "Double-clicked at (10,20)") {
		t.Fatalf("result = %q", firstText(res.Content))
	}
}

func TestClickRejectsPointsOutsideTheSession(t *testing.T) {
	in, sink := newTestInput()
	if _, err := in.click(context.Background(), arguments{"x": 1280.0, "y": 5.0}); err == nil {
		t.Fatal("click outside the session was accepted")
	}
	if len(sink.events) != 0 {
		t.Fatalf("input was sent for a rejected click: %+v", sink.events)
	}
}

func TestDragReleasesTheButtonWhenTheMoveFails(t *testing.T) {
	in, sink := newTestInput()
	_, err := in.move(context.Background(), arguments{"drag": true, "x": 5000.0, "y": 1.0, "duration": 0.0})
	if err == nil {
		t.Fatal("drag to an out-of-bounds point succeeded")
	}
	last := sink.events[len(sink.events)-1]
	if last.code != pdu.PTRFLAGS_BUTTON1 {
		t.Fatalf("last event %+v, want the left button released", last)
	}
}

func TestGlidePath(t *testing.T) {
	got := glidePath(image.Pt(0, 0), image.Pt(30, 60), 3)
	want := []image.Point{{10, 20}, {20, 40}, {30, 60}}
	if !slices.Equal(got, want) {
		t.Fatalf("glidePath = %v, want %v", got, want)
	}
	got = glidePath(image.Pt(100, 100), image.Pt(7, 3), 7)
	if len(got) != 7 || got[6] != image.Pt(7, 3) {
		t.Fatalf("glidePath does not end on the target: %v", got)
	}
	if got := glidePath(image.Pt(1, 1), image.Pt(9, 9), 0); !slices.Equal(got, []image.Point{{9, 9}}) {
		t.Fatalf("zero steps = %v, want a jump to the target", got)
	}
}

func TestWheelFlags(t *testing.T) {
	up := wheelFlags(2, false)
	if len(up) != 2 || up[0] != pdu.PTRFLAGS_WHEEL|120 {
		t.Fatalf("wheel up = %#x", up)
	}
	down := wheelFlags(-1, false)
	if want := uint16(pdu.PTRFLAGS_WHEEL | pdu.PTRFLAGS_WHEEL_NEGATIVE | 0x88); len(down) != 1 || down[0] != want {
		t.Fatalf("wheel down = %#x, want %#x", down, want)
	}
	if right := wheelFlags(1, true); right[0] != pdu.PTRFLAGS_HWHEEL|120 {
		t.Fatalf("horizontal wheel = %#x", right)
	}
	if none := wheelFlags(0, false); len(none) != 0 {
		t.Fatalf("zero notches = %#x", none)
	}
}

func TestNewSessionSize(t *testing.T) {
	if _, err := newSessionSize(1280, 800); err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][2]int{{0, 800}, {1280, 100}, {9000, 800}} {
		if _, err := newSessionSize(bad[0], bad[1]); err == nil {
			t.Errorf("newSessionSize(%d, %d) accepted", bad[0], bad[1])
		}
	}
}

func TestWithDefaultPort(t *testing.T) {
	cases := map[string]string{
		"192.168.1.50":      "192.168.1.50:3389",
		"host:3390":         "host:3390",
		"fe80::1":           "[fe80::1]:3389",
		"[fe80::1]:3390":    "[fe80::1]:3390",
		"desk.example.test": "desk.example.test:3389",
	}
	for in, want := range cases {
		if got := withDefaultPort(in); got != want {
			t.Errorf("withDefaultPort(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDisconnectReason(t *testing.T) {
	if got := disconnectReason(0x05); got != "another connection took the session" {
		t.Fatalf("0x05 = %q", got)
	}
	if got := disconnectReason(0x1234); got != "server error info 0x00001234" {
		t.Fatalf("unknown code = %q", got)
	}
}

func TestPaintComposesBitmapsIntoTheFramebuffer(t *testing.T) {
	conn := &rdpConn{fb: image.NewRGBA(image.Rect(0, 0, 4, 4))}
	bgra := []byte{0x10, 0x20, 0x30, 0xFF, 0x40, 0x50, 0x60, 0xFF}
	conn.paint([]grdp.Bitmap{{DestLeft: 1, DestTop: 2, DestRight: 2, DestBottom: 2, Width: 2, Height: 1, BitsPerPixel: 4, Data: bgra}})
	img := conn.capture()
	if c := img.RGBAAt(1, 2); c.R != 0x30 || c.G != 0x20 || c.B != 0x10 {
		t.Fatalf("pixel (1,2) = %+v", c)
	}
	if c := img.RGBAAt(2, 2); c.R != 0x60 {
		t.Fatalf("pixel (2,2) = %+v", c)
	}
	if c := img.RGBAAt(0, 0); c.A != 0 {
		t.Fatalf("pixel (0,0) was painted: %+v", c)
	}
}

func TestLaunchCommandCarriesTokenAndTierFlags(t *testing.T) {
	p, err := newAgentProxy(nil, "", map[string]bool{"Shell": true})
	if err != nil {
		t.Fatal(err)
	}
	got := p.launchCommand()
	want := `cmd /c \\tsclient\mcp\win-rdp-mcp.exe --transport rdpvc --token ` + p.link.token + ` --enable-tier3 --disable-tier2`
	if got != want {
		t.Fatalf("launch = %q, want %q", got, want)
	}
	if len(got) > runDialogMaxChars {
		t.Fatalf("launch command is %d characters; the Run dialog truncates past %d", len(got), runDialogMaxChars)
	}
	if !validAgentToken(p.link.token) {
		t.Fatalf("token %q is not a valid agent token", p.link.token)
	}
	other, err := newAgentProxy(nil, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.link.token == other.link.token {
		t.Fatal("two controllers drew the same token, so a stale agent could not be told apart")
	}
}

func TestWaitQuietReturnsAtOnceWhenIdle(t *testing.T) {
	conn := &rdpConn{}
	start := time.Now()
	conn.waitQuiet(context.Background(), 100*time.Millisecond, time.Second, false)
	if time.Since(start) > 50*time.Millisecond {
		t.Fatalf("an idle screen waited %s", time.Since(start))
	}
}

func TestWaitQuietWaitsOutRecentPaints(t *testing.T) {
	conn := &rdpConn{}
	conn.lastPaint.Store(time.Now().UnixNano())
	start := time.Now()
	conn.waitQuiet(context.Background(), 100*time.Millisecond, time.Second, false)
	if waited := time.Since(start); waited < 80*time.Millisecond || waited > 500*time.Millisecond {
		t.Fatalf("waited %s after a fresh paint, want about the 100ms quiet window", waited)
	}
}

func TestWaitQuietGivesUpAtItsLimit(t *testing.T) {
	conn := &rdpConn{}
	start := time.Now()
	conn.waitQuiet(context.Background(), 50*time.Millisecond, 150*time.Millisecond, true)
	if waited := time.Since(start); waited < 140*time.Millisecond || waited > 600*time.Millisecond {
		t.Fatalf("waited %s for a first paint that never came, want the 150ms limit", waited)
	}
}

func TestWaitPaintAfterSeesOnlyNewPaints(t *testing.T) {
	conn := &rdpConn{}
	before := time.Now().UnixNano()
	conn.lastPaint.Store(before)
	if conn.waitPaintAfter(context.Background(), before, 60*time.Millisecond) {
		t.Fatal("a paint from before the input counted as a reaction to it")
	}
	go func() {
		time.Sleep(30 * time.Millisecond)
		conn.lastPaint.Store(time.Now().UnixNano())
	}()
	if !conn.waitPaintAfter(context.Background(), before, time.Second) {
		t.Fatal("a fresh paint was not seen")
	}
}

func TestCheckAgentExe(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "win-rdp-mcp.exe")
	if err := checkAgentExe(exe); err == nil || !strings.Contains(err.Error(), "--no-agent") {
		t.Fatalf("a missing agent gave %v, want an error naming the way out", err)
	}
	if err := os.WriteFile(exe, []byte("MZ"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := checkAgentExe(exe); err != nil {
		t.Fatal(err)
	}
	if err := checkAgentExe(dir); err == nil {
		t.Fatal("a directory was accepted as the agent")
	}
}
