//go:build !linux

package main

import "errors"

// The controller drives the target through xfreerdp/xdotool/import, which this
// build ships against only on Linux. The agent half runs anywhere its own
// platform supports.
func controlMain([]string) error {
	return errors.New("the controller (win-rdp-mcp control) is only supported on Linux")
}
