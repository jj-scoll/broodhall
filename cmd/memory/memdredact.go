package main

// memory ingest redaction: every text field is masked with the existing secret maskers
// (secretSpans shapes plus key=value secrets) before it is stored or sent to the observer.
// Callers mask the full string first and truncate afterwards, so a cut can never expose
// a fragment of a secret.

import "strings"

// memdRedactor uses a fixed key: tags only need to be deterministic (idempotent masking,
// equal values equal tags within the store), and a 6-hex tag of a masked value is not a
// secret, so no key file is read or created on the ingest path.
var memdRedactor = newRedactor([]byte("memory-ingest-v1"), nil)

// memdRedact masks every secret shape and secret key=value in s. Masked output is
// stable under a second pass.
//
// Accepted risk (2026-10-28; R21, R22): the key=value rule is narrowed here only.
// Under a secret key the unquoted value is the first whitespace-delimited token (minus one
// trailing "," or ";"); it is always masked, and the walk goes on after it so later pairs
// on the line are still checked. The one exception is a purely numeric token under the
// plural count words TOKENS or KEYS (input_tokens: 1994); numeric values under any other
// secret word (password=12345678, token: 42) are masked. A multi-word passphrase
// (password: correct horse battery) therefore has its first word masked and the rest
// exposed, unless a shape pattern catches it. Go test result lines (--- PASS: TestX
// (0.01s)) get no key-value masking; shapes still apply to them. Quoted values are masked
// as before. The unnarrowed rule measured 8.3% of tool results, mostly false positives
// (PASS: lines, input_tokens: N, rg hits in api.tf). maskSecrets keeps the full rule.
func memdRedact(s string) string { return memdRedactor.maskSecretsMode(s, true) }

// cmdNorm strips one leading "rtk " token from a shell command.
func cmdNorm(cmd string) string { return strings.TrimPrefix(cmd, "rtk ") }
