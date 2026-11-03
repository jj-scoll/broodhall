package main

import (
	"strings"
	"testing"
)

// Fixtures are split like secretscan_test.go so scanners do not flag this file.
var (
	memdFakeGH   = "ghp_" + "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijkl"
	memdFakeAWS  = "wJalrXUtnFEMI/K7MDENG/" + "bPxRfiCYEXAMPLEKEY"
	memdFakeOAI  = "sk-" + "abc123def456ghi789jkl012mno"
	memdFakePEM  = "-----BEGIN RSA " + "PRIVATE KEY-----\nMIIEpAIBAAKCAQEAfakefakefake\n-----END RSA " + "PRIVATE KEY-----"
	memdPEMInner = "MIIEpAIBAAKCAQEAfakefakefake"
)

func TestMemdRedact(t *testing.T) {
	tests := []struct {
		name  string
		in    string
		leaks []string // substrings that must not survive
	}{
		{"github token", "clone with " + memdFakeGH + " now", []string{memdFakeGH}},
		{"aws secret key=value", "aws_secret_access_key=" + memdFakeAWS, []string{memdFakeAWS}},
		{"pem block", "before\n" + memdFakePEM + "\nafter", []string{memdPEMInner}},
		{"json-escaped string", `"{\"k\":\"` + memdFakeOAI + `\"}"`, []string{memdFakeOAI}},
		{"plain text untouched", "go test ./... passed", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := memdRedact(tt.in)
			for _, l := range tt.leaks {
				if strings.Contains(got, l) {
					t.Errorf("secret survived: %q", got)
				}
			}
			if len(tt.leaks) == 0 && got != tt.in {
				t.Errorf("changed benign text: %q", got)
			}
			if len(tt.leaks) > 0 && got == tt.in {
				t.Errorf("not masked")
			}
			if again := memdRedact(got); again != got {
				t.Errorf("not idempotent:\n1: %q\n2: %q", got, again)
			}
		})
	}
}

// Accepted risk 2026-10-28 and R21/R22: under a secret key the unquoted value is the first
// whitespace-delimited token; it is masked unless purely numeric under a count word
// (TOKENS, KEYS). Go test result lines are exempt from key-value masking.
func TestMemdRedactNarrowKeyValues(t *testing.T) {
	kept := []string{
		"--- PASS: TestMemdTail (0.01s)",
		"    --- FAIL: TestX/sub (1.20s)",
		"input_tokens: 1994",
		`"output_tokens": 603`,
		`"output_tokens": 603,`,
		"TOKENS=1,234",
		"KEYS: 12",
	}
	for _, in := range kept {
		t.Run("kept "+in, func(t *testing.T) {
			if got := memdRedact(in); got != in {
				t.Errorf("changed: %q", got)
			}
		})
	}
	masked := []struct {
		name, in string
		leaks    []string // must not survive
		keeps    []string // must survive
	}{
		{"plain", "password=hunter2", []string{"hunter2"}, nil},
		{"env", "DB_PASSWORD=s3cr3tValue", []string{"s3cr3tValue"}, nil},
		{"aws", "AWS_SECRET_ACCESS_KEY=" + memdFakeAWS, []string{memdFakeAWS}, nil},
		{"api key", "api_key: abcd1234efgh", []string{"abcd1234efgh"}, nil},
		{"numeric password", "password=12345678", []string{"12345678"}, nil},
		{"numeric api key", "api_key: 123456", []string{"123456"}, nil},
		{"singular token", "token: 42", []string{"42"}, nil},
		{"pin pass", "PIN_PASS=0000", []string{"0000"}, nil},
		{"quoted spaces", `password: "correct horse battery"`, []string{"correct horse"}, nil},
		{"quoted json", `"token": "abc def"`, []string{"abc def"}, nil},
		{"shape in prose", "note one two " + memdFakeGH + " three", []string{memdFakeGH}, nil},
		{"trailing comment", "password=hunter2 # prod", []string{"hunter2"}, []string{"# prod"}},
		{"colon comment", "password: x # note", []string{": x "}, []string{"# note"}},
		{"trailing word", "KEY=val other", []string{"val"}, []string{"other"}},
		{"trailing flag", "API_KEY=abc123 --verbose", []string{"abc123"}, []string{"--verbose"}},
		{"semicolon", "password=hunter2; echo done", []string{"hunter2"}, []string{"; echo done"}},
		{"export", "export X=1 PASSWORD=hunter2 foo", []string{"hunter2"}, []string{"export X=1", "foo"}},
		{"second pair", "password=a user=b", []string{"=a "}, []string{"user=b"}},
		{"second secret", "password=a token=bcd", []string{"=a ", "bcd"}, nil},
		{"tab", "PASSWORD=s3cret\tfoo", []string{"s3cret"}, []string{"\tfoo"}},
		{"json siblings", `{"x": 1, "password": 12345, "y": 2}`, []string{"12345"}, []string{`{"x": 1, "password": `, `, "y": 2}`}},
		{"first word of passphrase", "password: correct horse battery", []string{"correct"}, []string{"horse battery"}},
		{"test-name list", "PASS: TestA, TestB in x_test.go", []string{"TestA"}, []string{"TestB in x_test.go"}},
		{"grep hit", `api.tf:12:  resource "aws_iam_role" "x" {`, nil, []string{`resource "aws_iam_role"`}},
		{"test line with shape", "--- PASS: TestX (0.01s) " + memdFakeGH, []string{memdFakeGH}, nil},
	}
	for _, tt := range masked {
		t.Run("masked "+tt.name, func(t *testing.T) {
			got := memdRedact(tt.in)
			if tt.leaks != nil && got == tt.in {
				t.Errorf("not masked: %q", got)
			}
			for _, l := range tt.leaks {
				if strings.Contains(got, l) {
					t.Errorf("%q survived: %q", l, got)
				}
			}
			for _, k := range tt.keeps {
				if !strings.Contains(got, k) {
					t.Errorf("%q lost: %q", k, got)
				}
			}
			if again := memdRedact(got); again != got {
				t.Errorf("not idempotent:\n1: %q\n2: %q", got, again)
			}
		})
	}
}

// maskSecrets masks to the end of the line; memory masks the first token only (the
// accepted, documented risk: the rest of a multi-word passphrase is exposed).
func TestRedactSecretsKeepsMultiWordValue(t *testing.T) {
	const in = "password: correct horse battery"
	if got := memdRedactor.maskSecrets(in); strings.Contains(got, "correct") || strings.Contains(got, "horse") {
		t.Errorf("maskSecrets no longer masks the whole value: %q", got)
	}
	if got := memdRedact(in); strings.Contains(got, "correct") || !strings.Contains(got, "horse battery") {
		t.Errorf("memory should mask only the first word (accepted risk): %q", got)
	}
}

// Task 10 masks before cutting, so a cut at any byte inside a secret cannot expose a fragment.
func TestMemdRedactStraddlesCut(t *testing.T) {
	const n = 100
	for _, secret := range []string{memdFakeGH, memdFakeOAI} {
		s := strings.Repeat("x ", (n-len(secret)/2)/2) + secret + " tail"
		if i := strings.Index(s, secret); i >= n || i+len(secret) <= n {
			t.Fatalf("fixture does not straddle byte %d (secret at %d)", n, i)
		}
		cut := memdRedact(s)[:n]
		for w := 8; w <= len(secret); w++ {
			for i := 0; i+w <= len(secret); i++ {
				if strings.Contains(cut, secret[i:i+w]) && w >= 8 {
					t.Fatalf("fragment %q of secret survived the cut", secret[i:i+w])
				}
			}
		}
	}
}

func TestCmdNorm(t *testing.T) {
	tests := []struct{ in, want string }{
		{"rtk git status", "git status"},
		{"git status", "git status"},
		{"rtkfoo x", "rtkfoo x"},
		{"rtk rtk x", "rtk x"},
		{"rtk", "rtk"},
	}
	for _, tt := range tests {
		if got := cmdNorm(tt.in); got != tt.want {
			t.Errorf("cmdNorm(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
