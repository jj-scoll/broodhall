package main

// Script check of a generated handoff against the writer's input (digest + facts block):
// every answered question carried, every path / commit / PR number it names present in the
// input, every still-running background job mentioned. No model call.

import (
	"fmt"
	"os"
	"strings"

	"github.com/dlclark/regexp2"
	"github.com/jj-scoll/broodhall/internal/pytext"
)

var (
	pathRe   = pytext.Re(`(?:~|/home|/tmp|/etc|/nix|/var|/opt|/usr)/[\w./@+-]*`, 0)
	commitRe = pytext.Re(`\b(?=[0-9a-f]*\d)(?=[0-9a-f]*[a-f])[0-9a-f]{7,40}\b`, 0)
	prRe     = pytext.Re(`(?:\bPR|\bpull request|\bissue)\s*#?(\d+)\b|(?<![\w&])#(\d{2,})\b`, regexp2.IgnoreCase)
	qaRe     = pytext.Re(`^\[[^\]]*\] Q: (.*)\n  A: (.*)$`, regexp2.Multiline)
	jobRe    = pytext.Re(`^- (\w+): (.*?)(?: \(output \S+\))?$`, regexp2.Multiline)
	notesRe  = pytext.Re(`\s*\(notes: .*$`, 0)
	recRe    = pytext.Re(`\s*\(Recommended\)`, 0)
	words4   = pytext.Re(`[a-z0-9]{4,}`, 0)
	words3   = pytext.Re(`[a-z0-9]{3,}`, 0)
)

// carried is true when the answer's text, or 60% of its words (4+ letters), is in the handoff.
func carried(answer, handoff string) bool {
	a, h := pytext.Lower(answer), pytext.Lower(handoff)
	if strings.Contains(h, a) {
		return true
	}
	words := pytext.FindAll(words4, a)
	if len(words) == 0 {
		words = pytext.FindAll(words3, a)
	}
	n := 0
	for _, w := range words {
		if strings.Contains(h, w.String()) {
			n++
		}
	}
	return len(words) > 0 && float64(n) >= 0.6*float64(len(words))
}

// unique is dict.fromkeys(xs): first occurrences, in order.
func unique(xs []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range xs {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

func basename(p string) string { return p[strings.LastIndexByte(p, '/')+1:] }

func verifyGaps(handoff, facts, source string) []string {
	var out []string
	for _, m := range pytext.FindAll(qaRe, pytext.Section(facts, "###", "Questions asked", false, true)) {
		q, a := pytext.Group(m, 1), pytext.Group(m, 2)
		a = pytext.SubStr(notesRe, a, "")
		a = pytext.Strip(pytext.SubStr(recRe, a, ""))
		if q != "(unparsed)" && !strings.HasPrefix(a, "(declined") && !carried(a, handoff) {
			out = append(out, fmt.Sprintf(`answer not carried: Q "%s" -> A "%s"`, q, a))
		}
	}
	home := os.Getenv("HOME")
	var paths []string
	for _, m := range pytext.FindAll(pathRe, handoff) {
		paths = append(paths, strings.TrimRight(m.String(), ".,:;)"))
	}
	for _, p := range unique(paths) {
		alt := strings.Replace(p, home, "~", 1)
		if strings.HasPrefix(p, "~") {
			alt = strings.Replace(p, "~", home, 1)
		}
		// A path the writer composed from a directory plus a bare name is fine: only the last
		// component has to appear in the input.
		if !strings.Contains(source, strings.TrimRight(p, "/")) && !strings.Contains(source, strings.TrimRight(alt, "/")) &&
			!strings.Contains(source, basename(strings.TrimRight(p, "/"))) {
			out = append(out, "path "+p+" is not in the transcript")
		}
	}
	low := pytext.Lower(source)
	var commits []string
	for _, m := range pytext.FindAll(commitRe, handoff) {
		commits = append(commits, m.String())
	}
	for _, h := range unique(commits) {
		if !strings.Contains(low, h) {
			out = append(out, "commit "+h+" is not in the transcript")
		}
	}
	var prs []string
	for _, m := range pytext.FindAll(prRe, handoff) {
		n := pytext.Group(m, 1)
		if n == "" {
			n = pytext.Group(m, 2)
		}
		prs = append(prs, n)
	}
	for _, n := range unique(prs) {
		if pytext.Search(pytext.Re(`\b`+n+`\b`, 0), source) == nil {
			out = append(out, "PR/issue #"+n+" is not in the transcript")
		}
	}
	for _, m := range pytext.FindAll(jobRe, pytext.Section(facts, "###", "Background jobs still running", false, true)) {
		if !strings.Contains(handoff, pytext.Group(m, 1)) {
			out = append(out, "background job "+pytext.Group(m, 1)+" ("+pytext.Group(m, 2)+") is still running but not mentioned")
		}
	}
	return out
}
