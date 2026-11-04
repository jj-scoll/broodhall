package main

// doctor: checks that the guardrails plugin install can do its job.
//
//	plugin enabled   settings name the plugin and do not disable it
//	binary           this binary matches the checksum the launcher recorded at install
//	version          the binary carries a version stamp equal to the plugin's version
//	hooks reachable  every command in the plugin's hooks.json can start and names a real subcommand
//	duplicate guards no settings.json hook runs a guard the plugin already runs
//	rtk              a configured rtk hook points at a working rtk
//
// The SessionStart launcher (`run.sh install`) runs `doctor -hook` once the binary is in place.
// -hook prints only problems, as one JSON reply the user and Claude both see, and always
// exits 0: a healthy setup prints nothing, and doctor can never block a session. Without
// -hook it prints every check and exits 1 when one failed.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"
)

// Registered here, not in main.go's table: doctor reads that table, and Go refuses a
// package variable whose initializer refers back to it.
func init() { commands["doctor"] = doctorCmd }

// guardSubcommands are the hooks the plugin itself runs; a settings.json entry for one of
// them makes the guard run twice. TestDoctorGuardListMatchesPluginHooks keeps it equal to
// hooks.json minus install.
var guardSubcommands = map[string]bool{
	"pre-tool-use": true, "read-dedup": true, "large-read-guard": true, "retry-breaker": true, "record-edit": true,
	"config-gate": true, "session-env": true, "receipt": true, "gate": true, "compact-guard": true, "autoclear": true,
}

// doctorEnv is what doctor looks at. PluginRoot is empty when the binary runs outside a
// plugin (by hand, or as a statusLine command): the plugin checks are skipped then.
type doctorEnv struct {
	PluginRoot string
	Exe        string
	Settings   []string
	Hook       bool
}

func doctorCmd(args []string) int {
	hook := false
	for _, a := range args {
		if a == "-hook" || a == "--hook" {
			hook = true
		}
	}
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var e doctorEnv
	settings := fs.String("settings", "", "comma-separated settings files to read (default: user, project and local settings)")
	fs.StringVar(&e.PluginRoot, "plugin-root", os.Getenv("CLAUDE_PLUGIN_ROOT"), "the plugin's install directory")
	fs.BoolVar(&e.Hook, "hook", false, "print only problems and exit 0 (SessionStart)")
	if fs.Parse(args) != nil {
		if hook {
			return 0 // a bad flag must not break a session
		}
		return 2
	}
	e.Exe, _ = os.Executable()
	if *settings != "" {
		e.Settings = strings.Split(*settings, ",")
	} else {
		e.Settings = defaultSettingsFiles()
	}
	return doctor(os.Stdout, e)
}

// defaultSettingsFiles lists the user settings (under CLAUDE_CONFIG_DIR when set) and the
// current project's shared and local settings.
func defaultSettingsFiles() []string {
	cfg := os.Getenv("CLAUDE_CONFIG_DIR")
	if cfg == "" {
		home, _ := os.UserHomeDir()
		cfg = filepath.Join(home, ".claude")
	}
	files := []string{filepath.Join(cfg, "settings.json")}
	if proj := os.Getenv("CLAUDE_PROJECT_DIR"); proj != "" {
		files = append(files, filepath.Join(proj, ".claude", "settings.json"), filepath.Join(proj, ".claude", "settings.local.json"))
	} else if wd, err := os.Getwd(); err == nil {
		files = append(files, filepath.Join(wd, ".claude", "settings.json"), filepath.Join(wd, ".claude", "settings.local.json"))
	}
	return files
}

func doctor(w io.Writer, e doctorEnv) int {
	if e.Hook {
		var problems bytes.Buffer
		defer func(out io.Writer) { doctorHookReply(out, problems.String()) }(w)
		w = &problems
	}
	failed := false
	report := func(level, subject, problem string) {
		if problem != "" {
			failed = true
			fmt.Fprintf(w, "claude-tools doctor: %s %s: %s\n", level, subject, problem)
		} else if !e.Hook {
			fmt.Fprintf(w, "ok   %s\n", subject)
		}
	}
	skip := func(msg string) {
		if !e.Hook {
			fmt.Fprintf(w, "skip %s\n", msg)
		}
	}

	if e.PluginRoot == "" {
		skip("plugin checks: not running inside a plugin (CLAUDE_PLUGIN_ROOT is unset)")
	} else {
		pluginVer := pluginVersion(e.PluginRoot)
		switch pluginEnabled(e.Settings, "guardrails") {
		case "enabled":
			report("FAIL", "plugin enabled", "")
		case "disabled":
			report("FAIL", "plugin enabled", "settings disable the guardrails plugin, yet its hooks are running")
		default:
			if strings.Contains(filepath.ToSlash(e.PluginRoot), "/plugins/cache/") {
				report("WARN", "plugin enabled", "no enabledPlugins entry for guardrails in "+strings.Join(e.Settings, ", ")+" (managed settings are not read)")
			} else {
				skip("plugin enabled: loaded from a directory, not a marketplace install")
			}
		}
		report("FAIL", "hooks reachable", checkPluginHooks(e.PluginRoot))
		bi, ok := debug.ReadBuildInfo()
		rev, dirty := buildRev(bi, ok)
		if p := checkVersion(buildVersion(bi, ok), rev, pluginVer); p != "" {
			report("FAIL", "version", p)
		} else {
			report("FAIL", "version "+versionString(buildVersion(bi, ok), rev, dirty), "")
		}
	}
	if e.Exe != "" {
		report("FAIL", "binary "+e.Exe, checkBinaryChecksum(e.Exe))
	}
	if e.PluginRoot != "" || pluginEnabled(e.Settings, "guardrails") == "enabled" {
		for _, d := range duplicateGuards(e.Settings) {
			report("WARN", "duplicate guard", d)
		}
	}
	if p := checkRTK(e.Settings); p != "" {
		report("WARN", "rtk", p)
	}

	if failed && !e.Hook {
		return 1
	}
	return 0
}

// doctorHookReply sends -hook problems to both readers: systemMessage shows in the user's
// transcript (plain SessionStart stdout reaches only Claude), additionalContext lets Claude
// help fix them. Nothing is written when there are no problems.
func doctorHookReply(w io.Writer, problems string) {
	problems = strings.TrimRight(problems, "\n")
	if problems == "" {
		return
	}
	var r struct {
		SystemMessage      string `json:"systemMessage"`
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	r.SystemMessage = problems
	r.HookSpecificOutput.HookEventName = "SessionStart"
	r.HookSpecificOutput.AdditionalContext = problems
	json.NewEncoder(w).Encode(r)
}

// pluginVersion reads the version from the plugin's plugin.json, "" when unreadable.
func pluginVersion(root string) string {
	b, err := os.ReadFile(filepath.Join(root, ".claude-plugin", "plugin.json"))
	if err != nil {
		return ""
	}
	var p struct {
		Version string `json:"version"`
	}
	if json.Unmarshal(b, &p) != nil {
		return ""
	}
	return p.Version
}

// checkVersion returns what is wrong with the binary's version stamp, or "".
func checkVersion(binVer, binRev, pluginVer string) string {
	if binVer == "" && binRev == "" {
		return "binary has no version stamp: reinstall it (delete the plugin data directory's bin/ and restart), or rebuild with -ldflags -X main.stampVersion=<tag>"
	}
	if binVer != "" && pluginVer != "" && binVer != pluginVer {
		return fmt.Sprintf("binary is %s but the plugin is %s: the launcher installs the matching binary at the next session start", binVer, pluginVer)
	}
	return ""
}

// checkBinaryChecksum compares the binary's sha256 with <binary>.sha256, which the launcher
// writes after it verified the release archive against checksums.txt and unpacked it. A
// binary installed before that record existed has none, and passes.
func checkBinaryChecksum(exe string) string {
	f, err := os.Open(exe)
	if err != nil {
		return exe + ": not found"
	}
	defer f.Close()
	rec, err := os.ReadFile(exe + ".sha256")
	if err != nil {
		return ""
	}
	want := strings.Fields(string(rec))
	if len(want) == 0 || len(want[0]) != sha256.Size*2 {
		return exe + ".sha256 is unreadable: delete it and the binary's version directory to reinstall"
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "cannot read " + exe + ": " + err.Error()
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != strings.ToLower(want[0]) {
		return "checksum does not match the one recorded at install (" + got[:12] + " vs " + want[0][:12] + "): the binary changed after install; delete its version directory to reinstall"
	}
	return ""
}

// readSettings decodes the settings files that exist and parse; one that fails is skipped,
// since doctor reports on the plugin, not on every settings file.
func readSettings(files []string) []settingsView {
	var out []settingsView
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var s settingsView
		if json.Unmarshal(b, &s) == nil {
			out = append(out, s)
		}
	}
	return out
}

type settingsView struct {
	EnabledPlugins map[string]any `json:"enabledPlugins"`
	Hooks          map[string][]struct {
		Hooks []struct {
			Command string `json:"command"`
		} `json:"hooks"`
	} `json:"hooks"`
}

// hookCommands returns every hook command in the settings, in file order.
func (s settingsView) hookCommands() []string {
	var out []string
	for _, groups := range s.Hooks {
		for _, g := range groups {
			for _, h := range g.Hooks {
				if h.Command != "" {
					out = append(out, h.Command)
				}
			}
		}
	}
	return out
}

// pluginEnabled is "enabled" when any settings file enables name@<marketplace>, "disabled"
// when one sets it false and none enables it, else "absent".
func pluginEnabled(files []string, name string) string {
	res := "absent"
	for _, s := range readSettings(files) {
		for k, v := range s.EnabledPlugins {
			if k != name && !strings.HasPrefix(k, name+"@") {
				continue
			}
			if v == true {
				return "enabled"
			}
			res = "disabled"
		}
	}
	return res
}

// checkPluginHooks starts from the plugin's hooks.json and returns the first command that
// could not start, or "".
func checkPluginHooks(root string) string {
	b, err := os.ReadFile(filepath.Join(root, "hooks", "hooks.json"))
	if err != nil {
		return "cannot read " + filepath.Join(root, "hooks", "hooks.json")
	}
	var s settingsView
	if err := json.Unmarshal(b, &s); err != nil {
		return "hooks/hooks.json: " + err.Error()
	}
	for _, c := range s.hookCommands() {
		c = strings.ReplaceAll(c, "${CLAUDE_PLUGIN_ROOT}", root)
		if p := checkHookCommand(c); p != "" {
			return p
		}
	}
	return ""
}

// checkHookCommand returns why a hook command could not start, or "". Only plugin launcher
// commands (`sh <script> <subcommand>`) and direct claude-tools calls are understood; the
// subcommand must be one of this binary's, or the launcher's own `install`.
func checkHookCommand(cmd string) string {
	words := strings.Fields(strings.ReplaceAll(cmd, `"`, ""))
	if len(words) == 0 {
		return "empty command"
	}
	for i := range words {
		words[i] = os.ExpandEnv(words[i])
	}
	prog := words[0]
	if strings.Contains(prog, "/") {
		if fi, err := os.Stat(prog); err != nil || fi.IsDir() || fi.Mode()&0o111 == 0 {
			return prog + ": not found or not executable"
		}
	} else if _, err := exec.LookPath(prog); err != nil {
		return prog + ": not on PATH"
	}
	sub := ""
	switch {
	case filepath.Base(prog) == "claude-tools" && len(words) > 1:
		sub = words[1]
	case filepath.Base(prog) == "claude-tools":
		return "claude-tools: no subcommand given"
	case (prog == "sh" || prog == "bash") && len(words) > 1:
		script := words[1]
		if _, err := os.Stat(script); err != nil {
			return script + ": not found"
		}
		if len(words) > 2 {
			sub = words[2]
		}
	}
	if sub != "" && sub != "install" && commands[sub] == nil {
		return fmt.Sprintf("claude-tools has no subcommand %q (in %q)", sub, cmd)
	}
	return ""
}

// duplicateGuards describes each settings.json hook that runs a guard the plugin runs too.
func duplicateGuards(files []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, s := range readSettings(files) {
		for _, c := range s.hookCommands() {
			words := strings.Fields(strings.ReplaceAll(c, `"`, ""))
			for i := 0; i+1 < len(words); i++ {
				base := filepath.Base(words[i])
				launcher := base == "run.sh" && strings.Contains(words[i], "guardrails")
				if (base == "claude-tools" || launcher) && guardSubcommands[words[i+1]] && !seen[c] {
					seen[c] = true
					out = append(out, fmt.Sprintf("settings hook %q runs %s, which the guardrails plugin runs too: every tool call is guarded twice; remove the settings entry", c, words[i+1]))
				}
			}
		}
	}
	return out
}

// checkRTK checks an rtk hook that settings configure: rtk must be on PATH and answer
// `rtk gain`, which another tool that is also named rtk does not.
func checkRTK(files []string) string {
	found := false
	for _, s := range readSettings(files) {
		for _, c := range s.hookCommands() {
			if w := strings.Fields(c); len(w) > 0 && filepath.Base(w[0]) == "rtk" {
				found = true
			}
		}
	}
	if !found {
		return ""
	}
	path, err := exec.LookPath("rtk")
	if err != nil {
		return "a hook runs rtk, which is not on PATH: install it or remove the hook"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, path, "gain").Run(); err != nil {
		return path + " does not answer `rtk gain` (" + err.Error() + "): it may be a different tool named rtk"
	}
	return ""
}
