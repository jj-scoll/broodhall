package main

import (
	"strings"
	"testing"
)

// Third fix pass on PR #49 (verifier round 5): an unset disabled_guards is the plugin.json
// default, which is empty, and the option may be an array like allowed_email_domains.
func TestConfigGateUnsetDisabledGuardsIsEmpty(t *testing.T) {
	opt := func(v string) string {
		return `{"pluginConfigs":{"guardrails@broodhall":{"options":{"disabled_guards":` + v + `}}}}`
	}
	weak, _, err := settingsDiff([]byte(`{}`), []byte(opt(`"pii"`)))
	if err != nil || !strings.Contains(strings.Join(weak, "\n"), "guard family turned off: pii") {
		t.Errorf("unset -> pii: weak=%q err=%v, want a weakening", weak, err)
	}
	weak, _, _ = settingsDiff([]byte(opt(`""`)), []byte(`{}`))
	if len(weak) != 0 {
		t.Errorf("empty -> unset weakened: %q", weak)
	}
	weak, _, _ = settingsDiff([]byte(`{}`), []byte(opt(`["pii","git"]`)))
	if !strings.Contains(strings.Join(weak, "\n"), "pii") {
		t.Errorf("array disabled_guards not judged: %q", weak)
	}
	weak, _, _ = settingsDiff([]byte(opt(`["pii"]`)), []byte(opt(`"pii"`)))
	if len(weak) != 0 {
		t.Errorf("array pii -> string pii weakened: %q", weak)
	}
}

// Text fed into a shell on stdin runs as a script: heredoc, echo and printf pipes ask.
// Other pipes and mutation-killing forms from verifier round 5 are covered too.
func TestSetupWritePipedIntoShell(t *testing.T) {
	asks := []string{
		"cat <<'EOF' | bash\nguardrails setup -write\nEOF",
		`echo 'guardrails setup -write' | sh`,
		`printf '%s\n' "guardrails setup -profile observe -write" | bash -s`,
		`bash -o pipefail run.sh setup -write`,
		`ksh run.sh setup -write`,
	}
	for _, cmd := range asks {
		if d := guardCmd(cmd); !d.Ask && d.Exit != 2 {
			t.Errorf("%q: %s; want an ask", cmd, d.Kind())
		}
	}
	quiet := []string{
		`echo 'guardrails setup -write' | cat`,
		`echo 'guardrails setup -write' | rg setup`,
		"cat <<'EOF' | wc -l\nguardrails setup -write\nEOF",
		`echo 'guardrails setup' | bash`,
	}
	for _, cmd := range quiet {
		if d := guardCmd(cmd); d.Ask || d.Exit == 2 {
			t.Errorf("%q: %s; want no ask", cmd, d.Kind())
		}
	}
}

func TestConfigGateIgnoresOtherPluginIDs(t *testing.T) {
	old := `{}`
	next := `{"pluginConfigs":{"guardrails-dev@x":{"options":{"observe":true,"disabled_guards":"pii"}}}}`
	weak, _, _ := settingsDiff([]byte(old), []byte(next))
	if len(weak) != 0 {
		t.Errorf("guardrails-dev@x judged: %q", weak)
	}
}

func TestConfigGateDomainArrayWithCommas(t *testing.T) {
	opt := func(v string) string {
		return `{"pluginConfigs":{"guardrails@broodhall":{"options":{"allowed_email_domains":` + v + `}}}}`
	}
	weak, _, _ := settingsDiff([]byte(opt(`["@a.com"]`)), []byte(opt(`["@a.com,@b.com"]`)))
	if !strings.Contains(strings.Join(weak, "\n"), "@b.com") {
		t.Errorf("comma list inside an array element not judged: %q", weak)
	}
}

func TestConfigGateDisableWithOptionChanges(t *testing.T) {
	old := `{"enabledPlugins":{"guardrails@broodhall":true},"pluginConfigs":{"guardrails@broodhall":{"options":{"observe":false}}}}`
	next := `{"enabledPlugins":{"guardrails@broodhall":false},"pluginConfigs":{"guardrails@broodhall":{"options":{"observe":true,"disabled_guards":"pii"}}}}`
	weak, _, _ := settingsDiff([]byte(old), []byte(next))
	all := strings.Join(weak, "\n")
	if !strings.Contains(all, "plugin disabled") {
		t.Errorf("disable not reported: %q", weak)
	}
}
