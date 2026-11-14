package main

import (
	"context"
	"errors"
	"fmt"
	"image"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/stubbedev/win-rdp-mcp/internal/grdp/protocol/pdu"
)

type inputSink interface {
	Key(scancode uint8, extended, release bool) error
	Unicode(unit uint16, release bool) error
	Pointer(flags uint16, x, y int) error
	Synchronize(toggles uint32) error
}

const syncNumLockOnly = 0x2

type sessionClipboard interface {
	offerClipboard(ctx context.Context, text string) (fetched <-chan struct{}, err error)
	remoteClipboardText() string
}

const pasteMinRunes = 200

func pasteWorthy(text string) bool {
	return utf8.RuneCountInString(text) >= pasteMinRunes && !strings.ContainsAny(text, "\r\n\t")
}

const pasteFetchWait = 3 * time.Second

var errPasteNotTaken = errors.New("nothing in the session took the paste (Ctrl+V reached no window that accepts text)")

type pointerButton uint16

const (
	pointerLeft   pointerButton = pdu.PTRFLAGS_BUTTON1
	pointerRight  pointerButton = pdu.PTRFLAGS_BUTTON2
	pointerMiddle pointerButton = pdu.PTRFLAGS_BUTTON3

	wheelNotch       = 120
	moveStepInterval = 10 * time.Millisecond
	focusSettle      = 100 * time.Millisecond
)

var pointerButtons = map[string]pointerButton{
	"left": pointerLeft, "right": pointerRight, "middle": pointerMiddle,
}

func parsePointerButton(name string) (pointerButton, error) {
	b, ok := pointerButtons[name]
	if !ok {
		return 0, fmt.Errorf("unknown mouse button %q (use left, right or middle)", name)
	}
	return b, nil
}

type desktopInput struct {
	sink      inputSink
	clipboard sessionClipboard
	size      sessionSize
	pointer   *image.Point
}

func (in *desktopInput) paste(ctx context.Context, text string) error {
	fetched, err := in.clipboard.offerClipboard(ctx, text)
	if err != nil {
		return err
	}
	if err := in.press(keyCtrl, keyV); err != nil {
		return err
	}
	timer := time.NewTimer(pasteFetchWait)
	defer timer.Stop()
	select {
	case <-fetched:
		return nil
	case <-timer.C:
		return errPasteNotTaken
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (in *desktopInput) checkPoint(x, y int) error {
	if x < 0 || y < 0 || x >= in.size.width || y >= in.size.height {
		return fmt.Errorf("point (%d,%d) is outside the %dx%d session", x, y, in.size.width, in.size.height)
	}
	return nil
}

func (in *desktopInput) moveTo(x, y int) error {
	if err := in.checkPoint(x, y); err != nil {
		return err
	}
	if err := in.sink.Pointer(pdu.PTRFLAGS_MOVE, x, y); err != nil {
		return err
	}
	*in.pointer = image.Pt(x, y)
	return nil
}

func (in *desktopInput) button(b pointerButton, down bool) error {
	flags := uint16(b)
	if down {
		flags |= pdu.PTRFLAGS_DOWN
	}
	return in.sink.Pointer(flags, in.pointer.X, in.pointer.Y)
}

func (in *desktopInput) clickAt(x, y int, b pointerButton, times int) (string, error) {
	if err := in.moveTo(x, y); err != nil {
		return "", err
	}
	for range times {
		if err := in.button(b, true); err != nil {
			return "", err
		}
		if err := in.button(b, false); err != nil {
			return "", err
		}
	}
	return fmt.Sprintf("(%d,%d)", x, y), nil
}

func (in *desktopInput) press(keys ...keyDef) error {
	var errs []error
	pressed := 0
	for _, k := range keys {
		if err := in.sink.Key(k.scan, k.extended, false); err != nil {
			errs = append(errs, err)
			break
		}
		pressed++
	}
	for i := pressed - 1; i >= 0; i-- {
		errs = append(errs, in.sink.Key(keys[i].scan, keys[i].extended, true))
	}
	return errors.Join(errs...)
}

type keystroke struct {
	key   keyDef
	isKey bool
	shift bool
	unit  uint16
}

func typingStrokes(text string) []keystroke {
	strokes := make([]keystroke, 0, len(text))
	for _, r := range strings.ReplaceAll(text, "\r\n", "\n") {
		switch r {
		case '\n', '\r':
			strokes = append(strokes, keystroke{key: keyEnter, isKey: true})
			continue
		case '\t':
			strokes = append(strokes, keystroke{key: keyTab, isKey: true})
			continue
		}
		if key, shift, ok := typingKey(r); ok {
			strokes = append(strokes, keystroke{key: key, isKey: true, shift: shift})
			continue
		}
		for _, u := range utf16.AppendRune(nil, r) {
			strokes = append(strokes, keystroke{unit: u})
		}
	}
	return strokes
}

func (in *desktopInput) typeString(text string) error {
	if err := in.sink.Synchronize(syncNumLockOnly); err != nil {
		return err
	}
	for _, s := range typingStrokes(text) {
		if s.isKey && s.shift {
			if err := in.press(keyShift, s.key); err != nil {
				return err
			}
			continue
		}
		if s.isKey {
			if err := in.press(s.key); err != nil {
				return err
			}
			continue
		}
		if err := in.sink.Unicode(s.unit, false); err != nil {
			return err
		}
		if err := in.sink.Unicode(s.unit, true); err != nil {
			return err
		}
	}
	return nil
}

func glidePath(from, to image.Point, steps int) []image.Point {
	if steps < 1 {
		return []image.Point{to}
	}
	path := make([]image.Point, 0, steps)
	for step := 1; step <= steps; step++ {
		frac := float64(step) / float64(steps)
		path = append(path, image.Pt(
			from.X+int(float64(to.X-from.X)*frac),
			from.Y+int(float64(to.Y-from.Y)*frac)))
	}
	return path
}

func (in *desktopInput) glide(ctx context.Context, x, y int, duration time.Duration) error {
	if err := in.checkPoint(x, y); err != nil {
		return err
	}
	for i, p := range glidePath(*in.pointer, image.Pt(x, y), int(duration/moveStepInterval)) {
		if i > 0 && !sleepCtx(ctx, moveStepInterval) {
			return ctx.Err()
		}
		if err := in.moveTo(p.X, p.Y); err != nil {
			return err
		}
	}
	return nil
}

func wheelFlags(notches int, horizontal bool) []uint16 {
	base := uint16(pdu.PTRFLAGS_WHEEL)
	if horizontal {
		base = pdu.PTRFLAGS_HWHEEL
	}
	flags := base | wheelNotch
	if notches < 0 {
		flags = base | pdu.PTRFLAGS_WHEEL_NEGATIVE | (0x100-wheelNotch)&0xFF
		notches = -notches
	}
	out := make([]uint16, notches)
	for i := range out {
		out[i] = flags
	}
	return out
}

func (in *desktopInput) click(at image.Point, args arguments) (toolResult, error) {
	x, y := at.X, at.Y
	name := args.stringOr("button", "left")
	b, err := parsePointerButton(name)
	if err != nil {
		return toolResult{}, err
	}
	switch action := args.stringOr("action", "click"); action {
	case "hover":
		if err := in.moveTo(x, y); err != nil {
			return toolResult{}, err
		}
		return textResult("Hovered at (%d,%d)", x, y), nil
	case "double":
		at, err := in.clickAt(x, y, b, 2)
		if err != nil {
			return toolResult{}, err
		}
		return textResult("Double-clicked at %s", at), nil
	case "click":
		at, err := in.clickAt(x, y, b, 1)
		if err != nil {
			return toolResult{}, err
		}
		return textResult("Clicked %s at %s", name, at), nil
	default:
		return toolResult{}, fmt.Errorf("unknown click action %q (use click, double or hover)", action)
	}
}

func (in *desktopInput) typeText(ctx context.Context, args arguments) (toolResult, error) {
	text := args.stringOr("text", "")
	if x, y := args.intOr("x", 0), args.intOr("y", 0); x != 0 || y != 0 {
		if _, err := in.clickAt(x, y, pointerLeft, 1); err != nil {
			return toolResult{}, err
		}
		sleepCtx(ctx, focusSettle)
	}
	if args.boolOr("clear", false) {
		if err := in.press(keyCtrl, keyA); err != nil {
			return toolResult{}, err
		}
		if err := in.press(keyDel); err != nil {
			return toolResult{}, err
		}
	}
	verb, err := in.enterText(ctx, text)
	if err != nil {
		return toolResult{}, err
	}
	if args.boolOr("press_enter", false) {
		if err := in.press(keyEnter); err != nil {
			return toolResult{}, err
		}
	}
	return textResult("%s %d characters", verb, utf8.RuneCountInString(text)), nil
}

func (in *desktopInput) enterText(ctx context.Context, text string) (string, error) {
	if pasteWorthy(text) {
		previous := in.clipboard.remoteClipboardText()
		err := in.paste(ctx, text)
		if err == nil {
			if previous != "" {
				if _, err := in.clipboard.offerClipboard(ctx, previous); err != nil {
					logf("could not put the session clipboard back after pasting: %v", err)
				}
			}
			return "Pasted", nil
		}
		if !errors.Is(err, errPasteNotTaken) {
			return "", err
		}
	}
	return "Typed", in.typeString(text)
}

func (in *desktopInput) move(ctx context.Context, args arguments) (toolResult, error) {
	x, y := args.intOr("x", 0), args.intOr("y", 0)
	duration := time.Duration(args.floatOr("duration", 0.3) * float64(time.Second))
	if !args.boolOr("drag", false) {
		if err := in.glide(ctx, x, y, duration); err != nil {
			return toolResult{}, err
		}
		return textResult("Moved to (%d,%d)", x, y), nil
	}
	if sx, sy := args.intOr("start_x", 0), args.intOr("start_y", 0); sx != 0 || sy != 0 {
		if err := in.moveTo(sx, sy); err != nil {
			return toolResult{}, err
		}
	}
	if err := in.button(pointerLeft, true); err != nil {
		return toolResult{}, err
	}
	moveErr := in.glide(ctx, x, y, duration)
	if err := errors.Join(moveErr, in.button(pointerLeft, false)); err != nil {
		return toolResult{}, err
	}
	return textResult("Dragged to (%d,%d)", x, y), nil
}

func (in *desktopInput) scroll(args arguments) (toolResult, error) {
	amount := args.intOr("amount", 0)
	horizontal := args.boolOr("horizontal", false)
	if x, y := args.intOr("x", 0), args.intOr("y", 0); x != 0 || y != 0 {
		if err := in.moveTo(x, y); err != nil {
			return toolResult{}, err
		}
	}
	for _, flags := range wheelFlags(amount, horizontal) {
		if err := in.sink.Pointer(flags, in.pointer.X, in.pointer.Y); err != nil {
			return toolResult{}, err
		}
	}
	dir := "vertically"
	if horizontal {
		dir = "horizontally"
	}
	return textResult("Scrolled %d %s", amount, dir), nil
}

func (in *desktopInput) shortcut(args arguments) (toolResult, error) {
	raw := args.stringOr("keys", "")
	keys, err := parseKeyCombo(raw)
	if err != nil {
		return toolResult{}, err
	}
	if err := in.press(keys...); err != nil {
		return toolResult{}, err
	}
	return textResult("Executed shortcut: %s", raw), nil
}
