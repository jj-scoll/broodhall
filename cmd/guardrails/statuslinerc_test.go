package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

// countingReaderAt counts the bytes read through it.
type countingReaderAt struct {
	r io.ReaderAt
	n int64
}

func (c *countingReaderAt) ReadAt(p []byte, off int64) (int, error) {
	n, err := c.r.ReadAt(p, off)
	c.n += int64(n)
	return n, err
}

// rcScanFile runs the scan on path and returns its answer and the bytes it read.
func rcScanFile(t *testing.T, path string) (bool, int64) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	c := &countingReaderAt{r: f}
	return rcScan(c, st.Size()), c.n
}

// A transcript of tens of megabytes costs a bounded tail read, whether the marker is near the
// end, far from it, or absent. The bound is checked in bytes read, not time: a slow CI runner
// took 181 ms over the same 4 MB a fast one reads in a few.
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
	if on, n := rcScanFile(t, big); !on {
		t.Error("marker near the end of an 80 MB transcript not found")
	} else if n > rcScanCap {
		t.Errorf("tail scan read %d bytes of an 80 MB file, want at most %d", n, rcScanCap)
	}
	g, _ := os.OpenFile(big, os.O_APPEND|os.O_WRONLY, 0o644)
	for i := 0; i < 300; i++ {
		g.WriteString(slFiller(i) + "\n")
	}
	g.Close()
	if on, n := rcScanFile(t, big); on {
		t.Error("stale marker in an 80 MB transcript still reported")
	} else if n > rcScanCap {
		t.Errorf("stale scan read %d bytes of an 80 MB file, want at most %d", n, rcScanCap)
	}
	// No marker, and lines so long that rcMaxLinesAfter of them span more than rcScanCap: only
	// the cap stops the scan.
	long := `{"type":"api-request-blob","blob":"` + strings.Repeat("x", 100<<10) + `"}`
	if on, n := rcScanFile(t, slTranscript(t, slRepeat(long, 200)...)); on || n > rcScanCap {
		t.Errorf("20 MB of 100 KB lines, no marker: on=%v, read %d bytes, want false and at most %d", on, n, rcScanCap)
	}
}

func slRepeat(s string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = s
	}
	return out
}

// Entries bigger than the scan cap must not hang or grow memory without bound.
func TestRemoteControlActiveHugeLines(t *testing.T) {
	p := slTranscript(t, slBridge, `{"type":"api-request-blob","blob":"`+strings.Repeat("y", 20<<20)+`"}`)
	if on, n := rcScanFile(t, p); on {
		t.Error("marker behind a 20 MB line reported as live")
	} else if n > rcScanCap {
		t.Errorf("read %d bytes of a 20 MB line, want at most %d", n, rcScanCap)
	}
}

const (
	slRCActive = `{"parentUuid":null,"isSidechain":false,"type":"system","subtype":"bridge_status","content":"/remote-control is active","url":"https://example.invalid/code/session_x"}`
	slRCDown   = `{"parentUuid":null,"isSidechain":false,"type":"system","subtype":"informational","content":"Remote Control disconnected — server rejected the request","level":"warning"}`
	slRCNot    = `{"parentUuid":null,"isSidechain":false,"type":"system","subtype":"informational","content":"Remote Control not started here · another Claude Code is connected","level":"warning"}`
)

// Claude Code keeps writing bridge-session lines after Remote Control disconnects, so the
// newest of the activation and disconnect notices decides.
func TestStatuslineRemoteControlDisconnect(t *testing.T) {
	tests := []struct {
		name  string
		lines []string
		want  bool
	}{
		{"active notice then bridge lines", []string{slRCActive, slBridge, slFiller(1)}, true},
		{"disconnected, bridge lines keep coming", []string{slRCActive, slBridge, slRCDown, slFiller(1), slBridge, slFiller(2)}, false},
		{"disconnected, then reactivated", []string{slRCActive, slBridge, slRCDown, slFiller(1), slRCActive, slBridge, slFiller(2)}, true},
		{"not started here", []string{slRCNot, slBridge, slFiller(1)}, false},
		{"disconnect notice after the marker", []string{slBridge, slFiller(1), slRCDown}, false},
		{"disconnect older than the active notice", []string{slRCDown, slRCActive, slBridge}, true},
		{"disconnect notice quoted inside a tool result", []string{slBridge, `{"type":"user","message":{"content":"{\"type\":\"system\",\"subtype\":\"informational\",\"content\":\"Remote Control disconnected\"}"}}`}, true},
		{"oversized line is not a notice", []string{slBridge, strings.Replace(slRCDown, `"level"`, `"pad":"`+strings.Repeat("z", rcNoticeMaxLen)+`","level"`, 1), slFiller(1)}, true},
		{"active notice but no marker", append([]string{slRCActive}, slFillers(0, 5)...), false},
		{"active notice but marker out of range", append(append([]string{slBridge}, slFillers(0, rcMaxLinesAfter+5)...), slRCActive), false},
		{"disconnect with no marker", []string{slRCActive, slRCDown, slFiller(1)}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := strings.Contains(slRC(t, tc.lines...), "⇄ rc"); got != tc.want {
				t.Errorf("segment shown = %v, want %v", got, tc.want)
			}
		})
	}
}

// A notice is looked for about a mebibyte back, no further, so a long transcript stays cheap.
func TestStatuslineRemoteControlNoticeRange(t *testing.T) {
	pad := func(kib int) []string { // lines of 20 KiB
		n := kib / 20
		out := make([]string, n)
		for i := range out {
			out[i] = `{"type":"api-request-blob","blob":"` + strings.Repeat("x", 20<<10) + `"}`
		}
		return out
	}
	near := append(append([]string{slRCDown}, pad(500)...), slBridge)
	far := append(append([]string{slRCDown}, pad(1500)...), slBridge)
	if strings.Contains(slRC(t, near...), "⇄ rc") {
		t.Error("disconnect 500 KiB back ignored")
	}
	if !strings.Contains(slRC(t, far...), "⇄ rc") {
		t.Error("disconnect 1.5 MiB back should be out of range")
	}
}
