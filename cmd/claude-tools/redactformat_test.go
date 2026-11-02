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
