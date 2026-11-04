package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const slBridge = `{"type":"bridge-session","sessionId":"sess-1","bridgeSessionId":"cse_TEST","lastSequenceNum":7}`

// slFiller is one ordinary transcript entry (about 100 bytes).
func slFiller(i int) string {
	return fmt.Sprintf(`{"type":"attachment","isSidechain":false,"timestamp":"2026-10-28T03:00:00Z","n":%d}`, i)
}

func slFillers(from, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = slFiller(from + i)
	}
	return out
}

// slRC renders the status line for a transcript made of lines and returns the whole output.
func slRC(t *testing.T, lines ...string) string {
	t.Helper()
	return renderStatusline(t, slFull(fmt.Sprintf(`,"transcript_path":%q`, slTranscript(t, lines...))))
}

func TestStatuslineRemoteControl(t *testing.T) {
	cat := func(parts ...[]string) []string {
		var all []string
		for _, p := range parts {
			all = append(all, p...)
		}
		return all
	}
	tests := []struct {
		name  string
		lines []string
		want  bool
	}{
		{"no marker", slFillers(0, 30), false},
		{"empty transcript", nil, false},
		{"marker is the last line", cat(slFillers(0, 5), []string{slBridge}), true},
		{"marker with entries after it", cat([]string{slBridge}, slFillers(0, rcMaxLinesAfter-1)), true},
		{"marker at the window edge", cat([]string{slBridge}, slFillers(0, rcMaxLinesAfter)), true},
		{"stale marker, session went on", cat([]string{slBridge}, slFillers(0, rcMaxLinesAfter+1)), false},
		{"old marker then a fresh one", cat([]string{slBridge}, slFillers(0, 500), []string{slBridge}, slFillers(0, 3)), true},
		{"marker text only inside a tool result", cat(slFillers(0, 3), []string{`{"type":"user","message":{"content":"grep said {\"type\":\"bridge-session\"}"}}`}), false},
		{"marker with a truncated last line", cat([]string{slBridge}, slFillers(0, 3), []string{`{"type":"assis`}), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out := slRC(t, tc.lines...)
			if got := strings.Contains(out, "⇄ rc"); got != tc.want {
				t.Errorf("segment shown = %v, want %v\n%s", got, tc.want, out)
			}
		})
	}
}

func TestStatuslineRemoteControlSegmentsFlag(t *testing.T) {
	tp := slTranscript(t, slBridge)
	in := slFull(fmt.Sprintf(`,"transcript_path":%q`, tp))
	if out := renderStatuslineEnv(t, in, nil, "-segments", "dir,model"); strings.Contains(out, "⇄ rc") {
		t.Errorf("rc shown although -segments leaves it out:\n%s", out)
	}
	if out := renderStatuslineEnv(t, in, nil, "-segments", "dir,rc"); !strings.Contains(out, "⇄ rc") {
		t.Errorf("rc missing although -segments names it:\n%s", out)
	}
}

func TestStatuslineRemoteControlBadPath(t *testing.T) {
	for name, p := range map[string]string{
		"missing":   filepath.Join(t.TempDir(), "nope.jsonl"),
		"directory": t.TempDir(),
		"empty":     "",
	} {
		t.Run(name, func(t *testing.T) {
			out := renderStatusline(t, slFull(fmt.Sprintf(`,"transcript_path":%q`, p)))
			if strings.Contains(out, "⇄ rc") {
				t.Errorf("segment shown for a bad transcript path:\n%s", out)
			}
		})
	}
	t.Run("unreadable", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root reads anything")
		}
		p := slTranscript(t, slBridge)
		if err := os.Chmod(p, 0); err != nil {
			t.Fatal(err)
		}
		if out := renderStatusline(t, slFull(fmt.Sprintf(`,"transcript_path":%q`, p))); strings.Contains(out, "⇄ rc") {
			t.Errorf("segment shown for an unreadable transcript:\n%s", out)
		}
	})
}

// A transcript of tens of megabytes costs a bounded tail read, whether the marker is near the
// end, far from it, or absent.
func TestRemoteControlActiveHugeTranscript(t *testing.T) {
	big := filepath.Join(t.TempDir(), "big.jsonl")
	f, err := os.Create(big)
	if err != nil {
		t.Fatal(err)
	}
	pad := `{"type":"api-request-blob","blob":"` + strings.Repeat("x", 4000) + `"}` + "\n"
	for i := 0; i < 20000; i++ { // ~80 MB
		f.WriteString(pad)
	}
	f.WriteString(slBridge + "\n")
	for i := 0; i < 10; i++ {
		f.WriteString(slFiller(i) + "\n")
	}
	f.Close()
	start := time.Now()
	if !remoteControlActive(big) {
		t.Error("marker near the end of an 80 MB transcript not found")
	}
	if d := time.Since(start); d > 50*time.Millisecond {
		t.Errorf("tail scan took %v on an 80 MB file", d)
	}
	g, _ := os.OpenFile(big, os.O_APPEND|os.O_WRONLY, 0o644)
	for i := 0; i < 300; i++ {
		g.WriteString(slFiller(i) + "\n")
	}
	g.Close()
	start = time.Now()
	if remoteControlActive(big) {
		t.Error("stale marker in an 80 MB transcript still reported")
	}
	if d := time.Since(start); d > 50*time.Millisecond {
		t.Errorf("stale scan took %v on an 80 MB file", d)
	}
}

// Entries bigger than the scan cap must not hang or grow memory without bound.
func TestRemoteControlActiveHugeLines(t *testing.T) {
	p := slTranscript(t, slBridge, `{"type":"api-request-blob","blob":"`+strings.Repeat("y", 20<<20)+`"}`)
	start := time.Now()
	if remoteControlActive(p) {
		t.Error("marker behind a 20 MB line reported as live")
	}
	if d := time.Since(start); d > 200*time.Millisecond {
		t.Errorf("took %v", d)
	}
}
