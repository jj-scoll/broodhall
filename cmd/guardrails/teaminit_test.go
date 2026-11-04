package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runTeamInit(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	var out, errw bytes.Buffer
	code = teamInit(&out, &errw, args)
	return out.String(), errw.String(), code
}

// The default output is the block to paste into .claude/settings.json: stdout is only JSON, so
// `guardrails team-init > file` works. The exact text is the contract, so this is a golden.
const wantTeamInitProject = `{
  "enabledPlugins": {
    "context-guard@broodhall": true,
    "guardrails@broodhall": true,
    "memory@broodhall": true
  },
  "extraKnownMarketplaces": {
    "broodhall": {
      "autoUpdate": true,
      "source": {
        "repo": "jj-scoll/broodhall",
        "source": "github"
      }
    }
  }
}
`

func TestTeamInitProjectGolden(t *testing.T) {
	out, errw, code := runTeamInit(t)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errw)
	}
	if out != wantTeamInitProject {
		t.Errorf("output differs from the golden:\n--- got\n%s--- want\n%s", out, wantTeamInitProject)
	}
	if errw == "" {
		t.Error("want a stderr note saying where to paste the block")
	}
}

func TestTeamInitJSONHasDocumentedKeys(t *testing.T) {
	for _, args := range [][]string{{}, {"-managed"}, {"-managed", "-domains", "@acme.com"}, {"-plugins", "guardrails"}} {
		out, _, code := runTeamInit(t, args...)
		if code != 0 {
			t.Fatalf("%v: exit %d", args, code)
		}
		var doc struct {
			Known map[string]struct {
				Source struct {
					Source string `json:"source"`
					Repo   string `json:"repo"`
				} `json:"source"`
				AutoUpdate bool `json:"autoUpdate"`
			} `json:"extraKnownMarketplaces"`
			Enabled map[string]bool `json:"enabledPlugins"`
		}
		if err := json.Unmarshal([]byte(out), &doc); err != nil {
			t.Fatalf("%v: not valid JSON: %v\n%s", args, err, out)
		}
		m, ok := doc.Known["broodhall"]
		if !ok || m.Source.Source != "github" || m.Source.Repo != "jj-scoll/broodhall" {
			t.Errorf("%v: extraKnownMarketplaces.broodhall = %+v", args, m)
		}
		if len(doc.Enabled) == 0 {
			t.Errorf("%v: no enabledPlugins", args)
		}
		for id, on := range doc.Enabled {
			if !strings.HasSuffix(id, "@broodhall") || !on {
				t.Errorf("%v: enabledPlugins entry %q=%v", args, id, on)
			}
		}
	}
}

// The ids in the snippet must be plugins the marketplace really lists, under its real name.
func TestTeamInitMatchesMarketplaceFile(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(pluginDir, "../..", ".claude-plugin/marketplace.json"))
	if err != nil {
		t.Fatal(err)
	}
	var mkt struct {
		Name    string `json:"name"`
		Plugins []struct {
			Name string `json:"name"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal(b, &mkt); err != nil {
		t.Fatal(err)
	}
	listed := map[string]bool{}
	for _, p := range mkt.Plugins {
		listed[p.Name] = true
	}
	out, _, _ := runTeamInit(t, "-plugins", "guardrails,memory,context-guard,rtk")
	var doc struct {
		Enabled map[string]bool `json:"enabledPlugins"`
		Known   map[string]any  `json:"extraKnownMarketplaces"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatal(err)
	}
	if _, ok := doc.Known[mkt.Name]; !ok {
		t.Errorf("marketplace key %v does not include %q", doc.Known, mkt.Name)
	}
	for id := range doc.Enabled {
		name, m, _ := strings.Cut(id, "@")
		if m != mkt.Name || !listed[name] {
			t.Errorf("%q is not a plugin of marketplace %q", id, mkt.Name)
		}
	}
	if len(doc.Enabled) != 4 {
		t.Errorf("want 4 plugins, got %v", doc.Enabled)
	}
}

func TestTeamInitManagedWithDomains(t *testing.T) {
	out, errw, code := runTeamInit(t, "-managed", "-domains", "acme.com, @sub.acme.com ,dev@other.org")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errw)
	}
	var doc struct {
		Configs map[string]struct {
			Options map[string]any `json:"options"`
		} `json:"pluginConfigs"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatal(err)
	}
	got := doc.Configs["guardrails@broodhall"].Options["allowed_email_domains"]
	if got != "@acme.com,@sub.acme.com,dev@other.org" {
		t.Errorf("allowed_email_domains = %v", got)
	}
	for _, p := range []string{"/Library/Application Support/ClaudeCode/managed-settings.json", "/etc/claude-code/managed-settings.json", `C:\Program Files\ClaudeCode\managed-settings.json`} {
		if !strings.Contains(errw, p) {
			t.Errorf("stderr lacks managed path %q:\n%s", p, errw)
		}
	}
}

// pluginConfigs is read from user and managed settings only; a project file would silently ignore it.
func TestTeamInitDomainsNeedManaged(t *testing.T) {
	out, errw, code := runTeamInit(t, "-domains", "acme.com")
	if code != 2 || out != "" || !strings.Contains(errw, "-managed") {
		t.Errorf("code=%d out=%q err=%q; want exit 2, no stdout, a hint about -managed", code, out, errw)
	}
}

func TestTeamInitRejectsBadInput(t *testing.T) {
	for _, args := range [][]string{{"-plugins", "nosuch"}, {"-plugins", ""}, {"-bogus"}, {"-managed", "-domains", "not a domain"}} {
		out, _, code := runTeamInit(t, args...)
		if code != 2 || out != "" {
			t.Errorf("%v: code=%d out=%q; want exit 2 and no stdout", args, code, out)
		}
	}
}

func TestTeamInitRegistered(t *testing.T) {
	if commands["team-init"] == nil {
		t.Error("team-init is not in the command table")
	}
}
