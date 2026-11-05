package main

// The guardrails plugin (plugins/guardrails) runs every hook through scripts/run.sh, which
// fetches this version's binary on SessionStart and otherwise fails closed (opt-out: fail_open).

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const pluginDir = "../../plugins/guardrails"

// fakeTool stands in for the guardrails binary: it prints its args, the env the shim sets, and stdin.
const fakeTool = "#!/bin/sh\necho \"args=$*\"\necho \"off=$CLAUDE_TOOLS_OFF\"\necho \"allow=$CLAUDE_TOOLS_PII_ALLOW\"\necho \"state=$HANDOFF_STATE_DIR\"\necho \"stdin=$(cat)\"\nexit 3\n"

type pluginManifest struct {
	Version    string `json:"version"`
	UserConfig map[string]struct {
		Default any `json:"default"`
	} `json:"userConfig"`
}

func readManifest(t *testing.T) pluginManifest {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(pluginDir, ".claude-plugin/plugin.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m pluginManifest
	if err := json.Unmarshal(b, &m); err != nil || m.Version == "" {
		t.Fatalf("plugin.json: %v, version %q", err, m.Version)
	}
	return m
}

func runShim(t *testing.T, env map[string]string, stdin string, args ...string) (int, string) {
	t.Helper()
	root, _ := filepath.Abs(pluginDir)
	c := exec.Command("sh", append([]string{filepath.Join(root, "scripts/run.sh")}, args...)...)
	// A guard with no binary starts an install; point it at a release that does not exist.
	c.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "CLAUDE_PLUGIN_ROOT=" + root, "CLAUDE_TOOLS_RELEASE_URL=file://" + t.TempDir() + "/none"}
	for k, v := range env {
		c.Env = append(c.Env, k+"="+v)
	}
	c.Stdin = strings.NewReader(stdin)
	var out, errb bytes.Buffer
	c.Stdout, c.Stderr = &out, &errb
	err := c.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), out.String()
	} else if err != nil {
		t.Fatalf("run.sh: %v %s", err, errb.String())
	}
	return 0, out.String()
}

func writeExe(t *testing.T, path, body string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestShimNoBinaryFailsClosed(t *testing.T) {
	env := map[string]string{"CLAUDE_PLUGIN_DATA": t.TempDir()}
	code, out := runShim(t, env, `{"tool_name":"Bash"}`, "pre-tool-use")
	if code != 2 || out != "" {
		t.Fatalf("no binary: exit %d, stdout %q; want 2 and nothing on stdout (block)", code, out)
	}
	env["CLAUDE_PLUGIN_OPTION_FAIL_OPEN"] = "true"
	if code, out = runShim(t, env, `{"tool_name":"Bash"}`, "pre-tool-use"); code != 0 || out != "" {
		t.Fatalf("no binary with fail_open: exit %d, stdout %q; want 0 and nothing (allow)", code, out)
	}
}

// A missing binary blocks only the subcommands hooks.json marks onFailure: block (the guards).
// The rest exit 0 silently: exit 2 at Stop would keep Claude from ending its turn, and at
// PreCompact would block every compaction.
func TestShimNoBinaryBlocksOnlyGuards(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(pluginDir, "hooks/hooks.json"))
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Hooks map[string][]struct {
			Hooks []struct{ Command, OnFailure string }
		}
	}
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	guard := map[string]bool{}
	for _, groups := range f.Hooks {
		for _, g := range groups {
			for _, h := range g.Hooks {
				sub := h.Command[strings.LastIndex(h.Command, " ")+1:]
				guard[sub] = guard[sub] || h.OnFailure == "block"
			}
		}
	}
	delete(guard, "install")
	for sub, blocks := range guard {
		env := map[string]string{"CLAUDE_PLUGIN_DATA": t.TempDir()}
		code, out := runShim(t, env, "{}", sub)
		if want := map[bool]int{true: 2, false: 0}[blocks]; code != want || out != "" {
			t.Errorf("%s with no binary: exit %d, stdout %q; want %d and nothing", sub, code, out, want)
		}
	}
}

// The plugin ships every guard on: the default off list is empty, and the manifest says so.
func TestDefaultGuardsAllOn(t *testing.T) {
	if d := readManifest(t).UserConfig["disabled_guards"].Default; d != "" {
		t.Errorf("disabled_guards default %q, want empty", d)
	}
}

func TestShimRunsVersionedBinary(t *testing.T) {
	m := readManifest(t)
	data := t.TempDir()
	writeExe(t, filepath.Join(data, "bin", m.Version, "guardrails"), fakeTool)
	env := map[string]string{"CLAUDE_PLUGIN_DATA": data}

	code, out := runShim(t, env, "IN", "pre-tool-use")
	want := "args=pre-tool-use\noff=" + fmt.Sprint(m.UserConfig["disabled_guards"].Default) + "\nallow=\nstate=" + data + "\nstdin=IN\n"
	if code != 3 || out != want {
		t.Fatalf("defaults: exit %d, stdout %q; want 3 and %q", code, out, want)
	}

	env["CLAUDE_PLUGIN_OPTION_DISABLED_GUARDS"] = ""
	env["CLAUDE_PLUGIN_OPTION_ALLOWED_EMAIL_DOMAINS"] = "@example.com"
	if _, out = runShim(t, env, "", "read-dedup"); !strings.Contains(out, "off=\nallow=@example.com\n") {
		t.Fatalf("options: stdout %q", out)
	}

	other := filepath.Join(t.TempDir(), "ct")
	writeExe(t, other, "#!/bin/sh\necho override\n")
	env["CLAUDE_TOOLS_BIN"] = other
	if _, out = runShim(t, env, "", "pre-tool-use"); out != "override\n" {
		t.Fatalf("CLAUDE_TOOLS_BIN: stdout %q", out)
	}
}

// release writes v<version>/guardrails_<os>_<arch>.tar.gz and checksums.txt under a temp
// dir, laid out like a GitHub release download URL, and returns its file:// URL.
func release(t *testing.T, version, sum string) string {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: "guardrails", Mode: 0o755, Size: int64(len(fakeTool))})
	tw.Write([]byte(fakeTool))
	tw.Close()
	gz.Close()
	asset := "guardrails_" + runtime.GOOS + "_" + runtime.GOARCH + ".tar.gz"
	if sum == "" {
		h := sha256.Sum256(buf.Bytes())
		sum = hex.EncodeToString(h[:])
	}
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "v"+version), 0o755)
	os.WriteFile(filepath.Join(dir, "v"+version, asset), buf.Bytes(), 0o644)
	os.WriteFile(filepath.Join(dir, "v"+version, "checksums.txt"), []byte(sum+"  "+asset+"\n"), 0o644)
	return "file://" + dir
}

func TestShimInstall(t *testing.T) {
	m := readManifest(t)
	data := t.TempDir()
	env := map[string]string{"CLAUDE_PLUGIN_DATA": data, "CLAUDE_TOOLS_RELEASE_URL": release(t, m.Version, "")}
	if code, _ := runShim(t, env, "{}", "install"); code != 0 {
		t.Fatalf("install exit %d", code)
	}
	if code, out := runShim(t, env, "X", "pre-tool-use"); code != 3 || !strings.Contains(out, "stdin=X") {
		t.Fatalf("installed binary: exit %d, stdout %q", code, out)
	}
	// Installed: a later SessionStart does not download; it runs the doctor, whose output
	// (the stub's echo here) passes through.
	env["CLAUDE_TOOLS_RELEASE_URL"] = "file:///nonexistent"
	if code, out := runShim(t, env, "{}", "install"); code != 0 || !strings.HasPrefix(out, "args=doctor -hook\n") {
		t.Fatalf("second install: exit %d, stdout %q", code, out)
	}
}

func TestShimInstallRejectsBadChecksum(t *testing.T) {
	m := readManifest(t)
	data := t.TempDir()
	env := map[string]string{"CLAUDE_PLUGIN_DATA": data, "CLAUDE_TOOLS_RELEASE_URL": release(t, m.Version, strings.Repeat("0", 64))}
	code, out := runShim(t, env, "{}", "install")
	var msg struct{ SystemMessage string }
	if code != 0 || json.Unmarshal([]byte(out), &msg) != nil || !strings.Contains(msg.SystemMessage, "blocked") {
		t.Fatalf("bad checksum: exit %d, stdout %q; want 0 and a systemMessage", code, out)
	}
	if _, err := os.Stat(filepath.Join(data, "bin", m.Version, "guardrails")); err == nil {
		t.Fatal("bad checksum: binary installed anyway")
	}
	if code, out := runShim(t, env, "{}", "pre-tool-use"); code != 2 || out != "" {
		t.Fatalf("after failed install: exit %d, stdout %q; want block", code, out)
	}
}

// Every hook in hooks.json names install or a real subcommand.
func TestPluginHooksUseKnownCommands(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(pluginDir, "hooks/hooks.json"))
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Hooks map[string][]struct {
			Hooks []struct{ Command string }
		}
	}
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	n := 0
	for event, groups := range f.Hooks {
		for _, g := range groups {
			for _, h := range g.Hooks {
				_, sub, ok := strings.Cut(h.Command, "/scripts/run.sh\" ")
				if !ok || sub != "install" && commands[sub] == nil {
					t.Errorf("%s hook %q: want \"${CLAUDE_PLUGIN_ROOT}/scripts/run.sh\" <install|subcommand>", event, h.Command)
				}
				n++
			}
		}
	}
	if n == 0 {
		t.Fatal("hooks.json has no hooks")
	}
}

// The guards fail closed: a guard binary that crashes, is killed or hangs blocks the tool
// call instead of letting it through unchecked. A missing binary blocks them too: run.sh exits
// 2 then, unless the fail_open option is set (see TestShimNoBinaryBlocksOnlyGuards). The
// other hooks (install, retry-breaker, session-env, receipt, gate, compact-guard, and
// config-gate at SessionStart, where it only takes a snapshot) have no onFailure.
func TestPluginGuardHooksFailClosed(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(pluginDir, "hooks/hooks.json"))
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Hooks map[string][]struct {
			Hooks []struct{ Command, OnFailure string }
		}
	}
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	for event, groups := range f.Hooks {
		for _, g := range groups {
			for _, h := range g.Hooks {
				want := ""
				if event == "PreToolUse" || event == "ConfigChange" {
					want = "block"
				}
				if h.OnFailure != want {
					t.Errorf("%s hook %q: onFailure %q, want %q", event, h.Command, h.OnFailure, want)
				}
			}
		}
	}
}

// The rtk options reach the binary as CLAUDE_TOOLS_RTK and CLAUDE_TOOLS_RTK_CMD, and are off
// by default.
func TestShimPassesRTKOptions(t *testing.T) {
	m := readManifest(t)
	if d, ok := m.UserConfig["rtk"]; !ok || d.Default != false {
		t.Fatalf("plugin.json needs an rtk option that defaults to off, got %+v", d)
	}
	if _, ok := m.UserConfig["rtk_command"]; !ok {
		t.Fatal("plugin.json needs an rtk_command option")
	}
	data := t.TempDir()
	writeExe(t, filepath.Join(data, "bin", m.Version, "guardrails"), "#!/bin/sh\necho \"rtk=$CLAUDE_TOOLS_RTK cmd=$CLAUDE_TOOLS_RTK_CMD\"\n")
	env := map[string]string{"CLAUDE_PLUGIN_DATA": data}
	if _, out := runShim(t, env, "", "pre-tool-use"); out != "rtk= cmd=\n" {
		t.Errorf("defaults: %q", out)
	}
	env["CLAUDE_PLUGIN_OPTION_RTK"] = "true"
	env["CLAUDE_PLUGIN_OPTION_RTK_COMMAND"] = "/opt/bin/rtk"
	if _, out := runShim(t, env, "", "pre-tool-use"); out != "rtk=true cmd=/opt/bin/rtk\n" {
		t.Errorf("options: %q", out)
	}
	// Outside the plugin the same env vars pass through.
	env = map[string]string{"CLAUDE_PLUGIN_DATA": data, "CLAUDE_TOOLS_RTK": "1"}
	if _, out := runShim(t, env, "", "pre-tool-use"); out != "rtk=1 cmd=\n" {
		t.Errorf("env fallback: %q", out)
	}
}
