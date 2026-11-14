package main

import (
	"image"
	"image/color"
	"image/draw"
	"testing"
)

func screen(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(img, img.Bounds(), image.NewUniform(color.RGBA{A: 255}), image.Point{}, draw.Src)
	return img
}

func fill(img *image.RGBA, r image.Rectangle, c color.RGBA) *image.RGBA {
	out := image.NewRGBA(img.Bounds())
	copy(out.Pix, img.Pix)
	draw.Draw(out, r, image.NewUniform(c), image.Point{}, draw.Src)
	return out
}

func TestDialogAppearedInTheRunCorner(t *testing.T) {
	before := screen(1280, 800)
	area := runDialogArea(1280, 800)
	withRun := fill(before, image.Rect(8, 512, 435, 748), color.RGBA{240, 240, 240, 255})
	if !diffScreens(before, withRun, area).dialogAppeared() {
		t.Fatal("a Run dialog painting the corner was not recognised")
	}
	titleOnly := fill(before, image.Rect(8, 512, 435, 540), color.RGBA{0, 90, 200, 255})
	if diffScreens(before, titleOnly, area).dialogAppeared() {
		t.Fatal("a title-bar repaint of an existing dialog counted as a new dialog")
	}
	elsewhere := fill(withRun, image.Rect(600, 100, 1200, 500), color.RGBA{255, 255, 255, 255})
	if diffScreens(before, elsewhere, area).dialogAppeared() {
		t.Fatal("a large change outside the corner was accepted")
	}
}

func TestPasteLandedInsideTheDialog(t *testing.T) {
	area := runDialogArea(1280, 800)
	base := fill(screen(1280, 800), image.Rect(8, 512, 435, 748), color.RGBA{240, 240, 240, 255})
	typed := fill(base, image.Rect(85, 615, 380, 630), color.RGBA{0, 0, 0, 255})
	if !diffScreens(base, typed, area).landedInside() {
		t.Fatal("text appearing in the Run box was not recognised")
	}
	inNotepad := fill(base, image.Rect(48, 120, 700, 170), color.RGBA{0, 0, 0, 255})
	if diffScreens(base, inNotepad, area).landedInside() {
		t.Fatal("text appearing in another window was accepted")
	}
	if diffScreens(base, base, area).landedInside() {
		t.Fatal("no change at all was accepted")
	}
}
