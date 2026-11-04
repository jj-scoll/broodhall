package main

// redact --secrets: mask credentials in command output (secret files read through cat, head,
// grep, ...) so Claude can use a file's shape without seeing its values. Every secretSpans
// hit becomes [pattern-slug:tag], and values under secret-looking key names (password,
// api_token, clientSecret, ...) in toml, ini, env, yaml and json become [secret:tag], also
// after a grep or cat -n prefix. Tags are the --pii keyed hash, so equal values keep equal
// tags. Not caught: values on the lines after a YAML block scalar (key: |) or inside a
// multi-line JSON array, and a value printed without its key (grep -o, jq -r .password),
// which is why pre-tool-use only masks readers that print whole lines.

import (
	"regexp"
	"sort"
	"strings"
)

var (
	secretKeyRE = regexp.MustCompile(`(?:^|_)(?:TOKENS?|KEYS?|SECRETS?|PASSWORDS?|PASSWD|PASS|CREDENTIALS?|AUTH|API)(?:_|$)`)
	// keyCandRE is a key and its separator (password =, "apiKey":, DB_PASSWORD=). The key may
	// not follow "[", so the slug of a tag is never taken for a key.
	keyCandRE = regexp.MustCompile(`(?:^|[^\[A-Za-z0-9_.-])([A-Za-z0-9_.-]+)["']?[ \t]*[=:][ \t]*`)
	onlyTagRE = regexp.MustCompile(`^\[[a-z0-9-]+:[0-9a-f]{6}\]$`)
)

func (x *redactor) maskSecrets(s string) string { return x.maskSecretsMode(s, false) }

func (x *redactor) maskSecretsMode(s string, narrow bool) string {
	lines := strings.Split(x.maskSecretShapes(s), "\n")
	for i, l := range lines {
		if narrow && goTestLineRE.MatchString(l) {
			continue
		}
		lines[i] = x.walkKeyValues(l, narrow)
	}
	return strings.Join(lines, "\n")
}

var (
	numericRE = regexp.MustCompile(`^[0-9][0-9_.,]*$`)
	// secretWordRE is one "_"-separated segment of a normalized key that is a secret word
	// other than the plural count words TOKENS and KEYS.
	secretWordRE = regexp.MustCompile(`^(?:TOKEN|KEY|SECRETS?|PASSWORDS?|PASSWD|PASS|CREDENTIALS?|AUTH|API)$`)
	// goTestLineRE is a Go test result line (--- PASS: TestX (0.01s)); memd skips key-value
	// masking on it, shapes still apply.
	goTestLineRE = regexp.MustCompile(`^\s*--- (?:PASS|FAIL|SKIP): (?:Test|Benchmark|Example|Fuzz)\S* \([0-9.]+s\)\s*$`)
)

// narrowToken is the unquoted value of narrow mode: the first whitespace-delimited token of
// rest, minus one trailing "," or ";" so the mask never swallows a separator. keep reports a
// purely numeric token under a count word (input_tokens: 1994, KEYS: 12), which holds no
// secret; a numeric value under any other secret word is masked (password=12345678).
func narrowToken(key, rest string) (tok string, keep bool) {
	if i := strings.IndexAny(rest, " \t\r"); i >= 0 {
		tok = rest[:i]
	} else {
		tok = rest
	}
	tok = strings.TrimSuffix(strings.TrimSuffix(tok, ","), ";")
	if !numericRE.MatchString(tok) {
		return tok, false
	}
	for _, seg := range strings.Split(normKey(key), "_") {
		if secretWordRE.MatchString(seg) {
			return tok, false
		}
	}
	return tok, true
}

// maskSecretShapes replaces every secretSpans hit, merging overlapping spans.
func (x *redactor) maskSecretShapes(s string) string {
	spans := secretSpans(s)
	sort.Slice(spans, func(i, j int) bool {
		if spans[i].Start != spans[j].Start {
			return spans[i].Start < spans[j].Start
		}
		return spans[i].End > spans[j].End
	})
	var b strings.Builder
	at := 0
	for i := 0; i < len(spans); {
		sp, end := spans[i], spans[i].End
		for i++; i < len(spans) && spans[i].Start < end; i++ {
			end = max(end, spans[i].End)
		}
		b.WriteString(s[at:sp.Start])
		b.WriteString(x.tag(strings.ToLower(strings.ReplaceAll(sp.Pattern, " ", "-")), s[sp.Start:end]))
		at = end
	}
	return b.String() + s[at:]
}

// maskKeyValues walks the key candidates of one line left to right. A quoted value under a
// secret key is masked up to its closing quote and the walk goes on after it; an unquoted
// one is masked to the end of the line.
func (x *redactor) maskKeyValues(line string) string { return x.walkKeyValues(line, false) }

// walkKeyValues is the shared walk. With narrow set (memd only) an unquoted value is just
// its first whitespace-delimited token (see narrowToken) and the walk goes on after it.
func (x *redactor) walkKeyValues(line string, narrow bool) string {
	var b strings.Builder
	for {
		m := keyCandRE.FindStringSubmatchIndex(line)
		if m == nil {
			break
		}
		b.WriteString(line[:m[1]])
		key, rest := line[m[2]:m[3]], line[m[1]:]
		line = rest
		if !isSecretKey(key) {
			continue
		}
		if q := rest[:min(1, len(rest))]; q == `"` || q == "'" {
			if end := closingQuote(rest); end > 0 {
				if v := rest[1:end]; maskable(v) {
					b.WriteString(q + x.tag("secret", v))
					line = rest[end:]
				}
				continue
			}
		}
		if narrow {
			if v, keep := narrowToken(key, rest); !keep && maskable(v) {
				b.WriteString(x.tag("secret", v))
				line = rest[len(v):]
			}
			continue
		}
		if v := strings.TrimRight(rest, " \t\r"); maskable(v) {
			b.WriteString(x.tag("secret", v))
			line = rest[len(v):]
			break
		}
	}
	return b.String() + line
}

// isSecretKey: password, api_token, clientSecret, secret-key, --password, AUTH, ...
func isSecretKey(key string) bool { return secretKeyRE.MatchString(normKey(key)) }

func normKey(key string) string {
	return strings.NewReplacer("-", "_", ".", "_").Replace(strings.ToUpper(camelRE.ReplaceAllString(key, "${1}_$2")))
}

// maskable is false for values that hold no secret: empty, booleans, nulls, the opening of
// a nested block, and values already masked by shape.
func maskable(v string) bool {
	switch strings.ToLower(v) {
	case "", "true", "false", "null", "none", "~", "[", "|", ">":
		return false
	}
	return !strings.HasPrefix(v, "{") && !onlyTagRE.MatchString(v)
}

// closingQuote is the index of the quote that closes s[0], or -1. Backslash escapes count
// inside double quotes only.
func closingQuote(s string) int {
	for i := 1; i < len(s); i++ {
		switch {
		case s[0] == '"' && s[i] == '\\':
			i++
		case s[i] == s[0]:
			return i
		}
	}
	return -1
}
