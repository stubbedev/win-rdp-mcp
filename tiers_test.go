package main

import (
	"slices"
	"strings"
	"testing"
)

func TestDefaultSelectionIsTier1And2(t *testing.T) {
	enabled, err := resolveEnabledTools(toolSelection{})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(enabled), len(tier1)+len(tier2); got != want {
		t.Errorf("default tools = %d, want %d (tier1+tier2)", got, want)
	}
	for _, name := range tier3 {
		if enabled[name] {
			t.Errorf("tier 3 tool %s must not be enabled by default", name)
		}
	}
}

func TestToolSelection(t *testing.T) {
	tests := []struct {
		name  string
		sel   toolSelection
		want  []string
		unset []string
	}{
		{
			name: "enable all",
			sel:  toolSelection{enableAll: true},
			want: []string{"Shell", "Snapshot", "Click"},
		},
		{
			name:  "disable tier 2",
			sel:   toolSelection{disableTier2: true},
			want:  []string{"Snapshot"},
			unset: []string{"Click"},
		},
		{
			name:  "explicit list beats the tier toggles",
			sel:   toolSelection{enableAll: true, explicit: []string{"Snapshot", "Shell"}},
			want:  []string{"Snapshot", "Shell"},
			unset: []string{"Click", "FileWrite"},
		},
		{
			name:  "exclude applies last",
			sel:   toolSelection{enableAll: true, exclude: []string{"Shell"}},
			want:  []string{"Snapshot"},
			unset: []string{"Shell"},
		},
		{
			name:  "names are case-insensitive",
			sel:   toolSelection{explicit: []string{"snapshot", "SHELL"}},
			want:  []string{"Snapshot", "Shell"},
			unset: []string{"Click"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			enabled, err := resolveEnabledTools(tc.sel)
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range tc.want {
				if !enabled[name] {
					t.Errorf("%s should be enabled", name)
				}
			}
			for _, name := range tc.unset {
				if enabled[name] {
					t.Errorf("%s should not be enabled", name)
				}
			}
		})
	}
}

// A typo in --tools must fail loudly: silently enabling a smaller surface than
// the operator asked for is the dangerous direction of that mistake.
func TestUnknownToolIsRejected(t *testing.T) {
	_, err := resolveEnabledTools(toolSelection{explicit: []string{"Snapshot", "Snapshoot"}})
	if err == nil {
		t.Fatal("expected an error for an unknown tool name")
	}
	if !strings.Contains(err.Error(), "Snapshoot") {
		t.Errorf("error should name the offending tool, got %v", err)
	}
}

func TestTiersDoNotOverlap(t *testing.T) {
	seen := map[string]string{}
	for tier, group := range map[string][]string{"tier1": tier1, "tier2": tier2, "tier3": tier3} {
		for _, name := range group {
			if other, ok := seen[name]; ok {
				t.Errorf("%s appears in both %s and %s", name, other, tier)
			}
			seen[name] = tier
		}
	}
}

func TestExposedTier3IsSorted(t *testing.T) {
	enabled, err := resolveEnabledTools(toolSelection{enableAll: true})
	if err != nil {
		t.Fatal(err)
	}
	got := exposedTier3(enabled)
	if len(got) != len(tier3) {
		t.Fatalf("exposedTier3 = %d entries, want %d", len(got), len(tier3))
	}
	if !slices.IsSorted(got) {
		t.Errorf("exposedTier3 should be sorted, got %v", got)
	}
}
