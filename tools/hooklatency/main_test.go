package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// TestMain lets the tests run the real main() in a child process: the test binary re-executes
// itself with HOOKLATENCY_RUN_MAIN set, so flag parsing, os.Exit codes and stdout are the
// ones a user sees.
func TestMain(m *testing.M) {
	if os.Getenv("HOOKLATENCY_RUN_MAIN") == "1" {
		// os.Args is ["hooklatency.test", args...]; flag parsing reads os.Args[1:].
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// fakeGuardrails writes a stand-in guardrails binary: it appends one record per call to
// $HOOKLATENCY_LOG (subcommand, HOME, working directory, stdin byte count) and exits with
// the code in $FAKE_EXIT (default 0).
func fakeGuardrails(t *testing.T) string {
	t.Helper()
	script := `#!/bin/sh
n=$(wc -c | tr -d ' ')
printf '%s|%s|%s|%s|%s\n' "$*" "$HOME" "$XDG_CONFIG_HOME" "$(pwd)" "$n" >> "$HOOKLATENCY_LOG"
exit "${FAKE_EXIT:-0}"
`
	p := filepath.Join(t.TempDir(), "guardrails")
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func runMain(t *testing.T, env []string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(os.Environ(), append([]string{"HOOKLATENCY_RUN_MAIN=1"}, env...)...)
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	err := cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &ee):
		code = ee.ExitCode()
	default:
		t.Fatal(err)
	}
	return so.String(), se.String(), code
}

func TestMainReportsBothCases(t *testing.T) {
	bin := fakeGuardrails(t)
	log := filepath.Join(t.TempDir(), "calls.log")
	out, errOut, code := runMain(t, []string{"HOOKLATENCY_LOG=" + log}, "-bin", bin, "-n", "4", "-warm", "2")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, errOut)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 report lines, got %d:\n%s", len(lines), out)
	}
	re := regexp.MustCompile(`^(\S.*?)\s+n=4 min=\d+\.\d\d p50=\d+\.\d\d p95=\d+\.\d\d max=\d+\.\d\d ms$`)
	for i, want := range []string{"pre-tool-use", "redact --secrets 30KB"} {
		m := re.FindStringSubmatch(lines[i])
		if m == nil || m[1] != want {
			t.Errorf("line %d = %q, want case %q with n=4 and four timings", i, lines[i], want)
		}
	}

	// (warm 2 + n 4) runs of each case, interleaved pre-tool-use then redact.
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	calls := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(calls) != 12 {
		t.Fatalf("want 12 process runs, got %d:\n%s", len(calls), b)
	}
	for i, c := range calls {
		f := strings.Split(c, "|")
		if len(f) != 5 {
			t.Fatalf("call %d malformed: %q", i, c)
		}
		wantArgs, minStdin := "pre-tool-use", 80
		if i%2 == 1 {
			wantArgs, minStdin = "redact --secrets", 29000
		}
		if f[0] != wantArgs {
			t.Errorf("call %d args = %q, want %q (cases must interleave)", i, f[0], wantArgs)
		}
		home := f[1]
		if !strings.Contains(filepath.Base(home), "hooklatency-home-") {
			t.Errorf("call %d HOME = %q, want a hooklatency scratch dir", i, home)
		}
		if f[2] != home+"/config" {
			t.Errorf("call %d XDG_CONFIG_HOME = %q, want %q", i, f[2], home+"/config")
		}
		if wd, _ := filepath.EvalSymlinks(home); f[3] != wd && f[3] != home {
			t.Errorf("call %d cwd = %q, want scratch home %q", i, f[3], home)
		}
		var n int
		if _, err := fmt.Sscan(f[4], &n); err != nil || n < minStdin || n > 31000 {
			t.Errorf("call %d stdin bytes = %q, want >= %d", i, f[4], minStdin)
		}
	}
	if _, err := os.Stat(strings.Split(calls[0], "|")[1]); !os.IsNotExist(err) {
		t.Errorf("scratch home not removed after the run: %v", err)
	}
}

func TestMainFlagAndFailurePaths(t *testing.T) {
	bin := fakeGuardrails(t)
	log := filepath.Join(t.TempDir(), "calls.log")
	tests := []struct {
		name       string
		env        []string
		args       []string
		wantCode   int
		wantStderr string
	}{
		{"no bin flag", nil, nil, 2, "usage: hooklatency -bin PATH [-n 300]"},
		{"empty bin flag", nil, []string{"-bin", ""}, 2, "usage: hooklatency -bin PATH"},
		{"bin that does not exist", nil, []string{"-bin", filepath.Join(t.TempDir(), "absent"), "-n", "1", "-warm", "0"}, 1, "pre-tool-use: "},
		{"bin that exits non-zero", []string{"HOOKLATENCY_LOG=" + log, "FAKE_EXIT=3"}, []string{"-bin", bin, "-n", "1", "-warm", "0"}, 1, "exit status 3"},
		{"unknown flag", nil, []string{"-nope"}, 2, "flag provided but not defined"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, errOut, code := runMain(t, tt.env, tt.args...)
			if code != tt.wantCode {
				t.Errorf("exit %d, want %d (stderr %q)", code, tt.wantCode, errOut)
			}
			if !strings.Contains(errOut, tt.wantStderr) {
				t.Errorf("stderr %q missing %q", errOut, tt.wantStderr)
			}
			if out != "" {
				t.Errorf("stdout should be empty on failure, got %q", out)
			}
		})
	}
}

func TestRun(t *testing.T) {
	bin := fakeGuardrails(t)
	log := filepath.Join(t.TempDir(), "calls.log")
	dir := t.TempDir()
	env := append(os.Environ(), "HOOKLATENCY_LOG="+log, "HOME="+dir)
	d, err := run(bin, testCase{name: "x", args: []string{"a", "b"}, stdin: []byte("12345")}, env, dir)
	if err != nil {
		t.Fatal(err)
	}
	if d <= 0 {
		t.Errorf("duration = %v, want positive", d)
	}
	b, _ := os.ReadFile(log)
	if got := strings.TrimSpace(string(b)); !strings.HasPrefix(got, "a b|"+dir+"|") || !strings.HasSuffix(got, "|5") {
		t.Errorf("record = %q, want args %q, HOME %q and 5 stdin bytes", got, "a b", dir)
	}

	env = append(env, "FAKE_EXIT=1")
	if d, err = run(bin, testCase{}, env, dir); err == nil || d != 0 || !strings.Contains(err.Error(), bin) {
		t.Errorf("failing binary: d=%v err=%v, want zero duration and an error naming %s", d, err, bin)
	}
	if _, err = run(filepath.Join(dir, "absent"), testCase{}, env, dir); err == nil {
		t.Error("missing binary must be an error")
	}
}

func TestPct(t *testing.T) {
	s := make([]time.Duration, 10)
	for i := range s {
		s[i] = time.Duration(i+1) * time.Millisecond
	}
	tests := []struct {
		name string
		s    []time.Duration
		p    int
		want time.Duration
	}{
		{"p50 of ten is the 5th", s, 50, 5 * time.Millisecond},
		{"p95 of ten is the 10th", s, 95, 10 * time.Millisecond},
		{"p100 is the max", s, 100, 10 * time.Millisecond},
		{"p10 is the 1st", s, 10, 1 * time.Millisecond},
		{"p0 clamps to the min", s, 0, 1 * time.Millisecond},
		{"single sample", s[:1], 95, 1 * time.Millisecond},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := pct(tt.s, tt.p); got != tt.want {
				t.Errorf("pct(%d) = %v, want %v", tt.p, got, tt.want)
			}
		})
	}
	// Nearest rank on 300 sorted samples: p50 is the 150th, p95 the 285th.
	big := make([]time.Duration, 300)
	for i := range big {
		big[i] = time.Duration(i + 1)
	}
	if got := pct(big, 50); got != 150 {
		t.Errorf("p50 of 300 = %d, want 150", got)
	}
	if got := pct(big, 95); got != 285 {
		t.Errorf("p95 of 300 = %d, want 285", got)
	}
}

func TestMs(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want float64
	}{
		{0, 0},
		{time.Millisecond, 1},
		{1500 * time.Microsecond, 1.5},
		{2 * time.Second, 2000},
	}
	for _, tt := range tests {
		if got := ms(tt.d); got != tt.want {
			t.Errorf("ms(%v) = %v, want %v", tt.d, got, tt.want)
		}
	}
}
