package main

// guard-report: reads every session's guard ledger and prints, per rule, how often it fired,
// by kind. It exists for tuning: which rules fire most, and what observe mode (observe.go)
// would have stopped. Asks also get an outcome (askoutcome.go): "ran" lines when the tool ran
// after the ask, "not-run" lines when it did not. Denied and abandoned cannot be told apart,
// so the report says "not run".

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

// relaxMinAsks and relaxShare: a rule whose asks with a known outcome are at least this many
// and at least this share ran gets a "consider relaxing" hint.
const (
	relaxMinAsks = 10
	relaxShare   = 0.9
)

func init() { commands["guard-report"] = func([]string) int { return guardReportRun(os.Stdout) } }

var reportKinds = []string{"ask", "block", "mask", "nudge", "would-ask", "would-block"}

func guardReportRun(w io.Writer) int {
	resolveAsks("", askGrace) // old pending asks become not-run before counting
	files, _ := filepath.Glob(filepath.Join(stateDir(), "*.events"))
	byRule := map[string]map[string]int{}
	ran, notRun := map[string]int{}, map[string]int{}
	sessions, events := 0, 0
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		seen := false
		for _, line := range strings.Split(string(data), "\n") {
			p := strings.Split(line, "\t")
			if len(p) == 3 && p[1] == "ran" {
				ran[p[2]]++
				continue
			}
			if len(p) == 3 && p[1] == "not-run" {
				notRun[p[2]]++
				continue
			}
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
	fmt.Fprintln(tw, "rule\ttotal\task\tblock\tmask\tnudge\twould-ask\twould-block\tran-after-ask\tnot-run")
	for _, r := range rules {
		fmt.Fprintf(tw, "%s\t%d", r, total(r))
		for _, k := range reportKinds {
			fmt.Fprintf(tw, "\t%d", byRule[r][k])
		}
		fmt.Fprintf(tw, "\t%d\t%d\n", ran[r], notRun[r])
	}
	tw.Flush()
	fmt.Fprintln(w, "\nran-after-ask: the tool ran after the ask, so the user approved. not-run: no follow-up by the session's Stop")
	fmt.Fprintln(w, "(or none within 10 minutes, for older asks). A denial and an abandoned prompt look the same, so this is")
	fmt.Fprintln(w, "\"not run\", not \"denied\". Asks from before outcomes were recorded have neither.")
	blank := "\n"
	for _, r := range rules {
		if n := ran[r] + notRun[r]; n >= relaxMinAsks && float64(ran[r]) >= relaxShare*float64(n) {
			fmt.Fprintf(w, "%s%s: consider relaxing. %d of %d asks were approved.\n", blank, r, ran[r], n)
			blank = ""
		}
	}
	return 0
}
