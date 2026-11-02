package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// No test reads the real ~/.config/claude-tools/pii-columns.json: the path is a package
// variable, never an environment variable, and defaults to "" (embedded rules) here.
func init() { piiRulesPath = func() string { return "" } }

// setRulesPath points the loader at p for one test.
func setRulesPath(t *testing.T, p string) {
	t.Helper()
	old := piiRulesPath
	piiRulesPath = func() string { return p }
	t.Cleanup(func() { piiRulesPath = old })
}

func writeRulesFile(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "pii-columns.json")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	setRulesPath(t, p)
	return p
}

const (
	onlyLoyalty = `{"rules":[{"name":"loyalty","category":"other_identifier","pattern":"loyalty_code","tier":1}]}`
	onlySSN     = `{"rules":[{"name":"ssn","category":"gov_id","pattern":"ssn","tier":1}]}`
)

func TestPIIRulesPathFor(t *testing.T) {
	tests := []struct {
		name, xdg, home, want string
	}{
		{"home default", "", "/home/u", "/home/u/.config/claude-tools/pii-columns.json"},
		{"xdg wins", "/x/cfg", "/home/u", "/x/cfg/claude-tools/pii-columns.json"},
		{"relative xdg ignored", "cfg", "/home/u", "/home/u/.config/claude-tools/pii-columns.json"},
		{"nothing known", "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := piiRulesPathFor(tt.xdg, tt.home); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPIIRulesNoEnvOverride(t *testing.T) {
	// The only env input is the standard XDG/HOME pair; a claude-tools specific variable
	// must not move the path.
	t.Setenv("CLAUDE_TOOLS_PII_COLUMNS", "/tmp/loose.json")
	t.Setenv("PII_COLUMNS", "/tmp/loose.json")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "/home/u")
	piiRulesPath = defaultPIIRulesPath
	t.Cleanup(func() { piiRulesPath = func() string { return "" } })
	if got := piiRulesPath(); got != "/home/u/.config/claude-tools/pii-columns.json" {
		t.Errorf("path = %q", got)
	}
}

func TestLoadPIIRulesPrecedence(t *testing.T) {
	emb, src, err := loadPIIRulesSource()
	if src != "embedded" || err != nil || len(emb.rules) == 0 {
		t.Fatalf("no file: source %q err %v rules %d", src, err, len(emb.rules))
	}
	p := writeRulesFile(t, onlyLoyalty)
	rs, src, err := loadPIIRulesSource()
	if src != p || err != nil {
		t.Fatalf("source %q err %v, want file %q", src, err, p)
	}
	if len(rs.rules) != 1 {
		t.Errorf("rules = %d, want the file's 1", len(rs.rules))
	}
}

func TestLoadPIIRulesFailsClosed(t *testing.T) {
	tests := []struct {
		name    string
		body    string // "" with missing=true: no file
		missing bool
		wantErr string // substring; "" means no error expected
	}{
		{"missing file", "", true, ""},
		{"empty file", "", false, "pii-columns.json"},
		{"bad json", `{"rules": [`, false, "pii-columns.json"},
		{"bad pattern", `{"rules":[{"name":"x","category":"email","pattern":"(unclosed","tier":1}]}`, false, "bad pattern"},
		{"bad exclude", `{"rules":[{"name":"x","category":"email","pattern":"a","exclude":"[","tier":1}]}`, false, "bad exclude"},
		{"unknown category", `{"rules":[{"name":"x","category":"nonsense","pattern":"a","tier":1}]}`, false, "unknown category"},
		{"no active rules", `{"rules":[]}`, false, "no active rules"},
		{"only inactive tier", `{"rules":[{"name":"x","category":"email","pattern":"a","tier":3}]}`, false, "no active rules"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.missing {
				setRulesPath(t, filepath.Join(t.TempDir(), "absent.json"))
			} else {
				writeRulesFile(t, tt.body)
			}
			rs, src, err := loadPIIRulesSource()
			if src != "embedded" {
				t.Errorf("source = %q, want embedded", src)
			}
			if rs == nil || rs.category("email") != "email" {
				t.Fatalf("fell back to no rules: email column category = %v", rs)
			}
			if tt.wantErr == "" && err != nil {
				t.Errorf("unexpected error %v", err)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Errorf("err = %v, want containing %q", err, tt.wantErr)
			}
			if got := loadPIIRules(); got.category("email") != "email" {
				t.Errorf("loadPIIRules lost the embedded rules")
			}
		})
	}
	t.Run("path is a directory", func(t *testing.T) {
		setRulesPath(t, t.TempDir())
		rs, src, err := loadPIIRulesSource()
		if src != "embedded" || err == nil || rs.category("email") != "email" {
			t.Errorf("source %q err %v", src, err)
		}
	})
}

// A file that adds a rule masks a column the embedded rules leave alone; a file that drops a
// rule unmasks one: both through newRedactor and runRedact, the real masking path.
func TestPIIRulesFileIsLive(t *testing.T) {
	const csv = "id,loyalty_code,email\n1,LC-9041,ann@example.com\n"
	run := func() string {
		var out, errb bytes.Buffer
		x := newRedactor([]byte("k"), nil)
		if code := runRedact(x, []string{"--pii", "--in=csv"}, strings.NewReader(csv), &out, &errb); code != 0 {
			t.Fatalf("exit %d: %s", code, errb.String())
		}
		return out.String()
	}
	if strings.Contains(run(), "[other_identifier:") || !strings.Contains(run(), "LC-9041") {
		t.Fatalf("baseline: loyalty_code should be readable with the embedded rules:\n%s", run())
	}
	writeRulesFile(t, onlyLoyalty)
	if got := run(); strings.Contains(got, "LC-9041") || !strings.Contains(got, "[other_identifier:") {
		t.Errorf("added rule did not mask loyalty_code:\n%s", got)
	}
	// With only the loyalty rule, the email column header no longer matches: the cell is
	// left to the shape masker (email shapes are always masked), but the column rule is gone.
	if rs := loadPIIRules(); rs.category("email") != "" {
		t.Errorf("removed rule still masks the email column: %q", rs.category("email"))
	}
	writeRulesFile(t, onlySSN)
	if got := run(); !strings.Contains(got, "LC-9041") {
		t.Errorf("file without a loyalty rule should leave it readable:\n%s", got)
	}
}

func TestMirrorUsesRulesFile(t *testing.T) {
	// pii-mirror builds its redactor with newRedactor, so it sees the file too.
	writeRulesFile(t, onlyLoyalty)
	x := newRedactor([]byte("k"), nil)
	if x.rules.category("loyalty_code") == "" || x.rules.category("email") != "" {
		t.Errorf("newRedactor ignored the rules file")
	}
}

func TestPIIRulesInit(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "claude-tools", "pii-columns.json")
	setRulesPath(t, p)
	var out, errb bytes.Buffer
	if code := piiRulesRun(&out, &errb, []string{"init"}); code != 0 {
		t.Fatalf("init exit %d: %s", code, errb.String())
	}
	got, err := os.ReadFile(p)
	if err != nil || !bytes.Equal(got, piiColumnsJSON) {
		t.Fatalf("init did not write the embedded rules: %v", err)
	}
	os.WriteFile(p, []byte(onlySSN), 0o644)
	out.Reset()
	errb.Reset()
	if code := piiRulesRun(&out, &errb, []string{"init"}); code == 0 {
		t.Errorf("second init must refuse to overwrite")
	}
	if got, _ := os.ReadFile(p); string(got) != onlySSN {
		t.Errorf("init overwrote an existing file")
	}
}

func TestPIIRulesCheck(t *testing.T) {
	good := writeRulesFile(t, onlyLoyalty)
	var out, errb bytes.Buffer
	if code := piiRulesRun(&out, &errb, []string{"check"}); code != 0 || !strings.Contains(out.String(), "ok") || !strings.Contains(out.String(), "1 rules") {
		t.Errorf("default path: exit %d out %q err %q", code, out.String(), errb.String())
	}
	bad := filepath.Join(t.TempDir(), "bad.json")
	os.WriteFile(bad, []byte(`{"rules":[{"name":"x","category":"nonsense","pattern":"a","tier":1}]}`), 0o644)
	out.Reset()
	errb.Reset()
	if code := piiRulesRun(&out, &errb, []string{"check", bad}); code != 1 || !strings.Contains(out.String()+errb.String(), "unknown category") {
		t.Errorf("explicit bad path: exit %d out %q err %q", code, out.String(), errb.String())
	}
	if code := piiRulesRun(&out, &errb, []string{"check", good}); code != 0 {
		t.Errorf("explicit good path: exit %d", code)
	}
	if code := piiRulesRun(&out, &errb, []string{"check", filepath.Join(t.TempDir(), "nope.json")}); code != 1 {
		t.Errorf("missing path: exit %d, want 1", code)
	}
	if code := piiRulesRun(&out, &errb, []string{"bogus"}); code != 2 {
		t.Errorf("unknown verb: exit %d, want 2", code)
	}
}

// --- PreToolUse guard ---

func guardCmd(cmd string) decision {
	return decide(&guardEnv{Now: time.Unix(1_800_000_000, 0), Session: "s", Cwd: "/work", Home: "/home/user", State: memState{}}, cmd)
}

func TestGuardDeniesRulesFileWrites(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	const f = "~/.config/claude-tools/pii-columns.json"
	denied := []string{
		"sed -i 's/email/zzz/' " + f,
		"sed -i.bak 's/a/b/' /home/user/.config/claude-tools/pii-columns.json",
		"echo '{}' > " + f,
		"echo '{}' >> $HOME/.config/claude-tools/pii-columns.json",
		`cat x.json >| "${HOME}/.config/claude-tools/pii-columns.json"`,
		"cp /tmp/loose.json " + f,
		"mv /tmp/loose.json " + f,
		"rm " + f,
		"rm -rf ~/.config/claude-tools",
		"chmod 666 " + f,
		"echo hi | tee " + f,
		"tee -a /home/user/.config/claude-tools/pii-columns.json < /tmp/x",
		"install -m 644 /tmp/x " + f,
		"truncate -s 0 " + f,
		`python3 -c "open('/home/user/.config/claude-tools/pii-columns.json','w').write('{}')"`,
		`perl -pi -e 's/email/x/' ` + f,
		"cd ~/.config/claude-tools && sed -i 's/a/b/' pii-columns.json",
		"cd ~/.config/claude-tools; echo '{}' > pii-columns.json",
		"ls && rm " + f,
		"XDG_CONFIG_HOME=/tmp/loose claude-tools redact --pii",
		"export XDG_CONFIG_HOME=/tmp/loose; claude-tools redact",
		"HOME=/tmp/fake claude-tools pii-mirror a b",
		"HOME=/tmp/x claude-tools redact",
		"XDG_CONFIG_HOME=/tmp/x ~/.claude/bin/claude-tools pii-mirror a b",
		"env HOME=/x claude-tools redact",
		"cd /tmp && HOME=/x /home/user/.claude/bin/claude-tools redact",
		"echo hi | HOME=/x claude-tools redact",
		`bash -c 'HOME=/x claude-tools redact'`,
	}
	for _, cmd := range denied {
		t.Run(cmd, func(t *testing.T) {
			d := guardCmd(cmd)
			if d.Exit != 2 || d.Msg == "" || !strings.Contains(d.Msg, "the user's to edit") {
				t.Errorf("got exit %d msg %q, want block", d.Exit, d.Msg)
			}
		})
	}
	t.Run("exact message", func(t *testing.T) {
		if d := guardCmd("rm " + f); !strings.Contains(d.Msg, "PII rules (~/.config/claude-tools/pii-columns.json) are the user's to edit, not the model's.") {
			t.Errorf("msg = %q", d.Msg)
		}
	})
}

func TestGuardAllowsRulesFileReads(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	const f = "~/.config/claude-tools/pii-columns.json"
	allowed := []string{
		"cat " + f,
		"rg -n email /home/user/.config/claude-tools/pii-columns.json",
		"jq '.rules | length' " + f,
		"ls -la ~/.config/claude-tools",
		"claude-tools pii-rules init",
		"claude-tools pii-rules check " + f,
		"cat " + f + " 2>/dev/null",
		"cat " + f + " > /tmp/copy.json",
		"diff " + f + " cmd/claude-tools/pii-columns.json",
		"sed -i 's/a/b/' cmd/claude-tools/pii-columns.json",
		"cp " + f + " /tmp/backup.json",
		"HOME=$(mktemp -d) git status",
		"XDG_CONFIG_HOME=/tmp/x nvim --version",
		"export XDG_CONFIG_HOME=/tmp/loose; psql -c 'select 1'",
		"HOME=/tmp/x git -C ~/src/claude-tools status",
	}
	for _, cmd := range allowed {
		t.Run(cmd, func(t *testing.T) {
			if d := guardCmd(cmd); d.Exit != 0 || d.Ask {
				t.Errorf("got exit %d ask %v msg %q, want allow", d.Exit, d.Ask, d.Msg)
			}
		})
	}
}

func runToolHook(t *testing.T, home, tool string, input map[string]any) (int, string) {
	t.Helper()
	in, _ := json.Marshal(map[string]any{"session_id": "s", "cwd": home, "tool_name": tool, "tool_input": input})
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	var stdout, stderr bytes.Buffer
	code := preToolUseRun(in, "CT", func([]byte) ([]byte, []byte, int) { return nil, nil, 0 }, &stdout, &stderr)
	return code, stderr.String()
}

func TestGuardDeniesRulesFileEditTools(t *testing.T) {
	home := t.TempDir()
	cfg := filepath.Join(home, ".config", "claude-tools")
	os.MkdirAll(cfg, 0o755)
	file := filepath.Join(cfg, "pii-columns.json")
	os.WriteFile(file, []byte(onlySSN), 0o644)
	link := filepath.Join(home, "innocent.json")
	os.Symlink(file, link)

	for _, tool := range []string{"Edit", "Write", "MultiEdit"} {
		for name, p := range map[string]string{"direct": file, "tilde": "~/.config/claude-tools/pii-columns.json", "dotdot": cfg + "/../claude-tools/pii-columns.json", "symlink": link} {
			t.Run(tool+"/"+name, func(t *testing.T) {
				code, msg := runToolHook(t, home, tool, map[string]any{"file_path": p, "content": "{}"})
				if code != 2 || !strings.Contains(msg, "the user's to edit") {
					t.Errorf("exit %d msg %q, want block", code, msg)
				}
			})
		}
	}
	t.Run("NotebookEdit", func(t *testing.T) {
		if code, _ := runToolHook(t, home, "NotebookEdit", map[string]any{"notebook_path": file}); code != 2 {
			t.Errorf("exit %d, want block", code)
		}
	})
	t.Run("not yet created", func(t *testing.T) {
		os.Remove(file)
		os.Remove(link)
		if code, _ := runToolHook(t, home, "Write", map[string]any{"file_path": file, "content": "{}"}); code != 2 {
			t.Errorf("exit %d, want block", code)
		}
	})
	t.Run("other files and Read pass", func(t *testing.T) {
		other := filepath.Join(home, "notes.md")
		if code, _ := runToolHook(t, home, "Write", map[string]any{"file_path": other}); code != 0 {
			t.Errorf("Write elsewhere: exit %d", code)
		}
		if code, _ := runToolHook(t, home, "Read", map[string]any{"file_path": file}); code != 0 {
			t.Errorf("Read of the rules file: exit %d", code)
		}
		repoCopy := filepath.Join(home, "src", "claude-tools", "cmd", "claude-tools", "pii-columns.json")
		if code, _ := runToolHook(t, home, "Edit", map[string]any{"file_path": repoCopy}); code != 0 {
			t.Errorf("Edit of the repo copy: exit %d", code)
		}
	})
}

func TestEmbeddedRulesParse(t *testing.T) {
	rs, total, err := validatePIIRules(piiColumnsJSON)
	if err != nil || len(rs.rules) == 0 || total < len(rs.rules) {
		t.Fatalf("embedded rules do not pass the user-file checks: %v", err)
	}
}
