package main

// piiRewrite: the PreToolUse rewrite that runs SQL clients under `guardrails redact --pii`
// so client PII is masked before Claude, the transcript or a memory tool see the output. psql
// is forced to --html (one table per result, every value escaped) and duckdb to -json, so the
// redactor knows each result's column names; --to then
// restores the format the command asked for. Every duckdb call that runs SQL is wrapped: its
// SQL may come from a file (-f, < q.sql) or a .duckdb holding client tables, which the
// command text doesn't show. aws rds-data execute-statement gains --include-result-metadata;
// athena get-query-results is masked by its ColumnInfo. Gaps: SQL run inside `bash -c`,
// scripts, ssh or ssm, a \pset format or tuples_only inside a psql script (value masking
// only), and output written to a file and read later.

import (
	"path"
	"regexp"
	"sort"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

type shellEdit struct {
	start, end int
	text       string
}

// sqlCall is one parsed simple command: its words as the shell passes them, and as written.
type sqlCall struct {
	src   string
	args  []*syntax.Word
	words []string
}

func (c sqlCall) raw(i int) string { return c.src[c.args[i].Pos().Offset():c.args[i].End().Offset()] }
func (c sqlCall) pos(i int) int    { return int(c.args[i].Pos().Offset()) }
func (c sqlCall) end(i int) int    { return int(c.args[i].End().Offset()) }

// del removes words i..j (inclusive) and the space before them.
func (c sqlCall) del(i, j int) shellEdit { return shellEdit{c.end(i - 1), c.end(j), ""} }

// set replaces words i..j with text.
func (c sqlCall) set(i, j int, text string) shellEdit { return shellEdit{c.pos(i), c.end(j), text} }

func piiRewrite(cmd, self string) (string, bool) {
	f, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(cmd), "")
	if err != nil {
		return "", false
	}
	qself, err := syntax.Quote(self, syntax.LangBash)
	if err != nil {
		return "", false
	}
	// hidden holds calls whose stdout is piped, captured or sent to a file, not to the tool
	// result: <private> lines there would corrupt the next reader (jq, head, a CSV file).
	hidden := map[*syntax.CallExpr]bool{}
	hide := func(n syntax.Node) {
		syntax.Walk(n, func(n syntax.Node) bool {
			if ce, ok := n.(*syntax.CallExpr); ok {
				hidden[ce] = true
			}
			return true
		})
	}
	syntax.Walk(f, func(n syntax.Node) bool {
		switch n := n.(type) {
		case *syntax.BinaryCmd:
			if n.Op == syntax.Pipe || n.Op == syntax.PipeAll {
				hide(n.X)
			}
		case *syntax.CmdSubst, *syntax.ProcSubst:
			hide(n)
		case *syntax.Stmt:
			for _, r := range n.Redirs {
				switch r.Op {
				case syntax.RdrOut, syntax.AppOut, syntax.RdrClob, syntax.AppClob, syntax.RdrAll, syntax.AppAll:
					if r.N == nil || r.N.Value == "1" || r.Op == syntax.RdrAll || r.Op == syntax.AppAll {
						hide(n)
					}
				}
			}
		}
		return true
	})
	var edits []shellEdit
	syntax.Walk(f, func(n syntax.Node) bool {
		ce, ok := n.(*syntax.CallExpr)
		if !ok || len(ce.Args) == 0 {
			return true
		}
		c := sqlCall{src: cmd, args: ce.Args, words: make([]string, len(ce.Args))}
		for i, a := range ce.Args {
			c.words[i] = wordText(a)
		}
		var flags []string
		var es []shellEdit
		switch path.Base(c.words[0]) {
		case "psql":
			flags, es = psqlPlan(c)
		case "duckdb":
			flags, es = duckdbPlan(c)
		case "aws":
			flags, es = awsPlan(c)
		}
		if flags != nil {
			edits = append(edits, es...)
			if !hidden[ce] {
				flags = append([]string{"--private"}, flags...)
			}
			edits = append(edits, shellEdit{c.pos(0), c.pos(0), qself + " redact --pii " + strings.Join(flags, " ") + " -- "})
		}
		return true
	})
	if len(edits) == 0 {
		return "", false
	}
	sort.SliceStable(edits, func(i, j int) bool { return edits[i].start > edits[j].start })
	out := cmd
	for _, e := range edits {
		out = out[:e.start] + e.text + out[e.end:]
	}
	if _, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(out), ""); err != nil {
		return "", false
	}
	return out, true
}

var (
	psqlArgFlags     = map[string]bool{"-c": true, "-d": true, "-f": true, "-h": true, "-L": true, "-o": true, "-p": true, "-R": true, "-T": true, "-U": true, "-v": true, "--command": true, "--dbname": true, "--file": true, "--host": true, "--log-file": true, "--output": true, "--port": true, "--record-separator": true, "--table-attr": true, "--username": true, "--set": true, "--variable": true}
	psqlNoArgLetters = "aAbeEHlnqsStwWxX1"
	psqlArgLetters   = "cdfFhLopPRTUv"
	// psqlFormatPset are \pset options that override --csv or its header.
	psqlFormatPset = regexp.MustCompile(`^(?:format|tuples_only|csv_fieldsep|expanded|x)\b`)
)

// psqlPlan forces --html and returns the redact flags that restore -t/-A/-F/--csv. --csv
// can't be kept: results of the same width run together in it (#66). When the output format
// can't be pinned down (-P format=..., -H) or the results go to a file (-o), it masks values
// only.
func psqlPlan(c sqlCall) ([]string, []shellEdit) {
	var tuples, unaligned, csvAlready bool
	sep := ""
	type drop struct {
		i, j int
		keep string // what stays of a grouped flag (-qA -> -q, -Atc -> -c)
	}
	var drops []drop
	textOnly := []string{"--in=text"}
	for i := 1; i < len(c.words); i++ {
		w := c.words[i]
		switch {
		case w == "-t" || w == "--tuples-only":
			tuples = true
			drops = append(drops, drop{i, i, ""})
		case w == "-A" || w == "--no-align":
			unaligned = true
			drops = append(drops, drop{i, i, ""})
		case w == "--csv":
			csvAlready = true
			drops = append(drops, drop{i, i, ""})
		case w == "-H" || w == "--html":
			return textOnly, nil
		case w == "-o" || w == "--output" || strings.HasPrefix(w, "--output="):
			return textOnly, nil // results go to the file, which keeps the asked format
		case (w == "-F" || w == "--field-separator") && i+1 < len(c.words):
			sep = c.raw(i + 1)
			drops = append(drops, drop{i, i + 1, ""})
			i++
		case strings.HasPrefix(w, "--field-separator="):
			sep = strings.SplitN(c.raw(i), "=", 2)[1]
			drops = append(drops, drop{i, i, ""})
		case strings.HasPrefix(w, "-F") && !strings.HasPrefix(w, "--"):
			sep = c.raw(i)[2:]
			drops = append(drops, drop{i, i, ""})
		case w == "-P" || w == "--pset":
			if i+1 < len(c.words) && psqlFormatPset.MatchString(c.words[i+1]) {
				return textOnly, nil
			}
			i++
		case strings.HasPrefix(w, "--pset="):
			if psqlFormatPset.MatchString(w[len("--pset="):]) {
				return textOnly, nil
			}
		case psqlArgFlags[w]:
			i++
		case len(w) > 2 && w[0] == '-' && w[1] != '-':
			// A grouped flag: no-arg letters, then at most one letter whose value is the rest
			// of the word or the next word (-Atc "select").
			d, changed := drop{i, i, ""}, false
			for k := 1; k < len(w); k++ {
				l := w[k]
				switch {
				case l == 't':
					tuples, changed = true, true
				case l == 'A':
					unaligned, changed = true, true
				case l == 'H' || l == 'o':
					return textOnly, nil
				case strings.IndexByte(psqlNoArgLetters, l) >= 0:
					d.keep += string(l)
				case strings.IndexByte(psqlArgLetters, l) >= 0:
					val, attached := w[k+1:], k+1 < len(w)
					if !attached && i+1 < len(c.words) {
						val = c.words[i+1]
					}
					switch {
					case l == 'P' && psqlFormatPset.MatchString(val):
						return textOnly, nil
					case l == 'F' && attached:
						sep, changed = c.raw(i)[k+1:], true
					case l == 'F':
						sep, changed, d.j = c.raw(i+1), true, i+1
					default:
						d.keep += w[k:]
					}
					if !attached && l != 'F' {
						i++ // the next word is this letter's value
					}
					k = len(w)
				default:
					return textOnly, nil
				}
			}
			if changed {
				if d.keep != "" {
					d.keep = "-" + d.keep
				}
				drops = append(drops, d)
			}
		}
	}
	flags := []string{"--in=html"}
	switch {
	case csvAlready && tuples:
		flags = append(flags, "--to=csv", "--no-header")
	case csvAlready:
		flags = append(flags, "--to=csv")
	case unaligned:
		flags = append(flags, "--to=unaligned")
	default:
		flags = append(flags, "--to=aligned")
	}
	if !csvAlready {
		if tuples {
			flags = append(flags, "--no-header")
		} else {
			flags = append(flags, "--footer")
		}
		if sep != "" && unaligned {
			flags = append(flags, "--sep="+sep)
		}
	}
	var es []shellEdit
	for k, d := range drops {
		text := d.keep
		if k == 0 {
			text = strings.TrimSpace("--html " + d.keep) // before the kept letters: -c takes the next word
		}
		if text != "" {
			es = append(es, c.set(d.i, d.j, text))
		} else {
			es = append(es, c.del(d.i, d.j))
		}
	}
	if len(drops) == 0 {
		es = append(es, shellEdit{c.end(0), c.end(0), " --html"})
	}
	return flags, es
}

var (
	duckdbArgFlags = map[string]bool{"c": true, "cmd": true, "s": true, "init": true, "nullvalue": true, "newline": true, "f": true, "format-file": true, "storage-version": true}
	// duckdbNoSQL are the flags under which duckdb prints no query results.
	duckdbNoSQL = map[string]bool{"ui": true, "serve": true, "version": true, "help": true, "h": true, "manual": true, "format": true, "format-file": true}
)

// duckdbPlan forces -json and returns the redact flags that restore the asked output mode.
// With no mode flag and no -no-agent, duckdb would have run in agent mode (AI_AGENT is set
// in Claude Code's shell), so --cap keeps its row and cell limits.
func duckdbPlan(c sqlCall) ([]string, []shellEdit) {
	to, noHeader, sep := "markdown", false, "" // default (agent/box) output reads best as markdown
	mode, noAgent := false, false
	var drop [][2]int
	for i := 1; i < len(c.words); i++ {
		w := c.words[i]
		if !strings.HasPrefix(w, "-") {
			continue
		}
		name := strings.TrimLeft(w, "-")
		if duckdbNoSQL[name] {
			return nil, nil
		}
		switch name {
		case "json", "jsonlines":
			return []string{"--in=json"}, nil
		case "no-agent":
			noAgent = true
			continue
		case "csv", "quote":
			to = "csv"
		case "list":
			to = "unaligned"
		case "line":
			to = "line"
		case "markdown", "box", "table", "column", "ascii", "html":
			to = "markdown"
		case "noheader":
			noHeader = true
		case "header":
		case "separator":
			if i+1 < len(c.words) {
				sep = c.raw(i + 1)
				drop = append(drop, [2]int{i, i + 1})
				i++
			}
			continue
		default:
			if duckdbArgFlags[name] {
				i++
			}
			continue
		}
		mode = mode || name != "noheader" && name != "header"
		drop = append(drop, [2]int{i, i})
	}
	flags := []string{"--in=json", "--to=" + to}
	if noHeader {
		flags = append(flags, "--no-header")
	}
	if !mode && !noAgent {
		flags = append(flags, "--cap")
	}
	if sep != "" && (to == "unaligned" || to == "csv") {
		flags = append(flags, "--sep="+sep)
	}
	var es []shellEdit
	for k, d := range drop {
		if k == 0 {
			es = append(es, c.set(d[0], d[1], "-json"))
		} else {
			es = append(es, c.del(d[0], d[1]))
		}
	}
	if len(drop) == 0 {
		es = append(es, shellEdit{c.end(0), c.end(0), " -json"})
	}
	return flags, es
}

var awsValueFlags = map[string]bool{"--output": true, "--profile": true, "--region": true, "--endpoint-url": true, "--query": true, "--color": true, "--ca-bundle": true, "--cli-read-timeout": true, "--cli-connect-timeout": true, "--cli-binary-format": true}

// awsPlan wraps rds-data execute-statement (adding --include-result-metadata) and athena
// get-query-results. Text or table output carries no column names: values only.
func awsPlan(c sqlCall) ([]string, []shellEdit) {
	var pos []string
	text, meta := false, false
	for i := 1; i < len(c.words); i++ {
		w := c.words[i]
		switch {
		case w == "--output" && i+1 < len(c.words):
			text = c.words[i+1] == "text" || c.words[i+1] == "table"
			i++
		case strings.HasPrefix(w, "--output="):
			text = w == "--output=text" || w == "--output=table"
		case w == "--include-result-metadata":
			meta = true
		case awsValueFlags[w]:
			i++
		case !strings.HasPrefix(w, "-") && len(pos) < 2:
			pos = append(pos, w)
		}
	}
	if len(pos) < 2 {
		return nil, nil
	}
	in := "--in=json"
	if text {
		in = "--in=auto" // text output can itself be JSON (--query formattedRecords)
	}
	switch pos[0] + " " + pos[1] {
	case "rds-data execute-statement":
		if meta {
			return []string{in}, nil
		}
		last := len(c.args) - 1
		return []string{in}, []shellEdit{{c.end(last), c.end(last), " --include-result-metadata"}}
	case "athena get-query-results":
		return []string{in}, nil
	}
	return nil, nil
}
