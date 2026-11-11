package main

import (
	"fmt"
	"path/filepath"
	"testing"
)

// forkOpen is the queue-operation line Claude Code writes when a transcript file is opened:
// it has a timestamp and no uuid.
func forkOpen(sess, ts string) string {
	return fmt.Sprintf(`{"type":"queue-operation","operation":"enqueue","timestamp":%q,"sessionId":%q}`+"\n", ts, sess)
}

// forkPrompt is a typed prompt with a message uuid and, when from is set, the forkedFrom
// object Claude Code's in-session branch writes.
func forkPrompt(sess, uuid, text, ts, from string) string {
	ff := ""
	if from != "" {
		ff = fmt.Sprintf(`"forkedFrom":{"sessionId":%q,"messageUuid":%q},`, from, uuid)
	}
	return fmt.Sprintf(`{"type":"user","uuid":%q,%s"sessionId":%q,"cwd":"/work/demo","entrypoint":"cli","timestamp":%q,"message":{"role":"user","content":%q}}`+"\n", uuid, ff, sess, ts, text)
}

func sessionForkedFrom(t *testing.T, e *tailEnv, id string) string {
	t.Helper()
	var got string
	if err := e.db.QueryRow(`SELECT forked_from FROM sessions WHERE session_id = ?`, id).Scan(&got); err != nil {
		t.Fatalf("forked_from of %s: %v", id, err)
	}
	return got
}

// `claude --resume X --fork-session` (verified with claude 2.1.322): the new transcript has
// no forkedFrom field. It repeats the original's lines, uuids included, under the new
// sessionId, after a queue-operation line stamped at the fork time.
func TestTailLinksForkByCopiedUUID(t *testing.T) {
	for _, forkFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("forkFirst=%v", forkFirst), func(t *testing.T) {
			e := newTailEnv(t, "")
			orig, fork := "aaaa1111-0000-4000-8000-000000000000", "bbbb2222-0000-4000-8000-000000000000"
			origFile := forkOpen(orig, tailTS(10, 0, 0)) + forkPrompt(orig, "u-1", "first ask", tailTS(10, 0, 1), "")
			forkFile := forkOpen(fork, tailTS(11, 0, 0)) + forkPrompt(fork, "u-1", "first ask", tailTS(10, 0, 1), "") +
				forkPrompt(fork, "u-2", "forked ask", tailTS(11, 0, 5), "")
			write := func(id, body string) { tailAppend(t, e.path(id+".jsonl"), body) }
			tl := e.tailer()
			if forkFirst {
				write(fork, forkFile)
				tailScan(t, tl)
				write(orig, origFile)
			} else {
				write(orig, origFile)
				tailScan(t, tl)
				write(fork, forkFile)
			}
			tailScan(t, tl)
			if got := sessionForkedFrom(t, e, fork); got != orig {
				t.Errorf("fork.forked_from = %q, want %q", got, orig)
			}
			if got := sessionForkedFrom(t, e, orig); got != "" {
				t.Errorf("original.forked_from = %q, want empty", got)
			}
		})
	}
}

func TestTailUnrelatedSessionsAreNotForks(t *testing.T) {
	e := newTailEnv(t, "")
	a, b := "aaaa1111-0000-4000-8000-000000000000", "bbbb2222-0000-4000-8000-000000000000"
	tailAppend(t, e.path(a+".jsonl"), forkOpen(a, tailTS(10, 0, 0))+forkPrompt(a, "u-a", "one", tailTS(10, 0, 1), ""))
	tailAppend(t, e.path(b+".jsonl"), forkOpen(b, tailTS(11, 0, 0))+forkPrompt(b, "u-b", "two", tailTS(11, 0, 1), ""))
	tailScan(t, e.tailer())
	for _, id := range []string{a, b} {
		if got := sessionForkedFrom(t, e, id); got != "" {
			t.Errorf("%s.forked_from = %q, want empty", id, got)
		}
	}
}

// An explicit forkedFrom.sessionId (written by the in-session branch command) is stored as
// it is.
func TestTailStoresExplicitForkedFrom(t *testing.T) {
	e := newTailEnv(t, "")
	orig, fork := "aaaa1111-0000-4000-8000-000000000000", "bbbb2222-0000-4000-8000-000000000000"
	tailAppend(t, e.path(fork+".jsonl"), forkPrompt(fork, "u-1", "branch ask", tailTS(11, 0, 1), orig))
	tailScan(t, e.tailer())
	if got := sessionForkedFrom(t, e, fork); got != orig {
		t.Errorf("forked_from = %q, want %q", got, orig)
	}
}

// Subagent lines carry the parent's sessionId, so the offsets row of a subagent transcript
// names the parent. Without a sessionId the parent is the directory above subagents/.
func TestTailSubagentRecordsParentSession(t *testing.T) {
	parent := "cccc3333-0000-4000-8000-000000000000"
	t.Run("sessionId on the line", func(t *testing.T) {
		e := newTailEnv(t, filepath.Join(parent, "subagents"))
		p := e.path("agent-a1b2c3.jsonl")
		tailAppend(t, p, `{"type":"user","isSidechain":true,"agentId":"a1b2c3","sessionId":"`+parent+`","cwd":"/work/demo","timestamp":"`+tailTS(10, 0, 1)+`","message":{"role":"user","content":"brief"}}`+"\n")
		tailScan(t, e.tailer())
		var sid string
		var sub bool
		if err := e.db.QueryRow(`SELECT session_id, sub FROM transcript_offsets WHERE path = ?`, p).Scan(&sid, &sub); err != nil {
			t.Fatal(err)
		}
		if sid != parent || !sub {
			t.Errorf("offsets row = (%q, sub=%v), want (%q, true)", sid, sub, parent)
		}
	})
	t.Run("no sessionId on the line", func(t *testing.T) {
		e := newTailEnv(t, filepath.Join(parent, "subagents"))
		p := e.path("agent-d4e5f6.jsonl")
		tailAppend(t, p, `{"type":"user","isSidechain":true,"cwd":"/work/demo","timestamp":"`+tailTS(10, 0, 1)+`","message":{"role":"user","content":"brief"}}`+"\n")
		tailScan(t, e.tailer())
		var sid string
		if err := e.db.QueryRow(`SELECT session_id FROM transcript_offsets WHERE path = ?`, p).Scan(&sid); err != nil {
			t.Fatal(err)
		}
		if sid != parent {
			t.Errorf("offsets session_id = %q, want parent %q", sid, parent)
		}
		var n int
		if err := e.db.QueryRow(`SELECT count(*) FROM sessions WHERE session_id LIKE 'agent-%'`).Scan(&n); err != nil || n != 0 {
			t.Errorf("sessions rows named agent-* = %d (err %v), want none", n, err)
		}
	})
}

func TestUISessionsShowForks(t *testing.T) {
	db, _ := openTestMemd(t)
	apiExec(t, db, `INSERT INTO sessions (session_id, project, forked_from) VALUES ('orig','p',''), ('f1','p','orig'), ('f2','p','orig')`)
	h := uiHandler(db)
	var list struct {
		Items []struct {
			ID         string `json:"id"`
			ForkedFrom string `json:"forked_from"`
			Forks      int    `json:"forks"`
			ForkIDs    []string
		} `json:"items"`
	}
	apiJSON(t, h, "/api/v1/sessions", &list)
	got := map[string][2]any{}
	for _, s := range list.Items {
		got[s.ID] = [2]any{s.ForkedFrom, s.Forks}
	}
	if got["orig"] != [2]any{"", 2} || got["f1"] != [2]any{"orig", 0} || got["f2"] != [2]any{"orig", 0} {
		t.Errorf("sessions = %v", got)
	}
	var one struct {
		Forks []struct {
			ID string `json:"id"`
		} `json:"fork_sessions"`
	}
	apiJSON(t, h, "/api/v1/sessions/orig", &one)
	if len(one.Forks) != 2 {
		t.Errorf("detail fork_sessions = %+v, want f1 and f2", one.Forks)
	}
}

func TestTranscriptSessionID(t *testing.T) {
	for _, tc := range []struct{ path, want string }{
		{"/h/.claude/projects/-w/sess1.jsonl", "sess1"},
		{"/h/.claude/projects/-w/p1/subagents/agent-x.jsonl", "p1"},
		{"/h/.claude/projects/-w/subagents/agent-x.jsonl", "agent-x"},
	} {
		if got := transcriptSessionID(tc.path); got != tc.want {
			t.Errorf("transcriptSessionID(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}
