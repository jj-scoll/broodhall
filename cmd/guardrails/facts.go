package main

// Facts block for a handoff, pulled from a transcript by script so the writer cannot drop
// them: the person's typed prompts verbatim, every AskUserQuestion with the chosen answer,
// files changed, git branch and mutating git commands, background jobs still running.

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dlclark/regexp2"
	"github.com/jj-scoll/broodhall/internal/pytext"
)

const promptCap = 1000

var (
	gitMutating = pytext.Re(`\bgit\b.*\b(commit|push|checkout|switch|worktree|merge|rebase|reset|tag|cherry-pick|stash)\b`+
		`|\bgh pr (create|merge)\b`, 0)
	bgID       = pytext.Re(`running in background with ID: (\w+)\.(?: Output is being written to: (\S+?)\.?(?:\s|$))?`, 0)
	notif      = pytext.Re(`<task-id>(\w+)</task-id>.*?<status>(\w+)</status>`, regexp2.Singleline)
	declinedRe = pytext.Re(`said:\s*(.*)`, regexp2.Singleline)
	answerTail = pytext.Re(`\.?\s*You can now continue with these answers in mind\.?\s*$`, 0)
	answerEnd  = pytext.MatchRe(`(.*?)"(?= selected preview:| user notes:|\.\s|\.?$)`, regexp2.Singleline)
	answerNote = pytext.Re(` user notes: (.*)`, regexp2.Singleline)
	secrets    = []struct {
		re   *regexp2.Regexp
		keep bool // keep group 1 before [REDACTED]
	}{
		{pytext.Re(`(?i)\b((?:aws_)?(?:access_key_id|secret_access_key|session_token)[ \t]*[=:][ \t]*)\S+`, 0), true},
		{pytext.Re(`(?i)\b((?:api[_-]?key|secret|token|password|passwd)[ \t]*[=:][ \t]*)\S+`, 0), true},
		{pytext.Re(`\b(?:AKIA|ASIA)[A-Z0-9]{16}\b`, 0), false},
		{pytext.Re(`\b(?:sk-ant-|ghp_|gho_|github_pat_|xox[bpas]-)[\w-]{8,}`, 0), false},
		{pytext.Re(`-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`, regexp2.Singleline), false},
	}
)

func redact(text string) string {
	for _, s := range secrets {
		text = pytext.Sub(s.re, text, func(m regexp2.Match) string {
			if s.keep {
				return pytext.Group(&m, 1) + "[REDACTED]"
			}
			return "[REDACTED]"
		})
	}
	return text
}

// isoRe is the subset of datetime.fromisoformat the inputs use: a date, T or space, a time
// with optional fraction, and Z or a numeric offset (required: Python cannot compare a naive
// datetime with the transcripts' aware ones).
var isoRe = regexp.MustCompile(`^(\d{4})-(\d{2})-(\d{2})[T ](\d{2}):(\d{2})(?::(\d{2})(?:[.,](\d+))?)?(Z|[+-]\d{2}:?\d{2})$`)

// fromISO is datetime.fromisoformat for those forms, truncated to microseconds as Python is.
func fromISO(s string) (time.Time, error) {
	m := isoRe.FindStringSubmatch(s)
	if m == nil {
		return time.Time{}, fmt.Errorf("invalid isoformat string: %q", s)
	}
	n := func(i int) int { v, _ := strconv.Atoi(m[i]); return v }
	frac := (m[7] + "000000")[:6]
	us, _ := strconv.Atoi(frac)
	loc := time.UTC
	if z := m[8]; z != "Z" {
		z = strings.ReplaceAll(z, ":", "")
		h, _ := strconv.Atoi(z[1:3])
		mi, _ := strconv.Atoi(z[3:5])
		off := (h*60 + mi) * 60
		if z[0] == '-' {
			off = -off
		}
		loc = time.FixedZone("", off)
	}
	return time.Date(n(1), time.Month(n(2)), n(3), n(4), n(5), n(6), us*1000, loc), nil
}

// answers pairs each question with its answer in the AskUserQuestion result text.
func answers(questions []string, text string) [][2]string {
	text = pytext.SubStr(answerTail, text, "")
	type spot struct {
		pos int
		q   string
	}
	var found []spot
	for _, q := range questions {
		if pos := strings.Index(text, `"`+q+`"="`); pos >= 0 {
			found = append(found, spot{pos, q})
		}
	}
	sort.Slice(found, func(i, j int) bool {
		if found[i].pos != found[j].pos {
			return found[i].pos < found[j].pos
		}
		return found[i].q < found[j].q
	})
	var out [][2]string
	for i, f := range found {
		end := len(text)
		if i+1 < len(found) {
			end = found[i+1].pos
		}
		seg := ""
		if start := f.pos + len(f.q) + 4; start < end {
			seg = text[start:end]
		}
		a := pytext.Cut(seg, 300)
		if m := pytext.Search(answerEnd, seg); m != nil {
			a = pytext.Group(m, 1)
		}
		if m := pytext.Search(answerNote, seg); m != nil {
			a += " (notes: " + pytext.Strip(strings.TrimRight(pytext.Strip(pytext.Group(m, 1)), ". ")) + ".)"
		}
		out = append(out, [2]string{f.q, a})
	}
	if len(out) == 0 {
		out = append(out, [2]string{"(unparsed)", pytext.Cut(text, 300)})
	}
	return out
}

// ordered is an insertion-ordered map, as the OrderedDicts in facts.py.
type ordered[V any] struct {
	keys []string
	vals map[string]V
}

func (o *ordered[V]) get(k string) (V, bool) {
	v, ok := o.vals[k]
	return v, ok
}

// set keeps a known key in its place, as assigning to an existing dict key does.
func (o *ordered[V]) set(k string, v V) {
	if o.vals == nil {
		o.vals = map[string]V{}
	}
	if _, ok := o.vals[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.vals[k] = v
}

func (o *ordered[V]) pop(k string) {
	if _, ok := o.vals[k]; !ok {
		return
	}
	delete(o.vals, k)
	for i, x := range o.keys {
		if x == k {
			o.keys = append(o.keys[:i], o.keys[i+1:]...)
			break
		}
	}
}

// pyKey makes a json.loads value usable as a Go map key with Python's dict equality for the
// values transcripts hold (strings, None, numbers).
func pyKey(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return "\x00" + pytext.Repr(v)
}

type toolUse struct {
	name any
	inp  *pytext.Obj
}

type bgJob struct{ desc, output string }

func buildFacts(path, until string) (string, error) {
	var cutAt time.Time
	if until != "" {
		var err error
		if cutAt, err = fromISO(until); err != nil {
			return "", err
		}
	}
	lines, err := transcriptLines(path)
	if err != nil {
		return "", err
	}
	var cwd, branch any
	var prompts, qa, git []string
	uses := map[string]toolUse{}
	var changed ordered[*ordered[int]]
	var bg ordered[bgJob]
	for _, d := range lines {
		if ts := d.Get("timestamp"); until != "" && pytext.Truthy(ts) {
			t, err := fromISO(pytext.Str(ts))
			if err != nil {
				return "", err
			}
			if !t.Before(cutAt) {
				break
			}
		}
		if v := d.Get("cwd"); pytext.Truthy(v) {
			cwd = v
		}
		if v := d.Get("gitBranch"); pytext.Truthy(v) {
			branch = v
		}
		msg, ok := d.Get("message").(*pytext.Obj)
		if !ok {
			continue
		}
		at := stamp(d)
		content := msg.Get("content")
		var blocks []*pytext.Obj
		if list, ok := content.([]any); ok {
			for _, b := range list {
				if o, ok := b.(*pytext.Obj); ok {
					blocks = append(blocks, o)
				}
			}
		}
		switch d.Get("type") {
		case "assistant":
			for _, b := range blocks {
				if b.Get("type") == "tool_use" {
					inp, _ := b.Get("input").(*pytext.Obj) // `or {}`; a non-dict input has no keys to read
					uses[pyKey(b.Get("id"))] = toolUse{b.Get("name"), inp}
				}
			}
			continue
		case "user":
		default:
			continue
		}
		p := ""
		if !pytext.Truthy(d.Get("isCompactSummary")) {
			p = typedPrompt(d, msg)
		}
		if p != "" && !strings.HasPrefix(p, "[Request interrupted") {
			if n := pytext.RuneLen(p); n > promptCap {
				p = pytext.Cut(p, promptCap) + fmt.Sprintf(" [%d chars cut]", n-promptCap)
			}
			prompts = append(prompts, "["+at+"] "+p)
		}
		for _, m := range pytext.FindAll(notif, textOf(content)) {
			bg.pop(pytext.Group(m, 1))
		}
		for _, b := range blocks {
			if b.Get("type") != "tool_result" {
				continue
			}
			use, ok := uses[pyKey(b.Get("tool_use_id"))]
			if !ok {
				continue
			}
			inp := use.inp
			text := textOf(b.Get("content"))
			isErr := pytext.Truthy(b.Get("is_error"))
			switch use.name {
			case "AskUserQuestion":
				var qs []string
				if list, ok := inp.Get("questions").([]any); ok {
					for _, q := range list {
						if qo, ok := q.(*pytext.Obj); ok {
							qs = append(qs, getStr(qo, "question", ""))
						}
					}
				}
				var pairs [][2]string
				if isErr {
					a := "(declined)"
					if m := pytext.Search(declinedRe, text); m != nil {
						a += " " + pytext.Cut(pytext.Strip(pytext.Group(m, 1)), 300)
					}
					for _, q := range qs {
						pairs = append(pairs, [2]string{q, a})
					}
				} else {
					pairs = answers(qs, text)
				}
				for _, qa1 := range pairs {
					qa = append(qa, "["+at+"] Q: "+qa1[0]+"\n  A: "+qa1[1])
				}
			case "Edit", "Write", "NotebookEdit", "MultiEdit":
				if isErr {
					continue
				}
				fp := inp.Get("file_path")
				if !pytext.Truthy(fp) {
					fp = inp.Get("notebook_path")
				}
				if pytext.Truthy(fp) {
					c, ok := changed.get(pytext.Str(fp))
					if !ok {
						c = &ordered[int]{}
						changed.set(pytext.Str(fp), c)
					}
					n, _ := c.get(pytext.Str(use.name))
					c.set(pytext.Str(use.name), n+1)
				}
			case "Bash":
				cmd := getStr(inp, "command", "")
				if pytext.Search(gitMutating, cmd) != nil {
					head, _, _ := strings.Cut(pytext.Strip(text), "\n")
					e := ""
					if isErr {
						e = "ERROR "
					}
					git = append(git, "- "+pytext.Cut(cmd, 200)+" -> "+e+pytext.Cut(head, 200))
				}
				if m := pytext.Search(bgID, text); pytext.Truthy(inp.Get("run_in_background")) && m != nil {
					desc := pytext.Cut(cmd, 120)
					if v := inp.Get("description"); pytext.Truthy(v) {
						desc = pytext.Str(v)
					}
					bg.set(pytext.Group(m, 1), bgJob{desc, pytext.Group(m, 2)})
				}
			case "KillShell", "TaskStop":
				id := inp.Get("shell_id")
				if !pytext.Truthy(id) {
					id = inp.Get("task_id")
				}
				if s, ok := id.(string); ok {
					bg.pop(s)
				}
			}
		}
	}
	out := []string{"## Facts extracted by script (verbatim from the transcript)", "",
		"### The person's typed prompts, in order"}
	out = append(out, orNone(prompts)...)
	out = append(out, "", "### Questions asked and the person's answers")
	out = append(out, orNone(qa)...)
	out = append(out, "", "### Files changed (Edit/Write; files written by shell commands are not listed)")
	var files []string
	for _, fp := range changed.keys {
		c := changed.vals[fp]
		var counts []string
		for _, t := range c.keys {
			counts = append(counts, fmt.Sprintf("%s x%d", t, c.vals[t]))
		}
		files = append(files, "- "+fp+" ("+strings.Join(counts, ", ")+")")
	}
	out = append(out, orNone(files)...)
	br := "(not a git repo)"
	if pytext.Truthy(branch) {
		br = pytext.Str(branch)
	}
	out = append(out, "", "### Git", "cwd "+pytext.Str(cwd)+", branch "+br)
	out = append(out, git...)
	out = append(out, "", "### Background jobs still running at the cut")
	out = append(out, orNone(jobLines(&bg))...)
	return redact(strings.Join(out, "\n")), nil
}

const maxBgJobs = 10 // jobs listed in the facts block

// bgOmittedLine matches the note jobLines adds when it leaves jobs out; write-handoff copies it
// into the handoff header because the writer model tends to drop it (issue #17).
var bgOmittedLine = regexp.MustCompile(`\(\d+ more background jobs? still running [^)\n]*\)`)

// jobLines lists the running background jobs. Jobs with the same description count as one, the
// latest. Digits count: "Wait for PR 834" and "Wait for PR 836" are different work (issue #17).
// Of those, the latest maxBgJobs are listed and a line says how many were left out.
func jobLines(bg *ordered[bgJob]) []string {
	seen := map[string]bool{}
	var keep []string // newest first
	for i := len(bg.keys) - 1; i >= 0; i-- {
		id := bg.keys[i]
		if k := bg.vals[id].desc; !seen[k] {
			seen[k] = true
			keep = append(keep, id)
		}
	}
	listed := keep
	if len(listed) > maxBgJobs {
		listed = listed[:maxBgJobs]
	}
	var jobs []string
	for i := len(listed) - 1; i >= 0; i-- {
		j := bg.vals[listed[i]]
		line := "- " + listed[i] + ": " + j.desc
		if j.output != "" {
			line += " (output " + j.output + ")"
		}
		jobs = append(jobs, line)
	}
	if n := len(bg.keys) - len(listed); n > 0 {
		s := "s"
		if n == 1 {
			s = ""
		}
		jobs = append(jobs, fmt.Sprintf("(%d more background job%s still running are not listed: older, or the same description as a listed one)", n, s))
	}
	return jobs
}

func orNone(lines []string) []string {
	if len(lines) == 0 {
		return []string{"(none)"}
	}
	return lines
}
