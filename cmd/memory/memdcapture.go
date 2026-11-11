package main

// Capture rules for tool calls, applied before anything is stored or sent to the observer.
// Masking (memdRedact) finds secret-shaped strings; these rules keep whole classes of tool
// call out of memory, whatever their text looks like:
//
//   - A call that reads a credential file (Read or Grep on it, or a Bash command naming it)
//     keeps its input but not its output; a call that writes one (Write, Edit, ...) keeps
//     only the path. The file list is guardrails' (cmd/guardrails/secretfiles).
//   - A Bash command whose output is a credential (gh auth token, kubectl get secret, ...)
//     keeps its input but not its output.
//   - A call made in a directory listed in capture_exclude_dirs, or any call when
//     capture_tools is off, keeps the tool name and whether it errored, nothing else, and is
//     not queued for an observation.
//
// The stored row and the observer payload are both built from the same event, so a rule
// applied here holds for both. captureToolCall is the one entry point for other readers of
// transcripts (the history import).

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/jj-scoll/broodhall/cmd/guardrails/secretfiles"
)

const (
	captureFilePlace = "[output not kept: credential file]"
	captureCmdPlace  = "[output not kept: credential command]"
	// captureErrorMark is the output of a name-only call that errored.
	captureErrorMark = "[error]"
)

// captureSettings is the capture configuration. Home is the home directory that ~ and $HOME
// mean in a path.
type captureSettings struct {
	Tools       bool
	ExcludeDirs []string // absolute, cleaned
	Home        string
}

// captureCfg is the configuration in force; memdServe loads it before the tailer starts.
var captureCfg = defaultCaptureSettings()

func defaultCaptureSettings() captureSettings {
	home, _ := os.UserHomeDir()
	return captureSettings{Tools: true, Home: home}
}

type captureMode int

const (
	captureFull     captureMode = iota
	captureNoOutput             // keep the input, replace the output with a placeholder
	captureNoWrite              // keep only the path of what was written
	captureNameOnly             // keep the tool name and the error flag
)

// captureDecision is what to keep of one tool call. The zero value keeps everything.
type captureDecision struct {
	mode        captureMode
	placeholder string
	pathKey     string // captureNoWrite: the input key that held the path
	path        string
}

// queued reports whether the call goes to the observer: a name-only call has nothing to observe.
func (d captureDecision) queued() bool { return d.mode != captureNameOnly }

// input is the input to store for the (already masked) input text.
func (d captureDecision) input(masked string) string {
	switch d.mode {
	case captureNameOnly:
		return ""
	case captureNoWrite:
		b, _ := json.Marshal(map[string]string{d.pathKey: d.path})
		return string(b)
	}
	return masked
}

// output is the output to store for the (already masked) output text.
func (d captureDecision) output(masked string, errored bool) string {
	switch d.mode {
	case captureNameOnly:
		if errored {
			return captureErrorMark
		}
		return ""
	case captureNoOutput, captureNoWrite:
		return d.placeholder
	}
	return masked
}

// captureToolCall applies the rules to one finished tool call: inputJSON is the raw tool input
// as compact JSON, output the raw result text. It returns the masked input and output to keep
// and whether the call is queued for an observation.
func captureToolCall(cfg captureSettings, cwd, tool, inputJSON, output string, errored bool) (in, out string, queue bool) {
	d := captureFor(cfg, cwd, tool, inputJSON)
	return d.input(memdRedact(inputJSON)), d.output(memdRedact(output), errored), d.queued()
}

// captureFor decides what to keep of a tool call from its working directory, tool name and raw
// input JSON.
func captureFor(cfg captureSettings, cwd, tool, inputJSON string) captureDecision {
	if !cfg.Tools || captureExcluded(cfg, cwd) {
		return captureDecision{mode: captureNameOnly}
	}
	var in map[string]any
	if json.Unmarshal([]byte(inputJSON), &in) != nil {
		return captureDecision{}
	}
	str := func(k string) string { s, _ := in[k].(string); return s }
	switch tool {
	case "Write", "Edit", "MultiEdit", "NotebookEdit":
		for _, k := range []string{"file_path", "notebook_path", "path"} {
			if p := str(k); p != "" {
				if captureCredFile(cfg, cwd, p) {
					return captureDecision{mode: captureNoWrite, placeholder: captureFilePlace, pathKey: k, path: p}
				}
				break
			}
		}
	case "Read", "NotebookRead", "Grep":
		if captureCredFile(cfg, cwd, str("file_path")) || captureCredFile(cfg, cwd, str("notebook_path")) ||
			captureCredFile(cfg, cwd, str("path")) || tool == "Grep" && captureCredGlob(cfg, cwd, str("glob")) {
			return captureDecision{mode: captureNoOutput, placeholder: captureFilePlace}
		}
	case "Bash", "PowerShell":
		cmd := str("command")
		if captureCmdNamesCred(cfg, cwd, cmd) {
			return captureDecision{mode: captureNoOutput, placeholder: captureFilePlace}
		}
		if credCommandRE.MatchString(strings.Join(strings.Fields(cmd), " ")) {
			return captureDecision{mode: captureNoOutput, placeholder: captureCmdPlace}
		}
	}
	return captureDecision{}
}

// captureExcluded reports whether cwd is, or is inside, an excluded directory.
func captureExcluded(cfg captureSettings, cwd string) bool {
	if cwd == "" || len(cfg.ExcludeDirs) == 0 {
		return false
	}
	c := filepath.Clean(cwd)
	cands := []string{c, evalBest(c)}
	for _, d := range cfg.ExcludeDirs {
		for _, dd := range []string{d, evalBest(d)} {
			for _, cc := range cands {
				if cc == dd || strings.HasPrefix(cc, strings.TrimSuffix(dd, "/")+"/") {
					return true
				}
			}
		}
	}
	return false
}

// evalBest follows symlinks when the path exists and returns it cleaned otherwise.
func evalBest(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

// captureAbs makes a path word absolute: ~, $HOME and ${HOME} expanded, relative paths taken
// from cwd. It returns "" when the word holds another expansion.
func captureAbs(cfg captureSettings, cwd, w string) string {
	for _, v := range []string{"${HOME}", "$HOME"} {
		if rest, ok := strings.CutPrefix(w, v); ok && cfg.Home != "" && (rest == "" || rest[0] == '/') {
			w = cfg.Home + rest
		}
	}
	if w == "~" {
		w = cfg.Home
	} else if rest, ok := strings.CutPrefix(w, "~/"); ok {
		w = filepath.Join(cfg.Home, rest)
	}
	if w == "" || strings.ContainsAny(w, "$`") {
		return ""
	}
	if !filepath.IsAbs(w) {
		w = filepath.Join(cwd, w)
	}
	return filepath.Clean(w)
}

// captureCredFile reports whether one path names a credential file, as written or with
// symlinks followed.
func captureCredFile(cfg captureSettings, cwd, p string) bool {
	a := captureAbs(cfg, cwd, p)
	if a == "" {
		return false
	}
	return secretfiles.Match(a, cfg.Home) || secretfiles.Match(evalBest(a), cfg.Home)
}

// captureCredGlob reports whether a glob (a Grep glob argument) could select a credential file.
func captureCredGlob(cfg captureSettings, cwd, g string) bool {
	if g == "" {
		return false
	}
	return captureGlobHitsCred(cfg, filepath.Join(cwd, "x"), g, false) || captureCredFile(cfg, cwd, g)
}

// captureProbes are file names a glob is tried against, because the file need not exist on
// this machine (an import runs on a different host than the original session).
var captureProbes = []string{".env", ".env.local", ".env.production", ".npmrc", "x.pem", "x.tfvars", "credentials", "id_rsa", "id_ed25519"}

// captureGlobHitsCred reports whether a glob word names a credential file: it matches an
// existing one, or a probe name in the same directory that is protected.
func captureGlobHitsCred(cfg captureSettings, ref, g string, abs bool) bool {
	if !abs {
		g = filepath.Join(filepath.Dir(ref), g)
	}
	if ms, _ := filepath.Glob(g); len(ms) > 0 {
		for _, m := range ms {
			if secretfiles.Match(m, cfg.Home) || secretfiles.Match(evalBest(m), cfg.Home) {
				return true
			}
		}
	}
	dir := filepath.Dir(g)
	probes := append([]string{}, captureProbes...)
	for _, pat := range secretfiles.Paths {
		if !strings.ContainsAny(pat, "*?[") {
			probes = append(probes, filepath.Base(pat))
		}
	}
	for _, name := range probes {
		cand := filepath.Join(dir, name)
		if ok, _ := filepath.Match(g, cand); ok && secretfiles.Match(cand, cfg.Home) {
			return true
		}
	}
	// A glob on a full protected path, such as ~/.aws/cred*.
	for _, pat := range secretfiles.Paths {
		if rest, ok := strings.CutPrefix(pat, "~/"); ok {
			full := filepath.Join(cfg.Home, rest)
			if !strings.ContainsAny(full, "*?[") {
				if ok, _ := filepath.Match(g, full); ok {
					return true
				}
			}
		}
	}
	return false
}

// captureSplitRE splits a command into candidate path words: whitespace, quotes and shell
// punctuation, plus = and : so that --env-file=.env, VAR=.env and host:.env are found.
var captureSplitRE = regexp.MustCompile("[\\s'\"`;|&<>(){}=:,\\\\]+")

// captureCmdNamesCred reports whether a shell command names a credential file in any word.
// It is textual: a path built from a variable that is not set in the command is not found.
func captureCmdNamesCred(cfg captureSettings, cwd, cmd string) bool {
	for _, w := range captureSplitRE.Split(strings.ReplaceAll(cmd, "${HOME}", "$HOME"), -1) {
		if w == "" {
			continue
		}
		if strings.ContainsAny(w, "*?[") {
			a := captureAbs(cfg, cwd, w)
			if a != "" && captureGlobHitsCred(cfg, a, a, true) {
				return true
			}
			continue
		}
		if captureCredFile(cfg, cwd, w) {
			return true
		}
	}
	return false
}

// credCommandRE matches commands whose output is a credential, on a command with its spaces
// collapsed. Keep it short and explicit; add a command here only if its output is the secret.
var credCommandRE = regexp.MustCompile(`(?i)(?:^|[\s;&|(` + "`" + `])(?:` + strings.Join([]string{
	`gh auth token\b`,
	`kubectl\b.*\bget (?:secrets?|secrets?/\S+)(?:\s|$)`,
	`kubectl\b.*\bcreate token\b`,
	`aws\b.*\bconfigure get \S*(?:secret|session_token)`,
	`aws\b.*\bsts (?:get-session-token|assume-role\S*|get-federation-token)\b`,
	`aws\b.*\bsecretsmanager get-secret-value\b`,
	`aws\b.*\bssm get-parameters?\b.*--with-decryption`,
	`aws\b.*\becr get-login-password\b`,
	`gcloud\b.*\bprint-(?:access|identity)-token\b`,
	`az account get-access-token\b`,
	`op read\b`,
	`vault (?:read|kv get)\b`,
	`security\b.*\bfind-(?:generic|internet)-password\b.*\s-\w*w`,
}, "|") + `)`)

// loadCaptureSettings reads capture_tools (bool) and capture_exclude_dirs (a comma-separated
// string or a list) from the settings file, then lets MEMORY_CAPTURE_TOOLS (0/false/no/off or
// 1/true/yes/on) and MEMORY_CAPTURE_EXCLUDE_DIRS (a :-separated list) override them. A missing
// or unparsable file, or a value of the wrong type, keeps the default: capture everything, no
// excluded directories. A relative directory is ignored.
func loadCaptureSettings(path string) captureSettings {
	c := defaultCaptureSettings()
	var dirs []string
	if b, err := os.ReadFile(path); err == nil {
		var s map[string]any
		if err := json.Unmarshal(b, &s); err != nil {
			log.Printf("memory: capture settings: %s: %v; using defaults", path, err)
		} else {
			if v, ok := captureBool(s["capture_tools"]); ok {
				c.Tools = v
			}
			switch v := s["capture_exclude_dirs"].(type) {
			case string:
				dirs = strings.Split(v, ",")
			case []any:
				for _, x := range v {
					if s, ok := x.(string); ok {
						dirs = append(dirs, s)
					}
				}
			}
		}
	} else if !os.IsNotExist(err) {
		log.Printf("memory: capture settings: %v; using defaults", err)
	}
	if v, ok := captureBool(os.Getenv("MEMORY_CAPTURE_TOOLS")); ok {
		c.Tools = v
	}
	if v := os.Getenv("MEMORY_CAPTURE_EXCLUDE_DIRS"); v != "" {
		dirs = strings.Split(v, ":")
	}
	for _, d := range dirs {
		d = strings.TrimSpace(d)
		if d == "~" {
			d = c.Home
		} else if rest, ok := strings.CutPrefix(d, "~/"); ok {
			d = filepath.Join(c.Home, rest)
		}
		if filepath.IsAbs(d) {
			c.ExcludeDirs = append(c.ExcludeDirs, filepath.Clean(d))
		} else if d != "" {
			log.Printf("memory: capture settings: excluded directory %q ignored: not an absolute path", d)
		}
	}
	return c
}

// captureBool reads a bool setting from a JSON bool or a string such as "false" or "0".
func captureBool(v any) (val, ok bool) {
	switch t := v.(type) {
	case bool:
		return t, true
	case string:
		switch strings.ToLower(strings.TrimSpace(t)) {
		case "0", "false", "no", "off":
			return false, true
		case "1", "true", "yes", "on":
			return true, true
		}
	}
	return false, false
}
