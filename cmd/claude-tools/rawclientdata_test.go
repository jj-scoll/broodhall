package main

import (
	"regexp"
	"strings"
	"testing"
)

// withClientBuckets sets clientDataS3RE for one test: tests name their own client buckets
// (*-clientdata), whatever this build's clientrules.go holds.
func withClientBuckets(t *testing.T, re *regexp.Regexp) {
	saved := clientDataS3RE
	clientDataS3RE = re
	t.Cleanup(func() { clientDataS3RE = saved })
}

var testClientBuckets = regexp.MustCompile(`s3://[^/\s'"]*-clientdata/[^\s'"]*`)

func TestRawClientDataAsk(t *testing.T) {
	withClientBuckets(t, testClientBuckets)
	env := envAt(memState{}, t0, "/w")
	asks := []string{
		`python3 -c "import duckdb; print(duckdb.sql(\"from 's3://acme-prod-clientdata/data/x.parquet'\").fetchall())"`,
		"python3 - <<'EOF'\nimport duckdb\nprint(duckdb.sql(\"select * from 'scratch/clients.parquet'\").fetchall())\nEOF",
		`uv run python q.py s3://acme-uat-clientdata/exports/`,
		`cd /tmp/x && python -c "import pandas as pd; print(pd.read_parquet('out/*.parquet').head())"`,
	}
	for _, c := range asks {
		d := decide(env, c)
		if d.Kind() != "ask:raw-client-data" || !strings.Contains(d.Msg, "duckdb CLI") {
			t.Errorf("want ask:raw-client-data for %q, got %+v", c, d)
		}
	}
	allows := []string{
		`python3 -c "print(1)"`,
		`python3 q.py ~/.cache/pii-safe/acme-prod-clientdata/data/x.parquet`,
		`python3 q.py /home/user/.cache/pii-safe/acme-prod-clientdata/data/x.parquet`,
		`python3 -c "import duckdb; print(duckdb.sql(\"from 's3://acme-own-bucket/runs/a.parquet'\"))"`,
		`duckdb -csv -c "from 's3://acme-prod-clientdata/data/x.parquet'"`,
		`aws s3 ls s3://acme-prod-clientdata/data/`,
		`echo python3 x.parquet`,
		`python3 -c "import duckdb; duckdb.sql('from read_json(\"t.jsonl\")')"`,
	}
	for _, c := range allows {
		if d := decide(env, c); d.Kind() == "ask:raw-client-data" {
			t.Errorf("unexpected ask for %q: %s", c, d.Msg)
		}
	}
}

// With no client buckets (the public build) S3 reads pass and local parquet still asks.
func TestRawClientDataNoBuckets(t *testing.T) {
	withClientBuckets(t, nil)
	env := envAt(memState{}, t0, "/w")
	if d := decide(env, `uv run python q.py s3://acme-uat-clientdata/exports/`); d.Kind() == "ask:raw-client-data" {
		t.Errorf("S3 ask with no client buckets: %s", d.Msg)
	}
	if d := decide(env, `python3 q.py scratch/clients.parquet`); d.Kind() != "ask:raw-client-data" {
		t.Errorf("local parquet: want ask:raw-client-data, got %+v", d)
	}
}

// A duckdb dot-command that changes how or where results print undoes the forced -json, so
// redact --pii falls back to email/phone shapes and names pass (seen with .mode csv/line).
func TestDuckdbDotCommandBlocks(t *testing.T) {
	env := envAt(memState{}, t0, "/w")
	blocks := []string{
		"duckdb <<'EOF'\n.mode csv\nselect first_name from 'c.parquet';\nEOF",
		"duckdb lake.duckdb <<'EOF'\n  .output /tmp/x.txt\nselect 1;\nEOF",
		`duckdb -cmd '.mode line' -c "from people"`,
		"duckdb <<'EOF'\n.read q.sql\nEOF",
		`printf '.once out.csv\nselect 1;\n' | duckdb`,
		"duckdb <<'EOF'\n.shell cat x\nEOF",
	}
	for _, c := range blocks {
		if d := decide(env, c); d.Kind() != "block:duckdb-dot-command" || !strings.Contains(d.Msg, "-csv") {
			t.Errorf("want block:duckdb-dot-command for %q, got %+v", c, d)
		}
	}
	for _, c := range []string{
		`duckdb -c ".tables"`, `duckdb -c "describe people"`, `jq '.mode' x.json`,
		"cat <<'EOF'\n.mode csv\nEOF", "duckdb <<'EOF'\n.maxrows 50\nselect 1;\nEOF",
	} {
		if d := decide(env, c); d.Kind() == "block:duckdb-dot-command" {
			t.Errorf("unexpected block for %q: %s", c, d.Msg)
		}
	}
}
