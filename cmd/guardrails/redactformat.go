package main

// Output formats for redact --pii. pre-tool-use forces a headed format (psql --html, duckdb
// -json) so columns can be masked by name, and --to restores what the command asked for, so
// later pipes (cut -d'|', while read) still work.

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"io"
	"regexp"
	"strings"
	"unicode/utf8"
)

type redactOpts struct {
	in, to   string // in: csv|html|json|jsonl|text|auto; to: ""(as input)|csv|unaligned|aligned|line|markdown|json
	noHeader bool   // psql -t, duckdb -noheader
	footer   bool   // psql's "(N rows)" line
	sep      string // unaligned field separator; "" = "|"
	cap      bool   // the limits duckdb's agent mode would have applied (the forced -json turns it off)
}

// duckdb 2.0 agent-mode limits (`.help agent`): a result is shown whole up to 1000 rows and
// 10000 bytes, else as its first and last 20 rows; cells are cut at 500 characters.
const (
	agentMaxRows, agentMaxBytes, agentEdgeRows, agentMaxCell = 1000, 10000, 20, 500
)

// agentCap applies those limits to t and returns the footer line for a cut result. The
// hint names -no-agent: .maxrows does nothing once the output is JSON.
func agentCap(o redactOpts, t resultTable) (resultTable, string) {
	for _, r := range t.rows {
		for i, c := range r {
			if n := utf8.RuneCountInString(c); n > agentMaxCell {
				r[i] = string([]rune(c)[:agentMaxCell]) + fmt.Sprintf("…(+%d chars)", n-agentMaxCell)
			}
		}
	}
	n := len(t.rows)
	if n <= 2*agentEdgeRows {
		return t, ""
	}
	var b bytes.Buffer
	renderTable(&b, o, t)
	if n <= agentMaxRows && b.Len() <= agentMaxBytes {
		return t, ""
	}
	t.rows = append(t.rows[:agentEdgeRows:agentEdgeRows], t.rows[n-agentEdgeRows:]...)
	return t, fmt.Sprintf("first %d and last %d of %d rows (-no-agent for all)\n", agentEdgeRows, agentEdgeRows, n)
}

type resultTable struct {
	cols []string
	rows [][]string
	text []string // set instead of cols/rows: lines between results, printed as is
}

// docTable turns one duckdb -json document (an array of row objects) into a table.
func docTable(d any) (resultTable, bool) {
	rows, ok := d.([]any)
	if !ok || len(rows) == 0 {
		return resultTable{}, false
	}
	var t resultTable
	idx := map[string]int{}
	for _, r := range rows {
		o, ok := r.(*ordObject)
		if !ok {
			return resultTable{}, false
		}
		for _, k := range o.keys {
			if _, seen := idx[k]; !seen {
				idx[k] = len(t.cols)
				t.cols = append(t.cols, k)
			}
		}
	}
	for _, r := range rows {
		o := r.(*ordObject)
		row := make([]string, len(t.cols))
		for i := range row {
			row[i] = "NULL"
		}
		for i, k := range o.keys {
			row[idx[k]] = cellText(o.vals[i])
		}
		t.rows = append(t.rows, row)
	}
	return t, true
}

func cellText(v any) string {
	if v == nil {
		return "NULL"
	}
	if s, ok := scalarText(v); ok {
		return s
	}
	var b bytes.Buffer
	encodeOrdered(&b, v)
	return b.String()
}

func renderTables(o redactOpts, tables []resultTable) []byte {
	var b bytes.Buffer
	for i, t := range tables {
		if t.text != nil {
			for _, l := range t.text {
				b.WriteString(l + "\n")
			}
			continue
		}
		if i > 0 && (o.to == "line" || o.to == "markdown") {
			b.WriteByte('\n')
		}
		renderTable(&b, o, t)
	}
	return b.Bytes()
}

func renderTable(b *bytes.Buffer, o redactOpts, t resultTable) {
	switch o.to {
	case "csv":
		w := csv.NewWriter(b)
		if !o.noHeader {
			w.Write(t.cols)
		}
		w.WriteAll(t.rows)
	case "unaligned":
		sep := o.sep
		if sep == "" {
			sep = "|"
		}
		if !o.noHeader {
			b.WriteString(strings.Join(t.cols, sep) + "\n")
		}
		for _, r := range t.rows {
			b.WriteString(strings.Join(r, sep) + "\n")
		}
	case "aligned":
		w := make([]int, len(t.cols))
		lines := t.rows
		if !o.noHeader {
			lines = append([][]string{t.cols}, t.rows...)
		}
		for _, r := range lines {
			for i, c := range r {
				w[i] = max(w[i], len(c))
			}
		}
		line := func(r []string) {
			for i, c := range r {
				if i > 0 {
					b.WriteString(" | ")
				} else {
					b.WriteByte(' ')
				}
				if i < len(r)-1 {
					c += strings.Repeat(" ", w[i]-len(c))
				}
				b.WriteString(c)
			}
			b.WriteByte('\n')
		}
		if !o.noHeader {
			line(t.cols)
			dash := make([]string, len(w))
			for i := range w {
				dash[i] = strings.Repeat("-", w[i]+2)
			}
			b.WriteString(strings.Join(dash, "+") + "\n")
		}
		for _, r := range t.rows {
			line(r)
		}
	case "line":
		nw := 0
		for _, c := range t.cols {
			nw = max(nw, len(c))
		}
		for ri, r := range t.rows {
			if ri > 0 {
				b.WriteByte('\n')
			}
			for i, c := range r {
				fmt.Fprintf(b, "%*s = %s\n", nw, t.cols[i], c)
			}
		}
	case "markdown":
		esc := func(r []string) string {
			out := make([]string, len(r))
			for i, c := range r {
				out[i] = strings.ReplaceAll(c, "|", `\|`)
			}
			return "| " + strings.Join(out, " | ") + " |\n"
		}
		b.WriteString(esc(t.cols))
		b.WriteString("|" + strings.Repeat("---|", len(t.cols)) + "\n")
		for _, r := range t.rows {
			b.WriteString(esc(r))
		}
	}
	if o.footer && !o.noHeader {
		if len(t.rows) == 1 {
			b.WriteString("(1 row)\n")
		} else {
			fmt.Fprintf(b, "(%d rows)\n", len(t.rows))
		}
	}
}

// redactOutput masks in and renders it in the requested format.
func (x *redactor) redactOutput(o redactOpts, in []byte) []byte {
	if len(bytes.TrimSpace(in)) == 0 {
		return in
	}
	switch o.in {
	case "csv":
		// psql --csv prints every result set (header, then rows) and any \echo or \pset line
		// into one stream. A record whose field count differs from the current header starts
		// a new table (#64). Adjacent results of the same width still read as one table.
		r := csv.NewReader(bytes.NewReader(in))
		r.FieldsPerRecord, r.LazyQuotes = -1, true
		var tables []resultTable
		var heads []string // each table's header line as printed
		for {
			start := r.InputOffset()
			rec, err := r.Read()
			if err == io.EOF {
				break
			}
			if err != nil {
				return []byte(x.maskValues(string(in)))
			}
			if n := len(tables); n > 0 && len(rec) == len(tables[n-1].cols) {
				tables[n-1].rows = append(tables[n-1].rows, rec)
				continue
			}
			tables = append(tables, resultTable{cols: rec})
			heads = append(heads, strings.TrimRight(string(in[start:r.InputOffset()]), "\r\n"))
		}
		if len(tables) == 0 {
			return []byte(x.maskValues(string(in)))
		}
		for i, t := range tables {
			if len(tables) > 1 && len(t.cols) == 1 && len(t.rows) == 0 {
				// One field and no rows between other results: \echo or \pset text.
				tables[i] = resultTable{text: []string{x.maskValues(heads[i])}}
				continue
			}
			tables[i] = x.maskTable(t)
		}
		if o.to == "" {
			o.to = "csv"
		}
		return renderTables(o, tables)
	case "html":
		tables := psqlHTML(string(in))
		for i, t := range tables {
			if t.text == nil {
				tables[i] = x.maskTable(t)
				continue
			}
			for j, l := range t.text {
				t.text[j] = x.maskValues(l)
			}
		}
		if o.to == "" {
			o.to = "aligned"
		}
		return renderTables(o, tables)
	case "json", "jsonl", "auto", "":
		if t := bytes.TrimSpace(in); o.in == "auto" || o.in == "" {
			if t[0] != '{' && t[0] != '[' {
				break
			}
		}
		docs, ok := x.maskJSONDocs(in)
		if !ok {
			break
		}
		var b bytes.Buffer
		for i, d := range docs {
			t, isTable := docTable(d)
			if o.to == "" || o.to == "json" || !isTable {
				encodeDoc(&b, d)
				continue
			}
			if i > 0 && (o.to == "line" || o.to == "markdown") {
				b.WriteByte('\n')
			}
			foot := ""
			if o.cap {
				t, foot = agentCap(o, t)
			}
			renderTable(&b, o, t)
			b.WriteString(foot)
		}
		return b.Bytes()
	}
	return []byte(x.maskValues(string(in)))
}

// psqlHTML splits psql --html output into one table per result and the lines between them
// (\echo and \pset text, command tags). psql escapes every value, so no value can pass for
// markup and results of the same width stay apart, unlike in --csv (#66).
func psqlHTML(in string) []resultTable {
	var out []resultTable
	text := func(l string) { out = append(out, resultTable{text: []string{l}}) }
	afterTable := false
	for in != "" {
		line, rest, _ := strings.Cut(in, "\n")
		wasTable := afterTable
		afterTable = false
		if body, after, ok := strings.Cut(rest, "</table>\n"); ok && strings.HasPrefix(line, "<table") {
			out = append(out, htmlTable(body)...)
			in, afterTable = after, true
			continue
		}
		if p, after, ok := strings.Cut(strings.TrimPrefix(in, "<p>"), "</p>\n"); ok && strings.HasPrefix(line, "<p>") {
			// A command tag (<p>INSERT 0 1</p>) or a table's footers, one per <br />.
			for _, l := range strings.Split(strings.TrimSuffix(p, "<br />\n"), "<br />\n") {
				if l = htmlText(l); !(wasTable && psqlFooterRE.MatchString(l)) {
					text(l) // the row count is printed again by renderTable
				}
			}
			in = after
			continue
		}
		text(line)
		in = rest
	}
	return out
}

var psqlFooterRE = regexp.MustCompile(`^\(\d+ rows?\)$`)

// htmlTable reads one psql <table>: a caption, then a <th> header row and <td> rows, or in
// expanded mode (\x) one <th>name</th><td>value</td> row per column, records split by a
// colspan row. Expanded records come back as normal rows, so each value is masked by its
// column name. Any other shape becomes text lines, masked by value only.
func htmlTable(body string) []resultTable {
	var out []resultTable
	if _, c, ok := strings.Cut(body, "<caption>"); ok {
		if c, _, ok = strings.Cut(c, "</caption>"); ok {
			out = append(out, resultTable{text: []string{htmlText(c)}})
		}
	}
	type cell struct {
		th, span bool
		v        string
	}
	var rows [][]cell
	for _, tr := range strings.Split(body, "<tr")[1:] {
		var row []cell
		for {
			i := strings.Index(tr, "<td")
			if j := strings.Index(tr, "<th"); j >= 0 && (i < 0 || j < i) {
				i = j
			}
			if i < 0 {
				break
			}
			tag := tr[i+1 : i+3]
			open, rest, _ := strings.Cut(tr[i:], ">")
			v, after, ok := strings.Cut(rest, "</"+tag+">")
			if !ok {
				break
			}
			row = append(row, cell{tag == "th", strings.Contains(open, "colspan"), htmlText(v)})
			tr = after
		}
		rows = append(rows, row)
	}
	var t resultTable
	shaped := len(rows) > 0
	if shaped && len(rows[0]) > 0 && rows[0][0].th { // a header row
		for _, c := range rows[0] {
			shaped = shaped && c.th
			t.cols = append(t.cols, c.v)
		}
		for _, r := range rows[1:] {
			var vs []string
			for _, c := range r {
				shaped = shaped && !c.th
				vs = append(vs, c.v)
			}
			shaped = shaped && len(vs) == len(t.cols)
			t.rows = append(t.rows, vs)
		}
	} else if shaped { // expanded
		var rec []string
		first := true
		for _, r := range rows {
			switch {
			case len(r) == 1 && r[0].span:
				if rec != nil {
					t.rows, first, rec = append(t.rows, rec), false, nil
				}
			case len(r) == 2 && r[0].th && !r[1].th:
				if first {
					t.cols = append(t.cols, r[0].v)
				}
				shaped = shaped && len(rec) < len(t.cols) && t.cols[len(rec)] == r[0].v
				rec = append(rec, r[1].v)
			default:
				shaped = false
			}
		}
		if rec != nil {
			t.rows = append(t.rows, rec)
		}
		shaped = shaped && len(t.rows) > 0 && len(t.rows[len(t.rows)-1]) == len(t.cols)
	}
	if shaped {
		return append(out, t)
	}
	var lines []string
	for _, r := range rows {
		var vs []string
		for _, c := range r {
			vs = append(vs, c.v)
		}
		lines = append(lines, strings.Join(vs, " | "))
	}
	if lines == nil {
		return out
	}
	return append(out, resultTable{text: lines})
}

// htmlText undoes psql's html escaping. A NULL, empty or all-space cell prints as "&nbsp; ".
func htmlText(s string) string {
	if s == "&nbsp; " {
		return ""
	}
	return htmlUnescape.Replace(s)
}

var htmlUnescape = strings.NewReplacer("<br />\n", "\n", "&nbsp;", " ", "&lt;", "<", "&gt;", ">", "&quot;", `"`, "&amp;", "&")

// maskTable masks PII columns by header name and email/phone shapes in the other cells.
func (x *redactor) maskTable(t resultTable) resultTable {
	cats := make([]string, len(t.cols))
	for i, h := range t.cols {
		cats[i] = x.rules.category(h)
		t.cols[i] = x.maskValues(h) // an \echo line with commas reads as a header
	}
	for _, row := range t.rows {
		for i, v := range row {
			if i < len(cats) && cats[i] != "" {
				row[i] = x.maskCell(cats[i], v)
			} else {
				row[i] = x.maskValues(v)
			}
		}
	}
	return t
}
