package main

import (
	"fmt"
	"sort"
	"strings"
)

// Tool tiers mirror the upstream winremote-mcp risk split: tier 1 is read-only
// observation, tier 2 drives the desktop, tier 3 can change or destroy state.
// Tier 1+2 are on by default; tier 3 has to be asked for.
var (
	tier1 = []string{
		"Snapshot", "AnnotatedSnapshot", "GetClipboard", "GetSystemInfo",
		"ListProcesses", "FileList", "FileSearch", "RegRead", "ServiceList",
		"TaskList", "EventLog", "Ping", "PortCheck", "NetConnections", "OCR",
		"ScreenRecord", "Notification", "Wait", "GetTaskStatus", "GetRunningTasks",
	}
	tier2 = []string{
		"Click", "Type", "Move", "Scroll", "Shortcut", "FocusWindow",
		"MinimizeAll", "Scrape", "CancelTask", "ReconnectSession",
	}
	tier3 = []string{
		"Shell", "App", "PlaySound", "FileRead", "FileWrite", "FileDownload",
		"FileUpload", "KillProcess", "RegWrite", "ServiceStart", "ServiceStop",
		"TaskCreate", "TaskDelete", "SetClipboard", "LockScreen",
	}
)

// allTools is every tool name across the three tiers; lookupTool resolves a
// user-typed name case-insensitively onto its canonical spelling.
var (
	allTools   = set(tier1, tier2, tier3)
	lookupTool = func() map[string]string {
		m := map[string]string{}
		for name := range allTools {
			m[strings.ToLower(name)] = name
		}
		return m
	}()
)

func set(groups ...[]string) map[string]bool {
	out := map[string]bool{}
	for _, g := range groups {
		for _, name := range g {
			out[name] = true
		}
	}
	return out
}

func sortedNames(s map[string]bool) []string {
	out := make([]string, 0, len(s))
	for name := range s {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// parseToolCSV splits a comma-separated tool list, dropping empty entries.
func parseToolCSV(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// normalizeToolNames maps names onto their canonical spelling and rejects the
// whole list if any entry is unknown — a typo in --tools must not silently
// enable a smaller surface than the operator asked for.
func normalizeToolNames(names []string) ([]string, error) {
	var out, unknown []string
	for _, n := range names {
		if hit, ok := lookupTool[strings.ToLower(n)]; ok {
			out = append(out, hit)
		} else {
			unknown = append(unknown, n)
		}
	}
	if len(unknown) > 0 {
		return nil, fmt.Errorf("unknown tools: %s. Allowed tools: %s",
			strings.Join(unknown, ", "), strings.Join(sortedNames(allTools), ", "))
	}
	return out, nil
}

type toolSelection struct {
	enableTier3  bool
	disableTier2 bool
	enableAll    bool
	explicit     []string
	exclude      []string
}

// resolveEnabledTools applies the selection. Precedence: an explicit --tools
// list wins over the tier toggles; --exclude-tools is subtracted last.
func resolveEnabledTools(sel toolSelection) (map[string]bool, error) {
	var enabled map[string]bool

	switch {
	case len(sel.explicit) > 0:
		names, err := normalizeToolNames(sel.explicit)
		if err != nil {
			return nil, err
		}
		enabled = set(names)
	case sel.enableAll:
		enabled = set(tier1, tier2, tier3)
	default:
		groups := [][]string{tier1}
		if !sel.disableTier2 {
			groups = append(groups, tier2)
		}
		if sel.enableTier3 {
			groups = append(groups, tier3)
		}
		enabled = set(groups...)
	}

	if len(sel.exclude) > 0 {
		names, err := normalizeToolNames(sel.exclude)
		if err != nil {
			return nil, err
		}
		for _, n := range names {
			delete(enabled, n)
		}
	}
	return enabled, nil
}

// tierNames lists which tiers the enabled set touches, for the startup banner.
func tierNames(enabled map[string]bool) []string {
	var out []string
	for i, group := range [][]string{tier1, tier2, tier3} {
		for _, name := range group {
			if enabled[name] {
				out = append(out, fmt.Sprint(i+1))
				break
			}
		}
	}
	return out
}

// exposedTier3 returns the enabled tier-3 tools, sorted.
func exposedTier3(enabled map[string]bool) []string {
	var out []string
	for _, name := range tier3 {
		if enabled[name] {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}
