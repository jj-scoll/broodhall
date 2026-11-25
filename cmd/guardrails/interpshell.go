package main

// Shell commands inside an interpreter one-liner: python -c '...os.system("rm -rf ~")' and
// perl -e 'system("sh", "-c", $ARGV[0])' 'rm -rf ~' run a shell command the guard would
// judge if it were typed. interpreterScripts pulls out the literal arguments of the calls
// that run one (os.system, os.popen, subprocess.*, perl system and exec), so parseShell can
// read them like a bash -c string. Only literals count: a string the code builds at run
// time is not found, and printing or otherwise using a string is never a command.

import (
	"regexp"
	"strconv"
	"strings"
)

var (
	pyShellCall   = regexp.MustCompile(`\b(?:os\.(?:system|popen)|subprocess\.(?:run|call|check_call|check_output|Popen|getoutput|getstatusoutput))\s*\(\s*`)
	perlShellCall = regexp.MustCompile(`\b(?:system|exec)\b\s*\(?\s*`)
	perlArgv      = regexp.MustCompile(`^\$ARGV\[(\d)\]`)
)

// interpreterScripts lists the shell scripts a python -c or perl -e one-liner runs.
func interpreterScripts(w []string) []string {
	if len(w) < 3 {
		return nil
	}
	name := runnerName(w[0])
	flag := map[string]string{"python": "-c", "perl": "-e"}[name]
	if flag == "" {
		return nil
	}
	i := 1
	for i < len(w) && w[i] != flag && !(name == "perl" && w[i] == "-E") {
		i++
	}
	if i+1 >= len(w) {
		return nil
	}
	code, argv := w[i+1], w[i+2:]
	var scripts []string
	if name == "python" {
		for _, m := range pyShellCall.FindAllStringIndex(code, -1) {
			if elems, ok := literalList(code[m[1]:], nil); ok {
				scripts = append(scripts, elemsScript(elems))
			}
		}
		return scripts
	}
	for _, m := range perlShellCall.FindAllStringIndex(code, -1) {
		if elems, ok := literalList(code[m[1]:], argv); ok {
			scripts = append(scripts, elemsScript(elems))
		}
	}
	return scripts
}

// elemsScript is the script a call's arguments run: one string is a shell script, sh -c X
// runs X, and any other list is an argv.
func elemsScript(elems []string) string {
	if len(elems) >= 3 && shellNames[runnerName(elems[0])] && elems[1] == "-c" {
		return elems[2]
	}
	if len(elems) == 1 {
		return elems[0]
	}
	q := make([]string, len(elems))
	for i, e := range elems {
		q[i] = "'" + strings.ReplaceAll(e, "'", `'\''`) + "'"
	}
	return strings.Join(q, " ")
}

// literalList reads the call arguments at the start of s: one string literal, a [...] list
// of them (python), or a comma-separated run of literals and $ARGV[n] (perl, with argv).
func literalList(s string, argv []string) ([]string, bool) {
	s = strings.TrimSpace(s)
	list := strings.HasPrefix(s, "[")
	if list {
		s = s[1:]
	}
	var out []string
	for {
		s = strings.TrimSpace(s)
		if m := perlArgv.FindStringSubmatch(s); m != nil && argv != nil {
			n, _ := strconv.Atoi(m[1])
			if n >= len(argv) {
				return nil, false
			}
			out = append(out, argv[n])
			s = s[len(m[0]):]
		} else {
			v, rest, ok := stringLiteral(s)
			if !ok {
				return nil, false
			}
			out = append(out, v)
			s = rest
		}
		s = strings.TrimSpace(s)
		if !strings.HasPrefix(s, ",") || !list && argv == nil {
			return out, true
		}
		s = s[1:]
		if t := strings.TrimSpace(s); strings.HasPrefix(t, "]") || !list && !strings.HasPrefix(t, "\"") && !strings.HasPrefix(t, "'") && !strings.HasPrefix(t, "$ARGV") {
			return out, true // a trailing comma, or the call's keyword arguments
		}
	}
}

// stringLiteral reads a quoted literal, single or double, python raw strings too, at the start of s, with the
// common backslash escapes decoded.
func stringLiteral(s string) (val, rest string, ok bool) {
	raw := strings.HasPrefix(s, "r'") || strings.HasPrefix(s, `r"`)
	if raw {
		s = s[1:]
	}
	if s == "" || s[0] != '\'' && s[0] != '"' {
		return "", "", false
	}
	q := s[0]
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		c := s[i]
		switch {
		case c == q:
			return b.String(), s[i+1:], true
		case c == '\\' && !raw && i+1 < len(s):
			i++
			switch s[i] {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			default:
				b.WriteByte(s[i])
			}
		default:
			b.WriteByte(c)
		}
	}
	return "", "", false
}
