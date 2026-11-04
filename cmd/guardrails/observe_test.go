package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// corpusCommand returns the first corpus command whose current kind is want.
func corpusCommand(t *testing.T, want string) string {
	t.Helper()
	f, err := os.Open("testdata/guard-corpus.tsv")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if p := strings.SplitN(sc.Text(), "\t", 3); len(p) == 3 && p[1] == want {
			return strings.ReplaceAll(p[2], `\n`, "\n")
		}
	}
	t.Fatalf("no corpus row of kind %q", want)
	return ""
}

// hookRun feeds a real PreToolUse(Bash) payload through the hook entrypoint, with the ledger
// in its own state dir, and returns exit code, stdout, stderr and the session's ledger.
func hookRun(t *testing.T, observe, cmd string) (int, string, string, string) {
	t.Helper()
	t.Setenv("HANDOFF_STATE_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_TOOLS_OFF", "")
	t.Setenv("CLAUDE_TOOLS_OBSERVE", observe)
	payload := bashInput("PreToolUse", "obs1", t.TempDir(), cmd, "")
	var stdout, stderr bytes.Buffer
	code := preToolUseRun([]byte(payload), "CT", func([]byte) ([]byte, []byte, int) { return nil, nil, 0 }, &stdout, &stderr)
	ledger, _ := os.ReadFile(guardLedger("obs1"))
	return code, stdout.String(), stderr.String(), string(ledger)
}

func decisionOf(t *testing.T, stdout string) (ask bool, context string) {
	t.Helper()
	var o struct {
		H struct {
			PermissionDecision string `json:"permissionDecision"`
			AdditionalContext  string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if stdout != "" && json.Unmarshal([]byte(stdout), &o) != nil {
		t.Fatalf("stdout is not hook JSON: %q", stdout)
	}
	return o.H.PermissionDecision == "ask", o.H.AdditionalContext
}

func TestObserveAskBecomesAllowAndLedger(t *testing.T) {
	cmd := corpusCommand(t, "ask:git-reset-hard")

	// Off: the original decision.
	code, out, _, ledger := hookRun(t, "", cmd)
	if ask, _ := decisionOf(t, out); code != 0 || !ask {
		t.Fatalf("observe off: want ask, got exit %d stdout %q", code, out)
	}
	if !strings.Contains(ledger, "\task\tgit-reset-hard\n") || strings.Contains(ledger, "would-") {
		t.Errorf("observe off ledger = %q", ledger)
	}

	// On: allowed, noted, recorded as would-ask.
	code, out, errOut, ledger := hookRun(t, "1", cmd)
	ask, ctx := decisionOf(t, out)
	if code != 0 || ask || errOut != "" {
		t.Fatalf("observe on: want allow, got exit %d stdout %q stderr %q", code, out, errOut)
	}
	if !strings.Contains(ctx, "observe mode") || !strings.Contains(ctx, "git-reset-hard") {
		t.Errorf("context note = %q, want it to name observe mode and the rule", ctx)
	}
	if !strings.Contains(ledger, "\twould-ask\tgit-reset-hard\n") || strings.Contains(ledger, "\task\t") {
		t.Errorf("observe on ledger = %q, want one would-ask line", ledger)
	}
}

func TestObserveBlockInFamilyBecomesAllow(t *testing.T) {
	cmd := "echo '{}' > ~/.config/claude-tools/pii-columns.json"
	if code, _, _, _ := hookRun(t, "", cmd); code != 2 {
		t.Fatalf("observe off: want block (exit 2), got %d", code)
	}
	code, out, errOut, ledger := hookRun(t, "true", cmd)
	if code != 0 || errOut != "" {
		t.Fatalf("observe on: want allow, got exit %d stderr %q", code, errOut)
	}
	if _, ctx := decisionOf(t, out); !strings.Contains(ctx, "would have blocked") {
		t.Errorf("context = %q", ctx)
	}
	if !strings.Contains(ledger, "\twould-block\tpii-rules:redirect\n") {
		t.Errorf("ledger = %q", ledger)
	}
}

// The note says what the guard would have done and drops its instruction: the call already ran.
func TestObserveNoteDropsInstruction(t *testing.T) {
	for _, cmd := range []string{corpusCommand(t, "ask:git-reset-hard"), "echo '{}' > ~/.config/claude-tools/pii-columns.json"} {
		_, out, _, _ := hookRun(t, "1", cmd)
		_, ctx := decisionOf(t, out)
		for _, bad := range []string{"Confirm with the user", "run again", "ran anyway. ASK", "BLOCKED:", "ASK:"} {
			if strings.Contains(ctx, bad) {
				t.Errorf("note %q still carries %q", ctx, bad)
			}
		}
		if !strings.Contains(ctx, "would have asked:") && !strings.Contains(ctx, "would have blocked:") {
			t.Errorf("note = %q, want 'would have asked:' or 'would have blocked:'", ctx)
		}
	}
}

// Leaks cannot be taken back: the session-url block and a secret in outbound data stay enforced.
func TestObserveKeepsIrreversibleLeakGuards(t *testing.T) {
	code, _, _, ledger := hookRun(t, "1", `git commit -m "see https://claude.ai/code/session_abc123"`)
	if code != 2 || !strings.Contains(ledger, "\tblock\tsession-url\n") || strings.Contains(ledger, "would-") {
		t.Errorf("session-url: exit %d ledger %q, want the block", code, ledger)
	}
	// Other egress asks are observed.
	saved := egressOwn
	egressOwn = func() ([]string, string) { return []string{"me@example.com"}, "example.com" }
	defer func() { egressOwn = saved }()
	code, out, _, ledger := hookRun(t, "1", `gh issue create --title x --body "mail jane@acme-client.com"`)
	if ask, _ := decisionOf(t, out); code != 0 || ask || !strings.Contains(ledger, "\twould-ask\tegress:email\n") {
		t.Errorf("egress email: exit %d out %q ledger %q, want would-ask", code, out, ledger)
	}
}

// A would-event is recorded only when the call goes through: an enforced guard that stops the
// call later in decide() leaves no would-line.
func TestObserveLogsWouldOnlyWhenAllowed(t *testing.T) {
	cmd := "echo '{}' > ~/.config/claude-tools/pii-columns.json; rm -rf /"
	code, _, _, ledger := hookRun(t, "1", cmd)
	if code != 2 || strings.Contains(ledger, "would-") || !strings.Contains(ledger, "\tblock\t") {
		t.Errorf("exit %d ledger %q, want only the enforced block", code, ledger)
	}
	_, out, _, ledger := hookRun(t, "1", "echo '{}' > ~/.config/claude-tools/pii-columns.json; sed -i s/a/b/ ~/.claude/settings.json")
	if ask, _ := decisionOf(t, out); !ask || strings.Contains(ledger, "would-") {
		t.Errorf("out %q ledger %q, want only the enforced settings ask", out, ledger)
	}
}

// Non-Bash tools (MCP sends, edits of the PII rules file) honour observe too.
func TestObserveNonBashTools(t *testing.T) {
	saved := egressOwn
	egressOwn = func() ([]string, string) { return []string{"me@example.com"}, "example.com" }
	defer func() { egressOwn = saved }()
	run := func(observe, tool string, input map[string]any) (string, string) {
		t.Helper()
		t.Setenv("HANDOFF_STATE_DIR", t.TempDir())
		t.Setenv("HOME", t.TempDir())
		t.Setenv("CLAUDE_TOOLS_OFF", "")
		t.Setenv("CLAUDE_TOOLS_OBSERVE", observe)
		in, _ := json.Marshal(map[string]any{"session_id": "obs1", "cwd": t.TempDir(), "tool_name": tool, "tool_input": input})
		var stdout, stderr bytes.Buffer
		preToolUseRun(in, "CT", func([]byte) ([]byte, []byte, int) { return nil, nil, 0 }, &stdout, &stderr)
		ledger, _ := os.ReadFile(guardLedger("obs1"))
		return stdout.String(), string(ledger)
	}
	mail := map[string]any{"to": "jane@acme-client.com", "body": "hi"}
	if out, _ := run("", "mcp__claude_ai_Gmail__send_message", mail); !strings.Contains(out, `"ask"`) {
		t.Fatalf("observe off: want ask, got %q", out)
	}
	out, ledger := run("1", "mcp__claude_ai_Gmail__send_message", mail)
	if strings.Contains(out, `"ask"`) || !strings.Contains(out, "would have asked:") || !strings.Contains(ledger, "\twould-ask\tegress:email\n") {
		t.Errorf("mcp egress: out %q ledger %q, want allow + would-ask", out, ledger)
	}

	// A secret in an MCP send is an irreversible leak: it stays an ask.
	token := "ghp_" + strings.Repeat("aB3dE5fG7h", 4)[:36]
	out, ledger = run("1", "mcp__claude_ai_Atlassian_Rovo__createJiraIssue", map[string]any{"description": "use " + token})
	if !strings.Contains(out, `"ask"`) || !strings.Contains(ledger, "\task\tegress:secret\n") || strings.Contains(ledger, "would-") {
		t.Errorf("mcp secret: out %q ledger %q, want the ask", out, ledger)
	}

	rules := map[string]any{"file_path": "~/.config/claude-tools/pii-columns.json", "content": "{}"}
	if out, _ := run("", "Write", rules); out != "" {
		t.Fatalf("observe off: the deny goes to stderr, got stdout %q", out)
	}
	out, ledger = run("1", "Write", rules)
	if !strings.Contains(out, "would have blocked:") || !strings.Contains(ledger, "\twould-block\tpii-rules:Write\n") {
		t.Errorf("rules file: out %q ledger %q, want allow + would-block", out, ledger)
	}
}

// The core blocks have no switch (docs/guardrails/options.md): observe leaves them on.
func TestObserveKeepsCoreBlocks(t *testing.T) {
	for _, kind := range []string{"block:dangerous:rm -rf /", "block:dangerous:git push --force", "block:privileged-container"} {
		cmd := corpusCommand(t, kind)
		code, _, _, ledger := hookRun(t, "1", cmd)
		if code != 2 || !strings.Contains(ledger, "\tblock\t") || strings.Contains(ledger, "would-") {
			t.Errorf("%s: observe on gave exit %d ledger %q, want the block", kind, code, ledger)
		}
	}
}

// An observed rule must not hide a later enforced one: the secret-file mask still applies.
func TestObserveDoesNotSuppressLaterRules(t *testing.T) {
	_, out, _, _ := hookRun(t, "1", "git stash && cat ~/.aws/credentials")
	var o struct {
		H struct {
			UpdatedInput struct {
				Command string `json:"command"`
			} `json:"updatedInput"`
		} `json:"hookSpecificOutput"`
	}
	json.Unmarshal([]byte(out), &o)
	if !strings.Contains(o.H.UpdatedInput.Command, "CT") {
		t.Errorf("the secret read must still be masked in observe mode, got %q", out)
	}
}

func TestObserveOn(t *testing.T) {
	for v, want := range map[string]bool{"true": true, "TRUE": true, "1": true, "yes": true, "": false, "false": false, "0": false, "no": false} {
		if observeOn(v) != want {
			t.Errorf("observeOn(%q) = %v, want %v", v, !want, want)
		}
	}
}
