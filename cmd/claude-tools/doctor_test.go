package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
)

func TestBuildRev(t *testing.T) {
	const full = "53cae46a1b2c3d4e5f60718293a4b5c6d7e8f901"
	const other = "fe95340a1b2c3d4e5f60718293a4b5c6d7e8f901"
	vcs := func(kv ...string) *debug.BuildInfo {
		bi := &debug.BuildInfo{}
		for i := 0; i < len(kv); i += 2 {
			bi.Settings = append(bi.Settings, debug.BuildSetting{Key: kv[i], Value: kv[i+1]})
		}
		return bi
	}
	tests := []struct {
		name      string
		bi        *debug.BuildInfo
		ok        bool
		stamp     string
		wantRev   string
		wantDirty bool
	}{
		{"clean build", vcs("vcs.revision", full, "vcs.modified", "false"), true, "", full, false},
		{"dirty build", vcs("vcs.revision", full, "vcs.modified", "true"), true, "", full, true},
		{"built outside git", vcs("-trimpath", "true"), true, "", "", false},
		{"no build info", nil, false, "", "", false},
		{"built without git, stamped", vcs("-trimpath", "true"), true, other, other, false},
		{"no build info, stamped", nil, false, other, other, false},
		{"vcs info beats the stamp", vcs("vcs.revision", full, "vcs.modified", "true"), true, other, full, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			old := stampRev
			stampRev = tt.stamp
			defer func() { stampRev = old }()
			rev, dirty := buildRev(tt.bi, tt.ok)
			if rev != tt.wantRev || dirty != tt.wantDirty {
				t.Errorf("buildRev = (%q, %v), want (%q, %v)", rev, dirty, tt.wantRev, tt.wantDirty)
			}
		})
	}
}

func TestBuildVersion(t *testing.T) {
	mod := func(v string) *debug.BuildInfo { return &debug.BuildInfo{Main: debug.Module{Version: v}} }
	tests := []struct {
		name  string
		bi    *debug.BuildInfo
		ok    bool
		stamp string
		want  string
	}{
		{"stamped tag", mod("(devel)"), true, "v0.2.0", "0.2.0"},
		{"stamp without v", mod("(devel)"), true, "0.2.0", "0.2.0"},
		{"go install at a tag", mod("v0.3.1"), true, "", "0.3.1"},
		{"stamp beats module version", mod("v0.3.1"), true, "v0.2.0", "0.2.0"},
		{"local build", mod("(devel)"), true, "", ""},
		{"pseudo-version is not a release", mod("v0.0.0-20260101000000-abcdef123456"), true, "", ""},
		{"pseudo-version after a tag", mod("v0.2.1-0.20261103232052-133e3d30496c+dirty"), true, "", ""},
		{"no build info", nil, false, "", ""},
		{"no build info, stamped", nil, false, "v1.0.0", "1.0.0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			old := stampVersion
			stampVersion = tt.stamp
			defer func() { stampVersion = old }()
			if got := buildVersion(tt.bi, tt.ok); got != tt.want {
				t.Errorf("buildVersion = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestVersionString(t *testing.T) {
	tests := []struct {
		ver, rev string
		dirty    bool
		want     string
	}{
		{"0.2.0", "5b8331f4f8c401cc", false, "0.2.0 (5b8331f)"},
		{"0.2.0", "5b8331f4f8c401cc", true, "0.2.0 (5b8331f-dirty)"},
		{"0.2.0", "", false, "0.2.0"},
		{"", "5b8331f4f8c401cc", false, "5b8331f"},
		{"", "", false, "unknown"},
	}
	for _, tt := range tests {
		if got := versionString(tt.ver, tt.rev, tt.dirty); got != tt.want {
			t.Errorf("versionString(%q,%q,%v) = %q, want %q", tt.ver, tt.rev, tt.dirty, got, tt.want)
		}
	}
}

func TestCheckVersion(t *testing.T) {
	tests := []struct {
		name           string
		binVer, binRev string
		pluginVer      string
		wantProblem    string // substring; "" means no problem
	}{
		{"stamped and matching", "0.2.0", "abc", "0.2.0", ""},
		{"stamped, no plugin.json to compare", "0.2.0", "", "", ""},
		{"commit only", "", "abc1234", "0.2.0", ""},
		{"no stamp at all", "", "", "0.2.0", "no version stamp"},
		{"binary older than plugin", "0.1.2", "abc", "0.2.0", "binary is 0.1.2 but the plugin is 0.2.0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := checkVersion(tt.binVer, tt.binRev, tt.pluginVer)
			if tt.wantProblem == "" && got != "" || !strings.Contains(got, tt.wantProblem) {
				t.Errorf("checkVersion = %q, want %q", got, tt.wantProblem)
			}
		})
	}
}

// pluginFixture lays out an installed plugin: its root (hooks, launcher, plugin.json), a PATH
// of fake programs, and an installed binary with a recorded checksum.
type pluginFixture struct {
	dir, root, bin, settings string
}

const fixtureHooks = `{"hooks":{
 "SessionStart":[{"hooks":[{"type":"command","command":"sh \"${CLAUDE_PLUGIN_ROOT}/scripts/run.sh\" install"}]}],
 "PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"sh \"${CLAUDE_PLUGIN_ROOT}/scripts/run.sh\" pre-tool-use","onFailure":"block"}]}],
 "PostToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"sh \"${CLAUDE_PLUGIN_ROOT}/scripts/run.sh\" retry-breaker"}]}]}}`

func newPluginFixture(t *testing.T) *pluginFixture {
	t.Helper()
	dir := t.TempDir()
	f := &pluginFixture{dir: dir, root: filepath.Join(dir, "root"), settings: filepath.Join(dir, "settings.json")}
	write := func(p, body string, mode os.FileMode) string {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
		return p
	}
	write(filepath.Join(f.root, ".claude-plugin/plugin.json"), `{"name":"guardrails","version":"0.2.0"}`, 0o644)
	write(filepath.Join(f.root, "hooks/hooks.json"), fixtureHooks, 0o644)
	write(filepath.Join(f.root, "scripts/run.sh"), "#!/bin/sh\n", 0o755)
	bins := filepath.Join(dir, "path")
	write(filepath.Join(bins, "sh"), "#!/bin/true\n", 0o755)
	f.bin = write(filepath.Join(dir, "data/bin/0.2.0/claude-tools"), "binary bytes", 0o755)
	t.Setenv("PATH", bins)
	t.Setenv("HOME", dir)
	f.writeSettings(t, `{"enabledPlugins":{"guardrails@broodhall":true}}`)
	return f
}

func (f *pluginFixture) writeSettings(t *testing.T, body string) {
	t.Helper()
	if err := os.WriteFile(f.settings, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (f *pluginFixture) record(t *testing.T, content string) {
	t.Helper()
	sum := sha256.Sum256([]byte(content))
	line := hex.EncodeToString(sum[:]) + "  claude-tools\n"
	if err := os.WriteFile(f.bin+".sha256", []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (f *pluginFixture) env(hook bool) doctorEnv {
	return doctorEnv{PluginRoot: f.root, Exe: f.bin, Settings: []string{f.settings}, Hook: hook}
}

func TestCheckBinaryChecksum(t *testing.T) {
	f := newPluginFixture(t)
	if got := checkBinaryChecksum(f.bin); got != "" {
		t.Errorf("no recorded checksum: want skip, got %q", got)
	}
	f.record(t, "binary bytes")
	if got := checkBinaryChecksum(f.bin); got != "" {
		t.Errorf("matching checksum: got %q", got)
	}
	f.record(t, "what the release shipped")
	if got := checkBinaryChecksum(f.bin); !strings.Contains(got, "does not match") {
		t.Errorf("changed binary: got %q, want a mismatch", got)
	}
	if err := os.WriteFile(f.bin+".sha256", []byte("garbage\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := checkBinaryChecksum(f.bin); !strings.Contains(got, "unreadable") {
		t.Errorf("garbled record: got %q", got)
	}
	if got := checkBinaryChecksum(filepath.Join(f.dir, "gone")); !strings.Contains(got, "not found") {
		t.Errorf("missing binary: got %q", got)
	}
}

func TestPluginEnabled(t *testing.T) {
	f := newPluginFixture(t)
	tests := []struct {
		name, settings, want string
	}{
		{"enabled", `{"enabledPlugins":{"guardrails@broodhall":true}}`, "enabled"},
		{"enabled from another marketplace name", `{"enabledPlugins":{"guardrails@mine":true}}`, "enabled"},
		{"disabled", `{"enabledPlugins":{"guardrails@broodhall":false}}`, "disabled"},
		{"other plugin only", `{"enabledPlugins":{"memory@broodhall":true}}`, "absent"},
		{"no key", `{}`, "absent"},
		{"unknown keys tolerated", `{"x":[1],"enabledPlugins":{"guardrails@broodhall":true,"y":"z"}}`, "enabled"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f.writeSettings(t, tt.settings)
			if got := pluginEnabled([]string{f.settings}, "guardrails"); got != tt.want {
				t.Errorf("pluginEnabled = %q, want %q", got, tt.want)
			}
		})
	}
	if got := pluginEnabled([]string{filepath.Join(f.dir, "missing.json")}, "guardrails"); got != "absent" {
		t.Errorf("missing file: got %q", got)
	}
}

func TestCheckPluginHooks(t *testing.T) {
	f := newPluginFixture(t)
	if got := checkPluginHooks(f.root); got != "" {
		t.Errorf("healthy plugin: got %q", got)
	}
	// a hook naming a subcommand this binary does not have
	bad := strings.Replace(fixtureHooks, "retry-breaker", "retired-thing", 1)
	os.WriteFile(filepath.Join(f.root, "hooks/hooks.json"), []byte(bad), 0o644)
	if got := checkPluginHooks(f.root); !strings.Contains(got, `no subcommand "retired-thing"`) {
		t.Errorf("unknown subcommand: got %q", got)
	}
	os.WriteFile(filepath.Join(f.root, "hooks/hooks.json"), []byte(fixtureHooks), 0o644)
	os.Remove(filepath.Join(f.root, "scripts/run.sh"))
	if got := checkPluginHooks(f.root); !strings.Contains(got, "run.sh") || !strings.Contains(got, "not found") {
		t.Errorf("missing launcher: got %q", got)
	}
	os.Remove(filepath.Join(f.root, "hooks/hooks.json"))
	if got := checkPluginHooks(f.root); !strings.Contains(got, "hooks.json") {
		t.Errorf("missing hooks.json: got %q", got)
	}
}

func TestDuplicateGuards(t *testing.T) {
	tests := []struct {
		name, settings string
		want           int
	}{
		{"none", `{"hooks":{"PreToolUse":[{"hooks":[{"command":"rtk hook claude"}]}]}}`, 0},
		{"direct binary entry", `{"hooks":{"PreToolUse":[{"hooks":[{"command":"/b/claude-tools pre-tool-use"}]}]}}`, 1},
		{"quoted binary path", `{"hooks":{"PreToolUse":[{"hooks":[{"command":"\"$HOME/bin/claude-tools\" read-dedup"}]}]}}`, 1},
		{"launcher of the plugin copied in", `{"hooks":{"PreToolUse":[{"hooks":[{"command":"sh /x/guardrails/scripts/run.sh large-read-guard"}]}]}}`, 1},
		{"two entries", `{"hooks":{"PreToolUse":[{"hooks":[{"command":"/b/claude-tools pre-tool-use"},{"command":"/b/claude-tools retry-breaker"}]}]}}`, 2},
		{"non-guard subcommand", `{"hooks":{"SessionStart":[{"hooks":[{"command":"/b/claude-tools version"}]}]}}`, 0},
		{"statusline is not a guard", `{"statusLine":{"type":"command","command":"/b/claude-tools statusline"}}`, 0},
	}
	f := newPluginFixture(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f.writeSettings(t, tt.settings)
			if got := duplicateGuards([]string{f.settings}); len(got) != tt.want {
				t.Errorf("duplicateGuards = %q, want %d", got, tt.want)
			}
		})
	}
}

func TestCheckRTK(t *testing.T) {
	f := newPluginFixture(t)
	bins := filepath.Dir(filepath.Join(f.dir, "path", "sh"))
	f.writeSettings(t, `{"hooks":{"PreToolUse":[{"hooks":[{"command":"rtk hook claude"}]}]}}`)

	if got := checkRTK([]string{f.settings}); !strings.Contains(got, "not on PATH") {
		t.Errorf("hook names rtk, rtk absent: got %q", got)
	}
	os.WriteFile(filepath.Join(bins, "rtk"), []byte("#!/bin/sh\nexit 1\n"), 0o755)
	if got := checkRTK([]string{f.settings}); !strings.Contains(got, "rtk gain") {
		t.Errorf("a different rtk: got %q, want a hint about rtk gain", got)
	}
	os.WriteFile(filepath.Join(bins, "rtk"), []byte("#!/bin/sh\nexit 0\n"), 0o755)
	if got := checkRTK([]string{f.settings}); got != "" {
		t.Errorf("working rtk: got %q", got)
	}
	// no rtk hook configured: nothing to check, even with no rtk installed
	os.Remove(filepath.Join(bins, "rtk"))
	f.writeSettings(t, `{}`)
	if got := checkRTK([]string{f.settings}); got != "" {
		t.Errorf("no rtk hook: got %q", got)
	}
}

func TestDoctorHealthy(t *testing.T) {
	f := newPluginFixture(t)
	f.record(t, "binary bytes")
	old := stampVersion
	stampVersion = "v0.2.0"
	defer func() { stampVersion = old }()

	var out bytes.Buffer
	if code := doctor(&out, f.env(false)); code != 0 {
		t.Errorf("healthy: exit %d\n%s", code, out.String())
	}
	for _, want := range []string{"ok   plugin enabled", "ok   binary", "ok   hooks reachable", "ok   version"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("want %q in:\n%s", want, out.String())
		}
	}
	out.Reset()
	if code := doctor(&out, f.env(true)); code != 0 || out.Len() != 0 {
		t.Errorf("hook mode, healthy: want silence and exit 0, got %d:\n%s", code, out.String())
	}
}

// A problem is reported as one JSON reply for both readers and the exit status stays 0,
// so doctor can never block a session. The same problem gives exit 1 outside hook mode.
func TestDoctorProblems(t *testing.T) {
	f := newPluginFixture(t)
	f.record(t, "something else")
	f.writeSettings(t, `{"enabledPlugins":{"guardrails@broodhall":true},
	 "hooks":{"PreToolUse":[{"hooks":[{"command":"/b/claude-tools pre-tool-use"}]}]}}`)
	old := stampVersion
	stampVersion = "v0.1.2"
	defer func() { stampVersion = old }()

	var out bytes.Buffer
	if code := doctor(&out, f.env(false)); code != 1 {
		t.Errorf("problems, plain mode: exit %d, want 1", code)
	}
	for _, want := range []string{"does not match", "binary is 0.1.2 but the plugin is 0.2.0", "pre-tool-use"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("want %q in:\n%s", want, out.String())
		}
	}
	out.Reset()
	code := doctor(&out, f.env(true))
	user, model := decodeDoctorHook(t, out.Bytes())
	if code != 0 {
		t.Errorf("hook mode: exit %d, want 0", code)
	}
	for _, s := range []string{user, model} {
		if !strings.Contains(s, "does not match") || !strings.Contains(s, "pre-tool-use") {
			t.Errorf("reply lacks a problem: %q", s)
		}
	}
}

// Unreadable or broken settings, a missing plugin root: doctor says so and still exits 0 in hook mode.
func TestDoctorNeverBlocks(t *testing.T) {
	f := newPluginFixture(t)
	f.writeSettings(t, `{"hooks": `)
	e := f.env(true)
	e.PluginRoot = filepath.Join(f.dir, "nowhere")
	e.Settings = append(e.Settings, filepath.Join(f.dir, "missing.json"))
	var out bytes.Buffer
	if code := doctor(&out, e); code != 0 {
		t.Errorf("exit %d, want 0", code)
	}
	if out.Len() == 0 {
		t.Error("broken setup produced no reply")
	}
	decodeDoctorHook(t, out.Bytes())
}

// With no plugin root (the binary run by hand) the plugin checks are skipped, not failed.
func TestDoctorWithoutPluginRoot(t *testing.T) {
	f := newPluginFixture(t)
	e := f.env(false)
	e.PluginRoot = ""
	var out bytes.Buffer
	if code := doctor(&out, e); code != 0 {
		t.Errorf("exit %d\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "skip") {
		t.Errorf("want a skip note, got:\n%s", out.String())
	}
}

func TestDoctorCmdFlags(t *testing.T) {
	if code := doctorCmd([]string{"-no-such-flag"}); code != 2 {
		t.Errorf("bad flag: exit %d, want 2", code)
	}
	if code := doctorCmd([]string{"-no-such-flag", "-hook"}); code != 0 {
		t.Errorf("bad flag in hook mode: exit %d, want 0 (never block)", code)
	}
}

// decodeDoctorHook decodes doctor -hook output: SessionStart plain stdout reaches only Claude,
// so problems must come as one JSON reply whose systemMessage the user sees.
func decodeDoctorHook(t *testing.T, out []byte) (user, model string) {
	t.Helper()
	var r struct {
		SystemMessage      string `json:"systemMessage"`
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out, &r); err != nil {
		t.Fatalf("hook mode: want one JSON reply, got %v:\n%s", err, out)
	}
	if r.HookSpecificOutput.HookEventName != "SessionStart" {
		t.Errorf("hookEventName = %q, want SessionStart", r.HookSpecificOutput.HookEventName)
	}
	return r.SystemMessage, r.HookSpecificOutput.AdditionalContext
}

// Duplicates only matter when the plugin is in play: a standalone binary wired by hand is not a duplicate.
func TestDoctorDuplicatesNeedThePlugin(t *testing.T) {
	f := newPluginFixture(t)
	f.writeSettings(t, `{"hooks":{"PreToolUse":[{"hooks":[{"command":"/b/claude-tools pre-tool-use"}]}]}}`)
	e := f.env(false)
	e.PluginRoot = ""
	var out bytes.Buffer
	doctor(&out, e)
	if strings.Contains(out.String(), "duplicate") {
		t.Errorf("duplicate reported with no plugin installed:\n%s", out.String())
	}
}
