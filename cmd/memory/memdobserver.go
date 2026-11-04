package main

// memory observer: one queued tool use becomes one validated, grounded observation via
// `claude -p` (Haiku) with forced JSON structure. Outcomes: done, rejected (valid JSON that
// fails validation twice; never defaulted), unknown (auth/transport/parse failure; the
// caller leaves the row queued so an outage loses nothing).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	observerModel   = "claude-haiku-5-5"
	observerTimeout = 180 * time.Second
	// anchorMinRatio is the share of a fact's tokens that must occur in the payload.
	anchorMinRatio = 0.5
)

// observation is the validated observer output.
type observation struct {
	Type          string   `json:"type"`
	Title         string   `json:"title"`
	Subtitle      string   `json:"subtitle"`
	Narrative     string   `json:"narrative"`
	Facts         []string `json:"facts"`
	Concepts      []string `json:"concepts"`
	FilesRead     []string `json:"files_read"`
	FilesModified []string `json:"files_modified"`
}

// observerUsage is observer token use and cost, summed over every call made for one item.
type observerUsage struct {
	InputTokens, OutputTokens, CacheReadTokens, CacheCreationTokens int64
	CostUSD                                                         float64
}

func (u *observerUsage) add(o observerUsage) {
	u.InputTokens += o.InputTokens
	u.OutputTokens += o.OutputTokens
	u.CacheReadTokens += o.CacheReadTokens
	u.CacheCreationTokens += o.CacheCreationTokens
	u.CostUSD += o.CostUSD
}

// observerArgv is the verified minimal-context argv (argv[0] is the binary). It has no
// --bare, which refuses subscription auth. The empty values of --setting-sources and --tools
// are deliberate.
func observerArgv(systemPrompt, schema string) []string {
	return []string{
		"claude", "-p",
		"--setting-sources", "",
		"--strict-mcp-config",
		"--no-session-persistence",
		"--disable-slash-commands",
		"--tools", "",
		"--effort", "low",
		"--exclude-dynamic-system-prompt-sections",
		"--model", observerModel,
		"--system-prompt", systemPrompt,
		"--output-format", "json",
		"--json-schema", schema,
	}
}

// runClaude runs the CLI and returns stdout. It is a var so tests never start the real
// binary. The default sets the recursion-guard env and cwd itself (see observerCmd).
var runClaude = func(ctx context.Context, argv []string, stdin []byte) ([]byte, error) {
	cmd, err := observerCmd(ctx, argv, stdin)
	if err != nil {
		return nil, err
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		// A signed-out CLI exits 1; stdout (if any) still goes back so callers see the reason.
		return stdout.Bytes(), fmt.Errorf("claude: %w: %s", err, cut(stderr.String(), 300))
	}
	return stdout.Bytes(), nil
}

// observerCmd builds the exec.Cmd: env MEMORY_OBSERVER=1 marks the session as the observer's,
// and cwd memdDir()/observer-cwd (created here) makes its project dir recognisable. The daemon
// serves every account, so MEMORY_OBSERVER_CONFIG_DIR, when set, is the one account
// (CLAUDE_CONFIG_DIR) every observer and summary call runs as, whichever account started it.
func observerCmd(ctx context.Context, argv []string, stdin []byte) (*exec.Cmd, error) {
	dir := filepath.Join(memdDir(), "observer-cwd")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("observer cwd: %w", err)
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	env := os.Environ()
	if cfg := os.Getenv("MEMORY_OBSERVER_CONFIG_DIR"); cfg != "" {
		env = slices.DeleteFunc(env, func(e string) bool { return strings.HasPrefix(e, "CLAUDE_CONFIG_DIR=") })
		env = append(env, "CLAUDE_CONFIG_DIR="+cfg)
	}
	cmd.Env = append(env, "MEMORY_OBSERVER=1")
	cmd.Stdin = bytes.NewReader(stdin)
	// A child that outlives the kill and keeps stdout open must not hang Run after ctx ends.
	cmd.WaitDelay = 5 * time.Second
	return cmd, nil
}

// toolTag matches any opening or closing delimiter-like tag start, case-insensitively and
// with optional whitespace after "<" and "</".
var toolTag = regexp.MustCompile(`(?i)<\s*(/?)\s*tool_`)

// escapePayload keeps tool text from closing or reopening the data block (deterministic).
func escapePayload(s string) string { return toolTag.ReplaceAllString(s, `<\${1}tool_`) }

// capPayload keeps the first head and last tail characters of s, joined by a marker, and
// reports whether it cut anything. Callers redact first so a cut cannot expose part of a secret.
func capPayload(s string, head, tail int) (string, bool) {
	if utf8.RuneCountInString(s) <= head+tail {
		return s, false
	}
	r := []rune(s)
	n := len(r) - head - tail
	return string(r[:head]) + fmt.Sprintf("\n[... %d characters truncated ...]\n", n) + string(r[len(r)-tail:]), true
}

// observerPayload is the data-delimited block sent on stdin (and hashed for the dedupe key):
// redact, then cap, then escape. Deterministic for a given item.
func observerPayload(it *queueItem) (payload string, truncated bool) {
	field := func(s string) string {
		c, t := capPayload(memdRedact(s), obsPayloadHead, obsPayloadTail)
		truncated = truncated || t
		return escapePayload(c)
	}
	name := escapePayload(memdRedact(it.ToolName))
	in, out := field(it.Input), field(it.Output)
	return "<tool_use>\n<tool_name>" + name + "</tool_name>\n<tool_input>\n" + in +
		"\n</tool_input>\n<tool_output>\n" + out + "\n</tool_output>\n</tool_use>\n", truncated
}

// claudeResult is the part of `claude -p --output-format json` that observe reads.
type claudeResult struct {
	IsError          bool            `json:"is_error"`
	Result           string          `json:"result"`
	TotalCostUSD     float64         `json:"total_cost_usd"`
	StructuredOutput json.RawMessage `json:"structured_output"`
	Usage            struct {
		InputTokens         int64 `json:"input_tokens"`
		OutputTokens        int64 `json:"output_tokens"`
		CacheReadTokens     int64 `json:"cache_read_input_tokens"`
		CacheCreationTokens int64 `json:"cache_creation_input_tokens"`
	} `json:"usage"`
}

func (r *claudeResult) usage() observerUsage {
	return observerUsage{r.Usage.InputTokens, r.Usage.OutputTokens, r.Usage.CacheReadTokens, r.Usage.CacheCreationTokens, r.TotalCostUSD}
}

// Unknown reason classes (R24). The worker's health and attempt cap key on them.
const (
	reasonAuth        = "auth"
	reasonTransport   = "transport"
	reasonCLIError    = "cli_error"
	reasonNoStructure = "missing_structured_output"
)

var authMarkers = []string{"not logged in", "/login", "invalid api key", "oauth", "authentication"}

func hasAuthText(texts ...string) bool {
	for _, t := range texts {
		l := strings.ToLower(t)
		for _, m := range authMarkers {
			if strings.Contains(l, m) {
				return true
			}
		}
	}
	return false
}

// structuredOnce makes one CLI call and returns the raw structured_output. ok=false means
// unknown (no usable result) and reason is one of the class constants above. Stdout is
// parsed even when the CLI exits non-zero, because a signed-out CLI explains itself there.
func structuredOnce(ctx context.Context, stdin string, argv []string) (so json.RawMessage, reason string, usage observerUsage, ok bool) {
	ctx, cancel := context.WithTimeout(ctx, observerTimeout)
	defer cancel()
	out, err := runClaude(ctx, argv, []byte(stdin))
	var res claudeResult
	parsed := len(bytes.TrimSpace(out)) > 0 && json.Unmarshal(out, &res) == nil
	errText := ""
	if err != nil {
		errText = err.Error()
	}
	if parsed {
		usage = res.usage()
	}
	switch {
	case !parsed:
		// Empty or unparseable stdout: raw text may still say "Not logged in".
		if hasAuthText(string(out), errText) {
			return nil, reasonAuth, usage, false
		}
		return nil, reasonTransport, usage, false
	case err != nil:
		if hasAuthText(res.Result, errText) {
			return nil, reasonAuth, usage, false
		}
		if res.IsError {
			return nil, reasonCLIError, usage, false
		}
		return nil, reasonTransport, usage, false
	case res.IsError:
		if hasAuthText(res.Result) {
			return nil, reasonAuth, usage, false
		}
		return nil, reasonCLIError, usage, false
	}
	if len(res.StructuredOutput) == 0 || string(res.StructuredOutput) == "null" {
		// R20: the CLI enforces --json-schema, so no structured_output is a CLI-level
		// failure, not model output: unknown (row stays queued), never retried or rejected.
		return nil, reasonNoStructure, usage, false
	}
	return res.StructuredOutput, "", usage, true
}

// observeOnce makes one observer call. ok=false means unknown (reason is a class constant).
// Otherwise it returns the validated observation, or the machine-readable validation failure
// in reason.
func observeOnce(ctx context.Context, stdin string, argv []string) (obs *observation, reason string, usage observerUsage, ok bool) {
	so, reason, usage, ok := structuredOnce(ctx, stdin, argv)
	if !ok {
		return nil, reason, usage, false
	}
	var o observation
	if json.Unmarshal(so, &o) != nil {
		return nil, "invalid_shape", usage, true
	}
	if reason := validateObservation(&o); reason != "" {
		return nil, reason, usage, true
	}
	return &o, "", usage, true
}

// validateObservation enforces the schema's enum and caps in Go (the CLI's own enforcement
// is bypassed by test fakes and is not a guarantee). It returns "" or a reason code.
func validateObservation(o *observation) string {
	valid := false
	for _, t := range observerTypes {
		valid = valid || o.Type == t
	}
	switch {
	case !valid:
		return "invalid_type"
	case strings.TrimSpace(o.Title) == "":
		return "empty_title"
	case utf8.RuneCountInString(o.Title) > obsTitleMax:
		return "title_too_long"
	case utf8.RuneCountInString(o.Subtitle) > obsSubtitleMax:
		return "subtitle_too_long"
	case strings.TrimSpace(o.Narrative) == "":
		return "empty_narrative"
	case utf8.RuneCountInString(o.Narrative) > obsNarrativeMax:
		return "narrative_too_long"
	}
	return ""
}

// observe runs the observer for one queued item. outcome is done, rejected or unknown.
// Validation failures get exactly one retry whose stdin carries the error; an unknown result
// on either call is unknown. usage is summed over every call made.
func observe(ctx context.Context, it *queueItem) (obs *observation, drops []string, outcome string, reason string, usage observerUsage) {
	argv := observerArgv(observerSystemPrompt, observerSchema())
	payload, _ := observerPayload(it)
	stdin := observerStdinHeader + payload
	for attempt := 0; attempt < 2; attempt++ {
		o, why, u, ok := observeOnce(ctx, stdin, argv)
		usage.add(u)
		if !ok {
			return nil, nil, "unknown", why, usage
		}
		if why == "" {
			kept, drops := anchored(o, payload)
			return kept, drops, "done", "", usage
		}
		reason = why
		stdin = observerStdinHeader + payload +
			"\nYour previous reply failed validation (" + why + "). Reply again with a corrected object.\n"
	}
	return nil, nil, "rejected", reason, usage
}

// tokens returns the lowercased alphanumeric runs of s with at least 3 characters.
func tokens(s string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		if utf8.RuneCountInString(f) >= 3 {
			out = append(out, f)
		}
	}
	return out
}

// anchored returns a copy of obs keeping only grounded items. A fact is kept when at least
// anchorMinRatio of its alphanumeric tokens (3+ chars) appear in the payload; comparison is
// case-insensitive. A fact with no such tokens cannot be grounded and is dropped. A file is
// kept only if its path appears verbatim in the payload. Each drop is "fact:<text>" or
// "file:<path>".
func anchored(obs *observation, payload string) (kept *observation, drops []string) {
	have := map[string]bool{}
	for _, t := range tokens(payload) {
		have[t] = true
	}
	k := *obs
	k.Facts = nil
	for _, f := range obs.Facts {
		toks := tokens(f)
		hit := 0
		for _, t := range toks {
			if have[t] {
				hit++
			}
		}
		if len(toks) > 0 && float64(hit)/float64(len(toks)) >= anchorMinRatio {
			k.Facts = append(k.Facts, f)
		} else {
			drops = append(drops, "fact:"+f)
		}
	}
	keepFiles := func(files []string) []string {
		var out []string
		for _, f := range files {
			if f != "" && strings.Contains(payload, f) {
				out = append(out, f)
			} else {
				drops = append(drops, "file:"+f)
			}
		}
		return out
	}
	k.FilesRead = keepFiles(obs.FilesRead)
	k.FilesModified = keepFiles(obs.FilesModified)
	return &k, drops
}
