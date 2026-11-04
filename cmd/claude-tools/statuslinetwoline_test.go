package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

// slAccounts configures the optional account badge: personal by default, work for one dir.
var slAccounts = []string{"-account-default", "personal", "-account", ".second=work"}

var slEscapes = regexp.MustCompile("\033\\[[0-9;]*m|\033\\]8;;[^\033]*\033\\\\")

// slWidth counts visible runes, with the emoji-width glyphs at 2 cells like a terminal draws them.
func slWidth(s string) int {
	s = slEscapes.ReplaceAllString(s, "")
	w := utf8.RuneCountInString(s)
	for _, r := range "⚡⏳⏱" {
		w += strings.Count(s, string(r))
	}
	return w
}

// full is a payload with every field the two lines read; names and urls are invented.
func slFull(extra string) string {
	return fmt.Sprintf(`{"session_id":"sess-1","session_name":"Fix the widget","effort":{"level":"high"},
"output_style":{"name":"Explanatory"},"workspace":{"current_dir":"/srv/proj/app","git_worktree":"feat-x"},
"pr":{"number":42,"url":"https://example.com/o/r/pull/42"},
"model":{"display_name":"Opus 5.5"},"cost":{"total_duration_ms":600000},
"context_window":{"used_percentage":30,"context_window_size":1000000,"total_input_tokens":1000,"current_usage":{"cache_read_input_tokens":900}},
"prompt_cache":{"expires_at":%d,"requests":20,"misses":1,"recache_tokens_if_cold":101224},
"rate_limits":{"five_hour":{"used_percentage":42,"resets_at":%d},"seven_day":{"used_percentage":10,"resets_at":%d}}%s}`,
		slNow+120, slNow+4980, slNow+86400*3, extra)
}

func slLines(t *testing.T, out string) []string {
	t.Helper()
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %d: %q", len(lines), out)
	}
	return lines
}

func TestStatuslineIdentityLine(t *testing.T) {
	out := renderStatuslineEnv(t, slFull(""), map[string]string{"CLAUDE_CONFIG_DIR": "/home/x/.second"}, slAccounts...)
	l1 := slLines(t, out)[0]
	for _, want := range []string{
		"work", "Fix the widget", "high", "feat-x", "Explanatory",
		"\033]8;;https://example.com/o/r/pull/42\033\\", "PR #42",
	} {
		if !strings.Contains(l1, want) {
			t.Errorf("line 1 missing %q in %q", want, l1)
		}
	}
	if strings.Contains(l1, "ctx") || strings.Contains(l1, "Opus") {
		t.Errorf("line 2 content leaked onto line 1: %q", l1)
	}
}

func TestStatuslineIdentityHidesDefaults(t *testing.T) {
	in := `{"effort":{"level":"medium"},"output_style":{"name":"default"},"model":{"display_name":"Opus 5.5"}}`
	l1 := slLines(t, renderStatuslineEnv(t, in, nil, slAccounts...))[0]
	for _, bad := range []string{"medium", "default", "PR", "\033]8"} {
		if strings.Contains(l1, bad) {
			t.Errorf("line 1 shows %q when it should be hidden: %q", bad, l1)
		}
	}
	if !strings.Contains(l1, "personal") {
		t.Errorf("no personal badge in %q", l1)
	}
}

func TestStatuslineAccountBadge(t *testing.T) {
	for _, tc := range []struct{ dir, want, not string }{
		{"/home/x/.second", "work", "personal"},
		{"/home/x/.second/", "work", "personal"},
		{"/home/x/.claude", "personal", "work"},
		{"", "personal", "work"},
		{"/home/x/.second-old", "personal", "work"},
	} {
		l1 := slLines(t, renderStatuslineEnv(t, `{}`, map[string]string{"CLAUDE_CONFIG_DIR": tc.dir}, slAccounts...))[0]
		if !strings.Contains(l1, tc.want) || strings.Contains(l1, tc.not) {
			t.Errorf("CLAUDE_CONFIG_DIR=%q: line 1 = %q, want %q only", tc.dir, l1, tc.want)
		}
	}
}

func TestStatuslineLine2ResetCountdownAndCold(t *testing.T) {
	l2 := slLines(t, renderStatusline(t, slFull("")))[1]
	for _, want := range []string{"5h:42%", "1h23m", "cold = 101k recache"} {
		if !strings.Contains(l2, want) {
			t.Errorf("line 2 missing %q in %q", want, l2)
		}
	}
	// plenty of cache left: no recache warning; reset in the past: no countdown
	in := strings.Replace(slFull(""), fmt.Sprint(slNow+120), fmt.Sprint(slNow+900), 1)
	in = strings.Replace(in, fmt.Sprint(slNow+4980), fmt.Sprint(slNow-5), 1)
	l2 = slLines(t, renderStatusline(t, in))[1]
	if strings.Contains(l2, "recache") || strings.Contains(l2, "1h23m") {
		t.Errorf("warm cache / past reset still shows them: %q", l2)
	}
}

func TestStatuslineGuardCounter(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HANDOFF_STATE_DIR", dir)
	logGuardEvent("sess-1", "mask", "pii-wrap")
	logGuardEvent("sess-1", "mask", "pii-wrap")
	logGuardEvent("sess-1", "ask", "egress:pii")
	logGuardEvent("sess-2", "block", "other-session")
	l2 := slLines(t, renderStatuslineEnv(t, slFull(""), map[string]string{"HANDOFF_STATE_DIR": dir}))[1]
	if !strings.Contains(l2, "guards 2 masked · 1 asked") {
		t.Errorf("no guard counter in %q", l2)
	}
	if strings.Contains(l2, "blocked") {
		t.Errorf("another session's events counted: %q", l2)
	}
	if _, err := os.Stat(filepath.Join(dir, "claude-handoff", "sess-1.events.off")); err == nil {
		t.Error("statusline committed the receipt hook's offset")
	}
	// no ledger, no counter
	l2 = slLines(t, renderStatusline(t, slFull("")))[1]
	if strings.Contains(l2, "guards") {
		t.Errorf("counter shown with no events: %q", l2)
	}
}

func TestStatuslineWidthAware(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HANDOFF_STATE_DIR", dir)
	logGuardEvent("sess-1", "mask", "pii-wrap")
	env := map[string]string{"HANDOFF_STATE_DIR": dir, "CLAUDE_CONFIG_DIR": "/h/.second"}
	render := func() string { return renderStatuslineEnv(t, slFull(""), env, slAccounts...) }

	env["COLUMNS"] = "400"
	wide := slLines(t, render())
	for _, want := range []string{"ctx", "5h:42%", "7d:10%", "guards 1 masked", "cold = ", "req"} {
		if !strings.Contains(wide[0]+wide[1], want) {
			t.Errorf("wide render missing %q: %q", want, wide)
		}
	}

	for _, cols := range []int{120, 100, 70, 50} {
		env["COLUMNS"] = fmt.Sprint(cols)
		got := slLines(t, render())
		for i, l := range got {
			if w := slWidth(l); w > cols {
				t.Errorf("COLUMNS=%d line %d is %d wide: %q", cols, i+1, w, l)
			}
		}
		// the core survives at every width
		if !strings.Contains(got[1], "Opus 5.5") || !strings.Contains(got[1], "/srv/proj/app") {
			t.Errorf("COLUMNS=%d dropped model or dir: %q", cols, got[1])
		}
		if !strings.Contains(got[0], "work") {
			t.Errorf("COLUMNS=%d dropped the account badge: %q", cols, got[0])
		}
	}

	// the lowest-value segments go first
	env["COLUMNS"] = "70"
	l2 := slLines(t, render())[1]
	if strings.Contains(l2, "req") || strings.Contains(l2, "miss") {
		t.Errorf("req/miss kept at 70 columns: %q", l2)
	}
}

func TestStatuslineMalformedNeverBreaks(t *testing.T) {
	for _, in := range []string{
		``, `not json`, `[]`, `null`,
		`{"session_name":7,"effort":"high","output_style":[],"pr":"x","workspace":5,"worktree":"w"}`,
		`{"pr":{"number":"nope","url":42},"rate_limits":"x","prompt_cache":{"recache_tokens_if_cold":"big","expires_at":"soon"}}`,
		`{"session_id":"../../etc","session_name":"a\u001b]0;pwn\u0007b","pr":{"number":1,"url":"javascript:alert(1)"}}`,
		`{"rate_limits":{"five_hour":{"resets_at":"tomorrow","used_percentage":5}},"session_id":7}`,
	} {
		out := renderStatuslineEnv(t, in, map[string]string{"COLUMNS": "abc"}, slAccounts...)
		if strings.TrimSpace(out) == "" {
			t.Errorf("no output for %q", in)
		}
		if strings.Contains(out, "\033]0;") || strings.Contains(out, "\a") || strings.Contains(out, "javascript:") {
			t.Errorf("payload text reached the terminal unescaped for %q: %q", in, out)
		}
	}
}

// With no account flags there is no badge, and an empty identity line is not printed.
func TestStatuslineNoAccountByDefault(t *testing.T) {
	// A fixed cwd: the fallback is the process cwd, which on GitHub runners is under /home/runner/work.
	out := renderStatuslineEnv(t, `{"model":{"display_name":"Opus 5.5"},"cwd":"/tmp/proj"}`, map[string]string{"CLAUDE_CONFIG_DIR": "/home/x/.second"})
	if strings.Contains(out, "personal") || strings.Contains(out, "work") {
		t.Errorf("badge without -account flags: %q", out)
	}
	if n := strings.Count(strings.TrimRight(out, "\n"), "\n"); n != 0 {
		t.Errorf("want one line, got %d newlines: %q", n, out)
	}
	if strings.HasPrefix(out, "\n") {
		t.Errorf("empty first line: %q", out)
	}
}

// ctx colours follow -ctx-warn and -ctx-limit (tokens); the limit shows as the denominator.
func TestStatuslineCtxThresholds(t *testing.T) {
	in := func(tok int) string {
		return fmt.Sprintf(`{"context_window":{"used_percentage":%d,"context_window_size":1000000}}`, tok/10000)
	}
	args := []string{"-ctx-warn", "180000", "-ctx-limit", "260000"}
	for _, tc := range []struct {
		tok  int
		want string
	}{
		{100000, "\033[38;5;82mctx 100k/260k"},
		{200000, "\033[38;5;214mctx 200k/260k"},
		{270000, "\033[38;5;196mctx 270k/260k"},
	} {
		if out := renderStatuslineEnv(t, in(tc.tok), nil, args...); !strings.Contains(out, tc.want) {
			t.Errorf("%d tokens: want %q in %q", tc.tok, tc.want, out)
		}
	}
}

// Without flags the thresholds scale with the window the payload reports: 70% and 90%.
func TestStatuslineCtxDefaultsFromWindow(t *testing.T) {
	for _, tc := range []struct {
		pct  int
		want string
	}{
		{30, "\033[38;5;82mctx 300k/900k"},
		{75, "\033[38;5;214mctx 750k/900k"},
		{95, "\033[38;5;196mctx 950k/900k"},
	} {
		in := fmt.Sprintf(`{"context_window":{"used_percentage":%d,"context_window_size":1000000}}`, tc.pct)
		if out := renderStatusline(t, in); !strings.Contains(out, tc.want) {
			t.Errorf("%d%%: want %q in %q", tc.pct, tc.want, out)
		}
	}
	// no window size, no flags: the count shows with no limit and no colour judgement
	in := `{"context_window":{"used_percentage":30,"total_input_tokens":5}}`
	if out := renderStatusline(t, in); strings.Contains(out, "ctx") {
		t.Errorf("ctx shown without a token count: %q", out)
	}
}

// -segments picks what shows; names it does not know are ignored.
func TestStatuslineSegments(t *testing.T) {
	out := renderStatuslineEnv(t, slFull(""), nil, "-segments", "dir,model,bogus")
	if !strings.Contains(out, "/srv/proj/app") || !strings.Contains(out, "Opus 5.5") {
		t.Errorf("chosen segments missing: %q", out)
	}
	for _, bad := range []string{"ctx", "5h:", "cache", "7d:", "⏳"} {
		if strings.Contains(out, bad) {
			t.Errorf("segment %q shown though not chosen: %q", bad, out)
		}
	}
	// identity-line segments use the same list: session and pr are not chosen either
	if strings.Contains(out, "Fix the widget") || strings.Contains(out, "PR #42") {
		t.Errorf("identity segments shown though not chosen: %q", out)
	}
	out = renderStatuslineEnv(t, slFull(""), nil, "-segments", "session,pr")
	if !strings.Contains(out, "Fix the widget") || !strings.Contains(out, "PR #42") {
		t.Errorf("identity segments missing: %q", out)
	}
}

// A bad flag must not break the line: statusline prints with defaults.
func TestStatuslineBadFlagStillRenders(t *testing.T) {
	out := renderStatuslineEnv(t, slFull(""), nil, "-nonsense")
	if !strings.Contains(out, "Opus 5.5") {
		t.Errorf("no render after a bad flag: %q", out)
	}
}

// The desktop usage cache is written only when -usage-cache names a file.
func TestStatuslineUsageCacheOptIn(t *testing.T) {
	path, _ := usagePath(t)
	renderStatusline(t, slFull(""))
	if _, err := os.Stat(path); err == nil {
		t.Fatal("usage cache written without -usage-cache")
	}
	renderStatuslineEnv(t, slFull(""), nil, "-usage-cache", path)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"ctx":30`, `"five":42`, `"week":10`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("usage cache missing %s: %s", want, b)
		}
	}
}
