package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// In auto mode a denied rewrite comes back as the already-rewritten command. The hook must
// recognise exactly the wrapper it would emit: the guardrails binary it is running as,
// `redact --secrets --`, then a reader the mask accepts. Every other shape is judged as if
// the wrapper were not there.
func TestSecretWrapperRecognised(t *testing.T) {
	env := secretEnv(t)
	const self = "/opt/bin/guardrails"
	env.Self = self
	f := "~/.config/mark.toml"
	w := self + " redact --secrets -- "
	for _, c := range []struct{ name, cmd, want string }{
		{"wrapper alone", w + "cat " + f, "allow"},
		{"quoted self", "'" + self + "' redact --secrets -- cat " + f, "allow"},
		{"grep, trailing stage", w + "grep -n x " + f + " | head -5", "allow"},
		{"env assignment before the wrapper", "FOO=1 " + w + "cat " + f, "allow"},
		{"output redirected, as the rewrite leaves it", w + "cat " + f + " > /tmp/o", "allow"},
		{"command substitution", "echo $(" + w + "cat " + f + ")", "allow"},
		{"stdin redirect", w + "cat < " + f, "allow"},

		{"trailing stage reads the file unmasked", w + "cat " + f + " | cat " + f, "context:secret-file:mask"},
		{"&& chained unwrapped read", w + "cat " + f + " && cat " + f, "context:secret-file:mask"},
		{"; chained python reads it", w + "cat " + f + `; python3 -c "open('` + env.Home + `/.config/mark.toml')"`, "ask:secret-file:text"},
		{"trailing stage with a non-reader", w + "cat " + f + " | base64 " + f, "ask:secret-file:base64"},

		{"sudo before the wrapper", "sudo " + w + "cat " + f, "ask:secret-file:sudo"},
		{"env before the wrapper", "env FOO=1 " + w + "cat " + f, "ask:secret-file:env"},
		{"other binary named guardrails", "/tmp/guardrails redact --secrets -- cat " + f, "ask:secret-file:guardrails"},
		{"bare guardrails from PATH", "guardrails redact --secrets -- cat " + f, "ask:secret-file:guardrails"},
		{"older name, other path", "/opt/bin/claude-tools redact --secrets -- cat " + f, "ask:secret-file:claude-tools"},
		{"unclean path to self", self + "/../guardrails redact --secrets -- cat " + f, "ask:secret-file:guardrails"},
		{"non-reader behind the wrapper", w + "base64 " + f, "ask:secret-file:guardrails"},
		{"reader flags the mask cannot cover", w + "cat -A " + f, "ask:secret-file:guardrails"},
		{"grep -o drops the key", w + "grep -o 'password.*' " + f, "ask:secret-file:guardrails"},
		{"pii wrapper is not the secret mask", self + " redact --pii -- cat " + f, "ask:secret-file:guardrails"},
		{"extra redact flag", self + " redact --secrets --private -- cat " + f, "ask:secret-file:guardrails"},
		{"no double dash", self + " redact --secrets cat " + f, "ask:secret-file:guardrails"},
		{"other subcommand", self + " doctor --secrets -- cat " + f, "ask:secret-file:guardrails"},
		{"bash -c around the wrapper", "bash -c '" + w + "cat " + f + "'", "ask:secret-file:text"},
		{"wrapper around nothing protected", w + "cat ~/.config/other.toml", "allow"},
	} {
		if got := decide(env, c.cmd).Kind(); got != c.want {
			t.Errorf("%s: %q: got %s, want %s", c.name, c.cmd, got, c.want)
		}
	}
	// Not wrapped twice.
	for _, cmd := range []string{w + "cat " + f, w + "grep x " + f + " | head"} {
		if got, ok := secretRewrite(env, cmd, self); ok {
			t.Errorf("rewrote the wrapper again: %q", got)
		}
	}
	if got, ok := secretRewrite(env, w+"cat "+f+" && cat "+f, self); !ok || got != w+"cat "+f+" && "+w+"cat "+f {
		t.Errorf("chained read: %q %v", got, ok)
	}
	// A hook that does not know its own path recognises nothing.
	env.Self = ""
	if got := decide(env, w+"cat "+f).Kind(); got != "ask:secret-file:guardrails" {
		t.Errorf("no self: got %s", got)
	}
}

// The hook itself passes its own path, and a re-issued wrapper goes through with no ask.
func TestPreToolUseAllowsOwnSecretWrapper(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	const self = "/opt/bin/guardrails"
	run := func(cmd string) string {
		in, _ := json.Marshal(map[string]any{"session_id": "s", "cwd": t.TempDir(), "tool_name": "Bash",
			"tool_input": map[string]any{"command": cmd}})
		var stdout, stderr bytes.Buffer
		preToolUseRun(in, self, func([]byte) ([]byte, []byte, int) { return nil, nil, 0 }, &stdout, &stderr)
		return stdout.String()
	}
	if out := run(self + " redact --secrets -- cat ~/.aws/credentials"); strings.Contains(out, `"ask"`) || strings.Contains(out, "updatedInput") {
		t.Errorf("own wrapper: %s", out)
	}
	if out := run("/tmp/guardrails redact --secrets -- cat ~/.aws/credentials"); !strings.Contains(out, `"ask"`) {
		t.Errorf("foreign wrapper not asked: %s", out)
	}
}
