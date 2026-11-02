package main

// raw-client-data: Python that reads client data prints it unmasked, because redact --pii
// can only wrap SQL clients whose output names its columns. Client data is the S3 objects
// clientDataS3RE matches and local parquet files; the masked mirror ~/.cache/pii-safe/ is
// fine. The user confirms each such run.

import (
	"bytes"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

var (
	parquetRefRE = regexp.MustCompile(`[^\s'"(),=]*\.parquet\b`)
	pythonRE     = regexp.MustCompile(`^i?python[0-9.]*$`)
)

func rawClientDataRule(cmd string) decision {
	cmds, ok := parseShell(cmd)
	if !ok {
		return decision{}
	}
	py := false
	for _, c := range cmds {
		w := c.Words
		if len(w) > 0 && pythonRE.MatchString(path.Base(w[0])) || len(w) > 2 && w[0] == "uv" && w[1] == "run" && pythonRE.MatchString(path.Base(w[2])) {
			py = true
		}
	}
	if !py {
		return decision{}
	}
	ref := ""
	if clientDataS3RE != nil {
		ref = clientDataS3RE.FindString(cmd)
	}
	for _, p := range parquetRefRE.FindAllString(cmd, -1) {
		if ref != "" {
			break
		}
		if !strings.Contains(p, "://") && !strings.Contains(p, "/.cache/pii-safe/") {
			ref = p
		}
	}
	if ref == "" {
		return decision{}
	}
	masked := ""
	if maskedCopyTool != "" {
		masked = ", or a masked copy (" + maskedCopyTool + " writes ~/.cache/pii-safe/...)"
	}
	return decision{Ask: true, Rule: "raw-client-data",
		Msg: "ASK: Python reading raw client data (" + ref + "): its output is not PII-masked. Prefer the duckdb CLI, whose output is masked" + masked + ". Confirm with the user before running it."}
}

// duckdbDotRE finds dot-commands that change how or where duckdb prints results, or run SQL
// the hook can't see, at a line start or just inside a quote (-cmd '.mode line').
var duckdbDotRE = regexp.MustCompile(`(?m)(?:^|['"])[ \t]*\.(mode|output|once|read|shell|system|excel)\b`)

// duckdbDotRule blocks those in a duckdb command: the forced -json no longer holds, so
// redact --pii falls back to email/phone shapes and names print. Claude can redo the call
// with flags, so this blocks with the fix rather than asking.
func duckdbDotRule(cmd string) decision {
	m := duckdbDotRE.FindStringSubmatch(cmd)
	if m == nil {
		return decision{}
	}
	cmds, ok := parseShell(cmd)
	if !ok {
		return decision{}
	}
	for _, c := range cmds {
		if len(c.Words) > 0 && path.Base(c.Words[0]) == "duckdb" {
			return decision{Exit: 2, Rule: "duckdb-dot-command", Msg: duckdbDotMsg(m[1])}
		}
	}
	return decision{}
}

func duckdbDotMsg(dot string) string {
	return "BLOCKED: duckdb ." + dot + " changes how or where results print, so PII masking can't apply. Use flags instead: -csv, -line, -list, -markdown, -noheader, -separator S; COPY (...) TO 'file' to write a file; put the SQL of a .read file inline (-c or a heredoc)."
}

// duckdbHiddenDot is the same check on the SQL the hook couldn't see, made when redact runs
// duckdb: stdin (unless a terminal or /dev/null), -f and -init files, and ~/.duckdbrc. It
// returns the dot-command found and the stdin to pass on.
func duckdbHiddenDot(argv []string, stdin io.Reader) (string, io.Reader) {
	var text strings.Builder
	if f, ok := stdin.(*os.File); duckdbReadsStdin(argv) && (!ok || !isCharDevice(f)) {
		b, _ := io.ReadAll(stdin)
		text.Write(b)
		stdin = bytes.NewReader(b)
	}
	files := []string{filepath.Join(os.Getenv("HOME"), ".duckdbrc")}
	for i := 1; i+1 < len(argv); i++ {
		if argv[i] == "-f" || argv[i] == "-init" {
			files = append(files, argv[i+1])
		}
	}
	for _, f := range files {
		b, _ := os.ReadFile(f)
		text.WriteString("\n" + string(b))
	}
	if m := duckdbDotRE.FindStringSubmatch(text.String()); m != nil {
		return m[1], stdin
	}
	return "", stdin
}

// duckdbReadsStdin: duckdb reads SQL from stdin only without -c/-s/-f/-no-stdin and without
// SQL after the database argument. Otherwise stdin belongs to someone else (a while-read
// loop) and must not be consumed.
func duckdbReadsStdin(argv []string) bool {
	positional := 0
	for i := 1; i < len(argv); i++ {
		name, flag := strings.CutPrefix(argv[i], "-")
		switch name = strings.TrimLeft(name, "-"); {
		case !flag:
			positional++
		case name == "c" || name == "s" || name == "f" || name == "no-stdin":
			return false
		case duckdbArgFlags[name]:
			i++
		}
	}
	return positional < 2
}

func isCharDevice(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}
