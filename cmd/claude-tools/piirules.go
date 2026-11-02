package main

// PII column rules: where they come from at run time, and the `pii-rules` subcommand.
//
// Load order: (1) $XDG_CONFIG_HOME/claude-tools/pii-columns.json, or
// ~/.config/claude-tools/pii-columns.json; (2) the copy embedded in the binary. The user owns the
// file; the model must not be able to loosen masking, so:
//   - the path has no claude-tools specific environment override (the model could set one in
//     its own Bash command). XDG_CONFIG_HOME and HOME are the only inputs, and the
//     PreToolUse guard (piirulesguard.go) denies Bash commands that assign either one;
//   - a missing, unreadable or invalid file falls back to the embedded rules, never to "no
//     rules" and never to a panic: every masking path (redact, pii-mirror) goes through
//     loadPIIRules, which cannot fail.
//
// `claude-tools doctor` reports which source is active. `pii-rules init` seeds the file from
// the embedded copy; `pii-rules check [path]` validates one.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
)

// piiRulesFile is the file name under <config dir>/claude-tools/.
const piiRulesFile = "pii-columns.json"

// piiRulesPath returns the user rules file path, or "" when none can be named. A variable so
// tests choose a path without any environment input.
var piiRulesPath = defaultPIIRulesPath

func defaultPIIRulesPath() string {
	home, _ := os.UserHomeDir()
	return piiRulesPathFor(os.Getenv("XDG_CONFIG_HOME"), home)
}

// piiRulesPathFor follows the XDG rule: a relative XDG_CONFIG_HOME is ignored.
func piiRulesPathFor(xdg, home string) string {
	switch {
	case filepath.IsAbs(xdg):
		return filepath.Join(xdg, "claude-tools", piiRulesFile)
	case home != "":
		return filepath.Join(home, ".config", "claude-tools", piiRulesFile)
	}
	return ""
}

// embeddedCategories are the categories the embedded rules use, tiers included. A user file
// naming any other category is rejected: it would mask with a tag no consumer knows.
var embeddedCategories = sync.OnceValue(func() map[string]bool {
	var f struct{ Rules []struct{ Category string } }
	if err := json.Unmarshal(piiColumnsJSON, &f); err != nil {
		panic("embedded pii-columns.json: " + err.Error())
	}
	m := map[string]bool{}
	for _, r := range f.Rules {
		m[r.Category] = true
	}
	return m
})

// validatePIIRules parses a rules document and applies the user-file checks on top of
// parsePIIRules: known categories, and at least one active rule. total counts every rule in
// the file, active or not.
func validatePIIRules(data []byte) (rs *piiRuleSet, total int, err error) {
	var f struct {
		Rules []struct{ Name, Category string }
	}
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, 0, fmt.Errorf("pii-columns.json: %w", err)
	}
	known := embeddedCategories()
	for i, r := range f.Rules {
		if !known[r.Category] {
			return nil, 0, fmt.Errorf("pii-columns.json: rule %d (%q): unknown category %q", i, r.Name, r.Category)
		}
	}
	if rs, err = parsePIIRules(data); err != nil {
		return nil, 0, err
	}
	if len(rs.rules) == 0 {
		return nil, 0, errors.New("pii-columns.json: no active rules (tier 1 or default tier 2)")
	}
	return rs, len(f.Rules), nil
}

// loadPIIRulesSource returns the rules in force and where they came from: the file path, or
// "embedded". fileErr is why a present file was not used; a missing file is not an error.
// rs is never nil.
func loadPIIRulesSource() (rs *piiRuleSet, source string, fileErr error) {
	if p := piiRulesPath(); p != "" {
		data, err := os.ReadFile(p)
		switch {
		case errors.Is(err, fs.ErrNotExist):
		case err != nil:
			fileErr = err
		default:
			var ferr error
			if rs, _, ferr = validatePIIRules(data); ferr == nil {
				return rs, p, nil
			}
			fileErr = ferr
		}
	}
	rs, err := parsePIIRules(piiColumnsJSON)
	if err != nil {
		panic("embedded " + err.Error()) // a build defect, caught by TestEmbeddedRulesParse
	}
	return rs, "embedded", fileErr
}

func piiRulesCmd(args []string) int { return piiRulesRun(os.Stdout, os.Stderr, args) }

// piiRulesRun is `pii-rules init | check [path]`.
func piiRulesRun(w, errw io.Writer, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(errw, "usage: claude-tools pii-rules init | check [path]")
		return 2
	}
	switch args[0] {
	case "init":
		p := piiRulesPath()
		if p == "" {
			fmt.Fprintln(errw, "pii-rules init: cannot work out the config directory")
			return 1
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			fmt.Fprintln(errw, "pii-rules init:", err)
			return 1
		}
		f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if errors.Is(err, fs.ErrExist) {
			fmt.Fprintf(errw, "pii-rules init: %s exists; not overwriting\n", p)
			return 1
		}
		if err == nil {
			_, err = f.Write(piiColumnsJSON)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
		}
		if err != nil {
			fmt.Fprintln(errw, "pii-rules init:", err)
			return 1
		}
		fmt.Fprintf(w, "wrote %s (embedded rules; edit it to change masking)\n", p)
		return 0
	case "check":
		p := piiRulesPath()
		if len(args) > 1 {
			p = args[1]
		}
		if p == "" {
			fmt.Fprintln(errw, "pii-rules check: no path")
			return 1
		}
		data, err := os.ReadFile(p)
		if err != nil {
			fmt.Fprintln(errw, "pii-rules check:", err)
			return 1
		}
		rs, total, err := validatePIIRules(data)
		if err != nil {
			fmt.Fprintf(errw, "invalid %s: %v\n", p, err)
			return 1
		}
		fmt.Fprintf(w, "ok %s: %d rules active (%d in file), %d explicit columns\n", p, len(rs.rules), total, len(rs.explicit))
		return 0
	}
	fmt.Fprintf(errw, "pii-rules: unknown verb %q (init | check [path])\n", args[0])
	return 2
}
