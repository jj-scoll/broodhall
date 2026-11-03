package main

import (
	"database/sql"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// All fixtures are synthetic.

var hoNow = time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)

// seedHandoff inserts one handoff row stamped at.
func seedHandoff(t *testing.T, db *sql.DB, project string, fields map[string]any, at time.Time) {
	t.Helper()
	b, _ := json.Marshal(fields)
	ctxExec(t, db, `INSERT INTO work_state (project, list_name, fields, created_at) VALUES (?, 'handoff', ?, CAST(? AS TIMESTAMP))`,
		project, string(b), at.UTC().Format("2006-01-02 15:04:05"))
}

func hoEntry(text string) map[string]any {
	return map[string]any{"text": text, "obs_ids": "12,34", "project_dir": "/work/p", "handoff_file": "HANDOFF-abc.md"}
}

// hoSettings points the settings file at a temp dir for the test.
func hoSettings(t *testing.T, content string) {
	t.Helper()
	dir := t.TempDir()
	if content != "" {
		if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("MEMORY_SETTINGS", filepath.Join(dir, "settings.json"))
}

func TestClaimHandoffFormat(t *testing.T) {
	hoSettings(t, "")
	tests := []struct {
		name   string
		fields map[string]any
		want   string
	}{
		{"relative file joins project_dir and ids are listed", hoEntry("Goal: ship"),
			"# Handoff from a previous session (written 10 minutes ago, shown once)\nGoal: ship\nDetails: get_observations ids 12,34; full text in /work/p/HANDOFF-abc.md"},
		{"absolute file kept, no ids", map[string]any{"text": "  Goal: ship  \n", "handoff_file": "/elsewhere/H.md", "project_dir": "/work/p"},
			"# Handoff from a previous session (written 10 minutes ago, shown once)\nGoal: ship\nDetails: full text in /elsewhere/H.md"},
		{"no handoff_file defaults to HANDOFF.md", map[string]any{"text": "Goal: ship", "project_dir": "/work/p"},
			"# Handoff from a previous session (written 10 minutes ago, shown once)\nGoal: ship\nDetails: full text in /work/p/HANDOFF.md"},
		{"no project_dir leaves the name as written", map[string]any{"text": "Goal: ship", "handoff_file": "H.md"},
			"# Handoff from a previous session (written 10 minutes ago, shown once)\nGoal: ship\nDetails: full text in H.md"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := ctxDB(t)
			seedHandoff(t, db, "p", tt.fields, hoNow.Add(-10*time.Minute))
			got, err := claimHandoff(db, "p", "startup", hoNow)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("got:\n%s\nwant:\n%s", got, tt.want)
			}
		})
	}
}

func TestClaimHandoffShownOnce(t *testing.T) {
	hoSettings(t, "")
	db := ctxDB(t)
	seedHandoff(t, db, "p", hoEntry("first"), hoNow.Add(-time.Hour))
	if got, _ := claimHandoff(db, "p", "clear", hoNow); !strings.Contains(got, "first") {
		t.Fatalf("first claim = %q", got)
	}
	if got, _ := claimHandoff(db, "p", "clear", hoNow.Add(time.Minute)); got != "" {
		t.Errorf("second claim must be empty, got %q", got)
	}
	// the consumed row records who took it and when
	var raw string
	if err := db.QueryRow(`SELECT fields FROM work_state WHERE list_name='handoff' ORDER BY id DESC LIMIT 1`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var f map[string]any
	json.Unmarshal([]byte(raw), &f)
	if f["consumed_by"] != "clear" || f["consumed_at"] != float64(hoNow.UnixMilli()) {
		t.Errorf("consumed row = %v", f)
	}
	// a handoff written after that is new and shown again
	seedHandoff(t, db, "p", hoEntry("second"), hoNow.Add(2*time.Minute))
	if got, _ := claimHandoff(db, "p", "startup", hoNow.Add(3*time.Minute)); !strings.Contains(got, "second") {
		t.Errorf("new handoff after a consumed one must show, got %q", got)
	}
}

func TestClaimHandoffEligibility(t *testing.T) {
	tests := []struct {
		name     string
		settings string
		source   string
		age      time.Duration
		text     any
		project  string
		want     bool
	}{
		{"startup fresh", "", "startup", time.Hour, "x", "p", true},
		{"unknown source is treated like startup", "", "", time.Hour, "x", "p", true},
		{"resume never claims", "", "resume", time.Minute, "x", "p", false},
		{"compact within 30 minutes", "", "compact", 29 * time.Minute, "x", "p", true},
		{"compact past 30 minutes", "", "compact", 31 * time.Minute, "x", "p", false},
		{"startup within the default 168 hours", "", "startup", 167 * time.Hour, "x", "p", true},
		{"startup past the default 168 hours", "", "startup", 169 * time.Hour, "x", "p", false},
		{"setting shortens the window", `{"handoff_max_age_hours":"2"}`, "startup", 3 * time.Hour, "x", "p", false},
		{"setting shortens but entry inside it", `{"handoff_max_age_hours":"2"}`, "startup", time.Hour, "x", "p", true},
		{"setting shorter than 30m caps compact too", `{"handoff_max_age_hours":"0.25"}`, "compact", 20 * time.Minute, "x", "p", false},
		{"setting below zero falls back to 168", `{"handoff_max_age_hours":"-1"}`, "startup", 100 * time.Hour, "x", "p", true},
		{"unparsable setting falls back to 168", `{"handoff_max_age_hours":"soon"}`, "startup", 100 * time.Hour, "x", "p", true},
		{"empty text", "", "startup", time.Hour, "   ", "p", false},
		{"non-string text", "", "startup", time.Hour, 5, "p", false},
		{"other project", "", "startup", time.Hour, "x", "q", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hoSettings(t, tt.settings)
			db := ctxDB(t)
			seedHandoff(t, db, "p", map[string]any{"text": tt.text}, hoNow.Add(-tt.age))
			got, err := claimHandoff(db, tt.project, tt.source, hoNow)
			if err != nil {
				t.Fatal(err)
			}
			if (got != "") != tt.want {
				t.Errorf("claimed = %v, want %v (%q)", got != "", tt.want, got)
			}
			// a refused claim leaves the entry unshown for the next session
			if !tt.want && tt.project == "p" && tt.source == "resume" {
				if got, _ := claimHandoff(db, "p", "startup", hoNow); got == "" {
					t.Error("resume must not consume the handoff")
				}
			}
		})
	}
}

func TestClaimHandoffCapsLongText(t *testing.T) {
	hoSettings(t, "")
	long := strings.Repeat("line of handoff text that goes on\n", 200) // ~6.8k chars
	db := ctxDB(t)
	seedHandoff(t, db, "p", hoEntry(long), hoNow.Add(-time.Minute))
	got, _ := claimHandoff(db, "p", "startup", hoNow)
	if n := utf16Len(got); n > 3500 {
		t.Errorf("claimed block is %d units, over 3500", n)
	}
	lines := strings.Split(got, "\n")
	if len(lines) < 3 {
		t.Fatalf("no cut block: %q", got)
	}
	last := lines[len(lines)-1]
	if !strings.HasPrefix(last, "Cut to fit. Details: get_observations ids 12,34; full text in /work/p/HANDOFF-abc.md") {
		t.Errorf("last line = %q", last)
	}
	if lines[1] != "line of handoff text that goes on" {
		t.Errorf("body must be cut on whole lines, got first body line %q", lines[1])
	}

	// one line longer than the room: cut with an ellipsis, never splitting a surrogate pair
	db = ctxDB(t)
	seedHandoff(t, db, "p", hoEntry(strings.Repeat("😀", 4000)), hoNow.Add(-time.Minute))
	got, _ = claimHandoff(db, "p", "startup", hoNow)
	if n := utf16Len(got); n > 3500 {
		t.Errorf("emoji block is %d units, over 3500", n)
	}
	if !strings.Contains(got, "😀…\nCut to fit.") || strings.ContainsRune(got, '�') {
		t.Errorf("long single line not cut cleanly: %q", got[:120])
	}
}

func TestStartContextClaimsHandoffFirst(t *testing.T) {
	hoSettings(t, "")
	db := ctxDB(t)
	ctxObs(t, db, 1, "p", "Some observation", "narr", hoNow.Add(-time.Hour))
	seedHandoff(t, db, "p", hoEntry("Goal: ship it"), hoNow.Add(-time.Hour))

	// session_start_context and the plain builder never consume it
	if b := buildContext(db, "p", hoNow); strings.Contains(b, "# Handoff from a previous session") {
		t.Fatalf("buildContext must not claim:\n%s", b)
	}
	block := buildStartContext(db, "p", "startup", hoNow)
	if !strings.HasPrefix(block, "# Handoff from a previous session (written about 1 hour ago, shown once)\nGoal: ship it\nDetails:") {
		t.Errorf("handoff must lead the block:\n%.300s", block)
	}
	if !strings.Contains(block, "# Work state") || !strings.Contains(block, "Some observation") {
		t.Errorf("rest of the block missing:\n%s", block)
	}
	if again := buildStartContext(db, "p", "startup", hoNow); strings.Contains(again, "# Handoff from a previous session") {
		t.Errorf("second SessionStart must not show it again:\n%.300s", again)
	}
	// the handoff block is separated from the work state by a blank line
	if !strings.Contains(block, "HANDOFF-abc.md\n\n# Work state") {
		t.Errorf("want a blank line between handoff and work state:\n%.500s", block)
	}
}

func TestStartContextHandoffKeepsGuard(t *testing.T) {
	for _, tt := range []struct {
		name, text string
	}{
		{"ascii at the cap", strings.Repeat("quoted \"text\" and more words\n", 400)},
		{"multibyte", strings.Repeat("日本語のハンドオフ本文です。\n", 400)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			hoSettings(t, "")
			db := ctxDB(t)
			seedHeavy(t, db)
			seedHandoff(t, db, "p", hoEntry(tt.text), hoNow.Add(-time.Minute))
			block := buildStartContext(db, "p", "startup", hoNow)
			n := ctxSize(block)
			t.Logf("JSON bytes = %d (target %d, guard %d)", n, contextTarget, contextGuard)
			if n > contextGuard {
				t.Fatalf("JSON = %d bytes, over guard %d", n, contextGuard)
			}
			if !strings.HasPrefix(block, "# Handoff from a previous session") {
				t.Error("handoff must survive the guard and stay first")
			}
			if !strings.Contains(block, "Cut to fit. Details:") {
				t.Error("an over-long handoff must be cut with the Details pointer")
			}
			if !strings.Contains(block, "keep-me-open") {
				t.Error("open work_state item missing")
			}
			if n > contextTarget && !strings.Contains(block, ctxCutMarker) {
				// timeline entries are trimmed oldest first until the target holds
				t.Errorf("JSON = %d bytes, over target %d though trimming was possible", n, contextTarget)
			}
		})
	}
}

func TestMemdHTTPContextClaimsHandoff(t *testing.T) {
	hoSettings(t, "")
	db := ctxDB(t)
	seedHandoff(t, db, "proj-a", hoEntry("Goal: ship it"), time.Now().Add(-time.Hour))
	h := memdHandler(db)

	_, body, _ := httpDo(t, h, "GET", "/context?project=proj-a&source=resume", "")
	if strings.Contains(body, "Handoff from a previous session") {
		t.Fatalf("resume must not show it: %s", body)
	}
	// session_start_context is a model-facing tool, not a SessionStart: it never claims
	_, body, _ = httpDo(t, h, "POST", "/tool/session_start_context", `{"project":"proj-a"}`)
	if strings.Contains(body, "Handoff from a previous session") {
		t.Fatalf("the tool must not claim: %s", body)
	}
	_, body, _ = httpDo(t, h, "GET", "/context?project=proj-a&source=startup", "")
	var out struct {
		HookSpecificOutput struct {
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("not hook JSON: %v: %s", err, body)
	}
	if !strings.HasPrefix(out.HookSpecificOutput.AdditionalContext, "# Handoff from a previous session") {
		t.Errorf("startup must lead with the handoff: %.200s", out.HookSpecificOutput.AdditionalContext)
	}
	_, body, _ = httpDo(t, h, "GET", "/context?project=proj-a&source=startup", "")
	if strings.Contains(body, "Handoff from a previous session") {
		t.Errorf("shown twice: %s", body)
	}
}

// mem context hands the hook input's source to the daemon, so memory can tell a compact from a
// fresh start. The daemon here is a fake on a temp socket.
func TestMemContextPassesSource(t *testing.T) {
	dir, err := os.MkdirTemp("", "ctx")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	t.Setenv("XDG_RUNTIME_DIR", dir)
	ln, err := net.Listen("unix", filepath.Join(dir, "memory.sock"))
	if err != nil {
		t.Fatal(err)
	}
	var got string
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.RawQuery
		io.WriteString(w, `{"hookSpecificOutput":{"hookEventName":"SessionStart","additionalContext":"ok"}}`)
	})}
	go srv.Serve(ln)
	defer srv.Close()

	old := memdProjectKey
	memdProjectKey = func(string) string { return "p" }
	defer func() { memdProjectKey = old }()

	for _, tt := range []struct{ input, want string }{
		{`{"cwd":"/work/p","source":"compact"}`, "project=p&source=compact"},
		{`{"cwd":"/work/p"}`, "project=p"},
	} {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		io.WriteString(w, tt.input)
		w.Close()
		oldIn := os.Stdin
		os.Stdin = r
		got = ""
		code := memContext(nil)
		os.Stdin = oldIn
		r.Close()
		if code != 0 || got != tt.want {
			t.Errorf("input %s: exit %d, query %q, want %q", tt.input, code, got, tt.want)
		}
	}
}
