package main

import (
	"reflect"
	"testing"
)

func TestParseShell(t *testing.T) {
	tests := []struct {
		name   string
		cmd    string
		want   [][]string // argv of each simple command, wrappers peeled
		parsed bool
	}{
		{"and list", "cd x && make; ls | wc", [][]string{{"cd", "x"}, {"make"}, {"ls"}, {"wc"}}, true},
		{"subshell and subst", "(a b) && echo $(c d)", [][]string{{"a", "b"}, {"echo", "$(c d)"}, {"c", "d"}}, true},
		{"quotes are one word", `echo "a b" 'c d' e\ f`, [][]string{{"echo", "a b", "c d", "e f"}}, true},
		{"params stay literal", `rm -rf "$HOME" ${HOME}`, [][]string{{"rm", "-rf", "$HOME", "${HOME}"}}, true},
		{"wrapper peeled", "sudo -u root FOO=1 rm x", [][]string{{"rm", "x"}}, true},
		{"bash -c recursion", `bash -c 'a; b c'`, [][]string{{"bash", "-c", "a; b c"}, {"a"}, {"b", "c"}}, true},
		{"eval recursion", `eval "a b"`, [][]string{{"eval", "a b"}, {"a", "b"}}, true},
		{"heredoc data is not run", "cat <<'EOF'\nrm x\nEOF", [][]string{{"cat"}}, true},
		{"heredoc into shell is run", "bash <<'EOF'\nrm x\nEOF", [][]string{{"bash"}, {"rm", "x"}}, true},
		{"here-string into shell", `bash <<< 'rm x'`, [][]string{{"bash"}, {"rm", "x"}}, true},
		{"unterminated quote", `echo "a`, nil, false},
		{"unparseable inner script", `bash -c 'echo "a'`, nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmds, ok := parseShell(tt.cmd)
			if ok != tt.parsed {
				t.Fatalf("parsed = %v, want %v", ok, tt.parsed)
			}
			var got [][]string
			for _, c := range cmds {
				if len(c.Words) > 0 {
					got = append(got, c.Words)
				}
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("argv = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseShellRedirs(t *testing.T) {
	cmds, ok := parseShell("echo a > /x/y; echo b >> /z; cat < /in; { echo c; } > /w")
	if !ok {
		t.Fatal("not parsed")
	}
	var got []astRedir
	for _, c := range cmds {
		got = append(got, c.Redirs...)
	}
	want := []astRedir{{">", "/x/y"}, {">>", "/z"}, {"<", "/in"}, {">", "/w"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("redirs = %v, want %v", got, want)
	}
}
