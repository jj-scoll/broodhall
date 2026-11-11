package main

import (
	"encoding/json"
	"os"
	"sort"
	"strings"
	"testing"
)

// Synthetic, randomly generated values in the shape of provider tokens that the hand-written
// patterns do not cover, split so scanners don't flag this file.
var fakeGitleaksSecrets = []struct{ rule, value string }{
	{"gitlab-pat", "glpat-" + "u8jzPde0IgxLd6GncfBA"},
	{"stripe-access-token", "sk_live_" + "epfJBd0Kh8oOOL8dKLzdocJ2"},
	{"sendgrid-api-token", "SG." + "isAjIhKtJ0RlgLKOmxgJTe.hA-2O76UMFxFkM-R5Kjp1vRt_1fjORS-6ilI8ihN5KX"},
	{"shopify-access-token", "shpat_" + "b0eb53f16947ccf25ec84d8dbc742547"},
	{"digitalocean-pat", "dop_v1_" + "70f58904dba41ecccc3fc1626e53a13043b026c48bbf33feff9243a8f506b409"},
	{"databricks-api-token", "dapi" + "28b5b7a767c76fb008f86bebb2737f6a"},
	{"pypi-upload-token", "pypi-AgEIcHlwaS5vcmc" + "A9a9SkpXz9w3QlY7Zkuvqdt7s8Stqcbnr3yBdGBLEPH1qhT61qtc4xatws8p"},
	{"rubygems-api-token", "rubygems_" + "1af3176813e02ea68ef786e4d3cea27d26934b484e73cf57"},
}

func TestScanSecretsGitleaksRules(t *testing.T) {
	for _, c := range fakeGitleaksSecrets {
		t.Run(c.rule, func(t *testing.T) {
			got := scanSecrets("export TOKEN=" + c.value + "\n")
			if len(got) != 1 || got[0].Pattern != c.rule {
				t.Errorf("scanSecrets = %+v, want one %s match", got, c.rule)
			}
		})
	}
}

func TestSecretSpansGitleaksCoverValueOnly(t *testing.T) {
	for _, c := range fakeGitleaksSecrets {
		text := "see " + c.value + " and " + c.value + "\n"
		spans := secretSpans(text)
		if len(spans) != 2 {
			t.Errorf("%s: got %d spans, want 2 (every occurrence)", c.rule, len(spans))
			continue
		}
		for _, sp := range spans {
			if text[sp.Start:sp.End] != c.value {
				t.Errorf("%s: span %q, want exactly the value", c.rule, text[sp.Start:sp.End])
			}
		}
	}
}

func TestMaskSecretsGitleaksRules(t *testing.T) {
	x := testRedactor()
	for _, c := range fakeGitleaksSecrets {
		got := x.maskSecrets("token " + c.value + " end")
		if strings.Contains(got, c.value) || !strings.Contains(got, "["+c.rule+":") {
			t.Errorf("maskSecrets = %q, want a %s tag", got, c.rule)
		}
	}
}

// The hand-written patterns keep their names and masked shape where both would match, and
// key-name masking still owns generic key=value pairs.
func TestGitleaksKeepsExistingShape(t *testing.T) {
	x := testRedactor()
	f := fakeSecrets
	for _, c := range []struct{ text, slug string }{
		{"k " + f["github"], "github-token"},
		{"k " + f["aws"], "aws-access-key"},
		{"k " + f["slack"], "slack-token"},
		{f["pemRsa"] + "\n-----END RSA PRIVATE KEY-----", "private-key"},
		{`api_key = "q8Zr3xTn5Lw0Vb2Hc9Ud4Ke7Fy1Gs6Ja"`, "secret"},
	} {
		got := x.maskSecrets(c.text)
		if !strings.Contains(got, "["+c.slug+":") || strings.Count(got, "[") != 1 {
			t.Errorf("maskSecrets(%q) = %q, want one %s tag", c.text, got, c.slug)
		}
	}
	if got := scanSecrets("k " + f["github"]); len(got) != 1 || got[0].Pattern != "GitHub token" {
		t.Errorf("scanSecrets = %+v, want only the GitHub token match", got)
	}
}

// parityFixtures are the texts these tests and the hand-written pattern tests scan, for
// tools/gitleaksrules: it runs the gitleaks library and the detector over them and records the
// library's answers (regen.sh sets PARITY_FIXTURES_OUT; otherwise this is skipped).
func parityFixtures() []string {
	var out []string
	for _, c := range fakeGitleaksSecrets {
		out = append(out, "export TOKEN="+c.value+"\n", "see "+c.value+" and "+c.value+"\n", "token "+c.value+" end")
	}
	names := make([]string, 0, len(fakeSecrets))
	for k := range fakeSecrets {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		out = append(out, fakeSecrets[k], "k "+fakeSecrets[k])
	}
	return append(out, fakeSecrets["pemRsa"]+"\n-----END RSA PRIVATE KEY-----", `api_key = "q8Zr3xTn5Lw0Vb2Hc9Ud4Ke7Fy1Gs6Ja"`, fakeAtlassian, typicalOutput())
}

func TestWriteParityFixtures(t *testing.T) {
	path := os.Getenv("PARITY_FIXTURES_OUT")
	if path == "" {
		t.Skip("PARITY_FIXTURES_OUT not set")
	}
	b, err := json.Marshal(parityFixtures())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// typicalOutput is about 30 KB of source-looking lines, the size of a long tool result.
func typicalOutput() string {
	line := "func (x *redactor) maskSecretShapes(s string) string { spans := secretSpans(s) } // token=abc.def\n"
	return strings.Repeat(line, 30000/len(line))
}

// BenchmarkSecretSpans30K is the in-process scan (hand-written patterns plus gitleaks rules):
//
//	CGO_ENABLED=0 go test ./cmd/guardrails -run '^$' -bench SecretSpans30K
func BenchmarkSecretSpans30K(b *testing.B) {
	s := typicalOutput()
	secretSpans(s)
	b.SetBytes(int64(len(s)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		secretSpans(s)
	}
}
