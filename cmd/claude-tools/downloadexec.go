package main

// Download-and-run: code that comes off the network and runs before anyone has read it.
// Four shapes ask:
//   - a download piped into a shell or interpreter that reads its script from stdin
//     (curl ... | sh, wget -O- ... | sudo bash, curl ... | tee x | sh);
//   - a download handed to one as a file descriptor, a -c string or stdin
//     (bash <(curl ...), sh -c "$(curl ...)", eval "$(curl ...)", bash <<< "$(curl ...)");
//   - a file downloaded and then run in the same command (curl -o x ... && chmod +x x && ./x);
//   - the same inside bash -c, ssh and other strings handed to a shell.
// It asks rather than blocks: rustup, nvm and brew install this way on purpose, and the user
// can read the URL. A pipe into `sh -c 'script'` is fine, since stdin is then data. Piping a
// local file (cat x.sh | bash) is not a download and passes. There is no switch.

import (
	"path"
	"regexp"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// downloadExecFallback reads text the parser rejects: a downloader piped on to a shell.
var downloadExecFallback = regexp.MustCompile(`(?:^|[;&|(\s])(?:curl|wget)\b[^;&\n]*\|\s*(?:sudo\s+(?:-\S+\s+)*)?(?:\S*/)?(?:(?:ba|z|da|k)?sh|python[0-9.]*|perl|ruby|node)\b`)

func downloadExecRule(cmd string) decision {
	what := downloadExecIn(cmd, 0)
	if what == "" {
		return decision{}
	}
	return decision{Ask: true, Rule: "download-exec",
		Msg: "ASK: " + what + ". The code runs before anyone has read it. Download it, read it, then run it, or confirm with the user."}
}

// scriptRunners are the commands that run code read from stdin or a file.
var scriptRunners = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true,
	"python": true, "perl": true, "ruby": true, "node": true, "source": true, ".": true}

var shellNames = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true}

// runnerName is the base name without a version suffix (python3.12 is python).
func runnerName(w string) string {
	n := path.Base(w)
	if n == "." {
		return n
	}
	return strings.TrimRight(n, "0123456789.")
}

func isDownloader(words []string) bool {
	if len(words) == 0 {
		return false
	}
	n := path.Base(words[0])
	return n == "curl" || n == "wget"
}

// cmdWords is a statement's argv as text, wrappers peeled, and the Word node of each
// remaining argument (nil when peeling rewrote the argv).
func cmdWords(st *syntax.Stmt) (words []string, nodes []*syntax.Word) {
	call, ok := st.Cmd.(*syntax.CallExpr)
	if !ok {
		return nil, nil
	}
	texts := make([]string, len(call.Args))
	for i, a := range call.Args {
		texts[i] = wordText(a)
	}
	words = peelWrappers(texts)
	off := len(texts) - len(words)
	if off < 0 {
		return words, nil
	}
	for i, w := range words {
		if texts[off+i] != w {
			return words, nil
		}
	}
	return words, call.Args[off:]
}

// hasDownload reports whether n holds a curl or wget command anywhere.
func hasDownload(n syntax.Node) bool {
	found := false
	syntax.Walk(n, func(x syntax.Node) bool {
		if st, ok := x.(*syntax.Stmt); ok && !found {
			if w, _ := cmdWords(st); isDownloader(w) {
				found = true
			}
		}
		return !found
	})
	return found
}

// substWithDownload reports whether the word holds a <(...) (procSub) or $(...) (cmdSub)
// that runs a download.
func substWithDownload(w *syntax.Word, procSub, cmdSub bool) bool {
	found := false
	syntax.Walk(w, func(x syntax.Node) bool {
		switch x.(type) {
		case *syntax.ProcSubst:
			found = found || procSub && hasDownload(x)
		case *syntax.CmdSubst:
			found = found || cmdSub && hasDownload(x)
		}
		return !found
	})
	return found
}

// readsStdinAsScript reports whether the command runs the code it reads from stdin: a shell
// or interpreter with no script file and no -c/-e/-m string, or source /dev/stdin.
func readsStdinAsScript(words []string) bool {
	if len(words) == 0 {
		return false
	}
	name := runnerName(words[0])
	if !scriptRunners[name] {
		return false
	}
	args := words[1:]
	switch {
	case name == "source" || name == ".":
		return len(args) > 0 && (args[0] == "/dev/stdin" || args[0] == "-")
	case shellNames[name]:
		if script := dashC(args); script != "" {
			// stdin is data for the script, unless the script itself runs a shell on it.
			if inner, ok := parseShell(script); ok {
				for _, c := range inner {
					if readsStdinAsScript(c.Words) {
						return true
					}
				}
			}
			return false
		}
	}
	valued := map[string]bool{"-o": true, "-O": true, "--rcfile": true, "--init-file": true}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-" || a == "-s" && shellNames[name]:
			return true
		case a == "--":
			return i+1 >= len(args) // a file after -- is the script
		case valued[a]:
			i++
		case name == "python" && (a == "-c" || a == "-m"), name != "python" && !shellNames[name] && (a == "-e" || a == "-E"):
			return false
		case strings.HasPrefix(a, "-"):
		default:
			return false // a script file: stdin is data
		}
	}
	return true
}

// firstOperand is the file a runner executes: the first argument that is not an option.
// ok is false for -c / -e / -m forms, which run a string or module.
func firstOperand(words []string) (file string, idx int, ok bool) {
	name := runnerName(words[0])
	args := words[1:]
	if name == "source" || name == "." {
		if len(args) > 0 {
			return args[0], 1, true
		}
		return "", 0, false
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			if i+1 < len(args) {
				return args[i+1], i + 2, true
			}
			return "", 0, false
		case a == "-o" || a == "-O":
			i++
		case a == "-" || a == "-s":
			return "", 0, false
		case strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && (strings.ContainsAny(a, "cem") && !shellNames[name] || strings.Contains(a, "c") && shellNames[name]):
			return "", 0, false
		case strings.HasPrefix(a, "-"):
		default:
			return a, i + 1, true
		}
	}
	return "", 0, false
}

// downloadedFiles lists the files a curl or wget command writes: -o / -O, a redirect, or the
// name the URL ends in.
func downloadedFiles(words []string, redirs []*syntax.Redirect) []string {
	curl := path.Base(words[0]) == "curl"
	var files []string
	remoteName, stdout := false, false
	url, prefix := "", ""
	args := words[1:]
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case strings.HasPrefix(a, "--output=") && curl:
			files = append(files, strings.TrimPrefix(a, "--output="))
		case strings.HasPrefix(a, "--output-document=") && !curl:
			files = append(files, strings.TrimPrefix(a, "--output-document="))
		case (a == "--output" && curl || a == "--output-document" && !curl) && i+1 < len(args):
			i++
			files = append(files, args[i])
		case a == "--remote-name" && curl:
			remoteName = true
		case (a == "--directory-prefix" || a == "-P") && !curl && i+1 < len(args):
			i++
			prefix = args[i]
		case strings.HasPrefix(a, "--directory-prefix=") && !curl:
			prefix = strings.TrimPrefix(a, "--directory-prefix=")
		case strings.HasPrefix(a, "--"):
		case strings.HasPrefix(a, "-") && a != "-":
			out := byte('O')
			if curl {
				out = 'o'
			}
			for j := 1; j < len(a); j++ {
				if a[j] == 'O' && curl {
					remoteName = true
				}
				if a[j] == out {
					val := a[j+1:]
					if val == "" && i+1 < len(args) {
						i++
						val = args[i]
					}
					files = append(files, val)
					break
				}
			}
		case strings.Contains(a, "://"):
			url = a
		default:
			if url == "" {
				url = a
			}
		}
	}
	for _, r := range redirs {
		if r.Word != nil && (r.Op == syntax.RdrOut || r.Op == syntax.AppOut || r.Op == syntax.RdrClob) {
			files = append(files, wordText(r.Word))
		}
	}
	var out []string
	for _, f := range files {
		if f == "-" {
			stdout = true
			continue
		}
		out = append(out, path.Clean(f))
	}
	if len(out) == 0 && !stdout && url != "" && (remoteName || !curl) {
		out = append(out, path.Join(prefix, urlBase(url)))
	}
	return out
}

// urlBase is the file name wget and curl -O give a download.
func urlBase(u string) string {
	if i := strings.Index(u, "://"); i >= 0 {
		u = u[i+3:]
		if j := strings.IndexByte(u, '/'); j >= 0 {
			u = u[j:]
		} else {
			u = "/"
		}
	}
	if i := strings.IndexAny(u, "?#"); i >= 0 {
		u = u[:i]
	}
	if b := path.Base(u); b != "/" && b != "." {
		return b
	}
	return "index.html"
}

// stagesOf flattens a pipeline into its stages, left to right.
func stagesOf(st *syntax.Stmt) []*syntax.Stmt {
	if b, ok := st.Cmd.(*syntax.BinaryCmd); ok && (b.Op == syntax.Pipe || b.Op == syntax.PipeAll) {
		return append(stagesOf(b.X), stagesOf(b.Y)...)
	}
	return []*syntax.Stmt{st}
}

// downloadExecIn describes the first download-and-run in cmd, "" if there is none.
func downloadExecIn(cmd string, depth int) string {
	if depth > maxShellDepth {
		return ""
	}
	f, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(cmd), "")
	if err != nil {
		if downloadExecFallback.MatchString(cmd) {
			return "this pipes a download into a shell (the text could not be parsed)"
		}
		return ""
	}
	what := ""
	downloaded := map[string]bool{}
	syntax.Walk(f, func(n syntax.Node) bool {
		if what != "" {
			return false
		}
		switch n := n.(type) {
		case *syntax.BinaryCmd:
			if n.Op != syntax.Pipe && n.Op != syntax.PipeAll {
				return true
			}
			stages := stagesOf(&syntax.Stmt{Cmd: n})
			for i, s := range stages {
				if !hasDownload(s) {
					continue
				}
				for _, later := range stages[i+1:] {
					if w, _ := cmdWords(later); readsStdinAsScript(w) {
						what = "this pipes a download into " + path.Base(w[0])
						return false
					}
				}
			}
		case *syntax.Stmt:
			words, nodes := cmdWords(n)
			if len(words) == 0 {
				return true
			}
			name := runnerName(words[0])
			if w := runsDownloadedWord(n, name, words, nodes); w != "" {
				what = w
				return false
			}
			// Strings handed to another shell: the same rules one level down.
			var feeds []string
			for _, r := range n.Redirs {
				switch r.Op {
				case syntax.Hdoc, syntax.DashHdoc:
					if r.Hdoc != nil {
						feeds = append(feeds, wordText(r.Hdoc))
					}
				case syntax.WordHdoc:
					feeds = append(feeds, wordText(r.Word))
				}
			}
			for _, s := range nestedScripts(words, feeds) {
				if w := downloadExecIn(s.text, depth+1); w != "" {
					what = w
					return false
				}
			}
			// A file downloaded earlier in the command, run now.
			if isDownloader(words) {
				for _, f := range downloadedFiles(words, n.Redirs) {
					downloaded[f] = true
				}
				return true
			}
			if len(downloaded) > 0 {
				if file := executedFile(words); file != "" && downloaded[path.Clean(file)] {
					what = "this runs " + file + ", which the same command downloaded"
					return false
				}
			}
		}
		return true
	})
	return what
}

// executedFile is the file a command runs: ./x, /tmp/x, or the script operand of a shell,
// interpreter or source. Bare names look up PATH and are not files here.
func executedFile(words []string) string {
	if strings.Contains(words[0], "/") && !scriptRunners[runnerName(words[0])] {
		return words[0]
	}
	if scriptRunners[runnerName(words[0])] {
		if file, _, ok := firstOperand(words); ok {
			return file
		}
	}
	return ""
}

// runsDownloadedWord finds a download handed to a shell or eval: <(curl ...) as the script
// file, $(curl ...) as the -c string or eval's text, or curl's output as the shell's stdin.
func runsDownloadedWord(st *syntax.Stmt, name string, words []string, nodes []*syntax.Word) string {
	if !scriptRunners[name] && name != "eval" {
		return ""
	}
	if name == "eval" {
		for _, n := range nodes {
			if substWithDownload(n, false, true) {
				return "this evals the output of a download"
			}
		}
	}
	// Script operand and -c string, by position among the nodes.
	if nodes != nil && name != "eval" {
		if _, idx, ok := firstOperand(words); ok && idx < len(nodes) && substWithDownload(nodes[idx], true, false) {
			return "this runs a download through process substitution"
		}
		if shellNames[name] {
			if script := dashC(words[1:]); script != "" {
				for i, w := range words[1:] {
					if w == script && substWithDownload(nodes[i+1], false, true) {
						return "this runs the output of a download with " + name + " -c"
					}
				}
			}
		}
	}
	// Download as stdin: bash < <(curl ...), bash <<< "$(curl ...)".
	if readsStdinAsScript(words) {
		for _, r := range st.Redirs {
			if r.Word == nil {
				continue
			}
			switch r.Op {
			case syntax.RdrIn:
				if substWithDownload(r.Word, true, false) {
					return "this feeds a download to " + name + " on stdin"
				}
			case syntax.WordHdoc:
				if substWithDownload(r.Word, false, true) {
					return "this feeds a download to " + name + " on stdin"
				}
			}
		}
	}
	return ""
}
