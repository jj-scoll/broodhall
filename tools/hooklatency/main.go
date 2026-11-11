// hooklatency times fresh guardrails processes, so a change to what every hook process pays
// for (binary size, init work, first-scan cost) shows up as a number that can be reproduced:
//
//	go run ./tools/hooklatency -bin ./guardrails -n 300
//
// Each run is one whole process: fork, exec, read stdin, write stdout, exit, measured as wall
// clock from before Start to after Wait. Two cases run, interleaved so drift in machine load
// hits both alike:
//
//	pre-tool-use  a small Bash PreToolUse JSON on stdin (the real hook path, so it includes
//	              whatever rtk subprocess pre-tool-use spawns on this machine)
//	redact        `redact --secrets` over 30 KB of source-looking text on stdin
//
// HOME and XDG_* point at a scratch directory, so the runs do not write guard state into the
// real home. Output is one line per case: n, min, p50, p95, max, in milliseconds.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
)

type testCase struct {
	name  string
	args  []string
	stdin []byte
}

func main() {
	bin := flag.String("bin", "", "guardrails binary to time")
	n := flag.Int("n", 300, "fresh-process runs per case")
	warm := flag.Int("warm", 5, "untimed runs per case first")
	flag.Parse()
	if *bin == "" {
		fmt.Fprintln(os.Stderr, "usage: hooklatency -bin PATH [-n 300]")
		os.Exit(2)
	}
	home, err := os.MkdirTemp("", "hooklatency-home-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer os.RemoveAll(home)
	env := append(os.Environ(), "HOME="+home, "XDG_CONFIG_HOME="+home+"/config", "XDG_STATE_HOME="+home+"/state", "XDG_CACHE_HOME="+home+"/cache", "XDG_DATA_HOME="+home+"/data")

	line := "func (x *redactor) maskSecretShapes(s string) string { spans := secretSpans(s) } // token=abc.def\n"
	cases := []testCase{
		{"pre-tool-use", []string{"pre-tool-use"}, []byte(`{"hook_event_name":"PreToolUse","session_id":"hooklatency","cwd":"` + home + `","tool_name":"Bash","tool_input":{"command":"ls -la src"}}`)},
		{"redact --secrets 30KB", []string{"redact", "--secrets"}, []byte(strings.Repeat(line, 30000/len(line)))},
	}
	samples := make([][]time.Duration, len(cases))
	for i := -*warm; i < *n; i++ {
		for c, tc := range cases {
			d, err := run(*bin, tc, env, home)
			if err != nil {
				fmt.Fprintf(os.Stderr, "%s: %v\n", tc.name, err)
				os.Exit(1)
			}
			if i >= 0 {
				samples[c] = append(samples[c], d)
			}
		}
	}
	for c, tc := range cases {
		s := samples[c]
		sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
		fmt.Printf("%-24s n=%d min=%.2f p50=%.2f p95=%.2f max=%.2f ms\n", tc.name, len(s), ms(s[0]), ms(pct(s, 50)), ms(pct(s, 95)), ms(s[len(s)-1]))
	}
}

func run(bin string, tc testCase, env []string, dir string) (time.Duration, error) {
	cmd := exec.Command(bin, tc.args...)
	cmd.Env = env
	cmd.Dir = dir
	cmd.Stdin = bytes.NewReader(tc.stdin)
	start := time.Now()
	err := cmd.Run()
	d := time.Since(start)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", bin, err)
	}
	return d, nil
}

// pct is the nearest-rank percentile of sorted samples.
func pct(s []time.Duration, p int) time.Duration {
	i := (p*len(s) + 99) / 100
	if i < 1 {
		i = 1
	}
	return s[i-1]
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }
