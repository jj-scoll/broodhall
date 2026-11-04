package main

// team-init: prints a settings block that gives a team the broodhall marketplace and plugins.
//
//	guardrails team-init                     block for .claude/settings.json (or ~/.claude/settings.json)
//	guardrails team-init -managed            block for managed-settings.json, org-wide
//	guardrails team-init -managed -domains @acme.com   ... with the org's email domains
//
// stdout is only the JSON, so it can be redirected into a file. Where to paste it goes to stderr.
// The keys and their shapes are from the Claude Code docs:
//
//	extraKnownMarketplaces  https://code.claude.com/docs/en/settings-reference#extraknownmarketplaces
//	enabledPlugins          https://code.claude.com/docs/en/settings-reference#enabledplugins
//	pluginConfigs           https://code.claude.com/docs/en/settings-reference#pluginconfigs (user or managed only)
//	managed-settings paths  https://code.claude.com/docs/en/managed-settings#deploy-a-managed-settings-file

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
)

const (
	broodhallMarketplace = "broodhall"
	broodhallRepo        = "jj-scoll/broodhall"
)

// marketplacePlugins are the plugin names broodhall's marketplace.json lists.
// TestTeamInitMatchesMarketplaceFile keeps this list true to that file.
var marketplacePlugins = map[string]bool{"guardrails": true, "memory": true, "context-guard": true, "rtk": true}

func teamInitCmd(args []string) int { return teamInit(os.Stdout, os.Stderr, args) }

func teamInit(out, errw io.Writer, args []string) int {
	fs := flag.NewFlagSet("team-init", flag.ContinueOnError)
	fs.SetOutput(errw)
	managed := fs.Bool("managed", false, "print the block for managed-settings.json (org-wide) instead of project settings")
	plugins := fs.String("plugins", "guardrails,memory,context-guard", "comma-separated broodhall plugins to enable")
	domains := fs.String("domains", "", "your organisation's email domains for the guardrails egress guard (needs -managed)")
	autoUpdate := fs.Bool("auto-update", true, "set autoUpdate on the marketplace so installed plugins update together")
	if fs.Parse(args) != nil {
		return 2
	}

	enabled := map[string]any{}
	for _, p := range strings.Split(*plugins, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if !marketplacePlugins[p] {
			fmt.Fprintf(errw, "team-init: %q is not a plugin of the %s marketplace (guardrails, memory, context-guard, rtk)\n", p, broodhallMarketplace)
			return 2
		}
		enabled[p+"@"+broodhallMarketplace] = true
	}
	if len(enabled) == 0 {
		fmt.Fprintln(errw, "team-init: -plugins names no plugin")
		return 2
	}

	doc := map[string]any{
		"extraKnownMarketplaces": map[string]any{
			broodhallMarketplace: map[string]any{
				"source":     map[string]any{"source": "github", "repo": broodhallRepo},
				"autoUpdate": *autoUpdate,
			},
		},
		"enabledPlugins": enabled,
	}

	if *domains != "" {
		if !*managed {
			fmt.Fprintln(errw, "team-init: -domains writes pluginConfigs, which Claude Code reads only from user and managed settings, never from a project file; add -managed")
			return 2
		}
		list, err := normalizeDomains(*domains)
		if err != nil {
			fmt.Fprintln(errw, "team-init:", err)
			return 2
		}
		doc["pluginConfigs"] = map[string]any{
			"guardrails@" + broodhallMarketplace: map[string]any{
				"options": map[string]any{"allowed_email_domains": list},
			},
		}
	}

	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		fmt.Fprintln(errw, "team-init:", err)
		return 1
	}
	fmt.Fprintf(out, "%s\n", b)

	if *managed {
		fmt.Fprint(errw, "Save this as managed-settings.json in the system directory:\n"+
			"  macOS:        /Library/Application Support/ClaudeCode/managed-settings.json\n"+
			"  Linux or WSL: /etc/claude-code/managed-settings.json\n"+
			"  Windows:      C:\\Program Files\\ClaudeCode\\managed-settings.json\n"+
			"Or put it in managed-settings.d/ beside it, as a numbered file such as 20-broodhall.json.\n"+
			"A plugin set to true here is force-enabled and users cannot turn it off.\n")
	} else {
		fmt.Fprint(errw, "Merge this into .claude/settings.json and commit it. Each teammate accepts the folder trust prompt once,\n"+
			"then Claude Code registers the marketplace and installs the plugins that live in this repository. For your own machine, merge it into ~/.claude/settings.json.\n")
	}
	if enabled["rtk@"+broodhallMarketplace] != nil {
		fmt.Fprint(errw, "rtk comes from another repository, and Claude Code does not install such a plugin from settings alone.\n"+
			"Each person runs once: claude plugin install rtk@broodhall\n")
	}
	return 0
}

var domainRe = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?)+$`)

// normalizeDomains turns a comma-separated list of domains and addresses into the form the
// allowed_email_domains option takes: "@domain" for a domain, a full address as is.
func normalizeDomains(s string) (string, error) {
	var out []string
	seen := map[string]bool{}
	for _, d := range strings.Split(s, ",") {
		d = strings.TrimSpace(d)
		if d == "" {
			continue
		}
		local, dom, hasAt := strings.Cut(d, "@")
		switch {
		case !hasAt:
			dom, local = d, ""
		case strings.Contains(dom, "@"):
			return "", fmt.Errorf("%q is not an email address or domain", d)
		}
		if !domainRe.MatchString(dom) || (hasAt && strings.ContainsAny(local, " \t")) {
			return "", fmt.Errorf("%q is not an email address or domain", d)
		}
		d = local + "@" + dom
		if !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	if len(out) == 0 {
		return "", fmt.Errorf("-domains names no domain")
	}
	return strings.Join(out, ","), nil
}
