package main

import (
	"strings"
	"testing"
)

func TestMemdTranscriptReplies(t *testing.T) {
	lines := memdFixture(t)
	const path = "/home/u/.claude/projects/-work-demo/sess-basic-0001.jsonl"
	evs := parseMemdLines(path, "acct", lines, nil)
	var replies []memdEvent
	for _, e := range evs {
		if e.Kind == "reply" {
			replies = append(replies, e)
		}
	}
	if len(replies) != 2 {
		t.Fatalf("replies = %+v, want 2 (the thinking block is skipped)", replies)
	}
	if replies[0].ReplyText != "Checking the endpoint." || replies[1].ReplyText != "Done." {
		t.Errorf("reply texts = %q, %q", replies[0].ReplyText, replies[1].ReplyText)
	}
	if replies[0].ReplyKey == "" || replies[0].ReplyKey == replies[1].ReplyKey {
		t.Errorf("reply keys = %q, %q, want distinct and non-empty", replies[0].ReplyKey, replies[1].ReplyKey)
	}
	for _, e := range evs {
		if strings.Contains(e.ReplyText, "secret plan") {
			t.Errorf("thinking text stored: %+v", e)
		}
		if e.Kind == "prompt" || e.Kind == "reply" || e.Kind == "tool_use" {
			if e.GitBranch != "feat/demo" {
				t.Errorf("%s GitBranch = %q, want feat/demo", e.Kind, e.GitBranch)
			}
		}
	}
	if replies[0].Model != "model-x" {
		t.Errorf("reply Model = %q, want model-x", replies[0].Model)
	}
}

func TestMemdTranscriptReplyMasked(t *testing.T) {
	line := `{"type":"assistant","uuid":"a9","sessionId":"s","timestamp":"2026-10-28T10:00:00Z","message":{"id":"m9","role":"assistant","model":"model-x","content":[{"type":"text","text":"use token ` + memdFakeGH + ` here"}]}}`
	evs := parseMemdLines("/p/s.jsonl", "a", [][]byte{[]byte(line)}, nil)
	if len(evs) != 1 || evs[0].Kind != "reply" {
		t.Fatalf("events = %+v", evs)
	}
	if strings.Contains(evs[0].ReplyText, memdFakeGH) {
		t.Errorf("secret survived in reply: %q", evs[0].ReplyText)
	}
}

func TestMemdTranscriptSyntheticModelIgnored(t *testing.T) {
	line := `{"type":"assistant","uuid":"a9","sessionId":"s","timestamp":"2026-10-28T10:00:00Z","message":{"id":"m9","role":"assistant","model":"<synthetic>","content":[{"type":"text","text":"No response requested."}]}}`
	evs := parseMemdLines("/p/s.jsonl", "a", [][]byte{[]byte(line)}, nil)
	if len(evs) != 1 || evs[0].Model != "" {
		t.Errorf("events = %+v, want one reply with no model", evs)
	}
}

func TestMemdTranscriptEmptyTextBlockSkipped(t *testing.T) {
	line := `{"type":"assistant","uuid":"a9","sessionId":"s","timestamp":"2026-10-28T10:00:00Z","message":{"id":"m9","role":"assistant","content":[{"type":"text","text":"  \n"}]}}`
	if evs := parseMemdLines("/p/s.jsonl", "a", [][]byte{[]byte(line)}, nil); len(evs) != 0 {
		t.Errorf("events = %+v, want none for a blank text block", evs)
	}
}
