package pytext

// Expected values below were produced by CPython 3 (bytes.decode, repr, json.dumps, float
// repr), which this package reproduces.

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/dlclark/regexp2"
)

func mustDecode(t *testing.T, s string) any {
	t.Helper()
	v, err := DecodeJSON([]byte(s))
	if err != nil {
		t.Fatalf("DecodeJSON(%s): %v", s, err)
	}
	return v
}

func TestDecodeJSON(t *testing.T) {
	tests := []struct {
		name, in string
		wantErr  bool
	}{
		{"object", `{"a": 1}`, false},
		{"array", `[1, "x", null]`, false},
		{"scalar", `7`, false},
		{"empty", ``, true},
		{"extra data", `{} {}`, true},
		{"truncated object", `{"a": `, true},
		{"truncated array", `[1, `, true},
		{"nested error", `{"a": [1, }`, true},
		{"bad token", `{a: 1}`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := DecodeJSON([]byte(tt.in))
			if (err != nil) != tt.wantErr {
				t.Errorf("DecodeJSON(%q) error = %v, wantErr %v", tt.in, err, tt.wantErr)
			}
		})
	}
}

func TestObjKeyOrderAndDuplicates(t *testing.T) {
	o := mustDecode(t, `{"b": 1, "a": 2, "b": 3}`).(*Obj)
	if want := []string{"b", "a"}; !reflect.DeepEqual(o.Keys, want) {
		t.Errorf("Keys = %v, want %v (a repeated key keeps its first position)", o.Keys, want)
	}
	if got := Dumps(o); got != `{"b": 3, "a": 2}` {
		t.Errorf("Dumps = %s, want the last value of a repeated key", got)
	}
}

func TestObjAccessors(t *testing.T) {
	o := mustDecode(t, `{"s": "str", "n": 1, "o": {"x": "y"}, "z": null}`).(*Obj)
	var nilObj *Obj
	if got := o.GetStr("s"); got != "str" {
		t.Errorf("GetStr(s) = %q", got)
	}
	if got := o.GetStr("n"); got != "" {
		t.Errorf("GetStr(n) = %q, want empty for a non-string", got)
	}
	if got := o.GetStr("missing"); got != "" {
		t.Errorf("GetStr(missing) = %q", got)
	}
	if got := o.GetObj("o").GetStr("x"); got != "y" {
		t.Errorf("GetObj(o).GetStr(x) = %q", got)
	}
	if o.GetObj("s") != nil || o.GetObj("missing") != nil {
		t.Error("GetObj of a non-object or missing key must be nil")
	}
	if o.GetObj("missing").GetStr("x") != "" {
		t.Error("accessors on a nil *Obj must be safe")
	}
	if v, ok := o.Lookup("z"); !ok || v != nil {
		t.Errorf("Lookup(z) = %v, %v; want nil, true (present with a null value)", v, ok)
	}
	if v, ok := o.Lookup("missing"); ok || v != nil {
		t.Errorf("Lookup(missing) = %v, %v; want nil, false", v, ok)
	}
	if v, ok := nilObj.Lookup("x"); ok || v != nil {
		t.Errorf("nil.Lookup = %v, %v", v, ok)
	}
	if nilObj.Get("x") != nil {
		t.Error("nil.Get must be nil")
	}
}

func TestReadText(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, b []byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	tests := []struct {
		name    string
		data    []byte
		replace bool
		want    string
		wantErr bool
	}{
		{"plain", []byte("a\nb\n"), false, "a\nb\n", false},
		{"crlf and cr", []byte("a\r\nb\rc\n"), false, "a\nb\nc\n", false},
		{"invalid, strict", []byte("a\xffb"), false, "", true},
		{"invalid, replace", []byte("a\xffb"), true, "a�b", false},
		{"invalid with cr, replace", []byte("a\xff\r\nb"), true, "a�\nb", false},
		{"valid utf-8 untouched by replace", []byte("café"), true, "café", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ReadText(write(strings.ReplaceAll(tt.name, " ", "_")+".txt", tt.data), tt.replace)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
	if _, err := ReadText(filepath.Join(dir, "absent"), true); err == nil {
		t.Error("a missing file must be an error")
	}
	if _, err := ReadText(write("bad.txt", []byte("\xff")), false); err == nil || !strings.Contains(err.Error(), "invalid UTF-8") {
		t.Errorf("strict read of invalid UTF-8: error = %v", err)
	}
}

func TestPyReplaceInvalid(t *testing.T) {
	// One U+FFFD per maximal invalid subpart, as bytes.decode("utf-8", "replace").
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"valid", "héllo \U0001F600", "héllo \U0001F600"},
		{"lone invalid byte", "\xff", "�"},
		{"truncated three-byte sequence is one", "a\xe2\x82b", "a�b"},
		{"truncated at end", "\xe1\x80", "�"},
		{"truncated four-byte at end", "ok\xf0\x9f\x98", "ok�"},
		{"surrogate bytes are three", "\xed\xa0\x80", "���"},
		{"overlong four-byte is four", "\xf0\x80\x80\x80", "����"},
		{"above U+10FFFF is four", "\xf4\x90\x80\x80", "����"},
		{"overlong two-byte is two", "\xc0\x80", "��"},
		{"overlong three-byte is three", "\xe0\x80\x80", "���"},
		{"bad continuation keeps the ASCII after it", "\xc3(", "�("},
		{"stray continuation bytes", "\x80\x80", "��"},
		{"valid then invalid", "\U0001F600\xfe", "\U0001F600�"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := pyReplaceInvalid([]byte(tt.in)); got != tt.want {
				t.Errorf("pyReplaceInvalid(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestTruthy(t *testing.T) {
	tests := []struct {
		name string
		in   any
		want bool
	}{
		{"nil", nil, false},
		{"true", true, true},
		{"false", false, false},
		{"empty string", "", false},
		{"string", "x", true},
		{"zero int", mustDecode(t, `0`), false},
		{"zero float", mustDecode(t, `0.0`), false},
		{"negative zero", mustDecode(t, `-0.0`), false},
		{"nonzero", mustDecode(t, `-3`), true},
		{"tiny float", mustDecode(t, `1e-300`), true},
		{"empty array", mustDecode(t, `[]`), false},
		{"array", mustDecode(t, `[0]`), true},
		{"empty object", mustDecode(t, `{}`), false},
		{"object", mustDecode(t, `{"a": 0}`), true},
		{"nil object", (*Obj)(nil), false},
		{"unknown type", 3, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Truthy(tt.in); got != tt.want {
				t.Errorf("Truthy(%#v) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestStringHelpers(t *testing.T) {
	t.Run("Strip", func(t *testing.T) {
		tests := []struct{ in, want string }{
			{"  a b \t\n", "a b"},
			{"\x1c\x1d\x1e\x1f x \x1f", "x"}, // Python's whitespace has \x1c-\x1f; Go's lacks them
			{" x ", "x"},
			{"", ""},
			{" \n ", ""},
		}
		for _, tt := range tests {
			if got := Strip(tt.in); got != tt.want {
				t.Errorf("Strip(%q) = %q, want %q", tt.in, got, tt.want)
			}
		}
	})
	t.Run("Cut counts code points", func(t *testing.T) {
		tests := []struct {
			in   string
			n    int
			want string
		}{
			{"abcdef", 3, "abc"},
			{"abc", 3, "abc"},
			{"abc", 10, "abc"},
			{"abc", 0, ""},
			{"", 2, ""},
			{"hé\U0001F600llo", 3, "hé\U0001F600"},
		}
		for _, tt := range tests {
			if got := Cut(tt.in, tt.n); got != tt.want {
				t.Errorf("Cut(%q, %d) = %q, want %q", tt.in, tt.n, got, tt.want)
			}
		}
	})
	t.Run("RuneLen", func(t *testing.T) {
		if got := RuneLen("hé\U0001F600"); got != 3 {
			t.Errorf("RuneLen = %d, want 3", got)
		}
		if got := RuneLen(""); got != 0 {
			t.Errorf("RuneLen(empty) = %d", got)
		}
	})
	t.Run("Lower", func(t *testing.T) {
		tests := []struct{ in, want string }{
			{"ABC", "abc"},
			{"İ", "i̇"}, // Python lowers capital dotted I to i + combining dot
			{"İstanbul", "i̇stanbul"},
			{"ÉCOLE", "école"},
		}
		for _, tt := range tests {
			if got := Lower(tt.in); got != tt.want {
				t.Errorf("Lower(%q) = %q, want %q", tt.in, got, tt.want)
			}
		}
	})
}

func TestPyNum(t *testing.T) {
	tests := []struct {
		lit      string
		wantRepr string
		wantJSON string
	}{
		{"1.0", "1.0", "1.0"},
		{"1e5", "100000.0", "100000.0"},
		{"1E5", "100000.0", "100000.0"},
		{"-0.0", "-0.0", "-0.0"},
		{"0.0001", "0.0001", "0.0001"},
		{"0.00001", "1e-05", "1e-05"},
		{"1e-5", "1e-05", "1e-05"},
		{"1e-7", "1e-07", "1e-07"},
		{"1e15", "1000000000000000.0", "1000000000000000.0"},
		{"1e16", "1e+16", "1e+16"},
		{"1e22", "1e+22", "1e+22"},
		{"1.5e300", "1.5e+300", "1.5e+300"},
		{"1.23456789012345678", "1.2345678901234567", "1.2345678901234567"},
		{"2.50", "2.5", "2.5"},
		{"100.0", "100.0", "100.0"},
		{"0.1", "0.1", "0.1"},
		{"1e400", "inf", "Infinity"},
		{"-1e400", "-inf", "-Infinity"},
		{"123456789012345678", "123456789012345678", "123456789012345678"}, // ints stay exact
		{"-7", "-7", "-7"},
		{"-0", "0", "0"},
		{"0", "0", "0"},
	}
	for _, tt := range tests {
		t.Run(tt.lit, func(t *testing.T) {
			v := mustDecode(t, tt.lit)
			if got := Repr(v); got != tt.wantRepr {
				t.Errorf("Repr(%s) = %s, want %s", tt.lit, got, tt.wantRepr)
			}
			if got := Dumps(v); got != tt.wantJSON {
				t.Errorf("Dumps(%s) = %s, want %s", tt.lit, got, tt.wantJSON)
			}
		})
	}
}

func TestReprStr(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"plain", "plain", `'plain'`},
		{"single quote switches to double", "it's", `"it's"`},
		{"double quote stays single", `say "hi"`, `'say "hi"'`},
		{"both quotes escape the single", `both ' and "`, `'both \' and "'`},
		{"tab, newline, return", "tab\there\nx\r", `'tab\there\nx\r'`},
		{"backslash", `back\slash`, `'back\\slash'`},
		{"control and delete", "\x00\x1f\x7f", `'\x00\x1f\x7f'`},
		{"printable latin-1 and BMP", "café €", "'café €'"},
		{"non-printable latin-1 as x", " nbsp\u0080­", `'\xa0nbsp\x80\xad'`},
		{"non-printable BMP as u", "\u200b\u2028\ud7ff", `'\u200b\u2028\ud7ff'`},
		{"non-printable astral as U", "\U000e0001", `'\U000e0001'`},
		{"printable astral", "\U0001F600", "'\U0001F600'"},
		{"empty", "", `''`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ReprStr(tt.in); got != tt.want {
				t.Errorf("ReprStr(%q) = %s, want %s", tt.in, got, tt.want)
			}
		})
	}
}

func TestPyDumpsStr(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"plain", "plain", `"plain"`},
		{"quotes and backslash", "say \"hi\" \\", `"say \"hi\" \\"`},
		{"named escapes", "\b\f\n\r\t", `"\b\f\n\r\t"`},
		{"other controls as u", "\x00\x01\x1f\x7f", `"\u0000\u0001\u001f\u007f"`},
		{"non-ASCII as u", "caf\u00e9\u20ac", `"caf\u00e9\u20ac"`},
		{"astral as surrogate pair", "\U0001F600", `"\ud83d\ude00"`},
		{"astral plane 14 pair", "\U000e0001", `"\udb40\udc01"`},
		{"single quote untouched", "it's", `"it's"`},
		{"empty", "", `""`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := pyDumpsStr(tt.in); got != tt.want {
				t.Errorf("pyDumpsStr(%q) = %s, want %s", tt.in, got, tt.want)
			}
		})
	}
}

func TestStrReprDumps(t *testing.T) {
	v := mustDecode(t, `{"a": [1, 2.5, "x", null, true, false, {"k": []}], "b": {}, "a2": 1e5}`)
	if got, want := Repr(v), `{'a': [1, 2.5, 'x', None, True, False, {'k': []}], 'b': {}, 'a2': 100000.0}`; got != want {
		t.Errorf("Repr = %s, want %s", got, want)
	}
	if got, want := Dumps(v), `{"a": [1, 2.5, "x", null, true, false, {"k": []}], "b": {}, "a2": 100000.0}`; got != want {
		t.Errorf("Dumps = %s, want %s", got, want)
	}
	if got := Str("already a str"); got != "already a str" {
		t.Errorf("Str(string) = %q, want it unquoted", got)
	}
	if got := Str(v.(*Obj).Get("b")); got != "{}" {
		t.Errorf("Str(object) = %q", got)
	}
	if got := Str(nil); got != "None" {
		t.Errorf("Str(nil) = %q", got)
	}
	if got := Str(mustDecode(t, `["it's"]`)); got != `["it's"]` {
		t.Errorf("Str(list) = %q", got)
	}
	if Repr(3) != "" || Dumps(3) != "" {
		t.Error("a value json.loads cannot produce renders as empty")
	}
	if got := Dumps(mustDecode(t, `[]`)); got != "[]" {
		t.Errorf("Dumps(empty array) = %q", got)
	}
}

func TestRe(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		opts    regexp2.RegexOptions
		in      string
		want    bool
	}{
		{"\\w matches a letter beyond ASCII", `^\w+$`, 0, "café", true},
		{"\\w matches a digit and underscore", `^\w+$`, 0, "a_1٣", true},
		{"\\w rejects punctuation", `^\w+$`, 0, "a-b", false},
		{"\\W is the complement", `^\W+$`, 0, "-- !", true},
		{"\\d inside a class", `^[\d]+$`, 0, "\u0663", true},
		{"\\d matches any decimal digit", `^\d+$`, 0, "٣٤", true},
		{"\\d rejects a letter", `^\d+$`, 0, "a", false},
		{"\\s includes \\x1c", `^\s$`, 0, "\x1c", true},
		{"\\s in a class includes \\x1f", `^[\s]+$`, 0, " \x1f\t", true},
		{"\\S excludes \\x1d", `^\S$`, 0, "\x1d", false},
		{"\\S matches a printable", `^\S$`, 0, "a", true},
		{"\\b at a word edge", `\bcat\b`, 0, "a cat sat", true},
		{"\\b not inside a word", `\bcat\b`, 0, "concatenate", false},
		{"\\b treats accented letters as word chars", `\bcaf\b`, 0, "café", false},
		{"\\Z is the very end", `a\Z`, 0, "xa", true},
		{"\\Z is not before a trailing newline", `a\Z`, 0, "a\n", false},
		{"literal ] first in a class", `^[]a]+$`, 0, "]a]", true},
		{"literal ] after negation", `^[^]a]+$`, 0, "bc", true},
		{"negated class rejects its members", `^[^]a]+$`, 0, "]", false},
		{"[ inside a class is literal", `^[[a]+$`, 0, "[a", true},
		{"escaped backslash before w is literal", `^\\w$`, 0, `\w`, true},
		{"ignore case", `^abc$`, regexp2.IgnoreCase, "ABC", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Re(tt.pattern, tt.opts).MatchString(tt.in)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("Re(%q).MatchString(%q) = %v, want %v", tt.pattern, tt.in, got, tt.want)
			}
		})
	}
}

func TestReNegatedEscapeInClassPanics(t *testing.T) {
	for _, p := range []string{`[\W]`, `[\S]`, `[\D]`} {
		t.Run(p, func(t *testing.T) {
			defer func() {
				r := recover()
				if r == nil {
					t.Fatalf("Re(%q) did not panic", p)
				}
				if msg, _ := r.(string); !strings.Contains(msg, "negated class escape") {
					t.Errorf("panic = %v", r)
				}
			}()
			Re(p, 0)
		})
	}
}

func TestMatchReAnchorsAtStart(t *testing.T) {
	re := MatchRe(`b+`, 0)
	if Search(re, "abbb") != nil {
		t.Error("MatchRe must not match after the start (re.match)")
	}
	if m := Search(re, "bbba"); m == nil || m.String() != "bbb" {
		t.Errorf("MatchRe(bbba) = %v", m)
	}
	alt := MatchRe(`a|b`, 0)
	if Search(alt, "xb") != nil {
		t.Error("the anchor must bind the whole alternation")
	}
}

func TestSearchFindAllGroupSub(t *testing.T) {
	re := Re(`(\w+)@(\w+)?`, 0)
	if Search(re, "no match here") != nil {
		t.Error("Search with no match must be nil")
	}
	m := Search(re, "mail bob@ x")
	if m == nil {
		t.Fatal("no match")
	}
	if got := Group(m, 1); got != "bob" {
		t.Errorf("Group 1 = %q", got)
	}
	if got := Group(m, 2); got != "" {
		t.Errorf("Group 2 = %q, want empty for a group that did not take part", got)
	}

	all := FindAll(Re(`\d+`, 0), "a1 b22 c333")
	var got []string
	for _, x := range all {
		got = append(got, x.String())
	}
	if want := []string{"1", "22", "333"}; !reflect.DeepEqual(got, want) {
		t.Errorf("FindAll = %v, want %v", got, want)
	}
	if n := len(FindAll(Re(`\d`, 0), "none")); n != 0 {
		t.Errorf("FindAll with no match returned %d", n)
	}

	upper := Sub(Re(`[a-z]+`, 0), "ab-cd", func(m regexp2.Match) string { return strings.ToUpper(m.String()) })
	if upper != "AB-CD" {
		t.Errorf("Sub = %q", upper)
	}
	if got := SubStr(Re(`\s+`, 0), "a \t b\x1cc", "_"); got != "a_b_c" {
		t.Errorf("SubStr = %q", got)
	}
	if got := SubStr(Re(`x`, 0), "abc", "_"); got != "abc" {
		t.Errorf("SubStr without a match = %q", got)
	}
}

func TestSection(t *testing.T) {
	text := "# Title\n## Goal\n  do it  \n\n## Next steps\n1. a\n2. b\n### Sub\nbody\n### Other\nlast"
	tests := []struct {
		name      string
		hashes    string
		title     string
		strip     bool
		titleRest bool
		want      string
	}{
		{"stripped body ends at the next heading", "##", "Goal", true, false, "do it"},
		{"unstripped keeps whitespace", "##", "Goal", false, false, "  do it  \n\n"},
		{"last section runs to the end", "###", "Other", false, false, "last"},
		{"title prefix with rest of line", "##", "Next", true, true, "1. a\n2. b\n### Sub\nbody\n### Other\nlast"},
		{"title prefix without rest does not match", "##", "Next", true, false, ""},
		{"missing section", "##", "Absent", true, false, ""},
		{"wrong depth does not match", "###", "Goal", true, false, ""},
		{"regexp metacharacters in the title are literal", "###", "Sub.*", true, false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Section(text, tt.hashes, tt.title, tt.strip, tt.titleRest); got != tt.want {
				t.Errorf("Section = %q, want %q", got, tt.want)
			}
		})
	}
}
