package main

// pre-tool-use (PreToolUse(Bash) hook): block dangerous Bash patterns, pass everything else.
// Exit 0 = allow | Exit 2 = block (stderr fed back to Claude as reasoning) | Exit 1 = warn.
// Port of ~/.claude/hooks/pre-tool-use.sh; test-pre-tool-use.sh is its suite. The rules live
// in decide() so guard-replay can run them over history.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"
)

// supersededLegacy are the legacy substrings that decide() replaces with parsed rules.
var supersededLegacy = map[string]bool{"rm -rf /": true, "rm -rf ~": true, "rm -rf $HOME": true, "git push --force": true, "git push -f ": true}

// decide is the current rule set. guard-replay measures it against decideLegacy.
func decide(env *guardEnv, cmd string) decision {
	// The model must not loosen the PII rules (piirulesguard.go).
	if d := piiRulesGuard(env, cmd); d.Exit != 0 && !env.Off["pii"] && !env.observed(d) {
		return d
	}
	// rm of the filesystem root, home or system dirs: exact-target rule, not a substring.
	if t, ok := rmRootTarget(cmd, env.Home); ok {
		return decision{Exit: 2, Rule: "dangerous:rm -rf " + t,
			Msg: "BLOCKED: 'rm -rf " + t + "' is a high-risk destructive operation. Confirm with the user before attempting this."}
	}
	// Force pushes: a bare --force or -f blocks; --force-with-lease / --force-if-includes pass.
	if f := forcePushFlag(cmd); f != "" {
		p := map[string]string{"--force": "git push --force", "-f": "git push -f "}[f]
		return decision{Exit: 2, Rule: "dangerous:" + p,
			Msg: "BLOCKED: '" + p + "' is a high-risk destructive operation. Confirm with the user before attempting this."}
	}
	if d := shellRules(env, cmd); d.Exit != 0 {
		return d
	}
	// Text the parser cannot read gets the rm and force-push rules on loose words (shellloose.go).
	if _, ok := parseShell(cmd); !ok {
		if what := looseDestructive(cmd, env.Home); what != "" {
			return decision{Ask: true, Rule: "unparsed-destructive",
				Msg: "ASK: " + what + ". Confirm with the user before running it; rewrite it as plain bash if it is safe."}
		}
	}
	// Session links must not leave the machine (git push, gh pr create|edit).
	if d := sessionURLRule(env, cmd); d.Exit != 0 && !env.Off["session-url"] && !env.observed(d) {
		return d
	}
	// duckdb dot-commands that would print around the PII mask.
	if d := duckdbDotRule(cmd); d.Exit != 0 && !env.Off["pii"] && !env.observed(d) {
		return d
	}
	// Claude Code settings files: the user confirms each model write (settingsguard.go).
	if d := settingsGuard(env, cmd); d.Ask {
		return d
	}
	// Destructive git verbs: the user confirms each one.
	var rule, what string
	if cmds, ok := parseShell(cmd); ok {
		rule, what = gitDestructive(cmds)
	} else {
		rule, what = gitDestructiveText(cmd)
	}
	if g := askDecision(rule, what); rule != "" && !env.Off["git"] && !env.observed(g) {
		return g
	}
	// Git commands that skip hooks or signing: the same confirmation, the same family.
	if !env.Off["git"] {
		var bRule, bWhat string
		if cmds, ok := parseShell(cmd); ok {
			bRule, bWhat = gitHookBypass(cmds, env.Cwd)
		} else {
			bRule, bWhat = gitHookBypassText(cmd, env.Cwd)
		}
		if bRule != "" {
			return decision{Ask: true, Rule: bRule,
				Msg: "ASK: " + bWhat + ". A failing hook or a signing error is the repository's own check; fix the cause, or confirm with the user before skipping it."}
		}
	}
	// Code that comes off the network and runs unread: the user confirms (downloadexec.go).
	if d := downloadExecRule(cmd); d.Ask {
		return d
	}
	// Python reading raw client data prints it unmasked: the user confirms.
	if d := rawClientDataRule(cmd); d.Ask && !env.Off["raw-client-data"] && !env.observed(d) {
		return d
	}
	// Secret files: masked reads, the user confirms anything else (secretguard.go).
	d := secretFileRule(env, cmd)
	if d.Ask {
		return d
	}
	// Data leaving the machine: the user confirms PII, secrets and payloads the guard can't read (egressguard.go).
	if e := egressRule(env, cmd); e.Ask && !env.Off["egress"] && !env.observed(e) {
		return e
	}
	// Warnings that Claude can actually read: exit 0 + additionalContext.
	s := slowOpRule(env, cmd)
	if d.Context == "" {
		return s
	}
	if s.Context != "" {
		d.Context += "\n\n" + s.Context
	}
	return d
}

// emitAsk prints a permissionDecision "ask" reply: Claude Code shows the reason and prompts.
func emitAsk(w io.Writer, event, reason string) {
	type hookOut struct {
		HookEventName            string `json:"hookEventName"`
		PermissionDecision       string `json:"permissionDecision"`
		PermissionDecisionReason string `json:"permissionDecisionReason"`
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	enc.Encode(struct {
		HookSpecificOutput hookOut `json:"hookSpecificOutput"`
	}{hookOut{event, "ask", reason}})
}

// emitContext prints an additionalContext reply: model-visible, shown next to the tool
// result, never blocks.
func emitContext(w io.Writer, event, msg string) {
	type hookOut struct {
		HookEventName     string `json:"hookEventName"`
		AdditionalContext string `json:"additionalContext"`
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	enc.Encode(struct {
		HookSpecificOutput hookOut `json:"hookSpecificOutput"`
	}{hookOut{event, msg}})
}

func preToolUse(args []string) int {
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return 0
	}
	self, _ := os.Executable()
	return preToolUseRun(data, self, rtkHook, os.Stdout, os.Stderr)
}

// preToolUseRun is the whole hook. It is also the only Bash rewriter: Claude Code runs the
// PreToolUse hooks in parallel on the original input and keeps the updatedInput of whichever
// finishes last (measured on 2.1.311), so this hook runs `rtk hook claude` itself instead of
// racing it, and SQL clients get the PII wrap rather than rtk's rewrite.
func preToolUseRun(data []byte, self string, rtk func([]byte) ([]byte, []byte, int), stdout, stderr io.Writer) int {
	var in struct {
		SessionID string `json:"session_id"`
		Cwd       string `json:"cwd"`
		ToolName  string `json:"tool_name"`
		AgentID   string `json:"agent_id"`
		AgentID2  string `json:"agentId"`
		ToolInput struct {
			Command string `json:"command"`
		} `json:"tool_input"`
	}
	if json.Unmarshal(data, &in) != nil {
		return 0
	}
	home, _ := os.UserHomeDir()
	xdg := os.Getenv("XDG_CONFIG_HOME")
	off := guardsOff(os.Getenv("CLAUDE_TOOLS_OFF"))
	observe := observeOn(os.Getenv("CLAUDE_TOOLS_OBSERVE"))
	if in.ToolName != "Bash" {
		var raw struct {
			ToolInput map[string]any `json:"tool_input"`
		}
		json.Unmarshal(data, &raw)
		if d, ok := rulesFileToolDeny(in.ToolName, raw.ToolInput, in.Cwd, home, xdg); ok && !off["pii"] {
			if observe {
				logGuardEvent(in.SessionID, wouldKind(d), d.Rule)
				emitContext(stdout, "PreToolUse", observeNote([]decision{d}))
				return 0
			}
			logGuardEvent(in.SessionID, "block", d.Rule)
			fmt.Fprintln(stderr, d.Msg)
			return d.Exit
		}
		if d, ok := settingsFileToolDecision(in.ToolName, raw.ToolInput, in.Cwd, home); ok {
			logGuardEvent(in.SessionID, "ask", d.Rule)
			emitAsk(stdout, "PreToolUse", d.Msg)
			return 0
		}
		if d, ok := secretFileToolDecision(in.ToolName, raw.ToolInput, in.Cwd, home, xdg); ok {
			if d.Ask {
				logGuardEvent(in.SessionID, "ask", d.Rule)
				emitAsk(stdout, "PreToolUse", d.Msg)
			} else {
				logGuardEvent(in.SessionID, "block", d.Rule)
				fmt.Fprintln(stderr, d.Msg)
			}
			return d.Exit
		}
		if d, ok := egressToolDecision(in.ToolName, raw.ToolInput, in.Cwd); ok && d.Ask && !off["egress"] {
			if observe && !enforcedAlways[d.Rule] {
				logGuardEvent(in.SessionID, wouldKind(d), d.Rule)
				emitContext(stdout, "PreToolUse", observeNote([]decision{d}))
				return 0
			}
			logGuardEvent(in.SessionID, "ask", d.Rule)
			emitAsk(stdout, "PreToolUse", d.Msg)
			return 0
		}
		// Allowed: add what memory knows about the file. "" leaves the hook silent.
		if t := memdToolLookup(in.ToolName, raw.ToolInput, in.Cwd, in.AgentID != "" || in.AgentID2 != ""); t != "" {
			emitContext(stdout, "PreToolUse", t)
		}
		return 0
	}
	env := &guardEnv{Now: time.Now(), Session: in.SessionID, Cwd: in.Cwd, Home: home, XDGConfig: xdg, State: newFileState(in.SessionID), GitUnpushed: gitUnpushed, Off: off, Observe: observe}
	d := decide(env, in.ToolInput.Command)
	switch {
	case d.Ask:
		logGuardEvent(in.SessionID, "ask", d.Rule)
		emitAsk(stdout, "PreToolUse", d.Msg)
		return d.Exit
	case d.Exit != 0:
		logGuardEvent(in.SessionID, "block", d.Rule)
		fmt.Fprintln(stderr, d.Msg)
		return d.Exit
	}
	// The call runs: only now do the observed decisions count.
	for _, w := range env.Would {
		logGuardEvent(in.SessionID, wouldKind(w), w.Rule)
	}
	// The mask note is part of the mask, not a nudge.
	if d.Context != "" && d.Rule != "secret-file:mask" {
		logGuardEvent(in.SessionID, "nudge", d.Rule)
	}
	ctxOut := d.Context
	if note := observeNote(env.Would); note != "" {
		if ctxOut != "" {
			ctxOut = note + "\n\n" + ctxOut
		} else {
			ctxOut = note
		}
	}
	if memdShellLookup() {
		var raw struct {
			ToolInput map[string]any `json:"tool_input"`
		}
		json.Unmarshal(data, &raw)
		if t := memdToolLookup("Bash", raw.ToolInput, in.Cwd, in.AgentID != "" || in.AgentID2 != ""); t != "" {
			if ctxOut != "" {
				ctxOut += "\n\n"
			}
			ctxOut += t
		}
	}
	cmd, masked := secretRewrite(env, in.ToolInput.Command, self)
	if masked {
		logGuardEvent(in.SessionID, "mask", "secret-mask")
	} else {
		cmd = in.ToolInput.Command
	}
	wrapped, ok := piiRewrite(cmd, self)
	if ok = ok && !off["pii"]; ok || masked {
		if ok {
			logGuardEvent(in.SessionID, "mask", "pii-wrap")
			cmd = wrapped
		}
		emitRewrite(stdout, data, cmd, ctxOut)
		return 0
	}
	// A command that retargets git (-C, --git-dir, --work-tree) stays as typed: rtk's
	// `rtk git -C ...` wrapper would hide the target from guards that look for it.
	var out, errOut []byte
	var code int
	if !gitRetargets(in.ToolInput.Command) {
		out, errOut, code = rtk(data)
	}
	stderr.Write(errOut) // exit 2: rtk's denial; exit 0: a note such as rtk missing
	if code == 2 {
		return 2
	}
	if out = bytes.TrimSpace(out); len(out) > 0 {
		stdout.Write(withContext(out, ctxOut))
		io.WriteString(stdout, "\n")
		return 0
	}
	if ctxOut != "" {
		emitContext(stdout, "PreToolUse", ctxOut)
	}
	return 0
}

// emitRewrite replaces the command and keeps the other tool_input fields (description,
// timeout, run_in_background). No permissionDecision: the user's rules judge the new command.
func emitRewrite(w io.Writer, data []byte, cmd, context string) {
	var in struct {
		ToolInput map[string]any `json:"tool_input"`
	}
	json.Unmarshal(data, &in)
	in.ToolInput["command"] = cmd
	hso := map[string]any{"hookEventName": "PreToolUse", "updatedInput": in.ToolInput}
	if context != "" {
		hso["additionalContext"] = context
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.Encode(map[string]any{"hookSpecificOutput": hso})
}

// withContext adds the guard's additionalContext to rtk's reply; rtk's reply is kept as is
// when there is none or it is not the JSON expected.
func withContext(rtkOut []byte, context string) []byte {
	if context == "" {
		return rtkOut
	}
	var r map[string]map[string]any
	if json.Unmarshal(rtkOut, &r) != nil || r["hookSpecificOutput"] == nil {
		return rtkOut
	}
	r["hookSpecificOutput"]["additionalContext"] = context
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.Encode(r)
	return bytes.TrimSpace(b.Bytes())
}
