package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// secretEnv is a guard env with a real temporary HOME holding a mark.toml, a user list
// adding ~/secrets/*.env, and the cwd in ~/work.
func secretEnv(t *testing.T) *guardEnv {
	t.Helper()
	home := t.TempDir()
	for _, p := range []string{".config/mark.toml", "secrets/prod.env", "work/notes.md", ".config/other.toml"} {
		os.MkdirAll(filepath.Join(home, filepath.Dir(p)), 0o700)
		os.WriteFile(filepath.Join(home, p), []byte("x\n"), 0o600)
	}
	os.MkdirAll(filepath.Join(home, ".config/claude-tools"), 0o700)
	os.WriteFile(filepath.Join(home, ".config/claude-tools/secret-paths"), []byte("# mine\n~/secrets/*.env\n\n"), 0o600)
	return &guardEnv{Now: time.Unix(1_800_000_000, 0), Session: "s", Cwd: filepath.Join(home, "work"), Home: home, State: memState{}}
}

func TestSecretFileRuleKinds(t *testing.T) {
	env := secretEnv(t)
	h := env.Home
	for _, c := range []struct{ cmd, want string }{
		// Plain readers are masked.
		{"cat ~/.config/mark.toml", "context:secret-file:mask"},
		{"cat -n $HOME/.config/mark.toml | head -5", "context:secret-file:mask"},
		{"head -n 20 " + h + "/.config/mark.toml", "context:secret-file:mask"},
		{"tail -3 ~/.config/mark.toml", "context:secret-file:mask"},
		{"grep -n password ~/.config/mark.toml", "context:secret-file:mask"},
		{"grep token ~/.config/*.toml", "context:secret-file:mask"},
		{"rg token ~/.config", "context:secret-file:mask"},
		{"grep -rn token ~/.config", "context:secret-file:mask"},
		{"sed -n 1,5p ~/.config/mark.toml", "context:secret-file:mask"},
		{"sed -n '/^\\[x\\]/,/^\\[/p' ~/.config/mark.toml", "context:secret-file:mask"},
		{"jq . ~/.claude/.credentials.json", "context:secret-file:mask"},
		{"cat < ~/.config/mark.toml", "context:secret-file:mask"},
		{"cd ~/.config && cat mark.toml", "context:secret-file:mask"},
		{"cat ~/secrets/prod.env", "context:secret-file:mask"},
		{"cat ~/.config/claude-tools/secret-paths", "context:secret-file:mask"},
		// Readers whose output can drop the key, and everything else, ask.
		{"grep -o 'password.*' ~/.config/mark.toml", "ask:secret-file:grep"},
		{"rg --json x ~/.config/mark.toml", "ask:secret-file:rg"},
		{"rg -r '$1' 'password = (.*)' ~/.config/mark.toml", "ask:secret-file:rg"},
		{"grep --color=always x ~/.config/mark.toml", "ask:secret-file:grep"},
		{"tail -c 20 ~/.config/mark.toml", "ask:secret-file:tail"},
		{"tail -f ~/.config/mark.toml", "ask:secret-file:tail"},
		{"cat -A ~/.config/mark.toml", "ask:secret-file:cat"},
		{"sed -n 's/password = //p' ~/.config/mark.toml", "ask:secret-file:sed"},
		{"sed -i 1d ~/.config/mark.toml", "ask:secret-file:sed"},
		{"jq -r .password ~/.config/mark.toml", "ask:secret-file:jq"},
		{"base64 ~/.config/mark.toml", "ask:secret-file:base64"},
		{"xxd ~/.config/mark.toml", "ask:secret-file:xxd"},
		{"base64 < ~/.config/mark.toml", "ask:secret-file:base64"},
		{"cp ~/.config/mark.toml /tmp/x", "ask:secret-file:cp"},
		{"mark --config ~/.config/mark.toml", "ask:secret-file:mark"},
		{"echo x >> ~/.config/claude-tools/secret-paths", "ask:secret-file:echo"},
		{"while read l; do echo $l; done < ~/.config/mark.toml", "ask:secret-file:redirect"},
		{"find ~/.config -name '*.toml' -exec cat {} +", "ask:secret-file:find"},
		{`python3 -c "print(open('` + h + `/.config/mark.toml').read())"`, "ask:secret-file:text"},
		{"bash -c 'cat ~/.config/mark.toml'", "ask:secret-file:text"},
		{"X=~/.config/mark.toml; cat $X", "ask:secret-file:text"},
		{"cat ~/.config/mark.toml | (", "ask:secret-file:text"}, // unparseable
		// Metadata, other files and the consuming tool itself pass.
		{"ls -la ~/.config/mark.toml", "allow"},
		{"stat ~/.config/mark.toml && test -f ~/.config/mark.toml", "allow"},
		{"wc -l ~/.config/mark.toml", "allow"},
		{"ls ~/.config && cd ~/.config", "allow"},
		{"mark publish notes.md", "allow"},
		{"cat ~/.config/other.toml", "allow"},
		{"cat mark.toml", "allow"}, // cwd is ~/work
		{"rg foo ~/src", "allow"},
		{"cat ~/.ssh/id_ed25519.pub", "allow"},
		{"find ~/.config -name '*.toml'", "allow"},
		// grep/rg that print names or counts, never lines.
		{"rg -l token ~/.config", "allow"},
		{"rg -o -l token ~/.config/mark.toml", "allow"},
		{"grep -rc token ~/.config", "allow"},
		{"rg -tc token ~/.config", "context:secret-file:mask"},       // -t takes c as its value
		{"grep -er ~/.config/mark.toml", "context:secret-file:mask"}, // -e takes r: not recursive, the file is read
		// A value captured into a variable is used, not shown: neither masked nor asked.
		{`T=$(sed -n 's/^password="\(.*\)"$/\1/p' ~/.config/mark.toml); curl -u "me:$T" https://x`, "allow"},
		{`export T="$(cat ~/.config/mark.toml)"`, "allow"},
		{`T=$(cat ~/.config/mark.toml > /tmp/copy)`, "context:secret-file:mask"}, // the copy is masked
		{`echo "$(cat ~/.config/mark.toml)"`, "context:secret-file:mask"},
	} {
		if got := decide(env, c.cmd).Kind(); got != c.want {
			t.Errorf("%q: got %s, want %s", c.cmd, got, c.want)
		}
	}
}

// secret-paths is read and protected under both config dir names: the old one keeps working
// after the rename, and the new one is where a fresh list goes.
func TestSecretPathsUnderBothDirNames(t *testing.T) {
	env := secretEnv(t) // its list is at ~/.config/claude-tools/secret-paths
	h := env.Home
	os.MkdirAll(filepath.Join(h, ".config/guardrails"), 0o700)
	os.WriteFile(filepath.Join(h, ".config/guardrails/secret-paths"), []byte("~/vault/*.key\n"), 0o600)
	os.MkdirAll(filepath.Join(h, "vault"), 0o700)
	os.WriteFile(filepath.Join(h, "vault/a.key"), []byte("x\n"), 0o600)
	for _, c := range []struct{ cmd, want string }{
		{"cat ~/secrets/prod.env", "context:secret-file:mask"}, // old list
		{"cat ~/vault/a.key", "context:secret-file:mask"},      // new list
		{"cat ~/.config/guardrails/secret-paths", "context:secret-file:mask"},
		{"echo x >> ~/.config/guardrails/secret-paths", "ask:secret-file:echo"},
		{"cat ~/.local/state/guardrails/pii-key", "context:secret-file:mask"},
		{"cat ~/.local/state/claude-tools/pii-key", "context:secret-file:mask"},
	} {
		if got := decide(env, c.cmd).Kind(); got != c.want {
			t.Errorf("%q: got %s, want %s", c.cmd, got, c.want)
		}
	}
}

// The credentials file is protected in every Claude config dir (the session's
// CLAUDE_CONFIG_DIR, whatever its name, and each ~/.claude* dir), and the PII key where
// redact keeps it under XDG_STATE_HOME.
func TestSecretPathsFollowConfigDirs(t *testing.T) {
	env := secretEnv(t)
	h := env.Home
	for _, p := range []string{".claude", ".claude-work", ".claude-personal", "claude", "other", "src/app/.claude-x"} {
		os.MkdirAll(filepath.Join(h, p), 0o700)
		os.WriteFile(filepath.Join(h, p, ".credentials.json"), []byte("{}\n"), 0o600)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(h, "claude"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(h, "xdgstate"))
	for _, c := range []struct{ cmd, want string }{
		{"cat ~/.claude/.credentials.json", "context:secret-file:mask"},
		{"cat ~/.claude-work/.credentials.json", "context:secret-file:mask"},
		{"cat ~/.claude-personal/.credentials.json", "context:secret-file:mask"},
		{"grep -r token ~/.claude-personal", "context:secret-file:mask"},
		{"cat ~/claude/.credentials.json", "context:secret-file:mask"},
		{"base64 " + h + "/claude/.credentials.json", "ask:secret-file:base64"},
		{"cat ~/xdgstate/guardrails/pii-key", "context:secret-file:mask"},
		{"cat ~/xdgstate/claude-tools/pii-key", "context:secret-file:mask"},
		{"cat ~/.local/state/guardrails/pii-key", "context:secret-file:mask"},
		// The variable as written: the hook has the same CLAUDE_CONFIG_DIR as the shell.
		{"cat $CLAUDE_CONFIG_DIR/.credentials.json", "context:secret-file:mask"},
		{"cat ${CLAUDE_CONFIG_DIR}/.credentials.json", "context:secret-file:mask"},
		{"base64 $CLAUDE_CONFIG_DIR/.credentials.json", "ask:secret-file:base64"},
		{"bash -c 'cat $CLAUDE_CONFIG_DIR/.credentials.json'", "ask:secret-file:text"},
		{"cat $CLAUDE_CONFIG_DIR_OLD/.credentials.json", "allow"},
		{"cd ~ && cat .claude-personal/.credentials.json", "context:secret-file:mask"},
		// A ~/.claude* dir that does not exist yet: nothing to read, but planting one asks.
		{"echo x > ~/.claude-new/.credentials.json", "ask:secret-file:echo"},
		{"mkdir -p ~/.claude-new && cp /tmp/c ~/.claude-new/.credentials.json", "ask:secret-file:cp"},
		// Not a Claude config dir: not right in home, or not named .claude*.
		{"cat ~/other/.credentials.json", "allow"},
		{"cat ~/src/app/.claude-x/.credentials.json", "allow"},
	} {
		if got := decide(env, c.cmd).Kind(); got != c.want {
			t.Errorf("%q: got %s, want %s", c.cmd, got, c.want)
		}
	}
	if d, ok := secretFileToolDecision("Write", map[string]any{"file_path": filepath.Join(h, ".claude-new/.credentials.json")}, env.Cwd, h, ""); !ok || !d.Ask {
		t.Errorf("Write into a new ~/.claude* dir: %+v %v, want ask", d, ok)
	}
	for _, p := range []string{"claude/.credentials.json", ".claude-personal/.credentials.json", "xdgstate/guardrails/pii-key"} {
		if d, ok := secretFileToolDecision("Read", map[string]any{"file_path": filepath.Join(h, p)}, env.Cwd, h, ""); !ok || d.Exit != 2 {
			t.Errorf("Read %s: %+v %v, want blocked", p, d, ok)
		}
	}
}

// secret-paths is read from XDG_CONFIG_HOME when it is set, and the list there is protected.
func TestSecretPathsUnderXDGConfig(t *testing.T) {
	env := secretEnv(t)
	h := env.Home
	env.XDGConfig = filepath.Join(h, "xdgcfg")
	os.MkdirAll(filepath.Join(env.XDGConfig, "guardrails"), 0o700)
	os.WriteFile(filepath.Join(env.XDGConfig, "guardrails/secret-paths"), []byte("~/vault/*.key\n"), 0o600)
	os.MkdirAll(filepath.Join(h, "vault"), 0o700)
	os.WriteFile(filepath.Join(h, "vault/a.key"), []byte("x\n"), 0o600)
	for _, c := range []struct{ cmd, want string }{
		{"cat ~/vault/a.key", "context:secret-file:mask"},
		{"cat ~/xdgcfg/guardrails/secret-paths", "context:secret-file:mask"},
		{"echo x >> ~/xdgcfg/guardrails/secret-paths", "ask:secret-file:echo"},
	} {
		if got := decide(env, c.cmd).Kind(); got != c.want {
			t.Errorf("%q: got %s, want %s", c.cmd, got, c.want)
		}
	}
}

func TestSecretArgAsks(t *testing.T) {
	env := secretEnv(t)
	for _, cmd := range []string{
		"export GITHUB_TOKEN=" + fakeSecrets["github"],
		`curl -H "Authorization: Bearer ` + fakeSecrets["authHeader"] + `" https://x`,
	} {
		if d := decide(env, cmd); !d.Ask || !strings.HasPrefix(d.Rule, "secret-arg:") || strings.Contains(d.Msg, fakeSecrets["github"]) {
			t.Errorf("%q: got %s %q", cmd, d.Kind(), d.Msg)
		}
	}
}

func TestSecretRewrite(t *testing.T) {
	env := secretEnv(t)
	for _, c := range []struct{ cmd, want string }{
		{"cat ~/.config/mark.toml", "CT redact --secrets -- cat ~/.config/mark.toml"},
		{"cat -n ~/.config/mark.toml | head -5", "CT redact --secrets -- cat -n ~/.config/mark.toml | head -5"},
		{"echo hi; FOO=1 grep x ~/.config/mark.toml > /tmp/o", "echo hi; FOO=1 CT redact --secrets -- grep x ~/.config/mark.toml > /tmp/o"},
		{"cat < ~/.config/mark.toml", "CT redact --secrets -- cat < ~/.config/mark.toml"},
		{"echo $(cat ~/.config/mark.toml)", "echo $(CT redact --secrets -- cat ~/.config/mark.toml)"},
	} {
		if got, ok := secretRewrite(env, c.cmd, "CT"); !ok || got != c.want {
			t.Errorf("%q: got %q %v, want %q", c.cmd, got, ok, c.want)
		}
	}
	if got, ok := secretRewrite(env, "cat ~/.config/other.toml", "CT"); ok {
		t.Errorf("unprotected file rewritten: %q", got)
	}
}

func TestSecretFileTools(t *testing.T) {
	env := secretEnv(t)
	p := filepath.Join(env.Home, ".config/mark.toml")
	if d, ok := secretFileToolDecision("Read", map[string]any{"file_path": p}, env.Cwd, env.Home, ""); !ok || d.Exit != 2 || !strings.Contains(d.Msg, "cat ") {
		t.Errorf("Read: %+v %v", d, ok)
	}
	if d, ok := secretFileToolDecision("Write", map[string]any{"file_path": "~/.config/mark.toml"}, env.Cwd, env.Home, ""); !ok || !d.Ask {
		t.Errorf("Write: %+v %v", d, ok)
	}
	if _, ok := secretFileToolDecision("Read", map[string]any{"file_path": filepath.Join(env.Home, "work/notes.md")}, env.Cwd, env.Home, ""); ok {
		t.Errorf("Read of an ordinary file stopped")
	}
}

// End to end through the hook: the rewrite lands in updatedInput with the mask note.
func TestPreToolUseMasksSecretRead(t *testing.T) {
	code, out, rtkSaw := runPreHook(t, "cat ~/.config/mark.toml", `{"hookSpecificOutput":{"updatedInput":{"command":"rtk read"}}}`)
	if code != 0 || rtkSaw != nil || !strings.Contains(out, `CT redact --secrets -- cat ~/.config/mark.toml`) || !strings.Contains(out, "masked") {
		t.Errorf("exit %d, rtk ran=%v, out %s", code, rtkSaw != nil, out)
	}
}

// G4 (guard-evasion eval): a recursive search of a directory that holds a protected file by
// name (.env, *.tfvars, ...) printed the file's lines unmasked, because only dirs holding a
// path-listed file counted. It now runs under `redact --secrets --by-file`.
func TestRecursiveSearchIsMaskedByFile(t *testing.T) {
	env := secretEnv(t)
	os.WriteFile(filepath.Join(env.Cwd, ".env"), []byte("STRIPE_KEY=x\n"), 0o600)
	os.MkdirAll(filepath.Join(env.Cwd, "src/deep"), 0o700)
	os.WriteFile(filepath.Join(env.Cwd, "src/deep/prod.tfvars"), []byte("x\n"), 0o600)
	os.MkdirAll(filepath.Join(env.Cwd, "clean"), 0o700)
	os.WriteFile(filepath.Join(env.Cwd, "clean/a.go"), []byte("x\n"), 0o600)
	os.WriteFile(filepath.Join(env.Cwd, "clean/.env.example"), []byte("x\n"), 0o600)
	const self = "/bin/guardrails"
	const pre = self + " redact --secrets --by-file -- "
	for _, c := range []struct{ cmd, want string }{
		{"grep -rn STRIPE .", pre + "grep -rn STRIPE ."},
		{"grep -r STRIPE", pre + "grep -r STRIPE"},
		{"rg STRIPE", pre + "rg STRIPE"},
		{"rg -n STRIPE src", pre + "rg -n STRIPE src"},
		{"git grep -n STRIPE", pre + "git grep -n STRIPE"},
		{"cd src && grep -R foo . | head", "cd src && " + pre + "grep -R foo . | head"},
		// Output without file names cannot be told apart: left as before.
		{"grep -rhn STRIPE .", ""},
		{"rg --no-filename STRIPE", ""},
		{"rg --heading STRIPE", ""},
		{"grep -rZ STRIPE .", ""},
		// A directory with no protected file (a template is none), no recursion, no search:
		// untouched, so rtk still compresses an ordinary search.
		{"grep -rn foo clean", ""},
		{"cd clean && rg foo", ""},
		{"grep STRIPE notes.md", ""},
		{"ls -R", ""},
		{"git log --oneline", ""},
	} {
		got, ok := secretRewrite(env, c.cmd, self)
		if c.want == "" && ok || c.want != "" && (!ok || got != c.want) {
			t.Errorf("secretRewrite(%q) = %q, %v; want %q", c.cmd, got, ok, c.want)
		}
	}
	// A recursive search of a dir holding a path-listed file keeps the full mask.
	if got, _ := secretRewrite(env, "grep -rn token ~/.config", self); got != self+" redact --secrets -- grep -rn token ~/.config" {
		t.Errorf("path-listed dir: %q", got)
	}
	// Our own wrapper is not wrapped again.
	if got, ok := secretRewrite(env, pre+"grep -rn STRIPE .", self); ok {
		t.Errorf("rewrapped: %q", got)
	}
}
