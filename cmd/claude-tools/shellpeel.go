package main

// Wrapper peeling: commands that only run another command (sudo, env, timeout, nice, xargs,
// ...) are dropped from a simple command's argv, so the rules see the command that really
// runs. Each wrapper lists the options that take a separate value; everything else starting
// with a dash is one word. Wrappers that hand a string to a shell (watch, su -c, flock -c,
// script -c, trap) are in nestedScripts, and find -exec is in execArgs, because one statement
// can hold several commands there.

import (
	"path"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// wrapperValueOpts maps a wrapper to its options that take a separate value.
var wrapperValueOpts = map[string]map[string]bool{
	"sudo":     {"-u": true, "-g": true, "-h": true, "-p": true, "-C": true, "-D": true, "-R": true, "-T": true, "-U": true},
	"doas":     {"-u": true, "-C": true},
	"env":      {"-u": true, "-C": true, "--unset": true, "--chdir": true},
	"command":  nil,
	"builtin":  nil,
	"nohup":    nil,
	"setsid":   nil,
	"chronic":  nil,
	"unbuffer": nil,
	"exec":     {"-a": true},
	"time":     {"-f": true, "-o": true, "--format": true, "--output": true},
	"timeout":  {"-s": true, "-k": true, "--signal": true, "--kill-after": true},
	"nice":     {"-n": true, "--adjustment": true},
	"ionice":   {"-c": true, "-n": true, "-p": true, "-P": true, "-u": true, "--class": true, "--classdata": true, "--pid": true, "--pgid": true, "--uid": true},
	"stdbuf":   {"-i": true, "-o": true, "-e": true, "--input": true, "--output": true, "--error": true},
	"flock":    {"-w": true, "-E": true, "--timeout": true, "--wait": true, "--conflict-exit-code": true},
	"xargs": {"-a": true, "-d": true, "-E": true, "-I": true, "-L": true, "-n": true, "-P": true, "-s": true, "-R": true, "-S": true,
		"--arg-file": true, "--delimiter": true, "--max-args": true, "--max-procs": true, "--max-chars": true, "--process-slot-var": true},
	"parallel": {"-j": true, "-n": true, "-N": true, "-S": true, "-a": true, "-L": true, "-l": true, "-P": true,
		"--jobs": true, "--max-args": true, "--sshlogin": true, "--arg-file": true},
}

func isAssignment(w string) bool {
	return strings.Contains(w, "=") && !strings.HasPrefix(w, "-") && !strings.Contains(w, "/")
}

// peelWrappers drops leading assignments and wrappers (sudo -u x, env A=b, timeout 5, nice -n 5,
// xargs -n1, ...) so w[0] is the command that actually runs.
func peelWrappers(w []string) []string {
	rest, _ := peelWrappersEnv(w)
	return rest
}

// peelWrappersEnv is peelWrappers that also returns the assignments it dropped (VAR=x git
// commit, env VAR=x git commit), which the command runs with in its environment.
func peelWrappersEnv(w []string) (rest, env []string) {
	for len(w) > 0 {
		if isAssignment(w[0]) {
			env = append(env, w[0])
			w = w[1:]
			continue
		}
		next, ok := peelOne(w)
		if !ok {
			return w, env
		}
		w = next
	}
	return w, env
}

// skipOpts drops the leading options of a wrapper's argv; valued names those that take the
// next word as their value. "--" ends the options.
func skipOpts(w []string, valued map[string]bool) []string {
	for len(w) > 0 && strings.HasPrefix(w[0], "-") && w[0] != "-" {
		a := w[0]
		w = w[1:]
		if a == "--" {
			break
		}
		if valued[a] && len(w) > 0 {
			w = w[1:]
		}
	}
	return w
}

// peelOne strips one wrapper from the front of w; ok is false when w[0] is not a wrapper.
func peelOne(w []string) (rest []string, ok bool) {
	name := path.Base(w[0])
	valued, isWrapper := wrapperValueOpts[name]
	if !isWrapper {
		return nil, false
	}
	args := w[1:]
	switch name {
	case "env":
		return peelEnv(args), true
	case "command":
		if hasArg(args, "-v", "-V") {
			return nil, false // only looks the name up
		}
	case "timeout":
		args = skipOpts(args, valued)
		if len(args) > 0 {
			args = args[1:] // the duration
		}
		return args, true
	case "flock":
		if dashC(args) != "" {
			return nil, false // flock -c 'script': nestedScripts reads the string
		}
		args = skipOpts(args, valued)
		if len(args) > 0 {
			args = args[1:] // the lock file or descriptor
		}
		return args, true
	case "xargs":
		return skipOpts(args, valued), true
	case "parallel":
		return peelParallel(skipOpts(args, valued)), true
	}
	return skipOpts(args, valued), true
}

// peelEnv drops env's options; -S / --split-string holds a whole command line in one word.
func peelEnv(w []string) []string {
	valued := wrapperValueOpts["env"]
	for len(w) > 0 && strings.HasPrefix(w[0], "-") && w[0] != "-" {
		a := w[0]
		w = w[1:]
		switch {
		case a == "--":
			return w
		case a == "-S" || a == "--split-string":
			if len(w) == 0 {
				return nil
			}
			return append(splitWords(w[0]), w[1:]...)
		case strings.HasPrefix(a, "--split-string="):
			return append(splitWords(strings.TrimPrefix(a, "--split-string=")), w...)
		case strings.HasPrefix(a, "-S") && !strings.HasPrefix(a, "--"):
			return append(splitWords(a[2:]), w...)
		case valued[a] && len(w) > 0:
			w = w[1:]
		}
	}
	return w
}

// splitWords splits a command line into words the way a shell would; a line that is not one
// simple command splits at spaces.
func splitWords(s string) []string {
	f, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(s), "")
	if err == nil && len(f.Stmts) == 1 {
		if call, ok := f.Stmts[0].Cmd.(*syntax.CallExpr); ok {
			ws := make([]string, len(call.Args))
			for i, a := range call.Args {
				ws[i] = wordText(a)
			}
			return ws
		}
	}
	return strings.Fields(s)
}

// peelParallel turns `cmd ... ::: a b` into the command GNU parallel runs: the words before
// the first ":::" with each {} replaced by all the arguments, or the arguments appended.
func peelParallel(w []string) []string {
	cut := -1
	for i, a := range w {
		if strings.HasPrefix(a, ":::") {
			cut = i
			break
		}
	}
	if cut < 0 {
		return w
	}
	var args []string
	for _, a := range w[cut+1:] {
		if !strings.HasPrefix(a, ":::") {
			args = append(args, a)
		}
	}
	var out []string
	placed := false
	for _, a := range w[:cut] {
		if a == "{}" {
			out = append(out, args...)
			placed = true
			continue
		}
		out = append(out, a)
	}
	if !placed {
		out = append(out, args...)
	}
	return out
}

// dashC returns the script a `-c` option hands to a shell-like command (bash -c, su -c,
// script -c, flock -c): the word after a short cluster holding c (-c, -lc, -qec), or the
// value of --command. "" when there is none.
func dashC(args []string) string {
	for i, a := range args {
		switch {
		case strings.HasPrefix(a, "--command="):
			return strings.TrimPrefix(a, "--command=")
		case a == "--command" && i+1 < len(args):
			return args[i+1]
		case strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.Contains(a, "c") && i+1 < len(args):
			return args[i+1]
		}
	}
	return ""
}

// execArgs lists the commands a find hands to -exec, -execdir, -ok or -okdir: the words up
// to the ; or + that ends each one.
func execArgs(w []string) [][]string {
	var out [][]string
	for i := 1; i < len(w); i++ {
		switch w[i] {
		case "-exec", "-execdir", "-ok", "-okdir":
			j := i + 1
			for j < len(w) && w[j] != ";" && w[j] != "+" {
				j++
			}
			if j > i+1 {
				out = append(out, w[i+1:j])
			}
			i = j
		}
	}
	return out
}

// watchScript is the command line watch runs through sh -c: its words after the options.
func watchScript(args []string) string {
	args = skipOpts(args, map[string]bool{"-n": true, "--interval": true})
	return strings.Join(args, " ")
}
