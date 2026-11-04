package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const slNow = 1793160000

// renderStatusline runs statusline with in as stdin and returns what it prints.
func renderStatusline(t *testing.T, in string) string {
	t.Helper()
	return renderStatuslineEnv(t, in, nil)
}

// renderStatuslineEnv is renderStatusline with extra environment (COLUMNS, CLAUDE_CONFIG_DIR...).
func renderStatuslineEnv(t *testing.T, in string, env map[string]string, args ...string) string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("COLUMNS", "")
	t.Setenv("HANDOFF_STATE_DIR", t.TempDir())
	t.Setenv("STATUSLINE_NOW", fmt.Sprint(slNow))
	for k, v := range env {
		t.Setenv(k, v)
	}
	stdin, err := os.CreateTemp(t.TempDir(), "stdin")
	if err != nil {
		t.Fatal(err)
	}
	stdin.WriteString(in)
	stdin.Seek(0, 0)
	r, w, _ := os.Pipe()
	oldIn, oldOut := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = stdin, w
	statusline(args)
	w.Close()
	os.Stdin, os.Stdout = oldIn, oldOut
	out, _ := io.ReadAll(r)
	return string(out)
}

func slUser(content string) string {
	return fmt.Sprintf(`{"type":"user","isSidechain":false,"timestamp":"2026-10-28T03:00:00Z","message":{"id":null,"content":%q,"usage":null}}`, content)
}

// one main-thread response long before slNow: the transcript path renders it cold
const slOldAssistant = `{"type":"assistant","isSidechain":false,"timestamp":"2026-10-27T00:00:00Z","message":{"id":"m1","content":[],"usage":{"cache_read_input_tokens":5000,"cache_creation_input_tokens":700,"cache_creation":{"ephemeral_1h_input_tokens":700}}}}`

func slTranscript(t *testing.T, lines ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func slInput(tpath, promptCache string) string {
	s := fmt.Sprintf(`{"transcript_path":%q`, tpath)
	if promptCache != "" {
		s += `,"prompt_cache":` + promptCache
	}
	return s + "}"
}

// prompt_cache gives the TTL countdown and request/miss counts; the transcript's totals go.
func TestStatuslinePromptCacheReplacesTranscript(t *testing.T) {
	tp := slTranscript(t, slOldAssistant)
	out := renderStatusline(t, slInput(tp, fmt.Sprintf(`{"warm":true,"ttl":"1h","expires_at":%d,"requests":103,"misses":0}`, slNow+600)))
	for _, want := range []string{"⏳10m", "103req", "0miss"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in %q", want, out)
		}
	}
	for _, bad := range []string{"Σ", "cold"} {
		if strings.Contains(out, bad) {
			t.Errorf("unexpected %q in %q", bad, out)
		}
	}
}

// A miss is a rebuild: its count turns red.
func TestStatuslinePromptCacheMissesRed(t *testing.T) {
	tp := slTranscript(t, slOldAssistant)
	out := renderStatusline(t, slInput(tp, fmt.Sprintf(`{"expires_at":%d,"requests":9,"misses":2}`, slNow-5)))
	if !strings.Contains(out, "\033[38;5;196m2miss") {
		t.Errorf("misses not red: %q", out)
	}
	if !strings.Contains(out, "⏳cold") {
		t.Errorf("past expires_at not cold: %q", out)
	}
}

// With prompt_cache only the transcript's tail is read for the ping count: a corrupt line
// at the start (which zeroes the count on the full-read path) is never reached, and the
// real prompt that ends the run is larger than the first chunk read.
func TestStatuslinePingsFromTail(t *testing.T) {
	tp := slTranscript(t,
		`not json "type":"user"`,
		slUser("real prompt "+strings.Repeat("x", 200<<10)),
		slOldAssistant,
		slUser("keepalive-ping (1)"),
		slUser("keepalive-ping (2)"),
	)
	out := renderStatuslineEnv(t, slInput(tp, fmt.Sprintf(`{"expires_at":%d,"requests":1,"misses":0}`, slNow+600)), nil, "-keepalive-marker", "keepalive-ping")
	if !strings.Contains(out, "♻2/4") {
		t.Errorf("want ♻2/4 in %q", out)
	}
}

// Pings from the very first line count all the way to the start of the file.
func TestStatuslinePingsFromTailWholeFile(t *testing.T) {
	tp := slTranscript(t,
		slUser("keepalive-ping (1)"),
		slUser("keepalive-ping (2)"),
		slUser("keepalive-ping (3)"),
	)
	out := renderStatuslineEnv(t, slInput(tp, fmt.Sprintf(`{"expires_at":%d,"requests":1,"misses":0}`, slNow+600)), nil, "-keepalive-marker", "keepalive-ping")
	if !strings.Contains(out, "♻3/4") {
		t.Errorf("want ♻3/4 in %q", out)
	}
}

// The drift tripwire runs in process: no drift-check.py under this HOME, still flagged.
func TestStatuslineDriftInProcess(t *testing.T) {
	tp := slTranscript(t, repeat(driftRead("/a/cmap.py"), 3)...)
	out := renderStatusline(t, slInput(tp, ""))
	if !strings.Contains(out, "⚠ drift Read x3 cmap.py") {
		t.Errorf("no drift segment in %q", out)
	}
}

// Without prompt_cache (older clients) the transcript path still renders.
func TestStatuslineNoPromptCacheFallsBack(t *testing.T) {
	tp := slTranscript(t, slOldAssistant)
	out := renderStatusline(t, slInput(tp, ""))
	for _, want := range []string{"⏳cold", "Σ5k rd 1k wr"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in %q", want, out)
		}
	}
	if strings.Contains(out, "req") {
		t.Errorf("unexpected req/miss in %q", out)
	}
}

// Without -keepalive-marker no ping segment appears, whatever the transcript holds.
func TestStatuslineNoPingsWithoutMarker(t *testing.T) {
	tp := slTranscript(t, slUser("keepalive-ping (1)"), slOldAssistant, slUser("keepalive-ping (2)"))
	out := renderStatusline(t, slInput(tp, ""))
	if strings.Contains(out, "♻") {
		t.Errorf("ping segment without a marker: %q", out)
	}
}

// -keepalive-max sets the denominator; the fallback (no prompt_cache) full read counts too.
func TestStatuslinePingsMax(t *testing.T) {
	tp := slTranscript(t, slOldAssistant, slUser("keepalive-ping (1)"))
	out := renderStatuslineEnv(t, slInput(tp, ""), nil, "-keepalive-marker", "keepalive-ping", "-keepalive-max", "9")
	if !strings.Contains(out, "♻1/9") {
		t.Errorf("want ♻1/9 in %q", out)
	}
}
