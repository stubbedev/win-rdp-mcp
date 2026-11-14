package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"iter"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"
)

type serviceInfo struct {
	Name, DisplayName, State string
}

type scheduledTask struct {
	Name, Path, State string
}

type eventRecord struct {
	Time     time.Time
	ID       uint16
	Level    string
	Provider string
	Message  string
}

type taskTrigger int

const (
	triggerOnce taskTrigger = iota + 1
	triggerDaily
	triggerWeekly
	triggerMonthly
	triggerOnStart
	triggerOnLogon
	triggerOnIdle
)

var taskTriggers = map[string]taskTrigger{
	"ONCE": triggerOnce, "DAILY": triggerDaily, "WEEKLY": triggerWeekly, "MONTHLY": triggerMonthly,
	"ONSTART": triggerOnStart, "ONLOGON": triggerOnLogon, "ONIDLE": triggerOnIdle,
}

func parseTaskTrigger(s string) (taskTrigger, error) {
	t, ok := taskTriggers[strings.ToUpper(s)]
	if !ok {
		names := make([]string, 0, len(taskTriggers))
		for n := range taskTriggers {
			names = append(names, n)
		}
		slices.Sort(names)
		return 0, fmt.Errorf("unknown schedule %q (use one of %s)", s, strings.Join(names, ", "))
	}
	return t, nil
}

type commandLine struct {
	Path, Args string
}

func splitCommandLine(s string) (commandLine, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return commandLine{}, errors.New("command is empty")
	}
	if rest, ok := strings.CutPrefix(s, `"`); ok {
		path, args, closed := strings.Cut(rest, `"`)
		if !closed || path == "" {
			return commandLine{}, fmt.Errorf("command %q has an unterminated or empty quoted path", s)
		}
		return commandLine{Path: path, Args: strings.TrimSpace(args)}, nil
	}
	path, args, _ := strings.Cut(s, " ")
	return commandLine{Path: path, Args: strings.TrimSpace(args)}, nil
}

var eventLevels = map[string]uint8{
	"critical": 1, "error": 2, "warning": 3, "information": 4, "verbose": 5,
}

var eventLevelNames = map[uint8]string{
	0: "Information", 1: "Critical", 2: "Error", 3: "Warning", 4: "Information", 5: "Verbose",
}

func eventLevelName(level uint8) string {
	if n, ok := eventLevelNames[level]; ok {
		return n
	}
	return "Level " + strconv.Itoa(int(level))
}

func containsFold(s, sub string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}

func listResult[T any](items []T, empty string, headers []string, row func(T) []string) toolResult {
	if len(items) == 0 {
		return textResult("%s", empty)
	}
	rows := func(yield func([]string) bool) {
		for _, it := range items {
			if !yield(row(it)) {
				return
			}
		}
	}
	return textResult("%s", table(headers, iter.Seq[[]string](rows)))
}

func serviceListTool(_ context.Context, args arguments) (toolResult, error) {
	services, err := listServices()
	if err != nil {
		return toolResult{}, fmt.Errorf("listing services: %w", err)
	}
	if f := args.stringOr("filter", ""); f != "" {
		services = slices.DeleteFunc(services, func(s serviceInfo) bool {
			return !containsFold(s.Name, f) && !containsFold(s.DisplayName, f)
		})
	}
	slices.SortFunc(services, func(a, b serviceInfo) int { return cmp.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)) })
	return listResult(services, "No services found.", []string{"Name", "DisplayName", "Status"},
		func(s serviceInfo) []string { return []string{s.Name, s.DisplayName, s.State} }), nil
}

const serviceControlTimeout = 60 * time.Second

func serviceStartTool(ctx context.Context, args arguments) (toolResult, error) {
	name := args.stringOr("name", "")
	ctx, cancel := context.WithTimeout(ctx, serviceControlTimeout)
	defer cancel()
	state, err := startService(ctx, name)
	if err != nil {
		return toolResult{}, fmt.Errorf("starting service %s: %w", name, err)
	}
	return textResult("Service %s is %s", name, state), nil
}

func serviceStopTool(ctx context.Context, args arguments) (toolResult, error) {
	name := args.stringOr("name", "")
	ctx, cancel := context.WithTimeout(ctx, serviceControlTimeout)
	defer cancel()
	state, err := stopService(ctx, name)
	if err != nil {
		return toolResult{}, fmt.Errorf("stopping service %s: %w", name, err)
	}
	return textResult("Service %s is %s", name, state), nil
}

func taskListTool(_ context.Context, args arguments) (toolResult, error) {
	tasks, err := listTasks()
	if err != nil {
		return toolResult{}, fmt.Errorf("listing scheduled tasks: %w", err)
	}
	if f := args.stringOr("filter", ""); f != "" {
		tasks = slices.DeleteFunc(tasks, func(t scheduledTask) bool { return !containsFold(t.Name, f) })
	}
	slices.SortFunc(tasks, func(a, b scheduledTask) int { return cmp.Compare(a.Path, b.Path) })
	return listResult(tasks, "No scheduled tasks found.", []string{"TaskName", "State", "TaskPath"},
		func(t scheduledTask) []string { return []string{t.Name, t.State, t.Path} }), nil
}

func taskCreateTool(_ context.Context, args arguments) (toolResult, error) {
	name := args.stringOr("name", "")
	trigger, err := parseTaskTrigger(args.stringOr("schedule", ""))
	if err != nil {
		return toolResult{}, err
	}
	command, err := splitCommandLine(args.stringOr("command", ""))
	if err != nil {
		return toolResult{}, err
	}
	if err := createTask(name, command, trigger); err != nil {
		return toolResult{}, fmt.Errorf("creating scheduled task %s: %w", name, err)
	}
	return textResult("Scheduled task %q created", name), nil
}

func taskDeleteTool(_ context.Context, args arguments) (toolResult, error) {
	name := args.stringOr("name", "")
	if err := deleteTask(name); err != nil {
		return toolResult{}, fmt.Errorf("deleting scheduled task %s: %w", name, err)
	}
	return textResult("Scheduled task %q deleted", name), nil
}

func eventLogTool(_ context.Context, args arguments) (toolResult, error) {
	logName := args.stringOr("log_name", "System")
	count := min(max(args.intOr("count", 20), 1), 1000)
	var level uint8
	if name := strings.ToLower(args.stringOr("level", "")); name != "" {
		l, ok := eventLevels[name]
		if !ok {
			return toolResult{}, fmt.Errorf("unknown level %q", name)
		}
		level = l
	}
	events, err := readEventLog(logName, level, count)
	if err != nil {
		return toolResult{}, fmt.Errorf("reading the %s event log: %w", logName, err)
	}
	return listResult(events, "No events found.", []string{"TimeCreated", "Id", "Level", "Provider", "Message"},
		func(e eventRecord) []string {
			return []string{e.Time.UTC().Format(time.RFC3339), strconv.Itoa(int(e.ID)), e.Level, e.Provider,
				strings.Join(strings.Fields(e.Message), " ")}
		}), nil
}

const (
	pingTimeout  = 4 * time.Second
	pingInterval = time.Second
)

type pingReply struct {
	RTT time.Duration
	Err error
}

func pingTool(ctx context.Context, args arguments) (toolResult, error) {
	host := args.stringOr("host", "")
	count := min(max(args.intOr("count", 4), 1), 20)
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
	if err != nil {
		return toolResult{}, fmt.Errorf("resolving %s: %w", host, err)
	}
	ip := ips[0]
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	replies := make([]pingReply, 0, count)
	for i := range count {
		if i > 0 && !sleepCtx(ctx, pingInterval) {
			return toolResult{}, ctx.Err()
		}
		rtt, err := pingOnce(ctx, ip, pingTimeout)
		replies = append(replies, pingReply{RTT: rtt, Err: err})
	}
	return textResult("%s", formatPing(host, ip, replies)), nil
}

func formatPing(host string, ip net.IP, replies []pingReply) string {
	var b strings.Builder
	target := ip.String()
	if host != target {
		target = fmt.Sprintf("%s [%s]", host, ip)
	}
	fmt.Fprintf(&b, "Pinging %s:\n", target)
	var received int
	var minRTT, maxRTT, total time.Duration
	for _, r := range replies {
		if r.Err != nil {
			fmt.Fprintf(&b, "  %v\n", r.Err)
			continue
		}
		if received == 0 || r.RTT < minRTT {
			minRTT = r.RTT
		}
		maxRTT = max(maxRTT, r.RTT)
		total += r.RTT
		received++
		fmt.Fprintf(&b, "  Reply from %s: time=%s\n", ip, r.RTT.Round(time.Microsecond))
	}
	lost := len(replies) - received
	fmt.Fprintf(&b, "Packets: sent %d, received %d, lost %d (%d%% loss)", len(replies), received, lost, lost*100/len(replies))
	if received > 0 {
		fmt.Fprintf(&b, "\nRound trip: min %s, max %s, avg %s",
			minRTT.Round(time.Microsecond), maxRTT.Round(time.Microsecond), (total / time.Duration(received)).Round(time.Microsecond))
	}
	return b.String()
}
