package main

import (
	"strings"
	"testing"
)

// Real transcripts write the bridge-session line immediately before the bridge_status notice.
func TestStatuslineRemoteControlRealOrder(t *testing.T) {
	tests := []struct {
		name  string
		lines []string
		want  bool
	}{
		// FAILS on 76ce794 (HIGH 1).
		{"just activated: marker then active notice", append([]string{slBridge, slRCActive}, slFillers(0, 5)...), true},
		{"re-activated after a disconnect", append([]string{slBridge, slRCDown, slFiller(1), slBridge, slRCActive}, slFillers(0, 5)...), true},
		{"active notice newer than a disconnect, marker only before both", []string{slBridge, slRCDown, slFiller(1), slRCActive, slFiller(2)}, true},
		// M2
		{"user prompt quoting the notice", []string{slBridge, `{"parentUuid":"p","isSidechain":false,"type":"user","message":{"role":"user","content":"Remote Control disconnected again, why?"},"uuid":"u"}`, slFiller(1)}, true},
		// M8
		{"long bridge-session line still counts", []string{`{"type":"bridge-session","sessionId":"s","pad":"` + strings.Repeat("p", rcNoticeMaxLen) + `"}`, slFiller(1)}, true},
		// M24
		{"prompt naming bridge_status after a disconnect", []string{slRCDown, `{"parentUuid":"p","isSidechain":false,"type":"user","message":{"role":"user","content":"is bridge_status written on reconnect?"},"uuid":"u"}`, slBridge, slFiller(1)}, false},
		// M25
		{"informational entry mentioning the text mid-sentence", []string{slBridge, `{"parentUuid":null,"isSidechain":false,"type":"system","subtype":"informational","content":"Tip: if you see Remote Control disconnected, run /remote-control","level":"info"}`, slFiller(1)}, true},
		// M22
		{"2 KiB disconnect notice (long cwd)", []string{slBridge, slFiller(1), strings.Replace(slRCDown, `"level"`, `"cwd":"/`+strings.Repeat("d", 2<<10)+`","level"`, 1)}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := strings.Contains(slRC(t, tc.lines...), "⇄ rc"); got != tc.want {
				t.Errorf("segment shown = %v, want %v", got, tc.want)
			}
		})
	}
}

// M13: a disconnect 800 KiB back is inside the documented 1 MiB.
func TestStatuslineRemoteControlNoticeAt800KiB(t *testing.T) {
	lines := []string{slRCDown}
	for i := 0; i < 40; i++ {
		lines = append(lines, `{"type":"api-request-blob","blob":"`+strings.Repeat("x", 20<<10)+`"}`)
	}
	lines = append(lines, slBridge)
	if strings.Contains(slRC(t, lines...), "⇄ rc") {
		t.Error("disconnect 800 KiB back ignored")
	}
}
