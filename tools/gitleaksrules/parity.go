package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/jj-scoll/broodhall/cmd/guardrails/secretrules"
	"github.com/zricethezav/gitleaks/v8/detect"
)

type input struct{ kind, rule, text string }

// goldPerRule is how many of a rule's true positives go into the golden file.
const goldPerRule = 3

// parity runs the library and the detector over the same inputs and requires identical
// (rule, secret) findings, with the same multiplicity. It exits non-zero on any difference.
// With -write it also writes the library's answers for the committed inputs to the golden
// file the guardrails module tests against without importing the library.
func parity(args []string) error {
	fl := flag.NewFlagSet("parity", flag.ExitOnError)
	examples := fl.String("examples", "", "JSON lines of {rule,tp,fp}, from the library's rule generator (see regen.sh)")
	fixtures := fl.String("fixtures", "", "JSON array of strings: the guardrails test fixtures")
	repo := fl.String("repo", "", "directory whose files are also used as inputs (not written to the golden)")
	golden := fl.String("golden", "../../cmd/guardrails/secretrules/testdata/parity.json.gz", "golden file")
	write := fl.Bool("write", false, "write the golden file")
	fl.Parse(args)

	lib, err := library()
	if err != nil {
		return err
	}
	var ins []input
	if *examples != "" {
		ex, err := readExamples(*examples)
		if err != nil {
			return err
		}
		ins = append(ins, ex...)
	}
	ins = append(ins, handwritten()...)
	if *fixtures != "" {
		var fx []string
		b, err := os.ReadFile(*fixtures)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(b, &fx); err != nil {
			return err
		}
		for _, s := range fx {
			ins = append(ins, input{"fixture", "", s})
		}
	}
	// The live run uses every example, every mutation of each and 3000 mixes. The golden file
	// is a sample of that (it is committed): all fixtures and false positives, a few true
	// positives per rule, one mutation of each of those and 300 mixes of them.
	var base, goldBase []string
	var forGolden []input
	perRule := map[string]int{}
	for _, in := range ins {
		base = append(base, in.text)
		if in.kind == "tp" && perRule[in.rule] >= goldPerRule {
			continue
		}
		perRule[in.rule]++
		goldBase = append(goldBase, in.text)
		forGolden = append(forGolden, in)
	}
	var goldExtra []input // not among the live inputs
	for i, s := range goldBase {
		goldExtra = append(goldExtra, input{"mutation", "", mutators[i%len(mutators)](s)})
	}
	goldExtra = append(goldExtra, mixes(goldBase, 300)...)
	forGolden = append(forGolden, goldExtra...)
	all := append([]input{}, ins...)
	for _, s := range base {
		for _, m := range mutators {
			all = append(all, input{"mutation", "", m(s)})
		}
	}
	all = append(all, mixes(base, 3000)...)
	all = append(all, goldExtra...)
	if *repo != "" {
		files, err := repoFiles(*repo)
		if err != nil {
			return err
		}
		all = append(all, files...)
	}

	var bad []mismatch
	goldSet := map[string]bool{}
	for _, in := range forGolden {
		goldSet[in.text] = true
	}
	counts := map[string]int{}
	fired := map[string]int{}
	golds := map[string][]finding{}
	for _, in := range all {
		want := lib(in.text)
		var got []finding
		for _, f := range secretrules.Scan(in.text) {
			got = append(got, finding{f.RuleID, f.Secret})
		}
		counts[in.kind]++
		for _, f := range want {
			fired[f.Rule]++
		}
		w, g := keys(want), keys(got)
		if goldSet[in.text] {
			golds[in.text] = want
		}
		if !equal(w, g) {
			bad = append(bad, mismatch{in.kind, in.text, w, g})
		}
	}
	synN, synBad, synGold := runSynthetic(20000)
	counts["synthetic"] = synN
	bad = append(bad, synBad...)
	var kinds []string
	for k := range counts {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	total := 0
	for _, k := range kinds {
		fmt.Printf("  %-9s %6d inputs\n", k, counts[k])
		total += counts[k]
	}
	fmt.Printf("parity: %d inputs, %d rules fired by the library, %d mismatches\n", total, len(fired), len(bad))
	if d, err := detect.NewDetectorDefaultConfig(); err == nil {
		var silent []string
		for id := range d.Config.Rules {
			if fired[id] == 0 && !skipRules[id] {
				silent = append(silent, id)
			}
		}
		sort.Strings(silent)
		fmt.Printf("rules the library never fired on (expected: the rules that need a file path): %v\n", silent)
	}
	for i, r := range bad {
		if i == 20 {
			fmt.Println("  ...")
			break
		}
		fmt.Printf("MISMATCH [%s] %.300q\n  library: %q\n  ours:    %q\n", r.kind, r.in, short(r.lib), short(r.ours))
	}

	if *write {
		if err := writeGolden(*golden, forGolden, golds, synGold); err != nil {
			return err
		}
		fmt.Printf("wrote %s (%d cases)\n", *golden, len(forGolden))
	}
	if len(bad) > 0 {
		return fmt.Errorf("%d inputs differ between the library and the detector", len(bad))
	}
	return nil
}

func short(ks []string) []string {
	out := make([]string, len(ks))
	for i, k := range ks {
		r, s, _ := strings.Cut(k, "\x00")
		if len(s) > 60 {
			s = s[:60] + "..."
		}
		out[i] = r + "=" + s
	}
	return out
}

func keys(fs []finding) []string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = f.Rule + "\x00" + f.Secret
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

// handwritten are true positives for rules the library ships without examples (their
// generator marks them "validate TODO"), so the parity check still reaches them.
func handwritten() []input {
	r := rand.New(rand.NewSource(7))
	pick := func(alphabet string, n int) string {
		b := make([]byte, n)
		for i := range b {
			b[i] = alphabet[r.Intn(len(alphabet))]
		}
		return string(b)
	}
	const al = "abcdefghijklmnopqrstuvwxyz0123456789"
	var out []input
	for i := 0; i < 3; i++ {
		out = append(out,
			input{"tp", "dropbox-short-lived-api-token", fmt.Sprintf("dropbox_token = %q\n", "sl."+pick(al+"-=_", 135))},
			input{"tp", "dropbox-long-lived-api-token", fmt.Sprintf("DROPBOX_TOKEN: %s\n", pick(al, 11)+"AAAAAAAAAA"+pick(al+"-_=", 43))},
		)
	}
	return out
}

// readExamples is the true and false positives of every rule, de-duplicated and sorted so a
// regeneration that draws different random samples still writes the golden in a stable order.
func readExamples(path string) ([]input, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []input
	seen := map[string]bool{}
	dec := json.NewDecoder(bufio.NewReader(f))
	for dec.More() {
		var e struct {
			Rule   string
			TP, FP []string
		}
		if err := dec.Decode(&e); err != nil {
			return nil, err
		}
		for _, s := range e.TP {
			if !seen[s] {
				seen[s] = true
				out = append(out, input{"tp", e.Rule, s})
			}
		}
		for _, s := range e.FP {
			if !seen[s] {
				seen[s] = true
				out = append(out, input{"fp", e.Rule, s})
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.kind != b.kind {
			return a.kind > b.kind // tp before fp
		}
		if a.rule != b.rule {
			return a.rule < b.rule
		}
		return a.text < b.text
	})
	return out, nil
}

// mutators wrap an example in the situations where the library's choices show: newlines
// around the match (it trims them), upper case (keyword prefilter and case-insensitive
// regexes), the same value twice (multiplicity), a match on a last line with no newline (its
// line start is then 0), CRLF, invalid UTF-8, a trailing "gitleaks:allow" comment, an
// "example" stop word, a password label, JSON, leading blank lines and a URL.
var mutators = []func(string) string{
	func(s string) string { return "\n" + s + "\n" },
	strings.ToUpper,
	func(s string) string { return s + "\n" + s + "\n" },
	func(s string) string { return "filler line\nanother line\n" + s },
	func(s string) string { return s + "\r\nnext line\r\n" },
	func(s string) string { return "\xff" + s + "\xfe" },
	func(s string) string { return "x = " + s + " # gitleaks:allow\n" },
	func(s string) string { return s + " example" },
	func(s string) string { return "password: " + s + "\n" },
	func(s string) string { return `{"token": "` + s + `"}` },
	func(s string) string { return "\n\n" + s },
	func(s string) string { return "https://u:" + s + "@host/ " + s },
}

// mixes joins random examples with random separators into documents, so allowlists and
// line targets are tried with other text around and several findings in one input.
func mixes(base []string, n int) []input {
	r := rand.New(rand.NewSource(1))
	seps := []string{"\n", " ", "\r\n", "\n\n", "; ", ` "`, "\n  - ", "="}
	var out []input
	for i := 0; i < n; i++ {
		var sb strings.Builder
		for j, k := 0, 2+r.Intn(5); j < k; j++ {
			sb.WriteString(base[r.Intn(len(base))])
			sb.WriteString(seps[r.Intn(len(seps))])
		}
		out = append(out, input{"mix", "", sb.String()})
	}
	return out
}

// repoFiles is the text files under dir, whole, as one input each.
func repoFiles(dir string) ([]input, error) {
	var out []input
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if n := d.Name(); n == ".git" || n == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil || len(b) > 4<<20 || !utf8.Valid(b) {
			return nil
		}
		out = append(out, input{"repo-file", "", string(b)})
		return nil
	})
	return out, err
}

type mismatch struct {
	kind, in  string
	lib, ours []string
}

type goldenCase struct {
	Kind string       `json:"kind"`
	In   string       `json:"in"`
	Want []goldenWant `json:"want"`
}

type goldenWant struct {
	Rule   string `json:"rule"`
	Secret string `json:"secret"`
}

func writeGolden(path string, ins []input, answers map[string][]finding, syn map[string][]synthCase) error {
	enc := base64.StdEncoding
	var cases []goldenCase
	for _, in := range ins {
		c := goldenCase{Kind: in.kind, In: enc.EncodeToString([]byte(in.text)), Want: []goldenWant{}}
		for _, f := range answers[in.text] {
			c.Want = append(c.Want, goldenWant{f.Rule, enc.EncodeToString([]byte(f.Secret))})
		}
		sort.Slice(c.Want, func(i, j int) bool {
			if c.Want[i].Rule != c.Want[j].Rule {
				return c.Want[i].Rule < c.Want[j].Rule
			}
			return c.Want[i].Secret < c.Want[j].Secret
		})
		cases = append(cases, c)
	}
	var sb strings.Builder
	source, _ := json.Marshal("gitleaks " + libraryVersion() + ", default config, generic-api-key removed, no decoding, gitleaks:allow ignored")
	// one case per line: a regeneration diffs by case, not as one 1 MB line
	sb.WriteString(`{"source":` + string(source) + ",\"cases\":[\n")
	for i, c := range cases {
		b, err := json.Marshal(c)
		if err != nil {
			return err
		}
		sb.Write(b)
		if i < len(cases)-1 {
			sb.WriteString(",")
		}
		sb.WriteString("\n")
	}
	sb.WriteString("],\n\"synthetic\":[\n")
	for i, sp := range synthConfigs() {
		type synthGolden struct {
			synth
			Cases []goldenCase `json:"cases"`
		}
		g := synthGolden{synth: sp}
		for _, c := range syn[sp.Name] {
			gc := goldenCase{Kind: "synthetic", In: enc.EncodeToString([]byte(c.text)), Want: []goldenWant{}}
			for _, f := range c.want {
				gc.Want = append(gc.Want, goldenWant{f.Rule, enc.EncodeToString([]byte(f.Secret))})
			}
			g.Cases = append(g.Cases, gc)
		}
		b, err := json.Marshal(g)
		if err != nil {
			return err
		}
		sb.Write(b)
		if i < len(synthConfigs())-1 {
			sb.WriteString(",")
		}
		sb.WriteString("\n")
	}
	sb.WriteString("]}\n")
	// Gzipped with a zero header so the same answers give the same bytes.
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(sb.String())); err != nil {
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0o644)
}
