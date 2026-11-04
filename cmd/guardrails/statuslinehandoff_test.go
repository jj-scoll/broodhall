package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// slHandoffLine2 renders the payload for session sess-1 against a fake handoff state dir
// that prep fills, and returns line 2.
func slHandoffLine2(t *testing.T, prep func(state string), args ...string) string {
	t.Helper()
	base := t.TempDir()
	state := filepath.Join(base, "claude-handoff")
	if err := os.MkdirAll(state, 0o755); err != nil {
		t.Fatal(err)
	}
	prep(state)
	out := renderStatuslineEnv(t, slFull(""), map[string]string{"HANDOFF_STATE_DIR": base}, args...)
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	return lines[len(lines)-1] // the main line; line 1 is absent with -segments that skip identity
}

// slMark writes a state file for sess-1 whose mtime is ageSec before the statusline's clock.
func slMark(t *testing.T, state, suffix, body string, ageSec int64) {
	t.Helper()
	p := filepath.Join(state, "sess-1."+suffix)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	mt := time.Unix(slNow-ageSec, 0)
	if err := os.Chtimes(p, mt, mt); err != nil {
		t.Fatal(err)
	}
}

func TestStatuslineHandoffPhases(t *testing.T) {
	handoff := "/work/proj/HANDOFF-x.md\n/tmp/prev.jsonl\n"
	tests := []struct {
		name string
		prep func(t *testing.T, state string)
		want string // substring of line 2; "" = no handoff segment
	}{
		{"nothing", func(t *testing.T, s string) {}, ""},
		{"gated, waiting for next-steps", func(t *testing.T, s string) {
			slMark(t, s, "gate", "", 30)
		}, "handoff: next-steps"},
		{"gate mark too old", func(t *testing.T, s string) {
			slMark(t, s, "gate", "", 3600)
		}, ""},
		{"gate mark left over from a finished handoff", func(t *testing.T, s string) {
			slMark(t, s, "gate", "", 600)
			slMark(t, s, "autoclear", "", 500)
		}, ""},
		{"writer running", func(t *testing.T, s string) {
			slMark(t, s, "gate", "", 60)
			slMark(t, s, "autoclear", "", 23)
			slMark(t, s, "writing", fmt.Sprint(slNow-23)+"\n", 23)
		}, "handoff: writing 23s"},
		{"writer running over a minute", func(t *testing.T, s string) {
			slMark(t, s, "autoclear", "", 90)
			slMark(t, s, "writing", fmt.Sprint(slNow-90)+"\n", 90)
		}, "handoff: writing 1m30s"},
		{"writer marker stale (writer died)", func(t *testing.T, s string) {
			slMark(t, s, "autoclear", "", 7200)
			slMark(t, s, "writing", fmt.Sprint(slNow-7200)+"\n", 7200)
		}, ""},
		{"clear armed", func(t *testing.T, s string) {
			slMark(t, s, "autoclear", "", 100)
			slMark(t, s, "rearm", handoff, 5)
		}, "handoff: clear armed"},
		{"clear claimed by context-guard", func(t *testing.T, s string) {
			slMark(t, s, "autoclear", "", 100)
			slMark(t, s, "claimed", handoff, 1)
		}, "handoff: clearing"},
		{"cleared, just now", func(t *testing.T, s string) {
			slMark(t, s, "autoclear", "", 100)
			slMark(t, s, "cleared", handoff, 30)
		}, "handoff: cleared → HANDOFF-x.md"},
		{"cleared, expired", func(t *testing.T, s string) {
			slMark(t, s, "cleared", handoff, 300)
		}, ""},
		{"writer failed, just now", func(t *testing.T, s string) {
			slMark(t, s, "autoclear", "", 100)
			slMark(t, s, "failed", "", 30)
		}, "handoff: failed (see writer.log)"},
		{"writer failed, expired", func(t *testing.T, s string) {
			slMark(t, s, "failed", "", 300)
		}, ""},
		{"autoclear off: written, user runs /clear", func(t *testing.T, s string) {
			slMark(t, s, "autoclear", "", 100)
			slMark(t, s, "written", handoff, 30)
		}, "handoff: written → HANDOFF-x.md, run /clear"},
		{"autoclear off: written, long expired", func(t *testing.T, s string) {
			slMark(t, s, "written", handoff, 7200)
		}, ""},
		{"another session's state", func(t *testing.T, s string) {
			os.WriteFile(filepath.Join(s, "sess-2.rearm"), []byte(handoff), 0o644)
			os.WriteFile(filepath.Join(s, "sess-2.gate"), nil, 0o644)
		}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l2 := slHandoffLine2(t, func(s string) { tt.prep(t, s) })
			if tt.want == "" {
				if strings.Contains(l2, "handoff") {
					t.Errorf("segment shown with nothing to say: %q", l2)
				}
				return
			}
			if !strings.Contains(l2, tt.want) {
				t.Errorf("want %q in %q", tt.want, l2)
			}
		})
	}
}

// A new handoff cycle (gate newer than the last autoclear) wins over a leftover marker.
func TestStatuslineHandoffNewCycleBeatsLeftovers(t *testing.T) {
	l2 := slHandoffLine2(t, func(s string) {
		slMark(t, s, "autoclear", "", 100)
		slMark(t, s, "cleared", "/w/OLD.md\n", 90)
		slMark(t, s, "gate", "", 10)
	})
	if !strings.Contains(l2, "handoff: next-steps") || strings.Contains(l2, "OLD.md") {
		t.Errorf("leftover shown instead of the new cycle: %q", l2)
	}
}

func TestStatuslineHandoffSegmentOption(t *testing.T) {
	prep := func(s string) { slMark(t, s, "rearm", "/w/H.md\n", 5) }
	if l2 := slHandoffLine2(t, prep, "-segments", "dir,model"); strings.Contains(l2, "handoff") {
		t.Errorf("-segments without handoff still shows it: %q", l2)
	}
	if l2 := slHandoffLine2(t, prep, "-segments", "dir,model,handoff"); !strings.Contains(l2, "handoff: clear armed") {
		t.Errorf("-segments with handoff hides it: %q", l2)
	}
}

func TestStatuslineHandoffUnsafeSessionIgnored(t *testing.T) {
	base := t.TempDir()
	os.MkdirAll(filepath.Join(base, "claude-handoff"), 0o755)
	os.WriteFile(filepath.Join(base, "claude-handoff", "..rearm"), []byte("x\n"), 0o644)
	in := strings.Replace(slFull(""), `"sess-1"`, `".."`, 1)
	out := renderStatuslineEnv(t, in, map[string]string{"HANDOFF_STATE_DIR": base})
	if strings.Contains(out, "handoff") {
		t.Errorf("unsafe session id read state: %q", out)
	}
}
