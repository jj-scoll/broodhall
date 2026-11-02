package main

// rmRootTarget replaces the substring rule "rm -rf /": the old test called any path that
// starts with / the filesystem root, so every scratchpad cleanup was blocked (45 of 45 hits
// in the Oct 2026 history were subpaths, none the root). A command now blocks only when a
// recursive+force rm names exactly /, ~, $HOME, the user's home directory, /home, /nix or
// /storage (a trailing / or /* counts as the same target).

import (
	"path"
	"regexp"
	"strings"
)

var rmProtected = map[string]bool{"/home": true, "/nix": true, "/storage": true}

// shellSegments splits cmd into simple commands (words), cutting at ; & | ( ) newline and
// backtick outside quotes. ok is false for an unterminated quote.
func shellSegments(cmd string) (segs [][]string, ok bool) {
	var cur []string
	var word strings.Builder
	inWord := false
	endWord := func() {
		if inWord {
			cur = append(cur, word.String())
			word.Reset()
			inWord = false
		}
	}
	endSeg := func() {
		endWord()
		if len(cur) > 0 {
			segs = append(segs, cur)
			cur = nil
		}
	}
	rs := []rune(cmd)
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		switch {
		case c == '\'':
			inWord = true
			j := i + 1
			for j < len(rs) && rs[j] != '\'' {
				word.WriteRune(rs[j])
				j++
			}
			if j == len(rs) {
				return nil, false
			}
			i = j
		case c == '"':
			inWord = true
			j := i + 1
			for j < len(rs) && rs[j] != '"' {
				if rs[j] == '\\' && j+1 < len(rs) && strings.ContainsRune("\"\\$`", rs[j+1]) {
					j++
				}
				word.WriteRune(rs[j])
				j++
			}
			if j == len(rs) {
				return nil, false
			}
			i = j
		case c == '\\':
			if i+1 < len(rs) {
				i++
				if rs[i] != '\n' {
					inWord = true
					word.WriteRune(rs[i])
				}
			}
		case c == ' ' || c == '\t':
			endWord()
		case strings.ContainsRune(";&|()\n`", c):
			endSeg()
		default:
			inWord = true
			word.WriteRune(c)
		}
	}
	endSeg()
	return segs, true
}

// rootName canonicalises a protected rm target, or returns "".
func rootName(t, home string) string {
	if t == "" {
		return ""
	}
	trimmed := strings.TrimRight(strings.TrimSuffix(t, "*"), "/")
	switch {
	case trimmed == "" && strings.HasPrefix(t, "/"):
		return "/"
	case trimmed == "~":
		return "~"
	case trimmed == "$HOME" || trimmed == "${HOME}":
		return "$HOME"
	case home != "" && trimmed == strings.TrimRight(home, "/"):
		return "~"
	case rmProtected[trimmed]:
		return trimmed
	}
	return ""
}

var (
	heredocMarker = regexp.MustCompile(`<<(-?)\s*['"]?([A-Za-z_][A-Za-z0-9_]*)['"]?`)
	shellReader   = regexp.MustCompile(`(?:^|[\s;&|(])(?:bash|sh|zsh|dash|ksh|ssh|eval|source)(?:\s|$)`)
)

// stripHeredocs drops heredoc bodies, which are data (a python script, a file being
// written), unless the command line hands them to a shell, where they are commands.
func stripHeredocs(cmd string) string {
	lines := strings.Split(cmd, "\n")
	var out []string
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		out = append(out, line)
		type term struct {
			word string
			dash bool
		}
		var terms []term
		for _, m := range heredocMarker.FindAllStringSubmatchIndex(line, -1) {
			if m[0] > 0 && line[m[0]-1] == '<' { // here-string <<<
				continue
			}
			terms = append(terms, term{line[m[4]:m[5]], m[3] > m[2]})
		}
		keep := shellReader.MatchString(line)
		for _, t := range terms {
			for i++; i < len(lines); i++ {
				l := lines[i]
				if t.dash {
					l = strings.TrimLeft(l, "\t")
				}
				if l == t.word {
					break
				}
				if keep {
					out = append(out, lines[i])
				}
			}
		}
	}
	return strings.Join(out, "\n")
}

// shellFind runs match on every simple command in cmd (see parseShell) and returns the first
// non-empty result. Input that does not parse goes to fallback instead, so a rule stays as
// strict as its old substring test.
func shellFind(cmd string, match func(w []string) string, fallback func(cmd string) string) string {
	cmds, ok := parseShell(cmd)
	if !ok {
		return fallback(cmd)
	}
	for _, c := range cmds {
		if len(c.Words) == 0 {
			continue
		}
		if r := match(c.Words); r != "" {
			return r
		}
	}
	return ""
}

// rmRootTarget reports the first protected target of a recursive+force rm in cmd.
func rmRootTarget(cmd, home string) (string, bool) {
	t := shellFind(cmd, func(w []string) string {
		if path.Base(w[0]) == "rm" {
			return rmArgsTarget(w[1:], home)
		}
		return ""
	}, func(cmd string) string {
		for _, p := range []string{"rm -rf /", "rm -rf ~", "rm -rf $HOME"} {
			if strings.Contains(cmd, p) {
				return strings.TrimPrefix(p, "rm -rf ")
			}
		}
		return ""
	})
	return t, t != ""
}

// wrapperArgFlags are the sudo flags that take a separate argument.
var wrapperArgFlags = map[string]bool{"-u": true, "-g": true, "-h": true, "-p": true, "-C": true, "-D": true, "-R": true, "-T": true, "-U": true}

// peelWrappers drops leading assignments and wrappers (sudo -u x, env A=b, command, nohup,
// ...) so w[0] is the command that actually runs.
func peelWrappers(w []string) []string {
	for len(w) > 0 {
		switch {
		case strings.Contains(w[0], "=") && !strings.HasPrefix(w[0], "-") && !strings.Contains(w[0], "/"):
			w = w[1:]
		case path.Base(w[0]) == "sudo" || path.Base(w[0]) == "doas" || w[0] == "env" || w[0] == "command" || w[0] == "nohup" || w[0] == "exec" || w[0] == "time":
			isSudo := path.Base(w[0]) == "sudo"
			w = w[1:]
			for len(w) > 0 && strings.HasPrefix(w[0], "-") && w[0] != "-" {
				skip := 1
				if isSudo && wrapperArgFlags[w[0]] {
					skip = 2
				}
				if skip > len(w) {
					skip = len(w)
				}
				w = w[skip:]
			}
		default:
			return w
		}
	}
	return w
}

func rmArgsTarget(args []string, home string) string {
	var recursive, force, endFlags bool
	var targets []string
	for _, a := range args {
		switch {
		case endFlags || !strings.HasPrefix(a, "-") || a == "-":
			targets = append(targets, a)
		case a == "--":
			endFlags = true
		case a == "--recursive":
			recursive = true
		case a == "--force":
			force = true
		case strings.HasPrefix(a, "--"):
		default:
			for _, c := range a[1:] {
				switch c {
				case 'r', 'R':
					recursive = true
				case 'f':
					force = true
				}
			}
		}
	}
	if !recursive || !force {
		return ""
	}
	for _, t := range targets {
		if n := rootName(t, home); n != "" {
			return n
		}
	}
	return ""
}
