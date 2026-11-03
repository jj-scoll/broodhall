package main

// The smart_* tools read source files for the model, so they refuse the files that hold
// credentials. The protected files are secretPathsDefault plus the lines of
// <config dir>/memory/secret-paths (a path or glob per line, ~ allowed, # comments). The
// list adds to the defaults and is protected itself.

import (
	"os"
	"path/filepath"
	"strings"
)

var secretPathsDefault = []string{
	"~/.claude/.credentials.json",
	"~/.aws/credentials", "~/.aws/sso/cache/*.json", "~/.aws/cli/cache/*.json",
	"~/.ssh/id_rsa", "~/.ssh/id_ecdsa", "~/.ssh/id_ed25519", "~/.ssh/id_ecdsa_sk", "~/.ssh/id_ed25519_sk",
	"~/.netrc", "~/.pgpass", "~/.docker/config.json", "~/.config/gh/hosts.yml",
}

const secretGlobChr = "*?["

// secretListPath follows the XDG rule: a relative XDG_CONFIG_HOME is ignored.
func secretListPath(xdg, home string) string {
	switch {
	case filepath.IsAbs(xdg):
		return filepath.Join(xdg, "memory", "secret-paths")
	case home != "":
		return filepath.Join(home, ".config", "memory", "secret-paths")
	}
	return ""
}

// secretPaths is every protected path pattern, absolute and cleaned.
func secretPaths(home, xdg string) []string {
	ps := append([]string{}, secretPathsDefault...)
	if list := secretListPath(xdg, home); list != "" {
		ps = append(ps, list)
		if b, err := os.ReadFile(list); err == nil {
			for _, l := range strings.Split(string(b), "\n") {
				if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
					ps = append(ps, l)
				}
			}
		}
	}
	var out []string
	for _, p := range ps {
		if p = filepath.Clean(expandHome(p, home)); filepath.IsAbs(p) { // a relative line names no one file
			out = append(out, p)
		}
	}
	return out
}

// resolveForCompare makes p absolute and follows symlinks as far as the path exists.
func resolveForCompare(p, cwd string) string {
	if !filepath.IsAbs(p) {
		p = filepath.Join(cwd, p)
	}
	p = filepath.Clean(p)
	rest := ""
	for q := p; ; q = filepath.Dir(q) {
		if r, err := filepath.EvalSymlinks(q); err == nil {
			return filepath.Join(r, rest)
		}
		rest = filepath.Join(filepath.Base(q), rest)
		if filepath.Dir(q) == q {
			return p
		}
	}
}

func expandHome(p, home string) string {
	switch {
	case p == "~":
		return home
	case strings.HasPrefix(p, "~/"):
		return filepath.Join(home, p[2:])
	}
	return p
}

type secretGuard struct {
	home, cwd string
	paths     []string // as written, then with symlinks resolved
}

func newSecretGuard(home, xdg, cwd string) *secretGuard {
	g := &secretGuard{home: home, cwd: cwd}
	ps := secretPaths(home, xdg)
	for _, p := range ps {
		dir, base := filepath.Split(p)
		g.paths = append(g.paths, filepath.Join(resolveForCompare(dir, "/"), base))
	}
	g.paths = append(ps, g.paths...)
	return g
}

// files is what a path word may name: ~ and $HOME expanded, made absolute, globs expanded
// (dot files only for a dotted pattern, as the shell does). Nil when it holds other
// expansions.
func (g *secretGuard) files(w string) []string {
	for _, v := range []string{"${HOME}", "$HOME"} {
		if strings.HasPrefix(w, v) {
			w = g.home + w[len(v):]
		}
	}
	w = expandHome(w, g.home)
	if w == "" || strings.ContainsAny(w, "$`") {
		return nil
	}
	if !filepath.IsAbs(w) {
		w = filepath.Join(g.cwd, w)
	}
	w = filepath.Clean(w)
	if !strings.ContainsAny(w, secretGlobChr) {
		return []string{w}
	}
	ms, _ := filepath.Glob(w)
	out := []string{w}
	for _, m := range ms {
		if !strings.HasPrefix(filepath.Base(m), ".") || strings.HasPrefix(filepath.Base(w), ".") {
			out = append(out, m)
		}
	}
	return out
}

// protected reports whether word names a protected file; under reports whether it names a
// directory holding one.
func (g *secretGuard) protected(word string) (file, under bool) {
	for _, f := range g.files(word) {
		for _, c := range []string{f, resolveForCompare(f, "/")} {
			for _, p := range g.paths {
				ok, _ := filepath.Match(p, c)
				file = file || ok
				under = under || strings.HasPrefix(p, strings.TrimSuffix(c, "/")+"/")
			}
		}
	}
	return file, under
}
