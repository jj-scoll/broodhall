package main

import (
	"bytes"
	"regexp"
	"strings"
	"testing"
)

var secretTagRE = regexp.MustCompile(`\[[a-z0-9-]+:[0-9a-f]{6}\]`)

func TestMaskSecretsShapes(t *testing.T) {
	x := testRedactor()
	f := fakeSecrets
	for _, c := range []struct{ text, value, slug string }{
		{"key " + f["github"] + " here", f["github"], "github-token"},
		{"AKIA in text " + f["aws"] + ".", f["aws"], "aws-access-key"},
		{"x " + f["anthropic"], f["anthropic"], "anthropic-api-key"},
		{"aws_secret_access_key = " + f["awsSecret"], f["awsSecret"], "aws-secret-key"},
		{"Authorization: Bearer " + f["authHeader"], f["authHeader"], "authorization-header"},
		{"t " + fakeAtlassian + " t", fakeAtlassian, "atlassian-api-token"},
	} {
		got := x.maskSecrets(c.text)
		if strings.Contains(got, c.value) || !strings.Contains(got, "["+c.slug+":") {
			t.Errorf("maskSecrets(%q) = %q, want %s tag", c.text, got, c.slug)
		}
	}
	// Labels stay readable: only the value is replaced.
	if got := x.maskSecrets("aws_secret_access_key = " + f["awsSecret"]); !strings.HasPrefix(got, "aws_secret_access_key = [") {
		t.Errorf("label lost: %q", got)
	}
	// Equal values keep equal tags; the tag depends on the key.
	a, b := x.maskSecrets(f["github"]), x.maskSecrets("again "+f["github"])
	if !strings.HasSuffix(b, a) || a == x.maskSecrets(f["npm"]) {
		t.Errorf("tags not stable per value: %q %q", a, b)
	}
	if a == newRedactor([]byte("other"), nil).maskSecrets(f["github"]) {
		t.Errorf("tag does not depend on the key")
	}
	for _, keep := range []string{"plain text", "commit 24a8b5a3f1e2d3c4b5a6f7e8d9c0b1a2", "https://github.com/x/y"} {
		if got := x.maskSecrets(keep); got != keep {
			t.Errorf("kept %q changed to %q", keep, got)
		}
	}
}

// A private key is masked from BEGIN through END, not just its header line.
func TestMaskSecretsPrivateKeyBlock(t *testing.T) {
	x := testRedactor()
	body := "MIIEpAIBAAKCAQEAfakefakefake\nZmFrZWZha2VmYWtlZmFrZQ==\n"
	pem := "-----BEGIN RSA " + "PRIVATE KEY-----\n" + body + "-----END RSA " + "PRIVATE KEY-----"
	got := x.maskSecrets("before\n" + pem + "\nafter\n")
	if strings.Contains(got, "MIIEpA") || strings.Contains(got, "ZmFrZ") || !strings.HasPrefix(got, "before\n[private-key:") || !strings.HasSuffix(got, "]\nafter\n") {
		t.Errorf("PEM block not masked whole: %q", got)
	}
	// No END line (truncated output): mask to the end, failing closed.
	if got := x.maskSecrets("-----BEGIN " + "PRIVATE KEY-----\n" + body); strings.Contains(got, "MIIEpA") {
		t.Errorf("unterminated PEM leaked: %q", got)
	}
}

// Random passwords have no recognizable shape: values under secret-looking key names are
// masked in toml, ini, env, yaml and json, and other keys stay readable.
func TestMaskSecretsKeyNames(t *testing.T) {
	x := testRedactor()
	pw := "hunter" + "2-Zq9!xV"
	in := strings.Join([]string{
		`[confluence]`,
		`base_url = "https://example.atlassian.net/wiki"`,
		`username = "me@example.org"`,
		`password = "` + pw + `"`,
		`api_token = "` + fakeAtlassian + `"`,
		`space = "ENG"`,
		`export DB_PASSWORD=` + pw + `x`,
		`  clientSecret: ` + pw + `y`,
		`  "secret-key": "` + pw + `z",`,
		`{"user":"u1","apiKey":"` + pw + `w","n":2}`,
		`auth=` + pw + `v # comment`,
		`api_enabled = true`,
		`credentials:`,
		`"credentials": {`,
		`token = ""`,
	}, "\n")
	got := x.maskSecrets(in)
	if strings.Contains(got, pw) || strings.Contains(got, fakeAtlassian) {
		t.Fatalf("secret value left in:\n%s", got)
	}
	for _, want := range []string{
		`base_url = "https://example.atlassian.net/wiki"`,
		`username = "me@example.org"`,
		`space = "ENG"`,
		`password = "[secret:`,
		`api_token = "[atlassian-api-token:`,
		`export DB_PASSWORD=[secret:`,
		`  clientSecret: [secret:`,
		`  "secret-key": "[secret:`,
		`{"user":"u1","apiKey":"[secret:`,
		`","n":2}`,
		`auth=[secret:`,
		`api_enabled = true`,
		"credentials:\n",
		`"credentials": {`,
		`token = ""`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	// A value caught by its key gets the same tag as the same value caught by its shape.
	if a, b := x.maskSecrets(`token = "`+fakeSecrets["github"]+`"`), x.maskSecrets(fakeSecrets["github"]); !strings.Contains(a, b) {
		t.Errorf("shape tag %q not reused under a key: %q", b, a)
	}
}

// grep -n, grep -r, cat -n and YAML lists put text before the key; the key is still found.
func TestMaskSecretsKeyAfterPrefix(t *testing.T) {
	x := testRedactor()
	pw := "hunter" + "2-Zq9!xV"
	for _, line := range []string{
		`12:password = "` + pw + `"`,
		"     1\tpassword = \"" + pw + `"`,
		`/home/x/.config/mark.toml:3:token = ` + pw,
		`  - password: ` + pw,
		`url = https://h/x?token=` + pw,
	} {
		if got := x.maskSecrets(line); strings.Contains(got, pw) || !strings.Contains(got, "[secret:") {
			t.Errorf("%q -> %q", line, got)
		}
	}
	for _, keep := range []string{"see https://example.org/a:b", "12:username = bob", "time: 12:30"} {
		if got := x.maskSecrets(keep); got != keep {
			t.Errorf("kept %q changed to %q", keep, got)
		}
	}
}

func TestRedactSecretsExecAndFilter(t *testing.T) {
	gh := fakeSecrets["github"]
	var stdout, stderr bytes.Buffer
	code := runRedact(testRedactor(), []string{"--secrets", "--", "sh", "-c",
		`printf 'password=abc123\nuser=bob\n'; printf '%s\n' "$1" >&2; exit 3`, "sh", gh}, nil, &stdout, &stderr)
	if code != 3 {
		t.Errorf("exit %d, want 3", code)
	}
	if o := stdout.String(); strings.Contains(o, "abc123") || !strings.Contains(o, "user=bob") || !secretTagRE.MatchString(o) {
		t.Errorf("stdout %q", o)
	}
	if e := stderr.String(); strings.Contains(e, gh) || !strings.Contains(e, "[github-token:") {
		t.Errorf("stderr %q", e)
	}
	stdout.Reset()
	if code := runRedact(testRedactor(), []string{"--secrets"}, strings.NewReader("TOKEN="+gh+"\n"), &stdout, &stderr); code != 0 || strings.Contains(stdout.String(), gh) {
		t.Errorf("filter: exit %d, %q", code, stdout.String())
	}
}
