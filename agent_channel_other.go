//go:build !windows

package main

import (
	"errors"
	"io"
)

func openAgentChannel(string) (io.ReadWriteCloser, error) {
	return nil, errors.New("the rdpvc transport needs a Windows RDP session")
}

func relocateAgent([]string) (bool, error) { return false, nil }
