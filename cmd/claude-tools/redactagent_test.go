package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// rangeJSON is duckdb -json output for `select range as n, <cell> as s from range(rows)`.
func rangeJSON(rows int, cell string) string {
	var b strings.Builder
	b.WriteString("[")
	for i := range rows {
		if i > 0 {
			b.WriteString(",\n")
		}
		fmt.Fprintf(&b, `{"n":%d,"s":%q}`, i, cell)
	}
	b.WriteString("]\n")
	return b.String()
}

// --cap gives wrapped duckdb the limits its agent mode would have applied (duckdb 2.0
// `.help agent`): whole up to 1000 rows and 10000 bytes, else first and last 20 rows.
func TestRedactAgentCap(t *testing.T) {
	cap := []string{"--in=json", "--to=markdown", "--cap"}
	if got := redactWith(t, rangeJSON(3, "x"), cap...); got != "| n | s |\n|---|---|\n| 0 | x |\n| 1 | x |\n| 2 | x |\n" {
		t.Errorf("small result changed:\n%s", got)
	}
	got := redactWith(t, rangeJSON(1500, "x"), cap...)
	lines := strings.Split(strings.TrimSuffix(got, "\n"), "\n")
	if len(lines) != 2+40+1 || lines[2] != "| 0 | x |" || lines[21] != "| 19 | x |" || lines[22] != "| 1480 | x |" || lines[41] != "| 1499 | x |" {
		t.Errorf("row cap: %d lines, head %q, tail %q", len(lines), lines[:min(3, len(lines))], lines[max(0, len(lines)-3):])
	}
	if want := "first 20 and last 20 of 1500 rows (-no-agent for all)"; lines[len(lines)-1] != want {
		t.Errorf("footer = %q, want %q", lines[len(lines)-1], want)
	}
	if got := redactWith(t, rangeJSON(300, strings.Repeat("b", 60)), cap...); !strings.HasSuffix(got, "first 20 and last 20 of 300 rows (-no-agent for all)\n") {
		t.Errorf("byte cap missing: ...%s", got[max(0, len(got)-120):])
	}
	if got := redactWith(t, rangeJSON(1, strings.Repeat("a", 600)), cap...); !strings.Contains(got, "| 0 | "+strings.Repeat("a", 500)+"…(+100 chars) |") {
		t.Errorf("cell cut wrong: %q", got)
	}
	if n := strings.Count(redactWith(t, rangeJSON(1500, "x"), "--in=json", "--to=markdown"), "\n"); n != 1502 {
		t.Errorf("without --cap: %d lines, want 1502", n)
	}
}

// The hook sees only the command text; SQL from -f FILE or stdin (< q.sql, cat q.sql |) is
// checked when redact runs it, and a format-changing dot-command stops duckdb from running.
func TestRedactRefusesHiddenDuckdbDotCommands(t *testing.T) {
	dir := t.TempDir()
	ran := filepath.Join(dir, "ran")
	fake := filepath.Join(dir, "duckdb")
	os.WriteFile(fake, []byte("#!/bin/sh\ntouch "+ran+"\ncat\n"), 0o700)
	sqlFile := filepath.Join(dir, "q.sql")
	os.WriteFile(sqlFile, []byte("select 1;\n.mode csv\nselect first_name from people;\n"), 0o600)
	for name, tc := range map[string]struct {
		args  []string
		stdin string
	}{
		"stdin": {[]string{fake, "-json"}, ".mode line\nselect * from people;\n"},
		"-f":    {[]string{fake, "-json", "-f", sqlFile}, ""},
	} {
		os.Remove(ran)
		var stdout, stderr bytes.Buffer
		code := runRedact(testRedactor(), append([]string{"--pii", "--in=json", "--"}, tc.args...), strings.NewReader(tc.stdin), &stdout, &stderr)
		if _, err := os.Stat(ran); code != 2 || err == nil || !strings.Contains(stderr.String(), "BLOCKED") {
			t.Errorf("%s: exit %d, ran=%v, stderr %q", name, code, err == nil, stderr.String())
		}
	}
	var stdout, stderr bytes.Buffer
	if code := runRedact(testRedactor(), []string{"--pii", "--in=json", "--", fake, "-json"}, strings.NewReader(`[{"email":"x@y.org"}]`), &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "[email:") {
		t.Errorf("plain stdin SQL: exit %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
	// With -c or SQL as an argument duckdb leaves stdin alone (while read ...; do duckdb -c
	// ...; done < list), so redact must not read it either.
	for _, args := range [][]string{{fake, "-json", "-c", "select 1"}, {fake, "db.duckdb", "select 1"}} {
		in := strings.NewReader("loop input .mode\n.mode csv\n")
		var o, e bytes.Buffer
		if code := runRedact(testRedactor(), append([]string{"--pii", "--in=json", "--"}, args...), in, &o, &e); code != 0 || strings.Contains(e.String(), "BLOCKED") {
			t.Errorf("%v: exit %d, stderr %q", args[1:], code, e.String())
		}
	}
}

// Error messages quote cell values ("Could not convert string 'Ann'"): quoted text that is
// not in the command itself is masked; identifiers the command named stay readable.
func TestRedactMasksValuesInErrors(t *testing.T) {
	script := filepath.Join(t.TempDir(), "err.sh")
	os.WriteFile(script, []byte(`echo "Conversion Error: Could not convert string 'Bortholomew' to INT32 when casting from source column first_name" >&2
echo 'Catalog Error: Table with name "people" does not exist! Did you mean "persons"?' >&2
echo 'DETAIL:  Key (first_name, city)=(Annabel, Hull) already exists.' >&2
echo 'HTTP Error: 404 at URL "https://ext.example.org/v1/ext.gz" and '"'"'http://ext.example.org/v2/ext.gz'"'"'' >&2
echo 'IO Error: no access to the file at PATH "/var/lib/ext/a.ext" and '"'"'/opt/ext/b.ext'"'"'' >&2
echo 'HTTP Error: bad URL "https://zed:hunter2@host.example.org/x" and '"'"'ftp://yann@host.example.org/y'"'"'' >&2
echo 'IO Error: cannot open "rel/dir/cust.csv" and '"'"'cust2.csv'"'"'' >&2
echo 'HTTP Error: 403 at URL "https://host.example.org/u/zoe%40mail.example" and '"'"'https://host.example.org/q?claimant=Quillon'"'"'' >&2
exit 1
`), 0o600)
	var stdout, stderr bytes.Buffer
	code := runRedact(testRedactor(), []string{"--pii", "--in=json", "--", "sh", script, "select * from people"}, strings.NewReader(""), &stdout, &stderr)
	e := stderr.String()
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	for _, leak := range []string{"Bortholomew", "persons", "Annabel", "Hull", "hunter2", "yann", "rel/dir", "cust2.csv", "zoe%40", "Quillon"} {
		if strings.Contains(e, leak) {
			t.Errorf("stderr leaks %q:\n%s", leak, e)
		}
	}
	for _, keep := range []string{`name "people" does`, "source column first_name", "Key (first_name, city)=(", "[value:",
		`"https://ext.example.org/v1/ext.gz"`, `'http://ext.example.org/v2/ext.gz'`,
		`"/var/lib/ext/a.ext"`, `'/opt/ext/b.ext'`} {
		if !strings.Contains(e, keep) {
			t.Errorf("stderr lacks %q:\n%s", keep, e)
		}
	}
}
