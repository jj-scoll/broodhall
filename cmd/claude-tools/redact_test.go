package main

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

func testRedactor() *redactor {
	return newRedactor([]byte("test-key"), []string{"me@corp.example"})
}

func TestPIIColumnRules(t *testing.T) {
	rules := loadPIIRules()
	for _, c := range []string{"email", "emailaddress1", "EmailAddress2", "first_name", "firstName",
		"phone", "postal_code", "dob", "sin", "policy_number", "vin", "gender", "driver_id", "name"} {
		if rules.category(c) == "" {
			t.Errorf("%q not masked", c)
		}
	}
	for _, c := range []string{"email_enabled", "phone_type", "id", "created_at", "status", "count",
		"user_id", "city", "table_name", "tool", "ts", "column_name", "schema_name", "file_name",
		"data_type", "description", "key", "value", "type", "total", "amount", "premium", "rule_name"} {
		if cat := rules.category(c); cat != "" {
			t.Errorf("%q masked as %s", c, cat)
		}
	}
}

var tagRE = regexp.MustCompile(`\[(email|phone):[0-9a-f]{6}\]`)

func TestMaskValues(t *testing.T) {
	x := testRedactor()
	for _, in := range []string{"bob@example.org", "Bob.Smith+x@Mail.Example.CO.uk"} {
		if got := x.maskValues("contact " + in + " now"); !tagRE.MatchString(got) || strings.Contains(got, in) {
			t.Errorf("email %q -> %q", in, got)
		}
	}
	for _, in := range []string{"(416) 555-0199", "416-555-0199", "416.555.0199", "+1 416 555 0199", "1-416-555-0199"} {
		if got := x.maskValues("tel " + in + ";"); !regexp.MustCompile(`^tel \[phone:[0-9a-f]{6}\];$`).MatchString(got) {
			t.Errorf("phone %q -> %q", in, got)
		}
	}
	for _, keep := range []string{"me@corp.example", "1793165763", "2026-10-28", "12:34:56", "10.0.0.1",
		"v1.2.3", "id 4165550199", "a@b"} {
		if got := x.maskValues(keep); got != keep {
			t.Errorf("kept value %q changed to %q", keep, got)
		}
	}
	a, b := x.maskValues("x@y.org"), x.maskValues("x@y.org")
	if a != b || a == x.maskValues("z@y.org") {
		t.Errorf("tags not stable per value: %q %q", a, b)
	}
	if a == newRedactor([]byte("other"), nil).maskValues("x@y.org") {
		t.Errorf("tag does not depend on the key")
	}
}

func TestRedactCSV(t *testing.T) {
	in := "id,EmailAddress1,first_name,note,city\n" +
		"1,ann@corp.example,Ann,\"call 416-555-0199, ok\",Toronto\n" +
		"2,,\"Smith, Jo\",,\n" +
		"3,NULL,NULL,,\n" // duckdb -csv writes NULL; psql --csv leaves the cell empty
	out := string(testRedactor().redactCSV([]byte(in)))
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 4 || lines[0] != "id,EmailAddress1,first_name,note,city" || lines[3] != "3,NULL,NULL,," {
		t.Fatalf("header or row count changed:\n%s", out)
	}
	for _, leak := range []string{"ann@corp", "Ann,", "Smith", "416-555"} {
		if strings.Contains(out, leak) {
			t.Errorf("leaked %q:\n%s", leak, out)
		}
	}
	for _, want := range []string{"Toronto", "[email:", "[person_name:", "[phone:"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	if !strings.HasPrefix(lines[2], "2,,[person_name:") || !strings.HasSuffix(lines[2], ",,") {
		t.Errorf("empty cells must stay empty: %q", lines[2])
	}
}

func redactJSONString(t *testing.T, in string) string {
	t.Helper()
	out, ok := testRedactor().redactJSON([]byte(in))
	if !ok {
		t.Fatalf("not handled as JSON: %s", in)
	}
	var v any
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	return string(out)
}

func TestRedactJSONShapes(t *testing.T) {
	tests := []struct {
		name, in     string
		leaks, keeps []string
	}{
		{"duckdb -json rows",
			`[{"id":1,"email":"a@b.co","name":"Xavier","age":41,"memo":"ring 416-555-0199","city":"Ottawa","dob":null}]`,
			[]string{"a@b.co", "Xavier", `"age":41`, "416-555"}, []string{"Ottawa", `"dob":null`, `{"id":1,"email":`}},
		{"rds-data with metadata",
			`{"columnMetadata":[{"name":"id"},{"name":"last_name"}],"records":[[{"longValue":7},{"stringValue":"Nguyen"}]],"numberOfRecordsUpdated":0}`,
			[]string{"Nguyen"}, []string{`"name":"last_name"`, `"longValue":7`}},
		{"rds-data without metadata still value-masks",
			`{"records":[[{"stringValue":"q@r.example"}]]}`,
			[]string{"q@r.example"}, nil},
		{"athena get-query-results",
			`{"ResultSet":{"Rows":[{"Data":[{"VarCharValue":"id"},{"VarCharValue":"postal_code"}]},{"Data":[{"VarCharValue":"9"},{"VarCharValue":"M5V 2T6"}]}],"ResultSetMetadata":{"ColumnInfo":[{"Name":"id"},{"Name":"postal_code"}]}}}`,
			[]string{"M5V 2T6"}, []string{`"VarCharValue":"postal_code"`, `"Name":"postal_code"`, `"VarCharValue":"9"`}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out := redactJSONString(t, tc.in)
			for _, l := range tc.leaks {
				if strings.Contains(out, l) {
					t.Errorf("leaked %q: %s", l, out)
				}
			}
			for _, k := range tc.keeps {
				if !strings.Contains(out, k) {
					t.Errorf("lost %q: %s", k, out)
				}
			}
		})
	}
	if _, ok := testRedactor().redactJSON([]byte("id|email\n1|x")); ok {
		t.Errorf("plain text accepted as JSON")
	}
}

func TestRedactExecMasksBothStreamsAndKeepsExit(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runRedact(testRedactor(), []string{"--pii", "--in=csv", "--", "sh", "-c",
		`printf 'id,email\n1,x@y.org\n'; echo 'ERROR: Key (email)=(dup@y.org) exists' >&2; exit 3`},
		strings.NewReader(""), &stdout, &stderr)
	if code != 3 {
		t.Errorf("exit = %d, want 3", code)
	}
	if strings.Contains(stdout.String(), "x@y.org") || !strings.HasPrefix(stdout.String(), "id,email\n1,[email:") {
		t.Errorf("stdout: %q", stdout.String())
	}
	if strings.Contains(stderr.String(), "dup@y.org") || !strings.Contains(stderr.String(), "ERROR: Key (email)=([email:") {
		t.Errorf("stderr: %q", stderr.String())
	}
}

func TestRedactFilterAutoDetects(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := runRedact(testRedactor(), []string{"--pii"}, strings.NewReader(`[{"email":"x@y.org"}]`), &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if strings.Contains(stdout.String(), "x@y.org") || !strings.Contains(stdout.String(), `"email":"[email:`) {
		t.Errorf("json filter: %q", stdout.String())
	}
	stdout.Reset()
	runRedact(testRedactor(), []string{"--pii"}, strings.NewReader("1|Ann|ann@x.example\n"), &stdout, &stderr)
	if got := stdout.String(); strings.Contains(got, "ann@x.example") || !strings.HasPrefix(got, "1|Ann|[email:") {
		t.Errorf("text filter: %q", got)
	}
}

// date_ym cells keep YYYY-MM only; anything that is not a real ISO date stays fully masked
// (dob_age tag), empty and NULL stay as they are.
func TestMaskCellDateYearMonth(t *testing.T) {
	x := testRedactor()
	tests := []struct{ name, in, want string }{
		{"iso date", "2027-05-14", "2027-05"},
		{"timestamp space", "2027-05-14 10:22:00.000", "2027-05"},
		{"timestamp T zone", "2027-05-14T10:22:00Z", "2027-05"},
		{"timestamp offset", "2027-05-14T23:59:59+05:30", "2027-05"},
		{"timestamp no seconds", "2027-05-14 10:22", "2027-05"},
		{"leap day", "2028-02-29", "2028-02"},
		{"padded", " 2027-05-14 ", "2027-05"},
		{"garbage", "not a date", x.tag("dob_age", "not a date")},
		{"us format", "05/14/2027", x.tag("dob_age", "05/14/2027")},
		{"impossible day", "2027-02-30", x.tag("dob_age", "2027-02-30")},
		{"impossible month", "2027-13-01", x.tag("dob_age", "2027-13-01")},
		{"non-leap", "2027-02-29", x.tag("dob_age", "2027-02-29")},
		{"year only", "2027", x.tag("dob_age", "2027")},
		{"date plus junk", "2027-05-14 and Ann", x.tag("dob_age", "2027-05-14 and Ann")},
		{"empty", "", ""},
		{"NULL text", "NULL", "NULL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := x.maskCell("date_ym", tt.in); got != tt.want {
				t.Errorf("maskCell(date_ym, %q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
