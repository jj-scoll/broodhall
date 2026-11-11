package main

import (
	"reflect"
	"testing"
)

// The tailer commits under memdWriteMu, so creating the alias there must not take it again.
func TestTailerCreatesAlias(t *testing.T) {
	e := newTailEnv(t, "")
	seedLegacy(t, e.db, "tailproj", "", "t")
	cwd := "/work/tailproj"
	tailAppend(t, e.path("sess-t2.jsonl"), `{"type":"assistant","sessionId":"sess-t2","cwd":"`+cwd+`","entrypoint":"cli","timestamp":"`+
		tailTS(10, 30, 0)+`","message":{"role":"assistant","stop_reason":"end_turn","content":[]}}`+"\n")
	tailScan(t, e.tailer())
	if got, want := aliasRows(t, e.db), [][2]string{{"tailproj", projectKey(cwd)}}; !reflect.DeepEqual(got, want) {
		t.Errorf("aliases = %v, want %v", got, want)
	}
}
