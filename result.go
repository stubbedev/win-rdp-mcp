package main

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/color/palette"
	"image/draw"
	"image/gif"
	"image/jpeg"
	"image/png"
	"strings"
	"time"

	xdraw "golang.org/x/image/draw"
	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/math/fixed"
)

// contentBlock is one piece of a tool result: text, or an inline image the
// client renders (screenshots, annotated snapshots, screen recordings).
type contentBlock struct {
	Type string // "text" or "image"
	Text string
	Data []byte // raw image bytes; the SDK base64-encodes them on the wire
	MIME string
}

type toolResult struct {
	Content []contentBlock
	IsError bool
}

func textResult(format string, args ...any) toolResult {
	text := format
	if len(args) > 0 {
		text = fmt.Sprintf(format, args...)
	}
	return toolResult{Content: []contentBlock{{Type: "text", Text: text}}}
}

func imageResult(data []byte, mime, caption string) toolResult {
	return toolResult{Content: []contentBlock{
		{Type: "image", Data: data, MIME: mime},
		{Type: "text", Text: caption},
	}}
}

// withTaskID stamps the result with the task that produced it, so a later
// GetTaskStatus or CancelTask has an ID to name. It goes on the first text
// block, or becomes one when the result is image-only.
func (r toolResult) withTaskID(id string) toolResult {
	prefix := "[task:" + id + "] "
	for i, c := range r.Content {
		if c.Type == "text" {
			r.Content[i].Text = prefix + c.Text
			return r
		}
	}
	r.Content = append(r.Content, contentBlock{Type: "text", Text: strings.TrimSpace(prefix)})
	return r
}

// ── Image encoding ───────────────────────────────────────────────────────────

// resizeToWidth scales img down to maxWidth, preserving aspect. maxWidth of 0,
// or an image already narrower, is returned untouched — upscaling a screenshot
// only costs bytes.
func resizeToWidth(img image.Image, maxWidth int) image.Image {
	b := img.Bounds()
	if maxWidth <= 0 || b.Dx() <= maxWidth {
		return img
	}
	height := b.Dy() * maxWidth / b.Dx()
	dst := image.NewRGBA(image.Rect(0, 0, maxWidth, max(height, 1)))
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), img, b, xdraw.Src, nil)
	return dst
}

func jpegBytes(img image.Image, quality, maxWidth int) ([]byte, error) {
	quality = min(max(quality, 1), 100)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, resizeToWidth(img, maxWidth), &jpeg.Options{Quality: quality}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func pngBytes(img image.Image) ([]byte, error) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// gifBytes quantizes each frame onto the Plan9 palette and packs them into one
// looping animation. Floyd-Steinberg keeps gradients from banding into blocks.
func gifBytes(frames []image.Image, frameDelay time.Duration, maxWidth int) ([]byte, error) {
	if len(frames) == 0 {
		return nil, fmt.Errorf("no frames captured")
	}
	out := &gif.GIF{LoopCount: 0}
	// GIF delays are in hundredths of a second, and 0 means "as fast as
	// possible", which most viewers render inconsistently — floor at one tick.
	delay := max(int(frameDelay/(10*time.Millisecond)), 1)

	for _, frame := range frames {
		scaled := resizeToWidth(frame, maxWidth)
		paletted := image.NewPaletted(scaled.Bounds(), palette.Plan9)
		draw.FloydSteinberg.Draw(paletted, paletted.Bounds(), scaled, scaled.Bounds().Min)
		out.Image = append(out.Image, paletted)
		out.Delay = append(out.Delay, delay)
	}

	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, out); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ── Annotation ───────────────────────────────────────────────────────────────

var (
	annotationBox   = color.RGBA{R: 0xE0, G: 0x1B, B: 0x24, A: 0xFF}
	annotationLabel = color.RGBA{R: 0xFF, G: 0xFF, B: 0xFF, A: 0xFF}
)

// annotate draws a numbered red box around each element, so a model can say
// "click 7" instead of guessing pixel coordinates. Element rectangles are in
// screen coordinates; scale maps them onto a resized image.
func annotate(img *image.RGBA, elements []uiElement, origin image.Point, scale float64) {
	for _, el := range elements {
		r := image.Rect(
			int(float64(el.Rect.Min.X-origin.X)*scale),
			int(float64(el.Rect.Min.Y-origin.Y)*scale),
			int(float64(el.Rect.Max.X-origin.X)*scale),
			int(float64(el.Rect.Max.Y-origin.Y)*scale),
		).Intersect(img.Bounds())
		if r.Empty() {
			continue
		}
		strokeRect(img, r, annotationBox, 2)
		drawLabel(img, r.Min, fmt.Sprint(el.Index))
	}
}

func strokeRect(img *image.RGBA, r image.Rectangle, c color.Color, width int) {
	fill := image.NewUniform(c)
	edges := []image.Rectangle{
		image.Rect(r.Min.X, r.Min.Y, r.Max.X, r.Min.Y+width),
		image.Rect(r.Min.X, r.Max.Y-width, r.Max.X, r.Max.Y),
		image.Rect(r.Min.X, r.Min.Y, r.Min.X+width, r.Max.Y),
		image.Rect(r.Max.X-width, r.Min.Y, r.Max.X, r.Max.Y),
	}
	for _, e := range edges {
		draw.Draw(img, e.Intersect(img.Bounds()), fill, image.Point{}, draw.Src)
	}
}

// drawLabel paints the index in white on a solid red chip, placed above the box
// when there is room and inside it otherwise, so a control at y=0 stays legible.
func drawLabel(img *image.RGBA, at image.Point, text string) {
	face := basicfont.Face7x13
	width := font.MeasureString(face, text).Ceil() + 6
	height := face.Metrics().Height.Ceil() + 2

	top := at.Y - height
	if top < 0 {
		top = at.Y
	}
	chip := image.Rect(at.X, top, at.X+width, top+height).Intersect(img.Bounds())
	if chip.Empty() {
		return
	}
	draw.Draw(img, chip, image.NewUniform(annotationBox), image.Point{}, draw.Src)

	drawer := &font.Drawer{
		Dst: img, Src: image.NewUniform(annotationLabel), Face: face,
		Dot: fixed.P(chip.Min.X+3, chip.Min.Y+face.Metrics().Ascent.Ceil()),
	}
	drawer.DrawString(text)
}
