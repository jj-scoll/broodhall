package main

// guard-report: reads every session's guard ledger and prints, per rule, how often it fired,
// by kind. It exists for tuning: which rules fire most, and what observe mode (observe.go)
// would have stopped. The ledger holds a time, a kind and a rule id per event and nothing that
// says how an ask was answered, and no hook sees an ask's outcome (PostToolUse carries no
// link back to the ask), so approved versus denied is not knowable here and the report says so.

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"text/tabwriter"
)

func init() { commands["guard-report"] = func([]string) int { return guardReportRun(os.Stdout) } }

var reportKinds = []string{"ask", "block", "mask", "nudge", "would-ask", "would-block"}

func guardReportRun(w io.Writer) int {
	files, _ := filepath.Glob(filepath.Join(stateDir(), "*.events"))
	byRule := map[string]map[string]int{}
	sessions, events := 0, 0
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		seen := false
		for _, line := range strings.Split(string(data), "\n") {
			p := strings.Split(line, "\t")
			// Same shape as the summary and receipt: other kinds are not counted.
			if len(p) != 3 || !slices.Contains(reportKinds, p[1]) {
				continue
			}
			if byRule[p[2]] == nil {
				byRule[p[2]] = map[string]int{}
			}
			byRule[p[2]][p[1]]++
			events++
			seen = true
		}
		if seen {
			sessions++
		}
	}
	if events == 0 {
		fmt.Fprintln(w, "guard-report: no guard events recorded yet")
		return 0
	}
	total := func(r string) (n int) {
		for _, c := range byRule[r] {
			n += c
		}
		return n
	}
	rules := make([]string, 0, len(byRule))
	for r := range byRule {
		rules = append(rules, r)
	}
	sort.Slice(rules, func(i, j int) bool {
		if a, b := total(rules[i]), total(rules[j]); a != b {
			return a > b
		}
		return rules[i] < rules[j]
	})
	fmt.Fprintf(w, "guard-report: %d events in %d sessions\n\n", events, sessions)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "rule\ttotal\task\tblock\tmask\tnudge\twould-ask\twould-block")
	for _, r := range rules {
		fmt.Fprintf(tw, "%s\t%d", r, total(r))
		for _, k := range reportKinds {
			fmt.Fprintf(tw, "\t%d", byRule[r][k])
		}
		fmt.Fprintln(tw)
	}
	tw.Flush()
	fmt.Fprintln(w, "\nApproved versus denied asks are not recorded, so only fire counts are shown.")
	return 0
}
