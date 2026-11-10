package main

// redact --pii: mask personal data in SQL output before Claude, the transcript or a memory tool
// see it. Cells in PII-named columns (pii-columns.json rules, matched on CSV headers, JSON
// keys, rds-data columnMetadata and Athena ColumnInfo) become [category:tag], and email and
// phone shapes are masked in every string. tag = 6 hex of a keyed sha256 of the value, so equal values
// keep equal tags and joins still read. Filter: `redact --pii [--in=csv|json|text] < out`.
// Exec: `redact --pii [--in=...] -- psql ...` runs the command, masks its stdout and stderr,
// and exits with its code (no shell PIPESTATUS needed). pre-tool-use rewrites SQL client
// commands into the exec form.

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

//go:embed pii-columns.json
var piiColumnsJSON []byte

// piiExtraTier2 are the tier-2 rules masked by default: identifiers that point at one person
// (policy/claim numbers, VINs) and demographics. Coarse location and staff user ids stay.
var piiExtraTier2 = map[string]bool{"policy_claim_number": true, "vin": true, "demographic": true}

type piiRule struct {
	category         string
	pattern, exclude *regexp.Regexp
}

type piiRuleSet struct {
	rules    []piiRule
	explicit map[string]bool
}

// loadPIIRules is the rule set every masking path uses. See piirules.go for where it
// comes from.
func loadPIIRules() *piiRuleSet {
	rs, _, _ := loadPIIRulesSource()
	return rs
}

// parsePIIRules compiles a pii-columns.json document into a rule set. It is strict where
// the embedded copy is trusted: a bad regex or a category the embedded rules never use is
// an error, so a user file that cannot be fully applied is rejected whole.
func parsePIIRules(data []byte) (*piiRuleSet, error) {
	var f struct {
		Rules []struct {
			Name, Category, Pattern string
			Tier                    int
			Exclude                 *string
		}
		ExplicitColumns []string `json:"explicit_columns"`
	}
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("pii-columns.json: %w", err)
	}
	rs := &piiRuleSet{explicit: map[string]bool{}}
	for i, r := range f.Rules {
		if r.Tier != 1 && !piiExtraTier2[r.Name] {
			continue
		}
		pr := piiRule{category: r.Category}
		var err error
		if pr.pattern, err = regexp.Compile("^(?:" + r.Pattern + ")$"); err != nil {
			return nil, fmt.Errorf("pii-columns.json: rule %d (%q): bad pattern: %w", i, r.Name, err)
		}
		if r.Exclude != nil && *r.Exclude != "" {
			if pr.exclude, err = regexp.Compile("^(?:" + *r.Exclude + ")$"); err != nil {
				return nil, fmt.Errorf("pii-columns.json: rule %d (%q): bad exclude: %w", i, r.Name, err)
			}
		}
		rs.rules = append(rs.rules, pr)
	}
	for _, c := range f.ExplicitColumns {
		rs.explicit[c] = true
	}
	return rs, nil
}

var (
	camelRE       = regexp.MustCompile(`([a-z0-9])([A-Z])`)
	trailDigitsRE = regexp.MustCompile(`_?[0-9]+$`)
)

// category is the PII category of a column name, or "" when it is not masked. Names are
// tried as written (lowercased), as snake_case, and without a trailing number (email2).
func (r *piiRuleSet) category(col string) string {
	col = strings.TrimSpace(col)
	snake := strings.ToLower(camelRE.ReplaceAllString(col, "${1}_$2"))
	for _, n := range []string{strings.ToLower(col), snake, trailDigitsRE.ReplaceAllString(snake, "")} {
		if r.explicit[n] {
			return "key"
		}
		for _, pr := range r.rules {
			if pr.pattern.MatchString(n) && (pr.exclude == nil || !pr.exclude.MatchString(n)) {
				return pr.category
			}
		}
	}
	return ""
}

var (
	emailRE = regexp.MustCompile(`(?i)[a-z0-9._%+-]+@[a-z0-9-]+(?:\.[a-z0-9-]+)*\.[a-z]{2,}\b`)
	phoneRE = regexp.MustCompile(`(?:\+?1[\s.-]?)?(?:\(\d{3}\)\s?|\b\d{3}[\s.-])\d{3}[\s.-]\d{4}\b`)
)

type redactor struct {
	rules *piiRuleSet
	key   []byte
	allow map[string]bool
}

func newRedactor(key []byte, allow []string) *redactor {
	x := &redactor{rules: loadPIIRules(), key: key, allow: map[string]bool{}}
	for _, a := range allow {
		if a = strings.ToLower(strings.TrimSpace(a)); a != "" {
			x.allow[a] = true
		}
	}
	return x
}

// tag is [category:6 hex of sha256(hex(key) US category US value)], US = \x1f. A keyed hash
// DuckDB's sha256() can compute too, so copies made by a separate masking tool carry the same tags.
func (x *redactor) tag(category, value string) string {
	sum := sha256.Sum256([]byte(hex.EncodeToString(x.key) + "\x1f" + category + "\x1f" + value))
	return "[" + category + ":" + hex.EncodeToString(sum[:3]) + "]"
}

// maskValues masks email and phone shapes anywhere in s, except allowlisted addresses.
func (x *redactor) maskValues(s string) string {
	s = emailRE.ReplaceAllStringFunc(s, func(e string) string {
		if x.allow[strings.ToLower(e)] {
			return e
		}
		return x.tag("email", strings.ToLower(e))
	})
	return phoneRE.ReplaceAllStringFunc(s, func(p string) string {
		return x.tag("phone", strings.Map(func(r rune) rune {
			if r >= '0' && r <= '9' {
				return r
			}
			return -1
		}, p))
	})
}

// quotedRE finds what error messages quote: 'x' and "x" (duckdb, psql) and the values of
// psql's Key (cols)=(values).
var quotedRE = regexp.MustCompile(`'([^'\n]{1,200})'|"([^"\n]{1,200})"|\)=\(([^)\n]{1,200})\)`)

// maskErrors masks the quoted text of error messages ("Could not convert string 'Ann'")
// unless it comes from the command itself (known), so names the command used stay
// readable. Quoted URLs and absolute paths stay readable too, unless they hold "@" (user:pass
// or an email), "%" (an encoded "@" or name) or a query, fragment or "key=value" part.
// Hints such as `Did you mean "persons"` are masked: they can't be told apart from values.
func (x *redactor) maskErrors(s, known string) string {
	s = quotedRE.ReplaceAllStringFunc(s, func(m string) string {
		sub := quotedRE.FindStringSubmatchIndex(m)
		for g := 1; g <= 3; g++ {
			if a, b := sub[2*g], sub[2*g+1]; a >= 0 {
				if v := m[a:b]; !strings.Contains(known, v) && !(g <= 2 && isLocation(v)) {
					return m[:a] + x.tag("value", v) + m[b:]
				}
			}
		}
		return m
	})
	return x.maskValues(s)
}

// isLocation reports whether a quoted error value is a URL or an absolute path with no
// "@", "%", "?", "#" or "=" in it.
func isLocation(v string) bool {
	return !strings.ContainsAny(v, "@%?#=") && (strings.Contains(v, "://") || strings.HasPrefix(v, "/"))
}

// maskCell masks one cell of a PII column; empty and NULL cells stay as they are, so
// missing values still read (psql --csv writes NULL as empty, duckdb -csv as NULL).
func (x *redactor) maskCell(category, v string) string {
	if v == "" || v == "NULL" {
		return v
	}
	if category == "date_ym" {
		if ym, ok := yearMonth(v); ok {
			return ym
		}
		category = "dob_age" // not a real ISO date: fail closed with the full mask
	}
	return x.tag(category, v)
}

// isoDateRE is an ISO date with an optional time part; yearMonthSQL in piimirror.go uses the
// same expression.
const isoDateRE = `^([1-9][0-9]{3})-([0-9]{2})-([0-9]{2})(?:[T ][0-9]{2}:[0-9]{2}(?::[0-9]{2}(?:\.[0-9]+)?)?(?:Z|[+-][0-9]{2}(?::?[0-9]{2})?)?)?$`

var isoDate = regexp.MustCompile(isoDateRE)

// yearMonth generalizes an ISO date or timestamp to YYYY-MM, dropping the day and time.
// ok=false for anything else, including impossible dates (2027-02-30).
func yearMonth(v string) (string, bool) {
	m := isoDate.FindStringSubmatch(strings.TrimSpace(v))
	if m == nil {
		return "", false
	}
	y, _ := strconv.Atoi(m[1])
	mo, _ := strconv.Atoi(m[2])
	d, _ := strconv.Atoi(m[3])
	if t := time.Date(y, time.Month(mo), d, 0, 0, 0, 0, time.UTC); mo < 1 || mo > 12 || t.Day() != d {
		return "", false
	}
	return m[1] + "-" + m[2], true
}

// redactCSV masks a CSV result whose first row is the header. Unparseable input falls back
// to value masking.
func (x *redactor) redactCSV(in []byte) []byte { return x.redactOutput(redactOpts{in: "csv"}, in) }

// redactJSON masks a stream of JSON documents (duckdb -json prints one per statement,
// -jsonlines one per row): duckdb rows, aws rds-data and Athena results, and any other JSON
// by key name. ok=false when in is not JSON.
func (x *redactor) redactJSON(in []byte) ([]byte, bool) {
	docs, ok := x.maskJSONDocs(in)
	if !ok {
		return nil, false
	}
	var b bytes.Buffer
	for _, d := range docs {
		encodeDoc(&b, d)
	}
	return b.Bytes(), true
}

func (x *redactor) maskJSONDocs(in []byte) ([]any, bool) {
	docs, err := decodeOrderedStream(in)
	if err != nil {
		return nil, false
	}
	for i := range docs {
		docs[i] = x.maskDoc(docs[i])
	}
	return docs, true
}

func (x *redactor) maskDoc(v any) any {
	byKey := true // positional shapes carry column names as values (name, Name): mask by position
	if obj, isObj := v.(*ordObject); isObj {
		switch {
		case obj.get("records") != nil:
			x.maskRDSData(obj)
			byKey = false
		case obj.get("ResultSet") != nil:
			x.maskAthena(obj)
			byKey = false
		}
		// rds-data --format-records-as JSON: the rows are a JSON string.
		if fr, ok := obj.get("formattedRecords").(string); ok {
			if out, ok := x.redactJSON([]byte(fr)); ok {
				obj.set("formattedRecords", string(bytes.TrimSpace(out)))
			}
		}
	}
	return x.walk(v, byKey)
}

// encodeDoc writes one document the way duckdb -json does: an array of rows one per line,
// anything else compact on one line.
func encodeDoc(b *bytes.Buffer, d any) {
	if rows, ok := d.([]any); ok {
		b.WriteByte('[')
		for i, r := range rows {
			if i > 0 {
				b.WriteString(",\n")
			}
			encodeOrdered(b, r)
		}
		b.WriteString("]\n")
		return
	}
	encodeOrdered(b, d)
	b.WriteByte('\n')
}

// walk masks email/phone shapes in every string and, when byKey, values under PII-named keys.
func (x *redactor) walk(v any, byKey bool) any {
	switch t := v.(type) {
	case string:
		return x.maskValues(t)
	case []any:
		for i := range t {
			t[i] = x.walk(t[i], byKey)
		}
	case *ordObject:
		for i, k := range t.keys {
			if cat := x.rules.category(k); byKey && cat != "" && t.vals[i] != nil {
				if s, ok := scalarText(t.vals[i]); ok {
					t.vals[i] = x.maskCell(cat, s)
					continue
				}
			}
			t.vals[i] = x.walk(t.vals[i], byKey)
		}
	}
	return v
}

// maskRDSData masks the PII columns of rds-data records, using columnMetadata names.
func (x *redactor) maskRDSData(obj *ordObject) {
	meta, _ := obj.get("columnMetadata").([]any)
	cats := make([]string, len(meta))
	for i, m := range meta {
		if mo, ok := m.(*ordObject); ok {
			name, _ := mo.get("name").(string)
			cats[i] = x.rules.category(name)
		}
	}
	recs, _ := obj.get("records").([]any)
	for _, rec := range recs {
		fields, _ := rec.([]any)
		for i, f := range fields {
			fo, ok := f.(*ordObject)
			if !ok || i >= len(cats) || cats[i] == "" || len(fo.keys) != 1 || fo.keys[0] == "isNull" {
				continue
			}
			if s, ok := scalarText(fo.vals[0]); ok {
				fields[i] = &ordObject{keys: []string{"stringValue"}, vals: []any{x.maskCell(cats[i], s)}}
			}
		}
	}
}

// maskAthena masks the PII columns of get-query-results rows. Row 0 is the header row when
// its values equal the ColumnInfo names, and is kept.
func (x *redactor) maskAthena(obj *ordObject) {
	rs, _ := obj.get("ResultSet").(*ordObject)
	if rs == nil {
		return
	}
	var names []string
	if md, _ := rs.get("ResultSetMetadata").(*ordObject); md != nil {
		ci, _ := md.get("ColumnInfo").([]any)
		for _, c := range ci {
			n := ""
			if co, ok := c.(*ordObject); ok {
				n, _ = co.get("Name").(string)
			}
			names = append(names, n)
		}
	}
	rows, _ := rs.get("Rows").([]any)
	for ri, row := range rows {
		ro, _ := row.(*ordObject)
		if ro == nil {
			continue
		}
		data, _ := ro.get("Data").([]any)
		header := ri == 0 && len(data) == len(names)
		for i, d := range data {
			do, _ := d.(*ordObject)
			if header && do != nil && do.get("VarCharValue") != names[i] {
				header = false
			}
		}
		if header {
			continue
		}
		for i, d := range data {
			do, _ := d.(*ordObject)
			if do == nil || i >= len(names) {
				continue
			}
			if cat := x.rules.category(names[i]); cat != "" {
				if s, ok := do.get("VarCharValue").(string); ok {
					do.set("VarCharValue", x.maskCell(cat, s))
				}
			}
		}
	}
}

func scalarText(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	case json.Number:
		return t.String(), true
	case bool:
		return fmt.Sprint(t), true
	}
	return "", false
}

func runRedact(x *redactor, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	pii, secrets, private, o := false, false, false, redactOpts{in: "auto"}
	var argv []string
	for i, a := range args {
		switch {
		case a == "--pii":
			pii = true
		case a == "--secrets":
			secrets = true
		case a == "--private":
			private = true
		case strings.HasPrefix(a, "--in="):
			o.in = strings.TrimPrefix(a, "--in=")
		case strings.HasPrefix(a, "--to="):
			o.to = strings.TrimPrefix(a, "--to=")
		case strings.HasPrefix(a, "--sep="):
			o.sep = strings.TrimPrefix(a, "--sep=")
		case a == "--no-header":
			o.noHeader = true
		case a == "--footer":
			o.footer = true
		case a == "--cap":
			o.cap = true
		case a == "--":
			argv = args[i+1:]
		default:
			fmt.Fprintln(stderr, "redact: unknown argument "+a)
			return 2
		}
		if argv != nil {
			break
		}
	}
	if !pii && !secrets {
		fmt.Fprintln(stderr, "usage: guardrails redact --pii|--secrets [--in=csv|json|jsonl|text|auto] [--to=csv|unaligned|aligned|line|markdown|json] [--sep=S] [--no-header] [--footer] [--cap] [--private] [-- command args...]")
		return 2
	}
	mask := func(b []byte) []byte {
		if pii {
			func() {
				// A formatting bug must not lose the command's output (#64): mask it as text.
				defer func() {
					if p := recover(); p != nil {
						fmt.Fprintf(stderr, "redact: could not format the output (%v), so it is masked as plain text\n", p)
						b = []byte(x.maskValues(string(b)))
					}
				}()
				b = x.redactOutput(o, b)
			}()
		}
		if secrets {
			b = []byte(x.maskSecrets(string(b)))
		}
		if private {
			b = privateWrap(b)
		}
		return b
	}
	if argv == nil {
		in, err := io.ReadAll(stdin)
		if err != nil {
			fmt.Fprintln(stderr, "redact:", err)
			return 1
		}
		stdout.Write(mask(in))
		return 0
	}
	if len(argv) == 0 {
		fmt.Fprintln(stderr, "redact: no command after --")
		return 2
	}
	if filepath.Base(argv[0]) == "duckdb" {
		var dot string
		if dot, stdin = duckdbHiddenDot(argv, stdin); dot != "" {
			fmt.Fprintln(stderr, duckdbDotMsg(dot))
			return 2
		}
	}
	var out, errb bytes.Buffer
	c := exec.Command(argv[0], argv[1:]...)
	c.Stdin, c.Stdout, c.Stderr = stdin, &out, &errb
	err := c.Run()
	stdout.Write(mask(out.Bytes()))
	e := errb.String()
	if pii {
		e = x.maskErrors(e, strings.Join(argv, " "))
	}
	if secrets {
		e = x.maskSecrets(e)
	}
	stderr.Write([]byte(e))
	var ee *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &ee):
		return ee.ExitCode()
	default:
		fmt.Fprintln(stderr, "redact:", err)
		return 127
	}
}

// piiKey returns the local tag key, creating it (0600) on first use.
func piiKey() []byte {
	p := userPath(stateBase(), "pii-key")
	if k, err := os.ReadFile(p); err == nil && len(k) >= 16 {
		return k
	}
	k := make([]byte, 32)
	rand.Read(k)
	if os.MkdirAll(filepath.Dir(p), 0o700) == nil {
		os.WriteFile(p, k, 0o600)
	}
	return k
}

// piiAllow is the user's own addresses: git user.email plus GUARDRAILS_PII_ALLOW (or
// CLAUDE_TOOLS_PII_ALLOW), comma-separated.
func piiAllow() []string {
	allow := strings.Split(settingEnv("PII_ALLOW"), ",")
	if out, err := exec.Command("git", "config", "--global", "user.email").Output(); err == nil {
		allow = append(allow, strings.TrimSpace(string(out)))
	}
	return allow
}

func redactCmd(args []string) int {
	return runRedact(newRedactor(piiKey(), piiAllow()), args, os.Stdin, os.Stdout, os.Stderr)
}
