package main

// receipt (Stop hook): one line for the user about what the guards did since the last receipt,
// e.g. "guards · masked 2 (secret-mask) · asked 1 (egress) · nudged 4 (read-dedup 3, slow-op 1)".
// The guards' masks, rewrites and nudges otherwise reach only Claude. The line goes out as
// systemMessage, which Claude Code shows in the transcript and does not send to the model
// (checked on 2.1.312). Silent on quiet turns: a mask, ask or block always prints, nudges only
// from 3 up, and fewer wait for the next receipt. Fails open: every path exits 0 with no decision, so a missing or unreadable ledger never
// blocks Stop.

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

func receipt(args []string) int {
	var in struct {
		SessionID string `json:"session_id"`
	}
	data, err := io.ReadAll(os.Stdin)
	if err != nil || json.Unmarshal(data, &in) != nil {
		return 0
	}
	evs, commit := newGuardEvents(in.SessionID)
	line := receiptLine(evs)
	if line == "" {
		return 0
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	enc.Encode(map[string]string{"systemMessage": line})
	commit()
	return 0
}

// receiptLine summarizes events by kind, each with its rules (the part before any ':'),
// counted when there are several, or returns "" when the turn is not worth a line.
func receiptLine(evs []guardEvent) string {
	byKind := map[string]map[string]int{}
	for _, e := range evs {
		if byKind[e.Kind] == nil {
			byKind[e.Kind] = map[string]int{}
		}
		rule, _, _ := strings.Cut(e.Rule, ":")
		byKind[e.Kind][rule]++
	}
	if len(byKind["mask"])+len(byKind["ask"])+len(byKind["block"]) == 0 && len(evs) < 3 {
		return ""
	}
	parts := []string{"guards"}
	for _, k := range []struct{ kind, label string }{{"mask", "masked"}, {"ask", "asked"}, {"block", "blocked"}, {"nudge", "nudged"}} {
		rules := byKind[k.kind]
		if len(rules) == 0 {
			continue
		}
		names, total := make([]string, 0, len(rules)), 0
		for r, n := range rules {
			names, total = append(names, r), total+n
		}
		sort.Slice(names, func(i, j int) bool {
			if rules[names[i]] != rules[names[j]] {
				return rules[names[i]] > rules[names[j]]
			}
			return names[i] < names[j]
		})
		detail := names
		if len(names) > 1 {
			detail = make([]string, len(names))
			for i, r := range names {
				detail[i] = fmt.Sprintf("%s %d", r, rules[r])
			}
		}
		parts = append(parts, fmt.Sprintf("%s %d (%s)", k.label, total, strings.Join(detail, ", ")))
	}
	return strings.Join(parts, " · ")
}
