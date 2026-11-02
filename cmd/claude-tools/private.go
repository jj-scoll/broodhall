package main

import (
	"bytes"
	"regexp"
)

// <private> blocks keep client data out of claude-mem: it strips balanced <private>…</private>
// from tool output before storing observations. redact --private wraps masked output in
// blocks of at most privateChunk lines (each closed, so one stray tag cannot unbalance the rest);
// stripPrivate drops those blocks from text claude-tools itself persists (ingest, write-handoff).

const privateChunk = 50

var privateRE = regexp.MustCompile(`(?s)<private>.*?</private>`)

// privateWrap emits b as <private>\n…\n</private>\n chunks; empty b emits nothing.
// A "</private" in the data becomes "<\/private" so it cannot close a block early.
func privateWrap(b []byte) []byte {
	b = bytes.ReplaceAll(b, []byte("</private"), []byte(`<\/private`))
	var out bytes.Buffer
	lines := 0
	for len(b) > 0 {
		if lines == 0 {
			out.WriteString("<private>\n")
		}
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			i = len(b) - 1
		}
		out.Write(b[:i+1])
		if b[i] != '\n' {
			out.WriteByte('\n')
		}
		b = b[i+1:]
		if lines++; lines == privateChunk || len(b) == 0 {
			out.WriteString("</private>\n")
			lines = 0
		}
	}
	return out.Bytes()
}

// stripPrivate removes balanced <private>…</private> blocks from s.
func stripPrivate(s string) string {
	return privateRE.ReplaceAllString(s, "")
}
