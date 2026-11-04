package main

// Second fix pass on PR #49 (verifier round 4): the setup -write ask reads the command with the
// shell parser, config-gate checks plugin options in files that do not enable the plugin, and a
// few matchers get the cases the mutation run found untested.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Forms a model plausibly writes. Each runs `setup -write` and must ask.
var setupWriteBypassForms = []string{
	"guardrails setup -profile observe \\\n  -domains evil.example.org \\\n  -write", // line continuation
	`bash -lc 'guardrails setup -profile observe -write'`,
	`bash -ec 'guardrails setup -profile observe -write'`,
	`timeout 60 guardrails setup -profile observe -write`,
	`CLAUDE_CONFIG_DIR=~/.claude-work guardrails setup -profile observe -write`,
	`CLAUDE_CONFIG_DIR=/home/u/.claude guardrails setup -profile observe -write`,
	`PATH=/opt/bin:/usr/bin guardrails setup -write`,
	`if command -v guardrails >/dev/null; then guardrails setup -profile observe -write; fi`,
	`for d in a b; do guardrails setup -write; done`,
	`sh "/c/Users/John Smith/.claude/plugins/cache/broodhall/guardrails/1.7.0/scripts/run.sh" setup -profile observe -write`,
	`out=$(guardrails setup -profile observe -write) && echo "$out"`,
	"out=`guardrails setup -profile observe -write`",
	`eval "guardrails setup -profile observe -write"`,
	`{ guardrails setup -profile observe -write; }`,
	`! guardrails setup -profile observe -write`,
	`(guardrails setup -write)`,
	`sleep 1 & guardrails setup -profile observe -write`,
	`guardrails setup -profile observe -write &`,
	`guardrails setup -profile observe -write >/dev/null 2>&1`,
	`guardrails setup -profile observe -write>/dev/null`,
	`nice -n 5 guardrails setup -write`,
	`sudo -u jeff guardrails setup -write`,
	`env -i guardrails setup -write`,
	`stdbuf -oL guardrails setup -write`,
	`echo y | xargs guardrails setup -write`,
	`xargs -n1 guardrails setup -write < /dev/null`,
	`script -qc 'guardrails setup -write' /dev/null`,
	`flock /tmp/l guardrails setup -write`,
	`sh -x "$CLAUDE_PLUGIN_ROOT/scripts/run.sh" setup -write`,
	"bash <<'EOF'\nguardrails setup -profile observe -write\nEOF",
	`ssh localhost 'guardrails setup -write'`,
	`/opt/x/GUARDRAILS.EXE setup -write`,
	`C:\bin\guardrails.EXE setup -write`,
	`"C:\Program Files\x\guardrails.exe" setup -write`,
	`guardrails setup -write=TRUE`,
	`guardrails setup -write=t`,
	`guardrails setup -write=1`,
}

func TestSetupWriteAsksOnShellForms(t *testing.T) {
	for _, cmd := range setupWriteBypassForms {
		if d := guardCmd(cmd); !d.Ask && d.Exit != 2 {
			t.Errorf("%q: %s; want an ask", cmd, d.Kind())
		}
	}
}

// Quoted text, heredoc bodies and data arguments are not commands.
func TestSetupWriteNoAskOnQuotedText(t *testing.T) {
	for _, cmd := range []string{
		`git commit -m 'fix; guardrails setup -write now asks'`,
		"git commit -m \"$(cat <<'EOF'\nfix(guardrails): x\n\nguardrails setup -write now asks\nEOF\n)\"",
		"cat > notes.md <<'EOF'\nrun guardrails setup -write\nEOF",
		`echo 'guardrails setup -write'; ls`,
		`gh pr create --title "guardrails setup -write" --body "x"`,
		`rg 'guardrails setup -write' docs`,
		`printf '%s\n' "sh run.sh setup -write"`,
		`sh -c 'echo guardrails setup -write'`,
		`guardrails setup -write=false`,
		`guardrails setup -write=f`,
		`guardrails setup -write=FALSE`,
		`guardrails setup -write=0`,
		`guardrails setup -profile standard`,
		`guardrails doctor -write`,
		`guardrails setup-other -write`,
		`go test ./cmd/guardrails -run TestSetup -write`,
	} {
		if d := guardCmd(cmd); d.Ask || d.Exit != 0 {
			t.Errorf("%q: %s; want no ask", cmd, d.Kind())
		}
	}
}

// --- config-gate: options are judged whichever file enables the plugin ---

func TestConfigGateOptionsWithoutEnabledPlugins(t *testing.T) {
	for name, tc := range map[string][2]string{
		"observe on":       {`{"pluginConfigs":{"guardrails@broodhall":{"options":{"observe":false}}}}`, `{"pluginConfigs":{"guardrails@broodhall":{"options":{"observe":true}}}}`},
		"observe number 1": {`{"pluginConfigs":{"guardrails@broodhall":{"options":{"observe":false}}}}`, `{"pluginConfigs":{"guardrails@broodhall":{"options":{"observe":1}}}}`},
		"domain added":     {`{}`, `{"pluginConfigs":{"guardrails@broodhall":{"options":{"allowed_email_domains":"@evil.org"}}}}`},
		"domain in array":  {`{}`, `{"pluginConfigs":{"guardrails@broodhall":{"options":{"allowed_email_domains":["@evil.org"]}}}}`},
		"family disabled":  {`{"pluginConfigs":{"guardrails@broodhall":{"options":{"disabled_guards":""}}}}`, `{"pluginConfigs":{"guardrails@broodhall":{"options":{"disabled_guards":"git,egress"}}}}`},
		"only enabled in new file": {`{}`,
			`{"enabledPlugins":{"guardrails@broodhall":true},"pluginConfigs":{"guardrails@broodhall":{"options":{"observe":true}}}}`},
	} {
		weak, _, err := settingsDiff([]byte(tc[0]), []byte(tc[1]))
		if err != nil || len(weak) == 0 {
			t.Errorf("%s: weakened=%v err=%v; want a weakening", name, weak, err)
		}
	}
}

func TestConfigGateOptionsStaySilentWhenSafe(t *testing.T) {
	for name, tc := range map[string][2]string{
		"nothing":            {`{}`, `{}`},
		"observe off":        {`{"pluginConfigs":{"guardrails@broodhall":{"options":{"observe":true}}}}`, `{"pluginConfigs":{"guardrails@broodhall":{"options":{"observe":false}}}}`},
		"observe 0":          {`{}`, `{"pluginConfigs":{"guardrails@broodhall":{"options":{"observe":0}}}}`},
		"domain removed":     {`{"pluginConfigs":{"guardrails@broodhall":{"options":{"allowed_email_domains":"@a.com,@b.com"}}}}`, `{"pluginConfigs":{"guardrails@broodhall":{"options":{"allowed_email_domains":"@a.com"}}}}`},
		"domain array same":  {`{"pluginConfigs":{"guardrails@broodhall":{"options":{"allowed_email_domains":"@a.com"}}}}`, `{"pluginConfigs":{"guardrails@broodhall":{"options":{"allowed_email_domains":["@A.com"]}}}}`},
		"other plugin":       {`{}`, `{"pluginConfigs":{"memory@broodhall":{"options":{"observe":true}}}}`},
		"standard profile":   {`{}`, `{"pluginConfigs":{"guardrails@broodhall":{"options":{"disabled_guards":"","observe":false}}}}`},
		"plugin not enabled": {`{"enabledPlugins":{"guardrails@broodhall":false}}`, `{"enabledPlugins":{"guardrails@broodhall":false}}`},
	} {
		weak, _, err := settingsDiff([]byte(tc[0]), []byte(tc[1]))
		if err != nil || len(weak) != 0 {
			t.Errorf("%s: weakened=%v err=%v; want none", name, weak, err)
		}
	}
}

// The "plugin disabled" check still belongs to the file that enabled the plugin.
func TestConfigGatePluginDisabledOnlyForEnablingFile(t *testing.T) {
	weak, _, _ := settingsDiff([]byte(`{"pluginConfigs":{"guardrails@broodhall":{"options":{}}}}`), []byte(`{}`))
	for _, w := range weak {
		if strings.Contains(w, "plugin disabled") {
			t.Errorf("a file that never enabled the plugin cannot disable it: %q", weak)
		}
	}
	weak, _, _ = settingsDiff([]byte(`{"enabledPlugins":{"guardrails@broodhall":true}}`), []byte(`{}`))
	if len(weak) != 1 || !strings.Contains(weak[0], "plugin disabled") {
		t.Errorf("weak = %q", weak)
	}
}

// --- cheap mutation killers ---

func TestBroodhallURLCases(t *testing.T) {
	for u, want := range map[string]bool{
		"https://github.com/jj-scoll/broodhall":          true,
		"https://github.com/jj-scoll/broodhall.git":      true,
		"https://GitHub.com/JJ-Scoll/Broodhall.git":      true,
		"git@github.com:jj-scoll/broodhall.git":          true,
		"ssh://git@github.com/jj-scoll/broodhall.git":    true,
		"https://github.com/jj-scoll/broodhall-fork":     false,
		"https://github.com/jj-scoll/broodhall.git.evil": false,
		"https://github.com/evil/jj-scoll/broodhall":     false,
		"https://evil.com/github.com/jj-scoll/broodhall": false,
	} {
		if got := broodhallURLRe.MatchString(u); got != want {
			t.Errorf("%q: %v, want %v", u, got, want)
		}
	}
	c := newCfgFixture(t)
	c.write(t, "plugins/known_marketplaces.json", `{"up":{"source":{"source":"github","repo":"JJ-Scoll/Broodhall"}},"ssh":{"source":{"source":"git","url":"ssh://git@github.com/jj-scoll/broodhall.git"}}}`)
	got := broodhallMarketplaces(c.dir)
	if !got["up"] || !got["ssh"] {
		t.Errorf("got %v", got)
	}
}

func TestDefaultSettingsFilesReadManagedSettings(t *testing.T) {
	root := t.TempDir()
	old := managedRoot
	managedRoot = func() string { return root }
	defer func() { managedRoot = old }()
	os.MkdirAll(filepath.Join(root, "managed-settings.d"), 0o755)
	os.WriteFile(filepath.Join(root, "managed-settings.d", "20-x.json"), []byte(`{}`), 0o644)
	got := strings.Join(defaultSettingsFiles(), "\n")
	for _, w := range []string{filepath.Join(root, "managed-settings.json"), filepath.Join(root, "managed-settings.d", "20-x.json")} {
		if !strings.Contains(got, w) {
			t.Errorf("default settings files lack %s:\n%s", w, got)
		}
	}
}

func TestProjectDirHonoursEnv(t *testing.T) {
	d := t.TempDir()
	t.Setenv("CLAUDE_PROJECT_DIR", d)
	if got := projectDir(); got != d {
		t.Errorf("projectDir = %q, want %q", got, d)
	}
	if !strings.Contains(strings.Join(defaultSettingsFiles(), "\n"), filepath.Join(d, ".claude", "settings.json")) {
		t.Error("default settings files ignore CLAUDE_PROJECT_DIR")
	}
	t.Setenv("CLAUDE_PROJECT_DIR", "")
	wd, _ := os.Getwd()
	if got := projectDir(); got != wd {
		t.Errorf("projectDir = %q, want the working directory %q", got, wd)
	}
}

func TestAppliesToResolvesSymlinks(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "proj")
	if err := os.Symlink(real, link); err != nil {
		t.Skip(err)
	}
	p := installedPlugin{Scope: "project", ProjectPath: real}
	if !p.appliesTo(link) {
		t.Error("a project dir reached through a symlink should match the recorded real path")
	}
	if (installedPlugin{Scope: "project", ProjectPath: link}).appliesTo(real) != true {
		t.Error("a recorded symlink path should match the real project dir")
	}
	if p.appliesTo(t.TempDir()) {
		t.Error("another directory matched")
	}
}
