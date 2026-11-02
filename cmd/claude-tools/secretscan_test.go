// Cases ported from NVIDIA NemoClaw nemoclaw/src/security/secret-scanner.test.ts
// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"
)

// Synthetic values that look like real secrets but are not, split so gitleaks and
// trufflehog don't flag this file.
var fakeSecrets = map[string]string{
	"nvidia":            "nvapi-" + "abcdefghijklmnopqrstuvwxyz",
	"openai":            "sk-" + "abc123def456ghi789jkl012mno",
	"openaiProject":     "sk-proj-" + "abc123_def456-ghi789_jkl012-mno345",
	"github":            "ghp_" + "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmn",
	"githubFineGrained": "github_pat_" + "ABCDEFGHIJKLMNO_PQRSTUVWXYZabc",
	"aws":               "AKIA" + "IOSFODNN7EXAMPLE",
	"slack":             "xoxb-" + "123456789-abcdefghij",
	"slackApp":          "xapp-" + "1-A0000-12345-abcdef",
	"npm":               "npm_" + "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmn",
	"pemRsa":            "-----BEGIN RSA " + "PRIVATE KEY-----\nMIIEpA...",
	"pemOpenssh":        "-----BEGIN OPENSSH " + "PRIVATE KEY-----\nb3Blbn...",
	"telegram":          "123456789:" + "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghi",
	"google":            "AIza" + "SyA-1234567890abcdefghijklmnopqrstu",
	"anthropic":         "sk-ant-" + "api03-abcdefghijklmnopqrstuv",
	"huggingface":       "hf_" + "ABCDEFGHIJKLMNOPQRSTUVWXYZab",
	"discord":           "ABCDEFGHIJKLMNOPQRSTUVWx" + ".abc123" + ".ABCDEFGHIJKLMNOPQRSTUVWXYZa",
	"awsSecret":         "wJalrXUtnFEMI/K7MDENG/" + "bPxRfiCYEXAMPLEKEY",
	"authHeader": "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9" +
		".eyJzdWIiOiIxMjM0NTY3ODkwIn0.TJVA95OrM7E2cBab30RMHrHDcEfxjoYZgeFONFh7HgQ",
}

// Atlassian API tokens (not upstream; ~/.config/mark.toml holds them) are ATATT3 + 186.
var fakeAtlassian = "ATATT3" + strings.Repeat("xFake_Tok-", 17) + "en0=0A1B2C3D"

// SSO credentials (not upstream): ASIA keys, and the JSON `aws configure export-credentials` prints.
var fakeSessionToken = strings.Repeat("FwoGZXIvYXdzE", 10)

func TestScanSecretsKnownPatterns(t *testing.T) {
	f := fakeSecrets
	for _, c := range []struct{ text, want string }{
		{"my key is " + f["nvidia"], "NVIDIA API key"},
		{"export OPENAI_API_KEY=" + f["openai"], "OpenAI API key"},
		{"export OPENAI_API_KEY=" + f["openaiProject"], "OpenAI API key"},
		{"token: " + f["github"], "GitHub token"},
		{"token: " + f["githubFineGrained"], "GitHub token"},
		{"aws_access_key_id = " + f["aws"], "AWS access key"},
		{"SLACK_TOKEN=" + f["slack"], "Slack token"},
		{"SLACK_APP_TOKEN=" + f["slackApp"], "Slack token"},
		{"//registry.npmjs.org/:_authToken=" + f["npm"], "npm token"},
		{f["pemRsa"], "Private key"},
		{f["pemOpenssh"], "Private key"},
		{"bot token: " + f["telegram"], "Telegram bot token"},
		{"GOOGLE_API_KEY=" + f["google"], "Google API key"},
		{"key: " + f["anthropic"], "Anthropic API key"}, // and not also OpenAI
		{"HF_TOKEN=" + f["huggingface"], "HuggingFace token"},
		{"DISCORD_TOKEN=" + f["discord"], "Discord bot token"},
		{"aws_secret_access_key = " + f["awsSecret"], "AWS secret key"},
		{"Authorization: Bearer " + f["authHeader"], "Authorization header"},
		{`password="` + fakeAtlassian + `"`, "Atlassian API token"},
		{"aws_access_key_id = " + "ASIA" + "QWERTYUIOP123456", "AWS temporary access key"},
		{"AWS_SESSION_TOKEN=" + fakeSessionToken, "AWS session token"},
		{`"SessionToken": "` + fakeSessionToken + `"`, "AWS session token"},
		{`"SecretAccessKey": "` + f["awsSecret"] + `"`, "AWS secret key"},
	} {
		got := scanSecrets(c.text)
		if len(got) != 1 || got[0].Pattern != c.want {
			t.Errorf("scanSecrets(%q) = %+v, want one %q", c.text, got, c.want)
		}
	}
}

func TestScanSecretsSafeContent(t *testing.T) {
	for _, s := range []string{
		"# My Project\n\nThis is a regular markdown file.",
		"```python\nprint('hello world')\n```",
		"sk-short",
		"https://github.com/NVIDIA/NemoClaw/pull/1121",
		"id: 550e8400-e29b-41d4-a716-446655440000",
		"commit 24a8b5a3f1e2d3c4b5a6f7e8d9c0b1a2",
	} {
		if got := scanSecrets(s); len(got) != 0 {
			t.Errorf("scanSecrets(%q) = %+v, want none", s, got)
		}
	}
}

func TestScanSecretsRedactedPreview(t *testing.T) {
	if got := scanSecrets(fakeSecrets["nvidia"]); len(got) != 1 || got[0].Redacted != "nvap..wxyz" {
		t.Fatalf("got %+v, want Redacted nvap..wxyz", got)
	}
}

func TestScanSecretsMultiple(t *testing.T) {
	f := fakeSecrets
	if got := scanSecrets("NVIDIA_INFERENCE_API_KEY=" + f["nvidia"] + "\nOPENAI_KEY=" + f["openai"]); len(got) < 2 {
		t.Errorf("two different secrets: got %+v", got)
	}
	key1, key2 := "nvapi-"+"aaaabbbbccccddddeeeefffff", "nvapi-"+"xxxxyyyyzzzzwwwwvvvvuuuuu"
	if got := scanSecrets("first: " + key1 + "\nsecond: " + key2); len(got) != 2 {
		t.Errorf("two NVIDIA keys: got %+v, want 2", got)
	}
	if got := scanSecrets("key: " + f["nvidia"] + "\nrepeat: " + f["nvidia"]); len(got) != 1 {
		t.Errorf("repeated value: got %+v, want 1 after dedup", got)
	}
}

// Go's regexp has no lookbehind, so labelled secrets are matched with a capture group.
// The span must still cover only the value, never the label, or masking eats the label.
func TestSecretSpansCoverValueOnly(t *testing.T) {
	f := fakeSecrets
	for _, c := range []struct{ label, value string }{
		{"aws_secret_access_key = ", f["awsSecret"]},
		{"AWS_SECRET_ACCESS_KEY: ", f["awsSecret"]},
		{"DISCORD_TOKEN=\"", f["discord"]},
		{"Authorization: Bearer ", f["authHeader"]},
		{"bearer: '", f["authHeader"]},
		{`"SessionToken": "`, fakeSessionToken},
		{`"SecretAccessKey": "`, f["awsSecret"]},
	} {
		text := c.label + c.value + "\n"
		spans := secretSpans(text)
		if len(spans) != 1 || text[spans[0].Start:spans[0].End] != c.value {
			t.Errorf("secretSpans(%q) = %+v, want exactly the value at %d", text, spans, len(c.label))
		}
	}
	// Every occurrence gets a span, unlike scanSecrets, so a masker can cover them all.
	if got := secretSpans(f["github"] + " " + f["github"]); len(got) != 2 {
		t.Errorf("repeated token: got %d spans, want 2", len(got))
	}
}
