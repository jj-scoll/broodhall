package main

import (
	"strings"
	"testing"
)

// This build has neither of the private tools, so no guard message may send the model to them.
// Names are split so they stay out of the public tree.
func TestGuardMessagesNameNoPrivateTools(t *testing.T) {
	for i, msg := range guardHintMessages(t) {
		if msg == "" {
			t.Errorf("message %d is empty", i)
		}
		for _, private := range []string{"smart-explore", "claude-" + "mem", "pii-" + "mirror"} {
			if strings.Contains(msg, private) {
				t.Errorf("message %d = %q names %s", i, msg, private)
			}
		}
	}
}

func TestClientDataBucketRegexp(t *testing.T) {
	if re := clientBucketRegexp(""); re != nil {
		t.Errorf("empty config gave %v, want nil", re)
	}
	if re := clientBucketRegexp(" , "); re != nil {
		t.Errorf("blank config gave %v, want nil", re)
	}
	re := clientBucketRegexp("*-clientdata, acme-raw")
	if re == nil {
		t.Fatal("nil regexp")
	}
	for _, s := range []string{"s3://acme-prod-clientdata/data/x.parquet", "s3://acme-raw/a", "s3://-clientdata/k"} {
		if re.FindString(s) == "" {
			t.Errorf("no match for %q", s)
		}
	}
	for _, s := range []string{"s3://acme-own/a", "s3://acme-raw2/a", "s3://xacme-raw/a", "s3://acme-clientdata-bak/a", "https://acme-raw/a"} {
		if m := re.FindString(s); m != "" {
			t.Errorf("unexpected match %q in %q", m, s)
		}
	}
	// Regexp metacharacters in a name are literal.
	if re := clientBucketRegexp("a.b"); re.MatchString("s3://aXb/k") {
		t.Error("dot matched any character")
	}
}

func TestMaskedCopyDirs(t *testing.T) {
	withMaskedDirs(t, nil)
	env := envAt(memState{}, t0, "/w")
	cmd := `python3 q.py ~/.cache/masked/acme/x.parquet`
	if d := decide(env, cmd); d.Kind() != "ask:raw-client-data" {
		t.Fatalf("nothing configured: %+v, want ask (no exemption)", d)
	}
	t.Setenv("CLAUDE_TOOLS_MASKED_DIRS", "~/.cache/masked/, /srv/masked")
	loadClientRules()
	t.Cleanup(func() { t.Setenv("CLAUDE_TOOLS_MASKED_DIRS", ""); loadClientRules() })
	for _, c := range []string{cmd, `python3 q.py /home/u/.cache/masked/acme/x.parquet`, `python3 q.py /srv/masked/x.parquet`} {
		if d := decide(env, c); d.Kind() == "ask:raw-client-data" {
			t.Errorf("masked copy asked for %q", c)
		}
	}
	for _, c := range []string{`python3 q.py /srv/other/x.parquet`, `python3 q.py masked/x.parquet`} {
		if d := decide(env, c); d.Kind() != "ask:raw-client-data" {
			t.Errorf("%q: %+v, want ask", c, d)
		}
	}
}

// The binary reads its client rules from the environment run.sh fills from plugin options.
func TestClientRulesFromEnv(t *testing.T) {
	t.Setenv("CLAUDE_TOOLS_CLIENT_BUCKETS", "*-clientdata")
	t.Setenv("CLAUDE_TOOLS_MASKED_DIRS", "/srv/masked")
	loadClientRules()
	t.Cleanup(func() {
		t.Setenv("CLAUDE_TOOLS_CLIENT_BUCKETS", "")
		t.Setenv("CLAUDE_TOOLS_MASKED_DIRS", "")
		loadClientRules()
	})
	env := envAt(memState{}, t0, "/w")
	if d := decide(env, `python3 q.py s3://acme-prod-clientdata/x.parquet`); d.Kind() != "ask:raw-client-data" {
		t.Errorf("bucket from env: %+v", d)
	}
	if d := decide(env, `python3 q.py /srv/masked/x.parquet`); d.Kind() == "ask:raw-client-data" {
		t.Errorf("masked dir from env still asked: %+v", d)
	}
}
