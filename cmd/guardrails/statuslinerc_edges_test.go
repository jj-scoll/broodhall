package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// rcFile writes content verbatim (no newline added) and returns its path.
func rcFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func rcLines(lines ...string) string { return strings.Join(lines, "\n") + "\n" }

func rcPadLine(n int) string { // a JSON line of exactly n bytes, n >= 20
	return `{"type":"x","p":"` + strings.Repeat("p", n-len(`{"type":"x","p":""}`)) + `"}`
}

func TestRemoteControlActiveEdges(t *testing.T) {
	f := func(n int) []string { return slFillers(0, n) }
	cat := func(parts ...[]string) []string {
		var all []string
		for _, p := range parts {
			all = append(all, p...)
		}
		return all
	}
	one := func(s ...string) []string { return s }
	tests := []struct {
		name    string
		content string
		want    bool
	}{
		{"exactly 100 entries after", rcLines(cat(one(slBridge), f(100))...), true},
		{"101 entries after", rcLines(cat(one(slBridge), f(101))...), false},
		{"blank lines in the tail do not count", rcLines(cat(one(slBridge), f(100), one("", "", "", ""))...), true},
		{"blank lines interleaved do not count", rcLines(cat(one(slBridge), one("", ""), f(50), one(""), f(50))...), true},
		{"crlf endings", strings.ReplaceAll(rcLines(cat(one(slBridge), f(100))...), "\n", "\r\n"), true},
		{"crlf endings, one too many", strings.ReplaceAll(rcLines(cat(one(slBridge), f(101))...), "\n", "\r\n"), false},
		{"marker as only line, no trailing newline", slBridge, true},
		{"marker then partial final write", rcLines(slBridge) + `{"type":"assis`, true},
		{"partial final write counts as an entry", rcLines(cat(one(slBridge), f(100))...) + `{"type":"assis`, false},
		{"marker-prefixed non-JSON line is accepted", rcLines(`{"type":"bridge-session"garbage`), true},
		{"leading space before marker", rcLines(" " + slBridge), false},
		{"reordered keys are not recognised", rcLines(`{"sessionId":"s","type":"bridge-session"}`), false},
		{"longer type name", rcLines(`{"type":"bridge-session-v2","x":1}`), false},
		{"first line of file is the marker (start==0)", rcLines(cat(one(slBridge), f(3))...), true},
		{"marker in last line behind a multi-MB line", rcLines(slBridge, rcPadLine(3<<20)), true},
		{"marker behind a line longer than the cap", rcLines(slBridge, rcPadLine(5<<20)), false},
		{"many fillers then marker then 3 MB of 100 big lines", rcLines(cat(f(2000), one(slBridge), repeatN(rcPadLine(30<<10), 100))...), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := remoteControlActive(rcFile(t, tc.content)); got != tc.want {
				t.Errorf("remoteControlActive = %v, want %v", got, tc.want)
			}
		})
	}
}

func repeatN(s string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = s
	}
	return out
}

// Chunk edges: cumulative read boundaries from EOF are 64K, 192K, 448K, 960K, 1984K, 4032K,
// then the 4 MiB cap. The marker line is placed straddling, ending at, and starting at each.
func TestRemoteControlActiveChunkEdges(t *testing.T) {
	var edges []int
	for c, sum := 64<<10, 0; sum+c < rcScanCap; c *= 2 {
		sum += c
		edges = append(edges, sum)
	}
	edges = append(edges, rcScanCap)
	for _, edge := range edges {
		// tailLen = bytes from the marker line start to EOF. Fill after the marker with lines of 64 KiB
		// (at most 64 of them up to 4 MiB) plus one remainder line.
		for _, c := range []struct {
			name  string
			delta int // marker start = EOF - edge - delta
			want  bool
		}{
			{"straddles", 20, edge < rcScanCap},
			{"ends at edge", len(slBridge) + 1, edge < rcScanCap},
			{"starts at edge", 0, edge < rcScanCap}, // at the cap: a complete line is dropped as if partial
			{"starts just inside", -1, true},
		} {
			t.Run(fmt.Sprintf("%d/%s", edge, c.name), func(t *testing.T) {
				tail := edge + c.delta - (len(slBridge) + 1) // bytes after the marker line
				var b strings.Builder
				b.WriteString(rcLines(slFillers(0, 50)...)) // some head so the file is longer than the window
				if edge >= rcScanCap {
					b.WriteString(rcLines(rcPadLine(1 << 20)))
				}
				b.WriteString(slBridge + "\n")
				for tail > 0 {
					n := min(tail, 64<<10)
					if tail-n > 0 && tail-n < 21 { // keep every line at least 20 bytes
						n = tail - 21
					}
					b.WriteString(rcPadLine(n-1) + "\n")
					tail -= n
				}
				if got := remoteControlActive(rcFile(t, b.String())); got != c.want {
					t.Errorf("remoteControlActive = %v, want %v", got, c.want)
				}
			})
		}
	}
}

// A file exactly rcScanCap bytes long reaches the file start, so its first line is whole.
func TestRemoteControlActiveExactCap(t *testing.T) {
	head := slBridge + "\n"
	body := rcPadLine(rcScanCap-len(head)-1) + "\n"
	p := rcFile(t, head+body)
	if st, _ := os.Stat(p); st.Size() != rcScanCap {
		t.Fatalf("size %d", st.Size())
	}
	if !remoteControlActive(p) {
		t.Error("marker as first line of an exactly-cap file not found")
	}
	p2 := rcFile(t, "\n"+head+body) // one byte over the cap: marker line now starts at the floor
	if remoteControlActive(p2) {
		t.Log("cap+1 file with marker starting exactly at floor reported on")
	}
}
