package main

// context-guard runs the /clear at the end of the guardrails handoff chain, so it declares
// guardrails as a plugin dependency and Claude Code installs it alongside.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestContextGuardDependsOnGuardrails(t *testing.T) {
	root := filepath.Join(pluginDir, "../..")
	b, err := os.ReadFile(filepath.Join(root, "plugins/context-guard/.claude-plugin/plugin.json"))
	if err != nil {
		t.Fatal(err)
	}
	var plug struct {
		Dependencies []json.RawMessage `json:"dependencies"`
	}
	if err := json.Unmarshal(b, &plug); err != nil {
		t.Fatal(err)
	}
	// A bare string resolves in the same marketplace at its current copy. A version range
	// would need <plugin>--vX.Y.Z git tags, which the autorelease does not create.
	if len(plug.Dependencies) != 1 || string(plug.Dependencies[0]) != `"guardrails"` {
		t.Errorf(`context-guard dependencies = %s, want ["guardrails"]`, plug.Dependencies)
	}

	mb, err := os.ReadFile(filepath.Join(root, ".claude-plugin/marketplace.json"))
	if err != nil {
		t.Fatal(err)
	}
	var mkt struct {
		Plugins []struct {
			Name string `json:"name"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal(mb, &mkt); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range mkt.Plugins {
		found = found || p.Name == "guardrails"
	}
	if !found {
		t.Error("marketplace has no guardrails entry for the dependency to resolve to")
	}
}
