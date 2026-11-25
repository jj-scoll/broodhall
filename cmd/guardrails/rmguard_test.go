package main

import "testing"

func TestRmRootTarget(t *testing.T) {
	const home = "/nohome/user"
	tests := []struct {
		name string
		cmd  string
		want string // "" = not a root-target rm
	}{
		{"plain root", "rm -rf /", "/"},
		{"root glob", "rm -rf /*", "/"},
		{"tilde", "rm -rf ~", "~"},
		{"tilde slash", "rm -rf ~/", "~"},
		{"tilde glob", "rm -rf ~/*", "~"},
		{"HOME", "rm -rf $HOME", "$HOME"},
		{"HOME braces", "rm -rf ${HOME}", "$HOME"},
		{"HOME quoted", `rm -rf "$HOME"`, "$HOME"},
		{"HOME glob", "rm -rf $HOME/*", "$HOME"},
		{"literal home dir", "rm -rf /nohome/user", "~"},
		{"/home", "rm -rf /home", "/home"},
		{"/home slash", "rm -rf /home/", "/home"},
		{"/nix", "rm -rf /nix", "/nix"},
		{"/storage", "rm -rf /storage", "/storage"},
		{"fr order", "rm -fr /", "/"},
		{"split flags", "rm -r -f /", "/"},
		{"long flags", "rm --recursive --force /", "/"},
		{"capital R", "rm -Rf /", "/"},
		{"extra flag", "rm -rfv /", "/"},
		{"after double dash", "rm -rf -- /", "/"},
		{"second target", "rm -rf /tmp/x /", "/"},
		{"sudo", "sudo rm -rf /", "/"},
		{"after &&", "cd x && rm -rf /", "/"},
		{"after semicolon", "ls; rm -rf ~", "~"},
		{"in pipe chain", "true | rm -rf /", "/"},
		{"subshell", "(rm -rf /)", "/"},
		{"command subst", "echo $(rm -rf /)", "/"},
		{"env prefix", "FOO=1 rm -rf /", "/"},
		{"abs rm path", "/bin/rm -rf /", "/"},
		{"bash -c", `bash -c 'rm -rf /'`, "/"},
		{"sudo sh -c", `sudo sh -c "cd /x && rm -rf ~"`, "~"},
		{"ssh remote", `ssh host 'rm -rf /'`, "/"},
		{"eval", `eval "rm -rf /"`, "/"},
		{"second line", "ls\nrm -rf /", "/"},
		{"unbalanced quote falls back to substring", `echo "don't; rm -rf /`, "/"},

		// paths are resolved before they are compared: dot segments, $HOME and ~, the
		// command's own assignments, and glob characters that can match a protected target
		{"traversal to home", "rm -rf /nohome/../nohome/user", "~"},
		{"HOME traversal", "rm -rf $HOME/../user", "~"},
		{"tilde up to /home", "rm -rf ~/../../home", "/home"},
		{"dot segment root", "rm -rf /./", "/"},
		{"double slash", "rm -rf //", "/"},
		{"bracket glob", "rm -rf /nohom[e]/user", "~"},
		{"star segment", "rm -rf /h*", "/home"},
		{"question mark", "rm -rf /ni?", "/nix"},
		{"assigned target", `T=~; rm -rf "$T"`, "~"},
		{"assigned HOME target", "T=$HOME; rm -rf $T", "$HOME"},
		{"exported target", "export T=/nohome/user && rm -rf \"$T\"", "~"},
		{"traversal below tmp", "rm -rf /tmp/../tmp/x", ""},
		{"HOME subdir traversal", "rm -rf $HOME/.cache/../.cache/pip", ""},
		{"glob below home", "rm -rf /nohome/user/src/*", ""},
		{"glob in tmp", "rm -rf /tmp/*", ""},
		{"assigned subdir", `D=~/.cache/x; rm -rf "$D"`, ""},
		{"relative dotdot", "rm -rf ../build", ""},

		// heredocs: bodies are data unless a shell reads them
		{"heredoc data with odd quotes", "python3 - <<'EOF'\ns = '''don't\nrm -rf /\nEOF\nls", ""},
		{"heredoc file write", "cat > clean.sh <<'EOF'\nrm -rf /\nEOF", ""},
		{"heredoc into bash", "bash <<'EOF'\nrm -rf /\nEOF", "/"},
		{"heredoc into ssh", "ssh host <<EOF\nrm -rf ~\nEOF", "~"},
		{"command after heredoc", "cat <<EOF\nhello\nEOF\nrm -rf /", "/"},
		{"dash heredoc indented end", "cat <<-EOF\n\tx '\n\tEOF\nrm -rf /", "/"},

		// the 45/45 false positives: subpaths
		{"scratchpad", "rm -rf /tmp/claude-1000/-home-user-ask/abc/scratchpad/work", ""},
		{"nix store", "rm -rf /nix/store/abc-foo.drv", ""},
		{"project dir", "rm -rf /nohome/user/src/x/build", ""},
		{"tilde subdir", "rm -rf ~/.cache/tool", ""},
		{"HOME subdir", "rm -rf $HOME/projects/x/node_modules", ""},
		{"storage subdir", "rm -rf /storage/old", ""},
		// not a recursive force rm, or not rm
		{"no force", "rm -r /tmp/x", ""},
		{"no recursion", "rm -f /", ""},
		{"echo text", "echo rm -rf /", ""},
		{"grep text", `grep -n "rm -rf /" notes.md`, ""},
		{"commit message", `git commit -m "block rm -rf / properly"`, ""},
		{"other binary", "farm -rf /", ""},
		{"relative", "rm -rf build/", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _ := rmRootTarget(tt.cmd, home)
			if got != tt.want {
				t.Errorf("rmRootTarget(%q) = %q, want %q", tt.cmd, got, tt.want)
			}
		})
	}
}
