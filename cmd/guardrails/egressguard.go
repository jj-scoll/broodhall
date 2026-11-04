package main

// Egress payload scan. A command that sends data off the machine (gh writes and api calls,
// curl, wget, aws s3 uploads) asks when what it sends holds an outside email address, a
// phone number or a secret: in the command text (heredoc bodies included) or in a local
// file it uploads. Data the guard can't read asks too: stdin fed by a pipe, a directory, a
// file over egressMaxFile, or a file that doesn't exist yet, unless the same command writes
// it from a heredoc, echo or printf (whose text is part of the scanned command).
//
// It replaced "session taint -> ask on egress": over 2026-08-19..10-28 that gate would have
// asked 532 times in 15 sessions, nearly all routine gh writes and pushes. The same history
// puts this scan at about 30 asks in 2,865 egress calls (20 inline addresses, 21 of them at
// the user's own domain and so allowed here; 16 uploads of files made by another command; 10 piped stdin).
//
// Limits: Bash only (MCP write tools need their own PreToolUse matcher); git push is not
// scanned (it sends commits, reviewed as diffs); aws s3api and sends hidden in bash -c or a
// script are not parsed; only emails and phone numbers count as PII here.

import (
	"os"
	"os/exec"
	"path"
	"regexp"
	"slices"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// egressOwn returns the allowed addresses (piiAllow: git user.email plus GUARDRAILS_PII_ALLOW,
// where an "@domain" entry allows that whole domain) and the git user.email domain. Tests
// replace it.
var egressOwn = func() ([]string, string) {
	out, _ := exec.Command("git", "config", "--global", "user.email").Output()
	_, domain, _ := strings.Cut(strings.TrimSpace(string(out)), "@")
	return piiAllow(), strings.ToLower(domain)
}

const egressMaxFile = 4 << 20

var egressFallback = regexp.MustCompile(`(?:^|[;&|(\s])(?:gh|curl|wget)\s|aws\s.*\bs3\s+(?:cp|mv|sync)\s`)

// egressSend is one upload source: a resolved local path, or stdin.
type egressSend struct {
	path  string
	stdin bool
}

func egressAsk(rule, why string) decision {
	return decision{Ask: true, Rule: "egress:" + rule,
		Msg: "ASK: this call sends data off the machine and " + why + " Confirm with the user before running it."}
}

func egressRule(env *guardEnv, cmd string) decision {
	f, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(cmd), "")
	if err != nil {
		if egressFallback.MatchString(cmd) {
			return egressScan(cmd, "the command text")
		}
		return decision{}
	}
	// piped holds the statements whose stdin is the previous stage of a pipe.
	piped := map[*syntax.Stmt]bool{}
	syntax.Walk(f, func(n syntax.Node) bool {
		if b, ok := n.(*syntax.BinaryCmd); ok && (b.Op == syntax.Pipe || b.Op == syntax.PipeAll) {
			s := b.Y
			for {
				bb, ok := s.Cmd.(*syntax.BinaryCmd)
				if !ok || bb.Op != syntax.Pipe && bb.Op != syntax.PipeAll {
					break
				}
				s = bb.X
			}
			piped[s] = true
		}
		return true
	})
	// written maps a path the command writes to whether every write came from text it holds.
	written := map[string]bool{}
	// vars holds what the command assigns (S=/tmp/x; export S=...), so $S/body.md resolves;
	// any other expansion stays literal and its path won't be found.
	vars := map[string]string{"HOME": env.Home}
	expand := func(w *syntax.Word) string {
		if w == nil {
			return ""
		}
		return os.Expand(wordText(w), func(k string) string {
			if v, ok := vars[k]; ok {
				return v
			}
			return "${" + k + "}"
		})
	}
	var sends []egressSend
	var ask decision
	egress := false
	dir := env.Cwd
	syntax.Walk(f, func(n syntax.Node) bool {
		st, ok := n.(*syntax.Stmt)
		if !ok {
			return true
		}
		var assigns []*syntax.Assign
		var words []string
		switch c := st.Cmd.(type) {
		case *syntax.CallExpr:
			if len(c.Args) == 0 {
				assigns = c.Assigns
			}
			for _, a := range c.Args {
				words = append(words, expand(a))
			}
		case *syntax.DeclClause:
			assigns = c.Args
		}
		for _, as := range assigns {
			if as.Name != nil && as.Value != nil {
				vars[as.Name.Value] = expand(as.Value)
			}
		}
		words = peelWrappers(words)
		name := ""
		if len(words) > 0 {
			name = path.Base(words[0])
		}
		textIn, fileIn := false, ""
		for _, r := range st.Redirs {
			switch r.Op {
			case syntax.Hdoc, syntax.DashHdoc, syntax.WordHdoc:
				textIn = true
			case syntax.RdrIn:
				fileIn = resolveDir(dir, expand(r.Word), env.Home)
			}
		}
		fromText := textIn && name == "cat" || name == "echo" || name == "printf"
		for _, r := range st.Redirs {
			switch r.Op {
			case syntax.RdrOut, syntax.AppOut, syntax.RdrClob, syntax.AppClob, syntax.RdrAll, syntax.AppAll:
				if r.N == nil || r.N.Value == "1" || r.Op == syntax.RdrAll || r.Op == syntax.AppAll {
					p := resolveDir(dir, expand(r.Word), env.Home)
					if v, seen := written[p]; !seen || v {
						written[p] = fromText
					}
				}
			}
		}
		if name == "cd" && len(words) > 1 {
			dir = resolveDir(dir, words[1], env.Home)
			return true
		}
		if name == "" {
			return true
		}
		ss, ok := egressSends(name, words[1:], dir, env.Home)
		if !ok {
			return true
		}
		egress = true
		for _, s := range ss {
			switch {
			case !s.stdin:
				sends = append(sends, s)
			case piped[st]:
				if !ask.Ask {
					ask = egressAsk("stdin", name+" uploads its stdin, fed by a pipe the guard can't read.")
				}
			case fileIn != "":
				sends = append(sends, egressSend{path: fileIn})
			}
		}
		return true
	})
	if !egress {
		return decision{}
	}
	if ask.Ask {
		return ask
	}
	if d := egressScan(cmd, "the command text"); d.Ask {
		return d
	}
	for _, s := range sends {
		fi, err := os.Stat(s.path)
		switch {
		case err != nil && written[s.path]:
			continue // written from text in this command, which was scanned above
		case err != nil:
			return egressAsk("unscanned", "uploads "+s.path+", which doesn't exist yet and isn't written from text in this command, so the guard can't read it.")
		case fi.IsDir():
			return egressAsk("unscanned", "uploads the directory "+s.path+", which the guard doesn't scan.")
		case !fi.Mode().IsRegular(): // a device or FIFO could block the hook forever
			return egressAsk("unscanned", "uploads "+s.path+", which is not a regular file the guard can scan.")
		case fi.Size() > egressMaxFile:
			return egressAsk("unscanned", "uploads "+s.path+", which is too large for the guard to scan.")
		}
		b, err := os.ReadFile(s.path)
		if err != nil {
			return egressAsk("unscanned", "uploads "+s.path+", which the guard can't read.")
		}
		if d := egressScan(string(b), "the file "+s.path); d.Ask {
			return d
		}
	}
	return decision{}
}

// egressScan asks when text holds a secret, an outside email address or a phone number.
// The reason names the address's domain only, so it carries no PII itself.
func egressScan(text, where string) decision {
	if m := scanSecrets(text); len(m) > 0 {
		return egressAsk("secret", where+" holds a secret ("+m[0].Pattern+" "+m[0].Redacted+").")
	}
	var own []string
	domain, looked := "", false
	for _, e := range emailRE.FindAllString(text, -1) {
		e = strings.ToLower(e)
		local, d, _ := strings.Cut(e, "@")
		// unq.gbl.spaces is a Teams chat-thread id (19:...@unq.gbl.spaces), not a person.
		if strings.Contains(e, "noreply") || local == "git" || d == "example.com" || d == "example.org" || d == "example.net" || d == "unq.gbl.spaces" {
			continue
		}
		if !looked {
			own, domain = egressOwn()
			looked = true
		}
		inDomain := func(dom string) bool { return dom != "" && (d == dom || strings.HasSuffix(d, "."+dom)) }
		if inDomain(domain) || slices.ContainsFunc(own, func(o string) bool {
			o = strings.ToLower(strings.TrimSpace(o))
			if dom, ok := strings.CutPrefix(o, "@"); ok {
				return inDomain(dom)
			}
			return o == e
		}) {
			continue
		}
		return egressAsk("email", where+" holds an outside email address (@"+d+").")
	}
	if phoneRE.MatchString(text) {
		return egressAsk("phone", where+" holds a phone number.")
	}
	return decision{}
}

// egressSends reports whether name with args sends data off the machine, and the local
// files or stdin it uploads.
func egressSends(name string, args []string, dir, home string) ([]egressSend, bool) {
	file := func(p string) egressSend {
		if p == "-" {
			return egressSend{stdin: true}
		}
		return egressSend{path: resolveDir(dir, p, home)}
	}
	var out []egressSend
	switch name {
	case "gh":
		i := 0
		for ; i < len(args) && strings.HasPrefix(args[i], "-"); i++ {
			if args[i] == "-R" || args[i] == "--repo" {
				i++
			}
		}
		if i >= len(args) {
			return nil, false
		}
		sub, verb, rest := args[i], "", args[i+1:]
		if sub == "api" {
			for _, v := range flagValues(rest, "--input") {
				out = append(out, file(v))
			}
			for _, v := range flagValues(rest, "-F", "--field") {
				if _, f, ok := strings.Cut(v, "=@"); ok {
					out = append(out, file(f))
				}
			}
			return out, true
		}
		if len(rest) > 0 {
			verb, rest = rest[0], rest[1:]
		}
		switch {
		case sub == "pr" && slices.Contains([]string{"create", "comment", "edit", "review"}, verb),
			sub == "issue" && slices.Contains([]string{"create", "comment", "edit"}, verb):
			for _, v := range flagValues(rest, "--body-file", "-F") {
				out = append(out, file(v))
			}
		case sub == "release" && slices.Contains([]string{"create", "edit", "upload"}, verb):
			for _, v := range flagValues(rest, "--notes-file", "-F") {
				out = append(out, file(v))
			}
			if verb != "edit" {
				pos := positionals(rest, "-n", "--notes", "-t", "--title", "--target", "--notes-start-tag", "--discussion-category", "-F", "--notes-file", "-R", "--repo")
				for j := 1; j < len(pos); j++ { // pos[0] is the tag
					p, _, _ := strings.Cut(pos[j], "#")
					out = append(out, file(p))
				}
			}
		case sub == "gist" && verb == "create":
			for _, p := range positionals(rest, "-d", "--desc", "-f", "--filename") {
				out = append(out, file(p))
			}
		case sub == "gist" && verb == "edit":
			for _, v := range flagValues(rest, "-a", "--add") {
				out = append(out, file(v))
			}
		default:
			return nil, false
		}
		return out, true
	case "curl":
		for _, v := range flagValues(args, "-d", "--data", "--data-binary", "--data-ascii", "--data-urlencode", "--json") {
			if f, ok := strings.CutPrefix(v, "@"); ok {
				out = append(out, file(f))
			}
		}
		for _, v := range flagValues(args, "-F", "--form") {
			if i := strings.IndexAny(v, "@<"); i > 0 && v[i-1] == '=' {
				f, _, _ := strings.Cut(v[i+1:], ";")
				out = append(out, file(f))
			}
		}
		for _, v := range flagValues(args, "-T", "--upload-file") {
			if v == "." {
				v = "-"
			}
			out = append(out, file(v))
		}
		return out, true
	case "wget":
		for _, v := range flagValues(args, "--post-file", "--body-file") {
			out = append(out, file(v))
		}
		return out, true
	case "aws":
		i := slices.Index(args, "s3")
		if i < 0 || i+1 >= len(args) || !slices.Contains([]string{"cp", "mv", "sync"}, args[i+1]) {
			return nil, false
		}
		// aws options take a value unless they are one of the switches below.
		var pos []string
		rest := args[i+2:]
		for j := 0; j < len(rest); j++ {
			a := rest[j]
			switch {
			case a == "-" || !strings.HasPrefix(a, "-"):
				pos = append(pos, a)
			case !strings.Contains(a, "=") && !awsS3Switches[a]:
				j++
			}
		}
		if len(pos) >= 2 && strings.HasPrefix(pos[1], "s3://") && !strings.HasPrefix(pos[0], "s3://") {
			return []egressSend{file(pos[0])}, true
		}
		return nil, false
	}
	return nil, false
}

var awsS3Switches = map[string]bool{
	"--recursive": true, "--dryrun": true, "--quiet": true, "--no-progress": true, "--only-show-errors": true,
	"--follow-symlinks": true, "--no-follow-symlinks": true, "--delete": true, "--exact-timestamps": true,
	"--size-only": true, "--no-guess-mime-type": true, "--no-sign-request": true, "--debug": true,
	"--no-verify-ssl": true, "--no-paginate": true, "--force-glacier-transfer": true, "--ignore-glacier-warnings": true,
}

// flagValues returns the values of the named flags: "--x v", "--x=v", and for a one-letter
// flag also "-xv".
func flagValues(args []string, names ...string) []string {
	var out []string
	for j := 0; j < len(args); j++ {
		a := args[j]
		for _, n := range names {
			switch {
			case a == n && j+1 < len(args):
				out = append(out, args[j+1])
				j++
			case strings.HasPrefix(a, n+"="):
				out = append(out, a[len(n)+1:])
			case len(n) == 2 && len(a) > 2 && strings.HasPrefix(a, n) && a[1] != '-':
				out = append(out, a[2:])
			default:
				continue
			}
			break
		}
	}
	return out
}

// positionals returns the non-flag arguments, skipping the values of valueFlags; "-" counts
// as a positional (stdin).
func positionals(args []string, valueFlags ...string) []string {
	var out []string
	for j := 0; j < len(args); j++ {
		a := args[j]
		switch {
		case a == "-" || !strings.HasPrefix(a, "-"):
			out = append(out, a)
		case slices.Contains(valueFlags, a):
			j++
		}
	}
	return out
}

// egressToolVerb picks the MCP tools that send or store data elsewhere by their name's last part.
var egressToolVerb = regexp.MustCompile(`(?i)send|create|update|add|reply|forward|upload|comment|post|edit|transition|batch|draft`)

// egressToolDecision scans an MCP write tool's input: every string in it (nested included),
// and any existing regular local file named under a key holding "path" or "file". ok=false
// when the rule doesn't apply.
func egressToolDecision(tool string, input map[string]any, cwd string) (decision, bool) {
	i := strings.LastIndex(tool, "__")
	if !strings.HasPrefix(tool, "mcp__") || i < 0 || !egressToolVerb.MatchString(tool[i+2:]) {
		return decision{}, false
	}
	var texts, files []string
	var walk func(key string, v any)
	walk = func(key string, v any) {
		switch v := v.(type) {
		case string:
			texts = append(texts, v)
			if k := strings.ToLower(key); strings.Contains(k, "path") || strings.Contains(k, "file") {
				files = append(files, resolveDir(cwd, v, ""))
			}
		case []any:
			for _, e := range v {
				walk(key, e)
			}
		case map[string]any:
			for k, e := range v {
				walk(k, e)
			}
		}
	}
	walk("", input)
	if d := egressScan(strings.Join(texts, "\n"), "the "+tool+" input"); d.Ask {
		return d, true
	}
	for _, p := range files {
		if fi, err := os.Stat(p); err != nil || !fi.Mode().IsRegular() || fi.Size() > egressMaxFile {
			continue // not a local file (a remote path, a name) or one this rule doesn't read
		}
		if b, err := os.ReadFile(p); err == nil {
			if d := egressScan(string(b), "the file "+p); d.Ask {
				return d, true
			}
		}
	}
	return decision{}, true
}
