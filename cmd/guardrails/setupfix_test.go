package main

// Tests for the fixes to PR #49 found by the independent verifier: setup as a writer of
// settings.json (ask and config-gate), setup's file handling, doctor's scope filter, and
// the small matching fixes.

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- settings-file guard: setup -write is a write to settings.json ---

func TestSettingsGuardAsksOnSetupWrite(t *testing.T) {
	for _, cmd := range []string{
		`sh "/home/u/.claude/plugins/cache/broodhall/guardrails/1.6.0/scripts/run.sh" setup -profile observe -write`,
		`bash ${CLAUDE_PLUGIN_ROOT}/scripts/run.sh setup -domains evil.example.org -write`,
		`"${CLAUDE_PLUGIN_ROOT}/scripts/run.sh" setup -write`,
		`/home/u/.claude/plugins/data/guardrails-broodhall/bin/1.6.0/guardrails setup -domains evil.example.org -write`,
		`/home/u/.claude/plugins/data/guardrails-broodhall/bin/1.5.0/claude-tools setup -write`,
		`guardrails setup -profile observe -write`,
		`guardrails setup --write -profile observe`,
		`guardrails setup -write=true -profile observe`,
		`guardrails setup -profile observe -write=1`,
		`GUARDRAILS_X=1 guardrails setup -profile observe -write`,
		`env FOO=1 guardrails setup -write`,
		`exec guardrails setup -write`,
		`cd /tmp && guardrails setup -write`,
		`true; guardrails setup -write`,
		`sh -c 'guardrails setup -profile observe -write'`,
		`bash -c "cd /x; guardrails setup -write"`,
		`C:\bin\guardrails.exe setup -write`,
		`guardrails setup -config-dir /elsewhere -write`,
	} {
		if d := guardCmd(cmd); !d.Ask && d.Exit != 2 {
			t.Errorf("%s: %s; want an ask, as for any other model write to settings.json", cmd, d.Kind())
		}
	}
}

func TestSettingsGuardLeavesHarmlessSetupAlone(t *testing.T) {
	for _, cmd := range []string{
		`guardrails setup -profile standard`,
		`guardrails setup -profile observe -domains acme.com`,
		`guardrails setup -write=false`,
		`sh "${CLAUDE_PLUGIN_ROOT}/scripts/run.sh" setup -profile standard`,
		`npm run setup -- -write`,
		`make setup -write`,
		`./setup.sh -write`,
		`rg setup docs`,
		`echo "run guardrails setup -write later"`,
		`git commit -m "docs: guardrails setup -write"`,
		`guardrails doctor -write`,
		`guardrails team-init -managed`,
		`cat docs/guardrails/setup.md`,
	} {
		if d := guardCmd(cmd); d.Ask || d.Exit != 0 {
			t.Errorf("%s: %s; want no ask", cmd, d.Kind())
		}
	}
}

// --- config-gate: observe on and a new allowed domain weaken, like disabled_guards ---

func TestConfigGateWeakensOnObserveAndDomains(t *testing.T) {
	const on = `"enabledPlugins":{"guardrails@broodhall":true}`
	cfg := func(opts string) string {
		return `{` + on + `,"pluginConfigs":{"guardrails@broodhall":{"options":{` + opts + `}}}}`
	}
	tests := []struct {
		name, old, next string
		weak            bool
	}{
		{"observe off to on", cfg(`"observe":false`), cfg(`"observe":true`), true},
		{"observe unset to on", `{` + on + `}`, cfg(`"observe":true`), true},
		{"observe string true", cfg(`"observe":false`), cfg(`"observe":"true"`), true},
		{"observe string yes", cfg(`"observe":false`), cfg(`"observe":"yes"`), true},
		{"observe on to off", cfg(`"observe":true`), cfg(`"observe":false`), false},
		{"observe on stays on", cfg(`"observe":true`), cfg(`"observe":true`), false},
		{"observe false stays false", cfg(`"observe":false`), cfg(`"observe":false`), false},
		{"domain added", cfg(`"allowed_email_domains":"@acme.com"`), cfg(`"allowed_email_domains":"@acme.com,@evil.org"`), true},
		{"domain set from unset", `{` + on + `}`, cfg(`"allowed_email_domains":"@evil.org"`), true},
		{"domain removed", cfg(`"allowed_email_domains":"@acme.com,@evil.org"`), cfg(`"allowed_email_domains":"@acme.com"`), false},
		{"domain reordered", cfg(`"allowed_email_domains":"@a.com,@b.com"`), cfg(`"allowed_email_domains":"@b.com, @A.com"`), false},
		{"domain unchanged", cfg(`"allowed_email_domains":"@a.com"`), cfg(`"allowed_email_domains":"@a.com"`), false},
		{"domain emptied", cfg(`"allowed_email_domains":"@a.com"`), cfg(`"allowed_email_domains":""`), false},
		{"observe and domains together tightened", cfg(`"observe":true,"allowed_email_domains":"@a.com"`), cfg(`"observe":false`), false},
		{"other options changed", cfg(`"rtk":false`), cfg(`"rtk":true`), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			weak, _, err := settingsDiff([]byte(tt.old), []byte(tt.next))
			if err != nil {
				t.Fatal(err)
			}
			if (len(weak) > 0) != tt.weak {
				t.Errorf("weakened = %q, want weakened=%v", weak, tt.weak)
			}
		})
	}
}

// The gate only judges guardrails entries that were enabled; a project file that merely
// carries an observe option for another plugin passes.
func TestConfigGateObserveIgnoresOtherPlugins(t *testing.T) {
	old := `{"enabledPlugins":{"guardrails@broodhall":true}}`
	next := `{"enabledPlugins":{"guardrails@broodhall":true},"pluginConfigs":{"memory@broodhall":{"options":{"observe":true}}}}`
	if weak, _, _ := settingsDiff([]byte(old), []byte(next)); len(weak) != 0 {
		t.Errorf("weakened = %q", weak)
	}
}

// --- setup: file handling ---

func TestSetupRefusesNullFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "settings.json")
	os.WriteFile(p, []byte("null"), 0o600)
	var out, errw bytes.Buffer
	code := func() (c int) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("setup panicked on a settings.json of null: %v", r)
			}
		}()
		return setupRun(&out, &errw, []string{"-config-dir", dir, "-profile", "standard", "-write"})
	}()
	if b, _ := os.ReadFile(p); code != 1 || string(b) != "null" {
		t.Errorf("exit %d, file %q; want exit 1 and the file untouched", code, b)
	}
	if !strings.Contains(errw.String(), "settings.json") {
		t.Errorf("stderr should name the file: %q", errw.String())
	}
}

func TestSetupRefusesTrailingContent(t *testing.T) {
	for _, body := range []string{`{"a":1} {"b":2}`, `{"a":1}xyz`, "{\"a\":1}\n}", `{"a":1},`} {
		dir := t.TempDir()
		p := filepath.Join(dir, "settings.json")
		os.WriteFile(p, []byte(body), 0o600)
		_, _, code := runSetup(t, dir, "-profile", "standard", "-write")
		after, _ := os.ReadFile(p)
		if code != 1 || string(after) != body {
			t.Errorf("%q: exit %d, file now %s; want exit 1 and the file untouched", body, code, after)
		}
		if _, err := os.Stat(p + ".guardrails-setup.bak"); err == nil {
			t.Errorf("%q: wrote a backup for a refused file", body)
		}
	}
	// Trailing whitespace is fine.
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "settings.json"), []byte("{\"a\":1}\n\n  "), 0o600)
	if _, _, code := runSetup(t, dir, "-profile", "standard", "-write"); code != 0 {
		t.Errorf("trailing whitespace: exit %d", code)
	}
}

func TestSetupRefusesDanglingSymlink(t *testing.T) {
	dir, store := t.TempDir(), t.TempDir()
	target := filepath.Join(store, "not-yet.json")
	link := filepath.Join(dir, "settings.json")
	if err := os.Symlink(target, link); err != nil {
		t.Skip(err)
	}
	_, _, code := runSetup(t, dir, "-profile", "standard", "-write")
	fi, err := os.Lstat(link)
	if code != 1 || err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("exit %d, lstat %v %v; want exit 1 and the dangling link kept", code, fi, err)
	}
	if _, err := os.Stat(target); err == nil {
		t.Error("created the link's target")
	}
}

func TestSetupRefusesReadOnlyFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file modes")
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "settings.json")
	os.WriteFile(p, []byte(`{"a":1}`), 0o444)
	_, errw, code := runSetup(t, dir, "-profile", "standard", "-write")
	b, _ := os.ReadFile(p)
	fi, _ := os.Stat(p)
	if code != 1 || string(b) != `{"a":1}` || fi.Mode().Perm() != 0o444 {
		t.Errorf("exit %d, file %q mode %v; want exit 1 and the read-only file untouched", code, b, fi.Mode().Perm())
	}
	if !strings.Contains(errw, "read-only") {
		t.Errorf("stderr should say the file is read-only: %q", errw)
	}
	if _, err := os.Stat(p + ".guardrails-setup.bak"); err == nil {
		t.Error("wrote a backup for a refused file")
	}
}

func TestSetupBackupDoesNotFollowSymlink(t *testing.T) {
	dir, store := t.TempDir(), t.TempDir()
	victim := filepath.Join(store, "victim.txt")
	os.WriteFile(victim, []byte("precious"), 0o600)
	p := filepath.Join(dir, "settings.json")
	orig := `{"a":1}`
	os.WriteFile(p, []byte(orig), 0o600)
	if err := os.Symlink(victim, p+".guardrails-setup.bak"); err != nil {
		t.Skip(err)
	}
	_, _, code := runSetup(t, dir, "-profile", "standard", "-write")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if b, _ := os.ReadFile(victim); string(b) != "precious" {
		t.Errorf("backup was written through the symlink: victim = %q", b)
	}
	if b, _ := os.ReadFile(p + ".guardrails-setup.bak"); string(b) != orig {
		t.Errorf("backup = %q, want the original", b)
	}
}

// M11: the rewrite keeps the file's mode. 0644 differs from the 0600 a new temp file gets.
func TestSetupKeepsFileMode(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "settings.json")
	os.WriteFile(p, []byte(`{"a":1}`), 0o644)
	os.Chmod(p, 0o644)
	if _, _, code := runSetup(t, dir, "-profile", "standard", "-write"); code != 0 {
		t.Fatal("exit", code)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o644 {
		t.Errorf("mode = %v, want 0644", fi.Mode().Perm())
	}
}

// M22: the write replaces the file instead of truncating it in place, so a crash mid-write
// cannot leave a half file. A hard link to the old file keeps the old contents only then.
func TestSetupWriteIsAtomic(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "settings.json")
	orig := `{"a":1}`
	os.WriteFile(p, []byte(orig), 0o600)
	held := filepath.Join(dir, "held.json")
	if err := os.Link(p, held); err != nil {
		t.Skip(err)
	}
	if _, _, code := runSetup(t, dir, "-profile", "standard", "-write"); code != 0 {
		t.Fatal("exit", code)
	}
	if b, _ := os.ReadFile(held); string(b) != orig {
		t.Errorf("the old file was rewritten in place: %q", b)
	}
	if b, _ := os.ReadFile(p); !strings.Contains(string(b), "pluginConfigs") {
		t.Errorf("new contents missing: %q", b)
	}
}

// M23: an options value that is not an object is the user's data. Refuse, do not overwrite.
func TestSetupRefusesNonObjectOptions(t *testing.T) {
	for _, body := range []string{
		`{"pluginConfigs":{"guardrails@broodhall":{"options":"x"}}}`,
		`{"pluginConfigs":{"guardrails@broodhall":{"options":[1]}}}`,
		`{"pluginConfigs":{"guardrails@broodhall":{"options":null}}}`,
		`{"pluginConfigs":{"guardrails@broodhall":"x"}}`,
		`{"pluginConfigs":[1]}`,
	} {
		dir := t.TempDir()
		p := filepath.Join(dir, "settings.json")
		os.WriteFile(p, []byte(body), 0o600)
		_, _, code := runSetup(t, dir, "-profile", "standard", "-write")
		if b, _ := os.ReadFile(p); code != 1 || string(b) != body {
			t.Errorf("%s: exit %d, file %q; want exit 1 and the file untouched", body, code, b)
		}
	}
}

func TestSetupRejectsBadMarketplace(t *testing.T) {
	for _, m := range []string{"a/b", "..", "a..b", `a"b`, "a'b", "a b", "a@b", "-x", "", "a\\b", "a`b"} {
		if m == "" {
			continue // empty means: detect
		}
		dir := t.TempDir()
		_, _, code := runSetup(t, dir, "-marketplace", m, "-profile", "standard", "-write")
		if code != 2 {
			t.Errorf("-marketplace %q: exit %d, want 2", m, code)
		}
		if _, err := os.Stat(filepath.Join(dir, "settings.json")); err == nil {
			t.Errorf("-marketplace %q: wrote a file", m)
		}
	}
	dir := t.TempDir()
	if _, _, code := runSetup(t, dir, "-marketplace", "team-bh.2", "-profile", "standard", "-write"); code != 0 {
		t.Errorf("a plain dotted name: exit %d", code)
	}
}

func TestSetupMarketplaceIgnoresDisabledEntries(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "settings.json")
	os.WriteFile(p, []byte(`{"enabledPlugins":{"guardrails@aaa":false,"guardrails@bbb":true}}`), 0o600)
	if _, _, code := runSetup(t, dir, "-profile", "standard", "-write"); code != 0 {
		t.Fatal("exit", code)
	}
	guardOptions(t, readJSON(t, p), "guardrails@bbb")
	if pc, _ := readJSON(t, p)["pluginConfigs"].(map[string]any); pc["guardrails@aaa"] != nil {
		t.Errorf("wrote options for a disabled marketplace entry: %v", pc)
	}
}

// --- team-init ---

func TestTeamInitRejectsEmptyDomainList(t *testing.T) {
	for _, d := range []string{",", " , ,", " "} {
		out, errw, code := runTeamInit(t, "-managed", "-domains", d)
		if d == " " {
			continue // a blank flag value reads as "not given" for setup too
		}
		if code != 2 || out != "" || !strings.Contains(errw, "no domain") {
			t.Errorf("-domains %q: exit %d, stdout %q, stderr %q; want exit 2 and 'no domain'", d, code, out, errw)
		}
	}
}

func TestTeamInitNotesRtkNeedsInstall(t *testing.T) {
	for _, args := range [][]string{{"-plugins", "guardrails,rtk"}, {"-managed", "-plugins", "rtk"}} {
		_, errw, code := runTeamInit(t, args...)
		if code != 0 || !strings.Contains(errw, "claude plugin install rtk@broodhall") {
			t.Errorf("%v: exit %d; stderr should say rtk needs `claude plugin install rtk@broodhall`:\n%s", args, code, errw)
		}
	}
	_, errw, _ := runTeamInit(t)
	if strings.Contains(errw, "rtk") {
		t.Errorf("default plugin set has no rtk, but stderr mentions it:\n%s", errw)
	}
}

// --- doctor ---

func TestSkewIgnoresOtherProjectScope(t *testing.T) {
	c := newCfgFixture(t)
	c.write(t, "plugins/installed_plugins.json", `{"version":2,"plugins":{
		"guardrails@broodhall":[{"scope":"user","version":"1.6.0"},{"scope":"project","projectPath":"/elsewhere","version":"1.5.0"},{"scope":"local","projectPath":"/elsewhere","version":"1.4.0"}],
		"memory@broodhall":[{"scope":"user","version":"1.6.0"}]}}`)
	c.marketplace(t, "broodhall", "guardrails", "1.6.0", "memory", "1.6.0")
	if got := checkPluginSkew(c.dir, "/here"); len(got) != 0 {
		t.Errorf("skew from another project's install: %q", got)
	}
	if got := checkMarketplaceVersions(c.dir, "/here"); len(got) != 0 {
		t.Errorf("marketplace diff from another project's install: %q", got)
	}
	if got := checkPluginSkew(c.dir, ""); len(got) != 0 {
		t.Errorf("no project dir: project-scope installs must not count: %q", got)
	}
}

func TestSkewCountsCurrentProjectScope(t *testing.T) {
	c := newCfgFixture(t)
	c.write(t, "plugins/installed_plugins.json", `{"version":2,"plugins":{
		"guardrails@broodhall":[{"scope":"user","version":"1.6.0"},{"scope":"project","projectPath":"/here","version":"1.5.0"}],
		"memory@broodhall":[{"scope":"user","version":"1.6.0"}]}}`)
	if got := checkPluginSkew(c.dir, "/here"); len(got) != 1 {
		t.Errorf("this project's install should count: %q", got)
	}
	if got := checkPluginSkew(c.dir, "/here/"); len(got) != 1 {
		t.Errorf("a trailing slash should not matter: %q", got)
	}
	// Local scope counts the same way.
	c.write(t, "plugins/installed_plugins.json", `{"version":2,"plugins":{
		"guardrails@broodhall":[{"scope":"local","projectPath":"/here","version":"1.5.0"}],
		"memory@broodhall":[{"scope":"user","version":"1.6.0"}]}}`)
	if got := checkPluginSkew(c.dir, "/here"); len(got) != 1 {
		t.Errorf("this project's local install should count: %q", got)
	}
}

func TestSkewNonSemverDoesNotPickAPlugin(t *testing.T) {
	c := newCfgFixture(t)
	c.installed(t, "guardrails@broodhall", "1.10.0", "memory@broodhall", "1.9.0-rc")
	got := strings.Join(checkPluginSkew(c.dir, ""), "\n")
	if got == "" {
		t.Fatal("want a mismatch warning")
	}
	a, b := strings.Contains(got, "update guardrails@broodhall"), strings.Contains(got, "update memory@broodhall")
	if a != b {
		t.Errorf("with versions it cannot order, name both plugins or neither: %s", got)
	}
}

func TestMarketplaceRepoMatchIsExact(t *testing.T) {
	c := newCfgFixture(t)
	c.write(t, "plugins/known_marketplaces.json", `{
	  "fork":{"source":{"source":"git","url":"https://github.com/jj-scoll/broodhall-fork.git"}},
	  "forkrepo":{"source":{"source":"github","repo":"jj-scoll/broodhall-fork"}},
	  "other":{"source":{"source":"github","repo":"x/jj-scoll/broodhall"}},
	  "gitok":{"source":{"source":"git","url":"https://github.com/jj-scoll/broodhall.git"}},
	  "sshok":{"source":{"source":"git","url":"git@github.com:jj-scoll/broodhall.git"}},
	  "ghok":{"source":{"source":"github","repo":"jj-scoll/broodhall"}}}`)
	got := broodhallMarketplaces(c.dir)
	for _, n := range []string{"fork", "forkrepo", "other"} {
		if got[n] {
			t.Errorf("%s counts as broodhall", n)
		}
	}
	for _, n := range []string{"gitok", "sshok", "ghok", "broodhall"} {
		if !got[n] {
			t.Errorf("%s should count as broodhall", n)
		}
	}
}

func TestStatusLineOldBinaryBoundaries(t *testing.T) {
	for cmd, want := range map[string]bool{
		"claude-tools statusline":              true,
		"/h/bin/claude-tools":                  true,
		"/h/bin/claude-tools; echo":            true,
		"/h/bin/claude-tools|cat":              true,
		"/h/bin/claude-tools&":                 true,
		"b=$(ls /h/bin/*/claude-tools) && $b":  true,
		"`ls /h/bin/claude-tools`":             true,
		`C:\bin\claude-tools.exe statusline`:   true,
		"/h/bin/claude-tools.exe":              true,
		"(/h/bin/claude-tools)":                true,
		"ls -d \"$c\"/bin/*/claude-tools 2>/x": true,
		"~/.config/claude-tools/x":             false,
		"claude-tools-foo":                     false,
		"my-claude-tools":                      false,
		"/h/claude-tools.d/guardrails":         false,
		"/h/claude-toolsX":                     false,
		"guardrails statusline":                false,
	} {
		if got := oldBinaryRe.MatchString(cmd); got != want {
			t.Errorf("%q: match %v, want %v", cmd, got, want)
		}
	}
}

// Managed settings count for "plugin enabled": a plugin the organisation enables there must
// not draw a WARN at every session start.
func TestDoctorReadsManagedSettingsForPluginEnabled(t *testing.T) {
	f := newPluginFixture(t)
	f.record(t, "binary bytes")
	old := stampVersion
	stampVersion = "v0.2.0"
	defer func() { stampVersion = old }()
	e := f.env(true)
	f.writeSettings(t, `{}`)
	managed := filepath.Join(t.TempDir(), "managed-settings.json")
	os.WriteFile(managed, []byte(`{"enabledPlugins":{"guardrails@broodhall":true}}`), 0o644)
	e.Settings = append(e.Settings, managed)
	var out bytes.Buffer
	doctor(&out, e)
	if strings.Contains(out.String(), "plugin enabled") {
		t.Errorf("managed enablement still warns:\n%s", out.String())
	}
}

func TestManagedSettingsFilesListsDropIns(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "managed-settings.d"), 0o755)
	os.WriteFile(filepath.Join(root, "managed-settings.json"), []byte(`{}`), 0o644)
	os.WriteFile(filepath.Join(root, "managed-settings.d", "20-b.json"), []byte(`{}`), 0o644)
	os.WriteFile(filepath.Join(root, "managed-settings.d", "10-a.json"), []byte(`{}`), 0o644)
	os.WriteFile(filepath.Join(root, "managed-settings.d", "notes.txt"), []byte(`x`), 0o644)
	got := managedSettingsFilesIn(root)
	want := []string{
		filepath.Join(root, "managed-settings.json"),
		filepath.Join(root, "managed-settings.d", "10-a.json"),
		filepath.Join(root, "managed-settings.d", "20-b.json"),
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("got %q, want %q", got, want)
	}
}
