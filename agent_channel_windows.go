package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	procWTSVirtualChannelOpenEx = wtsapi32.NewProc("WTSVirtualChannelOpenEx")
	procWTSVirtualChannelQuery  = wtsapi32.NewProc("WTSVirtualChannelQuery")
	procWTSVirtualChannelClose  = wtsapi32.NewProc("WTSVirtualChannelClose")
)

const (
	wtsCurrentSession       = 0xFFFFFFFF
	wtsChannelOptionDynamic = 0x1
	wtsVirtualFileHandle    = 1
	channelPDUHeader        = 8
	channelReadBuffer       = 64 << 10
	channelWriteChunk       = 64 << 10
)

type virtualChannel struct {
	vc      uintptr
	file    windows.Handle
	readEv  windows.Handle
	writeEv windows.Handle
	readBuf []byte
	pending []byte
}

func openAgentChannel(name string) (io.ReadWriteCloser, error) {
	cname, err := windows.BytePtrFromString(name)
	if err != nil {
		return nil, err
	}
	vc, _, e := procWTSVirtualChannelOpenEx.Call(wtsCurrentSession, uintptr(unsafe.Pointer(cname)), wtsChannelOptionDynamic)
	if vc == 0 {
		return nil, fmt.Errorf("opening dynamic virtual channel %s: %w", name, e)
	}
	ch := &virtualChannel{vc: vc, readBuf: make([]byte, channelReadBuffer)}
	var handle *windows.Handle
	var size uint32
	if r, _, e := procWTSVirtualChannelQuery.Call(vc, wtsVirtualFileHandle,
		uintptr(unsafe.Pointer(&handle)), uintptr(unsafe.Pointer(&size))); r == 0 {
		ch.Close()
		return nil, fmt.Errorf("querying the file handle of channel %s: %w", name, e)
	}
	ch.file = *handle
	windows.WTSFreeMemory(uintptr(unsafe.Pointer(handle)))
	for _, ev := range []*windows.Handle{&ch.readEv, &ch.writeEv} {
		if *ev, err = windows.CreateEvent(nil, 1, 0, nil); err != nil {
			ch.Close()
			return nil, fmt.Errorf("creating an I/O event: %w", err)
		}
	}
	return ch, nil
}

func (c *virtualChannel) overlapped(ev windows.Handle, buf []byte, write bool) (uint32, error) {
	ov := windows.Overlapped{HEvent: ev}
	var done uint32
	var err error
	if write {
		err = windows.WriteFile(c.file, buf, &done, &ov)
	} else {
		err = windows.ReadFile(c.file, buf, &done, &ov)
	}
	if errors.Is(err, windows.ERROR_IO_PENDING) {
		err = windows.GetOverlappedResult(c.file, &ov, &done, true)
	}
	return done, err
}

func (c *virtualChannel) Read(p []byte) (int, error) {
	for len(c.pending) == 0 {
		n, err := c.overlapped(c.readEv, c.readBuf, false)
		if err != nil {
			return 0, fmt.Errorf("reading the virtual channel: %w", err)
		}
		if n < channelPDUHeader {
			return 0, fmt.Errorf("reading the virtual channel: %d-byte chunk is shorter than its header", n)
		}
		length := binary.LittleEndian.Uint32(c.readBuf)
		payload := c.readBuf[channelPDUHeader:n]
		if uint32(len(payload)) > length {
			payload = payload[:length]
		}
		c.pending = append(c.pending, payload...)
	}
	n := copy(p, c.pending)
	c.pending = c.pending[n:]
	return n, nil
}

func (c *virtualChannel) Write(p []byte) (int, error) {
	written := 0
	for written < len(p) {
		end := min(len(p), written+channelWriteChunk)
		n, err := c.overlapped(c.writeEv, p[written:end], true)
		written += int(n)
		if err != nil {
			return written, fmt.Errorf("writing the virtual channel: %w", err)
		}
	}
	return written, nil
}

func (c *virtualChannel) Close() error {
	var errs []error
	if c.vc != 0 {
		if r, _, e := procWTSVirtualChannelClose.Call(c.vc); r == 0 {
			errs = append(errs, fmt.Errorf("closing the virtual channel: %w", e))
		}
		c.vc = 0
	}
	for _, ev := range []windows.Handle{c.readEv, c.writeEv} {
		if ev != 0 {
			errs = append(errs, windows.CloseHandle(ev))
		}
	}
	return errors.Join(errs...)
}

func agentHome() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("locating the local app data directory: %w", err)
	}
	return filepath.Join(base, "win-rdp-mcp"), nil
}

func relocateAgent(args []string) (bool, error) {
	self, err := os.Executable()
	if err != nil {
		return false, fmt.Errorf("locating this binary: %w", err)
	}
	if !strings.HasPrefix(strings.ToLower(self), `\\tsclient\`) {
		return false, nil
	}
	home, err := agentHome()
	if err != nil {
		return false, err
	}
	if err := os.MkdirAll(home, 0o755); err != nil {
		return false, fmt.Errorf("creating %s: %w", home, err)
	}
	token := flagValue(args, "--token")
	dst := filepath.Join(home, "agent-"+token+".exe")
	if err := copyFile(self, dst); err != nil {
		return false, fmt.Errorf("copying the agent to %s: %w", dst, err)
	}
	logPath := filepath.Join(home, "agent-"+token+".log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return false, fmt.Errorf("opening %s: %w", logPath, err)
	}
	defer logFile.Close()
	cmd := exec.Command(dst, args...)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW | windows.CREATE_NEW_PROCESS_GROUP}
	if err := cmd.Start(); err != nil {
		return false, fmt.Errorf("starting the relocated agent %s: %w", dst, err)
	}
	if err := cmd.Process.Release(); err != nil {
		return false, err
	}
	removeStaleAgents(home, dst)
	return true, nil
}

func removeStaleAgents(home, keep string) {
	matches, err := filepath.Glob(filepath.Join(home, "agent-*.exe"))
	if err != nil {
		return
	}
	for _, m := range matches {
		if strings.EqualFold(m, keep) {
			continue
		}
		if err := os.Remove(m); err == nil {
			os.Remove(strings.TrimSuffix(m, ".exe") + ".log")
		}
	}
}

func flagValue(args []string, name string) string {
	for i, a := range args {
		if v, ok := strings.CutPrefix(a, name+"="); ok {
			return v
		}
		if a == name && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}
