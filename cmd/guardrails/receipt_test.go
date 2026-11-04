package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestReceiptLine(t *testing.T) {
	ev := func(kr ...string) []guardEvent {
		var evs []guardEvent
		for _, s := range kr {
			k, r, _ := strings.Cut(s, " ")
			evs = append(evs, guardEvent{k, r})
		}
		return evs
	}
	tests := []struct {
		name string
		evs  []guardEvent
		want string // "" means stay silent
	}{
		{"nothing", nil, ""},
		{"two nudges stay silent", ev("nudge read-dedup", "nudge read-dedup"), ""},
		{"three nudges print", ev("nudge read-dedup", "nudge large-read", "nudge read-dedup"),
			"guards · nudged 3 (read-dedup 2, large-read 1)"},
		{"one ask prints", ev("ask egress:pii"), "guards · asked 1 (egress)"},
		{"every kind, fixed order, rule prefix only", ev("nudge slow-op", "ask settings:edit", "mask secret-mask",
			"block dangerous:git", "mask pii-wrap", "mask secret-mask", "ask egress:secret"),
			"guards · masked 3 (secret-mask 2, pii-wrap 1) · asked 2 (egress 1, settings 1) · blocked 1 (dangerous) · nudged 1 (slow-op)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := receiptLine(tt.evs); got != tt.want {
				t.Errorf("receiptLine = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestReceiptHook(t *testing.T) {
	t.Setenv("HANDOFF_STATE_DIR", t.TempDir())
	stop := `{"hook_event_name":"Stop","session_id":"s","stop_hook_active":false}`
	run := func() string {
		t.Helper()
		code, out, errOut := runHook(t, receipt, stop)
		if code != 0 || errOut != "" {
			t.Fatalf("receipt must always exit 0 quietly, got exit %d stderr %q", code, errOut)
		}
		return out
	}

	logGuardEvent("s", "nudge", "read-dedup")
	if out := run(); out != "" {
		t.Fatalf("one nudge: want silence, got %q", out)
	}
	logGuardEvent("s", "ask", "egress:pii")
	out := run()
	var r map[string]any
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("want one JSON reply, got %q: %v", out, err)
	}
	if r["systemMessage"] != "guards · asked 1 (egress) · nudged 1 (read-dedup)" {
		t.Errorf("systemMessage = %q, want the held-back nudge counted too", r["systemMessage"])
	}
	if len(r) != 1 {
		t.Errorf("reply must hold only systemMessage (no decision: gate owns Stop blocking), got %v", r)
	}
	if out := run(); out != "" {
		t.Errorf("second Stop with no new events: want silence, got %q", out)
	}
	if _, out, _ := runHook(t, receipt, `{"session_id":"../x"}`); out != "" {
		t.Errorf("unsafe session: want silence, got %q", out)
	}
}
