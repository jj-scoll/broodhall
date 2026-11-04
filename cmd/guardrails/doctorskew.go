package main

// doctor's version checks. The three broodhall plugins ship from one release tag, so their
// installed versions should match each other and the marketplace checkout. They drift when one
// plugin updates and another does not (auto-update off, a failed update, a pinned scope).
//
// Claude Code records installs in <config>/plugins/installed_plugins.json: a "plugins" map from
// "<name>@<marketplace>" to the install records, each with a scope, installPath and version.
// The docs describe the fields but not the exact nesting, so the reader takes a list of records
// or a single record per id, and ignores any record without a version. See
// https://code.claude.com/docs/en/plugins/loading (Check which stage a plugin reached).

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

type installedPlugin struct {
	Name, Marketplace, Version, Scope string
}

// broodhallPlugins are the plugins that share this repository's version. rtk lives in another
// repository and has its own.
var broodhallPlugins = map[string]bool{"guardrails": true, "memory": true, "context-guard": true}

// readInstalledPlugins returns every install record that carries a version. A missing file is
// not an error (nothing installed yet); a file that does not parse is.
func readInstalledPlugins(configDir string) ([]installedPlugin, error) {
	b, err := os.ReadFile(filepath.Join(configDir, "plugins", "installed_plugins.json"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var doc struct {
		Plugins map[string]json.RawMessage `json:"plugins"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("installed_plugins.json: %w", err)
	}
	type rec struct {
		Scope   string `json:"scope"`
		Version string `json:"version"`
	}
	var out []installedPlugin
	ids := make([]string, 0, len(doc.Plugins))
	for id := range doc.Plugins {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		name, mkt, ok := strings.Cut(id, "@")
		if !ok || name == "" || mkt == "" {
			continue
		}
		var recs []rec
		if json.Unmarshal(doc.Plugins[id], &recs) != nil {
			var one rec
			if json.Unmarshal(doc.Plugins[id], &one) != nil {
				continue
			}
			recs = []rec{one}
		}
		for _, r := range recs {
			if r.Version != "" {
				out = append(out, installedPlugin{Name: name, Marketplace: mkt, Version: r.Version, Scope: r.Scope})
			}
		}
	}
	return out, nil
}

// broodhallMarketplaces returns the marketplace names that serve broodhall: the default name,
// plus any marketplace whose recorded source is the broodhall repository (a renamed copy).
func broodhallMarketplaces(configDir string) map[string]bool {
	out := map[string]bool{"broodhall": true}
	b, err := os.ReadFile(filepath.Join(configDir, "plugins", "known_marketplaces.json"))
	if err != nil {
		return out
	}
	var known map[string]struct {
		Source struct {
			Repo string `json:"repo"`
			URL  string `json:"url"`
		} `json:"source"`
	}
	if json.Unmarshal(b, &known) != nil {
		return out
	}
	for name, k := range known {
		if k.Source.Repo == "jj-scoll/broodhall" || strings.Contains(k.Source.URL, "jj-scoll/broodhall") {
			out[name] = true
		}
	}
	return out
}

// broodhallInstalls returns the installed broodhall plugins, grouped by marketplace.
func broodhallInstalls(configDir string) map[string][]installedPlugin {
	all, err := readInstalledPlugins(configDir)
	if err != nil || len(all) == 0 {
		return nil
	}
	mkts := broodhallMarketplaces(configDir)
	out := map[string][]installedPlugin{}
	for _, p := range all {
		if broodhallPlugins[p.Name] && mkts[p.Marketplace] {
			out[p.Marketplace] = append(out[p.Marketplace], p)
		}
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

var semverRe = regexp.MustCompile(`^(\d+)\.(\d+)\.(\d+)$`)

// versionLess orders X.Y.Z numerically; anything else sorts lexically.
func versionLess(a, b string) bool {
	ma, mb := semverRe.FindStringSubmatch(a), semverRe.FindStringSubmatch(b)
	if ma == nil || mb == nil {
		return a < b
	}
	for i := 1; i <= 3; i++ {
		x, _ := strconv.Atoi(ma[i])
		y, _ := strconv.Atoi(mb[i])
		if x != y {
			return x < y
		}
	}
	return false
}

// checkPluginSkew reports each marketplace whose installed broodhall plugins are not all at
// one version. Plugins behind the newest are the ones to update.
func checkPluginSkew(configDir string) []string {
	var out []string
	groups := broodhallInstalls(configDir)
	for _, mkt := range sortedKeys(groups) {
		versions := map[string]bool{}
		newest := ""
		for _, p := range groups[mkt] {
			versions[p.Version] = true
			if newest == "" || versionLess(newest, p.Version) {
				newest = p.Version
			}
		}
		if len(versions) < 2 {
			continue
		}
		var have, update []string
		seen := map[string]bool{}
		for _, p := range groups[mkt] {
			have = append(have, p.Name+" "+p.Version)
			if p.Version != newest && !seen[p.Name] {
				seen[p.Name] = true
				update = append(update, "claude plugin update "+p.Name+"@"+mkt)
			}
		}
		out = append(out, fmt.Sprintf("broodhall plugins from marketplace %q are at mismatched versions (%s): they ship from one release, so a hook from one can expect behavior the other lacks; run %s, then /reload-plugins",
			mkt, strings.Join(have, ", "), strings.Join(update, " and ")))
	}
	return out
}

// marketplaceVersion reads the version the marketplace checkout offers for plugin name: the
// plugin's own plugin.json first (it wins over the entry, as Claude Code resolves versions),
// then the entry's version. A plugin whose source is not a relative path has no files here.
// ok is false when nothing could be read.
func marketplaceVersion(configDir, mkt, name string) (version string, ok bool) {
	loc := filepath.Join(configDir, "plugins", "marketplaces", mkt)
	if b, err := os.ReadFile(filepath.Join(configDir, "plugins", "known_marketplaces.json")); err == nil {
		var known map[string]struct {
			InstallLocation string `json:"installLocation"`
		}
		if json.Unmarshal(b, &known) == nil && known[mkt].InstallLocation != "" {
			loc = known[mkt].InstallLocation
		}
	}
	b, err := os.ReadFile(filepath.Join(loc, ".claude-plugin", "marketplace.json"))
	if err != nil {
		return "", false
	}
	var m struct {
		Plugins []struct {
			Name    string          `json:"name"`
			Source  json.RawMessage `json:"source"`
			Version string          `json:"version"`
		} `json:"plugins"`
	}
	if json.Unmarshal(b, &m) != nil {
		return "", false
	}
	for _, p := range m.Plugins {
		if p.Name != name {
			continue
		}
		var rel string
		if json.Unmarshal(p.Source, &rel) != nil {
			return "", false // an object source: the files are not in the checkout
		}
		if pj := pluginVersion(filepath.Join(loc, filepath.FromSlash(rel))); pj != "" {
			return pj, true
		}
		return p.Version, p.Version != ""
	}
	return "", false
}

// checkMarketplaceVersions reports each installed broodhall plugin whose version differs from
// the one the marketplace checkout on disk offers. The checkout is what `claude plugin update`
// installs from. A ref or sha pinned in settings is not read: only the checkout is.
func checkMarketplaceVersions(configDir string) []string {
	var out []string
	groups := broodhallInstalls(configDir)
	for _, mkt := range sortedKeys(groups) {
		seen := map[string]bool{}
		for _, p := range groups[mkt] {
			key := p.Name + " " + p.Version
			if seen[key] {
				continue
			}
			seen[key] = true
			want, ok := marketplaceVersion(configDir, mkt, p.Name)
			if !ok || want == p.Version {
				continue
			}
			out = append(out, fmt.Sprintf("%s@%s is installed at %s but the marketplace checkout offers %s: run claude plugin update %s@%s, then /reload-plugins", p.Name, mkt, p.Version, want, p.Name, mkt))
		}
	}
	return out
}

// oldBinaryRe matches claude-tools as the last element of a path or a bare command word. A
// directory named claude-tools (~/.config/claude-tools/...) is followed by a slash and does not match.
var oldBinaryRe = regexp.MustCompile(`(^|[\s/'"])claude-tools($|[\s'"])`)

// checkStatusLine returns a problem when the settings' statusLine command still runs the binary
// under its old name, claude-tools. Plugins from the rename on install guardrails, so that name
// stops updating.
func checkStatusLine(files []string) string {
	for _, s := range readSettings(files) {
		if cmd := s.statusLineCommand(); oldBinaryRe.MatchString(cmd) {
			return fmt.Sprintf("statusLine command %q still runs claude-tools, the binary's name before it became guardrails: it stops getting updates. Use the bin/*/guardrails command from docs/guardrails/statusline.md (https://jj-scoll.github.io/broodhall/guardrails/statusline/)", cmd)
		}
	}
	return ""
}
