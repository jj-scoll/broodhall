package main

// Shared types for the PreToolUse(Bash) rule set. Rules are pure functions of the command and
// a guardEnv, so pre-tool-use and guard-replay run the same code.

import (
	"strings"
	"time"
)

// decision is what the guards say about one Bash command.
type decision struct {
	Exit    int    // 0 allow, 2 block, 1 legacy warn (non-blocking error, Claude never sees it)
	Rule    string // rule id, "" when allowed
	Msg     string // stderr text for Exit 1 or 2
	Context string // additionalContext for Exit 0 (model-visible reminder)
	Ask     bool   // Exit 0 with permissionDecision "ask": the user confirms, Msg is the reason
}

// Kind is the replay bucket: allow, block:<rule>, warn:<rule>, ask:<rule>, context:<rule>.
func (d decision) Kind() string {
	switch {
	case d.Ask:
		return "ask:" + d.Rule
	case d.Exit == 2:
		return "block:" + d.Rule
	case d.Exit == 1:
		return "warn:" + d.Rule
	case d.Context != "":
		return "context:" + d.Rule
	}
	return "allow"
}

// guardState remembers keyed events per session (plans run, reminders sent).
type guardState interface {
	// Seen reports whether key was marked within the window ending at now.
	Seen(key string, within time.Duration, now time.Time) bool
	Mark(key string, now time.Time)
}

// guardEnv is everything a rule may look at besides the command itself.
type guardEnv struct {
	Now     time.Time
	Session string
	Cwd     string
	Home    string
	// XDGConfig is the hook process's XDG_CONFIG_HOME (not the model's: Bash commands never
	// change it), where the PII rules file may live.
	XDGConfig string
	State     guardState
	// GitUnpushed returns the messages of commits in dir that are on no remote, each led by
	// a \x01 and its short hash. Nil in replay: replay has no repositories.
	GitUnpushed func(dir string) string
	// Off holds the rule families CLAUDE_TOOLS_OFF turns off (pii, raw-client-data,
	// session-url, egress, git). Nil runs every rule.
	Off map[string]bool
}

// guardsOff parses CLAUDE_TOOLS_OFF, a comma-separated list of rule families.
func guardsOff(s string) map[string]bool {
	off := map[string]bool{}
	for _, f := range strings.Split(s, ",") {
		if f = strings.TrimSpace(f); f != "" {
			off[f] = true
		}
	}
	return off
}

// decider evaluates one command; guard-replay runs two of them side by side.
type decider func(env *guardEnv, cmd string) decision

// memState is an in-memory guardState, one per session, for replay and tests.
type memState map[string]time.Time

func (m memState) Seen(key string, within time.Duration, now time.Time) bool {
	t, ok := m[key]
	return ok && now.Sub(t) <= within
}

func (m memState) Mark(key string, now time.Time) { m[key] = now }

// shortCmd squeezes a command to one line of at most n runes for reports.
func shortCmd(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "..."
	}
	return s
}
