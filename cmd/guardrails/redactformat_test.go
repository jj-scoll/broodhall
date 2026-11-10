package main

import (
	"bytes"
	"regexp"
	"strings"
	"testing"
)

// tagsOut replaces every mask tag with [cat:T] so expected output can be written literally.
func tagsOut(s string) string {
	return regexp.MustCompile(`\[([a-z_]+):[0-9a-f]{6}\]`).ReplaceAllString(s, "[$1:T]")
}

func redactWith(t *testing.T, stdin string, args ...string) string {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if code := runRedact(testRedactor(), append([]string{"--pii"}, args...), strings.NewReader(stdin), &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	return tagsOut(stdout.String())
}

func TestRedactRestoresPsqlFormats(t *testing.T) {
	csvIn := "id,email\n1,x@y.org\n2,\n"
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"-t -A", []string{"--in=csv", "--to=unaligned", "--no-header"}, "1|[email:T]\n2|\n"},
		{"-A", []string{"--in=csv", "--to=unaligned", "--footer"}, "id|email\n1|[email:T]\n2|\n(2 rows)\n"},
		{"-A -F tab", []string{"--in=csv", "--to=unaligned", "--sep=\t", "--no-header"}, "1\t[email:T]\n2\t\n"},
		{"--csv kept", []string{"--in=csv"}, "id,email\n1,[email:T]\n2,\n"},
		{"default aligned", []string{"--in=csv", "--to=aligned", "--footer"}, ""}, // shape-checked below
		{"-t aligned", []string{"--in=csv", "--to=aligned", "--no-header"}, " 1 | [email:T]\n 2 | \n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := redactWith(t, csvIn, tc.args...)
			if tc.name == "default aligned" {
				// Widths depend on the real tag length; check the shape, not the padding.
				lines := strings.Split(got, "\n")
				if len(lines) != 6 || !strings.Contains(lines[0], "id") || !strings.Contains(lines[0], "| ") ||
					!regexp.MustCompile(`^-+\+-+$`).MatchString(lines[1]) || !strings.Contains(lines[2], "[email:T]") || lines[4] != "(2 rows)" {
					t.Errorf("aligned:\n%s", got)
				}
				return
			}
			if got != tc.want {
				t.Errorf("got %q\nwant %q", got, tc.want)
			}
		})
	}
	if got := redactWith(t, "n\n1\n", "--in=csv", "--to=unaligned", "--footer"); got != "n\n1\n(1 row)\n" {
		t.Errorf("singular footer: %q", got)
	}
}

// psql --csv -f script.sql prints every result set and \echo/\pset line into one stream
// (captured from psql 18). Issue #64: this panicked in the aligned renderer.
const psqlScriptCSV = "Pager usage is off.\n" +
	"== tables: location, size, estimated rows\n" +
	"schema,tbl,size,est,email\n" +
	"public,users,8 kB,3,x@y.org\n" +
	"== foreign keys pointing INTO these tables\n" +
	"conrelid,confrelid,conname\n" +
	"== user triggers on these tables\n" +
	"tgrelid,tgname\n" +
	"users,trg\n"

func TestRedactPsqlScriptMixedOutput(t *testing.T) {
	got := redactWith(t, psqlScriptCSV, "--in=csv", "--to=aligned", "--footer")
	// Widths count the real 14-character tag; tagsOut shortens it after rendering.
	want := "Pager usage is off.\n" +
		" == tables: location |  size |  estimated rows\n" +
		"---------------------+-------+-----------------\n" +
		"(0 rows)\n" +
		" schema | tbl   | size | est | email\n" +
		"--------+-------+------+-----+----------------\n" +
		" public | users | 8 kB | 3   | [email:T]\n" +
		"(1 row)\n" +
		"== foreign keys pointing INTO these tables\n" +
		" conrelid | confrelid | conname\n" +
		"----------+-----------+---------\n" +
		"(0 rows)\n" +
		"== user triggers on these tables\n" +
		" tgrelid | tgname\n" +
		"---------+--------\n" +
		" users   | trg\n" +
		"(1 row)\n"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestRedactPsqlLaterResultMaskedByItsOwnHeader(t *testing.T) {
	// psql --csv -c 'select id ...; select first_name, last_name ...': the second header
	// must name the second result's columns, not be read as a row of the first.
	got := redactWith(t, "id\n1\nfirst_name,last_name\nAnn,Lee\n", "--in=csv")
	if want := "id\n1\nfirst_name,last_name\n[person_name:T],[person_name:T]\n"; got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

// psql --html -f script.sql (captured from psql 18.6): \echo text, two 1-column results, an
// empty result, a command tag and a value that reads like a footer. In CSV the 1-column
// results ran together, so Ann was masked under the \echo line's "header" (#66).
const psqlScriptHTML = "names\n" +
	"<table border=\"1\">\n  <tr>\n    <th align=\"center\">first_name</th>\n  </tr>\n" +
	"  <tr valign=\"top\">\n    <td align=\"left\">Ann</td>\n  </tr>\n</table>\n<p>(1 row)<br />\n</p>\n" +
	"<table border=\"1\">\n  <tr>\n    <th align=\"center\">id</th>\n  </tr>\n" +
	"  <tr valign=\"top\">\n    <td align=\"right\">1</td>\n  </tr>\n</table>\n<p>(1 row)<br />\n</p>\n" +
	"<table border=\"1\">\n  <tr>\n    <th align=\"center\">last_name</th>\n  </tr>\n</table>\n<p>(0 rows)<br />\n</p>\n" +
	"<p>INSERT 0 1</p>\n" +
	"<table border=\"1\">\n  <tr>\n    <th align=\"center\">tricky</th>\n  </tr>\n" +
	"  <tr valign=\"top\">\n    <td align=\"left\">(1 row)</td>\n  </tr>\n</table>\n<p>(1 row)<br />\n</p>\n"

func TestRedactPsqlHTMLOneTablePerResult(t *testing.T) {
	got := redactWith(t, psqlScriptHTML, "--in=html", "--to=aligned", "--footer")
	// Widths count the real 20-character tag; tagsOut shortens it after rendering.
	want := "names\n" +
		" first_name\n" +
		"----------------------\n" +
		" [person_name:T]\n" +
		"(1 row)\n" +
		" id\n" +
		"----\n" +
		" 1\n" +
		"(1 row)\n" +
		" last_name\n" +
		"-----------\n" +
		"(0 rows)\n" +
		"INSERT 0 1\n" +
		" tricky\n" +
		"---------\n" +
		" (1 row)\n" +
		"(1 row)\n"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestRedactPsqlHTMLEscapesTitleAndExpanded(t *testing.T) {
	// Captured from psql 18.6: escaped values, NULL and '', \echo '', a \pset title and
	// \x on. The expanded record comes back as a normal row, masked by its column name.
	in := "<table border=\"1\">\n  <tr>\n" +
		"    <th align=\"center\">note</th>\n    <th align=\"center\">html</th>\n    <th align=\"center\">n</th>\n" +
		"    <th align=\"center\">e</th>\n    <th align=\"center\">lit</th>\n    <th align=\"center\">lead</th>\n" +
		"    <th align=\"center\">mid</th>\n    <th align=\"center\">quo</th>\n  </tr>\n" +
		"  <tr valign=\"top\">\n    <td align=\"left\">two<br />\nlines</td>\n" +
		"    <td align=\"left\">&lt;b&gt;&amp;x</td>\n    <td align=\"left\">&nbsp; </td>\n" +
		"    <td align=\"left\">&nbsp; </td>\n    <td align=\"left\">&amp;lt;</td>\n" +
		"    <td align=\"left\">&nbsp;&nbsp;x</td>\n    <td align=\"left\">a  b</td>\n" +
		"    <td align=\"left\">&quot;q&quot;</td>\n  </tr>\n</table>\n<p>(1 row)<br />\n</p>\n" +
		"\n" +
		"Title is \"My title\".\n" +
		"<table border=\"1\">\n  <caption>My title</caption>\n  <tr>\n    <th align=\"center\">a</th>\n  </tr>\n" +
		"  <tr valign=\"top\">\n    <td align=\"right\">1</td>\n  </tr>\n</table>\n<p>(1 row)<br />\n</p>\n" +
		"Title is unset.\n" +
		"Expanded display is on.\n" +
		"<table border=\"1\">\n\n  <tr><td colspan=\"2\" align=\"center\">Record 1</td></tr>\n" +
		"  <tr valign=\"top\">\n    <th>first_name</th>\n    <td align=\"left\">Ann</td>\n  </tr>\n" +
		"  <tr valign=\"top\">\n    <th>id</th>\n    <td align=\"right\">2</td>\n  </tr>\n</table>\n\n"
	got := redactWith(t, in, "--in=html", "--to=csv")
	want := "note,html,n,e,lit,lead,mid,quo\n" +
		"\"two\nlines\",<b>&x,,,&lt;,\"  x\",a  b,\"\"\"q\"\"\"\n" +
		"\n" +
		"Title is \"My title\".\n" +
		"My title\n" +
		"a\n1\n" +
		"Title is unset.\n" +
		"Expanded display is on.\n" +
		"first_name,id\n[person_name:T],2\n" +
		"\n"
	if got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

func TestRedactRestoresDuckdbFormats(t *testing.T) {
	// duckdb -json prints one document per statement.
	jsonIn := `[{"id":1,"email":null,"s":"a|b"},` + "\n" + `{"id":22,"email":"x@y.org","s":"y"}]` + "\n" + `[{"first_name":"Ann"}]` + "\n"
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"-json kept, every statement masked", []string{"--in=json"},
			`[{"id":1,"email":null,"s":"a|b"},` + "\n" + `{"id":22,"email":"[email:T]","s":"y"}]` + "\n" + `[{"first_name":"[person_name:T]"}]` + "\n"},
		{"-csv", []string{"--in=json", "--to=csv"}, "id,email,s\n1,NULL,a|b\n22,[email:T],y\nfirst_name\n[person_name:T]\n"},
		{"-list -noheader", []string{"--in=json", "--to=unaligned", "--no-header"}, "1|NULL|a|b\n22|[email:T]|y\n[person_name:T]\n"},
		{"-line", []string{"--in=json", "--to=line"},
			"   id = 1\nemail = NULL\n    s = a|b\n\n   id = 22\nemail = [email:T]\n    s = y\n\nfirst_name = [person_name:T]\n"},
		{"-markdown", []string{"--in=json", "--to=markdown"},
			"| id | email | s |\n|---|---|---|\n| 1 | NULL | a\\|b |\n| 22 | [email:T] | y |\n\n| first_name |\n|---|\n| [person_name:T] |\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := redactWith(t, jsonIn, tc.args...); got != tc.want {
				t.Errorf("got %q\nwant %q", got, tc.want)
			}
		})
	}
}

func TestRedactJSONLinesAndFormattedRecords(t *testing.T) {
	got := redactWith(t, `{"id":1,"email":"x@y.org"}`+"\n"+`{"id":2,"last_name":"Lee"}`+"\n", "--in=jsonl")
	if got != `{"id":1,"email":"[email:T]"}`+"\n"+`{"id":2,"last_name":"[person_name:T]"}`+"\n" {
		t.Errorf("jsonl: %q", got)
	}
	got = redactWith(t, `{"formattedRecords":"[{\"id\":1,\"first_name\":\"Ann\"}]","numberOfRecordsUpdated":0}`, "--in=json")
	if strings.Contains(got, "Ann") || !strings.Contains(got, `\"first_name\":\"[person_name:T]\"`) {
		t.Errorf("formattedRecords: %s", got)
	}
	if got := redactWith(t, "", "--in=json", "--to=csv"); got != "" {
		t.Errorf("empty input: %q", got)
	}
}
