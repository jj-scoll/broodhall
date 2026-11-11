package secretrules

import (
	"bytes"
	"compress/gzip"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"testing"
)

// testdata/parity.json.gz is written by tools/gitleaksrules (regen.sh): each case is a text and
// the findings the gitleaks library returned for it, configured as the guardrails binary used
// it (default rules, generic-api-key removed, no decoding, gitleaks:allow ignored). Texts and
// secrets are base64 and the file is gzipped, because GitHub's push protection decodes base64
// and refuses the gitleaks example tokens.
//
//go:embed testdata/parity.json.gz
var parityGz []byte

type goldenCase struct {
	Kind string `json:"kind"`
	In   string `json:"in"`
	Want []struct {
		Rule   string `json:"rule"`
		Secret string `json:"secret"`
	} `json:"want"`
}

type goldenFile struct {
	Source string       `json:"source"`
	Cases  []goldenCase `json:"cases"`
	// Synthetic are small rule sets built to reach every branch of the library's matching,
	// with the library's answers for random texts over them.
	Synthetic []struct {
		Name  string `json:"name"`
		Rules []struct {
			ID       string  `json:"id"`
			Regex    string  `json:"regex"`
			Group    int     `json:"group"`
			Entropy  float64 `json:"entropy"`
			Keywords []string
			Allow    []specAllow `json:"allow"`
		} `json:"rules"`
		Global []specAllow  `json:"global"`
		Cases  []goldenCase `json:"cases"`
	} `json:"synthetic"`
}

type specAllow struct {
	And       bool     `json:"and"`
	Target    string   `json:"target"`
	Regexes   []string `json:"regexes"`
	StopWords []string `json:"stopWords"`
}

func (a specAllow) allowlist() Allowlist {
	x := Allowlist{And: a.And, Regexes: a.Regexes, StopWords: a.StopWords}
	switch a.Target {
	case "match":
		x.Target = TargetMatch
	case "line":
		x.Target = TargetLine
	}
	return x
}

func loadGolden(t *testing.T) goldenFile {
	t.Helper()
	var f goldenFile
	zr, err := gzip.NewReader(bytes.NewReader(parityGz))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewDecoder(zr).Decode(&f); err != nil {
		t.Fatal(err)
	}
	return f
}

func b64(t *testing.T, s string) string {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// keys is findings as sorted "rule\x00secret" strings, duplicates kept.
func keys(fs []Finding) []string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = f.RuleID + "\x00" + f.Secret
	}
	sort.Strings(out)
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func show(fs []Finding) []string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = fmt.Sprintf("%s=%.40s", f.RuleID, f.Secret)
	}
	return out
}

// check runs scan over cases and reports every difference from the library's answer.
func check(t *testing.T, label string, scan func(string) []Finding, cases []goldenCase) {
	t.Helper()
	mismatches, shown := map[string]int{}, 0
	total := map[string]int{}
	for i, c := range cases {
		in := b64(t, c.In)
		var want []Finding
		for _, w := range c.Want {
			want = append(want, Finding{w.Rule, b64(t, w.Secret)})
		}
		total[c.Kind]++
		got := scan(in)
		if !equal(keys(got), keys(want)) {
			mismatches[c.Kind]++
			if shown < 5 {
				shown++
				t.Errorf("%s case %d (%s): got %d findings, want %d\n  in:   %.200q\n  got:  %q\n  want: %q", label, i, c.Kind, len(got), len(want), in, show(got), show(want))
			}
		}
	}
	for kind, n := range total {
		if m := mismatches[kind]; m > 0 {
			t.Errorf("%s: %s: %d of %d cases differ from gitleaks", label, kind, m, n)
		}
	}
}

// TestParityWithGitleaks is the guard that Scan returns exactly what the gitleaks library
// returned: the same (rule, secret) pairs, with the same multiplicity, for every case.
func TestParityWithGitleaks(t *testing.T) {
	f := loadGolden(t)
	if len(f.Cases) < 1000 {
		t.Fatalf("golden has %d cases; regenerate with tools/gitleaksrules/regen.sh", len(f.Cases))
	}
	check(t, "default rules", Scan, f.Cases)
}

// TestSyntheticParityWithGitleaks covers what the default rules do not use (allowlist
// condition "and", regexTarget match and line, and so on) on rule sets built for it.
func TestSyntheticParityWithGitleaks(t *testing.T) {
	f := loadGolden(t)
	if len(f.Synthetic) == 0 {
		t.Fatal("golden has no synthetic rule sets")
	}
	for _, sp := range f.Synthetic {
		var rules []Rule
		for _, r := range sp.Rules {
			x := Rule{ID: r.ID, Regex: r.Regex, Group: r.Group, Entropy: r.Entropy, Keywords: r.Keywords}
			for _, a := range r.Allow {
				x.Allow = append(x.Allow, a.allowlist())
			}
			rules = append(rules, x)
		}
		var global []Allowlist
		for _, a := range sp.Global {
			global = append(global, a.allowlist())
		}
		if len(sp.Cases) < 500 {
			t.Fatalf("%s: %d cases", sp.Name, len(sp.Cases))
		}
		check(t, sp.Name, New(rules, global).Scan, sp.Cases)
	}
}

// TestGoldenReachesEveryRule: a rule the golden never fires would be untested, so every rule
// must have a case the library found it on, and the golden must have cases it found nothing on.
func TestGoldenReachesEveryRule(t *testing.T) {
	f := loadGolden(t)
	fired := map[string]bool{}
	empty := 0
	for _, c := range f.Cases {
		for _, w := range c.Want {
			fired[w.Rule] = true
		}
		if len(c.Want) == 0 {
			empty++
		}
	}
	for _, r := range defaultRules {
		if !fired[r.ID] {
			t.Errorf("rule %s has no golden case that fires it", r.ID)
		}
	}
	if empty == 0 {
		t.Error("golden has no negative cases")
	}
}
