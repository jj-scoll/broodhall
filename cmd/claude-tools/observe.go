package main

// Observe mode: the guards run, but an ask or a block from an optional family is not enforced.
// The call goes through with a note for Claude, and the ledger records "would-ask" or
// "would-block" with the rule id, so a new user can see what the guards would do before they
// let them stop anything. One global switch (CLAUDE_TOOLS_OBSERVE, the observe plugin option).
//
// Observe covers exactly the five families disabled_guards can name (git, egress, pii,
// raw-client-data, session-url). The core blocks, the settings-file guard and the secret-file
// guard have no switch in docs/guardrails/options.md and stay enforced: observing them would
// let a destructive command run or a secret reach the model on day one.

import (
	"fmt"
	"strings"
)

// observeOn parses the observe option: true, 1 and yes turn it on, like fail_open.
func observeOn(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "1", "yes":
		return true
	}
	return false
}

// observed reports whether d, which an optional family just produced, is let through instead
// of enforced. When it is, the decision is kept for the ledger and the note. The caller then
// carries on to the later rules, so an observed rule never hides an enforced one.
func (e *guardEnv) observed(d decision) bool {
	if !e.Observe {
		return false
	}
	e.Would = append(e.Would, d)
	return true
}

// wouldKind is the ledger kind for an observed decision.
func wouldKind(d decision) string {
	if d.Ask {
		return "would-ask"
	}
	return "would-block"
}

// observeNote is the model-visible line for observed decisions. Claude sees what the guard
// said, so it can adjust; the user sees the receipt line.
func observeNote(ds []decision) string {
	var parts []string
	for _, d := range ds {
		what := "been blocked"
		if d.Ask {
			what = "asked you first"
		}
		rule, _, _ := strings.Cut(d.Rule, " ")
		parts = append(parts, fmt.Sprintf("Guardrails observe mode: this call would have %s (%s) and ran anyway. %s", what, rule, d.Msg))
	}
	return strings.Join(parts, "\n\n")
}
