package main

// config-gate (ConfigChange and SessionStart hook): judges a settings change by what it does
// to the guards before Claude Code loads it into the session. The guards themselves come from
// this plugin's hooks.json, which a settings edit cannot touch, so the gate protects the
// things that switch them off or step around them.
//
//   - Exit 2 (the session keeps its current settings) when the change weakens a guard: the
//     guardrails plugin disabled or dropped from enabledPlugins; a family added to the plugin's
//     disabled_guards option; the observe option turned on; an entry added to the
//     allowed_email_domains option; a GUARDRAILS_* or CLAUDE_TOOLS_* env var (GUARDRAILS_OFF among
//     them) changed;
//     a PreToolUse or PermissionRequest hook added, since it can allow or rewrite a call the
//     guards ask about; a PreToolUse, PermissionRequest or ConfigChange hook, or any
//     onFailure:"block" hook, removed; onFailure:"block" dropped; disableAllHooks set;
//     defaultMode set to bypassPermissions; a deny or ask rule removed.
//   - Exit 1 (stderr shown to the user, the change applies) when it newly allows a way for
//     data to leave the machine: curl, wget, gh, git push, aws s3, ssh, interpreters, any
//     Bash, WebFetch, MCP tools that send or store data.
//
// Not covered: `claude plugin uninstall` and edits to managed settings, which do not arrive
// as a user, project or local settings change.
//
// Claude Code passes {source, file_path}. A change it staged for review arrives as
// <settings file>.proposed-<8 hex> beside the target, and exit 2 means it is never written.
// A direct edit is already on disk: exit 2 only keeps this session on its old settings, and
// the next session loads the edit. So the baseline is a snapshot of what the session loaded,
// in $XDG_STATE_HOME/claude-tools/config-gate/: taken at SessionStart (startup only) for the
// user and project settings files, moved on by every change that applies, kept by a blocked
// one. Without a snapshot, a proposal is compared with its target, and a direct edit applies
// and becomes the snapshot. A file that cannot be read blocks; invalid JSON passes, because
// Claude Code rejects it itself. Known gap: snapshots are shared, so a session started after
// a blocked edit re-snapshots it, and an older session then diffs its next change against
// the weakened file. The settings-file ask in pre-tool-use is the lock; this is the tripwire.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

func configGate(args []string) int {
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config-gate:", err)
		return 2
	}
	return configGateRun(data, userPath(stateBase(), "config-gate"), os.Stderr)
}

var proposalRE = regexp.MustCompile(`^(.+)\.proposed-[0-9a-f]{8}$`)

func configGateRun(data []byte, stateDir string, stderr io.Writer) int {
	var in struct {
		Event    string `json:"hook_event_name"`
		Source   string `json:"source"`
		FilePath string `json:"file_path"`
		Cwd      string `json:"cwd"`
	}
	if err := json.Unmarshal(data, &in); err != nil {
		fmt.Fprintln(stderr, "config-gate: bad hook input:", err)
		return 2
	}
	if in.Event == "SessionStart" {
		// Only a new process reads settings from disk. After compact or clear it still runs
		// what it loaded, so re-snapshotting would accept an edit this gate blocked.
		if in.Source == "startup" {
			snapshotSettings(stateDir, in.Cwd)
		}
		return 0
	}
	switch in.Source {
	case "user_settings", "project_settings", "local_settings":
	default:
		return 0
	}
	target, proposal := in.FilePath, false
	if m := proposalRE.FindStringSubmatch(filepath.Base(in.FilePath)); m != nil {
		target, proposal = filepath.Join(filepath.Dir(in.FilePath), m[1]), true
	}
	next, err := os.ReadFile(in.FilePath)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintf(stderr, "config-gate: cannot read %s, so the change is blocked: %v\n", in.FilePath, err)
		return 2
	}
	snap := snapshotPath(stateDir, target)
	old, err := os.ReadFile(snap)
	if err != nil && proposal {
		old, err = os.ReadFile(target)
	}
	if err != nil {
		saveSnapshot(snap, next)
		return 0
	}
	weak, egress, err := settingsDiff(old, next)
	if err != nil {
		return 0
	}
	if len(weak) > 0 {
		fmt.Fprintf(stderr, "config-gate: blocked the change to %s; this session keeps its current settings.\n  weakens a guard:\n", target)
		for _, w := range weak {
			fmt.Fprintln(stderr, "    - "+w)
		}
		if len(egress) > 0 {
			fmt.Fprintln(stderr, "  newly allows data to leave the machine:")
			for _, e := range egress {
				fmt.Fprintln(stderr, "    + "+e)
			}
		}
		if !proposal {
			fmt.Fprintln(stderr, "  The file itself is changed on disk, so the next session loads it.")
		}
		return 2
	}
	saveSnapshot(snap, next)
	if len(egress) > 0 {
		fmt.Fprintf(stderr, "config-gate: %s newly allows data to leave the machine:\n", target)
		for _, e := range egress {
			fmt.Fprintln(stderr, "  + "+e)
		}
		return 1
	}
	return 0
}

// snapshotSettings records the user and project settings files a session starts with.
func snapshotSettings(stateDir, cwd string) {
	cfg := os.Getenv("CLAUDE_CONFIG_DIR")
	if cfg == "" {
		cfg = filepath.Join(os.Getenv("HOME"), ".claude")
	}
	dirs := []string{cfg}
	if cwd != "" {
		dirs = append(dirs, filepath.Join(cwd, ".claude"))
	}
	for _, d := range dirs {
		for _, name := range []string{"settings.json", "settings.local.json"} {
			p := filepath.Join(d, name)
			if b, err := os.ReadFile(p); err == nil {
				saveSnapshot(snapshotPath(stateDir, p), b)
			} else if errors.Is(err, fs.ErrNotExist) {
				os.Remove(snapshotPath(stateDir, p))
			}
		}
	}
}

// snapshotPath keys a settings file by its symlink-resolved path, so a second config
// directory that links to ~/.claude/settings.json shares its snapshot. A deleted file
// resolves through its directory, so it still finds the snapshot taken while it existed.
func snapshotPath(stateDir, file string) string {
	p := filepath.Clean(file)
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	} else if d, err := filepath.EvalSymlinks(filepath.Dir(p)); err == nil {
		p = filepath.Join(d, filepath.Base(p))
	}
	sum := sha256.Sum256([]byte(p))
	return filepath.Join(stateDir, hex.EncodeToString(sum[:8])+".json")
}

func saveSnapshot(path string, data []byte) {
	if os.MkdirAll(filepath.Dir(path), 0o700) != nil {
		return
	}
	tmp := path + ".tmp"
	if os.WriteFile(tmp, data, 0o600) == nil {
		os.Rename(tmp, path)
	}
}

type settingsHook struct {
	Type      string `json:"type"`
	Command   string `json:"command"`
	Prompt    string `json:"prompt"`
	OnFailure string `json:"onFailure"`
}

type settingsDoc struct {
	Env         map[string]any `json:"env"`
	Permissions struct {
		Allow       []string `json:"allow"`
		Deny        []string `json:"deny"`
		Ask         []string `json:"ask"`
		DefaultMode string   `json:"defaultMode"`
	} `json:"permissions"`
	Hooks map[string][]struct {
		Matcher string         `json:"matcher"`
		Hooks   []settingsHook `json:"hooks"`
	} `json:"hooks"`
	DisableAllHooks bool `json:"disableAllHooks"`
	// EnabledPlugins values are true, false, or a list of versions in some Claude Code builds.
	EnabledPlugins map[string]any `json:"enabledPlugins"`
	PluginConfigs  map[string]struct {
		Options map[string]any `json:"options"`
	} `json:"pluginConfigs"`
}

// guardsPlugin prefixes the id ("guardrails@<marketplace>") of this plugin in settings.
const guardsPlugin = "guardrails@"

// defaultDisabledGuards is plugin.json's default for disabled_guards, used when the option
// is unset.
const defaultDisabledGuards = ""

// observeOption reports whether the observe option is on for id. The option is a boolean, and
// Claude Code may store it as a string; observeOn takes the same words the guard does.
func (s settingsDoc) observeOption(id string) bool {
	switch v := s.PluginConfigs[id].Options["observe"].(type) {
	case bool:
		return v
	case string:
		return observeOn(v)
	case float64:
		return v != 0 // Claude Code may hand the guard "1" for a number
	}
	return false
}

// allowedDomains returns the entries of the allowed_email_domains option for id, lower-cased.
func (s settingsDoc) allowedDomains(id string) map[string]bool {
	out := map[string]bool{}
	var parts []string
	switch v := s.PluginConfigs[id].Options["allowed_email_domains"].(type) {
	case string:
		parts = strings.Split(v, ",")
	case []any: // an array option: each element may itself hold a comma list
		for _, e := range v {
			if str, ok := e.(string); ok {
				parts = append(parts, strings.Split(str, ",")...)
			}
		}
	}
	for _, d := range parts {
		if d = strings.ToLower(strings.TrimSpace(d)); d != "" {
			out[d] = true
		}
	}
	return out
}

// pluginEnabled reports whether enabledPlugins lists id and does not say false.
func (s settingsDoc) pluginEnabled(id string) bool {
	v, ok := s.EnabledPlugins[id]
	return ok && v != false
}

// disabledGuards returns the families the plugin option turns off for id.
func (s settingsDoc) disabledGuards(id string) map[string]bool {
	switch v := s.PluginConfigs[id].Options["disabled_guards"].(type) {
	case string:
		return guardsOff(v)
	case []any:
		parts := make([]string, 0, len(v))
		for _, e := range v {
			if str, ok := e.(string); ok {
				parts = append(parts, str)
			}
		}
		return guardsOff(strings.Join(parts, ","))
	}
	return guardsOff(defaultDisabledGuards)
}

// guardEvents are the hook events whose hooks can stop a tool call or a settings change.
var guardEvents = map[string]bool{"PreToolUse": true, "PermissionRequest": true, "ConfigChange": true}

// hookKeys maps "Event [matcher] command" to whether that hook has onFailure:"block".
func (s settingsDoc) hookKeys() map[string]bool {
	keys := map[string]bool{}
	for event, groups := range s.Hooks {
		for _, g := range groups {
			for _, h := range g.Hooks {
				what := h.Command
				if what == "" {
					what = h.Type + ": " + h.Prompt
				}
				k := event + " [" + g.Matcher + "] " + what
				keys[k] = keys[k] || h.OnFailure == "block"
			}
		}
	}
	return keys
}

// settingsDiff compares two settings files' contents (nil next: the file was deleted).
func settingsDiff(old, next []byte) (weakened, egress []string, err error) {
	var a, b settingsDoc
	if len(old) > 0 {
		json.Unmarshal(old, &a) // an unreadable baseline counts as empty
	}
	if len(next) > 0 {
		if err := json.Unmarshal(next, &b); err != nil {
			return nil, nil, err
		}
	}
	oldHooks, newHooks := a.hookKeys(), b.hookKeys()
	for k, block := range oldHooks {
		nowBlock, kept := newHooks[k]
		switch {
		case !kept && (block || guardEvents[strings.SplitN(k, " ", 2)[0]]):
			weakened = append(weakened, "hook removed: "+k)
		case kept && block && !nowBlock:
			weakened = append(weakened, "onFailure:block dropped: "+k)
		}
	}
	for k := range newHooks {
		if _, had := oldHooks[k]; !had && (strings.HasPrefix(k, "PreToolUse ") || strings.HasPrefix(k, "PermissionRequest ")) {
			weakened = append(weakened, "hook added: "+k)
		}
	}
	// Options are judged for every guardrails id either file mentions, since the plugin may be
	// enabled in another file (project or managed settings) while this one holds its options.
	// Disabling the plugin is judged only for a file that had enabled it.
	ids := map[string]bool{}
	for _, d := range []settingsDoc{a, b} {
		for id := range d.EnabledPlugins {
			ids[id] = true
		}
		for id := range d.PluginConfigs {
			ids[id] = true
		}
	}
	for _, id := range slices.Sorted(maps.Keys(ids)) {
		if !strings.HasPrefix(id, guardsPlugin) {
			continue
		}
		if a.pluginEnabled(id) && !b.pluginEnabled(id) {
			weakened = append(weakened, "plugin disabled: "+id)
			continue
		}
		was := a.disabledGuards(id)
		for _, f := range slices.Sorted(maps.Keys(b.disabledGuards(id))) {
			if !was[f] {
				weakened = append(weakened, "guard family turned off: "+f+" ("+id+" disabled_guards)")
			}
		}
		if b.observeOption(id) && !a.observeOption(id) {
			weakened = append(weakened, "observe mode turned on: the optional guards only log ("+id+" observe)")
		}
		wasDomains := a.allowedDomains(id)
		for _, d := range slices.Sorted(maps.Keys(b.allowedDomains(id))) {
			if !wasDomains[d] {
				weakened = append(weakened, "email domain allowed: "+d+" ("+id+" allowed_email_domains)")
			}
		}
	}
	if b.DisableAllHooks && !a.DisableAllHooks {
		weakened = append(weakened, "disableAllHooks set")
	}
	if b.Permissions.DefaultMode == "bypassPermissions" && a.Permissions.DefaultMode != "bypassPermissions" {
		weakened = append(weakened, "defaultMode set to bypassPermissions")
	}
	for _, r := range a.Permissions.Deny {
		if !slices.Contains(b.Permissions.Deny, r) {
			weakened = append(weakened, "deny rule removed: "+r)
		}
	}
	for _, r := range a.Permissions.Ask {
		if !slices.Contains(b.Permissions.Ask, r) {
			weakened = append(weakened, "ask rule removed: "+r)
		}
	}
	for _, k := range envKeys(a.Env, b.Env) {
		if (strings.HasPrefix(k, "GUARDRAILS_") || strings.HasPrefix(k, "CLAUDE_TOOLS_")) && fmt.Sprint(a.Env[k]) != fmt.Sprint(b.Env[k]) {
			weakened = append(weakened, "env "+k+" changed")
		}
	}
	for _, r := range b.Permissions.Allow {
		if !slices.Contains(a.Permissions.Allow, r) && !slices.Contains(egress, r) && allowEgress(r) {
			egress = append(egress, r)
		}
	}
	slices.Sort(weakened)
	return weakened, egress, nil
}

func envKeys(maps ...map[string]any) []string {
	var keys []string
	for _, m := range maps {
		for k := range m {
			if !slices.Contains(keys, k) {
				keys = append(keys, k)
			}
		}
	}
	return keys
}

var (
	// egressWords run as the first word of an allowed command can send data anywhere.
	egressWords = map[string]bool{"curl": true, "wget": true, "scp": true, "sftp": true, "rsync": true, "ssh": true,
		"nc": true, "ncat": true, "socat": true, "telnet": true, "ftp": true, "http": true, "https": true, "xh": true,
		"gcloud": true, "gsutil": true, "az": true, "mail": true, "sendmail": true, "bash": true, "sh": true,
		"zsh": true, "env": true, "xargs": true, "eval": true, "node": true, "deno": true, "bun": true, "npx": true,
		"perl": true, "ruby": true, "php": true}
	ghSendRE  = regexp.MustCompile(`^(api|gist|release|create|comment|edit|upload|review|send)$`)
	awsSendRE = regexp.MustCompile(`^publish$|^(put|send|publish|upload)-`)
	// mcpReadRE: tools named for reading, whatever else their name holds (getTransitionsFor...).
	mcpReadRE = regexp.MustCompile(`^(?i:get|list|search|fetch|read|lookup|query|find)`)
)

// allowEgress reports whether an allow rule lets a tool call send data off the machine.
func allowEgress(rule string) bool {
	switch {
	case rule == "Bash", rule == "WebFetch", strings.HasPrefix(rule, "WebFetch("):
		return true
	case strings.HasPrefix(rule, "mcp__"):
		rest := strings.TrimSuffix(rule[len("mcp__"):], "__*")
		i := strings.LastIndex(rest, "__")
		return i < 0 || egressToolVerb.MatchString(rest[i+2:]) && !mcpReadRE.MatchString(rest[i+2:])
	case !strings.HasPrefix(rule, "Bash(") || !strings.HasSuffix(rule, ")"):
		return false
	}
	inner := strings.TrimSuffix(strings.TrimPrefix(rule, "Bash("), ")")
	inner = strings.TrimSpace(strings.TrimRight(strings.TrimSuffix(inner, ":*"), "* "))
	words := strings.Fields(inner)
	for len(words) > 0 && varAssignRE.MatchString(words[0]) {
		words = words[1:]
	}
	if len(words) == 0 {
		return true
	}
	first := filepath.Base(words[0])
	switch {
	case egressWords[first], pythonRE.MatchString(first):
		return true
	case first == "git":
		return len(words) == 1 || words[1] == "push"
	case first == "gh":
		return len(words) == 1 || slices.ContainsFunc(words[1:], ghSendRE.MatchString)
	case first == "aws" && len(words) > 1 && words[1] == "s3":
		return len(words) == 2 || words[2] == "cp" || words[2] == "mv" || words[2] == "sync"
	case first == "aws":
		return len(words) == 1 || slices.ContainsFunc(words[1:], awsSendRE.MatchString)
	}
	return false
}
