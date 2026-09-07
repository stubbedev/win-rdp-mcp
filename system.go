package main

import (
	"context"
	"encoding/base64"
	json "encoding/json/v2"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	htmltomarkdown "github.com/JohannesKaufmann/html-to-markdown/v2"
	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/mem"
	psnet "github.com/shirou/gopsutil/v4/net"
	"github.com/shirou/gopsutil/v4/process"
)

// ── Shell ────────────────────────────────────────────────────────────────────

// maxShellTimeout bounds the caller-supplied timeout so one call cannot occupy
// a shell slot indefinitely.
const maxShellTimeout = 10 * time.Minute

func shellTool(ctx context.Context, args arguments) (toolResult, error) {
	command := args.stringOr("command", "")
	if command == "" {
		return toolResult{}, fmt.Errorf("command is required")
	}
	timeout := min(max(time.Duration(args.intOr("timeout", 30))*time.Second, time.Second), maxShellTimeout)

	if cwd := args.stringOr("cwd", ""); cwd != "" {
		command = "Set-Location -LiteralPath " + psQuote(cwd) + "; " + command
	}

	// A failing command is a result the model should read and react to, not a
	// protocol error — so the output is returned either way.
	out, err := runPowerShell(ctx, command, timeout)
	if err != nil {
		if out == "" {
			return toolResult{}, err
		}
		return toolResult{Content: []contentBlock{{Type: "text", Text: out}}, IsError: true}, nil
	}
	if out == "" {
		return textResult("(no output)"), nil
	}
	return textResult("%s", out), nil
}

// ── Processes and system info ────────────────────────────────────────────────

func listProcessesTool(ctx context.Context, args arguments) (toolResult, error) {
	filter := args.stringOr("filter", "")
	sortBy := args.stringOr("sort_by", "memory")
	limit := max(args.intOr("limit", 30), 1)

	procs, err := process.ProcessesWithContext(ctx)
	if err != nil {
		return toolResult{}, err
	}

	type row struct {
		pid    int32
		name   string
		cpu    float64
		memMB  float64
		status string
	}
	var rows []row
	for _, p := range procs {
		name, err := p.NameWithContext(ctx)
		if err != nil {
			// Processes exit while we iterate, and some refuse to be read;
			// either way there is nothing to report for this PID.
			continue
		}
		if filter != "" && partialRatio(strings.ToLower(filter), strings.ToLower(name)) < 60 {
			continue
		}
		r := row{pid: p.Pid, name: name}
		if c, err := p.CPUPercentWithContext(ctx); err == nil {
			r.cpu = c
		}
		if m, err := p.MemoryInfoWithContext(ctx); err == nil && m != nil {
			r.memMB = float64(m.RSS) / (1 << 20)
		}
		if s, err := p.StatusWithContext(ctx); err == nil {
			r.status = strings.Join(s, ",")
		}
		rows = append(rows, r)
	}
	if len(rows) == 0 {
		return textResult("No processes found."), nil
	}

	slices.SortFunc(rows, func(a, b row) int {
		switch sortBy {
		case "name":
			return strings.Compare(strings.ToLower(a.name), strings.ToLower(b.name))
		case "cpu":
			return cmpDesc(a.cpu, b.cpu)
		default:
			return cmpDesc(a.memMB, b.memMB)
		}
	})
	rows = rows[:min(len(rows), limit)]

	return textResult("%s", table(
		[]string{"PID", "Name", "CPU%", "Mem(MB)", "Status"},
		func(yield func([]string) bool) {
			for _, r := range rows {
				if !yield([]string{
					strconv.Itoa(int(r.pid)), r.name,
					strconv.FormatFloat(r.cpu, 'f', 1, 64),
					strconv.FormatFloat(r.memMB, 'f', 1, 64),
					r.status,
				}) {
					return
				}
			}
		})), nil
}

func cmpDesc(a, b float64) int {
	switch {
	case a > b:
		return -1
	case a < b:
		return 1
	}
	return 0
}

func killProcessTool(ctx context.Context, args arguments) (toolResult, error) {
	if pid := args.intOr("pid", 0); pid != 0 {
		p, err := process.NewProcessWithContext(ctx, int32(pid))
		if err != nil {
			return toolResult{}, fmt.Errorf("PID %d not found", pid)
		}
		name, _ := p.NameWithContext(ctx)
		if err := p.KillWithContext(ctx); err != nil {
			return toolResult{}, fmt.Errorf("killing PID %d: %w", pid, err)
		}
		return textResult("Killed PID %d (%s)", pid, name), nil
	}

	name := args.stringOr("name", "")
	if name == "" {
		return toolResult{}, fmt.Errorf("provide pid or name")
	}
	procs, err := process.ProcessesWithContext(ctx)
	if err != nil {
		return toolResult{}, err
	}

	var killed []string
	for _, p := range procs {
		procName, err := p.NameWithContext(ctx)
		if err != nil {
			continue
		}
		// Exact, not fuzzy. Upstream matched at a similarity threshold, which
		// scores "notepad" against "notepad++" at 87 — high enough to kill a
		// process the user never named. Killing is not undoable, so the only
		// latitude here is the .exe suffix.
		if !sameProcessName(name, procName) {
			continue
		}
		if err := p.KillWithContext(ctx); err == nil {
			killed = append(killed, fmt.Sprintf("%s (PID %d)", procName, p.Pid))
		}
	}
	if len(killed) == 0 {
		return toolResult{}, fmt.Errorf("no process matching %q", name)
	}
	return textResult("Killed: %s", strings.Join(killed, ", ")), nil
}

// sameProcessName compares process names case-insensitively, treating "chrome"
// and "chrome.exe" as the same program and nothing else as a match.
func sameProcessName(want, actual string) bool {
	want = strings.TrimSuffix(strings.ToLower(want), ".exe")
	actual = strings.TrimSuffix(strings.ToLower(actual), ".exe")
	return want != "" && want == actual
}

func systemInfoTool(ctx context.Context, args arguments) (toolResult, error) {
	var b strings.Builder

	if info, err := host.InfoWithContext(ctx); err == nil {
		fmt.Fprintf(&b, "**System:** %s %s (%s)\n", info.Platform, info.PlatformVersion, info.KernelArch)
		boot := time.Unix(int64(info.BootTime), 0)
		fmt.Fprintf(&b, "**Uptime:** %s (boot: %s)\n",
			(time.Duration(info.Uptime) * time.Second).Round(time.Second), boot.Format("2006-01-02 15:04"))
	}
	// A short sampling window is needed for a meaningful percentage; the
	// alternative is a first reading of zero.
	if pct, err := cpu.PercentWithContext(ctx, 500*time.Millisecond, false); err == nil && len(pct) > 0 {
		count, _ := cpu.CountsWithContext(ctx, true)
		fmt.Fprintf(&b, "**CPU:** %.1f%% (%d logical cores)\n", pct[0], count)
	}
	if vm, err := mem.VirtualMemoryWithContext(ctx); err == nil {
		fmt.Fprintf(&b, "**Memory:** %.1f%% — %dMB / %dMB\n",
			vm.UsedPercent, vm.Used>>20, vm.Total>>20)
	}
	root := "/"
	if runtime.GOOS == "windows" {
		root = `C:\`
	}
	if du, err := disk.UsageWithContext(ctx, root); err == nil {
		fmt.Fprintf(&b, "**Disk (%s):** %.1f%% — %dGB / %dGB\n",
			root, du.UsedPercent, du.Used>>30, du.Total>>30)
	}
	if io, err := psnet.IOCountersWithContext(ctx, false); err == nil && len(io) > 0 {
		fmt.Fprintf(&b, "**Network:** sent %dMB / received %dMB\n", io[0].BytesSent>>20, io[0].BytesRecv>>20)
	}

	if b.Len() == 0 {
		return toolResult{}, fmt.Errorf("no system information available")
	}
	return textResult("%s", strings.TrimRight(b.String(), "\n")), nil
}

// ── Services, scheduled tasks, event log ─────────────────────────────────────
//
// ponytail: these four shell out to PowerShell exactly as the upstream server
// does. The Win32 service APIs would avoid a subprocess, but Get-ScheduledTask
// and Get-WinEvent have no equivalent worth hand-rolling, and a consistent
// formatting story across all of them is worth more than one saved fork.

func serviceListTool(ctx context.Context, args arguments) (toolResult, error) {
	cmd := "Get-Service"
	if f := args.stringOr("filter", ""); f != "" {
		cmd = fmt.Sprintf(`$f = %s; Get-Service | Where-Object { $_.DisplayName -like "*$f*" -or $_.Name -like "*$f*" }`, psQuote(f))
	}
	return psTable(ctx, cmd+" | Format-Table Name, DisplayName, Status -AutoSize", 30*time.Second)
}

func serviceStartTool(ctx context.Context, args arguments) (toolResult, error) {
	return psTable(ctx, "Start-Service -Name "+psQuote(args.stringOr("name", ""))+
		" -PassThru | Format-Table Name, Status -AutoSize", 60*time.Second)
}

func serviceStopTool(ctx context.Context, args arguments) (toolResult, error) {
	return psTable(ctx, "Stop-Service -Name "+psQuote(args.stringOr("name", ""))+
		" -Force -PassThru | Format-Table Name, Status -AutoSize", 60*time.Second)
}

func taskListTool(ctx context.Context, args arguments) (toolResult, error) {
	cmd := "Get-ScheduledTask"
	if f := args.stringOr("filter", ""); f != "" {
		cmd = fmt.Sprintf(`$f = %s; Get-ScheduledTask | Where-Object { $_.TaskName -like "*$f*" }`, psQuote(f))
	}
	return psTable(ctx, cmd+" | Format-Table TaskName, State, TaskPath -AutoSize", 60*time.Second)
}

// validSchedules is closed on purpose: /SC takes a fixed vocabulary, and
// rejecting anything else here keeps unvetted text out of the schtasks line.
var validSchedules = []string{"ONCE", "DAILY", "WEEKLY", "MONTHLY", "ONSTART", "ONLOGON", "ONIDLE"}

func taskCreateTool(ctx context.Context, args arguments) (toolResult, error) {
	schedule := strings.ToUpper(args.stringOr("schedule", ""))
	if !slices.Contains(validSchedules, schedule) {
		return toolResult{}, fmt.Errorf("unknown schedule %q (use one of %s)", schedule, strings.Join(validSchedules, ", "))
	}
	out, err := runCommand(ctx, 60*time.Second, "schtasks",
		"/Create", "/TN", args.stringOr("name", ""), "/TR", args.stringOr("command", ""), "/SC", schedule, "/F")
	if err != nil {
		return toolResult{}, err
	}
	return textResult("%s", out), nil
}

func taskDeleteTool(ctx context.Context, args arguments) (toolResult, error) {
	out, err := runCommand(ctx, 30*time.Second, "schtasks", "/Delete", "/TN", args.stringOr("name", ""), "/F")
	if err != nil {
		return toolResult{}, err
	}
	return textResult("%s", out), nil
}

var eventLevels = map[string]int{
	"critical": 1, "error": 2, "warning": 3, "information": 4, "verbose": 5,
}

func eventLogTool(ctx context.Context, args arguments) (toolResult, error) {
	logName := psQuote(args.stringOr("log_name", "System"))
	count := min(max(args.intOr("count", 20), 1), 1000)

	cmd := fmt.Sprintf("Get-WinEvent -LogName %s -MaxEvents %d", logName, count)
	if level := strings.ToLower(args.stringOr("level", "")); level != "" {
		num, ok := eventLevels[level]
		if !ok {
			return toolResult{}, fmt.Errorf("unknown level %q", level)
		}
		cmd = fmt.Sprintf("Get-WinEvent -FilterHashtable @{LogName=%s;Level=%d} -MaxEvents %d", logName, num, count)
	}
	return psTable(ctx, cmd+" | Format-Table TimeCreated, Id, LevelDisplayName, Message -AutoSize -Wrap", 60*time.Second)
}

func psTable(ctx context.Context, command string, timeout time.Duration) (toolResult, error) {
	out, err := runPowerShell(ctx, command, timeout)
	if err != nil {
		return toolResult{}, err
	}
	if out == "" {
		return textResult("(no output)"), nil
	}
	return textResult("%s", out), nil
}

// ── Network ──────────────────────────────────────────────────────────────────

func pingTool(ctx context.Context, args arguments) (toolResult, error) {
	host := args.stringOr("host", "")
	count := min(max(args.intOr("count", 4), 1), 20)

	// The count flag differs between Windows and everywhere else, and the
	// server is testable on Linux, so pick by platform rather than assuming.
	flag := "-c"
	if runtime.GOOS == "windows" {
		flag = "-n"
	}
	timeout := time.Duration(count*5+10) * time.Second
	out, err := runCommand(ctx, timeout, "ping", flag, strconv.Itoa(count), host)
	if err != nil && out == "" {
		return toolResult{}, err
	}
	return textResult("%s", out), nil
}

func portCheckTool(ctx context.Context, args arguments) (toolResult, error) {
	target := args.stringOr("host", "")
	port := args.intOr("port", 0)
	timeout := time.Duration(min(max(args.floatOr("timeout", 5.0), 0.1), 60) * float64(time.Second))

	address := net.JoinHostPort(target, strconv.Itoa(port))
	dialer := net.Dialer{Timeout: timeout}
	conn, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return textResult("Port %d on %s is CLOSED (%v)", port, target, err), nil
	}
	conn.Close()
	return textResult("Port %d on %s is OPEN", port, target), nil
}

func netConnectionsTool(ctx context.Context, args arguments) (toolResult, error) {
	filter := strings.ToLower(args.stringOr("filter", ""))
	limit := max(args.intOr("limit", 50), 1)

	conns, err := psnet.ConnectionsWithContext(ctx, "inet")
	if err != nil {
		return toolResult{}, err
	}

	var rows [][]string
	for _, c := range conns {
		local := formatAddr(c.Laddr.IP, c.Laddr.Port)
		remote := formatAddr(c.Raddr.IP, c.Raddr.Port)
		pid := ""
		if c.Pid != 0 {
			pid = strconv.Itoa(int(c.Pid))
		}
		if filter != "" && !strings.Contains(strings.ToLower(local+" "+remote+" "+c.Status+" "+pid), filter) {
			continue
		}
		rows = append(rows, []string{local, remote, c.Status, pid})
		if len(rows) >= limit {
			break
		}
	}
	if len(rows) == 0 {
		return textResult("No connections found."), nil
	}
	return textResult("%s", table([]string{"Local", "Remote", "Status", "PID"}, slices.Values(rows))), nil
}

func formatAddr(ip string, port uint32) string {
	if ip == "" {
		return ""
	}
	return net.JoinHostPort(ip, strconv.Itoa(int(port)))
}

// ── Scrape ───────────────────────────────────────────────────────────────────

const (
	maxScrapeBytes    = 1 << 20
	maxScrapeMarkdown = 50000
)

func scrapeTool(ctx context.Context, args arguments) (toolResult, error) {
	body, err := fetchValidated(ctx, args.stringOr("url", ""),
		http.Header{"User-Agent": []string{"win-rdp-mcp/" + Version}}, maxScrapeBytes)
	if err != nil {
		return toolResult{}, err
	}
	md, err := htmltomarkdown.ConvertString(string(body))
	if err != nil {
		return toolResult{}, err
	}
	if len(md) > maxScrapeMarkdown {
		md = md[:maxScrapeMarkdown] + "\n\n[... truncated]"
	}
	return textResult("%s", md), nil
}

// ── Audio ────────────────────────────────────────────────────────────────────

const maxSoundBytes = 10 << 20

func playSoundTool(ctx context.Context, args arguments) (toolResult, error) {
	path := args.stringOr("path", "")
	url := args.stringOr("url", "")
	if path == "" && url == "" {
		return toolResult{}, fmt.Errorf("provide either 'path' (local file) or 'url' (remote file)")
	}

	if path == "" {
		data, err := fetchValidated(ctx, url, nil, maxSoundBytes)
		if err != nil {
			return toolResult{}, err
		}
		suffix := ".wav"
		for _, ext := range []string{".mp3", ".ogg", ".wma", ".m4a"} {
			if strings.Contains(strings.ToLower(url), ext) {
				suffix = ext
				break
			}
		}
		tmp, err := os.CreateTemp("", "win-rdp-mcp-sound-*"+suffix)
		if err != nil {
			return toolResult{}, err
		}
		defer os.Remove(tmp.Name())
		if _, err := tmp.Write(data); err != nil {
			tmp.Close()
			return toolResult{}, err
		}
		tmp.Close()
		path = tmp.Name()
	}

	var script string
	var timeout time.Duration
	switch strings.ToLower(filepath.Ext(path)) {
	case ".mp3", ".ogg", ".wma", ".m4a":
		// SoundPlayer only understands WAV; compressed formats go through the
		// WPF MediaPlayer, polled until it reaches the end of the track.
		script = "Add-Type -AssemblyName presentationCore; " +
			"$p = New-Object System.Windows.Media.MediaPlayer; " +
			"$p.Open([uri]" + psQuote(path) + "); $p.Play(); Start-Sleep -Milliseconds 500; " +
			"while ($p.NaturalDuration.HasTimeSpan -and $p.Position -lt $p.NaturalDuration.TimeSpan) " +
			"{ Start-Sleep -Milliseconds 200 }; $p.Close()"
		timeout = 2 * time.Minute
	default:
		script = "(New-Object System.Media.SoundPlayer " + psQuote(path) + ").PlaySync()"
		timeout = 30 * time.Second
	}
	if _, err := runPowerShell(ctx, script, timeout); err != nil {
		return toolResult{}, err
	}
	return textResult("Played: %s", path), nil
}

// ── Files ────────────────────────────────────────────────────────────────────

const (
	maxTextRead     = 100_000
	maxUploadBase64 = 100 << 20 // roughly 75 MB decoded
)

func fileReadTool(_ context.Context, args arguments) (toolResult, error) {
	path := args.stringOr("path", "")
	data, err := os.ReadFile(path)
	if err != nil {
		return toolResult{}, err
	}
	if args.stringOr("encoding", "utf-8") == "binary" {
		return textResult("%s", base64.StdEncoding.EncodeToString(data)), nil
	}
	text := string(data)
	if len(text) > maxTextRead {
		text = text[:maxTextRead] + "\n\n[... truncated at 100KB]"
	}
	return textResult("%s", text), nil
}

func fileWriteTool(_ context.Context, args arguments) (toolResult, error) {
	path := args.stringOr("path", "")
	content := args.stringOr("content", "")

	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return toolResult{}, err
		}
	}
	flags := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	if args.boolOr("append", false) {
		flags = os.O_CREATE | os.O_WRONLY | os.O_APPEND
	}
	f, err := os.OpenFile(path, flags, 0o644)
	if err != nil {
		return toolResult{}, err
	}
	defer f.Close()
	if _, err := f.WriteString(content); err != nil {
		return toolResult{}, err
	}
	return textResult("Wrote %d characters to %s", len([]rune(content)), path), nil
}

func fileListTool(_ context.Context, args arguments) (toolResult, error) {
	path := args.stringOr("path", ".")
	showHidden := args.boolOr("show_hidden", false)

	entries, err := os.ReadDir(path)
	if err != nil {
		return toolResult{}, err
	}

	var rows [][]string
	for _, e := range entries {
		if !showHidden && strings.HasPrefix(e.Name(), ".") {
			continue
		}
		kind, size, modified := "FILE", "?", "?"
		if e.IsDir() {
			kind, size = "DIR", "<DIR>"
		}
		if info, err := e.Info(); err == nil {
			modified = info.ModTime().Format("2006-01-02 15:04")
			if !e.IsDir() {
				size = humanBytes(info.Size())
			}
		}
		rows = append(rows, []string{kind, e.Name(), size, modified})
	}
	if len(rows) == 0 {
		return textResult("Directory is empty."), nil
	}
	return textResult("%s", table([]string{"Type", "Name", "Size", "Modified"}, slices.Values(rows))), nil
}

func humanBytes(n int64) string {
	switch {
	case n < 1<<10:
		return fmt.Sprintf("%dB", n)
	case n < 1<<20:
		return fmt.Sprintf("%dKB", n>>10)
	case n < 1<<30:
		return fmt.Sprintf("%dMB", n>>20)
	}
	return fmt.Sprintf("%dGB", n>>30)
}

func fileSearchTool(ctx context.Context, args arguments) (toolResult, error) {
	pattern := args.stringOr("pattern", "")
	root := args.stringOr("path", ".")
	limit := max(args.intOr("limit", 50), 1)

	// Reject a bad pattern up front rather than walking the tree to discover it
	// matches nothing.
	if _, err := filepath.Match(pattern, ""); err != nil {
		return toolResult{}, fmt.Errorf("invalid pattern %q: %w", pattern, err)
	}

	type hit struct {
		path string
		size int64
	}
	var hits []hit
	truncated := false

	walk := func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// An unreadable subdirectory is normal on a Windows box; skip it
			// rather than abandoning the whole search.
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() {
			if !args.boolOr("recursive", true) && path != root {
				return fs.SkipDir
			}
			return nil
		}
		if ok, _ := filepath.Match(pattern, d.Name()); !ok {
			return nil
		}
		if len(hits) >= limit {
			truncated = true
			return fs.SkipAll
		}
		var size int64
		if info, err := d.Info(); err == nil {
			size = info.Size()
		}
		hits = append(hits, hit{path: path, size: size})
		return nil
	}
	if err := filepath.WalkDir(root, walk); err != nil {
		return toolResult{}, err
	}
	if len(hits) == 0 {
		return textResult("No files matching %q in %s", pattern, root), nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Found %d files", len(hits))
	if truncated {
		fmt.Fprintf(&b, " (stopped at the limit of %d)", limit)
	}
	b.WriteString(":\n")
	for _, h := range hits {
		fmt.Fprintf(&b, "  %s (%d bytes)\n", h.path, h.size)
	}
	return textResult("%s", strings.TrimRight(b.String(), "\n")), nil
}

func fileDownloadTool(_ context.Context, args arguments) (toolResult, error) {
	path := args.stringOr("path", "")
	data, err := os.ReadFile(path)
	if err != nil {
		return toolResult{}, err
	}
	return textResult("base64:%dbytes:%s", len(data), base64.StdEncoding.EncodeToString(data)), nil
}

func fileUploadTool(_ context.Context, args arguments) (toolResult, error) {
	path := args.stringOr("path", "")
	encoded := args.stringOr("data_base64", "")
	if len(encoded) > maxUploadBase64 {
		return toolResult{}, fmt.Errorf("data exceeds the maximum size of %s base64", humanBytes(maxUploadBase64))
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return toolResult{}, fmt.Errorf("data_base64 is not valid base64: %w", err)
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return toolResult{}, err
		}
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return toolResult{}, err
	}
	return textResult("Wrote %d bytes to %s", len(data), path), nil
}

// ── Task introspection ───────────────────────────────────────────────────────

func cancelTaskTool(_ context.Context, args arguments) (toolResult, error) {
	task, err := tasks.cancelTask(args.stringOr("task_id", ""))
	if err != nil {
		return toolResult{}, err
	}
	return textResult("Cancelled task %s (%s)", task.ID, task.Tool), nil
}

func getTaskStatusTool(_ context.Context, args arguments) (toolResult, error) {
	if id := args.stringOr("task_id", ""); id != "" {
		task, ok := tasks.get(id)
		if !ok {
			return toolResult{}, fmt.Errorf("task %s not found", id)
		}
		encoded, err := json.Marshal(task, json.FormatNilSliceAsNull(false))
		if err != nil {
			return toolResult{}, err
		}
		return textResult("%s", encoded), nil
	}

	listed := tasks.list("")
	if len(listed) == 0 {
		return textResult("No tasks in history."), nil
	}
	var b strings.Builder
	b.WriteString("Recent tasks:\n")
	for _, t := range listed[:min(len(listed), 20)] {
		fmt.Fprintf(&b, "  [%s] %s → %s%s%s\n", t.ID, t.Tool, t.Status, formatDuration(t.Duration), formatErr(t.Error))
	}
	return textResult("%s", strings.TrimRight(b.String(), "\n")), nil
}

func getRunningTasksTool(_ context.Context, _ arguments) (toolResult, error) {
	active := append(tasks.list(statusRunning), tasks.list(statusPending)...)
	if len(active) == 0 {
		return textResult("No active tasks."), nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Active tasks (%d):\n", len(active))
	for _, t := range active {
		fmt.Fprintf(&b, "  [%s] %s [%s] %s%s\n", t.ID, t.Tool, t.Category, t.Status, formatDuration(t.Duration))
	}
	return textResult("%s", strings.TrimRight(b.String(), "\n")), nil
}

func formatDuration(d *float64) string {
	if d == nil {
		return ""
	}
	return fmt.Sprintf(" (%.2fs)", *d)
}

func formatErr(msg string) string {
	if msg == "" {
		return ""
	}
	return " — " + msg
}

// ── Table rendering ──────────────────────────────────────────────────────────

// table renders aligned columns. Rows arrive as an iterator so a caller can
// stream them without first materialising a slice of slices.
func table(headers []string, rows func(func([]string) bool)) string {
	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)

	fmt.Fprintln(w, strings.Join(headers, "\t"))
	fmt.Fprintln(w, strings.Join(underlines(headers), "\t"))
	for row := range rows {
		fmt.Fprintln(w, strings.Join(row, "\t"))
	}
	w.Flush()
	return strings.TrimRight(b.String(), "\n")
}

func underlines(headers []string) []string {
	out := make([]string, len(headers))
	for i, h := range headers {
		out[i] = strings.Repeat("-", len(h))
	}
	return out
}
