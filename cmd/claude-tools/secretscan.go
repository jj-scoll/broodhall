// Ported from NVIDIA NemoClaw nemoclaw/src/security/secret-scanner.ts (patterns and
// scanForSecrets; the OpenClaw memory-path classifier is left out).
// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
// Changed from the original: Go's regexp (RE2) has no lookaround, so the three lookbehind
// patterns match their label and capture the value in group 1, and OpenAI's (?!ant-) is a
// check on the matched text. secretSpans also reports every occurrence with its byte
// offsets, for masking, and the private key pattern covers the block through its END line
// (and any key type, ENCRYPTED and PGP included) instead of the BEGIN line alone.

package main

import (
	"fmt"
	"regexp"
	"strings"
)

// High-confidence secret shapes: anchored to known prefixes or labels to keep false
// positives low. Base64- or hex-encoded secrets, and secrets split across writes, get past
// any regex scanner.
var secretPatterns = []secretPattern{
	prefixedToken("NVIDIA API key", "A-Za-z0-9_-", 20, "nvapi-"),
	prefixedToken("GitHub token", "A-Za-z0-9", 36, "ghp_", "gho_", "ghu_", "ghs_", "ghr_"),
	prefixedToken("GitHub token", "A-Za-z0-9_", 30, "github_pat_"),
	prefixedToken("npm token", "A-Za-z0-9", 36, "npm_"),
	{"OpenAI API key", regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{20,}\b`), 0}, // sk-ant- is dropped in secretSpans
	{"AWS access key", regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`), 0},
	{"AWS secret key", regexp.MustCompile(`(?i)aws_secret_access_key\s*[=:]\s*([A-Za-z0-9/+=]{40})\b`), 1},
	{"Slack token", regexp.MustCompile(`\b(?:xox[bpas]|xapp)-[A-Za-z0-9-]{10,}\b`), 0},
	// Discord needs its label so JWTs and other base64 strings don't match.
	{"Discord bot token", regexp.MustCompile(`(?:discord|bot|DISCORD_TOKEN|BOT_TOKEN|token)\s*[=:]\s*["']?([A-Za-z0-9]{24}\.[A-Za-z0-9_-]{6}\.[A-Za-z0-9_-]{27,})`), 1},
	// The whole block, so masking hides the key body; with no END line, the rest of the text.
	{"Private key", regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY(?: BLOCK)?-----(?s:.*?)(?:-----END [A-Z ]*PRIVATE KEY(?: BLOCK)?-----|\z)`), 0},
	{"Authorization header", regexp.MustCompile(`(?i)(?:Authorization\s*:\s*Bearer|Bearer\s*[=:])\s*["']?([A-Za-z0-9._~+/=-]{40,})`), 1},
	{"Telegram bot token", regexp.MustCompile(`\b\d{8,10}:[A-Za-z0-9_-]{35}\b`), 0},
	{"Google API key", regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`), 0},
	{"Anthropic API key", regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_-]{20,}\b`), 0},
	{"HuggingFace token", regexp.MustCompile(`\bhf_[A-Za-z0-9]{20,}\b`), 0},
	// Not upstream. Atlassian tokens are ATATT3 + 186 (base64url, then =<8 hex checksum).
	{"Atlassian API token", regexp.MustCompile(`\bATATT3[A-Za-z0-9_=-]{150,}`), 0},
	// Not upstream: SSO sessions use temporary ASIA keys, and `aws configure
	// export-credentials` prints the secret and session token as JSON.
	{"AWS temporary access key", regexp.MustCompile(`\bASIA[0-9A-Z]{16}\b`), 0},
	{"AWS secret key", regexp.MustCompile(`"SecretAccessKey"\s*:\s*"([A-Za-z0-9/+=]{40})(?:[^A-Za-z0-9/+=]|$)`), 1},
	{"AWS session token", regexp.MustCompile(`(?i)(?:aws_session_token|"SessionToken")\s*[=:]\s*["']?([A-Za-z0-9/+=]{100,})`), 1},
}

type secretPattern struct {
	name  string
	re    *regexp.Regexp
	group int // submatch holding the secret; 0 is the whole match
}

// prefixedToken is a provider token: one of prefixes, then at least min characters of class.
func prefixedToken(name, class string, min int, prefixes ...string) secretPattern {
	return secretPattern{name, regexp.MustCompile(fmt.Sprintf(`\b(?:%s)[%s]{%d,}\b`, strings.Join(prefixes, "|"), class, min)), 0}
}

type secretMatch struct{ Pattern, Redacted string }

type secretSpan struct {
	Pattern    string
	Start, End int
}

// secretSpans is every secret-shaped value in s with its byte offsets, pattern by pattern
// (spans from different patterns can overlap).
func secretSpans(s string) []secretSpan {
	var out []secretSpan
	for _, p := range secretPatterns {
		for _, m := range p.re.FindAllStringSubmatchIndex(s, -1) {
			start, end := m[2*p.group], m[2*p.group+1]
			if p.name == "OpenAI API key" && strings.HasPrefix(s[start:end], "sk-ant-") {
				continue
			}
			out = append(out, secretSpan{p.name, start, end})
		}
	}
	return out
}

// scanSecrets is scanForSecrets: one match per distinct pattern and value, with a preview
// that keeps the first and last 4 characters.
func scanSecrets(s string) []secretMatch {
	var out []secretMatch
	seen := map[string]bool{}
	for _, sp := range secretSpans(s) {
		v := s[sp.Start:sp.End]
		if seen[sp.Pattern+":"+v] {
			continue
		}
		seen[sp.Pattern+":"+v] = true
		r := "****"
		if len(v) > 8 {
			r = v[:4] + ".." + v[len(v)-4:]
		}
		out = append(out, secretMatch{sp.Pattern, r})
	}
	return out
}
