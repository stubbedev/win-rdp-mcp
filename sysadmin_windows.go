package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

var (
	wtsapi32 = windows.NewLazySystemDLL("wtsapi32.dll")
	winmm    = windows.NewLazySystemDLL("winmm.dll")
	iphlpapi = windows.NewLazySystemDLL("iphlpapi.dll")
	wevtapi  = windows.NewLazySystemDLL("wevtapi.dll")

	procWTSQuerySessionInformationW = wtsapi32.NewProc("WTSQuerySessionInformationW")
	procWTSConnectSessionW          = wtsapi32.NewProc("WTSConnectSessionW")

	procMciSendStringW     = winmm.NewProc("mciSendStringW")
	procMciGetErrorStringW = winmm.NewProc("mciGetErrorStringW")

	procIcmpCreateFile  = iphlpapi.NewProc("IcmpCreateFile")
	procIcmp6CreateFile = iphlpapi.NewProc("Icmp6CreateFile")
	procIcmpCloseHandle = iphlpapi.NewProc("IcmpCloseHandle")
	procIcmpSendEcho    = iphlpapi.NewProc("IcmpSendEcho")
	procIcmp6SendEcho2  = iphlpapi.NewProc("Icmp6SendEcho2")

	procEvtQuery                 = wevtapi.NewProc("EvtQuery")
	procEvtNext                  = wevtapi.NewProc("EvtNext")
	procEvtClose                 = wevtapi.NewProc("EvtClose")
	procEvtCreateRenderContext   = wevtapi.NewProc("EvtCreateRenderContext")
	procEvtRender                = wevtapi.NewProc("EvtRender")
	procEvtOpenPublisherMetadata = wevtapi.NewProc("EvtOpenPublisherMetadata")
	procEvtFormatMessage         = wevtapi.NewProc("EvtFormatMessage")
)

const (
	wtsConnectState = 8
	wtsActive       = 0
	wtsDisconnected = 4
)

func attachSessionToConsole(force bool) (bool, error) {
	var session uint32
	if err := windows.ProcessIdToSessionId(windows.GetCurrentProcessId(), &session); err != nil {
		return false, fmt.Errorf("finding this process's session: %w", err)
	}
	state, err := sessionConnectState(session)
	if err != nil {
		return false, err
	}
	console := windows.WTSGetActiveConsoleSessionId()
	if state == wtsActive && session == console {
		return false, nil
	}
	if state != wtsDisconnected && !force {
		return false, nil
	}
	password, err := windows.UTF16PtrFromString("")
	if err != nil {
		return false, err
	}
	if r, _, e := procWTSConnectSessionW.Call(uintptr(session), uintptr(console), uintptr(unsafe.Pointer(password)), 1); r == 0 {
		return false, fmt.Errorf("attaching session %d to the console (session %d): %w", session, console, e)
	}
	return true, nil
}

func sessionConnectState(session uint32) (uint32, error) {
	var buf *uint32
	var size uint32
	if r, _, e := procWTSQuerySessionInformationW.Call(0, uintptr(session), wtsConnectState,
		uintptr(unsafe.Pointer(&buf)), uintptr(unsafe.Pointer(&size))); r == 0 {
		return 0, fmt.Errorf("querying the state of session %d: %w", session, e)
	}
	defer windows.WTSFreeMemory(uintptr(unsafe.Pointer(buf)))
	if size < 4 {
		return 0, fmt.Errorf("querying the state of session %d: %d-byte answer", session, size)
	}
	return *buf, nil
}

func launchApp(name, args string) error {
	file, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return err
	}
	var params *uint16
	if args != "" {
		if params, err = windows.UTF16PtrFromString(args); err != nil {
			return err
		}
	}
	return windows.ShellExecute(0, nil, file, params, nil, windows.SW_SHOWNORMAL)
}

var mciAliasSeq atomic.Uint64

func mciSend(command string) (string, error) {
	cmd, err := windows.UTF16PtrFromString(command)
	if err != nil {
		return "", err
	}
	ret := make([]uint16, 128)
	code, _, _ := procMciSendStringW.Call(uintptr(unsafe.Pointer(cmd)), uintptr(unsafe.Pointer(&ret[0])), uintptr(len(ret)), 0)
	if code != 0 {
		msg := make([]uint16, 256)
		procMciGetErrorStringW.Call(code, uintptr(unsafe.Pointer(&msg[0])), uintptr(len(msg)))
		return "", fmt.Errorf("MCI %q: %s (error %d)", command, windows.UTF16ToString(msg), code)
	}
	return windows.UTF16ToString(ret), nil
}

func playAudio(ctx context.Context, path string) error {
	if strings.ContainsRune(path, '"') {
		return fmt.Errorf("audio path %q contains a quote", path)
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	device := "mpegvideo"
	if strings.EqualFold(filepath.Ext(path), ".wav") {
		device = "waveaudio"
	}
	alias := fmt.Sprintf("winrdpmcp%d", mciAliasSeq.Add(1))
	if _, err := mciSend(fmt.Sprintf(`open "%s" type %s alias %s`, path, device, alias)); err != nil {
		return err
	}
	defer mciSend("close " + alias)
	if _, err := mciSend("play " + alias); err != nil {
		return err
	}
	for {
		mode, err := mciSend("status " + alias + " mode")
		if err != nil {
			return err
		}
		if mode != "playing" {
			return nil
		}
		if !sleepCtx(ctx, 200*time.Millisecond) {
			_, stopErr := mciSend("stop " + alias)
			return errors.Join(ctx.Err(), stopErr)
		}
	}
}

var serviceStateNames = map[uint32]string{
	windows.SERVICE_STOPPED: "Stopped", windows.SERVICE_START_PENDING: "StartPending",
	windows.SERVICE_STOP_PENDING: "StopPending", windows.SERVICE_RUNNING: "Running",
	windows.SERVICE_CONTINUE_PENDING: "ContinuePending", windows.SERVICE_PAUSE_PENDING: "PausePending",
	windows.SERVICE_PAUSED: "Paused",
}

func serviceStateName(state uint32) string {
	if n, ok := serviceStateNames[state]; ok {
		return n
	}
	return fmt.Sprintf("State %d", state)
}

func withSCManager(access uint32, f func(windows.Handle) error) error {
	h, err := windows.OpenSCManager(nil, nil, access)
	if err != nil {
		return fmt.Errorf("opening the service control manager: %w", err)
	}
	defer windows.CloseServiceHandle(h)
	return f(h)
}

func listServices() ([]serviceInfo, error) {
	var out []serviceInfo
	err := withSCManager(windows.SC_MANAGER_ENUMERATE_SERVICE, func(scm windows.Handle) error {
		var needed, count, resume uint32
		var buf []byte
		for {
			var ptr *byte
			if len(buf) > 0 {
				ptr = &buf[0]
			}
			err := windows.EnumServicesStatusEx(scm, windows.SC_ENUM_PROCESS_INFO, windows.SERVICE_WIN32,
				windows.SERVICE_STATE_ALL, ptr, uint32(len(buf)), &needed, &count, &resume, nil)
			if err == nil {
				break
			}
			if !errors.Is(err, windows.ERROR_MORE_DATA) {
				return fmt.Errorf("enumerating services: %w", err)
			}
			buf = make([]byte, needed)
			resume = 0
		}
		if count == 0 {
			return nil
		}
		entries := unsafe.Slice((*windows.ENUM_SERVICE_STATUS_PROCESS)(unsafe.Pointer(&buf[0])), count)
		out = make([]serviceInfo, 0, count)
		for _, e := range entries {
			out = append(out, serviceInfo{
				Name:        windows.UTF16PtrToString(e.ServiceName),
				DisplayName: windows.UTF16PtrToString(e.DisplayName),
				State:       serviceStateName(e.ServiceStatusProcess.CurrentState),
			})
		}
		return nil
	})
	return out, err
}

func withService(name string, access uint32, f func(*mgr.Service) error) error {
	return withSCManager(windows.SC_MANAGER_CONNECT, func(scm windows.Handle) error {
		n, err := windows.UTF16PtrFromString(name)
		if err != nil {
			return err
		}
		h, err := windows.OpenService(scm, n, access)
		if err != nil {
			return fmt.Errorf("opening service %s: %w", name, err)
		}
		s := &mgr.Service{Name: name, Handle: h}
		defer s.Close()
		return f(s)
	})
}

func waitServiceState(ctx context.Context, s *mgr.Service, want svc.State) (string, error) {
	for {
		status, err := s.Query()
		if err != nil {
			return "", fmt.Errorf("querying service %s: %w", s.Name, err)
		}
		if status.State == want {
			return serviceStateName(uint32(status.State)), nil
		}
		if !sleepCtx(ctx, 250*time.Millisecond) {
			return serviceStateName(uint32(status.State)), fmt.Errorf("service %s is still %s: %w",
				s.Name, serviceStateName(uint32(status.State)), ctx.Err())
		}
	}
}

func startService(ctx context.Context, name string) (string, error) {
	var state string
	err := withService(name, windows.SERVICE_START|windows.SERVICE_QUERY_STATUS, func(s *mgr.Service) error {
		if err := s.Start(); err != nil && !errors.Is(err, windows.ERROR_SERVICE_ALREADY_RUNNING) {
			return err
		}
		var err error
		state, err = waitServiceState(ctx, s, svc.Running)
		return err
	})
	return state, err
}

func stopService(ctx context.Context, name string) (string, error) {
	var state string
	access := uint32(windows.SERVICE_STOP | windows.SERVICE_QUERY_STATUS | windows.SERVICE_ENUMERATE_DEPENDENTS)
	err := withService(name, access, func(s *mgr.Service) error {
		dependents, err := s.ListDependentServices(svc.Active)
		if err != nil {
			return fmt.Errorf("listing services that depend on %s: %w", name, err)
		}
		for _, dep := range dependents {
			if _, err := stopService(ctx, dep); err != nil {
				return fmt.Errorf("stopping dependent service: %w", err)
			}
		}
		if _, err := s.Control(svc.Stop); err != nil && !errors.Is(err, windows.ERROR_SERVICE_NOT_ACTIVE) {
			return err
		}
		state, err = waitServiceState(ctx, s, svc.Stopped)
		return err
	})
	return state, err
}

const (
	icmpStatusSuccess   = 0
	icmpReplyHeaderSize = 64
	icmpPayload         = "win-rdp-mcp"
	icmpv6StatusOffset  = 28
	afInet6             = 23
)

var icmpStatusText = map[uint32]string{
	11002: "destination net unreachable",
	11003: "destination host unreachable",
	11004: "destination protocol unreachable",
	11005: "destination port unreachable",
	11010: "request timed out",
	11013: "TTL expired in transit",
	11050: "general failure",
}

func icmpError(ip net.IP, status uint32) error {
	if text, ok := icmpStatusText[status]; ok {
		return fmt.Errorf("%s: %s", ip, text)
	}
	return fmt.Errorf("%s: ICMP status %d", ip, status)
}

func pingOnce(_ context.Context, ip net.IP, timeout time.Duration) (time.Duration, error) {
	v4 := ip.To4()
	create := procIcmp6CreateFile
	if v4 != nil {
		create = procIcmpCreateFile
	}
	h, _, e := create.Call()
	if windows.Handle(h) == windows.InvalidHandle {
		return 0, fmt.Errorf("opening an ICMP handle: %w", e)
	}
	defer procIcmpCloseHandle.Call(h)

	payload := []byte(icmpPayload)
	reply := make([]byte, icmpReplyHeaderSize+len(payload)+8)
	ms := uintptr(timeout.Milliseconds())
	start := time.Now()
	var n uintptr
	if v4 != nil {
		addr := binary.LittleEndian.Uint32(v4)
		n, _, e = procIcmpSendEcho.Call(h, uintptr(addr), uintptr(unsafe.Pointer(&payload[0])), uintptr(len(payload)),
			0, uintptr(unsafe.Pointer(&reply[0])), uintptr(len(reply)), ms)
	} else {
		var src, dst [28]byte
		binary.LittleEndian.PutUint16(src[0:], afInet6)
		binary.LittleEndian.PutUint16(dst[0:], afInet6)
		copy(dst[8:24], ip.To16())
		n, _, e = procIcmp6SendEcho2.Call(h, 0, 0, 0, uintptr(unsafe.Pointer(&src[0])), uintptr(unsafe.Pointer(&dst[0])),
			uintptr(unsafe.Pointer(&payload[0])), uintptr(len(payload)), 0,
			uintptr(unsafe.Pointer(&reply[0])), uintptr(len(reply)), ms)
	}
	rtt := time.Since(start)
	if n == 0 {
		var errno windows.Errno
		if errors.As(e, &errno) {
			return 0, icmpError(ip, uint32(errno))
		}
		return 0, fmt.Errorf("%s: %w", ip, e)
	}
	statusOffset := 4
	if v4 == nil {
		statusOffset = icmpv6StatusOffset
	}
	if status := binary.LittleEndian.Uint32(reply[statusOffset:]); status != icmpStatusSuccess {
		return 0, icmpError(ip, status)
	}
	return rtt, nil
}

const (
	evtQueryChannelPath      = 0x1
	evtQueryReverseDirection = 0x200
	evtRenderContextSystem   = 1
	evtRenderEventValues     = 0
	evtFormatMessageEvent    = 1
	evtBatch                 = 64
	evtNextTimeoutMs         = 5000

	evtSystemProviderName = 0
	evtSystemEventID      = 2
	evtSystemLevel        = 3
	evtSystemTimeCreated  = 8

	evtVarTypeString   = 1
	evtVarTypeByte     = 4
	evtVarTypeUInt16   = 6
	evtVarTypeFileTime = 17
	evtVariantSize     = 16
)

func evtClose(h uintptr) {
	if h != 0 {
		procEvtClose.Call(h)
	}
}

func readEventLog(logName string, level uint8, count int) ([]eventRecord, error) {
	path, err := windows.UTF16PtrFromString(logName)
	if err != nil {
		return nil, err
	}
	xpath := "*"
	if level > 0 {
		xpath = fmt.Sprintf("*[System[(Level=%d)]]", level)
	}
	query, err := windows.UTF16PtrFromString(xpath)
	if err != nil {
		return nil, err
	}
	results, _, e := procEvtQuery.Call(0, uintptr(unsafe.Pointer(path)), uintptr(unsafe.Pointer(query)),
		evtQueryChannelPath|evtQueryReverseDirection)
	if results == 0 {
		return nil, fmt.Errorf("querying the log: %w", e)
	}
	defer evtClose(results)
	render, _, e := procEvtCreateRenderContext.Call(0, 0, evtRenderContextSystem)
	if render == 0 {
		return nil, fmt.Errorf("creating a render context: %w", e)
	}
	defer evtClose(render)

	publishers := map[string]uintptr{}
	defer func() {
		for _, h := range publishers {
			evtClose(h)
		}
	}()

	out := make([]eventRecord, 0, count)
	handles := make([]uintptr, evtBatch)
	for len(out) < count {
		var returned uint32
		want := min(evtBatch, count-len(out))
		r, _, e := procEvtNext.Call(results, uintptr(want), uintptr(unsafe.Pointer(&handles[0])),
			evtNextTimeoutMs, 0, uintptr(unsafe.Pointer(&returned)))
		if r == 0 {
			if errors.Is(e, windows.ERROR_NO_MORE_ITEMS) {
				break
			}
			return nil, fmt.Errorf("reading events: %w", e)
		}
		for _, ev := range handles[:returned] {
			rec, err := renderEvent(render, ev, publishers)
			evtClose(ev)
			if err != nil {
				return nil, err
			}
			out = append(out, rec)
		}
	}
	return out, nil
}

func renderEvent(render, ev uintptr, publishers map[string]uintptr) (eventRecord, error) {
	var used, props uint32
	procEvtRender.Call(render, ev, evtRenderEventValues, 0, 0, uintptr(unsafe.Pointer(&used)), uintptr(unsafe.Pointer(&props)))
	if used == 0 {
		return eventRecord{}, errors.New("rendering an event: empty size")
	}
	words := make([]uint64, (used+7)/8)
	if r, _, e := procEvtRender.Call(render, ev, evtRenderEventValues, uintptr(len(words)*8),
		uintptr(unsafe.Pointer(&words[0])), uintptr(unsafe.Pointer(&used)), uintptr(unsafe.Pointer(&props))); r == 0 {
		return eventRecord{}, fmt.Errorf("rendering an event: %w", e)
	}
	buf := unsafe.Slice((*byte)(unsafe.Pointer(&words[0])), len(words)*8)
	value := func(idx int, typ uint32) (uint64, bool) {
		off := idx * evtVariantSize
		if idx >= int(props) || off+evtVariantSize > len(buf) {
			return 0, false
		}
		if binary.LittleEndian.Uint32(buf[off+12:]) != typ {
			return 0, false
		}
		return binary.LittleEndian.Uint64(buf[off:]), true
	}

	var rec eventRecord
	if p, ok := value(evtSystemProviderName, evtVarTypeString); ok && p != 0 {
		rec.Provider = windows.UTF16PtrToString(*(**uint16)(unsafe.Pointer(&buf[evtSystemProviderName*evtVariantSize])))
	}
	if id, ok := value(evtSystemEventID, evtVarTypeUInt16); ok {
		rec.ID = uint16(id)
	}
	var level uint8
	if l, ok := value(evtSystemLevel, evtVarTypeByte); ok {
		level = uint8(l)
	}
	rec.Level = eventLevelName(level)
	if ft, ok := value(evtSystemTimeCreated, evtVarTypeFileTime); ok {
		rec.Time = time.Unix(0, (int64(ft)-116444736000000000)*100).UTC()
	}
	rec.Message = formatEventMessage(ev, rec.Provider, publishers)
	runtime.KeepAlive(words)
	return rec, nil
}

func formatEventMessage(ev uintptr, provider string, publishers map[string]uintptr) string {
	meta, ok := publishers[provider]
	if !ok {
		name, err := windows.UTF16PtrFromString(provider)
		if err != nil {
			return fmt.Sprintf("(message unavailable: %v)", err)
		}
		h, _, e := procEvtOpenPublisherMetadata.Call(0, uintptr(unsafe.Pointer(name)), 0, 0, 0)
		if h == 0 {
			publishers[provider] = 0
			return fmt.Sprintf("(message unavailable: opening publisher %s: %v)", provider, e)
		}
		publishers[provider] = h
		meta = h
	}
	if meta == 0 {
		return "(message unavailable: no publisher metadata for " + provider + ")"
	}
	var used uint32
	procEvtFormatMessage.Call(meta, ev, 0, 0, 0, evtFormatMessageEvent, 0, 0, uintptr(unsafe.Pointer(&used)))
	if used == 0 {
		return "(no message)"
	}
	msg := make([]uint16, used)
	if r, _, e := procEvtFormatMessage.Call(meta, ev, 0, 0, 0, evtFormatMessageEvent, uintptr(used),
		uintptr(unsafe.Pointer(&msg[0])), uintptr(unsafe.Pointer(&used))); r == 0 {
		return fmt.Sprintf("(message unavailable: %v)", e)
	}
	return windows.UTF16ToString(msg)
}
