package main

// setup: writes the guardrails plugin options to the user's settings.json, the file Claude Code
// reads pluginConfigs from (user and managed settings only; a project file is ignored).
//
//	guardrails setup -profile standard -domains acme.com        show what it would write
//	guardrails setup -profile standard -domains acme.com -write write it
//
// It is what `/guardrails:setup` runs. It only touches the options it is given, keeps every
// other key in the file, shows old and new values first, writes nothing when nothing changes,
// and keeps a copy of the file it replaced. Profiles are presets of existing plugin options:
//
//	standard  every guard family on, observe off
//	observe   every guard family on, observe on (log what would be asked or blocked)
//
// A profile never turns a guard family off. config-gate refuses that change mid-session, and
// loosening the guards belongs to /plugin configure, where you choose it deliberately.
// pluginConfigs shape: https://code.claude.com/docs/en/settings-reference#pluginconfigs

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

func setupCmd(args []string) int { return setupRun(os.Stdout, os.Stderr, args) }

// setupOptionOrder lists the options setup manages, in display order.
var setupOptionOrder = []string{"disabled_guards", "observe", "allowed_email_domains"}

func setupRun(out, errw io.Writer, args []string) int {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	fs.SetOutput(errw)
	profile := fs.String("profile", "", "standard or observe (empty: leave the profile options as they are)")
	domains := fs.String("domains", "", "your organisation's email domains or addresses, comma-separated (empty: leave as is)")
	market := fs.String("marketplace", "", "marketplace name (default: the one enabledPlugins names for guardrails, else broodhall)")
	cfg := fs.String("config-dir", "", "Claude config directory (default: CLAUDE_CONFIG_DIR, else ~/.claude)")
	write := fs.Bool("write", false, "write the change; without it setup only shows it")
	if fs.Parse(args) != nil {
		return 2
	}

	want := map[string]any{}
	switch *profile {
	case "":
	case "standard":
		want["disabled_guards"], want["observe"] = "", false
	case "observe":
		want["disabled_guards"], want["observe"] = "", true
	default:
		fmt.Fprintf(errw, "setup: unknown profile %q (standard or observe)\n", *profile)
		return 2
	}
	if *domains != "" {
		list, err := normalizeDomains(*domains)
		if err != nil {
			fmt.Fprintln(errw, "setup:", err)
			return 2
		}
		want["allowed_email_domains"] = list
	}
	if *market != "" && (!marketplaceNameRE.MatchString(*market) || strings.Contains(*market, "..")) {
		fmt.Fprintf(errw, "setup: marketplace %q must be a bare name such as broodhall (letters, digits, dot, dash, underscore)\n", *market)
		return 2
	}

	dir := *cfg
	if dir == "" {
		dir = claudeConfigDir()
	}
	path := filepath.Join(dir, "settings.json")
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		r, err := filepath.EvalSymlinks(path)
		if err != nil {
			fmt.Fprintf(errw, "setup: %s is a symlink to a file that does not exist, so setup leaves it alone: %v\n", path, err)
			return 1
		}
		path = r // write through a symlinked settings.json, keep the link
	}

	raw, err := os.ReadFile(path)
	exists := err == nil
	if err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(errw, "setup: cannot read %s: %v\n", path, err)
		return 1
	}
	doc := map[string]any{}
	if exists && len(bytes.TrimSpace(raw)) > 0 {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		var top any
		if err := dec.Decode(&top); err != nil {
			fmt.Fprintf(errw, "setup: %s is not valid JSON, so setup leaves it alone: %v\n", path, err)
			return 1
		}
		if _, err := dec.Token(); err != io.EOF {
			fmt.Fprintf(errw, "setup: %s has content after its JSON value, so setup leaves it alone\n", path)
			return 1
		}
		obj, ok := top.(map[string]any)
		if !ok {
			fmt.Fprintf(errw, "setup: %s does not hold a JSON object, so setup leaves it alone\n", path)
			return 1
		}
		doc = obj
	}

	id := "guardrails@" + *market
	if *market == "" {
		id = "guardrails@" + broodhallMarketplace
		if ep, ok := doc["enabledPlugins"].(map[string]any); ok {
			keys := make([]string, 0, len(ep))
			for k, v := range ep {
				if strings.HasPrefix(k, guardsPlugin) && v != false {
					keys = append(keys, k)
				}
			}
			sort.Strings(keys)
			if len(keys) > 0 {
				id = keys[0]
			}
		}
	}

	pcAny, hasPC := doc["pluginConfigs"]
	pc, _ := pcAny.(map[string]any)
	if hasPC && pc == nil {
		fmt.Fprintf(errw, "setup: pluginConfigs in %s is not an object; fix it by hand first\n", path)
		return 1
	}
	if pc == nil {
		pc = map[string]any{}
	}
	entry, _ := pc[id].(map[string]any)
	if v, has := pc[id]; has && entry == nil {
		fmt.Fprintf(errw, "setup: pluginConfigs[%q] is %v, not an object; fix it by hand first\n", id, v)
		return 1
	}
	if entry == nil {
		entry = map[string]any{}
	}
	opts, _ := entry["options"].(map[string]any)
	if v, has := entry["options"]; has && opts == nil {
		fmt.Fprintf(errw, "setup: pluginConfigs[%q].options is %v, not an object; fix it by hand first\n", id, v)
		return 1
	}
	if opts == nil {
		opts = map[string]any{}
	}

	state := "will be created"
	if exists {
		state = "exists, other keys stay as they are"
	}
	fmt.Fprintf(out, "settings file: %s (%s)\nplugin options for %s:\n", path, state, id)
	changed := false
	for _, k := range setupOptionOrder {
		nv, set := want[k]
		ov, had := opts[k]
		switch {
		case !set && !had:
			continue
		case !set:
			fmt.Fprintf(out, "  %-22s %s (not touched)\n", k, jsonStr(ov))
		case had && jsonStr(ov) == jsonStr(nv):
			fmt.Fprintf(out, "  %-22s %s (no change)\n", k, jsonStr(ov))
		default:
			old := "(unset)"
			if had {
				old = jsonStr(ov)
			}
			fmt.Fprintf(out, "  %-22s %s -> %s\n", k, old, jsonStr(nv))
			opts[k] = nv
			changed = true
		}
	}
	if !changed {
		fmt.Fprintln(out, "already up to date: nothing to write.")
		return 0
	}
	if !*write {
		fmt.Fprintln(out, "dry run: nothing written. Run again with -write to apply this.")
		return 0
	}

	entry["options"] = opts
	pc[id] = entry
	doc["pluginConfigs"] = pc
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		fmt.Fprintln(errw, "setup:", err)
		return 1
	}
	if exists {
		if fi, err := os.Stat(path); err == nil && fi.Mode().Perm()&0o200 == 0 {
			fmt.Fprintf(errw, "setup: %s is read-only, so nothing was written; make it writable first\n", path)
			return 1
		}
		if err := writeBackup(path+".guardrails-setup.bak", raw); err != nil {
			fmt.Fprintf(errw, "setup: cannot back up %s, so nothing was written: %v\n", path, err)
			return 1
		}
	}
	if err := writeFileAtomic(path, buf.Bytes()); err != nil {
		fmt.Fprintf(errw, "setup: cannot write %s: %v\n", path, err)
		return 1
	}
	fmt.Fprintf(out, "wrote %s", path)
	if exists {
		fmt.Fprintf(out, " (previous contents kept in %s)", path+".guardrails-setup.bak")
	}
	fmt.Fprintln(out, "\nClaude Code reloads settings on change; start a new session if an option does not seem to apply.")
	return 0
}

var marketplaceNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// writeBackup replaces the backup file with data. It removes what is there first and creates
// the new file exclusively, so a symlink left at that name is replaced, never written through.
func writeBackup(path string, data []byte) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func jsonStr(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

// writeFileAtomic replaces path with data through a temp file in the same directory, keeping the
// mode of the file it replaces (0600 for a new one: settings can hold credentials).
func writeFileAtomic(path string, data []byte) error {
	mode := os.FileMode(0o600)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".settings-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
