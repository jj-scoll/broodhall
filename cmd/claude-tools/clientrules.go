package main

// Client-specific rule data, supplied by the user. Nothing is configured by default, so
// raw-client-data asks only for Python reading local parquet files and exempts no path.
// The plugin options client_data_buckets and masked_copy_dirs reach the binary as
// CLAUDE_TOOLS_CLIENT_BUCKETS and CLAUDE_TOOLS_MASKED_DIRS (run.sh sets them).

import (
	"os"
	"regexp"
	"strings"
)

// clientDataS3RE matches S3 objects that hold raw client data; nil means there are none.
var clientDataS3RE *regexp.Regexp

// maskedCopyDirs are directories that hold masked copies of client data: a parquet path
// that contains one is not raw client data. Empty means no exemption.
var maskedCopyDirs []string

func init() { loadClientRules() }

// loadClientRules reads the user's client rules from the environment.
func loadClientRules() {
	clientDataS3RE = clientBucketRegexp(os.Getenv("CLAUDE_TOOLS_CLIENT_BUCKETS"))
	maskedCopyDirs = maskedCopyDirs[:0:0]
	for _, d := range parseList(os.Getenv("CLAUDE_TOOLS_MASKED_DIRS")) {
		// "~/x/" and "/home/u/x/" name the same place; the command text may use either.
		if d = strings.TrimPrefix(d, "~"); d != "" && d != "/" {
			maskedCopyDirs = append(maskedCopyDirs, d)
		}
	}
}

// parseList splits a comma-separated option, dropping blanks.
func parseList(s string) []string {
	var out []string
	for _, f := range strings.Split(s, ",") {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

// clientBucketRegexp turns bucket-name patterns ("*-clientdata", "acme-raw"; `*` matches
// any run of name characters) into one regexp over s3://bucket/key text. No patterns gives nil.
func clientBucketRegexp(list string) *regexp.Regexp {
	var alts []string
	for _, p := range parseList(list) {
		alts = append(alts, strings.ReplaceAll(regexp.QuoteMeta(p), `\*`, `[^/\s'"]*`))
	}
	if len(alts) == 0 {
		return nil
	}
	return regexp.MustCompile(`s3://(?:` + strings.Join(alts, "|") + `)/[^\s'"]*`)
}

// isMaskedCopy reports whether a parquet path points into a configured masked-copy directory.
func isMaskedCopy(p string) bool {
	for _, d := range maskedCopyDirs {
		if strings.Contains(p, d) {
			return true
		}
	}
	return false
}

// Tools the guard messages point the model at; empty when this build has none.
const (
	structureTool  = "" // large-read-guard: file structure
	maskedCopyTool = "" // raw-client-data: masked parquet copy
)
