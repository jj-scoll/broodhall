package main

// The public repo, marketplace and plugin names are pinned here so a rename cannot drift.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestBroodhallNames(t *testing.T) {
	root := filepath.Join(pluginDir, "../..")
	read := func(rel string) []byte {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	files := map[string][]byte{
		".claude-plugin/marketplace.json":               read(".claude-plugin/marketplace.json"),
		"plugins/guardrails/.claude-plugin/plugin.json": read("plugins/guardrails/.claude-plugin/plugin.json"),
		"plugins/guardrails/scripts/run.sh":             read("plugins/guardrails/scripts/run.sh"),
	}

	var mkt struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(files[".claude-plugin/marketplace.json"], &mkt); err != nil {
		t.Fatal(err)
	}
	if mkt.Name != "broodhall" {
		t.Errorf("marketplace name = %q, want %q", mkt.Name, "broodhall")
	}

	var plug struct {
		Name       string `json:"name"`
		Repository string `json:"repository"`
	}
	if err := json.Unmarshal(files["plugins/guardrails/.claude-plugin/plugin.json"], &plug); err != nil {
		t.Fatal(err)
	}
	if plug.Name != "guardrails" {
		t.Errorf("plugin name = %q, want guardrails", plug.Name)
	}
	if plug.Repository != "https://github.com/jj-scoll/broodhall" {
		t.Errorf("plugin repository = %q", plug.Repository)
	}

	if !strings.Contains(string(files["plugins/guardrails/scripts/run.sh"]), "https://github.com/jj-scoll/broodhall/releases/download") {
		t.Error("run.sh does not default to the broodhall release URL")
	}

	// The guard masks Bash reads of secret files and blocks the Read tool; it asks only for Edit/Write.
	for name, b := range files {
		if strings.HasSuffix(name, ".json") && strings.Contains(string(b), "secret-file reads") {
			t.Errorf("%s description still says the plugin asks before secret-file reads", name)
		}
	}

	// Read-dedup and the retry breaker only add a note, and the Edit/Write ask is part of the secret guard.
	for name, b := range files {
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		if strings.Contains(string(b), "stops repeated") {
			t.Errorf("%s description still says the plugin stops repeated Reads", name)
		}
		if !strings.Contains(string(b), "asks before writes") {
			t.Errorf("%s description does not say the guard asks before writes to secret files", name)
		}
	}

	// Only secretguard's Bash read mask is on by default; the pii output mask is off by default,
	// so no summary may claim that output in general is masked.
	for _, rel := range []string{
		".claude-plugin/marketplace.json",
		"plugins/guardrails/.claude-plugin/plugin.json",
		"plugins/guardrails/README.md",
		"export/guardrails/_overlay/README.md",
	} {
		b, err := os.ReadFile(filepath.Join(root, rel))
		if os.IsNotExist(err) { // the overlay exists only in the private tree
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "secrets in output") {
			t.Errorf("%s still claims the plugin masks secrets in output", rel)
		}
	}

	// Built by concatenation so the old names never appear literally in the exported tree,
	// which a sweep and the site check forbid.
	for _, old := range []string{"jj-scoll/" + "guardrails", "@" + "jj-scoll"} {
		for name, b := range files {
			if strings.Contains(string(b), old) {
				t.Errorf("%s still contains %q", name, old)
			}
		}
	}
}

// All plugins ship under one version (the release tag is v<version>), and Claude Code only
// delivers a plugin update when its version changes, so the versions move in lockstep.
func TestPluginVersionsShared(t *testing.T) {
	root := filepath.Join(pluginDir, "../..")
	paths, err := filepath.Glob(filepath.Join(root, "plugins/*/.claude-plugin/plugin.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) < 3 {
		t.Fatalf("found %d plugin.json files, want at least 3: %v", len(paths), paths)
	}
	semver := regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
	first := ""
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		var plug struct {
			Version string `json:"version"`
		}
		if err := json.Unmarshal(b, &plug); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if !semver.MatchString(plug.Version) {
			t.Errorf("%s: version %q is not X.Y.Z", p, plug.Version)
		}
		if first == "" {
			first = plug.Version
		} else if plug.Version != first {
			t.Errorf("%s: version %q differs from %q; all plugins share one version", p, plug.Version, first)
		}
	}
}

// The rtk plugin lives in the jj-scoll/rtk fork, not in this repo: a git-subdir source
// checks out only its claude-plugin/ directory, from the fork's develop branch.
func TestMarketplaceRTKEntry(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(pluginDir, "../..", ".claude-plugin/marketplace.json"))
	if err != nil {
		t.Fatal(err)
	}
	var mkt struct {
		Plugins []struct {
			Name   string          `json:"name"`
			Source json.RawMessage `json:"source"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal(b, &mkt); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"source": "git-subdir", "url": "jj-scoll/rtk", "path": "claude-plugin", "ref": "develop"}
	for _, p := range mkt.Plugins {
		if p.Name != "rtk" {
			continue
		}
		var got map[string]string
		if err := json.Unmarshal(p.Source, &got); err != nil {
			t.Fatalf("rtk source is not an object of strings: %s", p.Source)
		}
		if len(got) != len(want) {
			t.Errorf("rtk source = %v, want %v", got, want)
		}
		for k, v := range want {
			if got[k] != v {
				t.Errorf("rtk source %s = %q, want %q", k, got[k], v)
			}
		}
		return
	}
	t.Error("marketplace.json has no rtk plugin entry")
}
