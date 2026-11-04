package main

import (
	"bufio"
	"os"
	"strings"
	"testing"
	"time"
)

// TestGuardCorpus pins both rule sets on testdata/guard-corpus.tsv. It replaces byte-parity
// with the bash hook for the rules that deliberately diverge from it.
func TestGuardCorpus(t *testing.T) {
	f, err := os.Open("testdata/guard-corpus.tsv")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	n := 0
	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) != 3 {
			t.Fatalf("bad corpus line: %q", line)
		}
		wantOld, wantNew, cmd := parts[0], parts[1], strings.ReplaceAll(parts[2], `\n`, "\n")
		n++
		env := func() *guardEnv {
			return &guardEnv{Now: time.Unix(1_800_000_000, 0), Session: "corpus", Cwd: "/work", Home: "/nohome/user", State: memState{}}
		}
		if got := decideLegacy(env(), cmd).Kind(); got != wantOld {
			t.Errorf("legacy %q: got %s, want %s", cmd, got, wantOld)
		}
		if got := decide(env(), cmd).Kind(); got != wantNew {
			t.Errorf("current %q: got %s, want %s", cmd, got, wantNew)
		}
	}
	if n == 0 {
		t.Fatal("empty corpus")
	}
}
