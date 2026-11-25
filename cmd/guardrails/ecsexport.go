package main

// ECS export: when the ecs_export option is on (GUARDRAILS_ECS_EXPORT), each guard ledger event
// is also appended as one Elastic Common Schema JSON object per line to a file shared by every
// session, $XDG_STATE_HOME/guardrails/events.ecs.jsonl unless ecs_export_path
// (GUARDRAILS_ECS_EXPORT_PATH) names another, so a log shipper such as the Wazuh agent can read
// it. Off by default. The field set is a contract for detection rules written over it; change it
// only with them.
//
// An event carries what the ledger line carries (time, kind, rule id) plus the session id, the
// host, the user and this binary's version: never command text, paths or values. Like the
// ledger, a failed write is ignored and costs one open; it must never change or slow a guard's
// decision. Outcome lines (ran, not-run) are not decisions and are not exported.

import (
	"encoding/json"
	"os"
	"os/user"
	"path/filepath"
	"runtime/debug"
	"time"
)

type ecsDecision struct {
	Timestamp string `json:"@timestamp"`
	Event     struct {
		Kind     string   `json:"kind"`
		Category []string `json:"category"`
		Provider string   `json:"provider"`
		Dataset  string   `json:"dataset"`
		Action   string   `json:"action"`
	} `json:"event"`
	Rule struct {
		Name    string `json:"name"`
		Ruleset string `json:"ruleset"`
	} `json:"rule"`
	Observer struct {
		Product string `json:"product"`
		Vendor  string `json:"vendor"`
		Version string `json:"version"`
	} `json:"observer"`
	Host   *ecsName `json:"host,omitempty"`
	User   *ecsName `json:"user,omitempty"`
	Labels struct {
		SessionID string `json:"session_id"`
	} `json:"labels"`
}

type ecsName struct {
	Name string `json:"name"`
}

// ecsExportPath is the export file, or "" when the export is off or its path is unusable. A
// path is absolute or starts with ~/; a relative one would land in each session's cwd.
func ecsExportPath() string {
	if !observeOn(settingEnv("ECS_EXPORT")) { // the same true/1/yes words as observe
		return ""
	}
	p := settingEnv("ECS_EXPORT_PATH")
	if p == "" {
		return userPath(stateBase(), "events.ecs.jsonl")
	}
	if p = expandHome(p, os.Getenv("HOME")); !filepath.IsAbs(p) {
		return ""
	}
	return p
}

// exportECSEvent appends one event; rule is already cut to its id.
func exportECSEvent(session, kind, rule string) {
	if kind == "ran" || kind == "not-run" {
		return
	}
	path := ecsExportPath()
	if path == "" {
		return
	}
	var e ecsDecision
	e.Timestamp = time.Now().UTC().Format(time.RFC3339)
	e.Event.Kind = "event"
	e.Event.Category = []string{"intrusion_detection"}
	e.Event.Provider = "guardrails"
	e.Event.Dataset = "guardrails.decisions"
	e.Event.Action = kind
	e.Rule.Name = rule
	e.Rule.Ruleset = "guardrails"
	e.Observer.Product = "guardrails"
	e.Observer.Vendor = "broodhall"
	bi, ok := debug.ReadBuildInfo()
	if e.Observer.Version = buildVersion(bi, ok); e.Observer.Version == "" {
		rev, dirty := buildRev(bi, ok)
		e.Observer.Version = versionString("", rev, dirty)
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		e.Host = &ecsName{h}
	}
	if u, err := user.Current(); err == nil && u.Username != "" {
		e.User = &ecsName{u.Username}
	}
	e.Labels.SessionID = session
	line, err := json.Marshal(e)
	if err != nil {
		return
	}
	if os.MkdirAll(filepath.Dir(path), 0o700) != nil {
		return
	}
	// One O_APPEND write per event, as for the ledger: concurrent hooks don't interleave lines.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	f.Write(append(line, '\n'))
	f.Close()
}
