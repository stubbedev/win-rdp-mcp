package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRatioAndPartialRatio(t *testing.T) {
	if got := ratio("chrome", "chrome"); got != 100 {
		t.Errorf("ratio of identical strings = %d, want 100", got)
	}
	if got := ratio("", ""); got != 100 {
		t.Errorf("ratio of two empty strings = %d, want 100", got)
	}
	if got := ratio("abc", "xyz"); got != 0 {
		t.Errorf("ratio of disjoint strings = %d, want 0", got)
	}

	// The point of partialRatio: a short needle inside a long window title.
	if got := partialRatio("chrome", "google chrome - inbox (5)"); got != 100 {
		t.Errorf("partialRatio for a contained substring = %d, want 100", got)
	}
	if got := partialRatio("notpad", "Untitled - Notepad"); got < 80 {
		t.Errorf("partialRatio for a near miss = %d, want >= 80", got)
	}
	if got := partialRatio("firefox", "Untitled - Notepad"); got >= 50 {
		t.Errorf("partialRatio for an unrelated title = %d, want < 50", got)
	}
	if got := partialRatio("", "anything"); got != 0 {
		t.Errorf("partialRatio with an empty needle = %d, want 0", got)
	}
}

// Killing by name is exact, because a near miss kills software the user never
// named — fuzzy matching scores "notepad" against "notepad++" at 87.
func TestSameProcessName(t *testing.T) {
	for _, tc := range []struct {
		want, actual string
		match        bool
	}{
		{"notepad", "notepad.exe", true},
		{"Notepad.exe", "notepad.exe", true},
		{"chrome", "chrome", true},
		{"notepad", "notepad++.exe", false},
		{"note", "notepad.exe", false},
		{"", "notepad.exe", false},
		{"chrome", "chromedriver.exe", false},
	} {
		if got := sameProcessName(tc.want, tc.actual); got != tc.match {
			t.Errorf("sameProcessName(%q, %q) = %v, want %v", tc.want, tc.actual, got, tc.match)
		}
	}
}

func TestArgumentCoercion(t *testing.T) {
	args := arguments{
		"n": float64(42), "s": "hello", "b": true,
		"stringy_bool": "yes", "stringy_num": "17", "f": 0.25,
		"junk": "twelve",
	}

	if got := args.intOr("n", 0); got != 42 {
		t.Errorf("intOr = %d, want 42", got)
	}
	if got := args.stringOr("s", "fallback"); got != "hello" {
		t.Errorf("stringOr = %q", got)
	}
	if !args.boolOr("b", false) || !args.boolOr("stringy_bool", false) {
		t.Error("boolOr should accept both a real bool and a stringly-typed one")
	}
	if got := args.intOr("stringy_num", 0); got != 17 {
		t.Errorf("intOr on a numeric string = %d, want 17", got)
	}
	if got := args.floatOr("f", 1); got != 0.25 {
		t.Errorf("floatOr = %v", got)
	}
	// Anything unparseable falls back rather than silently becoming zero.
	if got := args.intOr("junk", 9); got != 9 {
		t.Errorf("intOr on junk = %d, want the fallback 9", got)
	}
	if got := args.intOr("absent", 5); got != 5 {
		t.Errorf("intOr on a missing key = %d, want 5", got)
	}
	if args.boolOr("absent", true) != true {
		t.Error("boolOr should fall back for a missing key")
	}
}

func TestDecodeArguments(t *testing.T) {
	for name, raw := range map[string]any{
		"nil":     nil,
		"map":     map[string]any{"x": 1.0},
		"rawJSON": []byte(`{"x":1}`),
	} {
		t.Run(name, func(t *testing.T) {
			args, err := decodeArguments(raw)
			if err != nil {
				t.Fatal(err)
			}
			if args == nil {
				t.Fatal("decodeArguments returned a nil map")
			}
		})
	}
}

// parseUserSession has to read `query session` output, including the localised
// state names a non-English Windows prints.
func TestParseUserSession(t *testing.T) {
	tests := []struct {
		name           string
		out            string
		wantID         string
		wantDisconnect bool
		wantFound      bool
	}{
		{
			name: "active console session",
			out: ` SESSIONNAME       USERNAME                 ID  STATE   TYPE        DEVICE
 services                                    0  Disc
>console           alice                     1  Active  wdcon`,
			wantID: "1", wantFound: true,
		},
		{
			name: "disconnected rdp session",
			out: ` SESSIONNAME       USERNAME                 ID  STATE   TYPE        DEVICE
 services                                    0  Disc
                   alice                     2  Disc`,
			wantID: "2", wantDisconnect: true, wantFound: true,
		},
		{
			name: "localised disconnected state",
			out: ` 会话名            用户名                   ID  状态
 services                                    0  Disc
                   alice                     3  已断开`,
			wantID: "3", wantDisconnect: true, wantFound: true,
		},
		{
			name:      "no user logged on",
			out:       " SESSIONNAME       USERNAME    ID  STATE\n services                      0  Disc",
			wantFound: false,
		},
		{
			name:      "empty output",
			out:       "",
			wantFound: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			id, disconnected, found := parseUserSession(tc.out)
			if found != tc.wantFound {
				t.Fatalf("found = %v, want %v", found, tc.wantFound)
			}
			if !found {
				return
			}
			if id != tc.wantID {
				t.Errorf("session id = %q, want %q", id, tc.wantID)
			}
			if disconnected != tc.wantDisconnect {
				t.Errorf("disconnected = %v, want %v", disconnected, tc.wantDisconnect)
			}
		})
	}
}

func TestPSQuoteEscapesSingleQuotes(t *testing.T) {
	// A path or title carrying a quote must stay data, never become script. In
	// a PowerShell single-quoted string the only escape is a doubled quote, so
	// correctness means every run of quotes inside the wrapper is even-length —
	// an odd run would close the string and let the rest execute.
	got := psQuote(`C:\it's here'; Remove-Item C:\ -Recurse; '`)
	inner := got[1 : len(got)-1]
	for i := 0; i < len(inner); {
		if inner[i] != '\'' {
			i++
			continue
		}
		run := 0
		for i < len(inner) && inner[i] == '\'' {
			run++
			i++
		}
		if run%2 != 0 {
			t.Fatalf("psQuote left an odd run of %d quotes, which would end the string early: %s", run, got)
		}
	}
	if got := psQuote("plain"); got != "'plain'" {
		t.Errorf("psQuote(plain) = %s", got)
	}
	if got := psQuote("it's"); got != "'it''s'" {
		t.Errorf("psQuote(it's) = %s", got)
	}
}

func TestXMLEscape(t *testing.T) {
	if got := xmlEscape(`a<b>&"c"`); got != "a&lt;b&gt;&amp;&quot;c&quot;" {
		t.Errorf("xmlEscape = %q", got)
	}
}

func TestHumanBytes(t *testing.T) {
	for n, want := range map[int64]string{
		512: "512B", 2048: "2KB", 5 << 20: "5MB", 3 << 30: "3GB",
	} {
		if got := humanBytes(n); got != want {
			t.Errorf("humanBytes(%d) = %s, want %s", n, got, want)
		}
	}
}

func TestTableAligns(t *testing.T) {
	got := table([]string{"A", "Long"}, func(yield func([]string) bool) {
		yield([]string{"1", "x"})
		yield([]string{"22", "yy"})
	})
	lines := strings.Split(got, "\n")
	if len(lines) != 4 {
		t.Fatalf("expected a header, a rule and two rows, got %d lines:\n%s", len(lines), got)
	}
	if !strings.HasPrefix(lines[1], "-") {
		t.Errorf("second line should be the underline rule, got %q", lines[1])
	}
}

// ── Task manager ─────────────────────────────────────────────────────────────

func TestTaskManagerStampsAndRecordsTasks(t *testing.T) {
	m := newTaskManager()
	result, err := m.run(t.Context(), "GetSystemInfo", func(context.Context) (toolResult, error) {
		return textResult("hello"), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(result.Content[0].Text, "[task:") {
		t.Errorf("result should carry a task id, got %q", result.Content[0].Text)
	}

	listed := m.list("")
	if len(listed) != 1 || listed[0].Status != statusCompleted {
		t.Fatalf("expected one completed task, got %+v", listed)
	}
	if listed[0].Duration == nil {
		t.Error("a finished task should report a duration")
	}
}

func TestTaskManagerRecordsFailures(t *testing.T) {
	m := newTaskManager()
	_, err := m.run(t.Context(), "Shell", func(context.Context) (toolResult, error) {
		return toolResult{}, errors.New("boom")
	})
	if err == nil {
		t.Fatal("expected the tool error to surface")
	}
	listed := m.list(statusFailed)
	if len(listed) != 1 || listed[0].Error != "boom" {
		t.Fatalf("expected one failed task carrying the message, got %+v", listed)
	}
}

// Desktop tools must not overlap: two synthetic clicks at once are two clicks
// in the wrong places.
func TestDesktopTasksRunOneAtATime(t *testing.T) {
	m := newTaskManager()
	var (
		mu      sync.Mutex
		running int
		peak    int
	)

	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = m.run(t.Context(), "Click", func(context.Context) (toolResult, error) {
				mu.Lock()
				running++
				peak = max(peak, running)
				mu.Unlock()

				time.Sleep(20 * time.Millisecond)

				mu.Lock()
				running--
				mu.Unlock()
				return textResult("clicked"), nil
			})
		}()
	}
	wg.Wait()

	if peak != 1 {
		t.Errorf("peak concurrent desktop tasks = %d, want 1", peak)
	}
}

func TestCancelTask(t *testing.T) {
	m := newTaskManager()
	started := make(chan string, 1)
	done := make(chan struct{})

	go func() {
		defer close(done)
		_, _ = m.run(t.Context(), "Wait", func(ctx context.Context) (toolResult, error) {
			started <- m.list(statusRunning)[0].ID
			<-ctx.Done()
			return toolResult{}, ctx.Err()
		})
	}()

	id := <-started
	if _, err := m.cancelTask(id); err != nil {
		t.Fatalf("cancelling a running task: %v", err)
	}
	<-done

	task, ok := m.get(id)
	if !ok || task.Status != statusCancelled {
		t.Fatalf("task status = %+v, want cancelled", task)
	}
	// Cancelling twice is a mistake worth reporting, not a silent no-op.
	if _, err := m.cancelTask(id); err == nil {
		t.Error("cancelling an already-cancelled task should fail")
	}
	if _, err := m.cancelTask("nosuchtask"); err == nil {
		t.Error("cancelling an unknown task should fail")
	}
}

func TestTaskHistoryIsPruned(t *testing.T) {
	m := newTaskManager()
	for range maxTaskHistory + 20 {
		if _, err := m.run(t.Context(), "GetSystemInfo", func(context.Context) (toolResult, error) {
			return textResult("ok"), nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(m.tasks); got > maxTaskHistory+1 {
		t.Errorf("task history holds %d entries, want it pruned to about %d", got, maxTaskHistory)
	}
}

func TestWithTaskIDOnAnImageOnlyResult(t *testing.T) {
	got := toolResult{Content: []contentBlock{{Type: "image", Data: []byte("x"), MIME: "image/png"}}}.withTaskID("abc")
	if len(got.Content) != 2 || got.Content[1].Text != "[task:abc]" {
		t.Errorf("an image-only result should gain a text block carrying the id, got %+v", got.Content)
	}
}
