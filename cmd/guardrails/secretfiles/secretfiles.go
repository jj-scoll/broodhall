// Package secretfiles is the list of files that hold credentials, shared by the guardrails
// secret-file guard and the memory daemon's capture rules. It holds only the data and a
// path matcher; the guard's Bash parsing and masking stay in cmd/guardrails.
package secretfiles

import (
	"path/filepath"
	"slices"
	"strings"
)

// Paths is the fixed list of protected paths (~ is the home directory; globs allowed).
// ~/.config/mark.toml is the config of mark (github.com/kovetskiy/mark, Markdown to
// Confluence), which holds an Atlassian API token in plain text.
var Paths = []string{
	"~/.config/mark.toml",
	"~/.aws/credentials", "~/.aws/sso/cache/*.json", "~/.aws/cli/cache/*.json",
	"~/.ssh/id_rsa", "~/.ssh/id_ecdsa", "~/.ssh/id_ed25519", "~/.ssh/id_ecdsa_sk", "~/.ssh/id_ed25519_sk",
	"~/.netrc", "~/.pgpass", "~/.docker/config.json", "~/.config/gh/hosts.yml",
	"~/.local/state/guardrails/pii-key", "~/.local/state/claude-tools/pii-key",
	"~/.kube/config", "~/.pypirc", "~/.git-credentials",
	"~/.config/gcloud/application_default_credentials.json", "~/.config/gcloud/credentials.db",
}

// Names are file names protected in any directory (filepath.Match on the base name), and
// Skip the templates among them that hold no secret.
var (
	Names = []string{".env", ".env.*", ".npmrc", ".pypirc", ".git-credentials", "*.tfvars", "*.pem"}
	Skip  = []string{".env.example", ".env.sample", ".env.template"}
)

// Named reports whether a file's base name is one of Names and not a template in Skip.
func Named(base string) bool {
	if slices.Contains(Skip, base) {
		return false
	}
	return slices.ContainsFunc(Names, func(p string) bool { ok, _ := filepath.Match(p, base); return ok })
}

// Match reports whether the absolute, cleaned path p is a protected file: one of Paths with
// ~ taken as home, a protected name, or .credentials.json right in a ~/.claude* directory.
// It does not resolve symlinks or read the disk, and it does not know the lines a user adds
// to guardrails' secret-paths file.
func Match(p, home string) bool {
	if Named(filepath.Base(p)) {
		return true
	}
	if home == "" {
		return false
	}
	home = filepath.Clean(home)
	for _, pat := range Paths {
		if rest, ok := strings.CutPrefix(pat, "~/"); ok {
			pat = filepath.Join(home, rest)
		}
		if ok, _ := filepath.Match(pat, p); ok {
			return true
		}
	}
	d := filepath.Dir(p)
	return filepath.Base(p) == ".credentials.json" && filepath.Dir(d) == home && strings.HasPrefix(filepath.Base(d), ".claude")
}
