package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// smartWorkspace points the workspace root at dir and HOME/XDG into it, so the secret guard
// reads only test-owned lists.
func smartWorkspace(t *testing.T, dir string) {
	t.Helper()
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_PROJECT_DIR", dir)
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "xdg"))
}

func smartPkgWorkspace(t *testing.T) {
	t.Helper()
	wd, _ := os.Getwd()
	smartWorkspace(t, wd)
}

func TestMemSmartOutline(t *testing.T) {
	smartPkgWorkspace(t)
	tests := []struct {
		file string
		want []string
	}{
		{"testdata/memory/sample.go", []string{"(go, 25 lines)", "Greeter (L9-11)", "Greeter.Greet (L14-16)", "Shout (L19-21)", "helper (L23-25)", "Greet returns a greeting"}},
		{"testdata/memory/sample.py", []string{"(python, 17 lines)", "Counter (L4-12)", "bump (L10-12)", "scale (L15-17)"}},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			got, err := smartOutline(map[string]any{"file_path": tt.file})
			if err != nil {
				t.Fatal(err)
			}
			for _, w := range tt.want {
				if !strings.Contains(got, w) {
					t.Errorf("outline missing %q:\n%s", w, got)
				}
			}
			if strings.Contains(got, "return ") {
				t.Errorf("outline contains a body:\n%s", got)
			}
		})
	}
}

func TestMemSmartUnfold(t *testing.T) {
	smartPkgWorkspace(t)
	got, err := smartUnfold(map[string]any{"file_path": "testdata/memory/sample.go", "symbol_name": "Shout"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "return strings.ToUpper(s)") || !strings.Contains(got, "// Shout upper-cases s.") {
		t.Errorf("unfold missing body/doc:\n%s", got)
	}
	if strings.Contains(got, "helper") || strings.Contains(got, "Greet") {
		t.Errorf("unfold returned more than one symbol:\n%s", got)
	}
	m, _ := smartUnfold(map[string]any{"file_path": "testdata/memory/sample.go", "symbol_name": "Greet"})
	if !strings.Contains(m, "func (g *Greeter) Greet()") || strings.Contains(m, "Shout") {
		t.Errorf("method unfold by short name:\n%s", m)
	}
	py, _ := smartUnfold(map[string]any{"file_path": "testdata/memory/sample.py", "symbol_name": "bump"})
	if !strings.Contains(py, "self.n += by") || strings.Contains(py, "scale") {
		t.Errorf("python method unfold:\n%s", py)
	}
	miss, err := smartUnfold(map[string]any{"file_path": "testdata/memory/sample.go", "symbol_name": "nope"})
	if err != nil || !strings.Contains(miss, `Symbol "nope" not found`) {
		t.Errorf("missing symbol: %q %v", miss, err)
	}
	if _, err := smartUnfold(map[string]any{"file_path": "testdata/memory/absent.go", "symbol_name": "x"}); err == nil {
		t.Error("missing file: want error")
	}
}

func TestMemSmartSearch(t *testing.T) {
	dir := t.TempDir()
	smartWorkspace(t, dir)
	if err := os.MkdirAll(filepath.Join(dir, "node_modules", "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	src, _ := os.ReadFile("testdata/memory/sample.go")
	os.WriteFile(filepath.Join(dir, "a.go"), src, 0o644)
	py, _ := os.ReadFile("testdata/memory/sample.py")
	os.WriteFile(filepath.Join(dir, "b.py"), py, 0o644)
	os.WriteFile(filepath.Join(dir, "node_modules", "x", "c.go"), src, 0o644)

	got, err := smartSearch(map[string]any{"query": "shout", "path": dir})
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{`Smart Search: "shout"`, "Matching Symbols", "function Shout (a.go:19)", "Folded File Views", "Greeter.Greet"} {
		if !strings.Contains(got, w) {
			t.Errorf("search missing %q:\n%s", w, got)
		}
	}
	if strings.Contains(got, "node_modules") || strings.Contains(got, "b.py (python") {
		t.Errorf("search leaked skipped/unrelated file:\n%s", got)
	}
	got, _ = smartSearch(map[string]any{"query": "scale", "path": dir, "file_pattern": ".py"})
	if !strings.Contains(got, "function scale (b.py:15)") {
		t.Errorf("file_pattern search:\n%s", got)
	}
	got, _ = smartSearch(map[string]any{"query": "scale", "path": dir, "file_pattern": ".go"})
	if strings.Contains(got, "scale (") {
		t.Errorf("file_pattern did not filter:\n%s", got)
	}
	if _, err := smartSearch(map[string]any{"path": dir}); err == nil {
		t.Error("missing query: want error")
	}
}

func TestMemSmartOtherLanguages(t *testing.T) {
	dir := t.TempDir()
	smartWorkspace(t, dir)
	files := map[string]string{
		"a.ts": "import x from 'y';\n\n/** Adds. */\nexport function add(a: number, b: number): number {\n  return a + b;\n}\n\nexport class Box {\n  open(): void {}\n}\n\ninterface Shape {\n  area(): number;\n}\n",
		"b.js": "const twice = (n) => n * 2;\nclass K { run() {} }\n",
		"c.sh": "# Say hi.\nhello() {\n  echo hi\n}\n",
	}
	want := map[string][]string{
		"a.ts": {"add [exported] (L4-6)", "Box [exported] (L8-10)", "open (L9)", "Shape (L12-14)", "Adds."},
		"b.js": {"twice (L1)", "K (L2)", "run (L2)"},
		"c.sh": {"hello (L2-4)", "Say hi."},
	}
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		got, err := smartOutline(map[string]any{"file_path": p})
		if err != nil {
			t.Fatal(err)
		}
		for _, w := range want[name] {
			if !strings.Contains(got, w) {
				t.Errorf("%s outline missing %q:\n%s", name, w, got)
			}
		}
	}
	txt := filepath.Join(dir, "n.txt")
	os.WriteFile(txt, []byte("hi"), 0o644)
	if got, err := smartOutline(map[string]any{"file_path": txt}); err != nil || !strings.Contains(got, "Could not parse") {
		t.Errorf("unsupported: %q %v", got, err)
	}
}

func TestMemSmartAccessRules(t *testing.T) {
	ws := t.TempDir()
	smartWorkspace(t, ws)
	outside := t.TempDir()
	src, _ := os.ReadFile("testdata/memory/sample.go")
	os.WriteFile(filepath.Join(outside, "out.go"), src, 0o644)
	os.WriteFile(filepath.Join(ws, "ok.go"), src, 0o644)
	if err := os.Symlink(filepath.Join(outside, "out.go"), filepath.Join(ws, "link.go")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(ws, "linkdir")); err != nil {
		t.Fatal(err)
	}
	// A protected file: named in the user's secret-paths list, which the guard reads.
	secret := filepath.Join(ws, "vault.go")
	os.WriteFile(secret, []byte("package v\n\nfunc TopSecretSymbol() string { return \"hunter2\" }\n"), 0o644)
	os.MkdirAll(filepath.Join(ws, "xdg", "memory"), 0o755)
	os.WriteFile(filepath.Join(ws, "xdg", "memory", "secret-paths"), []byte(secret+"\n"), 0o644)
	os.WriteFile(filepath.Join(ws, ".netrc"), []byte("machine x password hunter2\n"), 0o644)

	denied := func(t *testing.T, got string, err error, banned ...string) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), "access denied") {
			t.Errorf("want access denied, got %q err=%v", got, err)
		}
		for _, b := range banned {
			if strings.Contains(got, b) || (err != nil && strings.Contains(err.Error(), b)) {
				t.Errorf("leaked %q: %q %v", b, got, err)
			}
		}
	}
	t.Run("outside path", func(t *testing.T) {
		for _, f := range []string{filepath.Join(outside, "out.go"), "../" + filepath.Base(outside) + "/out.go"} {
			got, err := smartOutline(map[string]any{"file_path": f})
			denied(t, got, err, "Shout")
			got, err = smartUnfold(map[string]any{"file_path": f, "symbol_name": "Shout"})
			denied(t, got, err, "ToUpper")
		}
		got, err := smartSearch(map[string]any{"query": "Shout", "path": outside})
		denied(t, got, err, "Shout")
	})
	t.Run("symlink escape", func(t *testing.T) {
		got, err := smartUnfold(map[string]any{"file_path": "link.go", "symbol_name": "Shout"})
		denied(t, got, err, "ToUpper")
		got, err = smartSearch(map[string]any{"query": "Shout", "path": "linkdir"})
		denied(t, got, err, "Shout")
		// A search from the workspace skips the escaping symlinked file but finds ok.go.
		got, err = smartSearch(map[string]any{"query": "Shout"})
		if err != nil || !strings.Contains(got, "ok.go") || strings.Contains(got, "link.go") {
			t.Errorf("workspace search: %q %v", got, err)
		}
	})
	t.Run("protected file", func(t *testing.T) {
		got, err := smartUnfold(map[string]any{"file_path": "vault.go", "symbol_name": "TopSecretSymbol"})
		denied(t, got, err, "hunter2")
		got, err = smartOutline(map[string]any{"file_path": "vault.go"})
		denied(t, got, err, "TopSecretSymbol")
		got, err = smartUnfold(map[string]any{"file_path": ".netrc", "symbol_name": "x"})
		denied(t, got, err, "hunter2")
		for _, q := range []string{"TopSecretSymbol", "hunter2"} {
			got, err = smartSearch(map[string]any{"query": q})
			// The header echoes the query; the file's name and body must not appear.
			if err != nil || strings.Contains(got, "vault.go") || strings.Contains(got, "func TopSecretSymbol") || strings.Contains(got, "return \"hunter2\"") {
				t.Errorf("search %q exposed protected file: %q %v", q, got, err)
			}
		}
	})
	t.Run("project dir unset uses cwd", func(t *testing.T) {
		t.Setenv("CLAUDE_PROJECT_DIR", "")
		wd, _ := os.Getwd()
		if _, err := smartOutline(map[string]any{"file_path": filepath.Join(wd, "testdata/memory/sample.go")}); err != nil {
			t.Errorf("cwd-rooted read failed: %v", err)
		}
		if _, err := smartOutline(map[string]any{"file_path": filepath.Join(outside, "out.go")}); err == nil {
			t.Error("outside cwd allowed")
		}
	})
}
