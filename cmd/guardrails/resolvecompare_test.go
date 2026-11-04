package main

import (
	"os"
	"path/filepath"
	"testing"
)

// resolveForCompare follows symlinks as far as the path exists and keeps the rest as typed.
func TestResolveForCompare(t *testing.T) {
	tmp, err := filepath.EvalSymlinks(t.TempDir()) // macOS: /var -> /private/var
	if err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(tmp, "real")
	file := filepath.Join(real, "file")
	for _, err := range []error{
		os.MkdirAll(filepath.Join(real, "sub"), 0o755),
		os.WriteFile(file, nil, 0o644),
		os.Symlink(real, filepath.Join(tmp, "link")),
		os.Symlink(filepath.Join(tmp, "gone"), filepath.Join(real, "dangling")),
		os.Symlink("loop", filepath.Join(real, "loop")),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}

	cases := []struct{ name, p, cwd, want string }{
		{"existing file", file, "/", file},
		{"existing dir", filepath.Join(real, "sub"), "/", filepath.Join(real, "sub")},
		{"missing tail under an existing dir", filepath.Join(real, "sub", "a", "b"), "/", filepath.Join(real, "sub", "a", "b")},
		{"symlinked dir mid-path", filepath.Join(tmp, "link", "file"), "/", file},
		{"symlinked dir with missing tail", filepath.Join(tmp, "link", "x", "y"), "/", filepath.Join(real, "x", "y")},
		{"dangling symlink is kept as named", filepath.Join(real, "dangling"), "/", filepath.Join(real, "dangling")},
		{"under a dangling symlink", filepath.Join(real, "dangling", "z"), "/", filepath.Join(real, "dangling", "z")},
		{"symlink loop is kept as named", filepath.Join(real, "loop", "q"), "/", filepath.Join(real, "loop", "q")},
		{"file used as a dir", filepath.Join(file, "sub"), "/", filepath.Join(file, "sub")},
		{"relative to cwd through a symlink", "file", filepath.Join(tmp, "link"), file},
		{"relative, missing", "nope/x", filepath.Join(tmp, "link"), filepath.Join(real, "nope", "x")},
		{"unclean path", filepath.Join(tmp, "link") + "/./sub/../file", "/", file},
		{"missing right up to the root", "/no-such-root-dir-xyz/a/b", "/", "/no-such-root-dir-xyz/a/b"},
		{"root", "/", "/", "/"},
	}
	for _, c := range cases {
		if got := resolveForCompare(c.p, c.cwd); got != c.want {
			t.Errorf("%s: resolveForCompare(%q, %q) = %q, want %q", c.name, c.p, c.cwd, got, c.want)
		}
	}
}
