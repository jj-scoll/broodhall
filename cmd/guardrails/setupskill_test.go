package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The command /guardrails:setup offers as its demo. It copies nothing from /dev/null to
// /dev/null, so it is harmless if a guard failed to stop it.
const setupDemoCommand = "dd if=/dev/null of=/dev/null count=0"

func TestSetupDemoCommandIsBlockedByTheCore(t *testing.T) {
	code, _, stderr := runHook(t, preToolUse, bashInput("PreToolUse", "demo", t.TempDir(), setupDemoCommand, ""))
	if code != 2 || !strings.Contains(stderr, "BLOCKED: 'dd if='") {
		t.Errorf("demo command: exit %d, stderr %q; want exit 2 and the dd block", code, stderr)
	}
}

// The skill must carry the demo command and drive the real subcommands, so a rename of either
// breaks this test instead of the user's first run.
func TestSetupSkillMatchesTheBinary(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(pluginDir, "skills", "setup", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.HasPrefix(s, "---\nname: setup\n") {
		t.Errorf("SKILL.md must start with frontmatter naming the skill setup:\n%.80s", s)
	}
	for _, want := range []string{
		setupDemoCommand,
		`run.sh" setup `,
		`run.sh" doctor`,
		"pii-rules init",
		"-write",
		"-profile standard",
		"-profile observe",
		"-domains",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("SKILL.md lacks %q", want)
		}
	}
	for _, sub := range []string{"setup", "doctor", "pii-rules"} {
		if commands[sub] == nil {
			t.Errorf("skill names the subcommand %q, which the binary lacks", sub)
		}
	}
}
