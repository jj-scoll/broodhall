package main

// Output formats for redact --pii. pre-tool-use forces a headed format (psql --csv, duckdb
// -json) so columns can be masked by name, and --to restores what the command asked for, so
// later pipes (cut -d'|', while read) still work.

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"strings"
	"unicode/utf8"
)

type redactOpts struct {
	in, to   string // in: csv|json|jsonl|text|auto; to: ""(as input)|csv|unaligned|aligned|line|markdown|json
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
		r := csv.NewReader(bytes.NewReader(in))
		r.FieldsPerRecord, r.LazyQuotes = -1, true
		rows, err := r.ReadAll()
		if err != nil || len(rows) == 0 {
			return []byte(x.maskValues(string(in)))
		}
		t := x.maskTable(resultTable{cols: rows[0], rows: rows[1:]})
		if o.to == "" {
			o.to = "csv"
		}
		return renderTables(o, []resultTable{t})
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

// maskTable masks PII columns by header name and email/phone shapes in the other cells.
func (x *redactor) maskTable(t resultTable) resultTable {
	cats := make([]string, len(t.cols))
	for i, h := range t.cols {
		cats[i] = x.rules.category(h)
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
