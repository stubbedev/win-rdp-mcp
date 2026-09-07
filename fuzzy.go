package main

import "strings"

// Fuzzy name matching, used where the upstream Python server used thefuzz:
// picking a window by title, filtering or killing a process by name.
//
// ratio is the same measure as difflib.SequenceMatcher.ratio and rapidfuzz's
// normalized indel similarity — 2*LCS / (len(a)+len(b)), scaled to 0-100 — so
// the thresholds carried over from upstream (50, 60, 80) mean what they meant
// there.

// fuzzyMaxLen bounds the O(n*m) table below. Titles and process names are far
// shorter than this; anything longer is compared on its opening characters.
const fuzzyMaxLen = 256

func ratio(a, b string) int {
	ra, rb := []rune(truncate(a)), []rune(truncate(b))
	total := len(ra) + len(rb)
	if total == 0 {
		return 100
	}
	return 100 * 2 * lcs(ra, rb) / total
}

// partialRatio scores the best alignment of the shorter string against any
// window of the longer one, so "chrome" still matches "Google Chrome - Inbox".
func partialRatio(a, b string) int {
	ra, rb := []rune(truncate(a)), []rune(truncate(b))
	if len(ra) > len(rb) {
		ra, rb = rb, ra
	}
	if len(ra) == 0 {
		if len(rb) == 0 {
			return 100
		}
		return 0
	}
	// An exact substring is a perfect partial match; this is both the common
	// case and a shortcut past the window scan.
	if strings.Contains(string(rb), string(ra)) {
		return 100
	}

	best := 0
	for start := range len(rb) - len(ra) + 1 {
		window := rb[start : start+len(ra)]
		best = max(best, 100*2*lcs(ra, window)/(2*len(ra)))
		if best == 100 {
			break
		}
	}
	return best
}

// lcs is the length of the longest common subsequence, on two rolling rows
// rather than a full table.
func lcs(a, b []rune) int {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	prev := make([]int, len(b)+1)
	curr := make([]int, len(b)+1)
	for i := range len(a) {
		for j := range len(b) {
			if a[i] == b[j] {
				curr[j+1] = prev[j] + 1
			} else {
				curr[j+1] = max(prev[j+1], curr[j])
			}
		}
		prev, curr = curr, prev
	}
	return prev[len(b)]
}

func truncate(s string) string {
	r := []rune(s)
	if len(r) <= fuzzyMaxLen {
		return s
	}
	return string(r[:fuzzyMaxLen])
}
