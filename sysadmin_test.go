package main

import (
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

func TestSplitCommandLine(t *testing.T) {
	cases := map[string]commandLine{
		`notepad.exe`:                               {Path: "notepad.exe"},
		`C:\tools\run.exe --flag value`:             {Path: `C:\tools\run.exe`, Args: "--flag value"},
		`"C:\Program Files\App\app.exe" -x "a b"`:   {Path: `C:\Program Files\App\app.exe`, Args: `-x "a b"`},
		`  "C:\Program Files\App\app.exe"  `:        {Path: `C:\Program Files\App\app.exe`},
		`powershell -NoProfile -Command "Get-Date"`: {Path: "powershell", Args: `-NoProfile -Command "Get-Date"`},
	}
	for in, want := range cases {
		got, err := splitCommandLine(in)
		if err != nil || got != want {
			t.Errorf("splitCommandLine(%q) = %+v, %v; want %+v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "   ", `"C:\unterminated`, `"" args`} {
		if _, err := splitCommandLine(bad); err == nil {
			t.Errorf("splitCommandLine(%q) accepted", bad)
		}
	}
}

func TestParseTaskTrigger(t *testing.T) {
	if got, err := parseTaskTrigger("daily"); err != nil || got != triggerDaily {
		t.Fatalf("daily = %v, %v", got, err)
	}
	_, err := parseTaskTrigger("HOURLY")
	if err == nil || !strings.Contains(err.Error(), "DAILY, MONTHLY, ONCE, ONIDLE, ONLOGON, ONSTART, WEEKLY") {
		t.Fatalf("unknown schedule error = %v", err)
	}
}

func TestEventLevelName(t *testing.T) {
	for level, want := range map[uint8]string{0: "Information", 2: "Error", 3: "Warning", 9: "Level 9"} {
		if got := eventLevelName(level); got != want {
			t.Errorf("eventLevelName(%d) = %q, want %q", level, got, want)
		}
	}
}

func TestFormatPing(t *testing.T) {
	ip := net.ParseIP("192.0.2.7").To4()
	out := formatPing("box", ip, []pingReply{
		{RTT: 2 * time.Millisecond},
		{Err: errors.New("192.0.2.7: request timed out")},
		{RTT: 4 * time.Millisecond},
		{RTT: 300 * time.Microsecond},
	})
	for _, want := range []string{
		"Pinging box [192.0.2.7]:",
		"Reply from 192.0.2.7: time=2ms",
		"Reply from 192.0.2.7: time<1ms",
		"request timed out",
		"sent 4, received 3, lost 1 (25% loss)",
		"min<1ms, max=4ms, avg=2.1ms",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("ping report lacks %q:\n%s", want, out)
		}
	}
	allLost := formatPing("192.0.2.7", ip, []pingReply{{Err: errors.New("x")}})
	if strings.Contains(allLost, "Round trip") || !strings.HasPrefix(allLost, "Pinging 192.0.2.7:") {
		t.Errorf("all-lost report:\n%s", allLost)
	}
}

func TestListResult(t *testing.T) {
	empty := listResult([]serviceInfo(nil), "No services found.", []string{"Name"}, func(s serviceInfo) []string { return []string{s.Name} })
	if got := firstText(empty.Content); got != "No services found." {
		t.Fatalf("empty = %q", got)
	}
	full := listResult([]serviceInfo{{Name: "Spooler", DisplayName: "Print Spooler", State: "Running"}},
		"", []string{"Name", "Status"}, func(s serviceInfo) []string { return []string{s.Name, s.State} })
	if got := firstText(full.Content); !strings.Contains(got, "Spooler") || !strings.Contains(got, "Running") {
		t.Fatalf("table = %q", got)
	}
}
