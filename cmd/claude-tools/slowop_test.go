package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

var t0 = time.Unix(1_800_000_000, 0)

func envAt(st guardState, now time.Time, cwd string) *guardEnv {
	return &guardEnv{Now: now, Session: "s", Cwd: cwd, Home: "/home/user", State: st}
}

func TestSlowOpNarrowedAndStateful(t *testing.T) {
	type step struct {
		after time.Duration // time since t0
		cwd   string
		cmd   string
		want  string // Kind
	}
	tests := []struct {
		name  string
		steps []step
	}{
		{"apply with no plan nudges", []step{{0, "/infra", "terraform apply", "context:plan-before-apply"}}},
		{"destroy with no plan nudges", []step{{0, "/infra", "terraform destroy -auto-approve", "context:plan-before-apply"}}},
		{"terragrunt run-all apply nudges", []step{{0, "/infra", "terragrunt run-all apply", "context:plan-before-apply"}}},
		{"plan then apply same dir is quiet", []step{
			{0, "/infra", "terraform plan", "allow"},
			{time.Minute, "/infra", "terraform apply", "allow"}}},
		{"plan in another dir does not count", []step{
			{0, "/other", "terraform plan", "allow"},
			{time.Minute, "/infra", "terraform apply", "context:plan-before-apply"}}},
		{"plan 29 minutes ago counts", []step{
			{0, "/infra", "terragrunt plan", "allow"},
			{29 * time.Minute, "/infra", "terragrunt apply", "allow"}}},
		{"plan 31 minutes ago expired", []step{
			{0, "/infra", "terragrunt plan", "allow"},
			{31 * time.Minute, "/infra", "terragrunt apply", "context:plan-before-apply"}}},
		{"plan -destroy is a plan", []step{
			{0, "/infra", "terraform plan -destroy", "allow"},
			{time.Minute, "/infra", "terraform destroy", "allow"}}},
		{"plan and apply in one command", []step{
			{0, "/", "cd infra && terraform plan && terraform apply", "allow"}}},
		{"cd to a different dir than the plan", []step{
			{0, "/infra", "terraform plan", "allow"},
			{time.Minute, "/infra", "cd ../prod && terraform apply", "context:plan-before-apply"}}},
		{"chdir flag plans and applies the same dir", []step{
			{0, "/", "terraform -chdir=infra plan", "allow"},
			{time.Minute, "/", "terraform -chdir=infra apply", "allow"}}},
		{"chdir flag other dir", []step{
			{0, "/", "terraform -chdir=infra plan", "allow"},
			{time.Minute, "/", "terraform -chdir=prod apply", "context:plan-before-apply"}}},
		{"apply of a saved plan file is quiet", []step{{0, "/infra", "terraform apply tfplan", "allow"}}},
		{"mentions are not runs", []step{
			{0, "/infra", "echo terraform apply", "allow"},
			{0, "/infra", `git commit -m "terraform apply notes"`, "allow"}}},
		{"docker build nudges once per dir per window", []step{
			{0, "/app", "docker build -t x .", "context:slow-op"},
			{time.Minute, "/app", "docker build -t x .", "allow"},
			{32 * time.Minute, "/app", "docker build -t x .", "context:slow-op"}, // 31m after the last mark
			{time.Minute, "/other", "docker build .", "context:slow-op"}}},
		{"tests and builds are not nudged", []step{
			{0, "/", "pytest -q", "allow"},
			{0, "/", "go build ./...", "allow"},
			{0, "/", "cargo build", "allow"},
			{0, "/", "npm run build", "allow"},
			{0, "/", "npm run test", "allow"},
			{0, "/", "yarn build", "allow"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := memState{}
			for i, s := range tt.steps {
				got := decide(envAt(st, t0.Add(s.after), s.cwd), s.cmd)
				if got.Kind() != s.want {
					t.Errorf("step %d %q at +%s: got %s, want %s", i, s.cmd, s.after, got.Kind(), s.want)
				}
				if got.Exit != 0 {
					t.Errorf("step %d %q: Exit = %d, the nudge must not use exit codes", i, s.cmd, got.Exit)
				}
			}
		})
	}
}

func TestSlowOpNilStateStillNudges(t *testing.T) {
	d := decide(envAt(nil, t0, "/infra"), "terraform apply")
	if d.Kind() != "context:plan-before-apply" {
		t.Errorf("got %s", d.Kind())
	}
}

// The wire contract (docs: PreToolUse additionalContext, exit 0): stdout JSON with
// hookSpecificOutput.hookEventName "PreToolUse" and additionalContext; nothing on stderr;
// no permissionDecision, so the normal permission flow continues.
func TestPreToolUseEmitsAdditionalContext(t *testing.T) {
	t.Setenv("HANDOFF_STATE_DIR", t.TempDir())
	in := bashInput("PreToolUse", "sess-1", "/infra", "terraform apply", "")
	code, out, errOut := runHook(t, preToolUse, in)
	if code != 0 || errOut != "" {
		t.Fatalf("exit %d, stderr %q; want exit 0 and no stderr", code, errOut)
	}
	var got struct {
		HookSpecificOutput map[string]any `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, out)
	}
	h := got.HookSpecificOutput
	if h["hookEventName"] != "PreToolUse" {
		t.Errorf("hookEventName = %v", h["hookEventName"])
	}
	if s, _ := h["additionalContext"].(string); !strings.Contains(s, "plan") {
		t.Errorf("additionalContext = %q, want plan reminder", s)
	}
	if _, bad := h["permissionDecision"]; bad {
		t.Error("must not set permissionDecision")
	}
}

// State survives between hook processes: plan in one call, apply in the next is quiet.
func TestPreToolUsePlanStateAcrossProcesses(t *testing.T) {
	t.Setenv("HANDOFF_STATE_DIR", t.TempDir())
	if code, out, _ := runHook(t, preToolUse, bashInput("PreToolUse", "sess-2", "/infra", "terraform plan", "")); code != 0 || out != "" {
		t.Fatalf("plan: exit %d out %q", code, out)
	}
	if code, out, _ := runHook(t, preToolUse, bashInput("PreToolUse", "sess-2", "/infra", "terraform apply", "")); code != 0 || out != "" {
		t.Errorf("apply after plan: exit %d out %q, want silent allow", code, out)
	}
	if _, out, _ := runHook(t, preToolUse, bashInput("PreToolUse", "sess-3", "/infra", "terraform apply", "")); out == "" {
		t.Error("another session must not inherit the plan")
	}
}
