//go:build !windows

package main

import "context"

func regReadTool(context.Context, arguments) (toolResult, error) { return toolResult{}, errWindowsOnly }
func regWriteTool(context.Context, arguments) (toolResult, error) {
	return toolResult{}, errWindowsOnly
}
