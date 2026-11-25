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

// G3 (guard-evasion eval): the password of scheme://user:pass@host and scheme://:pass@host
// was shown in clear. Scheme, user and host stay readable.
func TestMaskSecretsURLCredentials(t *testing.T) {
	x := testRedactor()
	pw := "s3cr3t" + "Pw-9zQ"
	for _, c := range []struct{ in, prefix, suffix string }{
		{"DATABASE_URL=postgres://app:" + pw + "@db.example.org:5432/prod", "DATABASE_URL=postgres://app:[", "]@db.example.org:5432/prod"},
		{"REDIS_URL=redis://:" + pw + "@cache:6379", "REDIS_URL=redis://:[", "]@cache:6379"},
		{`"amqps://u:` + pw + `@mq/v?x=1"`, `"amqps://u:[`, `]@mq/v?x=1"`},
		{"src/.env:3:mongodb+srv://a.b:" + pw + "@c.example.net/db", "src/.env:3:mongodb+srv://a.b:[", "]@c.example.net/db"},
	} {
		got := x.maskSecrets(c.in)
		if strings.Contains(got, pw) || !strings.HasPrefix(got, c.prefix) || !strings.HasSuffix(got, c.suffix) {
			t.Errorf("maskSecrets(%q) = %q, want %q<tag>%q", c.in, got, c.prefix, c.suffix)
		}
	}
	a, b := secretTagRE.FindString(x.maskSecrets("a://u:"+pw+"@h")), secretTagRE.FindString(x.maskSecrets("b://v:"+pw+"@h2"))
	if a == "" || a != b {
		t.Errorf("equal passwords should share a tag: %q %q", a, b)
	}
	for _, keep := range []string{
		"postgres://app@host/db", "postgres://host:5432/db", "https://example.org/a:b@c", "http://localhost:8080",
		"git@github.com:org/repo.git", "ssh user@host", "mail me@example.org", "redis://:@host", "see ftp://host:21/x",
	} {
		if got := x.maskSecrets(keep); got != keep {
			t.Errorf("kept %q changed to %q", keep, got)
		}
	}
}

// By-file mode masks only the lines that name a protected file, with key names and shapes.
func TestRedactByFileMasksOnlyProtectedFileLines(t *testing.T) {
	gh := fakeSecrets["github"]
	pw := "hunter" + "2-Zq9!xV"
	in := strings.Join([]string{
		"./.env:1:STRIPE_SECRET_KEY=" + pw,
		"./.env:2:DB=postgres://app:" + pw + "@h/db",
		"./.env-3-API_TOKEN=" + pw,
		"deploy/prod.tfvars:4:password = \"" + pw + "\"",
		"HEAD:.env:5:SECRET=" + pw,
		"./src/config.go:7:password = os.Getenv(\"PW\") // " + "TOKEN=" + "plain",
		"./README.md:9:set API_KEY=your-key-here",
		"./.env.example:1:API_KEY=changeme",
		"./notes.txt:2:token " + gh,
		"",
	}, "\n")
	var stdout, stderr bytes.Buffer
	code := runRedact(testRedactor(), []string{"--secrets", "--by-file", "--", "sh", "-c", `printf '%s' "$1"`, "sh", in}, nil, &stdout, &stderr)
	out := stdout.String()
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
	if strings.Contains(out, pw) {
		t.Errorf("protected-file value leaked:\n%s", out)
	}
	for _, want := range []string{"./.env:1:STRIPE_SECRET_KEY=[secret:", "./.env:2:DB=postgres://app:[", "./.env-3-API_TOKEN=[secret:", "deploy/prod.tfvars:4:password = \"[secret:", "HEAD:.env:5:SECRET=[secret:"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// Ordinary search output is untouched, templates included.
	for _, keep := range []string{"./src/config.go:7:password = os.Getenv(\"PW\") // TOKEN=plain", "./README.md:9:set API_KEY=your-key-here", "./.env.example:1:API_KEY=changeme", "./notes.txt:2:token " + gh} {
		if !strings.Contains(out, keep+"\n") {
			t.Errorf("ordinary line changed: %q in:\n%s", keep, out)
		}
	}
}
