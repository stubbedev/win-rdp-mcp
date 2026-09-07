package main

import "image"

// The desktop-control surface. Everything here is implemented with Win32 calls
// in platform_windows.go; platform_other.go supplies stubs that report the tool
// is Windows-only, so the whole server still builds, vets and tests on Linux
// and macOS — which is what lets CI and the Nix flake check it at all.

// windowInfo is one visible top-level window.
type windowInfo struct {
	Handle int64
	Title  string
	Rect   image.Rectangle
	PID    uint32
}

// uiElement is one child control of the foreground window: the poor man's
// accessibility tree the upstream server exposes, and enough for an agent to
// aim a click.
type uiElement struct {
	Index int
	Class string
	Text  string
	Rect  image.Rectangle
}

func (e uiElement) center() (int, int) {
	return (e.Rect.Min.X + e.Rect.Max.X) / 2, (e.Rect.Min.Y + e.Rect.Max.Y) / 2
}

// label is the element's text, falling back to its window class when the
// control has no caption of its own.
func (e uiElement) label() string {
	if e.Text != "" {
		return e.Text
	}
	return e.Class
}
