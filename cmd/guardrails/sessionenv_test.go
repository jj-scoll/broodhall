package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Fake session ids only; never a real one.
const fakeSid = "0badc0de-0000-4000-8000-000000000001"

func TestSessionEnv(t *testing.T) {
	tests := []struct {
		name    string
		stdin   string
		prior   string // existing env-file contents
		noFile  bool   // CLAUDE_ENV_FILE unset
		want    string // env-file contents afterwards
		wantErr bool
	}{
		{"appends export with 8-char id", `{"session_id":"` + fakeSid + `","hook_event_name":"SessionStart","source":"startup"}`,
			"", false, "export AWS_SDK_UA_APP_ID=claude-0badc0de\n", false},
		{"keeps lines other hooks wrote", `{"session_id":"` + fakeSid + `"}`,
			"export NODE_ENV=production\n", false, "export NODE_ENV=production\nexport AWS_SDK_UA_APP_ID=claude-0badc0de\n", false},
		{"adds the newline a prior hook left off", `{"session_id":"` + fakeSid + `"}`,
			"export A=1", false, "export A=1\nexport AWS_SDK_UA_APP_ID=claude-0badc0de\n", false},
		{"same id twice (resume, compact) adds one line", `{"session_id":"` + fakeSid + `"}`,
			"export AWS_SDK_UA_APP_ID=claude-0badc0de\n", false, "export AWS_SDK_UA_APP_ID=claude-0badc0de\n", false},
		{"new id after /clear appends; the later line wins", `{"session_id":"11111111-0000-4000-8000-000000000002"}`,
			"export AWS_SDK_UA_APP_ID=claude-0badc0de\n", false,
			"export AWS_SDK_UA_APP_ID=claude-0badc0de\nexport AWS_SDK_UA_APP_ID=claude-11111111\n", false},
		{"no CLAUDE_ENV_FILE is a no-op", `{"session_id":"` + fakeSid + `"}`, "", true, "", false},
		{"missing session id writes nothing", `{"hook_event_name":"SessionStart"}`, "", false, "", false},
		{"unsafe session id writes nothing", `{"session_id":"x\nexport EVIL=1"}`, "", false, "", false},
		{"short session id writes nothing", `{"session_id":"abc"}`, "", false, "", false},
		{"garbage stdin writes nothing", `not json`, "", false, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "env")
			if tt.prior != "" {
				if err := os.WriteFile(path, []byte(tt.prior), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			file := path
			if tt.noFile {
				file = ""
			}
			err := writeSessionEnv(strings.NewReader(tt.stdin), file)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			got, _ := os.ReadFile(path)
			if string(got) != tt.want {
				t.Errorf("env file = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSessionEnvOptIn(t *testing.T) {
	stdin := `{"session_id":"` + fakeSid + `"}`
	for _, tt := range []struct {
		name, option, legacy string
		want                 bool
	}{
		{"unset writes nothing", "", "", false},
		{"plugin option true", "true", "", true},
		{"plugin option false beats the env var", "false", "1", false},
		{"env var outside the plugin", "", "1", true},
		{"plugin option junk is off", "yes please", "", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "env")
			t.Setenv("CLAUDE_ENV_FILE", file)
			t.Setenv("CLAUDE_PLUGIN_OPTION_AWS_SESSION_TAG", tt.option)
			t.Setenv("CLAUDE_TOOLS_AWS_SESSION_TAG", tt.legacy)
			if tt.option == "" {
				os.Unsetenv("CLAUDE_PLUGIN_OPTION_AWS_SESSION_TAG")
			}
			code, out, errOut := runHook(t, sessionEnv, stdin)
			if code != 0 || out != "" || errOut != "" {
				t.Fatalf("exit %d stdout %q stderr %q, want a quiet 0", code, out, errOut)
			}
			got, _ := os.ReadFile(file)
			if (len(got) > 0) != tt.want {
				t.Errorf("env file = %q, wrote = %v, want %v", got, len(got) > 0, tt.want)
			}
		})
	}
}

func TestSessionEnvNeverBlocks(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_OPTION_AWS_SESSION_TAG", "true")
	t.Setenv("CLAUDE_ENV_FILE", filepath.Join(t.TempDir(), "no-such-dir", "env"))
	if code, out, _ := runHook(t, sessionEnv, `{"session_id":"`+fakeSid+`"}`); code != 0 || out != "" {
		t.Errorf("unwritable env file: exit %d stdout %q, want a quiet 0", code, out)
	}
}
