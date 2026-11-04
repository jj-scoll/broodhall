package main

// Write a session's handoff from its transcript, outside the session: a standard digest,
// the script facts block and the session's own Next Steps go to one isolated Haiku call with
// a JSON schema; the reply is rendered to the handoff skill's seven sections, checked by
// verifyGaps, and fixed by one more call only when the check finds gaps. The handoff is
// written to the project dir and its short briefing is offered to the memory daemon's
// "handoff" list (best effort).
//
// The calls run `claude -p` on the user's own Claude Code login; an inherited
// ANTHROPIC_API_KEY is dropped from the child's environment so it cannot bill an API account.
//
//	guardrails write-handoff <transcript.jsonl> <next-steps file> <cwd> <session id>
//
// Prints the handoff path; a one-line summary (time, cost, gaps) goes to stderr.
// The next-steps file: optional first line "file: HANDOFF<name>.md", then the session's
// Next Steps and Open Questions. It is deleted once the handoff is written.

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/dlclark/regexp2"
)

const (
	writerModel = "claude-haiku-5-5"
	briefingCap = 3000     // the memory daemon caps a handoff entry's fields at 4000 JSON chars
	at, atEsc   = "@", "＠" // `claude -p` attaches any @/path in its prompt; send fullwidth @ instead
)

const resultPrefix = "    -> " // starts a tool-result line in the digest

// recentKeep is how many of the newest digest lines keep their tool results while older ones are dropped.
const recentKeep = 20

// digestCap is the most bytes of digest sent to the writer (about 50k tokens).
const digestCap = 200_000

// logOut is where the per-call summary and the gap lists go.
var logOut io.Writer = os.Stderr

// capDigest joins the digest lines to at most limit bytes. When they do not fit it drops the
// oldest tool-result lines first, then the oldest lines of any kind, and puts a marker saying
// how much was left out in front. It never fails: the most recent end is what is kept.
func capDigest(lines []string, limit int) string {
	size := 0
	for _, l := range lines {
		size += len(l) + 1
	}
	if size-1 <= limit {
		return strings.Join(lines, "\n")
	}
	budget := limit - 200 // room for the marker
	drop := make([]bool, len(lines))
	results, others := 0, 0
	for i := 0; i < len(lines)-recentKeep && size > budget; i++ { // the newest lines keep their results
		if strings.HasPrefix(lines[i], resultPrefix) {
			drop[i], size, results = true, size-len(lines[i])-1, results+1
		}
	}
	for i := 0; i < len(lines) && size > budget; i++ {
		if !drop[i] {
			drop[i], size, others = true, size-len(lines[i])-1, others+1
		}
	}
	out := []string{fmt.Sprintf("[... %d older digest lines omitted (%d tool results, %d others) to keep the digest under %d KB ...]",
		results+others, results, others, limit/1000)}
	for i, l := range lines {
		if !drop[i] {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

// callSummary is one claude call's token counts (input includes cache reads and writes) and cost.
func callSummary(label string, res *obj) string {
	var in, out int64
	if u, ok := res.get("usage").(*obj); ok {
		n := func(k string) int64 {
			v, _ := u.get(k).(json.Number)
			i, _ := v.Int64()
			return i
		}
		in = n("input_tokens") + n("cache_creation_input_tokens") + n("cache_read_input_tokens")
		out = n("output_tokens")
	}
	return fmt.Sprintf("%s: in=%d out=%d $%.4f", label, in, out, costOf(res))
}

// logGaps prints a heading with the count, then each gap.
func logGaps(heading string, found []string) {
	fmt.Fprintf(logOut, "%s (%d):\n", heading, len(found))
	for _, g := range found {
		fmt.Fprintln(logOut, "- "+g)
	}
}

//go:embed handoffskill.md
var handoffSkill string

var (
	handoffFile = regexp.MustCompile(`^HANDOFF[A-Za-z0-9._-]*\.md$`)
	fileLine    = pyMatchRe(`file:\s*(\S+)`, 0)
	fileHeader  = pyRe(`\Afile:.*\n?`, 0)
	jsonObject  = pyRe(`\{.*\}`, regexp2.Singleline)
	mdFence     = pyRe("^```(?:markdown|md)?\\n(.*)\\n```$", regexp2.Singleline)
	keyChars    = pyRe(`[^a-z0-9]+`, 0)

	// The sections are the seven things handoffskill.md says a note must cover.
	sections = []string{"Goal", "Where Things Stand", "Next Action", "Dead Ends",
		"Decisions and Constraints", "Locations", "Open Questions"}
	claudeArgs = []string{"-p", "--safe-mode", "--no-session-persistence", "--strict-mcp-config", "--tools", "",
		"--model", writerModel, "--effort", "high", "--output-format", "json"}
)

const writerPromptText = `You are Claude Code, about to end a long session. Below is the SKILL you follow to write a
handoff, then a transcript digest of everything that happened in the session. You cannot run
tools; write the handoff from this material alone.

Follow the skill's required content. Reply with one JSON object that has one key per section
(Goal, Where Things Stand, Next Action, Dead Ends, Decisions and Constraints, Locations, Open
Questions), each holding that section's markdown body without its heading. Where the skill says
to leave out an empty item, write a short line saying there is nothing to report.

Write the handoff in the third person or impersonally ("the session", "the person", or no
subject), never "I" or "we": a different session will read it, and "I" would not be that session.

=== SKILL START ===
{skill}
=== SKILL END ===

=== SESSION DIGEST START ===
{digest}
=== SESSION DIGEST END ===

After the digest comes a FACTS BLOCK that a script extracted from the full transcript. It is
exact where the digest is truncated: the person's typed prompts word for word, their answer to
every question asked, the files changed, mutating git commands, and background jobs still
running. Prefer it over the digest for those facts, keep the person's own words for their goal
and constraints, and carry each answered question into the handoff as a decision.

=== FACTS BLOCK START ===
{facts}
=== FACTS BLOCK END ===

The session wrote its own Next Steps before handing off; it knows its plan better than the digest
shows. Use them as the handoff's Next Action, keeping their wording, and add only steps the digest
shows are missing.

=== SESSION'S NEXT STEPS START ===
{next}
=== SESSION'S NEXT STEPS END ===
`

const fixPromptText = `Below is a session handoff, the facts block a script extracted from the session, and the
problems a script found in the handoff. Fix only those problems:
- answer not carried: add it under Decisions and Constraints (the question, the option chosen,
  any notes);
- path, commit or PR/issue not in the transcript: correct it from the facts block if it is there,
  otherwise remove it; a path the handoff proposes to create can stay;
- background job not mentioned: add it under Where Things Stand with its ID and what it does,
  noting that it may have finished since.
Change nothing else. Reply with the full corrected HANDOFF.md and nothing else.

=== HANDOFF START ===
{handoff}
=== HANDOFF END ===

=== FACTS BLOCK START ===
{facts}
=== FACTS BLOCK END ===

=== PROBLEMS ===
{gaps}
`

// writerPrompt fills the template in one pass, so text in a value is never substituted again.
func writerPrompt(skill, dig, facts, next string) string {
	return strings.NewReplacer("{skill}", skill, "{digest}", dig, "{facts}", facts, "{next}", next).Replace(writerPromptText)
}

func escapeAt(s string) string { return strings.ReplaceAll(s, at, atEsc) }

// sectionKey is the section's JSON key; the API wants keys matching [a-zA-Z0-9_.-].
func sectionKey(s string) string { return strings.Trim(subStr(keyChars, pyLower(s), "_"), "_") }

func newObj(kv ...any) *obj {
	o := &obj{vals: map[string]any{}}
	for i := 0; i < len(kv); i += 2 {
		k := kv[i].(string)
		o.keys = append(o.keys, k)
		o.vals[k] = kv[i+1]
	}
	return o
}

func handoffSchema() *obj {
	required := []any{}
	props := newObj()
	for _, s := range sections {
		k := sectionKey(s)
		required = append(required, k)
		props.keys = append(props.keys, k)
		props.vals[k] = newObj("type", "string", "minLength", json.Number("1"),
			"description", "Markdown body of the '"+s+"' section, without its heading")
	}
	return newObj("type", "object", "additionalProperties", false, "required", required, "properties", props)
}

func shortInput(inp any) string {
	o, ok := inp.(*obj)
	if !ok {
		return ""
	}
	for _, k := range []string{"file_path", "notebook_path", "command", "pattern", "url", "query", "prompt", "description"} {
		if v := o.get(k); truthy(v) {
			return k + "=" + pyReprStr(cut(pyStr(v), 300))
		}
	}
	return cut(pyDumps(o), 300)
}

// digest is the typed prompts, assistant text, tool calls with short inputs and result heads.
func digest(path string) (string, error) {
	lines, err := transcriptLines(path)
	if err != nil {
		return "", err
	}
	var out []string
	for _, d := range lines {
		msg, ok := d.get("message").(*obj)
		if !ok {
			continue
		}
		ts := stamp(d)
		switch d.get("type") {
		case "user":
			if p := typedPrompt(d, msg); p != "" {
				out = append(out, "["+ts+"] USER: "+cut(p, 3000))
			}
			list, _ := msg.get("content").([]any)
			for _, b := range list {
				if o, ok := b.(*obj); ok && o.get("type") == "tool_result" {
					head := cut(strings.ReplaceAll(pyStrip(stripPrivate(textOf(o.get("content")))), "\n", " | "), 300)
					e := ""
					if truthy(o.get("is_error")) {
						e = "ERROR "
					}
					out = append(out, resultPrefix+e+head)
				}
			}
		case "assistant":
			list, _ := msg.get("content").([]any) // a string or dict content has no dict items
			for _, b := range list {
				o, ok := b.(*obj)
				if !ok {
					continue
				}
				switch o.get("type") {
				case "text":
					if t := pyStrip(getStr(o, "text", "")); t != "" {
						out = append(out, "["+ts+"] ASSISTANT: "+cut(t, 2000))
					}
				case "tool_use":
					out = append(out, "["+ts+"] TOOL "+pyStr(o.get("name"))+": "+shortInput(o.get("input")))
				}
			}
		}
	}
	return capDigest(out, digestCap), nil
}

// skillText is the memory plugin's handoff skill up to its save steps, which do not apply to
// a tool-less run. A test holds it equal to plugins/memory/skills/handoff/SKILL.md.
func skillText() (string, error) { return handoffSkill, nil }

// splitNext reads the next-steps file's text: the handoff file name its "file:" line asks for
// (else HANDOFF-<session[:8]>.md) and the Next Steps without that line.
func splitNext(text, session string) (name, next string) {
	if m := search(fileLine, text); m != nil {
		name = group(m, 1)
	}
	if !handoffFile.MatchString(name) {
		name = "HANDOFF-" + cut(session, 8) + ".md"
	}
	return name, pyStrip(subStr(fileHeader, text, ""))
}

// childEnv is the environment for `claude -p`: the parent's, without ANTHROPIC_API_KEY, so the
// call runs on the user's Claude Code login and a stray key bills nothing.
func childEnv() []string {
	env := []string{}
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "ANTHROPIC_API_KEY=") {
			env = append(env, kv)
		}
	}
	return env
}

// callClaude runs one isolated `claude -p` and returns its JSON result. It runs in a fresh
// empty directory so nothing in the project is picked up.
func callClaude(prompt string, extra ...string) (*obj, error) {
	dir, err := os.MkdirTemp("", "write-handoff-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	ctx, cancel := context.WithTimeout(context.Background(), 900*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "claude", append(append([]string{}, claudeArgs...), extra...)...)
	cmd.Stdin = strings.NewReader(escapeAt(prompt))
	cmd.Dir = dir
	cmd.Env = childEnv()
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("claude: %v: %s %s", err, cut(stderr.String(), 300), cut(stdout.String(), 300))
	}
	v, err := decodeJSON(stdout.Bytes())
	if err != nil {
		return nil, fmt.Errorf("claude output: %v", err)
	}
	res, ok := v.(*obj)
	if !ok {
		return nil, errors.New("claude output is not a JSON object")
	}
	return res, nil
}

func costOf(res *obj) float64 {
	var f float64
	if n, ok := res.get("total_cost_usd").(json.Number); ok {
		f, _ = n.Float64()
	}
	return f
}

func briefing(text, path string) string {
	parts := []string{}
	for _, t := range []string{"Goal", "Next Action", "Decisions and Constraints"} {
		parts = append(parts, t+":\n"+pySection(text, "##", t, true, false))
	}
	return cut("Full handoff: "+path+" (read it first).\n\n"+strings.Join(parts, "\n\n"), briefingCap)
}

// store offers the briefing to the memory plugin's daemon (work_state_write on its unix
// socket, $XDG_RUNTIME_DIR/memory.sock) for the next session in the project to show once.
func store(cwd string, fields *obj) error {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		return errors.New("no XDG_RUNTIME_DIR, so no memory socket")
	}
	sock := filepath.Join(dir, "memory.sock")
	client := http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		},
		DisableKeepAlives: true,
	}}
	body := pyDumps(newObj("list", "handoff", "fields", fields, "cwd", cwd))
	resp, err := client.Post("http://memory/tool/work_state_write", "application/json", strings.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

// writeHandoffFile writes the handoff and returns its path and the summary line.
func writeHandoffFile(transcript, nextPath, cwd, session string) (string, string, error) {
	start := time.Now()
	raw, err := readPyText(nextPath, false)
	if err != nil {
		return "", "", err
	}
	name, next := splitNext(raw, session)
	dig, err := digest(transcript)
	if err != nil {
		return "", "", err
	}
	dig = redact(dig)
	facts, err := buildFacts(transcript, "")
	if err != nil {
		return "", "", err
	}
	skill, err := skillText()
	if err != nil {
		return "", "", err
	}
	res, err := callClaude(writerPrompt(skill, dig, facts, next), "--json-schema", pyDumps(handoffSchema()))
	if err != nil {
		return "", "", err
	}
	cost := costOf(res)
	fmt.Fprintln(logOut, callSummary("writer", res))
	out, _ := res.get("structured_output").(*obj)
	if !truthy(out) {
		m := search(jsonObject, getStr(res, "result", ""))
		if m == nil {
			return "", "", errors.New("claude reply has no JSON object")
		}
		v, err := decodeJSON([]byte(m.String()))
		if out, _ = v.(*obj); err != nil || out == nil {
			return "", "", fmt.Errorf("claude reply: no JSON object (%v)", err)
		}
	}
	body := make([]string, len(sections))
	for i, s := range sections {
		v, ok := out.lookup(sectionKey(s))
		if !ok {
			return "", "", fmt.Errorf("claude reply lacks %q", sectionKey(s))
		}
		body[i] = "## " + s + "\n\n" + pyStrip(pyStr(v))
	}
	text := strings.ReplaceAll(fmt.Sprintf("# Handoff\n\n> Written %s by guardrails write-handoff (%s) from the transcript %s\n> Project: %s\n\n",
		time.Now().Format("2006-01-02 15:04 MST"), writerModel, transcript, cwd)+strings.Join(body, "\n\n"), atEsc, at)
	known := strings.Join([]string{facts, dig, next, transcript, cwd}, "\n") // header paths are known
	found := verifyGaps(text, facts, known)
	fixedNote := ""
	if len(found) > 0 {
		logGaps("gaps found", found)
		gaps := make([]string, len(found))
		for i, g := range found {
			gaps[i] = "- " + g
		}
		fix := strings.NewReplacer("{handoff}", text, "{facts}", facts, "{gaps}", strings.Join(gaps, "\n")).Replace(fixPromptText)
		res, err := callClaude(fix)
		if err != nil {
			return "", "", err
		}
		cost += costOf(res)
		fmt.Fprintln(logOut, callSummary("fix", res))
		reply := pyStrip(getStr(res, "result", ""))
		fixed := sub(mdFence, reply, func(m regexp2.Match) string { return group(&m, 1) })
		if strings.HasPrefix(fixed, "# Handoff") {
			text = strings.ReplaceAll(fixed, atEsc, at)
			left := verifyGaps(text, facts, known)
			logGaps("gaps remaining after fix", left)
			fixedNote = fmt.Sprintf(" fixed remaining=%d", len(left))
		} else {
			fmt.Fprintf(logOut, "fix reply rejected (does not start with \"# Handoff\"), keeping the draft: %q\n", cut(reply, 100))
			fixedNote = " fix-rejected"
		}
	}
	// After the fix call, which may rewrite the header: the reader must know jobs are missing.
	if m := bgOmittedLine.FindString(facts); m != "" {
		text = strings.Replace(text, "\n\n## ", "\n> "+strings.Trim(m, "()")+"\n\n## ", 1)
	}
	path := filepath.Join(cwd, name)
	if err := os.WriteFile(path+".tmp", []byte(pyStrip(text)+"\n"), 0o644); err != nil {
		return "", "", err
	}
	if err := os.Rename(path+".tmp", path); err != nil {
		return "", "", err
	}
	stored := "stored"
	// The file is the handoff; the memory briefing is a convenience.
	if err := store(cwd, newObj("text", briefing(text, path), "handoff_file", path)); err != nil {
		stored = "not stored (" + err.Error() + ")"
	}
	if err := os.Remove(nextPath); err != nil {
		return "", "", err
	}
	return path, fmt.Sprintf("%s %s %.0fs $%.3f gaps=%d%s %s", session, path, time.Since(start).Seconds(), cost,
		len(found), fixedNote, stored), nil
}

func writeHandoff(args []string) int {
	if len(args) < 4 {
		fmt.Fprintln(os.Stderr, "usage: guardrails write-handoff <transcript.jsonl> <next-steps file> <cwd> <session id>")
		return 2
	}
	path, summary, err := writeHandoffFile(args[0], args[1], args[2], args[3])
	if err != nil {
		fmt.Fprintln(os.Stderr, "write-handoff:", err)
		return 1
	}
	fmt.Println(path)
	fmt.Fprintln(os.Stderr, summary)
	return 0
}
