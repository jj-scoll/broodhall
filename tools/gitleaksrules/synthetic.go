package main

import (
	"math/rand"
	"regexp"
	"strings"

	"github.com/jj-scoll/broodhall/cmd/guardrails/secretrules"
	"github.com/zricethezav/gitleaks/v8/config"
	"github.com/zricethezav/gitleaks/v8/detect"
)

// The default rules use only some of what the library can express: no allowlist of theirs
// has condition "and" or a regexTarget (the rules that do are generic-api-key, which is
// skipped, and the ones that need a path). A newer gitleaks may use them, and the detector
// implements them, so they are proven here against the library on small rule sets built to
// reach every branch: rules with several or optional capture groups, matches that begin or
// end with a newline or span lines, entropy at the threshold and over multi-byte characters,
// keywords that overlap or are absent, and allowlists of every shape, global and per rule.
// The specs are written to the golden file, so the guardrails tests run them without the
// library.

type specAllow struct {
	And       bool     `json:"and,omitempty"`
	Target    string   `json:"target,omitempty"` // "", "match" or "line"
	Regexes   []string `json:"regexes,omitempty"`
	StopWords []string `json:"stopWords,omitempty"`
}

type specRule struct {
	ID       string      `json:"id"`
	Regex    string      `json:"regex"`
	Group    int         `json:"group,omitempty"`
	Entropy  float64     `json:"entropy,omitempty"`
	Keywords []string    `json:"keywords,omitempty"`
	Allow    []specAllow `json:"allow,omitempty"`
}

type synth struct {
	Name   string      `json:"name"`
	Rules  []specRule  `json:"rules"`
	Global []specAllow `json:"global,omitempty"`
}

func synthConfigs() []synth {
	return []synth{
		{Name: "engine", Rules: []specRule{
			{ID: "tok-eq", Regex: `(?i)tok(?:en)?\s*[=:]\s*([a-z0-9]{4,})(?:\s|$)`, Group: 1, Keywords: []string{"tok"}},
			{ID: "first-nonempty", Regex: `(x*)(y+)(z*)`, Keywords: []string{"y"}},
			{ID: "group2-optional", Regex: `L:(\d*)\s(\w*)`, Group: 2, Keywords: []string{"l:"}},
			{ID: "plain", Regex: `SECRET|S3CRET`, Keywords: []string{"secret", "s3cret"}},
			{ID: "plain-overlap", Regex: `ECRE`, Keywords: []string{"ecr", "cre"}},
			{ID: "no-keywords", Regex: `\bq[0-9]{3}\b`},
			{ID: "edge-newline", Regex: `\n?key\s?(=)\s?([a-z]+)\n?`, Group: 2, Keywords: []string{"key"}},
			{ID: "multiline", Regex: `begin(?s:.*?)end`, Keywords: []string{"begin"}},
			{ID: "entropy-eq", Regex: `E:([a-z]{4})`, Entropy: 2, Keywords: []string{"e:"}},
			{ID: "entropy-utf8", Regex: `U:(\S{4})`, Entropy: 1.7, Keywords: []string{"u:"}},
			{ID: "entropy-nogroup", Regex: `M:[a-z]{6}`, Entropy: 2.2, Keywords: []string{"m:"}},
			{ID: "rematch-alone", Regex: `\B(z)q`, Keywords: []string{"zq"}},
			{ID: "upper-keyword", Regex: `(?i)PASSWORD\s*=\s*(\w+)`, Keywords: []string{"password"}},
		}},
		{Name: "allowlists", Rules: []specRule{
			{ID: "al-or", Regex: `A:([a-z0-9]{3,8})`, Keywords: []string{"a:"}, Allow: []specAllow{
				{Regexes: []string{`^foo`, `(?i)^bar$`}, StopWords: []string{"zzz"}}}},
			{ID: "al-and", Regex: `B:([a-z0-9]{3,8})`, Keywords: []string{"b:"}, Allow: []specAllow{
				{And: true, Regexes: []string{`^x`}, StopWords: []string{"ab"}}}},
			{ID: "al-and-regex-only", Regex: `B2:([a-z0-9]{3,8})`, Keywords: []string{"b2:"}, Allow: []specAllow{
				{And: true, Regexes: []string{`^x`, `9$`}}}},
			{ID: "al-match", Regex: `C:([a-z0-9]{3,8})\n?`, Keywords: []string{"c:"}, Allow: []specAllow{
				{Target: "match", Regexes: []string{`C:z`, `\n$`}}}},
			{ID: "al-line", Regex: `D:([a-z0-9]{3,8})`, Keywords: []string{"d:"}, Allow: []specAllow{
				{Target: "line", Regexes: []string{`^#`, `\n#`, `skip`}},
				{And: true, Target: "line", Regexes: []string{`ok`}, StopWords: []string{"q"}}}},
			{ID: "al-multi", Regex: `F:(\d*)x`, Keywords: []string{"f:"}, Allow: []specAllow{
				{StopWords: []string{"bad"}}, {Regexes: []string{`^\d{2}$`}}, {Regexes: []string{`^$`}}}},
			{ID: "al-path", Regex: `P:(\S+)`, Keywords: []string{"p:"}},
			{ID: "al-empty-group", Regex: `H:(\d*)(\w*)`, Group: 1, Keywords: []string{"h:"}, Allow: []specAllow{
				{Regexes: []string{`^$`, `^9`}}}},
			{ID: "al-line-multi", Regex: `G:([a-z]+)\n+[a-z0-9]+`, Keywords: []string{"g:"}, Allow: []specAllow{
				{Target: "line", Regexes: []string{`\n[a-z0-9]+$`}}}},
		}, Global: []specAllow{
			{Regexes: []string{`^/Users/(?i)bob`, `^/BIN/x$`}},
			{StopWords: []string{"example", "000"}},
			{And: true, Target: "line", Regexes: []string{`^# `}, StopWords: []string{"zz"}},
		}},
	}
}

var synthWords = []string{
	"tok", "TOK", "token", "Token", "=", ":", " ", "  ", "\n", "\r\n", "\t", "abcd", "aabc", "aaaa", "abcdefgh",
	"ABCD", "x", "y", "z", "12", "345", "7", "a", "b", "c", "aab", "abc", "SECRET", "secret", "S3CRET", "q123",
	"q12", "begin", "end", "key", "val", "é", "èàù", "é", "password", "PASSWORD", "EXAMPLE", "example", "foo",
	"bar", "/Users/Bob/x", "/USR/x", "/bin/x", "/BIN/x", "\"", "'", ";", "#", "# ", "zz", "zq", "E:", "U:", "L:", "M:",
	"A:", "B:", "B2:", "C:", "D:", "F:", "P:", "0000", "a1b2c3d4", "skip", "ok", "q", "bad", "9", "x9", "foo1", "BAR", "ecre", "abcdef", "ab",
	// whole values for the allowlist rules: paths for the joined (?i), matches that end a line
	// or are followed by more of it, empty secrets
	"P:/bin/x ", "P:/BIN/x ", "P:/Users/BOB ", "P:/users/bob ", "P:/BIN/X ", "D:abq ", "D:abc ok", "D:ab9q ok",
	"\r", "C:zab", "C:abc\n", "F:x", "F:12x", "F:123x", "D:abq\n#", "\n#D:abc",
	"D:abq\rok", "U:\u00e8\u00e0\u00f9\u00e9", "U:\u00e9\u00e9\u00e9\u00e9", "U:\u00e8\u00e0\u00f9a", "G:abc\nde", "G:ab\n\nxy9", "G:abc\nde ok",
	"H:ab", "H:12 ", "H:9a", "H:", "H:7x",
}

func synthTexts(seed int64, n int) []string {
	r := rand.New(rand.NewSource(seed))
	out := make([]string, n)
	for i := range out {
		var sb strings.Builder
		for j, k := 0, 2+r.Intn(11); j < k; j++ {
			sb.WriteString(synthWords[r.Intn(len(synthWords))])
		}
		out[i] = sb.String()
	}
	return out
}

func toLibraryAllow(a specAllow) *config.Allowlist {
	al := &config.Allowlist{StopWords: a.StopWords}
	if a.And {
		al.MatchCondition = config.AllowlistMatchAnd
	}
	al.RegexTarget = a.Target
	for _, r := range a.Regexes {
		al.Regexes = append(al.Regexes, regexp.MustCompile(r))
	}
	if err := al.Validate(); err != nil {
		panic(err)
	}
	return al
}

// libraryFor is the gitleaks detector for a spec, configured as library() is.
func libraryFor(sp synth) func(string) []finding {
	cfg := config.Config{Rules: map[string]config.Rule{}, Keywords: map[string]struct{}{}}
	for _, r := range sp.Rules {
		cr := config.Rule{RuleID: r.ID, Regex: regexp.MustCompile(r.Regex), SecretGroup: r.Group, Entropy: r.Entropy, Keywords: r.Keywords}
		for _, k := range r.Keywords {
			cfg.Keywords[k] = struct{}{}
		}
		for _, a := range r.Allow {
			cr.Allowlists = append(cr.Allowlists, toLibraryAllow(a))
		}
		cfg.Rules[r.ID] = cr
	}
	for _, a := range sp.Global {
		cfg.Allowlists = append(cfg.Allowlists, toLibraryAllow(a))
	}
	d := detect.NewDetector(cfg)
	d.MaxDecodeDepth = 0
	d.IgnoreGitleaksAllow = true
	return func(s string) []finding {
		var out []finding
		for _, f := range d.DetectString(s) {
			out = append(out, finding{f.RuleID, f.Secret})
		}
		return out
	}
}

func oursFor(sp synth) func(string) []finding {
	conv := func(a specAllow) secretrules.Allowlist {
		x := secretrules.Allowlist{And: a.And, Regexes: a.Regexes, StopWords: a.StopWords}
		switch a.Target {
		case "match":
			x.Target = secretrules.TargetMatch
		case "line":
			x.Target = secretrules.TargetLine
		}
		return x
	}
	var rules []secretrules.Rule
	for _, r := range sp.Rules {
		sr := secretrules.Rule{ID: r.ID, Regex: r.Regex, Group: r.Group, Entropy: r.Entropy, Keywords: r.Keywords}
		for _, a := range r.Allow {
			sr.Allow = append(sr.Allow, conv(a))
		}
		rules = append(rules, sr)
	}
	var global []secretrules.Allowlist
	for _, a := range sp.Global {
		global = append(global, conv(a))
	}
	d := secretrules.New(rules, global)
	return func(s string) []finding {
		var out []finding
		for _, f := range d.Scan(s) {
			out = append(out, finding{f.RuleID, f.Secret})
		}
		return out
	}
}

type synthCase struct {
	text string
	want []finding
}

// runSynthetic compares library and detector on n random texts per spec. It returns the
// number of inputs, the mismatches (described), and per spec the cases for the golden file.
func runSynthetic(n int) (inputs int, bad []mismatch, golden map[string][]synthCase) {
	golden = map[string][]synthCase{}
	for i, sp := range synthConfigs() {
		lib, ours := libraryFor(sp), oursFor(sp)
		bySig, hits, without := map[string]int{}, 0, 0
		for _, text := range synthTexts(int64(100+i), n) {
			inputs++
			want, got := lib(text), ours(text)
			if !equal(keys(want), keys(got)) {
				bad = append(bad, mismatch{"synthetic " + sp.Name, text, keys(want), keys(got)})
			}
			// the golden keeps a few cases of each combination of rules that fired (and of
			// empty secrets), so rare branches are in it, not just the common ones
			sig := ""
			for _, k := range keys(want) {
				rule, secret, _ := strings.Cut(k, "\x00")
				sig += "|" + rule
				if secret == "" {
					sig += "!"
				}
			}
			if len(want) > 0 && bySig[sig] < 4 && hits < 900 {
				bySig[sig]++
				hits++
				golden[sp.Name] = append(golden[sp.Name], synthCase{text, want})
			} else if len(want) == 0 && without < 300 {
				without++
				golden[sp.Name] = append(golden[sp.Name], synthCase{text, nil})
			}
		}
	}
	return inputs, bad, golden
}
