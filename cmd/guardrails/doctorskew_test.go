package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// cfgFixture is a fake CLAUDE_CONFIG_DIR: installed_plugins.json, known_marketplaces.json and
// a marketplace checkout under plugins/marketplaces/<name>. Nothing here touches a real config.
type cfgFixture struct {
	dir string
}

func newCfgFixture(t *testing.T) *cfgFixture {
	t.Helper()
	return &cfgFixture{dir: t.TempDir()}
}

func (c *cfgFixture) write(t *testing.T, rel, body string) {
	t.Helper()
	p := filepath.Join(c.dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// installed writes installed_plugins.json from id=version pairs, the shape Claude Code
// documents: a plugins map from "<name>@<marketplace>" to a list of install records.
func (c *cfgFixture) installed(t *testing.T, pairs ...string) {
	t.Helper()
	var recs []string
	for i := 0; i+1 < len(pairs); i += 2 {
		recs = append(recs, `"`+pairs[i]+`":[{"scope":"user","installPath":"/x/`+pairs[i]+`","version":"`+pairs[i+1]+`"}]`)
	}
	c.write(t, "plugins/installed_plugins.json", `{"version":2,"plugins":{`+strings.Join(recs, ",")+`}}`)
}

// marketplace writes a checkout of marketplace name whose relative-path plugins carry the given
// plugin.json versions.
func (c *cfgFixture) marketplace(t *testing.T, name string, pairs ...string) {
	t.Helper()
	var entries []string
	for i := 0; i+1 < len(pairs); i += 2 {
		entries = append(entries, `{"name":"`+pairs[i]+`","source":"./plugins/`+pairs[i]+`"}`)
		c.write(t, "plugins/marketplaces/"+name+"/plugins/"+pairs[i]+"/.claude-plugin/plugin.json",
			`{"name":"`+pairs[i]+`","version":"`+pairs[i+1]+`"}`)
	}
	c.write(t, "plugins/marketplaces/"+name+"/.claude-plugin/marketplace.json",
		`{"name":"`+name+`","owner":{"name":"x"},"plugins":[`+strings.Join(entries, ",")+`]}`)
}

func TestReadInstalledPlugins(t *testing.T) {
	c := newCfgFixture(t)
	// Both documented-by-implication shapes: a list of records per id, or a single object.
	c.write(t, "plugins/installed_plugins.json", `{"version":2,"plugins":{
	  "guardrails@broodhall":[{"scope":"user","version":"1.6.0"},{"scope":"project","version":"1.5.3"}],
	  "memory@broodhall":{"scope":"user","version":"1.6.0"},
	  "junk":[{"version":"1"}],
	  "nover@broodhall":[{"scope":"user"}]}}`)
	got, err := readInstalledPlugins(c.dir)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"guardrails broodhall 1.6.0 user": true, "guardrails broodhall 1.5.3 project": true, "memory broodhall 1.6.0 user": true}
	if len(got) != len(want) {
		t.Fatalf("got %d records, want %d: %+v", len(got), len(want), got)
	}
	for _, p := range got {
		k := p.Name + " " + p.Marketplace + " " + p.Version + " " + p.Scope
		if !want[k] {
			t.Errorf("unexpected record %q", k)
		}
	}
}

func TestReadInstalledPluginsMissingOrBroken(t *testing.T) {
	c := newCfgFixture(t)
	if got, err := readInstalledPlugins(c.dir); err != nil || got != nil {
		t.Errorf("missing file: got %v, %v; want nil, nil", got, err)
	}
	c.write(t, "plugins/installed_plugins.json", `{"plugins": `)
	if _, err := readInstalledPlugins(c.dir); err == nil {
		t.Error("broken JSON: want an error")
	}
}

func TestCheckPluginSkew(t *testing.T) {
	tests := []struct {
		name  string
		pairs []string
		want  []string // substrings of the single problem; nil means no problem
	}{
		{"all equal", []string{"guardrails@broodhall", "1.6.0", "memory@broodhall", "1.6.0", "context-guard@broodhall", "1.6.0"}, nil},
		{"memory ahead of guardrails", []string{"guardrails@broodhall", "1.5.3", "memory@broodhall", "1.6.0"},
			[]string{"mismatched", "guardrails 1.5.3", "memory 1.6.0", "claude plugin update guardrails@broodhall"}},
		{"three plugins, one behind", []string{"guardrails@broodhall", "1.6.0", "memory@broodhall", "1.6.0", "context-guard@broodhall", "1.4.0"},
			[]string{"mismatched", "claude plugin update context-guard@broodhall"}},
		{"only one broodhall plugin", []string{"guardrails@broodhall", "1.5.3"}, nil},
		{"same names, another marketplace", []string{"guardrails@other", "1.5.3", "memory@other", "1.6.0"}, nil},
		{"rtk is versioned on its own", []string{"guardrails@broodhall", "1.6.0", "rtk@broodhall", "0.3.0"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newCfgFixture(t)
			c.installed(t, tt.pairs...)
			got := checkPluginSkew(c.dir)
			if tt.want == nil {
				if len(got) != 0 {
					t.Errorf("want no problem, got %q", got)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("want one problem, got %q", got)
			}
			for _, w := range tt.want {
				if !strings.Contains(got[0], w) {
					t.Errorf("want %q in %q", w, got[0])
				}
			}
		})
	}
}

// A marketplace is broodhall's when it is named broodhall or its recorded source is the repo.
func TestCheckPluginSkewRecognisesRenamedMarketplace(t *testing.T) {
	c := newCfgFixture(t)
	c.installed(t, "guardrails@team-bh", "1.5.3", "memory@team-bh", "1.6.0")
	c.write(t, "plugins/known_marketplaces.json", `{"team-bh":{"source":{"source":"github","repo":"jj-scoll/broodhall"},"installLocation":"`+filepath.Join(c.dir, "plugins/marketplaces/team-bh")+`"}}`)
	if got := checkPluginSkew(c.dir); len(got) != 1 || !strings.Contains(got[0], "team-bh") {
		t.Errorf("renamed marketplace: got %q", got)
	}
}

func TestCheckMarketplaceVersions(t *testing.T) {
	c := newCfgFixture(t)
	c.installed(t, "guardrails@broodhall", "1.5.3", "memory@broodhall", "1.6.0")
	c.marketplace(t, "broodhall", "guardrails", "1.6.0", "memory", "1.6.0")
	got := checkMarketplaceVersions(c.dir)
	if len(got) != 1 {
		t.Fatalf("want one problem (guardrails behind), got %q", got)
	}
	for _, w := range []string{"guardrails@broodhall", "1.5.3", "1.6.0", "claude plugin update guardrails@broodhall"} {
		if !strings.Contains(got[0], w) {
			t.Errorf("want %q in %q", w, got[0])
		}
	}
}

func TestCheckMarketplaceVersionsQuietCases(t *testing.T) {
	t.Run("equal", func(t *testing.T) {
		c := newCfgFixture(t)
		c.installed(t, "guardrails@broodhall", "1.6.0")
		c.marketplace(t, "broodhall", "guardrails", "1.6.0")
		if got := checkMarketplaceVersions(c.dir); len(got) != 0 {
			t.Errorf("got %q", got)
		}
	})
	t.Run("no marketplace checkout", func(t *testing.T) {
		c := newCfgFixture(t)
		c.installed(t, "guardrails@broodhall", "1.5.3")
		if got := checkMarketplaceVersions(c.dir); len(got) != 0 {
			t.Errorf("got %q", got)
		}
	})
	t.Run("entry names an external source", func(t *testing.T) {
		c := newCfgFixture(t)
		c.installed(t, "rtk@broodhall", "0.1.0")
		c.write(t, "plugins/marketplaces/broodhall/.claude-plugin/marketplace.json",
			`{"name":"broodhall","owner":{"name":"x"},"plugins":[{"name":"rtk","source":{"source":"git-subdir","url":"u","path":"p"}}]}`)
		if got := checkMarketplaceVersions(c.dir); len(got) != 0 {
			t.Errorf("an external source has no version to read here: got %q", got)
		}
	})
	t.Run("version only on the entry", func(t *testing.T) {
		c := newCfgFixture(t)
		c.installed(t, "memory@broodhall", "1.5.0")
		c.write(t, "plugins/marketplaces/broodhall/.claude-plugin/marketplace.json",
			`{"name":"broodhall","owner":{"name":"x"},"plugins":[{"name":"memory","source":"./plugins/memory","version":"1.6.0"}]}`)
		if got := checkMarketplaceVersions(c.dir); len(got) != 1 {
			t.Errorf("entry version should count: got %q", got)
		}
	})
	t.Run("plugin.json beats the entry", func(t *testing.T) {
		c := newCfgFixture(t)
		c.installed(t, "memory@broodhall", "1.6.0")
		c.write(t, "plugins/marketplaces/broodhall/plugins/memory/.claude-plugin/plugin.json", `{"name":"memory","version":"1.6.0"}`)
		c.write(t, "plugins/marketplaces/broodhall/.claude-plugin/marketplace.json",
			`{"name":"broodhall","owner":{"name":"x"},"plugins":[{"name":"memory","source":"./plugins/memory","version":"9.9.9"}]}`)
		if got := checkMarketplaceVersions(c.dir); len(got) != 0 {
			t.Errorf("manifest version wins over the entry: got %q", got)
		}
	})
}

func TestCheckStatusLineOldBinary(t *testing.T) {
	f := newPluginFixture(t)
	tests := []struct {
		name, settings string
		want           bool
	}{
		{"old docs command", `{"statusLine":{"type":"command","command":"sh -c 'b=$(ls -d \"$HOME\"/.claude/plugins/data/guardrails-broodhall/bin/*/claude-tools | tail -1); exec \"$b\" statusline'"}}`, true},
		{"direct path", `{"statusLine":{"type":"command","command":"/home/me/.claude/bin/claude-tools statusline"}}`, true},
		{"quoted path", `{"statusLine":{"type":"command","command":"\"$HOME/bin/claude-tools\" statusline"}}`, true},
		{"new binary", `{"statusLine":{"type":"command","command":"sh -c 'exec \"$b/guardrails\" statusline'"}}`, false},
		{"config dir named claude-tools is not the binary", `{"statusLine":{"type":"command","command":"cat ~/.config/claude-tools/line.txt"}}`, false},
		{"no statusLine", `{}`, false},
		{"statusLine is not an object", `{"statusLine":"x"}`, false},
		{"hook, not statusLine", `{"hooks":{"PreToolUse":[{"hooks":[{"command":"/b/claude-tools pre-tool-use"}]}]}}`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f.writeSettings(t, tt.settings)
			got := checkStatusLine([]string{f.settings})
			if (got != "") != tt.want {
				t.Fatalf("checkStatusLine = %q, want problem=%v", got, tt.want)
			}
			if tt.want {
				for _, w := range []string{"claude-tools", "guardrails", "bin/*/guardrails"} {
					if !strings.Contains(got, w) {
						t.Errorf("want %q in %q", w, got)
					}
				}
			}
		})
	}
}

func TestDoctorReportsSkewAndStatusLine(t *testing.T) {
	f := newPluginFixture(t)
	f.record(t, "binary bytes")
	f.writeSettings(t, `{"enabledPlugins":{"guardrails@broodhall":true},"statusLine":{"type":"command","command":"/b/claude-tools statusline"}}`)
	old := stampVersion
	stampVersion = "v0.2.0"
	defer func() { stampVersion = old }()
	c := newCfgFixture(t)
	c.installed(t, "guardrails@broodhall", "1.5.3", "memory@broodhall", "1.6.0")

	e := f.env(false)
	e.ConfigDir = c.dir
	var out bytes.Buffer
	doctor(&out, e)
	for _, w := range []string{"WARN plugin versions", "mismatched", "WARN statusLine", "claude-tools"} {
		if !strings.Contains(out.String(), w) {
			t.Errorf("want %q in:\n%s", w, out.String())
		}
	}

	e.Hook = true
	out.Reset()
	if code := doctor(&out, e); code != 0 {
		t.Errorf("hook mode exit %d, want 0", code)
	}
	user, _ := decodeDoctorHook(t, out.Bytes())
	if !strings.Contains(user, "mismatched") {
		t.Errorf("hook reply lacks the skew warning: %q", user)
	}
}

// With no ConfigDir doctor reads no installed_plugins.json: the tests above never see a real config.
func TestDoctorSkipsSkewWithoutConfigDir(t *testing.T) {
	f := newPluginFixture(t)
	f.record(t, "binary bytes")
	old := stampVersion
	stampVersion = "v0.2.0"
	defer func() { stampVersion = old }()
	var out bytes.Buffer
	doctor(&out, f.env(false))
	if strings.Contains(out.String(), "plugin versions") {
		t.Errorf("skew checked with no config dir:\n%s", out.String())
	}
}

func TestDoctorHealthyVersionsLine(t *testing.T) {
	f := newPluginFixture(t)
	f.record(t, "binary bytes")
	old := stampVersion
	stampVersion = "v0.2.0"
	defer func() { stampVersion = old }()
	c := newCfgFixture(t)
	c.installed(t, "guardrails@broodhall", "1.6.0", "memory@broodhall", "1.6.0")
	c.marketplace(t, "broodhall", "guardrails", "1.6.0", "memory", "1.6.0")
	e := f.env(false)
	e.ConfigDir = c.dir
	var out bytes.Buffer
	if code := doctor(&out, e); code != 0 {
		t.Fatalf("exit %d\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "ok   plugin versions") {
		t.Errorf("want an ok line:\n%s", out.String())
	}
	e.Hook = true
	out.Reset()
	doctor(&out, e)
	if out.Len() != 0 {
		t.Errorf("hook mode, healthy: want silence, got %s", out.String())
	}
}
