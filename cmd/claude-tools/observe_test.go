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
	cmd := `git commit -m "see https://claude.ai/code/session_abc123"`
	if code, _, _, _ := hookRun(t, "", cmd); code != 2 {
		t.Fatalf("observe off: want block (exit 2), got %d", code)
	}
	code, out, errOut, ledger := hookRun(t, "true", cmd)
	if code != 0 || errOut != "" {
		t.Fatalf("observe on: want allow, got exit %d stderr %q", code, errOut)
	}
	if _, ctx := decisionOf(t, out); !strings.Contains(ctx, "would have been blocked") {
		t.Errorf("context = %q", ctx)
	}
	if !strings.Contains(ledger, "\twould-block\tsession-url\n") {
		t.Errorf("ledger = %q", ledger)
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
