package main

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"
	"testing"
)

func TestPrivateWrap(t *testing.T) {
	var many, want strings.Builder
	for i := 0; i < 120; i++ {
		fmt.Fprintf(&many, "r%d\n", i)
	}
	for _, c := range [][2]int{{0, 50}, {50, 100}, {100, 120}} {
		want.WriteString("<private>\n")
		for i := c[0]; i < c[1]; i++ {
			fmt.Fprintf(&want, "r%d\n", i)
		}
		want.WriteString("</private>\n")
	}
	tests := []struct{ name, in, want string }{
		{"empty", "", ""},
		{"one line", "a,b\n", "<private>\na,b\n</private>\n"},
		{"no trailing newline", "a,b", "<private>\na,b\n</private>\n"},
		{"exactly 50 lines", strings.Repeat("x\n", 50), "<private>\n" + strings.Repeat("x\n", 50) + "</private>\n"},
		{"120 lines in 3 chunks", many.String(), want.String()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := string(privateWrap([]byte(tt.in))); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// A close tag in the data would end the block early and leave the rest for claude-mem to store.
func TestPrivateWrapNeutralizesCloseTags(t *testing.T) {
	closeTag := regexp.MustCompile(`</private\s*>`) // claude-mem's CLOSE_TAG_RE for this name
	for _, in := range []string{"1,x</private>leak\n2,PAST\n", "a</private >b\n", "</private>\n</private>\n"} {
		got := string(privateWrap([]byte(in)))
		if n := len(closeTag.FindAllString(got, -1)); n != 1 || !strings.HasSuffix(got, "</private>\n") {
			t.Errorf("privateWrap(%q) = %q: %d close tags, want 1 at the end", in, got, n)
		}
		if s := stripPrivate(got); s != "\n" {
			t.Errorf("stripPrivate left %q", s)
		}
	}
}

func TestStripPrivate(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{"none", "plain text", "plain text"},
		{"one block", "a\n<private>\nx@y.org\n</private>\nb", "a\n\nb"},
		{"two blocks", "<private>\n1\n</private>\n<private>\n2\n</private>\nok", "\n\nok"},
		{"unclosed kept", "how <private> works", "how <private> works"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := stripPrivate(tt.in); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRedactPrivateWrapsStdoutOnly(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runRedact(testRedactor(), []string{"--pii", "--private", "--in=csv", "--", "sh", "-c",
		`printf 'id,email\n1,x@y.org\n'; echo 'oops' >&2`},
		strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	got := stdout.String()
	if !strings.HasPrefix(got, "<private>\nid,email\n1,[email:") || !strings.HasSuffix(got, "\n</private>\n") || strings.Contains(got, "x@y.org") {
		t.Errorf("stdout: %q", got)
	}
	if stderr.String() != "oops\n" {
		t.Errorf("stderr: %q", stderr.String())
	}
	stdout.Reset()
	runRedact(testRedactor(), []string{"--pii", "--private", "--", "true"}, strings.NewReader(""), &stdout, &stderr)
	if stdout.Len() != 0 {
		t.Errorf("empty output wrapped: %q", stdout.String())
	}
}
