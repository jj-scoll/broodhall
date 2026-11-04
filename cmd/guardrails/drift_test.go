package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The cases are drift-check.py's --selftest.

func driftLine(name string, input map[string]any) string {
	b, _ := json.Marshal(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{
		map[string]any{"type": "tool_use", "name": name, "input": input}}}})
	return string(b)
}

func driftRead(p string) string { return driftLine("Read", map[string]any{"file_path": p}) }

func driftRun(t *testing.T, lines []string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return driftVerdict(p)
}

func repeat(s string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = s
	}
	return out
}

func cat(parts ...[]string) []string {
	var out []string
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func TestDriftVerdict(t *testing.T) {
	var clean []string
	for i := 0; i < 10; i++ {
		clean = append(clean, driftRead(fmt.Sprintf("/a/f%d.py", i)))
	}
	loop := cat(clean, repeat(driftRead("/a/cmap.py"), 3))
	edit := driftLine("Edit", map[string]any{"file_path": "/a/x.py"})
	bash := []string{
		driftLine("Bash", map[string]any{"command": "pytest  -x"}),
		driftLine("Bash", map[string]any{"command": "pytest -x"}),
		driftLine("Bash", map[string]any{"command": "pytest\t-x"}),
	}
	var aged []string
	aged = append(aged, repeat(driftRead("/a/old.py"), 3)...)
	for i := 0; i < 60; i++ {
		aged = append(aged, driftRead(fmt.Sprintf("/a/n%d.py", i)))
	}
	long := driftRead("/a/" + strings.Repeat("é", 40) + ".py")

	for _, tc := range []struct {
		name  string
		lines []string
		want  string
	}{
		{"clean", clean, ""},
		{"3 reads of one file exceed 2", loop, "drift Read x3 cmap.py"},
		{"3 edits do not trip", cat(clean, repeat(edit, 3)), ""},
		{"4 edits do", cat(clean, repeat(edit, 4)), "drift Edit x4 x.py"},
		{"bash whitespace dedupe and +N", cat(bash, loop), "drift Read x3 cmap.py +1"},
		{"window drops old repeats", aged, ""},
		{"malformed and non-list content skipped", cat([]string{"{not json", `{"message":{"content":"tool_use"}}`}, loop), "drift Read x3 cmap.py"},
		{"sidechain calls ignored", repeat(`{"isSidechain":true,"message":{"content":[{"type":"tool_use","name":"Read","input":{"file_path":"/a/s.py"}}]}}`, 4), ""},
		{"label cut to 27 characters, not bytes", repeat(long, 3), "drift Read x3 " + strings.Repeat("é", 27) + "…"},
	} {
		if got := driftRun(t, tc.lines); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

// Only the last 500,000 bytes are read, and the cut first line is dropped.
func TestDriftVerdictTail(t *testing.T) {
	pad := `{"pad":"` + strings.Repeat("x", 600_000) + `"}`
	lines := cat(repeat(driftRead("/a/early.py"), 3), []string{pad}, repeat(driftRead("/a/late.py"), 3))
	if got := driftRun(t, lines); got != "drift Read x3 late.py" {
		t.Errorf("got %q", got)
	}
}
