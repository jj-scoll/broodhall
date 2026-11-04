package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runSetup(t *testing.T, dir string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	var out, errw bytes.Buffer
	code = setupRun(&out, &errw, append([]string{"-config-dir", dir}, args...))
	return out.String(), errw.String(), code
}

func readJSON(t *testing.T, p string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("%s is not valid JSON: %v\n%s", p, err, b)
	}
	return m
}

func guardOptions(t *testing.T, m map[string]any, id string) map[string]any {
	t.Helper()
	pc, _ := m["pluginConfigs"].(map[string]any)
	e, _ := pc[id].(map[string]any)
	o, _ := e["options"].(map[string]any)
	if o == nil {
		t.Fatalf("no pluginConfigs[%q].options in %v", id, m)
	}
	return o
}

func TestSetupDryRunWritesNothing(t *testing.T) {
	dir := t.TempDir()
	out, _, code := runSetup(t, dir, "-profile", "standard", "-domains", "acme.com")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "settings.json")); !os.IsNotExist(err) {
		t.Errorf("dry run created settings.json (stat err %v)", err)
	}
	for _, w := range []string{filepath.Join(dir, "settings.json"), "guardrails@broodhall", "allowed_email_domains", "@acme.com", "disabled_guards", "dry run", "-write"} {
		if !strings.Contains(out, w) {
			t.Errorf("preview lacks %q:\n%s", w, out)
		}
	}
}

func TestSetupWriteCreatesFile(t *testing.T) {
	dir := t.TempDir()
	out, _, code := runSetup(t, dir, "-profile", "standard", "-domains", "acme.com,dev@other.org", "-write")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	o := guardOptions(t, readJSON(t, filepath.Join(dir, "settings.json")), "guardrails@broodhall")
	if o["allowed_email_domains"] != "@acme.com,dev@other.org" || o["disabled_guards"] != "" || o["observe"] != false {
		t.Errorf("options = %v", o)
	}
	// The preview comes before the write line, so the user sees what will be written first.
	pv, wr := strings.Index(out, "allowed_email_domains"), strings.Index(out, "wrote ")
	if pv < 0 || wr < 0 || pv > wr {
		t.Errorf("want the preview before the 'wrote' line:\n%s", out)
	}
}

func TestSetupObserveProfile(t *testing.T) {
	dir := t.TempDir()
	if _, _, code := runSetup(t, dir, "-profile", "observe", "-write"); code != 0 {
		t.Fatal("exit", code)
	}
	o := guardOptions(t, readJSON(t, filepath.Join(dir, "settings.json")), "guardrails@broodhall")
	if o["observe"] != true || o["disabled_guards"] != "" {
		t.Errorf("observe profile options = %v", o)
	}
}

func TestSetupMergeKeepsEverythingElse(t *testing.T) {
	dir := t.TempDir()
	orig := `{
  "cleanupPeriodDays": 30,
  "bigNumber": 12345678901234567890,
  "permissions": {"allow": ["Bash(ls)"]},
  "enabledPlugins": {"guardrails@broodhall": true},
  "pluginConfigs": {
    "other@x": {"options": {"a": 1}},
    "guardrails@broodhall": {"options": {"rtk": true, "handoff_tokens": "9000"}, "mcpServers": {"s": {}}}
  }
}`
	p := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(p, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, code := runSetup(t, dir, "-profile", "standard", "-domains", "acme.com", "-write"); code != 0 {
		t.Fatal("exit", code)
	}
	raw, _ := os.ReadFile(p)
	for _, keep := range []string{`"cleanupPeriodDays": 30`, `12345678901234567890`, `Bash(ls)`, `"other@x"`, `"handoff_tokens": "9000"`, `"mcpServers"`} {
		if !strings.Contains(string(raw), keep) {
			t.Errorf("rewrite lost %q:\n%s", keep, raw)
		}
	}
	o := guardOptions(t, readJSON(t, p), "guardrails@broodhall")
	if o["rtk"] != true || o["allowed_email_domains"] != "@acme.com" {
		t.Errorf("options = %v", o)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600 kept", fi.Mode().Perm())
	}
	bak, err := os.ReadFile(p + ".guardrails-setup.bak")
	if err != nil || string(bak) != orig {
		t.Errorf("backup missing or different: %v", err)
	}
}

func TestSetupIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	args := []string{"-profile", "standard", "-domains", "acme.com", "-write"}
	runSetup(t, dir, args...)
	p := filepath.Join(dir, "settings.json")
	first, _ := os.ReadFile(p)
	os.Remove(p + ".guardrails-setup.bak") // a first-run file has no backup anyway
	out, _, code := runSetup(t, dir, args...)
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	second, _ := os.ReadFile(p)
	if !bytes.Equal(first, second) {
		t.Errorf("second run changed the file:\n%s\n---\n%s", first, second)
	}
	if !strings.Contains(out, "already up to date") {
		t.Errorf("want 'already up to date':\n%s", out)
	}
	if _, err := os.Stat(p + ".guardrails-setup.bak"); !os.IsNotExist(err) {
		t.Error("a no-change run wrote a backup")
	}
}

func TestSetupShowsOldAndNewValues(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"pluginConfigs":{"guardrails@broodhall":{"options":{"allowed_email_domains":"@old.com"}}}}`), 0o644)
	out, _, _ := runSetup(t, dir, "-domains", "new.com")
	if !strings.Contains(out, "@old.com") || !strings.Contains(out, "@new.com") {
		t.Errorf("preview should show the old and the new value:\n%s", out)
	}
}

func TestSetupOmittedFlagsLeaveOptionsAlone(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "settings.json")
	os.WriteFile(p, []byte(`{"pluginConfigs":{"guardrails@broodhall":{"options":{"observe":true,"allowed_email_domains":"@old.com"}}}}`), 0o644)
	out, _, code := runSetup(t, dir, "-write")
	if code != 0 || !strings.Contains(out, "already up to date") {
		t.Errorf("no flags: code %d\n%s", code, out)
	}
}

func TestSetupDetectsMarketplaceName(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "settings.json")
	os.WriteFile(p, []byte(`{"enabledPlugins":{"guardrails@team-bh":true}}`), 0o644)
	if _, _, code := runSetup(t, dir, "-profile", "standard", "-write"); code != 0 {
		t.Fatal("exit", code)
	}
	guardOptions(t, readJSON(t, p), "guardrails@team-bh")
}

func TestSetupRefusesBrokenSettings(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "settings.json")
	os.WriteFile(p, []byte(`{"hooks": `), 0o644)
	_, errw, code := runSetup(t, dir, "-profile", "standard", "-write")
	if code != 1 || !strings.Contains(errw, "settings.json") {
		t.Errorf("code %d err %q; want exit 1 naming the file", code, errw)
	}
	if b, _ := os.ReadFile(p); string(b) != `{"hooks": ` {
		t.Errorf("broken file was modified: %q", b)
	}
}

func TestSetupRejectsBadInput(t *testing.T) {
	for _, args := range [][]string{{"-profile", "yolo"}, {"-domains", "not a domain"}, {"-bogus"}, {"-marketplace", "a@b"}} {
		dir := t.TempDir()
		_, _, code := runSetup(t, dir, append(args, "-write")...)
		if code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
		if _, err := os.Stat(filepath.Join(dir, "settings.json")); !os.IsNotExist(err) {
			t.Errorf("%v: wrote a file", args)
		}
	}
}

// home-manager style: settings.json is a symlink. The write goes through it and keeps the link.
func TestSetupWritesThroughSymlink(t *testing.T) {
	dir, store := t.TempDir(), t.TempDir()
	target := filepath.Join(store, "real.json")
	os.WriteFile(target, []byte(`{"model":"x"}`), 0o644)
	link := filepath.Join(dir, "settings.json")
	if err := os.Symlink(target, link); err != nil {
		t.Skip("no symlinks:", err)
	}
	if _, _, code := runSetup(t, dir, "-profile", "standard", "-write"); code != 0 {
		t.Fatal("exit", code)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("symlink replaced: %v %v", fi, err)
	}
	m := readJSON(t, target)
	if m["model"] != "x" {
		t.Errorf("lost model: %v", m)
	}
	guardOptions(t, m, "guardrails@broodhall")
}

func TestSetupUsesClaudeConfigDirEnv(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	var out, errw bytes.Buffer
	if code := setupRun(&out, &errw, []string{"-profile", "standard", "-write"}); code != 0 {
		t.Fatalf("exit %d: %s", code, errw.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "settings.json")); err != nil {
		t.Error(err)
	}
}
