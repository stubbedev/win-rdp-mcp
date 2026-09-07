//go:build !windows

package main

import (
	"errors"
	"image"
	"time"
)

// Stubs for every non-Windows target. The server still builds, vets and tests
// here — which is how CI and the Nix flake exercise the protocol, config, tier
// and auth logic on Linux — but anything that touches a Windows desktop says so
// instead of pretending to work.

var errWindowsOnly = errors.New("this tool is only available when the server runs on Windows")

func initPlatform() {}

func captureRect(image.Rectangle) (*image.RGBA, error) { return nil, errWindowsOnly }
func captureScreen(int) (*image.RGBA, image.Rectangle, error) {
	return nil, image.Rectangle{}, errWindowsOnly
}

func enumerateWindows() ([]windowInfo, error)   { return nil, errWindowsOnly }
func interactiveElements() ([]uiElement, error) { return nil, errWindowsOnly }

func focusWindow(string, int64) (string, error)    { return "", errWindowsOnly }
func resizeWindow(int64, int, int) (string, error) { return "", errWindowsOnly }

func mouseClick(int, int, string, string) (string, error) { return "", errWindowsOnly }
func mouseMove(int, int, time.Duration) error             { return errWindowsOnly }
func mouseDrag(int, int, int, int, time.Duration) error   { return errWindowsOnly }
func mouseScroll(int, int, int, bool) error               { return errWindowsOnly }
func typeText(string) error                               { return errWindowsOnly }
func pressKeys([]string) error                            { return errWindowsOnly }

func getClipboard() (string, error) { return "", errWindowsOnly }
func setClipboard(string) error     { return errWindowsOnly }
func lockWorkstation() error        { return errWindowsOnly }
func systemLanguage() string        { return "unknown" }
