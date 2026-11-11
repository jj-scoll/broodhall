package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	capHome       = "/home/u"
	capCredPlace  = "[output not kept: credential file]"
	capCmdPlace   = "[output not kept: credential command]"
	capOutputBody = "line one\nline two"
	capProjectCwd = "/work/app"
)

func capCfg() captureSettings { return captureSettings{Tools: true, Home: capHome} }

func TestCaptureCredentialFileReads(t *testing.T) {
	tests := []struct {
		name, tool, input string
		dropped           bool
	}{
		{"Read .env", "Read", `{"file_path":"/work/app/.env"}`, true},
		{"Read .env.local", "Read", `{"file_path":"/work/app/.env.local"}`, true},
		{"Read relative .env", "Read", `{"file_path":".env"}`, true},
		{"Read tilde ssh key", "Read", `{"file_path":"~/.ssh/id_ed25519"}`, true},
		{"Read aws credentials", "Read", `{"file_path":"/home/u/.aws/credentials"}`, true},
		{"Read template", "Read", `{"file_path":"/work/app/.env.example"}`, false},
		{"Read public key", "Read", `{"file_path":"/home/u/.ssh/id_ed25519.pub"}`, false},
		{"Read source", "Read", `{"file_path":"/work/app/main.go"}`, false},
		{"Grep on .env", "Grep", `{"pattern":"KEY","path":"/work/app/.env"}`, true},
		{"Grep with glob", "Grep", `{"pattern":"KEY","glob":".env*"}`, true},
		{"Grep source", "Grep", `{"pattern":"KEY","path":"/work/app"}`, false},
		{"cat .env", "Bash", `{"command":"cat .env"}`, true},
		{"cat tilde key", "Bash", `{"command":"cat ~/.ssh/id_rsa"}`, true},
		{"cat HOME var", "Bash", `{"command":"cat $HOME/.aws/credentials"}`, true},
		{"cat braces HOME", "Bash", `{"command":"cat ${HOME}/.aws/credentials"}`, true},
		{"quoted", "Bash", `{"command":"cat \".env\""}`, true},
		{"piped", "Bash", `{"command":"cat .env | grep KEY"}`, true},
		{"chained", "Bash", `{"command":"cd /x && cat .env.production"}`, true},
		{"bash -c", "Bash", `{"command":"bash -c 'cat .env'"}`, true},
		{"via variable assignment", "Bash", `{"command":"F=.env; cat $F"}`, true},
		{"flag value", "Bash", `{"command":"docker compose --env-file=.env config"}`, true},
		{"redirect", "Bash", `{"command":"wc -l < .env"}`, true},
		{"glob in dir", "Bash", `{"command":"cat .env*"}`, true},
		{"glob on creds", "Bash", `{"command":"cat ~/.aws/cred*"}`, true},
		{"PowerShell", "PowerShell", `{"command":"Get-Content .env"}`, true},
		{"template via cat", "Bash", `{"command":"cat .env.example"}`, false},
		{"public key", "Bash", `{"command":"cat ~/.ssh/id_ed25519.pub"}`, false},
		{"plain ls", "Bash", `{"command":"ls -la"}`, false},
		{"go test", "Bash", `{"command":"go test ./..."}`, false},
		{"environment word", "Bash", `{"command":"cat environment.md"}`, false},
		{"printenv name", "Bash", `{"command":"echo $ENVIRONMENT dotenv"}`, false},
		{"other tool", "WebFetch", `{"url":"https://x.test/.env"}`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in, out, queue := captureToolCall(capCfg(), capProjectCwd, tt.tool, tt.input, capOutputBody, false)
			if tt.dropped {
				if out != capCredPlace {
					t.Errorf("output = %q, want %q", out, capCredPlace)
				}
			} else if out != capOutputBody || in != tt.input {
				t.Errorf("input/output changed: in=%q out=%q", in, out)
			}
			if !queue {
				t.Errorf("queue = false, want true (output-dropped and full calls are observed)")
			}
		})
	}
}

func TestCaptureCredentialFileWrites(t *testing.T) {
	for _, tc := range []struct{ tool, input string }{
		{"Write", `{"file_path":"/work/app/.env","content":"API_KEY=hunter2hunter2"}`},
		{"Edit", `{"file_path":"/work/app/.env","old_string":"A=1","new_string":"A=2zzzzzzzz"}`},
		{"MultiEdit", `{"file_path":"/home/u/.aws/credentials","edits":[{"old_string":"a","new_string":"b"}]}`},
		{"NotebookEdit", `{"notebook_path":"/work/app/secrets.pem","new_source":"x"}`},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			in, out, queue := captureToolCall(capCfg(), capProjectCwd, tc.tool, tc.input, "updated", false)
			if strings.Contains(in, "hunter2") || strings.Contains(in, "new_string") || strings.Contains(in, "old_string") ||
				strings.Contains(in, "content") || strings.Contains(in, "edits") || strings.Contains(in, "new_source") {
				t.Errorf("input kept what was written: %s", in)
			}
			if !strings.Contains(in, `"file_path"`) && !strings.Contains(in, `"notebook_path"`) {
				t.Errorf("input lost the path: %s", in)
			}
			if out != capCredPlace || !queue {
				t.Errorf("out=%q queue=%v", out, queue)
			}
		})
	}
	// A write to an ordinary file is kept whole.
	input := `{"file_path":"/work/app/main.go","content":"package main"}`
	if in, out, _ := captureToolCall(capCfg(), capProjectCwd, "Write", input, "ok", false); in != input || out != "ok" {
		t.Errorf("ordinary write changed: %q %q", in, out)
	}
}

func TestCaptureCredentialCommands(t *testing.T) {
	drop := []string{
		"gh auth token",
		"gh auth token --hostname github.com",
		"kubectl get secret db -o yaml",
		"kubectl -n prod get secrets",
		"kubectl get secret db -o jsonpath='{.data.password}' | base64 -d",
		"aws configure get aws_secret_access_key",
		"aws configure get default.aws_secret_access_key --profile x",
		"aws configure get aws_session_token",
		"aws sts get-session-token --serial-number x",
		"aws sts assume-role --role-arn x --role-session-name y",
		"aws secretsmanager get-secret-value --secret-id x",
		"aws ecr get-login-password",
		"gcloud auth print-access-token",
		"gcloud auth application-default print-access-token",
		"gcloud auth print-identity-token",
		"az account get-access-token",
		"op read op://vault/item/field",
		"vault read secret/data/x",
		"vault kv get secret/x",
		"security find-generic-password -s x -w",
		"security find-generic-password -ga x -w",
		"TOKEN=$(gh auth token) curl x",
		"sudo kubectl get secret x",
		"cd /x && vault kv get -format=json secret/y",
	}
	keep := []string{
		"gh auth status",
		"gh pr list",
		"kubectl get pods",
		"kubectl get secretproviderclass",
		"aws configure get region",
		"aws sts get-caller-identity",
		"aws s3 ls",
		"gcloud auth list",
		"op item list",
		"vault status",
		"vault kv list secret/",
		"security find-generic-password -s x",
		"git log --oneline",
	}
	run := func(cmd string) (string, bool) {
		_, out, queue := captureToolCall(capCfg(), capProjectCwd, "Bash", `{"command":`+jsonStr(cmd)+`}`, capOutputBody, false)
		if !queue {
			t.Errorf("%q: queue = false", cmd)
		}
		return out, queue
	}
	for _, cmd := range drop {
		if out, _ := run(cmd); out != capCmdPlace {
			t.Errorf("%q: output = %q, want dropped", cmd, out)
		}
	}
	for _, cmd := range keep {
		if out, _ := run(cmd); out != capOutputBody {
			t.Errorf("%q: output dropped, want kept", cmd)
		}
	}
}

func TestCaptureExcludedDirs(t *testing.T) {
	cfg := capCfg()
	cfg.ExcludeDirs = []string{"/work/private", "/home/u/clients"}
	for _, tc := range []struct {
		cwd  string
		name bool
	}{
		{"/work/private", true},
		{"/work/private/sub/dir", true},
		{"/home/u/clients/acme", true},
		{"/work/privateer", false},
		{"/work/app", false},
	} {
		in, out, queue := captureToolCall(cfg, tc.cwd, "Bash", `{"command":"ls"}`, capOutputBody, false)
		if tc.name {
			if in != "" || out != "" || queue {
				t.Errorf("cwd %s: want name only, got in=%q out=%q queue=%v", tc.cwd, in, out, queue)
			}
		} else if in == "" || out != capOutputBody || !queue {
			t.Errorf("cwd %s: want kept, got in=%q out=%q queue=%v", tc.cwd, in, out, queue)
		}
	}
	// An errored name-only call says so.
	if _, out, _ := captureToolCall(cfg, "/work/private", "Bash", `{"command":"false"}`, "boom", true); out != captureErrorMark {
		t.Errorf("errored output = %q, want %q", out, captureErrorMark)
	}
}

func TestCaptureToolsOff(t *testing.T) {
	cfg := capCfg()
	cfg.Tools = false
	in, out, queue := captureToolCall(cfg, capProjectCwd, "Read", `{"file_path":"/work/app/main.go"}`, capOutputBody, false)
	if in != "" || out != "" || queue {
		t.Errorf("got in=%q out=%q queue=%v, want name only and not queued", in, out, queue)
	}
	if _, out, _ := captureToolCall(cfg, capProjectCwd, "Bash", `{"command":"false"}`, "boom", true); out != captureErrorMark {
		t.Errorf("errored output = %q, want %q", out, captureErrorMark)
	}
}

func TestCaptureRedactsKeptText(t *testing.T) {
	in, out, _ := captureToolCall(capCfg(), capProjectCwd, "Bash", `{"command":"echo `+memdFakeGH+`"}`, "got "+memdFakeGH, false)
	if strings.Contains(in, memdFakeGH) || strings.Contains(out, memdFakeGH) {
		t.Errorf("secret kept: in=%q out=%q", in, out)
	}
}

func TestLoadCaptureSettings(t *testing.T) {
	dir := t.TempDir()
	write := func(body string) string {
		p := filepath.Join(dir, "s.json")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	t.Setenv("MEMORY_CAPTURE_TOOLS", "")
	t.Setenv("MEMORY_CAPTURE_EXCLUDE_DIRS", "")
	t.Setenv("HOME", capHome)

	if c := loadCaptureSettings(filepath.Join(dir, "missing.json")); !c.Tools || len(c.ExcludeDirs) != 0 {
		t.Errorf("missing file = %+v, want defaults", c)
	}
	if c := loadCaptureSettings(write(`not json`)); !c.Tools {
		t.Errorf("bad json = %+v, want defaults", c)
	}
	c := loadCaptureSettings(write(`{"capture_tools": false, "capture_exclude_dirs": "~/clients, /work/private/"}`))
	if c.Tools || len(c.ExcludeDirs) != 2 || c.ExcludeDirs[0] != "/home/u/clients" || c.ExcludeDirs[1] != "/work/private" {
		t.Errorf("settings = %+v", c)
	}
	c = loadCaptureSettings(write(`{"capture_exclude_dirs": ["/a", "~/b"]}`))
	if !c.Tools || len(c.ExcludeDirs) != 2 || c.ExcludeDirs[1] != "/home/u/b" {
		t.Errorf("array form = %+v", c)
	}
	c = loadCaptureSettings(write(`{"capture_tools": "false"}`))
	if c.Tools {
		t.Errorf("string false = %+v, want tools off", c)
	}
	// Relative directories name no one place and are ignored.
	c = loadCaptureSettings(write(`{"capture_exclude_dirs": "rel/dir,/abs"}`))
	if len(c.ExcludeDirs) != 1 || c.ExcludeDirs[0] != "/abs" {
		t.Errorf("relative dir kept: %+v", c)
	}
	// The environment wins over the file.
	t.Setenv("MEMORY_CAPTURE_TOOLS", "1")
	t.Setenv("MEMORY_CAPTURE_EXCLUDE_DIRS", "/env/one:/env/two")
	c = loadCaptureSettings(write(`{"capture_tools": false, "capture_exclude_dirs": "/file"}`))
	if !c.Tools || len(c.ExcludeDirs) != 2 || c.ExcludeDirs[0] != "/env/one" {
		t.Errorf("env did not win: %+v", c)
	}
	t.Setenv("MEMORY_CAPTURE_TOOLS", "false")
	if c = loadCaptureSettings(write(`{}`)); c.Tools {
		t.Errorf("MEMORY_CAPTURE_TOOLS=false ignored: %+v", c)
	}
}

// The parser applies the rules to both the stored event and the queue decision.
func TestCaptureInParser(t *testing.T) {
	old := captureCfg
	defer func() { captureCfg = old }()
	captureCfg = captureSettings{Tools: true, Home: capHome, ExcludeDirs: []string{"/work/private"}}

	line := func(s string) []byte { return []byte(s) }
	use := func(id, cwd, name, input string) []byte {
		return line(`{"type":"assistant","sessionId":"s1","cwd":"` + cwd + `","timestamp":"2026-11-01T10:00:00Z","message":{"content":[{"type":"tool_use","id":"` + id + `","name":"` + name + `","input":` + input + `}]}}`)
	}
	res := func(id, text string, isErr bool) []byte {
		e := "false"
		if isErr {
			e = "true"
		}
		return line(`{"type":"user","sessionId":"s1","timestamp":"2026-11-01T10:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"` + id + `","is_error":` + e + `,"content":` + jsonStr(text) + `}]}}`)
	}
	lines := [][]byte{
		use("a", "/work/app", "Read", `{"file_path":"/work/app/.env"}`),
		res("a", "DB_PASSWORD=plainplain", false),
		use("b", "/work/private", "Bash", `{"command":"ls"}`),
		res("b", "x", true),
		use("c", "/work/app", "Bash", `{"command":"ls"}`),
		res("c", "main.go", false),
	}
	evs := parseMemdLines("/home/u/.claude/projects/-work-app/s1.jsonl", "acct", lines, nil)
	if len(evs) != 3 {
		t.Fatalf("events = %d, want 3", len(evs))
	}
	if evs[0].Output != capCredPlace || !evs[0].cap.queued() || strings.Contains(evs[0].Output, "plainplain") {
		t.Errorf("credential read: %+v", evs[0])
	}
	if evs[1].Input != "" || evs[1].Output != captureErrorMark || evs[1].cap.queued() || evs[1].ToolName != "Bash" {
		t.Errorf("excluded dir: %+v", evs[1])
	}
	if evs[2].Output != "main.go" || !evs[2].cap.queued() {
		t.Errorf("ordinary call: %+v", evs[2])
	}
}

func jsonStr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
